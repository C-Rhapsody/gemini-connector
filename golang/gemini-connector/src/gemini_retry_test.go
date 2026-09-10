package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecuteAgy_TransientStreamLag_RetriesAndSucceeds(t *testing.T) {
	resetQuotaState(t)

	oldRunner := agyCmdRunner
	oldDelay := agyRetryDelay
	agyRetryDelay = 1 * time.Millisecond
	t.Cleanup(func() {
		agyCmdRunner = oldRunner
		agyRetryDelay = oldDelay
	})

	attempts := 0
	agyCmdRunner = func(cmd *exec.Cmd) error {
		attempts++
		if attempts == 1 {
			cmd.Stderr.Write([]byte("error: the connection to the agent was interrupted before the response finished: subscriber fell behind updates, stalled for 6s\n"))
			return errors.New("exit status 1")
		}
		cmd.Stdout.Write([]byte(`{"conversation_id":"conv-123","status":"SUCCESS","response":"Recovered successfully on retry!","num_turns":1}`))
		return nil
	}

	resp, err := executeAgy(context.Background(), "test prompt", "conv-123")
	if err != nil {
		t.Fatalf("expected executeAgy to succeed on retry, got err: %v", err)
	}
	if resp != "Recovered successfully on retry!" {
		t.Fatalf("unexpected response: %q", resp)
	}
	if attempts != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", attempts)
	}
}

func TestExecuteAgy_TransientStreamLag_ExhaustsRetries(t *testing.T) {
	resetQuotaState(t)

	oldRunner := agyCmdRunner
	oldDelay := agyRetryDelay
	agyRetryDelay = 1 * time.Millisecond
	t.Cleanup(func() {
		agyCmdRunner = oldRunner
		agyRetryDelay = oldDelay
	})

	attempts := 0
	agyCmdRunner = func(cmd *exec.Cmd) error {
		attempts++
		cmd.Stderr.Write([]byte("error: the connection to the agent was interrupted before the response finished: subscriber fell behind updates, stalled for 6s\n"))
		return errors.New("exit status 1")
	}

	_, err := executeAgy(context.Background(), "test prompt", "conv-123")
	if err == nil {
		t.Fatal("expected executeAgy to fail when all retry attempts fail")
	}
	ae, ok := err.(*AgyError)
	if !ok || ae.Type != "cli_failure" {
		t.Fatalf("expected cli_failure error, got %#v", err)
	}
	if !strings.Contains(ae.Detail, "subscriber fell behind updates") {
		t.Fatalf("expected detail to mention subscriber fell behind updates, got: %q", ae.Detail)
	}
	if attempts != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d", attempts)
	}
}

func TestExecuteAgy_NonTransientError_NoRetry(t *testing.T) {
	resetQuotaState(t)

	oldRunner := agyCmdRunner
	oldDelay := agyRetryDelay
	agyRetryDelay = 1 * time.Millisecond
	t.Cleanup(func() {
		agyCmdRunner = oldRunner
		agyRetryDelay = oldDelay
	})

	attempts := 0
	agyCmdRunner = func(cmd *exec.Cmd) error {
		attempts++
		cmd.Stderr.Write([]byte("fatal: syntax error in config file\n"))
		return errors.New("exit status 1")
	}

	_, err := executeAgy(context.Background(), "test prompt", "conv-123")
	if err == nil {
		t.Fatal("expected executeAgy to fail on non-transient error")
	}
	if attempts != 1 {
		t.Fatalf("expected exactly 1 attempt for non-transient error, got %d", attempts)
	}
}

func TestExecuteAgy_AuthenticationRequired_NoRetry(t *testing.T) {
	resetQuotaState(t)

	oldRunner := agyCmdRunner
	oldDelay := agyRetryDelay
	agyRetryDelay = 1 * time.Millisecond
	t.Cleanup(func() {
		agyCmdRunner = oldRunner
		agyRetryDelay = oldDelay
	})

	attempts := 0
	agyCmdRunner = func(cmd *exec.Cmd) error {
		attempts++
		cmd.Stderr.Write([]byte("error: authentication required, please run agy login\n"))
		return errors.New("exit status 1")
	}

	_, err := executeAgy(context.Background(), "test prompt", "conv-123")
	if err == nil {
		t.Fatal("expected authentication error")
	}
	ae, ok := err.(*AgyError)
	if !ok || ae.Type != "authentication_required" {
		t.Fatalf("expected authentication_required, got %#v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected exactly 1 attempt for authentication error, got %d", attempts)
	}
}

