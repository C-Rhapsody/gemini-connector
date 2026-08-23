package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// agyTranscriptStep mirrors the fields of a brain transcript line needed to
// tell whether a turn actually produced a final answer.
type agyTranscriptStep struct {
	StepIndex int             `json:"step_index"`
	Source    string          `json:"source"`
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"created_at"`
	Content   string          `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

// agyBrainDirOverride allows tests to redirect the brain directory.
var agyBrainDirOverride string

// agyBrainTranscriptPath points at the compact brain transcript that agy
// maintains for the given conversation.
func agyBrainTranscriptPath(convID string) string {
	if convID == "" {
		return ""
	}
	base := agyBrainDirOverride
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".gemini", "antigravity-cli", "brain")
	}
	return filepath.Join(base, convID, ".system_generated", "logs", "transcript.jsonl")
}

// salvageTurnResponse recovers the final answer of a turn that agy reported
// as ERROR. Some intermediate tool failures - for example agy's internal
// grep_search exiting non-zero - surface as the CLI's top-level status even
// though the model went on to finish the turn and answer; the authoritative
// record of that answer is the brain transcript. A candidate response only
// counts when it appears after the matching user input of this turn, carries
// text content, and is not a tool-call step.
func salvageTurnResponse(convID string, userPrompt string, turnStart time.Time) string {
	path := agyBrainTranscriptPath(convID)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	fragment := promptFragment(userPrompt)
	steps := parseAgyTranscriptSteps(data)

	userIdx := -1
	for i, s := range steps {
		if s.Type != "USER_INPUT" || !s.done() || !s.after(turnStart) {
			continue
		}
		if fragment == "" || strings.Contains(s.Content, fragment) {
			userIdx = i
		}
	}
	if userIdx < 0 {
		return ""
	}

	best := ""
	for _, s := range steps[userIdx+1:] {
		if s.Type != "PLANNER_RESPONSE" || !s.done() ||
			s.hasToolCalls() || strings.TrimSpace(s.Content) == "" {
			continue
		}
		best = s.Content
	}
	return best
}

func (s agyTranscriptStep) done() bool {
	return s.Status == "DONE"
}

func (s agyTranscriptStep) hasToolCalls() bool {
	return len(s.ToolCalls) > 0 && string(s.ToolCalls) != "null"
}

// after reports whether the step was created around or after t. Brain
// timestamps are second-granularity UTC, so a small tolerance absorbs clock
// skew between the connector and agy.
func (s agyTranscriptStep) after(t time.Time) bool {
	ts, err := time.Parse(time.RFC3339, s.CreatedAt)
	if err != nil {
		return false
	}
	return !ts.Before(t.Add(-2 * time.Second))
}

// promptFragment extracts a short identifying prefix of the outgoing prompt:
// the first non-empty line, capped at 64 runes.
func promptFragment(userPrompt string) string {
	line := ""
	for _, l := range strings.Split(userPrompt, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			line = t
			break
		}
	}
	r := []rune(line)
	if len(r) > 64 {
		r = r[:64]
	}
	return string(r)
}

func parseAgyTranscriptSteps(data []byte) []agyTranscriptStep {
	var steps []agyTranscriptStep
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s agyTranscriptStep
		if err := json.Unmarshal([]byte(line), &s); err == nil {
			steps = append(steps, s)
		}
	}
	return steps
}
