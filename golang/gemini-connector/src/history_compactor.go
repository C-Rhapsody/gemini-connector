package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// DefaultCompactionBudget is the hard byte budget for compacted prompts.
	DefaultCompactionBudget = 64 * 1024 // 64 KiB
	// maxToolOutputSummaryBytes limits individual tool outputs during compaction.
	maxToolOutputSummaryBytes = 256
)

// ComputeHistoryHash returns a deterministic SHA-256 hex string of the message history.
// It excludes volatile credentials and hashes only semantic structure and content.
func ComputeHistoryHash(messages []ChatMessage) string {
	h := sha256.New()
	for _, m := range messages {
		h.Write([]byte(m.Role))
		h.Write([]byte(":"))
		h.Write([]byte(formatMessageContent(m.Content)))
		h.Write([]byte(":"))
		h.Write([]byte(m.ToolCallID))
		h.Write([]byte(":"))
		h.Write([]byte(m.Name))
		h.Write([]byte(";"))
		for _, tc := range m.ToolCalls {
			h.Write([]byte(tc.ID))
			h.Write([]byte(tc.Type))
			h.Write([]byte(tc.Function.Name))
			h.Write([]byte(tc.Function.Arguments))
		}
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ExtractCurrentTurn partitions messages into:
// 1. systemMsgs: all system/developer instruction messages
// 2. priorTranscriptMsgs: past completed turns (candidate for sync/compaction)
// 3. currentTurnMsgs: the active turn, preserving user intent and atomic tool round-trips
func ExtractCurrentTurn(messages []ChatMessage) (systemMsgs []ChatMessage, priorTranscriptMsgs []ChatMessage, currentTurnMsgs []ChatMessage) {
	if len(messages) == 0 {
		return nil, nil, nil
	}

	// Separate all system and developer messages
	var nonSystem []ChatMessage
	for _, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			systemMsgs = append(systemMsgs, m)
		} else {
			nonSystem = append(nonSystem, m)
		}
	}

	if len(nonSystem) == 0 {
		return systemMsgs, nil, nil
	}

	// Find the start of the current active turn.
	// A user turn starts with the latest "user" message.
	// Any following "assistant" (tool calls) and "tool" messages form an atomic group with that user message.
	lastUserIdx := -1
	for i := len(nonSystem) - 1; i >= 0; i-- {
		if nonSystem[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}

	var curTurnStart int
	if lastUserIdx >= 0 {
		curTurnStart = lastUserIdx
	} else {
		// If there is no user message, check if trailing messages are tool results
		// and group them with their preceding assistant message.
		curTurnStart = len(nonSystem) - 1
		for curTurnStart > 0 && nonSystem[curTurnStart].Role == "tool" {
			curTurnStart--
		}
		if curTurnStart > 0 && nonSystem[curTurnStart-1].Role == "assistant" && len(nonSystem[curTurnStart-1].ToolCalls) > 0 {
			curTurnStart--
		}
	}

	currentTurnMsgs = nonSystem[curTurnStart:]
	priorTranscriptMsgs = nonSystem[:curTurnStart]

	// Verify atomic tool containment: ensure no tool message in currentTurnMsgs is an orphan.
	// If any tool message in currentTurnMsgs refers to a tool call in an assistant message
	// that was left in priorTranscriptMsgs, pull that assistant message into currentTurnMsgs.
	for len(priorTranscriptMsgs) > 0 {
		lastPrior := priorTranscriptMsgs[len(priorTranscriptMsgs)-1]
		if lastPrior.Role == "assistant" && len(lastPrior.ToolCalls) > 0 {
			// Check if any tool message in currentTurnMsgs belongs to this assistant message
			hasMatch := false
			for _, cur := range currentTurnMsgs {
				if cur.Role == "tool" {
					for _, tc := range lastPrior.ToolCalls {
						if cur.ToolCallID == tc.ID {
							hasMatch = true
							break
						}
					}
				}
				if hasMatch {
					break
				}
			}
			if hasMatch {
				// Move assistant message to currentTurnMsgs to maintain atomic pairing
				currentTurnMsgs = append([]ChatMessage{lastPrior}, currentTurnMsgs...)
				priorTranscriptMsgs = priorTranscriptMsgs[:len(priorTranscriptMsgs)-1]
				continue
			}
		}
		break
	}

	return systemMsgs, priorTranscriptMsgs, currentTurnMsgs
}

// RenderTurnPromptWithTools renders a prompt for a resumed session turn.
// It contains tool definitions, system instructions, and the current active turn,
// omitting prior history which is already recorded in the AGY session transcript.
func RenderTurnPromptWithTools(systemMsgs []ChatMessage, currentTurnMsgs []ChatMessage, tools []ToolDefinition, toolChoice any) (string, error) {
	var parsedChoice ParsedToolChoice
	var err error
	if len(tools) > 0 {
		parsedChoice, err = ParseToolChoice(toolChoice, tools)
		if err != nil {
			return "", fmt.Errorf("invalid tool_choice: %w", err)
		}
	}

	var sb strings.Builder

	if len(tools) > 0 {
		sb.WriteString(renderHermesTools(tools, parsedChoice))
	}

	if len(systemMsgs) > 0 {
		sb.WriteString("<SYSTEM_AND_DEVELOPER_MESSAGES>\n")
		for _, msg := range systemMsgs {
			sb.WriteString(fmt.Sprintf("[%s]\n%s\n\n", msg.Role, formatMessageContent(msg.Content)))
		}
		sb.WriteString("</SYSTEM_AND_DEVELOPER_MESSAGES>\n\n")
	}

	if len(currentTurnMsgs) > 0 {
		sb.WriteString("<CURRENT_TURN>\n")
		for i, msg := range currentTurnMsgs {
			if i > 0 {
				sb.WriteString("\n")
			}
			header := fmt.Sprintf("[%s]", msg.Role)
			if msg.Role == "tool" {
				var meta []string
				if msg.ToolCallID != "" {
					meta = append(meta, fmt.Sprintf("call_id: %s", msg.ToolCallID))
				}
				if msg.Name != "" {
					meta = append(meta, fmt.Sprintf("name: %s", msg.Name))
				}
				if len(meta) > 0 {
					header += fmt.Sprintf(" (%s)", strings.Join(meta, ", "))
				}
			}
			sb.WriteString(renderSingleMessage(header, msg))
			sb.WriteString("\n")
		}
		sb.WriteString("</CURRENT_TURN>")
	}

	return strings.TrimSpace(sb.String()), nil
}

// CompactPromptWithTools builds a structured, bounded prompt within maxBytes.
// It preserves system instructions, tool definitions, and the current active turn.
// Older turns are preserved from latest backwards, summarizing long tool outputs
// without breaking protocol pairing. If even the minimum required base prompt
// exceeds maxBytes, an explicit error is returned without silent truncation.
func CompactPromptWithTools(messages []ChatMessage, tools []ToolDefinition, toolChoice any, maxBytes int) (string, int, bool, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultCompactionBudget
	}

	systemMsgs, priorTranscriptMsgs, currentTurnMsgs := ExtractCurrentTurn(messages)

	// Step 1: Render the mandatory base prompt (tools + system + current turn)
	basePrompt, err := RenderTurnPromptWithTools(systemMsgs, currentTurnMsgs, tools, toolChoice)
	if err != nil {
		return "", 0, false, err
	}

	baseLen := len(basePrompt)
	if baseLen > maxBytes {
		return "", 0, false, fmt.Errorf("mandatory prompt size (%d bytes) exceeds compaction budget (%d bytes)", baseLen, maxBytes)
	}

	if len(priorTranscriptMsgs) == 0 {
		log.Printf("[Compaction] rendered_bytes=%d omitted_msgs=0 summarized=false", baseLen)
		return basePrompt, 0, false, nil
	}

	// Step 2: Group priorTranscriptMsgs into atomic turn units.
	// A turn unit is a user message followed by any assistant tool calls and corresponding tool results.
	type turnUnit struct {
		msgs []ChatMessage
	}
	var units []turnUnit
	var currentUnit []ChatMessage

	for _, m := range priorTranscriptMsgs {
		if m.Role == "user" && len(currentUnit) > 0 {
			units = append(units, turnUnit{msgs: currentUnit})
			currentUnit = nil
		}
		currentUnit = append(currentUnit, m)
	}
	if len(currentUnit) > 0 {
		units = append(units, turnUnit{msgs: currentUnit})
	}

	// Step 3: Fit units from newest to oldest within remaining budget
	overhead := len("<CONVERSATION_TRANSCRIPT>\n\n</CONVERSATION_TRANSCRIPT>\n\n") + 128
	availableForTranscript := maxBytes - baseLen - overhead
	if availableForTranscript <= 0 {
		omittedCount := len(priorTranscriptMsgs)
		log.Printf("[Compaction] rendered_bytes=%d omitted_msgs=%d summarized=false", baseLen, omittedCount)
		return basePrompt, omittedCount, false, nil
	}

	var includedUnits []turnUnit
	isSummarized := false
	omittedUnitsCount := 0

	for i := len(units) - 1; i >= 0; i-- {
		u := units[i]
		// Prepare a compacted copy of messages in this unit
		var compactedMsgs []ChatMessage
		for _, m := range u.msgs {
			if m.Role == "tool" {
				cStr := formatMessageContent(m.Content)
				if len(cStr) > maxToolOutputSummaryBytes {
					isSummarized = true
					shortContent := cStr[:maxToolOutputSummaryBytes] + fmt.Sprintf("... [truncated %d bytes]", len(cStr)-maxToolOutputSummaryBytes)
					mCopy := m
					mCopy.Content = shortContent
					compactedMsgs = append(compactedMsgs, mCopy)
					continue
				}
			}
			compactedMsgs = append(compactedMsgs, m)
		}

		// Estimate unit size
		unitSize := 0
		for _, m := range compactedMsgs {
			unitSize += len(formatMessageContent(m.Content)) + 100
		}

		if unitSize <= availableForTranscript {
			availableForTranscript -= unitSize
			includedUnits = append([]turnUnit{{msgs: compactedMsgs}}, includedUnits...)
		} else {
			omittedUnitsCount = i + 1
			break
		}
	}

	omittedCount := 0
	for i := 0; i < omittedUnitsCount; i++ {
		omittedCount += len(units[i].msgs)
	}

	// If no units could fit, return base prompt
	if len(includedUnits) == 0 {
		omittedCount = len(priorTranscriptMsgs)
		log.Printf("[Compaction] rendered_bytes=%d omitted_msgs=%d summarized=%t", baseLen, omittedCount, isSummarized)
		return basePrompt, omittedCount, isSummarized, nil
	}

	// Step 4: Re-render with included transcript units
	var flattenedTranscript []ChatMessage
	for _, u := range includedUnits {
		flattenedTranscript = append(flattenedTranscript, u.msgs...)
	}

	var parsedChoice ParsedToolChoice
	if len(tools) > 0 {
		parsedChoice, _ = ParseToolChoice(toolChoice, tools)
	}

	var sb strings.Builder
	if len(tools) > 0 {
		sb.WriteString(renderHermesTools(tools, parsedChoice))
	}
	if len(systemMsgs) > 0 {
		sb.WriteString("<SYSTEM_AND_DEVELOPER_MESSAGES>\n")
		for _, msg := range systemMsgs {
			sb.WriteString(fmt.Sprintf("[%s]\n%s\n\n", msg.Role, formatMessageContent(msg.Content)))
		}
		sb.WriteString("</SYSTEM_AND_DEVELOPER_MESSAGES>\n\n")
	}

	sb.WriteString("<CONVERSATION_TRANSCRIPT>\n")
	if omittedCount > 0 {
		sb.WriteString(fmt.Sprintf("[System Note: %d earlier messages omitted to fit context budget]\n\n", omittedCount))
	}
	for idx, msg := range flattenedTranscript {
		turnNum := idx + 1
		header := fmt.Sprintf("[Turn %d] %s", turnNum, msg.Role)
		if msg.Role == "tool" {
			var meta []string
			if msg.ToolCallID != "" {
				meta = append(meta, fmt.Sprintf("call_id: %s", msg.ToolCallID))
			}
			if msg.Name != "" {
				meta = append(meta, fmt.Sprintf("name: %s", msg.Name))
			}
			if len(meta) > 0 {
				header += fmt.Sprintf(" (%s)", strings.Join(meta, ", "))
			}
		}
		sb.WriteString(renderSingleMessage(header, msg))
		sb.WriteString("\n\n")
	}
	sb.WriteString("</CONVERSATION_TRANSCRIPT>\n\n")

	if len(currentTurnMsgs) > 0 {
		sb.WriteString("<CURRENT_TURN>\n")
		for i, msg := range currentTurnMsgs {
			if i > 0 {
				sb.WriteString("\n")
			}
			header := fmt.Sprintf("[%s]", msg.Role)
			if msg.Role == "tool" {
				var meta []string
				if msg.ToolCallID != "" {
					meta = append(meta, fmt.Sprintf("call_id: %s", msg.ToolCallID))
				}
				if msg.Name != "" {
					meta = append(meta, fmt.Sprintf("name: %s", msg.Name))
				}
				if len(meta) > 0 {
					header += fmt.Sprintf(" (%s)", strings.Join(meta, ", "))
				}
			}
			sb.WriteString(renderSingleMessage(header, msg))
			sb.WriteString("\n")
		}
		sb.WriteString("</CURRENT_TURN>")
	}

	finalPrompt := strings.TrimSpace(sb.String())
	if len(finalPrompt) > maxBytes {
		// If safety margin was slightly exceeded, fallback to base prompt with omitted note
		log.Printf("[Compaction] transcript exceeded budget (%d > %d); falling back to base prompt", len(finalPrompt), maxBytes)
		return basePrompt, len(priorTranscriptMsgs), isSummarized, nil
	}

	log.Printf("[Compaction] rendered_bytes=%d omitted_msgs=%d summarized=%t", len(finalPrompt), omittedCount, isSummarized)
	return finalPrompt, omittedCount, isSummarized, nil
}

