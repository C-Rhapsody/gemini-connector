package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// TestStandardChatWithoutConversationID verifies that standard Chat Completions
// do not require an active conversation ID provider (no messenger dependency).
func TestStandardChatWithoutConversationID(t *testing.T) {
	server, _, _ := setupTestServer(t)
	// Explicitly clear conversation ID provider
	server.SetConversationIDProvider(func() string { return "" })

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hello stateless"}]}`
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	agyCmdRunner = func(cmd *exec.Cmd) error {
		// Verify that --conversation argument is omitted when conversation ID is empty
		for i, arg := range cmd.Args {
			if arg == "--conversation" {
				t.Fatalf("expected --conversation to be omitted for standard chat, found at index %d", i)
			}
		}
		resp := AgyResponse{Status: "SUCCESS", Response: "Hello from isolated AGY!"}
		b := mustJSON(resp)
		cmd.Stdout.Write([]byte(b))
		return nil
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK without conversation ID, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Hello from isolated AGY!") {
		t.Errorf("expected response to contain greeting, got: %s", rec.Body.String())
	}
}

// TestStandardChatFullHistoryPreserved verifies that multi-turn history is preserved
// in the prompt and not truncated or summarized by server-side compaction.
func TestStandardChatFullHistoryPreserved(t *testing.T) {
	server, _, _ := setupTestServer(t)
	server.SetConversationIDProvider(func() string { return "" })

	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [
			{"role": "system", "content": "You are assistant"},
			{"role": "user", "content": "Question 1"},
			{"role": "assistant", "content": "Answer 1"},
			{"role": "user", "content": "Question 2"}
		]
	}`

	var observedPrompt string
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	agyCmdRunner = func(cmd *exec.Cmd) error {
		if cmd.Stdin != nil {
			var b strings.Builder
			var buf [1024]byte
			for {
				n, _ := cmd.Stdin.Read(buf[:])
				if n == 0 {
					break
				}
				b.Write(buf[:n])
			}
			observedPrompt = b.String()
		}
		resp := AgyResponse{Status: "SUCCESS", Response: "Answer 2"}
		cmd.Stdout.Write([]byte(mustJSON(resp)))
		return nil
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify all messages are present in the prompt
	for _, expected := range []string{"You are assistant", "Question 1", "Answer 1", "Question 2"} {
		if !strings.Contains(observedPrompt, expected) {
			t.Errorf("expected observed prompt to contain %q, but was:\n%s", expected, observedPrompt)
		}
	}
}

// TestStandardChatHarnessNeutrality verifies that different client harnesses
// (e.g. Hermes, Cursor, Continue, etc.) receive the exact same treatment.
func TestStandardChatHarnessNeutrality(t *testing.T) {
	server, _, _ := setupTestServer(t)
	server.SetConversationIDProvider(func() string { return "" })

	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	agyCmdRunner = func(cmd *exec.Cmd) error {
		resp := AgyResponse{Status: "SUCCESS", Response: "Neutral response"}
		cmd.Stdout.Write([]byte(mustJSON(resp)))
		return nil
	}

	userAgents := []string{
		"hermes-agent/1.0",
		"openai-python/1.12.0",
		"curl/8.4.0",
		"custom-harness/2.0",
	}

	for _, ua := range userAgents {
		body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"neutral query"}]}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Host = "127.0.0.1:49152"
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		rec := httptest.NewRecorder()

		server.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("harness %q failed with status %d: %s", ua, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Neutral response") {
			t.Errorf("harness %q expected 'Neutral response', got: %s", ua, rec.Body.String())
		}
	}
}

// TestStandardChatIsolation verifies that conversation A and conversation B remain
// strictly isolated: neither prompt nor conversation session leaks across requests.
func TestStandardChatIsolation(t *testing.T) {
	server, _, _ := setupTestServer(t)
	server.SetConversationIDProvider(func() string { return "" })

	var mu sync.Mutex
	capturedPrompts := make(map[string]string)
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	agyCmdRunner = func(cmd *exec.Cmd) error {
		// Verify no --conversation is passed
		for i, a := range cmd.Args {
			if a == "--conversation" {
				t.Fatalf("unexpected --conversation flag in standard chat at index %d", i)
			}
		}
		var p string
		if cmd.Stdin != nil {
			var b strings.Builder
			var buf [1024]byte
			for {
				n, _ := cmd.Stdin.Read(buf[:])
				if n == 0 {
					break
				}
				b.Write(buf[:n])
			}
			p = b.String()
		}
		mu.Lock()
		if strings.Contains(p, "TOPIC_A") {
			capturedPrompts["A"] = p
		}
		if strings.Contains(p, "TOPIC_B") {
			capturedPrompts["B"] = p
		}
		mu.Unlock()

		resp := AgyResponse{Status: "SUCCESS", Response: "Isolated reply"}
		cmd.Stdout.Write([]byte(mustJSON(resp)))
		return nil
	}

	sendReq := func(topic string, ua string) {
		body := fmt.Sprintf(`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"Discuss %s"}]}`, topic)
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Host = "127.0.0.1:49152"
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("Content-Type", "application/json")
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request for %s failed with status %d: %s", topic, rec.Code, rec.Body.String())
		}
	}

	// Request A from Hermes harness
	sendReq("TOPIC_A", "hermes-agent/1.0")

	// Request B from OpenAI SDK harness
	sendReq("TOPIC_B", "openai-python/1.12.0")

	mu.Lock()
	promptA := capturedPrompts["A"]
	promptB := capturedPrompts["B"]
	mu.Unlock()

	if strings.Contains(promptB, "TOPIC_A") {
		t.Fatalf("STATE LEAK: Prompt B contains TOPIC_A from conversation A:\n%s", promptB)
	}
	if strings.Contains(promptA, "TOPIC_B") {
		t.Fatalf("STATE LEAK: Prompt A contains TOPIC_B from conversation B:\n%s", promptA)
	}
}

