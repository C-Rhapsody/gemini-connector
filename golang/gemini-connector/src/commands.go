package main

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// version is the connector version, overridable at build time via
// -ldflags "-X main.version=...". Defaults to "dev".
var version = "dev"

const (
	summaryPreviewTurns = 10
	summaryPreviewChars = 2000
	summaryTurnMaxChars = 400
	listMaxEntries      = 10
)

func statusConversation(cfg *Config, adapter Messenger, chatID string, msgs *Messages) {
	convID := cfg.ConversationID()
	if convID == "" {
		adapter.Send(chatID, msgs.ErrorMissingUUID)
		return
	}
	turns := loadTranscript(convID)
	adapter.Send(chatID, fmt.Sprintf("📋 현재 agy 대화\n\n대화 ID: %s\n기록된 턴 수: %d", convID, len(turns)))
}

func summaryConversation(cfg *Config, adapter Messenger, chatID string, msgs *Messages) {
	convID := cfg.ConversationID()
	if convID == "" {
		adapter.Send(chatID, msgs.ErrorMissingUUID)
		return
	}
	turns := loadTranscript(convID)
	if len(turns) == 0 {
		adapter.Send(chatID, "📄 아직 기록된 대화가 없습니다.")
		return
	}

	start := 0
	if len(turns) > summaryPreviewTurns {
		start = len(turns) - summaryPreviewTurns
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("📄 최근 대화 미리보기 (총 %d턴 중 최근 %d턴)\n\n", len(turns), len(turns)-start))
	if start > 0 {
		b.WriteString("…(이전 대화 일부 생략)…\n\n")
	}
	for i := start; i < len(turns); i++ {
		content := truncateRunes(turns[i].Content, summaryTurnMaxChars)
		if len([]rune(turns[i].Content)) > summaryTurnMaxChars {
			content += "…"
		}
		b.WriteString("[" + turns[i].Role + "]\n")
		b.WriteString(content)
		b.WriteString("\n\n")
	}

	text := b.String()
	if len([]rune(text)) > summaryPreviewChars {
		text = truncateRunes(text, summaryPreviewChars) + "\n…(미리보기 일부 생략)…"
	}
	adapter.Send(chatID, text)
}

// truncateRunes truncates a string to at most max runes, never splitting a
// multi-byte character.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func versionInfo(adapter Messenger, chatID string, msgs *Messages) {
	agyVersion := "확인 불가"
	out, err := exec.Command("agy", "--version").Output()
	if err != nil {
		log.Printf("Failed to get agy version: %v", err)
	} else if v := strings.TrimSpace(string(out)); v != "" {
		agyVersion = v
	}
	adapter.Send(chatID, fmt.Sprintf("ℹ️ 버전 정보\n\n커넥터: %s\nagy: %s", version, agyVersion))
}

func listConversations(adapter Messenger, chatID string, msgs *Messages) {
	entries, err := loadConversationCache()
	if err != nil {
		adapter.Send(chatID, fmt.Sprintf("⚠️ 대화 캐시를 읽지 못했습니다: %v", err))
		return
	}
	if len(entries) == 0 {
		adapter.Send(chatID, "📭 캐시된 agy 대화가 없습니다.")
		return
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("📁 캐시된 agy 대화 목록 (총 %d개 중 상위 %d개)\n\n", len(entries), min(listMaxEntries, len(entries))))
	for i := 0; i < listMaxEntries && i < len(entries); i++ {
		conv := entries[i]
		b.WriteString(fmt.Sprintf("[%d] %s\n    (%s)\n\n", i+1, truncateString(conv.ID, 36), truncateString(conv.Workspace, 40)))
	}
	b.WriteString("/switch <ID> 로 대화를 전환할 수 있습니다.")
	adapter.Send(chatID, b.String())
}

func switchConversation(cfg *Config, adapter Messenger, chatID string, args string, msgs *Messages) {
	newID := strings.TrimSpace(args)
	if newID == "" {
		adapter.Send(chatID, "ℹ️ 사용법: /switch <대화 ID>\n예: /switch 1234abcd-…")
		return
	}
	if err := updateEnvKey(cfg.envPath, "AGY_CONVERSATION_ID", newID); err != nil {
		log.Printf("Failed to update .env with new conversation ID: %v", err)
	}
	cfg.applyNewConversation(newID)
	adapter.Send(chatID, fmt.Sprintf("✅ 대화를 전환했습니다. (새 대화 ID: %s)", truncateString(newID, 8)))
}
