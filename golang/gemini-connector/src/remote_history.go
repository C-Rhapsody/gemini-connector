package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	remoteHistoryModeHeader     = "X-Hermes-History-Mode"
	remoteHistoryOwnerHeader    = "X-Hermes-Context-Owner"
	remoteHistoryTurnIDHeader   = "X-Hermes-Turn-ID"
	remoteHistorySequenceHeader = "X-Hermes-Turn-Sequence"
	remoteHistoryCurrentOnly    = "current_only"
	remoteHistoryOwnerAGY       = "agy"
)

type RemoteHistoryRequest struct {
	Enabled  bool
	Explicit bool
	TurnID   string
	Sequence int
}

var (
	ErrRemoteReplayInFlight = errors.New("remote history request is already in flight")
	ErrRemoteReplayCapacity = errors.New("remote history replay ledger is at capacity")
)

const (
	maxRemoteReplayEntries      = 512
	maxRemoteReplayTurnIDBytes  = 256
	maxRemoteReplayResponseSize = 2 * 1024 * 1024
	maxRemoteReplayBytes        = 32 * 1024 * 1024
	remoteReplayTTL             = 10 * time.Minute
)

type RemoteReplayResult struct {
	Response string
	Usage    *AgyUsage
}

type remoteReplayEntry struct {
	complete      bool
	createdAt     time.Time
	responseBytes int
	result        RemoteReplayResult
}

// RemoteReplayLedger prevents a transport retry from appending the same logical
// Hermes sequence to AGY twice. It is intentionally memory-only: prompt contents
// and permanent context must not be written to a second local persistence store.
type RemoteReplayLedger struct {
	mu                 sync.Mutex
	entries            map[string]remoteReplayEntry
	totalResponseBytes int
}

func NewRemoteReplayLedger() *RemoteReplayLedger {
	return &RemoteReplayLedger{entries: make(map[string]remoteReplayEntry)}
}

func remoteReplayKey(convID string, request RemoteHistoryRequest, promptHash string) string {
	return convID + "\x00" + request.TurnID + "\x00" + strconv.Itoa(request.Sequence) + "\x00" + promptHash
}

func remoteReplayPromptHash(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:])
}

func remoteReplayFingerprint(prompt, model, schema string, stopSequences []string, responseFormatType string, structured bool) string {
	var b strings.Builder
	values := append([]string{prompt, model, schema, responseFormatType, strconv.FormatBool(structured)}, stopSequences...)
	for _, value := range values {
		fmt.Fprintf(&b, "%d:", len(value))
		b.WriteString(value)
	}
	return b.String()
}

func cloneAgyUsage(u *AgyUsage) *AgyUsage {
	if u == nil {
		return nil
	}
	copy := *u
	return &copy
}

// Acquire returns (cached result, owns execution, error). A new request owns the
// AGY call. A completed identical request receives its cached raw AGY response;
// a changed payload at the same sequence is a distinct AGY request.
func (l *RemoteReplayLedger) Acquire(convID string, request RemoteHistoryRequest, prompt string) (*RemoteReplayResult, bool, error) {
	if l == nil || !request.Enabled {
		return nil, true, nil
	}
	hash := remoteReplayPromptHash(prompt)
	key := remoteReplayKey(convID, request, hash)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]remoteReplayEntry)
	}
	l.evictExpired(time.Now())
	if entry, ok := l.entries[key]; ok {
		if !entry.complete {
			return nil, false, ErrRemoteReplayInFlight
		}
		result := entry.result
		result.Usage = cloneAgyUsage(result.Usage)
		return &result, false, nil
	}
	l.evictOneIfFull()
	if len(l.entries) >= maxRemoteReplayEntries {
		return nil, false, ErrRemoteReplayCapacity
	}
	l.entries[key] = remoteReplayEntry{createdAt: time.Now()}
	return nil, true, nil
}

func (l *RemoteReplayLedger) Complete(convID string, request RemoteHistoryRequest, prompt, response string, usage *AgyUsage) bool {
	if l == nil || !request.Enabled {
		return false
	}
	hash := remoteReplayPromptHash(prompt)
	key := remoteReplayKey(convID, request, hash)
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok || entry.complete {
		return false
	}
	if len(response) > maxRemoteReplayResponseSize {
		delete(l.entries, key)
		return false
	}
	l.evictForResponse(len(response), key)
	if l.totalResponseBytes+len(response) > maxRemoteReplayBytes {
		delete(l.entries, key)
		return false
	}
	entry.complete = true
	entry.responseBytes = len(response)
	entry.result = RemoteReplayResult{Response: response, Usage: cloneAgyUsage(usage)}
	l.totalResponseBytes += entry.responseBytes
	l.entries[key] = entry
	return true
}

