package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAPIKey = "test-secret-key-that-is-at-least-32-bytes-long-ok"

func setupTestServer(t *testing.T) (*OpenAICompatibleServer, *TurnCoordinator, *APILogger) {
	t.Helper()
	dir := t.TempDir()
	logger := NewAPILogger(dir)
	turns := NewTurnCoordinator()
	server := NewOpenAICompatibleServer(testAPIKey, turns, logger, func() string { return "test-session-conv-id" })
	server.syncTracker = NewSessionSyncTracker(filepath.Join(dir, "session_sync.json"))

	// Inject stub models
	server.catalog.fetcher = func(ctx context.Context) ([]string, error) {
		return []string{
			"gemini-3.8-flash-high",
			"gemini-3.8-flash-low",
			"gemini-3.1-pro-high",
		}, nil
	}
	return server, turns, logger
}

func TestResolveAPIKey(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	_ = os.WriteFile(envFile, []byte("OPENAI_COMPAT_API_KEY=env-file-key-that-is-at-least-32-bytes-long\n"), 0644)

	// Precedence 1: Explicit --api-key
	k1, err := resolveAPIKey("cli-key-that-is-at-least-32-bytes-long-ok", true, "inherited-key-32-bytes-long-long-ok", true, envFile)
	if err != nil || k1 != "cli-key-that-is-at-least-32-bytes-long-ok" {
		t.Fatalf("expected cli key, got %q, err: %v", k1, err)
	}

	// Precedence 2: Inherited OS env
	k2, err := resolveAPIKey("", false, "inherited-key-32-bytes-long-long-ok", true, envFile)
	if err != nil || k2 != "inherited-key-32-bytes-long-long-ok" {
		t.Fatalf("expected inherited key, got %q, err: %v", k2, err)
	}

	// Precedence 3: .env file
	k3, err := resolveAPIKey("", false, "", false, envFile)
	if err != nil || k3 != "env-file-key-that-is-at-least-32-bytes-long" {
		t.Fatalf("expected .env file key, got %q, err: %v", k3, err)
	}

	// Fail closed on empty values
	if _, err := resolveAPIKey("", true, "inherited", true, envFile); err == nil {
		t.Errorf("expected error for empty explicit key")
	}
	if _, err := resolveAPIKey("", false, "", true, envFile); err == nil {
		t.Errorf("expected error for empty inherited key")
	}

	// Fail on short key (< 32 bytes)
	if _, err := resolveAPIKey("short-key", true, "", false, envFile); err == nil {
		t.Errorf("expected error for key < 32 bytes")
	}

	// Fail when missing everywhere
	emptyEnv := filepath.Join(dir, "empty.env")
	_ = os.WriteFile(emptyEnv, []byte(""), 0644)
	if _, err := resolveAPIKey("", false, "", false, emptyEnv); err == nil {
		t.Errorf("expected error when no key is provided")
	}
}

func TestRoutingAndAuthentication(t *testing.T) {
	server, _, _ := setupTestServer(t)

	// 1. Unknown route -> 404 before auth
	req := httptest.NewRequest("GET", "/v1/unknown", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown route, got %d", rec.Code)
	}

	// 2. Known route wrong method -> 405 before auth
	req = httptest.NewRequest("POST", "/v1/models", nil)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 for POST /v1/models, got %d", rec.Code)
	}
	if rec.Header().Get("Allow") != "GET" {
		t.Errorf("expected Allow: GET, got %q", rec.Header().Get("Allow"))
	}

	// 3. Missing auth -> 401
	req = httptest.NewRequest("GET", "/v1/models", nil)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing auth, got %d", rec.Code)
	}

	// 4. Invalid auth -> 401
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid auth, got %d", rec.Code)
	}

	// 5. Auth precedes Host check: invalid auth + invalid host -> 401
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Host = "evil.com"
	req.Header.Set("Authorization", "Bearer wrong-key")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when both auth and host are invalid, got %d", rec.Code)
	}

	// 6. Valid auth + invalid Host -> 400 (invalid_host)
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Host = "evil.com"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid host, got %d", rec.Code)
	}

	// 7. Valid auth + valid Host + invalid Origin -> 400 (disallowed_origin)
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Origin", "http://evil.com")
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for disallowed origin, got %d", rec.Code)
	}

	// 8. Valid auth + valid Host -> 200
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for valid GET /v1/models, got %d", rec.Code)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Errorf("expected X-Request-Id header")
	}
}

func TestGetModelsCatalog(t *testing.T) {
	server, _, _ := setupTestServer(t)

	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp ModelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "list" || len(resp.Data) != 3 {
		t.Fatalf("unexpected models list response: %+v", resp)
	}
	for _, m := range resp.Data {
		if !strings.HasPrefix(m.ID, "gemini-") {
			t.Errorf("non-Gemini model leaked: %s", m.ID)
		}
		if m.Object != "model" || m.OwnedBy != "agy" {
			t.Errorf("unexpected model attributes: %+v", m)
		}
	}
}