// SessionSyncState tracks bootstrap and history synchronization per conversation.
type SessionSyncState struct {
	ConvIDHash       string `json:"conv_id_hash"`
	BootstrapVersion int    `json:"bootstrap_version"`
	LastHistoryHash  string `json:"last_history_hash"`
	SyncedTurns      int    `json:"synced_turns"`
}

// SessionSyncTracker persists synchronization metadata without credentials.
type SessionSyncTracker struct {
	mu        sync.Mutex
	storePath string
	sessions  map[string]SessionSyncState // key: sha256(convID)
}

func hashConvID(convID string) string {
	sum := sha256.Sum256([]byte(convID))
	return hex.EncodeToString(sum[:])
}

// NewSessionSyncTracker creates a tracker persisted to context/session_sync.json.
func NewSessionSyncTracker(storePath string) *SessionSyncTracker {
	if storePath == "" {
		if dir := transcriptDir(); dir != "" {
			storePath = filepath.Join(dir, "session_sync.json")
		}
	}
	tracker := &SessionSyncTracker{
		storePath: storePath,
		sessions:  make(map[string]SessionSyncState),
	}
	tracker.load()
	return tracker
}

func (t *SessionSyncTracker) load() {
	if t.storePath == "" {
		return
	}
	data, err := os.ReadFile(t.storePath)
	if err != nil {
		return
	}
	var loaded map[string]SessionSyncState
	if err := json.Unmarshal(data, &loaded); err == nil {
		t.sessions = loaded
	}
}

func (t *SessionSyncTracker) persist() {
	if t.storePath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(t.storePath), 0755)
	data, err := json.MarshalIndent(t.sessions, "", "  ")
	if err == nil {
		_ = os.WriteFile(t.storePath, data, 0644)
	}
}

// IsSessionSynced checks if the conversation ID has already been bootstrapped with history.
func (t *SessionSyncTracker) IsSessionSynced(convID string) bool {
	if convID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h := hashConvID(convID)
	s, ok := t.sessions[h]
	return ok && s.BootstrapVersion > 0
}

// MarkSessionSynced marks the conversation as bootstrapped with the specified history hash.
func (t *SessionSyncTracker) MarkSessionSynced(convID string, historyHash string, turnCount int) {
	if convID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h := hashConvID(convID)
	t.sessions[h] = SessionSyncState{
		ConvIDHash:       h,
		BootstrapVersion: 1,
		LastHistoryHash:  historyHash,
		SyncedTurns:      turnCount,
	}
	t.persist()
}

// ResetSession clears synchronization metadata for the given conversation ID.
func (t *SessionSyncTracker) ResetSession(convID string) {
	if convID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	h := hashConvID(convID)
	delete(t.sessions, h)
	t.persist()
}
