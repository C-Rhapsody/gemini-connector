package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	transcriptMaxTurns = 40
	transcriptMaxChars = 20000
)

// TranscriptTurn is a single user/assistant exchange stored per conversation.
type TranscriptTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var transcriptMu sync.Mutex

// transcriptDirOverride allows tests to redirect the transcript directory.
var transcriptDirOverride string

func transcriptDir() string {
	if transcriptDirOverride != "" {
		return transcriptDirOverride
	}
	exePath, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exePath), "..", "context")
}

func transcriptPath(convID string) string {
	return filepath.Join(transcriptDir(), convID+".json")
}

func appendTranscript(convID string, role string, content string) {
	transcriptMu.Lock()
	defer transcriptMu.Unlock()

	turns := loadTranscriptUnlocked(convID)
	turns = append(turns, TranscriptTurn{Role: role, Content: content})

	if dir := transcriptDir(); dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	data, err := json.MarshalIndent(turns, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(transcriptPath(convID), data, 0644)
}

func loadTranscript(convID string) []TranscriptTurn {
	transcriptMu.Lock()
	defer transcriptMu.Unlock()
	return loadTranscriptUnlocked(convID)
}

func loadTranscriptUnlocked(convID string) []TranscriptTurn {
	data, err := os.ReadFile(transcriptPath(convID))
	if err != nil {
		return nil
	}
	var turns []TranscriptTurn
	if err := json.Unmarshal(data, &turns); err != nil {
		return nil
	}
	return turns
}

func deleteTranscript(convID string) {
	transcriptMu.Lock()
	defer transcriptMu.Unlock()
	_ = os.Remove(transcriptPath(convID))
}

// buildSummaryPrompt assembles a summarization prompt from the recorded
// transcript. It caps the included content to the most recent turns and a
// character budget so the prompt stays manageable, while the transcript file
// keeps the full history.
func buildSummaryPrompt(convID string) string {
	turns := loadTranscript(convID)
	if len(turns) == 0 {
		return ""
	}

	start := 0
	if len(turns) > transcriptMaxTurns {
		start = len(turns) - transcriptMaxTurns
	}

	var b strings.Builder
	b.WriteString("다음은 사용자와 AI 어시스턴트의 이전 대화 기록입니다. ")
	b.WriteString("이 대화에서 다룬 핵심 주제, 중요한 결정과 맥락, 사용자의 의도를 간결하게 요약해 주세요. ")
	b.WriteString("이후 대화는 이 요약을 기준으로 계속 이어집니다.\n\n")

	if start > 0 {
		b.WriteString("…(이전 대화 일부 생략)…\n\n")
	}

	for i := start; i < len(turns); i++ {
		turn := turns[i]
		b.WriteString("[" + turn.Role + "]\n")
		b.WriteString(turn.Content)
		b.WriteString("\n\n")
	}

	text := b.String()
	if len(text) > transcriptMaxChars {
		cut := len(text) - transcriptMaxChars
		idx := strings.Index(text[cut:], "\n")
		if idx > 0 {
			cut += idx
		}
		text = "…(이전 대화 일부 생략)…\n" + text[cut:]
	}
	return text
}