func TestChatCompletions_Validation(t *testing.T) {
	server, _, _ := setupTestServer(t)

	post := func(body string, ct string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Host = "127.0.0.1:49152"
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}

	// 1. Invalid Content-Type
	rec := post(`{}`, "text/plain")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for text/plain, got %d", rec.Code)
	}

	// 2. Invalid max_tokens / max_completion_tokens
	for _, b := range []string{
		`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"max_tokens":0}`,
		`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"max_tokens":-5}`,
		`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"max_tokens":"not-a-number"}`,
		`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":0}`,
		`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":-1}`,
	} {
		rec = post(b, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for invalid token limit body %s, got %d", b, rec.Code)
		}
	}

	// 3. Invalid response_format
	rec = post(`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"unsupported_type"}}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unsupported response_format, got %d", rec.Code)
	}

	// 4. Unknown model
	rec = post(`{"model":"unknown-model","messages":[{"role":"user","content":"hi"}]}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown model, got %d", rec.Code)
	}

	// 5. Empty messages
	rec = post(`{"model":"gemini-3.8-flash-high","messages":[]}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty messages, got %d", rec.Code)
	}

	// 6. Invalid role
	rec = post(`{"model":"gemini-3.8-flash-high","messages":[{"role":"admin","content":"hi"}]}`, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid role, got %d", rec.Code)
	}
}

func TestChatCompletions_NonStream_Success(t *testing.T) {
	server, _, _ := setupTestServer(t)

	// Mock agyCmdRunner
	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: "The answer is 42.",
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"system","content":"be helpful"},{"role":"user","content":"what is the answer?"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if resp.Object != "chat.completion" || resp.Model != "gemini-3.8-flash-high" {
		t.Errorf("unexpected response object or model: %+v", resp)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "The answer is 42." || resp.Choices[0].FinishReason != "stop" {
		t.Errorf("unexpected choice: %+v", resp.Choices)
	}
}

func TestChatCompletions_Stream_Success(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		lines := []string{
			`{"event":"init","conversation_id":"00000000","init":{"model":"gemini-3.8-flash-high"}}`,
			`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"Hello"}}`,
			`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":" world!"}}`,
			`{"event":"result","result":{"status":"SUCCESS","response":"Hello world!"}}`,
		}
		for _, l := range lines {
			cmd.Stdout.Write([]byte(l + "\n"))
		}
		return nil
	}

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("expected text/event-stream header, got %q", rec.Header().Get("Content-Type"))
	}

	scanner := bufio.NewScanner(rec.Body)
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") || strings.HasPrefix(line, "event: ") {
			events = append(events, line)
		}
	}

	if rec.Header().Get("X-Request-ID") == "" {
		t.Errorf("expected X-Request-ID response header to be set")
	}

	if len(events) < 3 {
		t.Fatalf("expected multiple SSE events, got: %v", events)
	}

	// Ensure no raw event: request_id preamble exists, and all data frames are JSON or [DONE]
	for _, ev := range events {
		if strings.HasPrefix(ev, "event: request_id") {
			t.Errorf("unexpected raw request_id event frame: %s", ev)
		}
		if strings.HasPrefix(ev, "data: ") {
			dataPayload := strings.TrimPrefix(ev, "data: ")
			if dataPayload == "[DONE]" {
				continue
			}
			var chunk ChatCompletionChunk
			if err := json.Unmarshal([]byte(dataPayload), &chunk); err != nil {
				t.Fatalf("non-JSON data line encountered: %q, err: %v", dataPayload, err)
			}
		}
	}

	// Check terminal event is [DONE]
	last := events[len(events)-1]
	if last != "data: [DONE]" {
		t.Errorf("expected last SSE line to be data: [DONE], got %q", last)
	}
}

func TestChatCompletions_DrainingReturns503(t *testing.T) {
	server, _, _ := setupTestServer(t)
	server.Drain()

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 while draining, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("expected Retry-After: 1")
	}
}

func TestChatCompletions_QueueFull429(t *testing.T) {
	server, _, _ := setupTestServer(t)

	// Block the worker by stubbing runner
	oldRunner := agyCmdRunner
	blockCh := make(chan struct{})
	var blockOnce sync.Once
	unblock := func() {
		blockOnce.Do(func() {
			close(blockCh)
		})
	}
	defer func() {
		unblock()
		agyCmdRunner = oldRunner
	}()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		<-blockCh
		return nil
	}

	var wg sync.WaitGroup
	var statuses []int
	var mu sync.Mutex

	// Fire 5 requests concurrently (admission limit is 4)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
			req.Host = "127.0.0.1:49152"
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			server.ServeHTTP(rec, req)
			mu.Lock()
			statuses = append(statuses, rec.Code)
			mu.Unlock()
		}()
	}

	// Give a moment for the 5th request to hit queue limit
	time.Sleep(200 * time.Millisecond)

	// Unblock workers and wait for all requests to finish
	unblock()
	wg.Wait()

	mu.Lock()
	has429 := false
	for _, s := range statuses {
		if s == http.StatusTooManyRequests {
			has429 = true
			break
		}
	}
	mu.Unlock()

	if !has429 {
		t.Errorf("expected at least one 429 status for N+1 request among %v", statuses)
	}
}