func (l *RemoteReplayLedger) Abort(convID string, request RemoteHistoryRequest, prompt string) {
	if l == nil || !request.Enabled {
		return
	}
	hash := remoteReplayPromptHash(prompt)
	key := remoteReplayKey(convID, request, hash)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deleteEntry(key)
}

func (l *RemoteReplayLedger) deleteEntry(key string) {
	if entry, ok := l.entries[key]; ok {
		if entry.complete {
			l.totalResponseBytes -= entry.responseBytes
		}
		delete(l.entries, key)
	}
}

func (l *RemoteReplayLedger) evictExpired(now time.Time) {
	for key, entry := range l.entries {
		if !entry.createdAt.IsZero() && now.Sub(entry.createdAt) >= remoteReplayTTL {
			l.deleteEntry(key)
		}
	}
}

func (l *RemoteReplayLedger) evictForResponse(needed int, keepKey string) {
	for l.totalResponseBytes+needed > maxRemoteReplayBytes {
		var oldestKey string
		var oldest time.Time
		for key, entry := range l.entries {
			if key == keepKey || !entry.complete {
				continue
			}
			if oldestKey == "" || entry.createdAt.Before(oldest) {
				oldestKey, oldest = key, entry.createdAt
			}
		}
		if oldestKey == "" {
			return
		}
		l.deleteEntry(oldestKey)
	}
}

func (l *RemoteReplayLedger) evictOneIfFull() {
	if len(l.entries) < maxRemoteReplayEntries {
		return
	}
	var oldestKey string
	var oldest time.Time
	for key, entry := range l.entries {
		if !entry.complete {
			continue
		}
		if oldestKey == "" || entry.createdAt.Before(oldest) {
			oldestKey, oldest = key, entry.createdAt
		}
	}
	if oldestKey != "" {
		l.deleteEntry(oldestKey)
	}
}

// Requests without the mode header remain on the legacy connector path.
func ParseRemoteHistoryRequest(headers http.Header) (RemoteHistoryRequest, error) {
	mode := strings.TrimSpace(headers.Get(remoteHistoryModeHeader))
	if mode == "" {
		return RemoteHistoryRequest{}, nil
	}
	if mode != remoteHistoryCurrentOnly {
		return RemoteHistoryRequest{}, fmt.Errorf("unsupported history mode %q", mode)
	}
	if owner := strings.TrimSpace(headers.Get(remoteHistoryOwnerHeader)); owner != remoteHistoryOwnerAGY {
		return RemoteHistoryRequest{}, fmt.Errorf("current-only history requires AGY context owner")
	}
	turnID := headers.Get(remoteHistoryTurnIDHeader)
	if strings.TrimSpace(turnID) == "" {
		return RemoteHistoryRequest{}, fmt.Errorf("current-only history requires a turn ID")
	}
	if len(turnID) > maxRemoteReplayTurnIDBytes {
		return RemoteHistoryRequest{}, fmt.Errorf("current-only history turn ID exceeds %d bytes", maxRemoteReplayTurnIDBytes)
	}
	if strings.IndexFunc(turnID, unicode.IsControl) >= 0 {
		return RemoteHistoryRequest{}, fmt.Errorf("current-only history turn ID contains a control character")
	}
	sequenceText := strings.TrimSpace(headers.Get(remoteHistorySequenceHeader))
	sequence, err := strconv.Atoi(sequenceText)
	if err != nil || sequence < 1 {
		return RemoteHistoryRequest{}, fmt.Errorf("current-only history requires a positive turn sequence")
	}
	return RemoteHistoryRequest{Enabled: true, Explicit: true, TurnID: turnID, Sequence: sequence}, nil
}