func TestExecuteAgy_ContextCancelled_NoRetry(t *testing.T) {
	resetQuotaState(t)

	oldRunner := agyCmdRunner
	oldDelay := agyRetryDelay
	agyRetryDelay = 1 * time.Millisecond
	t.Cleanup(func() {
		agyCmdRunner = oldRunner
		agyRetryDelay = oldDelay
	})

	ctx, cancel := context.WithCancel(context.Background())

	attempts := 0
	agyCmdRunner = func(cmd *exec.Cmd) error {
		attempts++
		cancel()
		cmd.Stderr.Write([]byte("error: subscriber fell behind updates, stalled for 6s\n"))
		return errors.New("exit status 1")
	}

	_, err := executeAgy(ctx, "test prompt", "conv-123")
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	if attempts != 1 {
		t.Fatalf("expected no retry when context is cancelled, got %d attempts", attempts)
	}
}

func TestExecuteAgy_DisableRetryOption(t *testing.T) {
	resetQuotaState(t)

	oldRunner := agyCmdRunner
	oldDelay := agyRetryDelay
	agyRetryDelay = 1 * time.Millisecond
	t.Cleanup(func() {
		agyCmdRunner = oldRunner
		agyRetryDelay = oldDelay
	})

	attempts := 0
	agyCmdRunner = func(cmd *exec.Cmd) error {
		attempts++
		cmd.Stderr.Write([]byte("error: subscriber fell behind updates, stalled for 6s\n"))
		return errors.New("exit status 1")
	}

	_, err := executeAgy(context.Background(), "test prompt", "conv-123", AgyCallOptions{DisableRetry: true})
	if err == nil {
		t.Fatal("expected error when DisableRetry is set")
	}
	if attempts != 1 {
		t.Fatalf("expected exactly 1 attempt when DisableRetry is set, got %d", attempts)
	}
}

func TestExecuteAgy_SalvagedResponseOnStreamLag(t *testing.T) {
	resetQuotaState(t)

	tmp := t.TempDir()
	oldBrain := agyBrainDirOverride
	oldRunner := agyCmdRunner
	oldDelay := agyRetryDelay
	agyBrainDirOverride = tmp
	agyRetryDelay = 1 * time.Millisecond
	t.Cleanup(func() {
		agyBrainDirOverride = oldBrain
		agyCmdRunner = oldRunner
		agyRetryDelay = oldDelay
	})

	convID := "salvage-stream-conv"
	logDir := filepath.Join(tmp, convID, ".system_generated", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	transcript := strings.Join([]string{
		`{"step_index":0,"type":"USER_INPUT","status":"DONE","created_at":"` + now + `","content":"what is the price?"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","created_at":"` + now + `","content":"The price is 100 USD."}`,
	}, "\n") + "\n"

	if err := os.WriteFile(filepath.Join(logDir, "transcript.jsonl"), []byte(transcript), 0644); err != nil {
		t.Fatal(err)
	}

	attempts := 0
	agyCmdRunner = func(cmd *exec.Cmd) error {
		attempts++
		cmd.Stderr.Write([]byte("error: the connection to the agent was interrupted before the response finished: subscriber fell behind updates, stalled for 6s\n"))
		return errors.New("exit status 1")
	}

	resp, err := executeAgy(context.Background(), "what is the price?", convID)
	if err != nil {
		t.Fatalf("expected executeAgy to salvage response, got err: %v", err)
	}
	if resp != "The price is 100 USD." {
		t.Fatalf("unexpected salvaged response: %q", resp)
	}
	if attempts != 1 {
		t.Fatalf("expected 1 attempt because turn was salvaged, got %d", attempts)
	}
}