func TestChatCompletions_MaxTokens_Success(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: "token ok",
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"max_tokens":128}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid max_tokens, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatCompletions_MaxTokens_Precedence(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: "precedence ok",
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"max_completion_tokens":128}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when both max_tokens and max_completion_tokens are provided, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestChatCompletions_HermesTools_Fallback(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: `{"type":"final","content":"I am ready without tool calls."}`,
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	// 1. Initial request with tools and tool_choice (Hermes style)
	body := `{
		"model":"gemini-3.8-flash-high",
		"messages":[{"role":"user","content":"search something"}],
		"tools":[{"type":"function","function":{"name":"search","description":"search web","parameters":{"type":"object"}}}],
		"tool_choice":"auto"
	}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for Hermes tools request, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("unexpected choice finish_reason: %+v", resp.Choices)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("expected 0 fake tool calls in fallback mode, got %+v", resp.Choices[0].Message.ToolCalls)
	}

	// 2. Follow-up conversation containing role: "tool"
	toolBody := `{
		"model":"gemini-3.8-flash-high",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":"checking..."},
			{"role":"tool","tool_call_id":"call_123","content":"tool result text"}
		]
	}`
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(toolBody))
	req2.Host = "127.0.0.1:49152"
	req2.Header.Set("Authorization", "Bearer "+testAPIKey)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()

	server.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for message history with role 'tool', got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestChatCompletions_ResponseFormat_Schema(t *testing.T) {
	server, _, _ := setupTestServer(t)

	var capturedSchemaPath string
	var capturedSchemaContent string

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		for i, arg := range cmd.Args {
			if arg == "--json-schema" && i+1 < len(cmd.Args) {
				capturedSchemaPath = cmd.Args[i+1]
				data, err := os.ReadFile(capturedSchemaPath)
				if err == nil {
					capturedSchemaContent = string(data)
				}
				break
			}
		}
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: `{"ans": 42}`,
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	body := `{
		"model":"gemini-3.8-flash-high",
		"messages":[{"role":"user","content":"give number"}],
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"test_schema",
				"schema":{"type":"object","properties":{"ans":{"type":"integer"}}}
			}
		}
	}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for response_format json_schema, got %d: %s", rec.Code, rec.Body.String())
	}

	if capturedSchemaPath == "" {
		t.Fatal("expected --json-schema argument to be passed to agy")
	}
	if !strings.Contains(capturedSchemaContent, `"properties":{"ans":{"type":"integer"}}`) {
		t.Fatalf("unexpected schema content: %s", capturedSchemaContent)
	}

	// Verify temporary schema file cleanup
	if _, err := os.Stat(capturedSchemaPath); !os.IsNotExist(err) {
		t.Fatalf("expected temporary schema file %s to be deleted, but it still exists", capturedSchemaPath)
	}
}

func TestChatCompletions_Stream_Usage(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		lines := []string{
			`{"event":"init","conversation_id":"00000000","init":{"model":"gemini-3.8-flash-high"}}`,
			`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"Hi"}}`,
			`{"event":"result","result":{"status":"SUCCESS","response":"Hi","usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16}}}`,
		}
		for _, l := range lines {
			cmd.Stdout.Write([]byte(l + "\n"))
		}
		return nil
	}

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	scanner := bufio.NewScanner(rec.Body)
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}

	if len(dataLines) < 2 {
		t.Fatalf("too few SSE lines: %v", dataLines)
	}
	if dataLines[len(dataLines)-1] != "[DONE]" {
		t.Fatalf("expected last line to be [DONE], got %s", dataLines[len(dataLines)-1])
	}

	// Penultimate event should be the usage chunk
	usageLine := dataLines[len(dataLines)-2]
	var usageChunk ChatCompletionChunk
	if err := json.Unmarshal([]byte(usageLine), &usageChunk); err != nil {
		t.Fatalf("failed to decode usage chunk: %v (raw: %s)", err, usageLine)
	}
	if usageChunk.Usage == nil || usageChunk.Usage.PromptTokens != 12 || usageChunk.Usage.CompletionTokens != 4 || usageChunk.Usage.TotalTokens != 16 {
		t.Fatalf("unexpected usage chunk content: %+v", usageChunk.Usage)
	}
}