// SplitRemoteHistoryMessages validates the wire shape before any current-turn
// extraction. This route must never inspect or classify a prior Hermes transcript.
func SplitRemoteHistoryMessages(messages []ChatMessage, sequence int) ([]ChatMessage, []ChatMessage, error) {
	var systemMsgs []ChatMessage
	var nonSystem []ChatMessage
	for _, msg := range messages {
		if msg.Role == "system" || msg.Role == "developer" {
			systemMsgs = append(systemMsgs, msg)
		} else {
			nonSystem = append(nonSystem, msg)
		}
	}
	if sequence <= 1 {
		if len(nonSystem) != 1 || nonSystem[0].Role != "user" {
			return nil, nil, fmt.Errorf("current-only sequence 1 requires exactly one current user message")
		}
		return systemMsgs, nonSystem, nil
	}
	if len(nonSystem) < 2 || nonSystem[0].Role != "assistant" || len(nonSystem[0].ToolCalls) == 0 {
		return nil, nil, fmt.Errorf("current-only sequence %d requires an assistant tool-call and results", sequence)
	}
	if err := validateRemoteToolPair(nonSystem); err != nil {
		return nil, nil, fmt.Errorf("current-only sequence %d has invalid tool pairing: %w", sequence, err)
	}
	return systemMsgs, nonSystem, nil
}

func validateRemoteToolPair(messages []ChatMessage) error {
	callIDs := make(map[string]struct{})
	for _, call := range messages[0].ToolCalls {
		if call.ID == "" {
			return fmt.Errorf("assistant tool-call has no ID")
		}
		if _, duplicate := callIDs[call.ID]; duplicate {
			return fmt.Errorf("assistant tool-call ID %q is duplicated", call.ID)
		}
		callIDs[call.ID] = struct{}{}
	}
	seenResults := make(map[string]struct{})
	for _, msg := range messages[1:] {
		if msg.Role != "tool" {
			return fmt.Errorf("delta contains non-tool message with role %q", msg.Role)
		}
		if msg.ToolCallID == "" {
			return fmt.Errorf("tool result has no tool_call_id")
		}
		if _, ok := callIDs[msg.ToolCallID]; !ok {
			return fmt.Errorf("tool result %q has no matching assistant call", msg.ToolCallID)
		}
		if _, duplicate := seenResults[msg.ToolCallID]; duplicate {
			return fmt.Errorf("tool result %q is duplicated", msg.ToolCallID)
		}
		seenResults[msg.ToolCallID] = struct{}{}
	}
	if len(seenResults) != len(callIDs) {
		return fmt.Errorf("tool result set does not cover all assistant calls")
	}
	return nil
}

// RenderRemoteHistoryPrompt preserves the permanent system prompt while delegating
// transcript ownership to the resumed AGY conversation. Later requests contain only
// new tool results; AGY already has the assistant tool-call in its conversation.
func RenderRemoteHistoryPrompt(
	systemMsgs []ChatMessage,
	currentTurnMsgs []ChatMessage,
	tools []ToolDefinition,
	toolChoice any,
	sequence int,
) (string, error) {
	if sequence <= 1 {
		if len(currentTurnMsgs) == 0 {
			return "", fmt.Errorf("current-only sequence 1 has no current user message")
		}
		hasUser := false
		for _, msg := range currentTurnMsgs {
			if msg.Role == "user" {
				hasUser = true
				break
			}
		}
		if !hasUser {
			return "", fmt.Errorf("current-only sequence 1 requires a current user message")
		}
		return RenderTurnPromptWithTools(systemMsgs, currentTurnMsgs, tools, toolChoice)
	}

	var toolDelta []ChatMessage
	for _, msg := range currentTurnMsgs {
		if msg.Role == "tool" {
			toolDelta = append(toolDelta, msg)
		}
	}
	if len(toolDelta) == 0 {
		return "", fmt.Errorf("current-only sequence %d has no tool-result delta", sequence)
	}
	prompt, err := RenderTurnPromptWithTools(systemMsgs, toolDelta, tools, toolChoice)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("<AGY_REMOTE_HISTORY_DELTA nonce=\"%s\" sequence=\"%d\">\n%s\n</AGY_REMOTE_HISTORY_DELTA>", remoteDeltaNonce(toolDelta), sequence, prompt), nil
}

func remoteDeltaNonce(messages []ChatMessage) string {
	payload, _ := json.Marshal(messages)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:8])
}

// A remote-history resume failure must be surfaced, not converted into a prompt
// containing Hermes' complete transcript. The legacy route retains its fallback.
func shouldFallbackAfterRemoteResumeFailure(remoteHistory bool, err error) bool {
	if remoteHistory || err == nil {
		return false
	}
	var agyErr *AgyError
	return errors.As(err, &agyErr) && agyErr.Type == "session_resume_failed"
}
