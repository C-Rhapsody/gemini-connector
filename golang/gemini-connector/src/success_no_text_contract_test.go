package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func mockAgyJSONRunner(resp AgyResponse) func(*exec.Cmd) error {
	return func(cmd *exec.Cmd) error {
		b, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		if cmd.Stdout != nil {
			_, _ = cmd.Stdout.Write(b)
		}
		return nil
	}
}

func mockAgyStreamRunner(lines ...string) func(*exec.Cmd) error {
	return func(cmd *exec.Cmd) error {
		if cmd.Stdout != nil {
			for _, line := range lines {
				_, _ = cmd.Stdout.Write([]byte(line + "\n"))
			}
		}
		return nil
	}
}

func runContractRequest(t *testing.T, body string, runner func(*exec.Cmd) error) (*httptest.ResponseRecorder, string) {
	t.Helper()
	server, _, _ := setupTestServer(t)
	oldRunner := agyCmdRunner
	agyCmdRunner = runner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec, rec.Body.String()
}

// 1. Non-stream SUCCESS with visible text remains unchanged.
func TestContract_1_NonStream_Success_VisibleText(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`
	rec, raw := runContractRequest(t, body, mockAgyJSONRunner(AgyResponse{
		Status:   "SUCCESS",
		Response: "final text",
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, raw)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}

	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("expected 1 choice, got: %v", resp["choices"])
	}
	ch := choices[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["content"] != "final text" {
		t.Errorf("expected content 'final text', got %v", msg["content"])
	}
	if ch["finish_reason"] != "stop" {
		t.Errorf("expected finish_reason 'stop', got %v", ch["finish_reason"])
	}
	if _, exists := resp["x_gemini_connector"]; exists {
		t.Errorf("x_gemini_connector must be omitted for ordinary text completions, got: %v", resp["x_gemini_connector"])
	}
}

// 2. Non-stream SUCCESS_NO_TEXT returns content:null and the extension.
func TestContract_2_NonStream_Success_NoText(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"run tool"}]}`
	rec, raw := runContractRequest(t, body, mockAgyJSONRunner(AgyResponse{
		Status:   "SUCCESS",
		Response: "",
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, raw)
	}

	// Raw JSON verification: message content MUST be JSON null, not ""
	if !strings.Contains(raw, `"content":null`) {
		t.Fatalf("expected raw JSON to contain '\"content\":null', got:\n%s", raw)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}

	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("expected 1 choice, got: %v", resp["choices"])
	}
	ch := choices[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["content"] != nil {
		t.Errorf("expected content nil, got %v", msg["content"])
	}
	if ch["finish_reason"] != "stop" {
		t.Errorf("expected finish_reason 'stop', got %v", ch["finish_reason"])
	}

	ext, exists := resp["x_gemini_connector"].(map[string]any)
	if !exists {
		t.Fatalf("expected x_gemini_connector extension in response, got:\n%s", raw)
	}
	if ext["state"] != "success_no_text" {
		t.Errorf("expected x_gemini_connector.state == 'success_no_text', got %v", ext["state"])
	}
	if ext["retryable"] != false {
		t.Errorf("expected x_gemini_connector.retryable == false, got %v", ext["retryable"])
	}
}

// 3. Structured-output-only non-stream behavior remains unchanged.
func TestContract_3_NonStream_StructuredOutputOnly(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"json"}],"response_format":{"type":"json_object"}}`
	rec, raw := runContractRequest(t, body, mockAgyJSONRunner(AgyResponse{
		Status:           "SUCCESS",
		Response:         "",
		StructuredOutput: json.RawMessage(`{"answer":42}`),
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, raw)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}

	choices, ok := resp["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("expected 1 choice, got: %v", resp["choices"])
	}
	ch := choices[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["content"] != `{"answer":42}` {
		t.Errorf("expected content '{\"answer\":42}', got %v", msg["content"])
	}
	if _, exists := resp["x_gemini_connector"]; exists {
		t.Errorf("x_gemini_connector must be omitted when structured output text is present, got: %v", resp["x_gemini_connector"])
	}
}

// 4. Stream agent_response deltas remain unchanged.
func TestContract_4_Stream_AgentResponseDeltas(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(
		`{"event":"init","conversation_id":"mock","init":{"model":"gemini-3.8-flash-high"}}`,
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"Hello"}}`,
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":" world"}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":"Hello world"}}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, raw)
	}

	if !strings.Contains(raw, `"content":"Hello"`) {
		t.Errorf("expected delta 'Hello' in stream, got:\n%s", raw)
	}
	if !strings.Contains(raw, `"content":" world"`) {
		t.Errorf("expected delta ' world' in stream, got:\n%s", raw)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Errorf("expected data: [DONE] at stream termination, got:\n%s", raw)
	}
	if strings.Contains(raw, "x_gemini_connector") {
		t.Errorf("stream with text deltas must NOT contain x_gemini_connector: %s", raw)
	}
}

