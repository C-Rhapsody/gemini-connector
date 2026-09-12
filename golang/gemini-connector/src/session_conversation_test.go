package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 1. API handler passes Config.ConversationID() to executor (not empty)
func TestSession_APIHandler_PassesConfigConversationIDToExecutor(t *testing.T) {
	server, _, _ := setupTestServer(t)
	expectedConvID := "custom-config-conv-uuid-9999"
	server.SetConversationIDProvider(func() string {
		return expectedConvID
	})

	var observedConvID string
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })
	agyCmdRunner = func(cmd *exec.Cmd) error {
		for i, a := range cmd.Args {
			if a == "--conversation" && i+1 < len(cmd.Args) {
				observedConvID = cmd.Args[i+1]
			}
		}
		resp := AgyResponse{Status: "SUCCESS", Response: "ok"}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"ping"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if observedConvID != expectedConvID {
		t.Fatalf("expected executor to receive conversation ID %q, got %q", expectedConvID, observedConvID)
	}
}

// 2. API executor args contain --conversation and the correct ID
func TestSession_APIExecutor_ArgsIncludeConversationAndCorrectID(t *testing.T) {
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	var observedCmd *exec.Cmd
	agyCmdRunner = func(cmd *exec.Cmd) error {
		observedCmd = cmd
		resp := AgyResponse{Status: "SUCCESS", Response: "ok"}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	testID := "target-conv-id-555"
	_, err := executeAgy(context.Background(), "hello", testID, AgyCallOptions{
		Profile: ProfileAPI,
		Model:   "gemini-3.8-flash-high",
	})
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}

	hasConv := false
	for i, a := range observedCmd.Args {
		if a == "--conversation" && i+1 < len(observedCmd.Args) && observedCmd.Args[i+1] == testID {
			hasConv = true
			break
		}
	}
	if !hasConv {
		t.Fatalf("expected --conversation %s in args, got: %v", testID, observedCmd.Args)
	}
}

// 3. Empty conversation ID in remote history request produces explicit configuration error
func TestSession_APIHandler_EmptyConversationID_ReturnsExplicitError(t *testing.T) {
	server, _, _ := setupTestServer(t)
	// Explicitly configure empty conversation ID
	server.SetConversationIDProvider(func() string {
		return ""
	})

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"ping"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hermes-History-Mode", "current_only")
	req.Header.Set("X-Hermes-Context-Owner", "agy")
	req.Header.Set("X-Hermes-Turn-ID", "turn_1")
	req.Header.Set("X-Hermes-Turn-Sequence", "1")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 Internal Server Error for missing conversation ID, got %d", rec.Code)
	}

	var errResp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	if errResp.Error.Code != "missing_conversation_id" {
		t.Fatalf("expected error code 'missing_conversation_id', got %q", errResp.Error.Code)
	}
}