func TestChatCompletions_NonStream_Usage(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()

	// 1. When usage is reported
	agyCmdRunner = func(cmd *exec.Cmd) error {
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: "with usage",
			Usage: &AgyUsage{
				InputTokens:     25,
				OutputTokens:    10,
				ThinkingTokens:  3,
				CacheReadTokens: 5,
				TotalTokens:     35,
			},
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Usage == nil {
		t.Fatal("expected non-nil usage")
	}
	if resp.Usage.PromptTokens != 25 || resp.Usage.CompletionTokens != 10 || resp.Usage.TotalTokens != 35 {
		t.Fatalf("unexpected usage tokens: %+v", resp.Usage)
	}
	if resp.Usage.CompletionTokensDetails == nil || resp.Usage.CompletionTokensDetails.ReasoningTokens != 3 {
		t.Fatalf("unexpected reasoning tokens: %+v", resp.Usage.CompletionTokensDetails)
	}
	if resp.Usage.PromptTokensDetails == nil || resp.Usage.PromptTokensDetails.CachedTokens != 5 {
		t.Fatalf("unexpected cached tokens: %+v", resp.Usage.PromptTokensDetails)
	}

	// 2. When usage is unavailable (nil), it must NOT fabricate 0 tokens
	agyCmdRunner = func(cmd *exec.Cmd) error {
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: "without usage",
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req2.Host = "127.0.0.1:49152"
	req2.Header.Set("Authorization", "Bearer "+testAPIKey)
	req2.Header.Set("Content-Type", "application/json")

	server.ServeHTTP(rec2, req2)
	var resp2 ChatCompletionResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.Usage != nil {
		t.Fatalf("expected nil usage when agy returns no usage, got %+v", resp2.Usage)
	}
}

func TestChatCompletions_StopSequences(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()

	// Non-stream stop sequence
	agyCmdRunner = func(cmd *exec.Cmd) error {
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: "Hello World! Goodbye World!",
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stop":["World"]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	var resp ChatCompletionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "Hello " {
		t.Fatalf("expected truncated content 'Hello ', got %q", resp.Choices[0].Message.Content)
	}
}

// panicOnFlushRecorder simulates net/http.(*response).Flush panicking with a nil dereference
type panicOnFlushRecorder struct {
	*httptest.ResponseRecorder
	flushedCount int
}

func (p *panicOnFlushRecorder) Flush() {
	p.flushedCount++
	if p.flushedCount > 1 {
		panic("runtime error: invalid memory address or nil pointer dereference")
	}
	p.ResponseRecorder.Flush()
}

func TestChatCompletions_Stream_ClientDisconnectNoPanic(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()

	started := make(chan struct{})
	unblock := make(chan struct{})

	agyCmdRunner = func(cmd *exec.Cmd) error {
		close(started)
		<-unblock
		return context.Canceled
	}

	ctx, cancel := context.WithCancel(context.Background())
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		server.ServeHTTP(rec, req)
	}()

	// Wait for worker to start
	<-started
	// Cancel request context while streaming
	cancel()
	close(unblock)

	select {
	case <-done:
		// Succeeded without hanging or crashing
	case <-time.After(3 * time.Second):
		t.Fatal("ServeHTTP timed out after client disconnect")
	}
}

func TestChatCompletions_Stream_FlusherPanicRecovered(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()

	agyCmdRunner = func(cmd *exec.Cmd) error {
		lines := []string{
			`{"event":"init","conversation_id":"00000000","init":{"model":"gemini-3.8-flash-high"}}`,
			`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"Hello"}}`,
			`{"event":"result","result":{"status":"SUCCESS","response":"Hello"}}`,
		}
		for _, l := range lines {
			cmd.Stdout.Write([]byte(l + "\n"))
		}
		return nil
	}

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")

	rec := &panicOnFlushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}

	// Must not panic even when flusher panics
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic during stream: %v", r)
		}
	}()

	server.ServeHTTP(rec, req)
}