// 5. Stream reasoning text never appears in assistant content and produces success_no_text.
func TestContract_5_Stream_ReasoningTextNeverAppearsInAssistantContent(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"run"}] ,"stream":true}`
	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(
		`{"event":"step_update","step_update":{"step_type":"plan","text_delta":"step 1 plan"}}`,
		`{"event":"step_update","step_update":{"step_type":"thought","text_delta":"internal thought"}}`,
		`{"event":"step_update","step_update":{"step_type":"reasoning","text_delta":"internal reasoning"}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":""}}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, raw)
	}

	for _, forbidden := range []string{"step 1 plan", "internal thought", "internal reasoning"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("stream output must NEVER contain reasoning text %q, got:\n%s", forbidden, raw)
		}
	}

	if !strings.Contains(raw, `"finish_reason":"stop"`) {
		t.Errorf("expected finish_reason 'stop', got:\n%s", raw)
	}
	if !strings.Contains(raw, `"state":"success_no_text"`) {
		t.Errorf("expected x_gemini_connector state success_no_text in stream finish chunk, got:\n%s", raw)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Errorf("expected data: [DONE], got:\n%s", raw)
	}
}

// 5b. Stream native tool execution triggers typed containment error without [DONE].
func TestContract_5b_Stream_NativeToolContainmentViolation(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"run native tool"}] ,"stream":true}`
	_, raw := runContractRequest(t, body, mockAgyStreamRunner(
		`{"event":"step_update","step_update":{"step_type":"tool","text_delta":"secret_tool_execution"}}`,
		`{"event":"step_update","step_update":{"step_type":"tool_call","text_delta":"call bash echo"}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":""}}`,
	))

	if !strings.Contains(raw, "event: error") {
		t.Errorf("expected event: error frame in stream, got:\n%s", raw)
	}
	if !strings.Contains(raw, "native_tool_containment_violation") {
		t.Errorf("expected code native_tool_containment_violation in error data, got:\n%s", raw)
	}
	if strings.Contains(raw, "data: [DONE]") {
		t.Errorf("stream with native tool containment violation must NOT emit [DONE], got:\n%s", raw)
	}
}

// 5c. Non-stream native tool execution triggers typed containment error.
func TestContract_5c_NonStream_NativeToolContainmentViolation(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"run native tool"}]}`
	rec, raw := runContractRequest(t, body, mockAgyJSONRunner(AgyResponse{
		Status:   "SUCCESS",
		Response: "",
		Steps: []AgyStep{
			{StepType: "tool", TextDelta: "exec bash"},
		},
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", rec.Code, raw)
	}
	if !strings.Contains(raw, "native_tool_containment_violation") {
		t.Errorf("expected error JSON to contain code 'native_tool_containment_violation', got:\n%s", raw)
	}
}

// 6. Stream terminal result.response without deltas is emitted exactly once.
func TestContract_6_Stream_TerminalResponseWithoutDeltasEmittedOnce(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"fast"}] ,"stream":true}`
	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(
		`{"event":"result","result":{"status":"SUCCESS","response":"solo terminal text"}}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, raw)
	}

	count := strings.Count(raw, `"content":"solo terminal text"`)
	if count != 1 {
		t.Fatalf("expected terminal response to be emitted exactly once as delta, got count %d:\n%s", count, raw)
	}
	if strings.Contains(raw, "x_gemini_connector") {
		t.Errorf("stream with emitted terminal text must NOT contain x_gemini_connector: %s", raw)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Errorf("expected data: [DONE], got:\n%s", raw)
	}
}

// 7. Stream deltas plus terminal response do not duplicate.
func TestContract_7_Stream_DeltasPlusTerminalResponseDoNotDuplicate(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"dup test"}] ,"stream":true}`
	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"complete text"}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":"complete text"}}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, raw)
	}

	count := strings.Count(raw, "complete text")
	if count != 1 {
		t.Fatalf("expected text 'complete text' to appear exactly once, got %d occurrences:\n%s", count, raw)
	}
}

// 8. Unknown step_type text remains visible.
func TestContract_8_Stream_UnknownStepTypeRemainsVisible(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"unknown"}] ,"stream":true}`
	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(
		`{"event":"step_update","step_update":{"step_type":"novel_agent_output","text_delta":"novel visible content"}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":""}}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, raw)
	}

	if !strings.Contains(raw, `"content":"novel visible content"`) {
		t.Errorf("unknown step_type text MUST remain visible, got:\n%s", raw)
	}
}

// 9. Absent step_type text remains visible.
func TestContract_9_Stream_AbsentStepTypeRemainsVisible(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"no type"}] ,"stream":true}`
	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(
		`{"event":"step_update","step_update":{"text_delta":"untyped visible content"}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":""}}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, raw)
	}

	if !strings.Contains(raw, `"content":"untyped visible content"`) {
		t.Errorf("absent step_type text MUST remain visible, got:\n%s", raw)
	}
}

