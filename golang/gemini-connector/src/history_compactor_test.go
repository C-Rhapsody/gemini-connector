package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractCurrentTurn_PreservesLatestUserTurn(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "System prompt"},
		{Role: "user", Content: "Turn 1 question"},
		{Role: "assistant", Content: "Turn 1 answer"},
		{Role: "user", Content: "Turn 2 question"},
	}

	sys, prior, cur := ExtractCurrentTurn(messages)

	if len(sys) != 1 || sys[0].Role != "system" {
		t.Fatalf("expected 1 system message, got %d", len(sys))
	}
	if len(prior) != 2 {
		t.Fatalf("expected 2 prior messages, got %d", len(prior))
	}
	if len(cur) != 1 || cur[0].Content != "Turn 2 question" {
		t.Fatalf("expected 1 current message with 'Turn 2 question', got %v", cur)
	}
}

func TestExtractCurrentTurn_AssistantToolCallsAndToolResultsKeptTogether(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "System prompt"},
		{Role: "user", Content: "Turn 1 question"},
		{Role: "assistant", Content: "Turn 1 answer"},
		{Role: "user", Content: "Check status"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{
					ID:   "call_status_01",
					Type: "function",
					Function: ToolCallFunction{
						Name:      "check_status",
						Arguments: `{"target":"all"}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_status_01",
			Name:       "check_status",
			Content:    `{"status":"healthy"}`,
		},
	}

	sys, prior, cur := ExtractCurrentTurn(messages)

	if len(sys) != 1 {
		t.Fatalf("expected 1 system message, got %d", len(sys))
	}
	if len(prior) != 2 {
		t.Fatalf("expected 2 prior messages, got %d", len(prior))
	}
	// The current user turn must atomically contain: user + assistant tool call + tool result
	if len(cur) != 3 {
		t.Fatalf("expected 3 current turn messages, got %d: %+v", len(cur), cur)
	}
	if cur[0].Role != "user" || cur[0].Content != "Check status" {
		t.Errorf("expected cur[0] to be user 'Check status', got %v", cur[0])
	}
	if cur[1].Role != "assistant" || len(cur[1].ToolCalls) != 1 {
		t.Errorf("expected cur[1] to be assistant with tool calls, got %v", cur[1])
	}
	if cur[2].Role != "tool" || cur[2].ToolCallID != "call_status_01" {
		t.Errorf("expected cur[2] to be tool result for call_status_01, got %v", cur[2])
	}
}

func TestExtractCurrentTurn_NoOrphanToolCallOrResult(t *testing.T) {
	// Even if an assistant tool call has no preceding user message in a headless sequence,
	// the assistant tool call and tool result must never be severed into an orphan.
	messages := []ChatMessage{
		{Role: "system", Content: "System prompt"},
		{Role: "user", Content: "Past turn"},
		{Role: "assistant", Content: "Past response"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{
					ID:   "call_probe_99",
					Type: "function",
					Function: ToolCallFunction{
						Name:      "probe_tool",
						Arguments: `{}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_probe_99",
			Name:       "probe_tool",
			Content:    `{"ok":true}`,
		},
	}

	sys, prior, cur := ExtractCurrentTurn(messages)

	if len(sys) != 1 {
		t.Fatalf("expected 1 system message, got %d", len(sys))
	}
	// Verify that the tool message in cur has its matching assistant message in cur
	hasTool := false
	hasMatchingAsst := false
	for _, m := range cur {
		if m.Role == "tool" && m.ToolCallID == "call_probe_99" {
			hasTool = true
		}
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "call_probe_99" {
			hasMatchingAsst = true
		}
	}
	if !hasTool || !hasMatchingAsst {
		t.Fatalf("atomic pairing failed: hasTool=%t hasMatchingAsst=%t; cur=%+v", hasTool, hasMatchingAsst, cur)
	}

	// Verify no orphan tool message in prior
	for _, m := range prior {
		if m.ToolCallID == "call_probe_99" {
			t.Errorf("orphan tool call in prior messages: %+v", m)
		}
	}
}

func TestCompaction_BudgetExceeded_ReturnsExplicitError(t *testing.T) {
	// If the mandatory prompt (tools + system + current turn) exceeds budget,
	// it must NOT silently truncate, but return an explicit error.
	hugeMessage := strings.Repeat("A", 2000)
	messages := []ChatMessage{
		{Role: "system", Content: "System prompt"},
		{Role: "user", Content: hugeMessage},
	}

	// Set a very small budget (500 bytes) that cannot fit the huge user message
	_, _, _, err := CompactPromptWithTools(messages, nil, nil, 500)
	if err == nil {
		t.Fatalf("expected budget exceeded error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds compaction budget") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestCompaction_TruncatesToolOutputsWithoutBreakingProtocol(t *testing.T) {
	hugeToolResult := strings.Repeat("DATA-BLOCK-", 100) // ~1100 bytes
	messages := []ChatMessage{
		{Role: "system", Content: "System prompt"},
		{Role: "user", Content: "Past turn query"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{
					ID:   "call_fetch_01",
					Type: "function",
					Function: ToolCallFunction{
						Name:      "fetch_data",
						Arguments: `{}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_fetch_01",
			Name:       "fetch_data",
			Content:    hugeToolResult,
		},
		{Role: "assistant", Content: "Past response completed"},
		{Role: "user", Content: "Current active query"},
	}

	// Budget is sufficient to include past turns with truncated tool outputs (e.g. 8 KiB)
	prompt, omitted, summarized, err := CompactPromptWithTools(messages, nil, nil, 8192)
	if err != nil {
		t.Fatalf("unexpected error during compaction: %v", err)
	}
	if !summarized {
		t.Errorf("expected summarized=true due to large tool result")
	}
	if omitted != 0 {
		t.Errorf("expected 0 omitted messages within 8 KiB budget, got %d", omitted)
	}

	// Crucial: protocol pairing must be preserved
	if !strings.Contains(prompt, "call_id: call_fetch_01") {
		t.Errorf("prompt missing call_id metadata:\n%s", prompt)
	}
	if !strings.Contains(prompt, "name: fetch_data") {
		t.Errorf("prompt missing tool name metadata:\n%s", prompt)
	}
	if !strings.Contains(prompt, "[truncated") {
		t.Errorf("prompt did not truncate large tool content:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Current active query") {
		t.Errorf("prompt missing active query:\n%s", prompt)
	}
}

func TestSessionSync_RestartDoesNotDuplicateBootstrap(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "session_sync.json")

	// Instance 1: initial run
	tracker1 := NewSessionSyncTracker(storePath)
	convID := "session-abc-12345"
	historyHash := "hash-turn-1-turn-2"

	if tracker1.IsSessionSynced(convID) {
		t.Fatalf("session should not be synced initially")
	}

	tracker1.MarkSessionSynced(convID, historyHash, 2)
	if !tracker1.IsSessionSynced(convID) {
		t.Fatalf("session should be marked as synced")
	}

	// Instance 2: simulated connector restart loading from same storePath
	tracker2 := NewSessionSyncTracker(storePath)
	if !tracker2.IsSessionSynced(convID) {
		t.Fatalf("session should remain synced across restart without duplicating bootstrap")
	}

	// Different conversation ID must not be synced
	if tracker2.IsSessionSynced("session-other-999") {
		t.Fatalf("unrelated session should not be synced")
	}
}
