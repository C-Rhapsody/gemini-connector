package main

import (
	"context"
	"sync"
)

// turnJob is a unit of sequential agy work. Each job carries its own
// cancellable context so that /stop can abort the running job and drop
// queued ones without touching jobs enqueued afterwards.
type turnJob struct {
	ctx    context.Context
	cancel context.CancelFunc
	run    func(ctx context.Context)
}

// agyTurnQueue serializes agy work (AI turns, conversation resets, switches).
// Jobs run strictly FIFO, one at a time, each on its own goroutine spawned by
// the worker loop.
type agyTurnQueue struct {
	mu        sync.Mutex
	jobs      []turnJob
	running   bool
	curCancel context.CancelFunc
}

func newAgyTurnQueue() *agyTurnQueue {
	return &agyTurnQueue{}
}

// Enqueue appends work to the FIFO queue and starts the worker if idle.
// It returns the number of jobs that must finish before this one starts
// (the actively running job plus queued predecessors), so callers can tell
// the user that the request is waiting. Zero means it runs immediately.
func (q *agyTurnQueue) Enqueue(run func(ctx context.Context)) (ahead int) {
	ctx, cancel := context.WithCancel(context.Background())
	q.mu.Lock()
	ahead = len(q.jobs)
	if q.running {
		ahead++
	}
	q.jobs = append(q.jobs, turnJob{ctx: ctx, cancel: cancel, run: run})
	if q.running {
		q.mu.Unlock()
		return ahead
	}
	q.running = true
	q.mu.Unlock()
	go q.worker()
	return ahead
}

// worker executes queued jobs one at a time until the queue drains.
func (q *agyTurnQueue) worker() {
	for {
		q.mu.Lock()
		if len(q.jobs) == 0 {
			q.running = false
			q.curCancel = nil
			q.mu.Unlock()
			return
		}
		job := q.jobs[0]
		q.jobs = q.jobs[1:]
		q.curCancel = job.cancel
		q.mu.Unlock()

		job.run(job.ctx)
		job.cancel() // release context resources
	}
}

// StopActive cancels the currently running job (if any) and drops every job
// that was queued before this call. Jobs enqueued afterwards are kept and
// will run once the cancelled job unwinds. It reports whether a job was
// actively running and how many queued jobs were dropped.
func (q *agyTurnQueue) StopActive() (active bool, dropped int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	active = q.running
	if active && q.curCancel != nil {
		q.curCancel()
	}
	dropped = len(q.jobs)
	for _, job := range q.jobs {
		job.cancel()
	}
	q.jobs = nil
	return active, dropped
}

// Busy reports whether a job is running or queued.
func (q *agyTurnQueue) Busy() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running || len(q.jobs) > 0
}
