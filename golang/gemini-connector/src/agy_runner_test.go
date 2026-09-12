package main

import (
	"bytes"
	"context"
	"os/exec"
	"sync"
	"testing"
)

func TestAgyRunnerIsolation(t *testing.T) {
	var count1, count2 int
	var mu sync.Mutex

	runner1 := func(cmd *exec.Cmd) error {
		mu.Lock()
		count1++
		mu.Unlock()
		if cmd.Stdout != nil {
			cmd.Stdout.Write([]byte(`{"conversation_id":"conv-1","status":"SUCCESS","response":"res1"}`))
		}
		return nil
	}

	runner2 := func(cmd *exec.Cmd) error {
		mu.Lock()
		count2++
		mu.Unlock()
		if cmd.Stdout != nil {
			cmd.Stdout.Write([]byte(`{"conversation_id":"conv-2","status":"SUCCESS","response":"res2"}`))
		}
		return nil
	}

	exec1 := newAgyExecutorWithRunner(runner1)
	exec2 := newAgyExecutorWithRunner(runner2)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		res, err := exec1.Execute(context.Background(), "p1", "", AgyCallOptions{})
		if err != nil || res.Text != "res1" {
			t.Errorf("exec1 failed: res=%v, err=%v", res, err)
		}
	}()

	go func() {
		defer wg.Done()
		res, err := exec2.Execute(context.Background(), "p2", "", AgyCallOptions{})
		if err != nil || res.Text != "res2" {
			t.Errorf("exec2 failed: res=%v, err=%v", res, err)
		}
	}()

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if count1 != 1 || count2 != 1 {
		t.Fatalf("expected each runner called once, got c1=%d, c2=%d", count1, count2)
	}
}

func TestBoundedWriter_LimitEnforced(t *testing.T) {
	var buf bytes.Buffer
	bw := NewBoundedWriter(&buf, 10)

	n, err := bw.Write([]byte("12345"))
	if err != nil || n != 5 {
		t.Fatalf("expected write 5 bytes, got n=%d, err=%v", n, err)
	}

	n, err = bw.Write([]byte("67890123"))
	if err == nil {
		t.Fatalf("expected error on exceeding limit")
	}
	if !bw.Exceeded {
		t.Fatalf("expected Exceeded to be true")
	}
	if buf.Len() != 10 {
		t.Fatalf("expected buffer capped at 10 bytes, got %d", buf.Len())
	}
}
