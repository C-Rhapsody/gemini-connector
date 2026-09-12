package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// parseSSEEvents parses raw SSE stream into a list of data strings
func parseSSEEvents(raw string) []string {
	var events []string
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			events = append(events, data)
		}
	}
	return events
}

// TestStreamStopAcrossChunks verifies that when a stop sequence is split across
// multiple streaming chunks (e.g. chunk 1: "Hello <EN", chunk 2: "D> world"),
// the stop sequence prefix is held back and not leaked to the client.
func TestStreamStopAcrossChunks(t *testing.T) {
	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "hi"}],
		"stream": true,
		"stop": ["<END>"]
	}`

	// AGY emits two step_update deltas that together contain "<END>"
	line1 := makeStreamUpdateLine("Hello ")
	line2 := makeStreamUpdateLine("<EN")
	line3 := makeStreamUpdateLine("D> remainder")
	resLine := `{"event":"result","result":{"status":"SUCCESS","response":"Hello <END> remainder"}}`

	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(line1, line2, line3, resLine))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, raw)
	}

	events := parseSSEEvents(raw)
	var fullText strings.Builder
	for _, ev := range events {
		if ev == "[DONE]" {
			break
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(ev), &chunk); err != nil {
			t.Fatalf("failed to unmarshal SSE chunk: %v", err)
		}
		for _, ch := range chunk.Choices {
			fullText.WriteString(ch.Delta.Content)
		}
	}

	assembled := fullText.String()
	// Must be exactly "Hello " and must NOT contain "<EN" or "<END>"
	if assembled != "Hello " {
		t.Errorf("expected assembled text to be exactly 'Hello ', got %q", assembled)
	}
	if strings.Contains(assembled, "<EN") {
		t.Errorf("stop sequence prefix '<EN' leaked into streamed output: %q", assembled)
	}
}

// TestStreamToolArgumentsUTF8 verifies that streaming tool call arguments with multi-byte
// UTF-8 characters (Korean, emojis) are not split across byte boundaries and reassemble cleanly
// without any unicode replacement characters (\ufffd).
func TestStreamToolArgumentsUTF8(t *testing.T) {
	body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [{"role": "user", "content": "test"}],
		"stream": true,
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "save_note",
					"parameters": {"type": "object"}
				}
			}
		],
		"tool_choice": "auto"
	}`

	// Arguments string longer than 30 bytes with 3-byte Korean and 4-byte emoji:
	// "{"note": "안녕하세요 반갑습니다! 🚀🌟"}"
	rawArgs := `{"note": "안녕하세요 반갑습니다! 🚀🌟"}`
	agyEnvelope := `{"type": "tool_call", "calls": [{"id": "call_1", "name": "save_note", "arguments": ` + rawArgs + `}]}`

	line1 := makeStreamUpdateLine(agyEnvelope)
	resLine := `{"event":"result","result":{"status":"SUCCESS","response":` + mustJSON(agyEnvelope) + `}}`

	rec, raw := runContractRequest(t, body, mockAgyStreamRunner(line1, resLine))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, raw)
	}

	events := parseSSEEvents(raw)
	var reassembledArgs strings.Builder
	for _, ev := range events {
		if ev == "[DONE]" {
			break
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(ev), &chunk); err != nil {
			t.Fatalf("failed to unmarshal SSE chunk: %v", err)
		}
		for _, ch := range chunk.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				reassembledArgs.WriteString(tc.Function.Arguments)
			}
		}
	}

	resultStr := reassembledArgs.String()
	if strings.Contains(resultStr, "\ufffd") || strings.Contains(resultStr, "\\ufffd") {
		t.Fatalf("reassembled arguments contain unicode replacement character: %q", resultStr)
	}

	// Verify valid JSON
	var parsed map[string]any
	if err := json.Unmarshal([]byte(resultStr), &parsed); err != nil {
		t.Fatalf("reassembled arguments should be valid JSON: %v, raw: %q", err, resultStr)
	}
	if parsed["note"] != "안녕하세요 반갑습니다! 🚀🌟" {
		t.Errorf("note value mismatch, got %v", parsed["note"])
	}
}


