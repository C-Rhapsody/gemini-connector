package main

import (
	"encoding/json"
	"testing"
)

func TestCompletionResultTerminal(t *testing.T) {
	t.Run("TerminalStructuredOutputOverridesProgressDeltas", func(t *testing.T) {
		ndjson := []byte("{\"event\":\"step_update\",\"step_update\":{\"step_type\":\"message\",\"text_delta\":\"progress calculating...\"}}\n" +
			"{\"event\":\"result\",\"result\":{\"status\":\"SUCCESS\",\"response\":\"\",\"structured_output\":{\"answer\":42}}}\n")

		res, err := ParseAgyExecutionOutput(ndjson, nil, ParsedToolChoice{Mode: "none"}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.IsError {
			t.Fatalf("expected success, got error: %s (%s)", res.ErrorCode, res.ErrorMessage)
		}
		if res.Text != `{"answer":42}` {
			t.Errorf("expected structured output to override progress, got: %q", res.Text)
		}
		if res.FinishReason != "stop" {
			t.Errorf("expected finish_reason 'stop', got %q", res.FinishReason)
		}
	})

	t.Run("NativeToolExecutionProducesFinalTextWithoutClientToolCalls", func(t *testing.T) {
		ndjson := []byte("{\"event\":\"step_update\",\"step_update\":{\"step_type\":\"tool_call\"}}\n" +
			"{\"event\":\"result\",\"result\":{\"status\":\"SUCCESS\",\"response\":\"Directory listing: file1.txt, file2.txt\",\"steps\":[{\"step_type\":\"tool_call\"}]}}\n")

		tools := []ToolDefinition{{Type: "function", Function: &FunctionDefinition{Name: "list_files"}}}
		res, err := ParseAgyExecutionOutput(ndjson, tools, ParsedToolChoice{Mode: "auto"}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.IsError {
			t.Fatalf("expected success, got error: %s", res.ErrorCode)
		}
		if !res.SawNativeTool {
			t.Errorf("expected SawNativeTool to be true")
		}
		if len(res.ToolCalls) > 0 {
			t.Errorf("expected no client tool calls for native execution, got: %v", res.ToolCalls)
		}
		if res.Text != "Directory listing: file1.txt, file2.txt" {
			t.Errorf("expected final text, got: %q", res.Text)
		}
		if res.FinishReason != "stop" {
			t.Errorf("expected finish_reason 'stop', got %q", res.FinishReason)
		}
	})

	t.Run("ClientToolCallEnvelopeParsed", func(t *testing.T) {
		envelope := `{"type":"tool_call","calls":[{"id":"call_1","name":"lookup","arguments":{"query":"test"}}]}`
		jsonInput, _ := json.Marshal(AgyResponse{
			Status:   "SUCCESS",
			Response: envelope,
		})

		tools := []ToolDefinition{{Type: "function", Function: &FunctionDefinition{Name: "lookup"}}}
		res, err := ParseAgyExecutionOutput(jsonInput, tools, ParsedToolChoice{Mode: "auto"}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.IsError {
			t.Fatalf("expected success, got error: %s", res.ErrorCode)
		}
		if len(res.ToolCalls) != 1 {
			t.Fatalf("expected 1 tool call, got %d", len(res.ToolCalls))
		}
		if res.ToolCalls[0].Function.Name != "lookup" {
			t.Errorf("expected function name 'lookup', got %q", res.ToolCalls[0].Function.Name)
		}
		if res.FinishReason != "tool_calls" {
			t.Errorf("expected finish_reason 'tool_calls', got %q", res.FinishReason)
		}
		if res.Text != "" {
			t.Errorf("expected empty text for tool call, got %q", res.Text)
		}
	})

	t.Run("EmptyUpstreamIsError502", func(t *testing.T) {
		jsonInput, _ := json.Marshal(AgyResponse{
			Status:   "SUCCESS",
			Response: "",
		})

		res, err := ParseAgyExecutionOutput(jsonInput, nil, ParsedToolChoice{Mode: "none"}, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected error for empty response, got success")
		}
		if res.ErrorCode != "upstream_no_output" {
			t.Errorf("expected error code 'upstream_no_output', got %q", res.ErrorCode)
		}
		if res.HTTPStatus != 502 {
			t.Errorf("expected HTTP status 502, got %d", res.HTTPStatus)
		}
	})

	t.Run("StopSequenceTruncationIsNormalStop", func(t *testing.T) {
		jsonInput, _ := json.Marshal(AgyResponse{
			Status:   "SUCCESS",
			Response: "<STOP>rest of message",
		})

		res, err := ParseAgyExecutionOutput(jsonInput, nil, ParsedToolChoice{Mode: "none"}, []string{"<STOP>"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.IsError {
			t.Fatalf("expected normal stop, got error: %s", res.ErrorCode)
		}
		if res.Text != "" {
			t.Errorf("expected empty string text, got %q", res.Text)
		}
		if !res.WasTruncatedByStop {
			t.Errorf("expected WasTruncatedByStop to be true")
		}
		if res.FinishReason != "stop" {
			t.Errorf("expected finish_reason 'stop', got %q", res.FinishReason)
		}
	})
}
