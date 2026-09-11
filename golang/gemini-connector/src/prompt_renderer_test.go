package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestRenderPrompt_RoleOrderAndSeparation verifies that system/developer instructions
// are separated from conversation history, role ordering is strictly preserved,
// and past user messages are distinguished from the current turn.
func TestRenderPrompt_RoleOrderAndSeparation(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "System rule 1"},
		{Role: "developer", Content: "Developer instruction 2"},
		{Role: "user", Content: "Hello from past user"},
		{Role: "assistant", Content: "Hello from assistant"},
		{Role: "user", Content: "Current user question"},
	}

	rendered, err := RenderPrompt(messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. Verify system section
	if !strings.Contains(rendered, "<SYSTEM_AND_DEVELOPER_MESSAGES>") || !strings.Contains(rendered, "</SYSTEM_AND_DEVELOPER_MESSAGES>") {
		t.Fatalf("expected <SYSTEM_AND_DEVELOPER_MESSAGES> tags, got:\n%s", rendered)
	}
	sysBlock := rendered[strings.Index(rendered, "<SYSTEM_AND_DEVELOPER_MESSAGES>"):strings.Index(rendered, "</SYSTEM_AND_DEVELOPER_MESSAGES>")]
	if !strings.Contains(sysBlock, "System rule 1") || !strings.Contains(sysBlock, "Developer instruction 2") {
		t.Errorf("system block missing instructions: %s", sysBlock)
	}

	// 2. Verify transcript section
	if !strings.Contains(rendered, "<CONVERSATION_TRANSCRIPT>") || !strings.Contains(rendered, "</CONVERSATION_TRANSCRIPT>") {
		t.Fatalf("expected <CONVERSATION_TRANSCRIPT> tags, got:\n%s", rendered)
	}
	transBlock := rendered[strings.Index(rendered, "<CONVERSATION_TRANSCRIPT>"):strings.Index(rendered, "</CONVERSATION_TRANSCRIPT>")]
	if !strings.Contains(transBlock, "[Turn 1] user:") || !strings.Contains(transBlock, "Hello from past user") {
		t.Errorf("transcript missing Turn 1 user: %s", transBlock)
	}
	if !strings.Contains(transBlock, "[Turn 2] assistant:") || !strings.Contains(transBlock, "Hello from assistant") {
		t.Errorf("transcript missing Turn 2 assistant: %s", transBlock)
	}
	if strings.Contains(transBlock, "Current user question") {
		t.Errorf("current turn question leaked into transcript: %s", transBlock)
	}

	// 3. Verify current turn section
	if !strings.Contains(rendered, "<CURRENT_TURN>") || !strings.Contains(rendered, "</CURRENT_TURN>") {
		t.Fatalf("expected <CURRENT_TURN> tags, got:\n%s", rendered)
	}
	turnBlock := rendered[strings.Index(rendered, "<CURRENT_TURN>"):strings.Index(rendered, "</CURRENT_TURN>")]
	if !strings.Contains(turnBlock, "[user]:") || !strings.Contains(turnBlock, "Current user question") {
		t.Errorf("current turn missing active user prompt: %s", turnBlock)
	}
	if strings.Contains(turnBlock, "Hello from past user") {
		t.Errorf("past user prompt leaked into current turn: %s", turnBlock)
	}

	// 4. Verify overall ordering of tags
	idxSys := strings.Index(rendered, "<SYSTEM_AND_DEVELOPER_MESSAGES>")
	idxTrans := strings.Index(rendered, "<CONVERSATION_TRANSCRIPT>")
	idxTurn := strings.Index(rendered, "<CURRENT_TURN>")
	if idxSys >= idxTrans || idxTrans >= idxTurn {
		t.Errorf("sections out of expected order (sys < trans < turn): sys=%d, trans=%d, turn=%d", idxSys, idxTrans, idxTurn)
	}
}

