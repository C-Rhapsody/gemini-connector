package main

import (
	"context"
	"errors"
	"sync"
)

type JobOrigin int

const (
	OriginTelegram JobOrigin = iota
	OriginCron
	OriginAPI
)

const maxAPIAdmitted = 4

var (
	ErrAPIQueueFull = errors.New("api queue is full")
)

// turnJob is a unit of sequential agy work. Each job carries its own
// cancellable context so that /stop can abort the running job and drop
// queued ones without touching jobs enqueued afterwards. An optional onDrop
// hook lets owners observe queued-but-cancelled work synchronously during
// StopActive; it is never invoked for jobs that started running — those
// observe context cancellation directly.
type turnJob struct {
	ctx    context.Context
	cancel context.CancelFunc
	run    func(ctx context.Context)
	onDrop func()
	origin JobOrigin
}

// agyTurnQueue serializes agy work (AI turns, conversation resets, cron
// planner and scheduled runs). Jobs run strictly FIFO, one at a time.
//
// The worker is a single long-lived goroutine and claims the next job under
// the queue lock, which makes "currently running" (cur) unambiguous: a job is
// either claimed by the worker or still sitting in jobs, never both and
// neither. This removes the historical race where StopActive could run
// between Enqueue and the worker's first claim, misreporting the dropped
// count or skipping the cancel of the job about to start.
type agyTurnQueue struct {
	mu   sync.Mutex
	jobs []turnJob
	cur  *turnJob // claimed by the worker and not yet finished
	wake chan struct{}
}

func newAgyTurnQueue() *agyTurnQueue {
	q := &agyTurnQueue{wake: make(chan struct{}, 1)}
	go q.worker()
	return q
}

func (q *agyTurnQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Enqueue appends work to the FIFO queue. It returns the number of jobs that
// must finish before this one starts (the actively running job plus queued
// predecessors), so callers can tell the user that the request is waiting.
// Zero means it runs immediately.
func (q *agyTurnQueue) Enqueue(run func(ctx context.Context)) int {
	return q.EnqueueManaged(run, nil)
}

// EnqueueManaged behaves like Enqueue but invokes onDrop synchronously when
// the job is discarded by StopActive before it ever starts.
func (q *agyTurnQueue) EnqueueManaged(run func(ctx context.Context), onDrop func()) int {
	return q.EnqueueWithOrigin(run, onDrop, OriginTelegram)
}

func (q *agyTurnQueue) EnqueueWithOrigin(run func(ctx context.Context), onDrop func(), origin JobOrigin) int {
	ctx, cancel := context.WithCancel(context.Background())
	q.mu.Lock()
	ahead := len(q.jobs)
	if q.cur != nil {
		ahead++
	}
	q.jobs = append(q.jobs, turnJob{ctx: ctx, cancel: cancel, run: run, onDrop: onDrop, origin: origin})
	q.mu.Unlock()
	q.signal()
	return ahead
}

// EnqueueAPI enqueues an API job bounded by maxAPIAdmitted. If the API admission
// limit is reached, it returns ErrAPIQueueFull immediately.
func (q *agyTurnQueue) EnqueueAPI(parentCtx context.Context, run func(ctx context.Context)) (int, error) {
	q.mu.Lock()
	apiCount := 0
	if q.cur != nil && q.cur.origin == OriginAPI {
		apiCount++
	}
	for _, j := range q.jobs {
		if j.origin == OriginAPI {
			apiCount++
		}
	}
	if apiCount >= maxAPIAdmitted {
		q.mu.Unlock()
		return 0, ErrAPIQueueFull
	}

	ctx, cancel := context.WithCancel(parentCtx)
	ahead := len(q.jobs)
	if q.cur != nil {
		ahead++
	}
	q.jobs = append(q.jobs, turnJob{ctx: ctx, cancel: cancel, run: run, origin: OriginAPI})
	q.mu.Unlock()
	q.signal()
	return ahead, nil
}

// worker claims and executes queued jobs one at a time until the queue
// drains, then parks until the next enqueue.
func (q *agyTurnQueue) worker() {
	for {
		q.mu.Lock()
		for len(q.jobs) == 0 || q.cur != nil {
			q.mu.Unlock()
			<-q.wake
			q.mu.Lock()
		}
		job := q.jobs[0]
		q.jobs = q.jobs[1:]

		// If the job's context expired or was canceled before starting, skip running it.
		if job.ctx.Err() != nil {
			q.mu.Unlock()
			job.cancel()
			if job.onDrop != nil {
				job.onDrop()
			}
			continue
		}

		q.cur = &job
		q.mu.Unlock()

		job.run(job.ctx)
		job.cancel() // release context resources

		q.mu.Lock()
		q.cur = nil
		q.mu.Unlock()
	}
}

// StopActive cancels the currently running non-API job (if any) and drops every
// non-API job queued before this call, invoking their onDrop hooks synchronously.
// API-origin jobs are preserved and neither canceled nor counted in dropped.
func (q *agyTurnQueue) StopActive() (active bool, dropped int) {
	q.mu.Lock()
	active = q.cur != nil && q.cur.origin != OriginAPI
	if active {
		q.cur.cancel()
	}

	var surviving []turnJob
	var hooks []func()
	for _, job := range q.jobs {
		if job.origin == OriginAPI {
			surviving = append(surviving, job)
		} else {
			job.cancel()
			dropped++
			if job.onDrop != nil {
				hooks = append(hooks, job.onDrop)
			}
		}
	}
	q.jobs = surviving
	q.mu.Unlock()
	for _, h := range hooks {
		h()
	}
	return active, dropped
}

// StopAll cancels all running and queued jobs regardless of origin, used for
// connector shutdown.
func (q *agyTurnQueue) StopAll() {
	q.mu.Lock()
	if q.cur != nil {
		q.cur.cancel()
	}
	for _, job := range q.jobs {
		job.cancel()
		if job.onDrop != nil {
			job.onDrop()
		}
	}
	q.jobs = nil
	q.mu.Unlock()
}

// Busy reports whether a job is running or queued.
func (q *agyTurnQueue) Busy() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.cur != nil || len(q.jobs) > 0
}
