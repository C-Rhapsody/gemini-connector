package main

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// TestStructuredTerminalOverridesProgress verifies that when intermediate progress/explanation
// deltas are emitted, but a terminal structured_output is provided, the terminal structured output
// is authoritative and progress deltas do not leak into or override the final structured response.
func TestStructuredTerminalOverridesProgress(t *testing.T) {
	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "return json"}],
		"response_format": {"type": "json_object"}
	}`

	progressLine := makeStreamUpdateLine("Calculating answer...")
	terminalResult := `{"event":"result","result":{"status":"SUCCESS","response":"","structured_output":{"result":42}}}`

	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(progressLine, terminalResult))
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

	content := ""
	if resp.Choices[0].Message.Content != nil {
		if s, ok := resp.Choices[0].Message.Content.(string); ok {
			content = s
		}
	}

	if strings.Contains(content, "Calculating answer") {
		t.Errorf("progress text leaked into structured response: %q", content)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("expected valid JSON content in structured output, got: %q (err: %v)", content, err)
	}
	if parsed["result"] != float64(42) {
		t.Errorf("expected result 42, got %v", parsed["result"])
	}
}

// TestNativeExecutionWithFinalText verifies that when AGY executes tools natively and produces
// a valid final text response, the connector returns 200 OK with the final text, and does NOT
// return unexecuted tool_calls back to the client.
func TestNativeExecutionWithFinalText(t *testing.T) {
	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "list directory"}],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "list_files",
					"parameters": {"type": "object"}
				}
			}
		]
	}`

	// Simulate AGY executing native tools and then producing a final text response
	nativeStepLine := `{"event":"step_update","step_update":{"step_type":"tool_call"}}`
	finalLine := `{"event":"result","result":{"status":"SUCCESS","response":"Found 3 files: a.txt, b.txt, c.txt","steps":[{"step_type":"tool_call"}]}}`

	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(nativeStepLine, finalLine))
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
	if ch.FinishReason != "stop" {
		t.Errorf("expected finish_reason 'stop', got %q", ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) > 0 {
		t.Errorf("expected no tool calls for already executed native tool, got %v", ch.Message.ToolCalls)
	}
	content, _ := ch.Message.Content.(string)
	if !strings.Contains(content, "Found 3 files") {
		t.Errorf("expected final text containing 'Found 3 files', got %q", content)
	}
}

// TestNoOutputParity verifies that an empty upstream response is categorized consistently
// as an error (upstream_no_output), whereas an output made empty by a stop sequence is treated
// as a normal completion with empty content and finish_reason="stop".
func TestNoOutputParity(t *testing.T) {
	t.Run("EmptyUpstreamIsError", func(t *testing.T) {
		body := `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hello"}]}`
		// AGY returns SUCCESS but with completely empty response and no steps
		rec, raw := runContractRequest(t, body, func(cmd *exec.Cmd) error {
			resp := AgyResponse{Status: "SUCCESS", Response: ""}
			b, _ := json.Marshal(resp)
			cmd.Stdout.Write(b)
			return nil
		})

		if rec.Code != http.StatusBadGateway {
			t.Errorf("expected 502 Bad Gateway for empty upstream output, got %d: %s", rec.Code, raw)
		}
		if !strings.Contains(raw, "upstream_no_output") {
			t.Errorf("expected error code 'upstream_no_output', got %s", raw)
		}
	})

	t.Run("StopSequenceEmptyContentIsNormalStop", func(t *testing.T) {
		body := `{
			"model":"gemini-3.8-flash-high",
			"messages":[{"role":"user","content":"hello"}],
			"stop":["<STOP>"]
		}`
		// Response starts with stop sequence immediately
		rec, raw := runContractRequest(t, body, func(cmd *exec.Cmd) error {
			resp := AgyResponse{Status: "SUCCESS", Response: "<STOP>rest of message"}
			b, _ := json.Marshal(resp)
			cmd.Stdout.Write(b)
			return nil
		})

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for stop sequence truncation, got %d: %s", rec.Code, raw)
		}
		var resp ChatCompletionResponse
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			t.Fatalf("failed to parse response JSON: %v", err)
		}
		if len(resp.Choices) != 1 {
			t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
		}
		if resp.Choices[0].FinishReason != "stop" {
			t.Errorf("expected finish_reason 'stop', got %q", resp.Choices[0].FinishReason)
		}
		content, ok := resp.Choices[0].Message.Content.(string)
		if !ok || content != "" {
			t.Errorf("expected empty string content, got %v", resp.Choices[0].Message.Content)
		}
	})
}
