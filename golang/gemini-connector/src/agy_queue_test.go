package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// busyReporter lets waitQueueIdle work for both raw queues and coordinators.
type busyReporter interface {
	Busy() bool
}

func waitQueueIdle(t *testing.T, q busyReporter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for q.Busy() {
		if time.Now().After(deadline) {
			t.Fatal("queue did not become idle in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAgyTurnQueue_FIFOOrder(t *testing.T) {
	q := newAgyTurnQueue()
	var mu sync.Mutex
	var order []string
	done := make(chan struct{})

	q.Enqueue(func(ctx context.Context) { mu.Lock(); order = append(order, "a"); mu.Unlock() })
	q.Enqueue(func(ctx context.Context) { mu.Lock(); order = append(order, "b"); mu.Unlock() })
	q.Enqueue(func(ctx context.Context) {
		mu.Lock()
		order = append(order, "c")
		mu.Unlock()
		close(done)
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("jobs did not finish in time")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("unexpected execution order: %v", order)
	}
}

func TestAgyTurnQueue_EnqueueReportsWaiting(t *testing.T) {
	q := newAgyTurnQueue()
	started := make(chan struct{})
	release := make(chan struct{})

	ahead1 := q.Enqueue(func(ctx context.Context) {
		close(started)
		<-release
	})
	<-started
	ahead2 := q.Enqueue(func(ctx context.Context) {})

	if ahead1 != 0 {
		t.Fatalf("first job should report no waiting jobs, got %d", ahead1)
	}
	if ahead2 != 1 {
		t.Fatalf("second job should report 1 waiting job, got %d", ahead2)
	}
	close(release)
	waitQueueIdle(t, q)
}

func TestAgyTurnQueue_StopCancelsActiveAndDropsQueued(t *testing.T) {
	q := newAgyTurnQueue()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var droppedRan bool
	var mu sync.Mutex

	q.Enqueue(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
	})
	ahead := q.Enqueue(func(ctx context.Context) {
		mu.Lock()
		droppedRan = true
		mu.Unlock()
	})
	if ahead != 0 { // nothing running yet at enqueue time is fine either way
		t.Logf("queued job reported ahead=%d", ahead)
	}

	<-started // deterministic: the first job has been claimed by the worker

	active, dropped := q.StopActive()
	if !active {
		t.Fatal("StopActive should report an active job")
	}
	if dropped != 1 {
		t.Fatalf("StopActive should drop 1 queued job, got %d", dropped)
	}

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("running job was not cancelled")
	}

	waitQueueIdle(t, q)
	mu.Lock()
	defer mu.Unlock()
	if droppedRan {
		t.Fatal("dropped queued job must not run")
	}
}

func TestAgyTurnQueue_StopKeepsJobsEnqueuedAfterwards(t *testing.T) {
	q := newAgyTurnQueue()
	blocked := make(chan struct{})
	release := make(chan struct{})

	q.Enqueue(func(ctx context.Context) {
		close(blocked)
		<-release
	})
	<-blocked

	active, _ := q.StopActive()
	if !active {
		t.Fatal("expected active job")
	}

	var ran bool
	done := make(chan struct{})
	q.Enqueue(func(ctx context.Context) {
		ran = true
		close(done)
	})

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("post-stop job did not run")
	}
	if !ran {
		t.Fatal("post-stop job flag not set")
	}
	waitQueueIdle(t, q)
}

func TestAgyTurnQueue_StopWhenIdle(t *testing.T) {
	q := newAgyTurnQueue()
	active, dropped := q.StopActive()
	if active || dropped != 0 {
		t.Fatalf("idle stop should be a no-op, got active=%v dropped=%d", active, dropped)
	}
}

// TestAgyTurnQueue_StopStartRaceStress hammers Enqueue and StopActive
// concurrently. It pins down the historical race where StopActive could run
// between Enqueue and the worker's first claim: whatever the interleaving,
// dropped jobs must never execute, onDrop must fire exactly once per dropped
// hook-bearing job, and the queue must keep working afterwards.
func TestAgyTurnQueue_StopStartRaceStress(t *testing.T) {
	for iteration := 0; iteration < 200; iteration++ {
		q := newAgyTurnQueue()

		const jobsPerRound = 8
		var mu sync.Mutex
		ran := make(map[int]bool)
		drops := make(map[int]int)

		stopDone := make(chan struct{})
		go func() {
			defer close(stopDone)
			time.Sleep(time.Duration(iteration%7) * time.Microsecond)
			q.StopActive()
		}()

		for i := 0; i < jobsPerRound; i++ {
			i := i
			q.EnqueueManaged(func(ctx context.Context) {
				time.Sleep(time.Millisecond)
				mu.Lock()
				ran[i] = true
				mu.Unlock()
			}, func() {
				mu.Lock()
				drops[i]++
				mu.Unlock()
			})
		}

		<-stopDone
		waitQueueIdle(t, q)

		mu.Lock()
		for id := range drops {
			if drops[id] != 1 {
				mu.Unlock()
				t.Fatalf("iteration %d: onDrop fired %d times for job %d", iteration, drops[id], id)
			}
			if ran[id] {
				mu.Unlock()
				t.Fatalf("iteration %d: dropped job %d executed", iteration, id)
			}
		}
		mu.Unlock()

		// The queue must remain fully functional after a stop.
		done := make(chan struct{})
		q.Enqueue(func(ctx context.Context) { close(done) })
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: queue unusable after stop", iteration)
		}
		waitQueueIdle(t, q)
	}
}

func TestMessagesApplyDefaults(t *testing.T) {
	m := Messages{StartupWelcome: "custom"}
	m.applyDefaults()
	if m.StartupWelcome != "custom" {
		t.Fatalf("existing message must be preserved, got %q", m.StartupWelcome)
	}
	if m.StopDone == "" || m.StopDoneWithQueued == "" || m.StopNothing == "" || m.QueuedNotice == "" {
		t.Fatal("missing fields must fall back to defaults")
	}
	if m.CommandStartHelp != defaultMessages.CommandStartHelp {
		t.Fatalf("empty help must fall back to default, got %q", m.CommandStartHelp)
	}
}
