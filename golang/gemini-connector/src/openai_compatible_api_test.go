package main

import (
	"bufio"
	"context"
	"encoding/json"
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
	server := NewOpenAICompatibleServer(testAPIKey, turns, logger)

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

	if len(events) < 4 {
		t.Fatalf("expected multiple SSE events, got: %v", events)
	}

	// Check preamble
	if events[0] != "event: request_id" {
		t.Errorf("expected first event to be request_id preamble, got %q", events[0])
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
			Response: "I am ready without tool calls.",
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
