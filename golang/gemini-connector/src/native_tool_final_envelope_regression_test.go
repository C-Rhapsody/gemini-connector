package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestNativeToolFinalEnvelopeRegression reproduces the defect where BuildCompletionResult
// bypasses ParseAndValidateAgyEnvelope when SawNativeTool is true, leaking the internal
// {"type":"final","content":"..."} envelope to the user.
func TestNativeToolFinalEnvelopeRegression(t *testing.T) {
	tools := []ToolDefinition{{Type: "function", Function: &FunctionDefinition{Name: "terminal"}}}
	parsedChoice := ParsedToolChoice{Mode: "auto"}

	t.Run("PureJSON_SawNativeToolTrue_ExtractsContent", func(t *testing.T) {
		rawEnvelope := `{"type":"final","content":"영상 요약"}`
		res := BuildCompletionResult(
			"SUCCESS",
			rawEnvelope,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true, // SawNativeTool = true
			nil,
			"conv-1",
			tools,
			parsedChoice,
			nil,
		)

		if res.IsError {
			t.Fatalf("expected success, got error: %s (%s)", res.ErrorCode, res.ErrorMessage)
		}
		if res.Text != "영상 요약" {
			t.Fatalf("FAILED: expected content '영상 요약' to be extracted, but got raw text: %q", res.Text)
		}
		if res.FinishReason != "stop" {
			t.Errorf("expected finish_reason 'stop', got %q", res.FinishReason)
		}
		if len(res.ToolCalls) > 0 {
			t.Errorf("expected no client tool calls, got: %v", res.ToolCalls)
		}
	})

	t.Run("JSONCodeBlock_SawNativeToolTrue_ExtractsContent", func(t *testing.T) {
		rawEnvelope := "```json\n{\n  \"type\": \"final\",\n  \"content\": \"영상 요약\"\n}\n```"
		res := BuildCompletionResult(
			"SUCCESS",
			rawEnvelope,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true, // SawNativeTool = true
			nil,
			"conv-1",
			tools,
			parsedChoice,
			nil,
		)

		if res.IsError {
			t.Fatalf("expected success, got error: %s (%s)", res.ErrorCode, res.ErrorMessage)
		}
		if res.Text != "영상 요약" {
			t.Fatalf("FAILED: expected content '영상 요약' to be extracted from codeblock, but got raw text: %q", res.Text)
		}
	})

	t.Run("ParityBetweenNativeToolTrueAndFalse", func(t *testing.T) {
		complexContent := "한국어 요약 🚀\n줄바꿈과 \"따옴표\" 및 ```go\nfmt.Println(\"안녕\")\n```"
		envObj := map[string]any{
			"type":    "final",
			"content": complexContent,
		}
		envBytes, _ := json.Marshal(envObj)
		rawEnvelope := string(envBytes)

		resWithoutNative := BuildCompletionResult(
			"SUCCESS",
			rawEnvelope,
			nil,
			nil,
			"",
			true,
			false, // SawNativeTool = false
			nil,
			"conv-1",
			tools,
			parsedChoice,
			nil,
		)
		resWithNative := BuildCompletionResult(
			"SUCCESS",
			rawEnvelope,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true, // SawNativeTool = true
			nil,
			"conv-1",
			tools,
			parsedChoice,
			nil,
		)

		if resWithoutNative.IsError {
			t.Fatalf("resWithoutNative failed: %s", resWithoutNative.ErrorMessage)
		}
		if resWithNative.IsError {
			t.Fatalf("resWithNative failed: %s", resWithNative.ErrorMessage)
		}

		if resWithoutNative.Text != complexContent {
			t.Errorf("resWithoutNative: expected %q, got %q", complexContent, resWithoutNative.Text)
		}
		if resWithNative.Text != complexContent {
			t.Errorf("resWithNative: expected %q, got %q", complexContent, resWithNative.Text)
		}
		if resWithNative.Text != resWithoutNative.Text {
			t.Errorf("parity mismatch: withNative=%q, withoutNative=%q", resWithNative.Text, resWithoutNative.Text)
		}
	})

	t.Run("NoToolsInRequest_UserJSONNeverUnwrapped", func(t *testing.T) {
		// When no tools are in request, connector did NOT request internal envelope.
		// User JSON resembling an envelope must NOT be unwrapped!
		userJSON := `{"type":"final","content":"사용자 데이터"}`
		res := BuildCompletionResult(
			"SUCCESS",
			userJSON,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true,
			nil,
			"conv-1",
			nil, // No tools!
			ParsedToolChoice{Mode: "none"},
			nil,
		)

		if res.IsError {
			t.Fatalf("expected success, got error: %s", res.ErrorCode)
		}
		if res.Text != userJSON {
			t.Errorf("user JSON must NOT be unwrapped when tools are absent, got: %q", res.Text)
		}
	})

	t.Run("MalformedEnvelope_SawNativeToolTrue_ReturnsErrorNotRawText", func(t *testing.T) {
		// When tools were provided and envelope is malformed, it must NOT leak as success text
		badEnvelope := `{"type":"unknown_type","content":"should not leak"}`
		res := BuildCompletionResult(
			"SUCCESS",
			badEnvelope,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true, // SawNativeTool = true
			nil,
			"conv-1",
			tools,
			parsedChoice,
			nil,
		)

		if !res.IsError {
			t.Fatalf("expected error for malformed envelope, but got success with Text: %q", res.Text)
		}
		if res.ErrorCode != "invalid_tool_envelope" {
			t.Errorf("expected error code 'invalid_tool_envelope', got %q", res.ErrorCode)
		}
	})

	t.Run("EmptyFinalContent_SawNativeToolTrue_ReturnsErrorNotSuccess", func(t *testing.T) {
		emptyEnv := `{"type":"final","content":""}`
		res := BuildCompletionResult(
			"SUCCESS",
			emptyEnv,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true,
			nil,
			"conv-1",
			tools,
			parsedChoice,
			nil,
		)

		if !res.IsError {
			t.Fatalf("expected error for empty final content with native tools, got success: %q", res.Text)
		}
		if res.ErrorCode != "native_tool_containment_violation" {
			t.Errorf("expected error code 'native_tool_containment_violation', got %q", res.ErrorCode)
		}
	})

	t.Run("NilFinalContent_SawNativeToolTrue_ReturnsErrorNotSuccess", func(t *testing.T) {
		nilContentEnv := `{"type":"final"}`
		res := BuildCompletionResult(
			"SUCCESS",
			nilContentEnv,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true,
			nil,
			"conv-1",
			tools,
			parsedChoice,
			nil,
		)

		if !res.IsError {
			t.Fatalf("expected error for nil final content with native tools, got success: %q", res.Text)
		}
		if res.ErrorCode != "native_tool_containment_violation" {
			t.Errorf("expected error code 'native_tool_containment_violation', got %q", res.ErrorCode)
		}
	})

	t.Run("NestedJSONInsideContentNotRecursivelyUnwrapped", func(t *testing.T) {
		nestedJSON := `{"type":"final","content":"{\"nested_key\":\"nested_value\"}"}`
		res := BuildCompletionResult(
			"SUCCESS",
			nestedJSON,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true,
			nil,
			"conv-1",
			tools,
			parsedChoice,
			nil,
		)

		if res.IsError {
			t.Fatalf("unexpected error: %s", res.ErrorMessage)
		}
		expected := `{"nested_key":"nested_value"}`
		if res.Text != expected {
			t.Errorf("expected %q, got %q", expected, res.Text)
		}
	})

	t.Run("NativeToolPlainResponsePreserved", func(t *testing.T) {
		plainText := "Directory listing: file1.txt, file2.txt"
		res := BuildCompletionResult(
			"SUCCESS",
			plainText,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true,
			nil,
			"conv-1",
			tools,
			parsedChoice,
			nil,
		)

		if res.IsError {
			t.Fatalf("unexpected error: %s", res.ErrorMessage)
		}
		if res.Text != plainText {
			t.Errorf("expected %q, got %q", plainText, res.Text)
		}
		if len(res.ToolCalls) > 0 {
			t.Errorf("expected no client tool calls, got: %v", res.ToolCalls)
		}
	})

	t.Run("ToolChoiceRequired_SawNativeToolTrue_RejectsFinalResponse", func(t *testing.T) {
		rawEnvelope := `{"type":"final","content":"done"}`
		choiceRequired := ParsedToolChoice{Mode: "required"}
		res := BuildCompletionResult(
			"SUCCESS",
			rawEnvelope,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true,
			nil,
			"conv-1",
			tools,
			choiceRequired,
			nil,
		)

		if !res.IsError {
			t.Fatalf("expected error when tool_choice is required but final returned, got success: %q", res.Text)
		}
	})

	t.Run("ToolChoiceSpecific_SawNativeToolTrue_RejectsFinalResponse", func(t *testing.T) {
		rawEnvelope := `{"type":"final","content":"done"}`
		choiceSpecific := ParsedToolChoice{Mode: "specific", SpecificFunction: "terminal"}
		res := BuildCompletionResult(
			"SUCCESS",
			rawEnvelope,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			"",
			true,
			true,
			nil,
			"conv-1",
			tools,
			choiceSpecific,
			nil,
		)

		if !res.IsError {
			t.Fatalf("expected error when tool_choice is specific but final returned, got success: %q", res.Text)
		}
	})

	t.Run("TerminalResultOverridesProgressDeltas_SawNativeToolTrue", func(t *testing.T) {
		progressDeltas := "중간 진행 상황 메시지 (노출되면 안 됨)..."
		finalEnvelope := `{"type":"final","content":"최종 분석 결과"}`
		res := BuildCompletionResult(
			"SUCCESS",
			finalEnvelope,
			nil,
			[]AgyStep{{StepType: "tool_call"}},
			progressDeltas,
			true,
			true, // SawNativeTool = true
			nil,
			"conv-1",
			tools,
			parsedChoice,
			nil,
		)

		if res.IsError {
			t.Fatalf("unexpected error: %s", res.ErrorMessage)
		}
		if res.Text != "최종 분석 결과" {
			t.Errorf("expected final envelope to take priority, got %q", res.Text)
		}
		if strings.Contains(res.Text, "중간 진행 상황") {
			t.Errorf("progress deltas leaked into final response: %q", res.Text)
		}
	})
}