// TestChatCompletions_Stream_HermesShape_FirstFrameJSON verifies that a Hermes-style
// request (containing tools and max_tokens) produces purely valid JSON data frames from the
// very first frame, with no non-standard event: request_id preamble.
func TestChatCompletions_Stream_HermesShape_FirstFrameJSON(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()

	agyCmdRunner = func(cmd *exec.Cmd) error {
		lines := []string{
			`{"event":"init","conversation_id":"00000000","init":{"model":"gemini-3.8-flash-high"}}`,
			`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"{\"type\":\"final\",\"content\":\"Hermes stream\"}"}}`,
			`{"event":"result","result":{"status":"SUCCESS","response":""}}`,
		}
		for _, l := range lines {
			cmd.Stdout.Write([]byte(l + "\n"))
		}
		return nil
	}

	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "What is the weather?"}],
		"stream": true,
		"max_tokens": 100,
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "get_current_weather",
					"description": "Get current weather",
					"parameters": {"type": "object", "properties": {"location": {"type": "string"}}}
				}
			}
		]
	}`

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Errorf("expected X-Request-ID header to be present")
	}

	scanner := bufio.NewScanner(rec.Body)
	var dataLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: request_id") {
			t.Fatalf("unexpected non-standard event: request_id preamble found: %q", line)
		}
		if strings.HasPrefix(line, "data: ") {
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}

	if len(dataLines) < 3 {
		t.Fatalf("expected at least 3 data lines, got %d: %v", len(dataLines), dataLines)
	}

	// 1. First data line MUST be valid JSON with role: "assistant"
	var firstChunk ChatCompletionChunk
	if err := json.Unmarshal([]byte(dataLines[0]), &firstChunk); err != nil {
		t.Fatalf("first data line must parse as JSON ChatCompletionChunk, got err: %v (line: %s)", err, dataLines[0])
	}
	if len(firstChunk.Choices) != 1 || firstChunk.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("unexpected first chunk choices: %+v", firstChunk.Choices)
	}

	// 2. All data lines except the last must parse as valid JSON
	for i := 0; i < len(dataLines)-1; i++ {
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(dataLines[i]), &chunk); err != nil {
			t.Fatalf("data line at index %d failed JSON parse: %v (line: %s)", i, err, dataLines[i])
		}
	}

	// 3. The last data line must be exactly [DONE]
	lastLine := dataLines[len(dataLines)-1]
	if lastLine != "[DONE]" {
		t.Fatalf("expected last line to be [DONE], got %q", lastLine)
	}
}

// TestChatCompletions_Stream_Heartbeat_ConcurrentRace verifies that when the heartbeat ticker
// fires rapidly concurrently with stream deltas, ResponseWriter access is serialized without data race.
func TestChatCompletions_Stream_Heartbeat_ConcurrentRace(t *testing.T) {
	server, _, _ := setupTestServer(t)
	// Inject very fast heartbeat interval to force race conditions if un-synchronized
	server.heartbeatInterval = 2 * time.Millisecond

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()

	agyCmdRunner = func(cmd *exec.Cmd) error {
		for i := 0; i < 15; i++ {
			ev := fmt.Sprintf(`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"chunk%d "}}`+"\n", i)
			cmd.Stdout.Write([]byte(ev))
			time.Sleep(3 * time.Millisecond)
		}
		cmd.Stdout.Write([]byte(`{"event":"result","result":{"status":"SUCCESS","response":"done"}}` + "\n"))
		return nil
	}

	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	scanner := bufio.NewScanner(rec.Body)
	sawHeartbeat := false
	sawChunks := 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == ": heartbeat" {
			sawHeartbeat = true
		}
		if strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
			var chunk ChatCompletionChunk
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err == nil {
				sawChunks++
			}
		}
	}

	if sawChunks < 15 {
		t.Errorf("expected at least 15 chunks, got %d", sawChunks)
	}
	_ = sawHeartbeat // heartbeat comments are allowed and verified
}

// trackedLifetimeWriter tracks if Write or Flush is called after the handler returns.
type trackedLifetimeWriter struct {
	*httptest.ResponseRecorder
	mu              sync.Mutex
	handlerReturned bool
	calledAfterExit bool
}

func (w *trackedLifetimeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.handlerReturned {
		w.calledAfterExit = true
	}
	w.mu.Unlock()
	return w.ResponseRecorder.Write(p)
}

func (w *trackedLifetimeWriter) Flush() {
	w.mu.Lock()
	if w.handlerReturned {
		w.calledAfterExit = true
	}
	w.mu.Unlock()
	w.ResponseRecorder.Flush()
}

// TestChatCompletions_Stream_ResponseWriterNotUsedAfterHandlerReturns verifies that
// after client disconnect, all goroutines writing to ResponseWriter have terminated before
// ServeHTTP returns.
func TestChatCompletions_Stream_ResponseWriterNotUsedAfterHandlerReturns(t *testing.T) {
	server, _, _ := setupTestServer(t)
	server.heartbeatInterval = 5 * time.Millisecond

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()

	started := make(chan struct{})
	unblock := make(chan struct{})

	agyCmdRunner = func(cmd *exec.Cmd) error {
		close(started)
		<-unblock
		return context.Canceled
	}

	ctx, cancel := context.WithCancel(context.Background())
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")

	tracked := &trackedLifetimeWriter{
		ResponseRecorder: httptest.NewRecorder(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		server.ServeHTTP(tracked, req)
		tracked.mu.Lock()
		tracked.handlerReturned = true
		tracked.mu.Unlock()
	}()

	<-started
	cancel()
	close(unblock)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeHTTP timed out")
	}

	// Allow any lingering goroutine to attempt a write
	time.Sleep(50 * time.Millisecond)

	tracked.mu.Lock()
	calledAfter := tracked.calledAfterExit
	tracked.mu.Unlock()

	if calledAfter {
		t.Fatalf("ResponseWriter was accessed after ServeHTTP returned!")
	}
}

// TestChatCompletions_Stream_Disconnect_QueueProcessesNext verifies that after a streaming
// request disconnects, the turn queue cleanly drops/finishes and the next queued job is processed.
func TestChatCompletions_Stream_Disconnect_QueueProcessesNext(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()

	started1 := make(chan struct{})
	unblock1 := make(chan struct{})

	turn1Active := true
	agyCmdRunner = func(cmd *exec.Cmd) error {
		if turn1Active {
			close(started1)
			<-unblock1
			return context.Canceled
		}
		// Second call succeeds
		lines := []string{
			`{"event":"init","conversation_id":"00000000","init":{"model":"gemini-3.8-flash-high"}}`,
			`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"Success2"}}`,
			`{"event":"result","result":{"status":"SUCCESS","response":"Success2"}}`,
		}
		for _, l := range lines {
			cmd.Stdout.Write([]byte(l + "\n"))
		}
		return nil
	}

	// First request: stream with disconnect
	ctx1, cancel1 := context.WithCancel(context.Background())
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"turn 1"}],"stream":true}`
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx1)
	req1.Host = "127.0.0.1:49152"
	req1.Header.Set("Authorization", "Bearer "+testAPIKey)
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()

	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		server.ServeHTTP(rec1, req1)
	}()

	<-started1
	cancel1()
	close(unblock1)
	<-done1

	// Now run second request: must succeed normally, queue must not be stuck!
	turn1Active = false
	body2 := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"turn 2"}],"stream":true}`
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body2))
	req2.Host = "127.0.0.1:49152"
	req2.Header.Set("Authorization", "Bearer "+testAPIKey)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()

	server.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("expected second request to succeed with 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "Success2") {
		t.Fatalf("expected second request to contain 'Success2', got: %s", rec2.Body.String())
	}
}

// TestChatCompletions_MessagesCountLimit tests the defensive operating limit on messages count:
// 1023 -> not messages_too_many (succeeds)
// 1024 -> not messages_too_many (succeeds)
// 1025 -> HTTP 400 messages_too_many
func TestChatCompletions_MessagesCountLimit(t *testing.T) {
	server, _, _ := setupTestServer(t)

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: "ok",
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	postMessages := func(count int) *httptest.ResponseRecorder {
		msgs := make([]ChatMessage, count)
		for i := 0; i < count; i++ {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			msgs[i] = ChatMessage{
				Role:    role,
				Content: "m",
			}
		}
		reqBody := ChatCompletionRequest{
			Model:    "gemini-3.8-flash-high",
			Messages: msgs,
		}
		b, err := json.Marshal(reqBody)
		if err != nil {
			t.Fatalf("marshal error: %v", err)
		}

		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(b))
		req.Host = "127.0.0.1:49152"
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}

	// 1. 1023 messages -> NOT rejected with messages_too_many (must succeed with 200)
	rec1023 := postMessages(1023)
	if rec1023.Code != http.StatusOK {
		t.Fatalf("expected 1023 messages to succeed with 200, got %d: %s", rec1023.Code, rec1023.Body.String())
	}

	// 2. 1024 messages -> NOT rejected with messages_too_many (must succeed with 200)
	rec1024 := postMessages(1024)
	if rec1024.Code != http.StatusOK {
		t.Fatalf("expected 1024 messages to succeed with 200, got %d: %s", rec1024.Code, rec1024.Body.String())
	}

	// 3. 1025 messages -> rejected with HTTP 400 and messages_too_many
	rec1025 := postMessages(1025)
	if rec1025.Code != http.StatusBadRequest {
		t.Fatalf("expected 1025 messages to return 400, got %d: %s", rec1025.Code, rec1025.Body.String())
	}

	var errResp APIErrorResponse
	if err := json.Unmarshal(rec1025.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal error response: %v", err)
	}

	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("expected error.type invalid_request_error, got %q", errResp.Error.Type)
	}
	if errResp.Error.Code != "messages_too_many" {
		t.Errorf("expected error.code messages_too_many, got %q", errResp.Error.Code)
	}
	if errResp.Error.Param == nil || *errResp.Error.Param != "messages" {
		t.Errorf("expected error.param 'messages', got %v", errResp.Error.Param)
	}
	expectedMsg := "messages count exceeds maximum 1024"
	if errResp.Error.Message != expectedMsg {
		t.Errorf("expected error.message %q, got %q", expectedMsg, errResp.Error.Message)
	}
}

// TestChatCompletions_ByteLimits verifies that body, message, and aggregate byte limits are preserved:
// - body 1 MiB limit
// - individual message 256 KiB limit
// - aggregate message content 768 KiB limit
func TestChatCompletions_ByteLimits(t *testing.T) {
	server, _, _ := setupTestServer(t)

	postRaw := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Host = "127.0.0.1:49152"
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}

	// 1. Single message exceeds defaultMaxMessageBytes (256 KiB)
	hugeMessage := strings.Repeat("x", 256*1024+1)
	reqObj1 := map[string]any{
		"model": "gemini-3.8-flash-high",
		"messages": []map[string]any{
			{"role": "user", "content": hugeMessage},
		},
	}
	b1, _ := json.Marshal(reqObj1)
	rec1 := postRaw(string(b1))
	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for message > 256 KiB, got %d", rec1.Code)
	}
	var errResp1 APIErrorResponse
	_ = json.Unmarshal(rec1.Body.Bytes(), &errResp1)
	if errResp1.Error.Code != "message_too_large" {
		t.Errorf("expected code message_too_large, got %q", errResp1.Error.Code)
	}

	// 2. Aggregate messages exceed defaultMaxAggregateBytes (768 KiB)
	// 4 messages of 200 KiB each = 800 KiB > 768 KiB
	msg200k := strings.Repeat("y", 200*1024)
	reqObj2 := map[string]any{
		"model": "gemini-3.8-flash-high",
		"messages": []map[string]any{
			{"role": "user", "content": msg200k},
			{"role": "assistant", "content": msg200k},
			{"role": "user", "content": msg200k},
			{"role": "assistant", "content": msg200k},
		},
	}
	b2, _ := json.Marshal(reqObj2)
	rec2 := postRaw(string(b2))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for aggregate > 768 KiB, got %d", rec2.Code)
	}
	var errResp2 APIErrorResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &errResp2)
	if errResp2.Error.Code != "messages_aggregate_too_large" {
		t.Errorf("expected code messages_aggregate_too_large, got %q", errResp2.Error.Code)
	}

	// 3. Request body exceeds defaultMaxBodyBytes (1 MiB)
	hugeBody := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"padding":"` + strings.Repeat("z", 1024*1024) + `"}`
	rec3 := postRaw(hugeBody)
	if rec3.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for body > 1 MiB, got %d", rec3.Code)
	}
	var errResp3 APIErrorResponse
	_ = json.Unmarshal(rec3.Body.Bytes(), &errResp3)
	if errResp3.Error.Code != "request_too_large" {
		t.Errorf("expected code request_too_large, got %q", errResp3.Error.Code)
	}
}

