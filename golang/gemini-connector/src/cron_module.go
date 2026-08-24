package main

import (
	"context"
	"errors"
	"log"
)

// errCronPlannerStopped reports that a queued planner turn was discarded by
// /stop before it could run.
var errCronPlannerStopped = errors.New("예약 작업 해석이 중지되었습니다")

// CronModule bundles the cron subsystem's construction and lifecycle: store,
// service and scheduler. It is the only place where the cron feature meets
// concrete infrastructure (database path, messaging surface, turn
// coordinator); everything below it works against ports.
type CronModule struct {
	Store     *CronStore
	Service   *CronService
	Scheduler *CronScheduler
}

// Stop halts the scheduler loop.
func (m *CronModule) Stop() {
	if m != nil && m.Scheduler != nil {
		m.Scheduler.Stop()
	}
}

// Close releases the database.
func (m *CronModule) Close() error {
	if m == nil || m.Store == nil {
		return nil
	}
	return m.Store.Close()
}

// CronModuleOptions carries every dependency the cron subsystem may touch.
type CronModuleOptions struct {
	Disabled       bool
	AdminUserIDs   []string
	ConvID         func() string
	MissingConvMsg string
	Surface        CronSurface
	Turns          *TurnCoordinator
}

// NewCronModule wires the cron subsystem. A nil module (no error) means cron
// is unavailable for a benign reason — disabled by flag, no interactive
// adapter, or unusable database — and startup continues without it.
func NewCronModule(opts CronModuleOptions) (*CronModule, error) {
	if opts.Disabled {
		log.Println("Cron subsystem disabled via --cron-disabled")
		return nil, nil
	}
	if opts.Surface == nil {
		log.Println("Cron subsystem unavailable: interactive adapter is not active")
		return nil, nil
	}
	cronDBPath, err := cronDatabasePath()
	if err != nil {
		log.Printf("Cron disabled: cannot resolve database path: %v", err)
		return nil, nil
	}
	store, err := OpenCronStore(cronDBPath)
	if err != nil {
		log.Printf("Cron disabled: database open failed (%s): %v", cronDBPath, err)
		return nil, nil
	}

	sched := NewCronScheduler(store, opts.ConvID, opts.Surface, nil)
	sched.SetCoordinator(opts.Turns)
	svc := NewCronService(store, opts.ConvID, opts.MissingConvMsg, opts.Surface,
		newCronPlanner(opts.Turns, opts.ConvID), opts.AdminUserIDs, sched)

	if missed, err := sched.Reconcile(); err != nil {
		log.Printf("Cron initial reconcile failed: %v", err)
	} else if missed > 0 {
		log.Printf("Cron reconcile adjusted %d trigger(s) after downtime", missed)
	}
	sched.Start()
	log.Printf("Cron scheduler started (db: %s)", cronDBPath)

	return &CronModule{Store: store, Service: svc, Scheduler: sched}, nil
}

// newCronPlanner submits candidate generation through the shared turn
// coordinator so planning serializes with user turns and /stop can cancel it.
func newCronPlanner(turns *TurnCoordinator, convID func() string) cronPlannerFunc {
	type plannerResult struct {
		response string
		err      error
	}
	return func(ctx context.Context, prompt string) (string, error) {
		ch := make(chan plannerResult, 1)
		// A dropped submission (queued work discarded by /stop) must notify
		// the waiting caller immediately instead of letting its 6-minute
		// budget expire silently.
		turns.SubmitManaged(func(ctx context.Context) {
			resp, err := executeAgy(ctx, prompt, convID(), AgyCallOptions{Profile: ProfilePlanner})
			ch <- plannerResult{response: resp, err: err}
		}, func() {
			ch <- plannerResult{err: errCronPlannerStopped}
		})
		select {
		case r := <-ch:
			return r.response, r.err
		case <-ctx.Done():
			// The caller's budget expired; the queued/running turn keeps its
			// own lifecycle and will be reconciled by /stop or completion.
			return "", ctx.Err()
		}
	}
}