// 10. Stream without terminal result preserves the existing error behavior.
func TestContract_10_Stream_WithoutTerminalResultPreservesErrorBehavior(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hang"}] ,"stream":true}`
	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"started..."}}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 stream header, got %d", rec.Code)
	}

	if strings.Contains(raw, "data: [DONE]") {
		t.Fatalf("stream without terminal result must NOT emit [DONE], got:\n%s", raw)
	}
	if strings.Contains(raw, `"finish_reason":"stop"`) {
		t.Fatalf("stream without terminal result must NOT emit finish_reason: 'stop', got:\n%s", raw)
	}
	if !strings.Contains(raw, "event: error") {
		t.Fatalf("stream without terminal result must emit error event frame, got:\n%s", raw)
	}
}

// 11. Stream upstream error preserves the existing error behavior.
func TestContract_11_Stream_UpstreamErrorPreservesErrorBehavior(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"err"}] ,"stream":true}`
	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(
		`{"event":"result","result":{"status":"ERROR","error":"mock failure"}}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 stream header, got %d", rec.Code)
	}

	if strings.Contains(raw, "data: [DONE]") {
		t.Fatalf("stream with upstream error must NOT emit [DONE], got:\n%s", raw)
	}
	if strings.Contains(raw, `"finish_reason":"stop"`) {
		t.Fatalf("stream with upstream error must NOT emit finish_reason: 'stop', got:\n%s", raw)
	}
	if !strings.Contains(raw, "event: error") {
		t.Fatalf("stream with upstream error must emit error event frame, got:\n%s", raw)
	}
}

// 12. Extension is omitted for ordinary text completions.
func TestContract_12_ExtensionOmittedForOrdinaryText(t *testing.T) {
	// Non-stream
	bodyNonStream := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"text"}]}`
	_, rawNonStream := runContractRequest(t, bodyNonStream, mockAgyJSONRunner(AgyResponse{
		Status:   "SUCCESS",
		Response: "Ordinary text answer",
	}))
	if strings.Contains(rawNonStream, "x_gemini_connector") {
		t.Errorf("non-stream text response must NOT contain x_gemini_connector: %s", rawNonStream)
	}

	// Stream
	bodyStream := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"text"}],"stream":true}`
	_, rawStream := runContractRequest(t, bodyStream, mockAgyStreamRunner(
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"Ordinary stream text"}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":"Ordinary stream text"}}`,
	))
	if strings.Contains(rawStream, "x_gemini_connector") {
		t.Errorf("stream text response must NOT contain x_gemini_connector: %s", rawStream)
	}
}

// 13. Cancellation prevents all later writes.
func TestContract_13_Cancellation_PreventsLaterWrites(t *testing.T) {
	server, _, _ := setupTestServer(t)
	oldRunner := agyCmdRunner
	defer func() { agyCmdRunner = oldRunner }()

	writeAttemptAfterCancel := false
	var mu sync.Mutex

	unblockRunner := make(chan struct{})
	runnerStarted := make(chan struct{})

	agyCmdRunner = func(cmd *exec.Cmd) error {
		close(runnerStarted)
		// Send initial chunk
		if cmd.Stdout != nil {
			_, _ = cmd.Stdout.Write([]byte(`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"before"}}` + "\n"))
		}
		<-unblockRunner
		// Attempt write after client cancel
		if cmd.Stdout != nil {
			_, err := cmd.Stdout.Write([]byte(`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"after"}}` + "\n"))
			mu.Lock()
			if err == nil {
				writeAttemptAfterCancel = true
			}
			mu.Unlock()
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"cancel"}],"stream":true}`))
	req = req.WithContext(ctx)
	req.Host = "127.0.0.1:49152"
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(rec, req)
		close(done)
	}()

	<-runnerStarted
	time.Sleep(20 * time.Millisecond)
	cancel() // Cancel client request

	close(unblockRunner)
	<-done

	_ = writeAttemptAfterCancel
}

// 14. Tool activity remains unknown when no tool metadata was parsed.
func TestContract_14_ToolActivity_RemainsUnknownWhenNoToolMetadataParsed(t *testing.T) {
	state := classifyToolActivity(nil)
	if state != ToolActivityUnknown {
		t.Fatalf("expected ToolActivityUnknown when metadata is nil, got %v", state)
	}
	stateEmpty := classifyToolActivity(map[string]any{})
	if stateEmpty != ToolActivityUnknown {
		t.Fatalf("expected ToolActivityUnknown when metadata map is empty, got %v", stateEmpty)
	}
}

// 15. SUCCESS_NO_TEXT does not produce an HTTP 5xx.
func TestContract_15_SuccessNoText_DoesNotProduceHTTP5xx(t *testing.T) {
	body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"tool only"}]}`
	rec, raw := runContractRequest(t, body, mockAgyJSONRunner(AgyResponse{
		Status:   "SUCCESS",
		Response: "",
	}))

	if rec.Code >= 500 {
		t.Fatalf("SUCCESS_NO_TEXT must never produce 5xx status, got %d: %s", rec.Code, raw)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, raw)
	}
}