// TestHandlerBoundary_NativeToolFinalEnvelope verifies the fix through the actual
// HTTP handler / encoder boundary for both non-streaming JSON and streaming SSE.
func TestHandlerBoundary_NativeToolFinalEnvelope(t *testing.T) {
	bodyNonStream := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "https://youtu.be/test"}],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "terminal",
					"parameters": {"type": "object"}
				}
			}
		],
		"stream": false
	}`

	bodyStream := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "https://youtu.be/test"}],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "terminal",
					"parameters": {"type": "object"}
				}
			}
		],
		"stream": true
	}`

	finalEnvelope := `{"type":"final","content":"영상 분석 결과입니다: ADK 그래프 엔지니어링"}`
	nativeStepLine := `{"event":"step_update","step_update":{"step_type":"tool_call"}}`
	finalResultLine := `{"event":"result","result":{"status":"SUCCESS","response":` + string(mustJSON(finalEnvelope)) + `,"steps":[{"step_type":"tool_call"}]}}`

	t.Run("HTTP_NonStream_MessageContentMatchesExtractedContent", func(t *testing.T) {
		rec, raw := runContractRequest(t, bodyNonStream, mockAgyStreamRunner(nativeStepLine, finalResultLine))
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
		content, ok := resp.Choices[0].Message.Content.(string)
		if !ok {
			t.Fatalf("expected string content, got %v", resp.Choices[0].Message.Content)
		}

		// It must be the extracted content, NOT the raw JSON envelope!
		if content != "영상 분석 결과입니다: ADK 그래프 엔지니어링" {
			t.Fatalf("FAILED: expected extracted content, but got raw text: %q", content)
		}
		if len(resp.Choices[0].Message.ToolCalls) > 0 {
			t.Errorf("expected no tool calls, got %v", resp.Choices[0].Message.ToolCalls)
		}
	})

	t.Run("HTTP_Stream_SSEContentMatchesExtractedContentAndNoRawLeak", func(t *testing.T) {
		rec, raw := runContractRequest(t, bodyStream, mockAgyStreamRunner(
			nativeStepLine,
			makeStreamUpdateLine(finalEnvelope[:15]),
			makeStreamUpdateLine(finalEnvelope[15:]),
			finalResultLine,
		))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, raw)
		}

		// Reassemble SSE deltas
		var sseContent strings.Builder
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			dataPayload := strings.TrimPrefix(line, "data: ")
			var chunk ChatCompletionChunk
			if err := json.Unmarshal([]byte(dataPayload), &chunk); err == nil {
				if len(chunk.Choices) > 0 {
					sseContent.WriteString(chunk.Choices[0].Delta.Content)
				}
			}
		}

		reassembled := sseContent.String()
		if reassembled != "영상 분석 결과입니다: ADK 그래프 엔지니어링" {
			t.Fatalf("FAILED: expected reassembled SSE content to be extracted content, got: %q", reassembled)
		}

		// Verify no raw envelope leak in raw SSE stream
		if strings.Contains(raw, `"type":"final"`) || strings.Contains(raw, `"type\": \"final\"`) {
			t.Errorf("raw SSE stream leaked internal envelope keywords:\n%s", raw)
		}
	})
}

