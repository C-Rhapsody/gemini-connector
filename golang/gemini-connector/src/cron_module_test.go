package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The cron planner must respect the shared FIFO order: while any turn holds
// the queue, planning waits. /stop therefore cancels a queued or running
// planner exactly like a user turn.
func TestCronPlanner_WaitsForQueueAndStopsWithIt(t *testing.T) {
	turns := NewTurnCoordinator()
	planner := newCronPlanner(turns, func() string { return "conv-test" })

	blocked := make(chan struct{})
	unwound := make(chan struct{}, 1)
	turns.Submit(func(ctx context.Context) {
		close(blocked)
		<-ctx.Done()
		unwound <- struct{}{}
	})
	<-blocked

	type plannerResult struct {
		err error
	}
	done := make(chan plannerResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := planner(ctx, "예약 작업 후보 만들어줘")
		done <- plannerResult{err: err}
	}()

	// While the first turn blocks the queue, the planner must not complete.
	select {
	case r := <-done:
		t.Fatalf("planner bypassed the shared queue: %v", r.err)
	case <-time.After(150 * time.Millisecond):
	}

	active, dropped := turns.StopActive()
	if !active || dropped != 1 {
		t.Fatalf("expected active stop dropping the queued planner, got active=%v dropped=%d", active, dropped)
	}

	select {
	case r := <-done:
		if !errors.Is(r.err, errCronPlannerStopped) {
			t.Fatalf("planner should report the drop, got %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("planner did not unwind after stop")
	}
	select {
	case <-unwound:
	case <-time.After(5 * time.Second):
		t.Fatal("blocking turn did not unwind")
	}
}

// NewCronModule declines benignly when there is no interactive surface.
func TestNewCronModule_NilWithoutSurface(t *testing.T) {
	m, err := NewCronModule(CronModuleOptions{
		ConvID: func() string { return "" },
		Turns:  NewTurnCoordinator(),
	})
	if err != nil || m != nil {
		t.Fatalf("expected nil module without error, got %v/%v", m, err)
	}
}

func TestNewCronModule_NilWhenDisabled(t *testing.T) {
	m, err := NewCronModule(CronModuleOptions{Disabled: true})
	if err != nil || m != nil {
		t.Fatalf("expected nil module without error, got %v/%v", m, err)
	}
}