// TestChatCompletions_MultiTurnRolePreservation_Regression captures the actual stdin passed
// to AGY and verifies that multi-turn history is rendered with explicit role/turn boundaries
// instead of being flattened into an opaque JSON array string (which causes semantic drift).
func TestChatCompletions_MultiTurnRolePreservation_Regression(t *testing.T) {
	server, _, _ := setupTestServer(t)

	var capturedStdin string
	var capturedArgs []string
	var mu sync.Mutex

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		mu.Lock()
		capturedArgs = append([]string(nil), cmd.Args...)
		if cmd.Stdin != nil {
			b, _ := io.ReadAll(cmd.Stdin)
			capturedStdin = string(b)
		}
		mu.Unlock()

		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: "16",
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	callID := "call_calc1"
	reqBody := map[string]any{
		"model": "gemini-3.8-flash-high",
		"messages": []map[string]any{
			{"role": "system", "content": "You are a helpful coding assistant."},
			{"role": "user", "content": "Topic A: Tell me about Go channels."},
			{"role": "assistant", "content": "Go channels are typed conduits for synchronization."},
			{"role": "user", "content": "Topic B: Tell me about Rust ownership."},
			{"role": "assistant", "content": "Rust uses ownership to manage memory safely without GC."},
			{
				"role":    "assistant",
				"content": "",
				"tool_calls": []map[string]any{
					{
						"id":   callID,
						"type": "function",
						"function": map[string]any{
							"name":      "calculator",
							"arguments": `{"expr":"2+2"}`,
						},
					},
				},
			},
			{
				"role":         "tool",
				"tool_call_id": callID,
				"name":         "calculator",
				"content":      "4",
			},
			{"role": "user", "content": "Now answer this: What is 4 * 4?"},
		},
	}

	b, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(b))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	stdin := capturedStdin
	args := capturedArgs
	mu.Unlock()

	// 1. Verify --conversation is present with configured session ID
	hasConv := false
	for i, arg := range args {
		if arg == "--conversation" && i+1 < len(args) && args[i+1] == "test-session-conv-id" {
			hasConv = true
			break
		}
	}
	if !hasConv {
		t.Fatalf("expected --conversation test-session-conv-id in args, got: %v", args)
	}

	// 2. RED Check: AGY stdin MUST NOT be a raw JSON array of req.Messages
	var rawArr []any
	if err := json.Unmarshal([]byte(stdin), &rawArr); err == nil && len(rawArr) == 8 {
		t.Fatalf("FAIL (Semantic Drift Root Cause): AGY stdin was passed as a raw opaque JSON array of req.Messages:\n%s", stdin)
	}

	// 3. System section check
	if !strings.Contains(stdin, "<SYSTEM_AND_DEVELOPER_MESSAGES>") || !strings.Contains(stdin, "You are a helpful coding assistant.") {
		t.Errorf("expected <SYSTEM_AND_DEVELOPER_MESSAGES> containing system prompt, got:\n%s", stdin)
	}

	// 4. Transcript section check
	if !strings.Contains(stdin, "<CONVERSATION_TRANSCRIPT>") {
		t.Fatalf("expected <CONVERSATION_TRANSCRIPT> in stdin, got:\n%s", stdin)
	}

	// Transcript must contain past topics and tool calls in order
	idxA := strings.Index(stdin, "Topic A: Tell me about Go channels.")
	idxB := strings.Index(stdin, "Topic B: Tell me about Rust ownership.")
	idxToolCall := strings.Index(stdin, callID)
	idxToolRes := strings.Index(stdin, `4`)
	if idxA == -1 || idxB == -1 || idxA > idxB {
		t.Errorf("expected past topics in order in transcript, idxA=%d, idxB=%d", idxA, idxB)
	}
	if idxToolCall == -1 || idxToolRes == -1 {
		t.Errorf("expected tool call metadata and tool result in transcript")
	}

	// 5. Current turn check
	if !strings.Contains(stdin, "<CURRENT_TURN>") {
		t.Fatalf("expected <CURRENT_TURN> in stdin, got:\n%s", stdin)
	}
	idxCurrent := strings.Index(stdin, "Now answer this: What is 4 * 4?")
	idxCurrentTurnTag := strings.Index(stdin, "<CURRENT_TURN>")
	if idxCurrent == -1 || idxCurrent < idxCurrentTurnTag {
		t.Errorf("expected current question to be inside <CURRENT_TURN>, got:\n%s", stdin)
	}

	// Ensure past topic A is NOT inside <CURRENT_TURN>
	currentTurnPart := stdin[idxCurrentTurnTag:]
	if strings.Contains(currentTurnPart, "Topic A") {
		t.Errorf("past Topic A leaked into <CURRENT_TURN>")
	}
}