// TestRenderPrompt_ToolCallsAndMetadata verifies that assistant tool_calls metadata
// (call ID, type, function name, arguments) and tool message metadata (tool_call_id, name)
// are accurately preserved in the rendered prompt.
func TestRenderPrompt_ToolCallsAndMetadata(t *testing.T) {
	callID := "call_weather_99"
	messages := []ChatMessage{
		{Role: "user", Content: "What is the weather in London?"},
		{
			Role:    "assistant",
			Content: "Let me check that for you.",
			ToolCalls: []ToolCall{
				{
					ID:   callID,
					Type: "function",
					Function: ToolCallFunction{
						Name:      "get_weather",
						Arguments: `{"city":"London","units":"metric"}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: callID,
			Name:       "get_weather",
			Content:    `{"temp": 15, "desc": "Partly Cloudy"}`,
		},
		{Role: "user", Content: "And what should I pack?"},
	}

	rendered, err := RenderPrompt(messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(rendered, callID) {
		t.Errorf("tool call ID %s missing from rendered prompt:\n%s", callID, rendered)
	}
	if !strings.Contains(rendered, "get_weather") {
		t.Errorf("function name get_weather missing from rendered prompt:\n%s", rendered)
	}
	if !strings.Contains(rendered, `{"city":"London","units":"metric"}`) {
		t.Errorf("arguments missing from rendered prompt:\n%s", rendered)
	}
	if !strings.Contains(rendered, `call_id: call_weather_99`) || !strings.Contains(rendered, `name: get_weather`) {
		t.Errorf("tool message metadata missing from rendered prompt:\n%s", rendered)
	}
	if !strings.Contains(rendered, `{"temp": 15, "desc": "Partly Cloudy"}`) {
		t.Errorf("tool response content missing from rendered prompt:\n%s", rendered)
	}
}

// TestRenderPrompt_LastMessageIsTool verifies that when the conversation ends with tool execution results,
// the trailing tool message(s) become the <CURRENT_TURN> so the assistant can formulate its response.
func TestRenderPrompt_LastMessageIsTool(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "Look up 1 and 2"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{ID: "call_1", Function: ToolCallFunction{Name: "fn1", Arguments: "{}"}},
				{ID: "call_2", Function: ToolCallFunction{Name: "fn2", Arguments: "{}"}},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Name: "fn1", Content: "result 1"},
		{Role: "tool", ToolCallID: "call_2", Name: "fn2", Content: "result 2"},
	}

	rendered, err := RenderPrompt(messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The current turn must contain BOTH trailing tool messages
	if !strings.Contains(rendered, "<CURRENT_TURN>") {
		t.Fatalf("expected <CURRENT_TURN> tag, got:\n%s", rendered)
	}
	turnBlock := rendered[strings.Index(rendered, "<CURRENT_TURN>"):strings.Index(rendered, "</CURRENT_TURN>")]
	if !strings.Contains(turnBlock, "call_id: call_1") || !strings.Contains(turnBlock, "result 1") {
		t.Errorf("current turn missing tool 1: %s", turnBlock)
	}
	if !strings.Contains(turnBlock, "call_id: call_2") || !strings.Contains(turnBlock, "result 2") {
		t.Errorf("current turn missing tool 2: %s", turnBlock)
	}

	// The transcript must contain the prior user and assistant turn, but NOT the trailing tools
	transBlock := rendered[strings.Index(rendered, "<CONVERSATION_TRANSCRIPT>"):strings.Index(rendered, "</CONVERSATION_TRANSCRIPT>")]
	if !strings.Contains(transBlock, "Look up 1 and 2") {
		t.Errorf("transcript missing initial user request: %s", transBlock)
	}
	if strings.Contains(transBlock, "result 1") || strings.Contains(transBlock, "result 2") {
		t.Errorf("transcript should not contain current turn tools: %s", transBlock)
	}
}

// TestRenderPrompt_ContentVarieties verifies preservation of string, array of text parts,
// object content, empty content, Unicode, and escape sequences without corruption.
func TestRenderPrompt_ContentVarieties(t *testing.T) {
	unicodeStr := "한글 테스트 및 이모지: 🚀🎉 こんにちは, \t escaped \n characters \\ \"quotes\""
	messages := []ChatMessage{
		// 1. String with Unicode and escapes
		{Role: "user", Content: unicodeStr},
		// 2. Empty string content (e.g. pure tool call)
		{Role: "assistant", Content: ""},
		// 3. Array of text parts
		{
			Role: "user",
			Content: []any{
				map[string]any{"type": "text", "text": "Part 1 of message"},
				map[string]any{"type": "text", "text": "Part 2 of message"},
			},
		},
		// 4. Object/Structured content
		{
			Role: "assistant",
			Content: map[string]any{
				"status": "active",
				"items":  []any{"a", 42},
			},
		},
		// 5. Current turn
		{Role: "user", Content: "Final prompt"},
	}

	rendered, err := RenderPrompt(messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. Unicode & escapes
	if !strings.Contains(rendered, unicodeStr) {
		t.Errorf("unicode string corrupted or missing from rendered prompt:\n%s", rendered)
	}

	// 2. Array of text parts
	if !strings.Contains(rendered, "Part 1 of message") || !strings.Contains(rendered, "Part 2 of message") {
		t.Errorf("array of text parts corrupted:\n%s", rendered)
	}

	// 3. Object content (must be valid JSON, not [object Object] or Go map format)
	if strings.Contains(rendered, "[object Object]") || strings.Contains(rendered, "map[") {
		t.Errorf("object content rendered as invalid format:\n%s", rendered)
	}
	if !strings.Contains(rendered, `"status":"active"`) && !strings.Contains(rendered, `"status": "active"`) {
		t.Errorf("object content missing valid JSON serialization:\n%s", rendered)
	}
}

// TestRenderPrompt_LongHistoryNoReordering verifies that a 20-message sequence preserves
// exact turn ordering from 1 to 19 without dropping or interleaving.
func TestRenderPrompt_LongHistoryNoReordering(t *testing.T) {
	var messages []ChatMessage
	messages = append(messages, ChatMessage{Role: "system", Content: "System prompt"})

	for i := 1; i <= 20; i++ {
		role := "user"
		if i%2 == 0 {
			role = "assistant"
		}
		messages = append(messages, ChatMessage{
			Role:    role,
			Content: fmt.Sprintf("Message payload turn-%02d", i),
		})
	}

	rendered, err := RenderPrompt(messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify sequential index order in transcript for turns 1 through 19
	lastIdx := -1
	for i := 1; i <= 19; i++ {
		needle := fmt.Sprintf("Message payload turn-%02d", i)
		idx := strings.Index(rendered, needle)
		if idx == -1 {
			t.Fatalf("missing turn-%02d in rendered prompt", i)
		}
		if idx <= lastIdx {
			t.Fatalf("turn-%02d appeared at position %d, earlier than previous at %d (reordering detected)", i, idx, lastIdx)
		}
		lastIdx = idx
	}

	// Verify turn 20 is inside <CURRENT_TURN>
	if !strings.Contains(rendered, "<CURRENT_TURN>") {
		t.Fatal("missing <CURRENT_TURN>")
	}
	idxTurn20 := strings.Index(rendered, "Message payload turn-20")
	idxCurrentTurn := strings.Index(rendered, "<CURRENT_TURN>")
	if idxTurn20 < idxCurrentTurn {
		t.Errorf("turn 20 expected inside <CURRENT_TURN>, but was found earlier")
	}
}

// TestRenderPrompt_EdgeCases verifies edge case inputs: empty slice, single user message,
// only system messages.
func TestRenderPrompt_EdgeCases(t *testing.T) {
	// 1. Empty slice
	r1, err1 := RenderPrompt(nil)
	if err1 != nil || r1 != "" {
		t.Errorf("expected empty string and nil error for empty slice, got: %q, %v", r1, err1)
	}

	// 2. Single user message
	r2, err2 := RenderPrompt([]ChatMessage{{Role: "user", Content: "Just one prompt"}})
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if strings.Contains(r2, "<SYSTEM_AND_DEVELOPER_MESSAGES>") || strings.Contains(r2, "<CONVERSATION_TRANSCRIPT>") {
		t.Errorf("single user message should not contain system or transcript tags:\n%s", r2)
	}
	if !strings.Contains(r2, "<CURRENT_TURN>") || !strings.Contains(r2, "Just one prompt") {
		t.Errorf("single user message must be in <CURRENT_TURN>:\n%s", r2)
	}

	// 3. Only system messages
	r3, err3 := RenderPrompt([]ChatMessage{{Role: "system", Content: "Pure system instruction"}})
	if err3 != nil {
		t.Fatalf("unexpected error: %v", err3)
	}
	if !strings.Contains(r3, "<SYSTEM_AND_DEVELOPER_MESSAGES>") || !strings.Contains(r3, "Pure system instruction") {
		t.Errorf("expected system tag with instruction:\n%s", r3)
	}
	if strings.Contains(r3, "<CURRENT_TURN>") || strings.Contains(r3, "<CONVERSATION_TRANSCRIPT>") {
		t.Errorf("pure system messages should not emit transcript or current turn tags:\n%s", r3)
	}
}

func TestRenderPromptWithTools_SchemaDelivered(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "Probe system"},
	}
	tools := []ToolDefinition{
		{
			Type: "function",
			Function: &FunctionDefinition{
				Name:        "hermes_probe",
				Description: "Probe a host system",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"}},"required":["target"]}`),
			},
		},
	}

	rendered, err := RenderPromptWithTools(messages, tools, "auto")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, reqText := range []string{
		"<HERMES_TOOLS>",
		"hermes_probe",
		"Probe a host system",
		`"target"`,
		"auto",
		`"type": "final"`,
		`"type": "tool_call"`,
		"</HERMES_TOOLS>",
	} {
		if !strings.Contains(rendered, reqText) {
			t.Errorf("expected prompt to contain %q, but was:\n%s", reqText, rendered)
		}
	}
}

func TestRenderPromptWithTools_RoleToolRoundTrip(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "Run probe"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{
					ID:   "call_probe_1",
					Type: "function",
					Function: ToolCallFunction{
						Name:      "hermes_probe",
						Arguments: `{"target":"localhost"}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_probe_1",
			Name:       "hermes_probe",
			Content:    `{"status":"healthy"}`,
		},
	}
	tools := []ToolDefinition{
		{
			Type: "function",
			Function: &FunctionDefinition{
				Name: "hermes_probe",
			},
		},
	}

	rendered, err := RenderPromptWithTools(messages, tools, "auto")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, expected := range []string{
		"call_id: call_probe_1",
		"name: hermes_probe",
		`{"status":"healthy"}`,
		`{"target":"localhost"}`,
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("expected prompt to contain %q, got:\n%s", expected, rendered)
		}
	}
}

func TestRenderPromptWithTools_NoToolsOmitsSection(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "Ordinary question"},
	}
	rendered, err := RenderPromptWithTools(messages, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(rendered, "<HERMES_TOOLS>") {
		t.Errorf("expected <HERMES_TOOLS> to be omitted when tools is empty:\n%s", rendered)
	}
}

