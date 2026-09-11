package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func makeStreamUpdateLine(delta string) string {
	b, _ := json.Marshal(delta)
	return `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":` + string(b) + `}}`
}

func TestToolChatHandler_NonStream_SingleToolCall(t *testing.T) {
	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "What is the weather in Seoul?"}],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "get_weather",
					"description": "Get current weather",
					"parameters": {"type": "object", "properties": {"location": {"type": "string"}}, "required": ["location"]}
				}
			}
		],
		"tool_choice": "auto"
	}`

	agyEnvelope := `{"type": "tool_call", "calls": [{"name": "get_weather", "arguments": {"location": "Seoul"}}]}`
	rec, raw := runContractRequest(t, body, mockAgyJSONRunner(AgyResponse{
		Status:   "SUCCESS",
		Response: agyEnvelope,
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, raw)
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}

	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	ch := resp.Choices[0]
	if ch.FinishReason != "tool_calls" {
		t.Errorf("expected finish_reason 'tool_calls', got %q", ch.FinishReason)
	}
	if ch.Message.Content != nil {
		t.Errorf("expected content to be null, got %v", ch.Message.Content)
	}
	if len(ch.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(ch.Message.ToolCalls))
	}
	tc := ch.Message.ToolCalls[0]
	if tc.Function.Name != "get_weather" {
		t.Errorf("expected tool name 'get_weather', got %q", tc.Function.Name)
	}
	if !strings.Contains(tc.Function.Arguments, "Seoul") {
		t.Errorf("expected arguments to contain 'Seoul', got %q", tc.Function.Arguments)
	}
	if !strings.HasPrefix(tc.ID, "call_") {
		t.Errorf("expected call ID to start with 'call_', got %q", tc.ID)
	}
}

func TestToolChatHandler_NonStream_FinalResponse(t *testing.T) {
	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "Hi"}],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "get_weather"
				}
			}
		]
	}`

	agyEnvelope := `{"type": "final", "content": "Hello there! How can I help you today?"}`
	rec, raw := runContractRequest(t, body, mockAgyJSONRunner(AgyResponse{
		Status:   "SUCCESS",
		Response: agyEnvelope,
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, raw)
	}

	var resp ChatCompletionResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}

	ch := resp.Choices[0]
	if ch.FinishReason != "stop" {
		t.Errorf("expected finish_reason 'stop', got %q", ch.FinishReason)
	}
	contentStr, ok := ch.Message.Content.(string)
	if !ok || contentStr != "Hello there! How can I help you today?" {
		t.Errorf("expected content string 'Hello there! How can I help you today?', got %v", ch.Message.Content)
	}
	if len(ch.Message.ToolCalls) > 0 {
		t.Errorf("expected no tool calls for final response, got %v", ch.Message.ToolCalls)
	}
}

func TestToolChatHandler_Stream_ToolCall_IndexedDeltasAndNoContentLeak(t *testing.T) {
	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "Check weather in Seoul"}],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "get_weather",
					"parameters": {"type": "object", "properties": {"location": {"type": "string"}}}
				}
			}
		],
		"stream": true
	}`

	agyEnvelope := `{"type":"tool_call","calls":[{"name":"get_weather","arguments":{"location":"Seoul"}}]}`

	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(
		makeStreamUpdateLine(agyEnvelope[:20]),
		makeStreamUpdateLine(agyEnvelope[20:]),
		`{"event":"result","result":{"status":"SUCCESS","response":""}}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, raw)
	}

	// Tool intent must NEVER leak into content deltas
	if strings.Contains(raw, `"content":"{"`) || strings.Contains(raw, `"content":"tool_call"`) {
		t.Fatalf("tool intent raw JSON leaked into content delta:\n%s", raw)
	}

	// Must contain indexed tool_calls
	if !strings.Contains(raw, `"tool_calls"`) {
		t.Fatalf("stream must contain tool_calls delta, got:\n%s", raw)
	}
	if !strings.Contains(raw, `"name":"get_weather"`) {
		t.Fatalf("stream missing function name get_weather, got:\n%s", raw)
	}
	if !strings.Contains(raw, "Seoul") {
		t.Fatalf("stream missing arguments content 'Seoul', got:\n%s", raw)
	}
	if !strings.Contains(raw, `"finish_reason":"tool_calls"`) {
		t.Fatalf("stream missing finish_reason 'tool_calls', got:\n%s", raw)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Fatalf("stream must terminate with data: [DONE], got:\n%s", raw)
	}
}

func TestToolChatHandler_Stream_FinalResponse(t *testing.T) {
	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "Hi"}],
		"tools": [
			{
				"type": "function",
				"function": {"name": "get_weather"}
			}
		],
		"stream": true
	}`

	agyEnvelope := `{"type":"final","content":"Greetings human!"}`

	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(
		makeStreamUpdateLine(agyEnvelope),
		`{"event":"result","result":{"status":"SUCCESS","response":""}}`,
	))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, raw)
	}

	if !strings.Contains(raw, `"content":"Greetings human!"`) {
		t.Fatalf("expected unwrapped content 'Greetings human!', got:\n%s", raw)
	}
	if strings.Contains(raw, `{"type":"final"`) {
		t.Fatalf("raw envelope JSON leaked into content, got:\n%s", raw)
	}
	if !strings.Contains(raw, `"finish_reason":"stop"`) {
		t.Fatalf("expected finish_reason 'stop', got:\n%s", raw)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Fatalf("expected data: [DONE], got:\n%s", raw)
	}
}

func TestToolChatHandler_InvalidEnvelopeError(t *testing.T) {
	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "Test"}],
		"tools": [{"type": "function", "function": {"name": "f1"}}]
	}`

	// Malformed envelope (not matching discriminated union)
	rec, raw := runContractRequest(t, body, mockAgyJSONRunner(AgyResponse{
		Status:   "SUCCESS",
		Response: "I cannot answer this with a tool envelope.",
	}))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for malformed envelope, got %d: %s", rec.Code, raw)
	}
	if !strings.Contains(raw, "invalid_tool_envelope") {
		t.Errorf("expected error code 'invalid_tool_envelope', got:\n%s", raw)
	}
}