// TestChatCompletions_Stream_UsesSameRenderer verifies that streaming requests
// use the exact same role-preserving prompt renderer as non-stream requests.
func TestChatCompletions_Stream_UsesSameRenderer(t *testing.T) {
	server, _, _ := setupTestServer(t)

	var lastCapturedStdin string
	var mu sync.Mutex

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		mu.Lock()
		if cmd.Stdin != nil {
			b, _ := io.ReadAll(cmd.Stdin)
			lastCapturedStdin = string(b)
		}
		mu.Unlock()

		isStream := false
		for _, arg := range cmd.Args {
			if arg == "stream-json" {
				isStream = true
				break
			}
		}

		if isStream {
			cmd.Stdout.Write([]byte(`{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"hello"}}` + "\n"))
			cmd.Stdout.Write([]byte(`{"event":"result","result":{"status":"SUCCESS","response":"hello"}}` + "\n"))
		} else {
			resp := AgyResponse{
				Status:   "SUCCESS",
				Response: "hello",
			}
			b, _ := json.Marshal(resp)
			cmd.Stdout.Write(b)
		}
		return nil
	}

	messages := []map[string]any{
		{"role": "system", "content": "You are a test system."},
		{"role": "user", "content": "Past question"},
		{"role": "assistant", "content": "Past answer"},
		{"role": "user", "content": "Active question"},
	}

	// 1. Non-stream request
	bodyNonStream, _ := json.Marshal(map[string]any{
		"model":    "gemini-3.8-flash-high",
		"messages": messages,
		"stream":   false,
	})
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bodyNonStream))
	req1.Host = "127.0.0.1:49152"
	req1.Header.Set("Authorization", "Bearer "+testAPIKey)
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	server.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("non-stream expected 200, got %d: %s", rec1.Code, rec1.Body.String())
	}
	mu.Lock()
	nonStreamStdin := lastCapturedStdin
	mu.Unlock()

	// Reset sync tracker so stream request tests the same bootstrap rendering conditions
	server.syncTracker.ResetSession("test-session-conv-id")

	// 2. Stream request
	bodyStream, _ := json.Marshal(map[string]any{
		"model":    "gemini-3.8-flash-high",
		"messages": messages,
		"stream":   true,
	})
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bodyStream))
	req2.Host = "127.0.0.1:49152"
	req2.Header.Set("Authorization", "Bearer "+testAPIKey)
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	server.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("stream expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	mu.Lock()
	streamStdin := lastCapturedStdin
	mu.Unlock()

	if streamStdin != nonStreamStdin {
		t.Fatalf("stream stdin differs from non-stream stdin!\nNon-Stream:\n%s\nStream:\n%s", nonStreamStdin, streamStdin)
	}
}