// 4. API executor with empty conversation ID runs isolated turn without --conversation arg
func TestSession_APIExecutor_EmptyConversationID_OmitsConversationArg(t *testing.T) {
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	var observedCmd *exec.Cmd
	agyCmdRunner = func(cmd *exec.Cmd) error {
		observedCmd = cmd
		resp := AgyResponse{Status: "SUCCESS", Response: "isolated response"}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	res, err := executeAgy(context.Background(), "hello", "", AgyCallOptions{
		Profile: ProfileAPI,
	})
	if err != nil {
		t.Fatalf("expected execution to succeed with empty conversation ID, got error: %v", err)
	}
	if res.Text != "isolated response" {
		t.Fatalf("expected text 'isolated response', got %q", res.Text)
	}
	for i, arg := range observedCmd.Args {
		if arg == "--conversation" {
			t.Fatalf("expected --conversation to be omitted when ID is empty, found at index %d", i)
		}
	}
}

// 5. ProfilePlanner args remain unchanged (plan, sandbox, disable-slash-commands)
func TestSession_ProfilePlanner_ArgsUnchanged(t *testing.T) {
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	var observedCmd *exec.Cmd
	agyCmdRunner = func(cmd *exec.Cmd) error {
		observedCmd = cmd
		resp := AgyResponse{Status: "SUCCESS", Response: "planner result"}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	plannerID := "planner-conv-uuid"
	_, err := executeAgy(context.Background(), "plan cron", plannerID, AgyCallOptions{
		Profile: ProfilePlanner,
	})
	if err != nil {
		t.Fatalf("unexpected planner execution error: %v", err)
	}

	wantArgs := []string{
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--print-timeout", "5m",
		"--mode", "plan",
		"--sandbox",
		"--disable-slash-commands",
		"--conversation", plannerID,
	}
	if !reflect.DeepEqual(observedCmd.Args[1:], wantArgs) {
		t.Fatalf("ProfilePlanner args mismatch:\ngot  %v\nwant %v", observedCmd.Args[1:], wantArgs)
	}
}

// 6. ProfileInteractive args remain unchanged
func TestSession_ProfileInteractive_ArgsUnchanged(t *testing.T) {
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	var observedCmd *exec.Cmd
	agyCmdRunner = func(cmd *exec.Cmd) error {
		observedCmd = cmd
		resp := AgyResponse{Status: "SUCCESS", Response: "interactive result"}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	interactiveID := "interactive-conv-uuid"
	_, err := executeAgy(context.Background(), "chat query", interactiveID, AgyCallOptions{
		Profile: ProfileInteractive,
	})
	if err != nil {
		t.Fatalf("unexpected interactive execution error: %v", err)
	}

	wantArgs := []string{
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--print-timeout", "5m",
		"--conversation", interactiveID,
	}
	if !reflect.DeepEqual(observedCmd.Args[1:], wantArgs) {
		t.Fatalf("ProfileInteractive args mismatch:\ngot  %v\nwant %v", observedCmd.Args[1:], wantArgs)
	}
}

// 7. Session resume failure triggers fallback or explicit error
func TestSession_ResumeFailure_TriggersCompactionFallback(t *testing.T) {
	server, _, _ := setupTestServer(t)
	testConvID := "resumed-session-id"
	server.SetConversationIDProvider(func() string { return testConvID })

	// Pre-mark session as synced so server attempts a resumed turn
	server.syncTracker.MarkSessionSynced(testConvID, "dummy-history-hash", 4)

	callCount := 0
	var receivedPrompts []string

	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })
	agyCmdRunner = func(cmd *exec.Cmd) error {
		callCount++
		if cmd.Stdin != nil {
			var b bytes.Buffer
			b.ReadFrom(cmd.Stdin)
			receivedPrompts = append(receivedPrompts, b.String())
		}

		if callCount == 1 {
			// First attempt (resumed turn): simulate AGY stderr indicating session not found
			cmd.Stderr.Write([]byte("warning: conversation \"resumed-session-id\" not found\n"))
			resp := AgyResponse{Status: "SUCCESS", Response: "unintended new conversation"}
			b, _ := json.Marshal(resp)
			cmd.Stdout.Write(b)
			return nil
		}

		// Second attempt (fallback compaction): succeeds
		resp := AgyResponse{Status: "SUCCESS", Response: "recovered via fallback"}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "What is 2+2?"},
			{"role": "assistant", "content": "4"},
			{"role": "user", "content": "What about 3+3?"}
		]
	}`

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK via fallback recovery, got %d: %s", rec.Code, rec.Body.String())
	}
	if callCount != 2 {
		t.Fatalf("expected 2 attempts (initial resume + compaction fallback), got %d", callCount)
	}

	// First attempt was current-turn only (did not have past transcript)
	if strings.Contains(receivedPrompts[0], "What is 2+2?") {
		t.Errorf("expected attempt 1 to omit past transcript, got:\n%s", receivedPrompts[0])
	}
	// Second attempt was structured compaction fallback (contained past transcript)
	if !strings.Contains(receivedPrompts[1], "What is 2+2?") {
		t.Errorf("expected attempt 2 (fallback) to include compacted past transcript, got:\n%s", receivedPrompts[1])
	}
}

// 8. Client cancellation triggers agy process tree cleanup
func TestSession_ClientCancellation_TriggersProcessCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	cleanupCalled := false
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })
	agyCmdRunner = func(cmd *exec.Cmd) error {
		// Verify cmd.Cancel is registered and callable
		if cmd.Cancel == nil {
			t.Fatalf("cmd.Cancel must be registered for process cleanup")
		}
		// Cancel parent context
		cancel()
		cleanupCalled = true
		return ctx.Err()
	}

	_, _ = executeAgy(ctx, "test prompt", "conv-test", AgyCallOptions{
		Profile: ProfileAPI,
	})

	select {
	case <-ctx.Done():
		// Verified context was canceled
	case <-time.After(time.Second):
		t.Fatalf("expected context cancellation")
	}

	if !cleanupCalled {
		t.Fatalf("expected cleanup / cancel path to execute")
	}
}
