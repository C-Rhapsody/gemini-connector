package main

import "context"

// TurnCoordinator is the single entry point for serialized agy work. Chat
// turns, conversation management, cron planning and scheduled cron runs all
// submit here so they share one FIFO order and one cancellation switch.
type TurnCoordinator struct {
	q *agyTurnQueue
}

func NewTurnCoordinator() *TurnCoordinator {
	return &TurnCoordinator{q: newAgyTurnQueue()}
}

// Submit enqueues a unit of serialized work and returns how many turns are
// ahead of it (zero means it runs immediately).
func (c *TurnCoordinator) Submit(run func(ctx context.Context)) int {
	return c.q.Enqueue(run)
}

// SubmitManaged behaves like Submit but invokes onDrop synchronously when the
// turn is discarded by StopActive before it ever starts. Owners with durable
// state (the cron execution ledger) use it to record the drop.
func (c *TurnCoordinator) SubmitManaged(run func(ctx context.Context), onDrop func()) int {
	return c.q.EnqueueManaged(run, onDrop)
}

// StopActive cancels the running turn and drops queued ones. See
// agyTurnQueue.StopActive for the exact contract.
func (c *TurnCoordinator) StopActive() (active bool, dropped int) {
	return c.q.StopActive()
}

// Busy reports whether a turn is running or queued.
func (c *TurnCoordinator) Busy() bool {
	return c.q.Busy()
}
