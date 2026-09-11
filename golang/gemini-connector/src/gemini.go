package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type AgyResponse struct {
	ConversationID   string          `json:"conversation_id"`
	Status           string          `json:"status"`
	Response         string          `json:"response"`
	Error            string          `json:"error,omitempty"`
	DurationSeconds  float64         `json:"duration_seconds"`
	NumTurns         int             `json:"num_turns"`
	Usage            *AgyUsage       `json:"usage,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
}

type AgyUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

type AgyError struct {
	Type   string
	Err    error
	Detail string
}

func (e *AgyError) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Detail)
}

// AgyProfile selects the execution policy of one agy invocation. Profiles
// make the intent explicit at the call site instead of leaking boolean
// flags (e.g. planner mode) through the runner boundary.
type AgyProfile int

const (
	// ProfileInteractive is a regular user chat turn.
	ProfileInteractive AgyProfile = iota
	// ProfilePlanner runs under plan/sandbox restrictions for /cron candidate
	// generation: the model must answer with a single JSON object and cannot
	// modify the workspace while producing it.
	ProfilePlanner
	// ProfileScheduled is a cron-triggered background turn.
	ProfileScheduled
	// ProfileBootstrap creates or replays into a fresh conversation.
	ProfileBootstrap
	// ProfileAPI runs stateless OpenAI-compatible completions with sandbox restrictions
	// and no dangerous permissions.
	ProfileAPI
)

// agyLaunchSem is the shared AGY-launch mutex ensuring that at most one AGY
// process (chat turn, model discovery, cron planning) runs at any time.
var agyLaunchSem = make(chan struct{}, 1)

func init() {
	agyLaunchSem <- struct{}{}
}

func acquireAgyLaunch(ctx context.Context, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-agyLaunchSem:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func releaseAgyLaunch() {
	select {
	case agyLaunchSem <- struct{}{}:
	default:
	}
}

var urlFetchFailurePattern = regexp.MustCompile(`Failed to fetch document content at (\S+)`)

func extractUrlFetchFailure(detail string) string {
	match := urlFetchFailurePattern.FindStringSubmatch(detail)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func findProjectRoot() string {
	searchDir, err := os.Executable()
	if err != nil {
		return ""
	}
	searchDir = filepath.Dir(searchDir)
	for {
		if info, err := os.Stat(filepath.Join(searchDir, ".gemini")); err == nil && info.IsDir() {
			return searchDir
		}
		parentDir := filepath.Dir(searchDir)
		if parentDir == searchDir {
			break
		}
		searchDir = parentDir
	}
	return ""
}