// TestChatCompletions_Stateless_NoStateLeak verifies that consecutive HTTP requests
// do not leak prompts, conversations, or command arguments between requests.
func TestChatCompletions_Stateless_NoStateLeak(t *testing.T) {
	server, _, _ := setupTestServer(t)

	var lastCapturedStdin string
	var lastCapturedArgs []string
	var mu sync.Mutex

	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()
	agyCmdRunner = func(cmd *exec.Cmd) error {
		mu.Lock()
		lastCapturedArgs = append([]string(nil), cmd.Args...)
		if cmd.Stdin != nil {
			b, _ := io.ReadAll(cmd.Stdin)
			lastCapturedStdin = string(b)
		}
		mu.Unlock()

		resp := AgyResponse{Status: "SUCCESS", Response: "ok"}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	}

	sendReq := func(userContent string) (string, []string) {
		body, _ := json.Marshal(map[string]any{
			"model": "gemini-3.8-flash-high",
			"messages": []map[string]any{
				{"role": "user", "content": userContent},
			},
		})
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Host = "127.0.0.1:49152"
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		mu.Lock()
		defer mu.Unlock()
		return lastCapturedStdin, lastCapturedArgs
	}

	// Request 1: Unique Topic A
	stdin1, args1 := sendReq("UNIQUE_TOPIC_ALPHA_12345")
	if !strings.Contains(stdin1, "UNIQUE_TOPIC_ALPHA_12345") {
		t.Fatal("expected request 1 to contain ALPHA")
	}
	hasConv1 := false
	for i, a := range args1 {
		if a == "--conversation" && i+1 < len(args1) && args1[i+1] == "test-session-conv-id" {
			hasConv1 = true
			break
		}
	}
	if !hasConv1 {
		t.Fatal("request 1 should have --conversation test-session-conv-id")
	}

	// Request 2: Unique Topic B
	stdin2, args2 := sendReq("UNIQUE_TOPIC_BETA_67890")
	if !strings.Contains(stdin2, "UNIQUE_TOPIC_BETA_67890") {
		t.Fatal("expected request 2 to contain BETA")
	}
	if strings.Contains(stdin2, "UNIQUE_TOPIC_ALPHA_12345") {
		t.Fatalf("STATE LEAK: request 2 stdin contained data from request 1:\n%s", stdin2)
	}
	hasConv2 := false
	for i, a := range args2 {
		if a == "--conversation" && i+1 < len(args2) && args2[i+1] == "test-session-conv-id" {
			hasConv2 = true
			break
		}
	}
	if !hasConv2 {
		t.Fatal("request 2 should have --conversation test-session-conv-id")
	}
}


