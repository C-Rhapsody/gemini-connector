package main

import (
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
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

func statusConversation(cfg *Config, adapter Messenger, chatID string, replyTo int, msgs *Messages) {
	convID := cfg.ConversationID()
	if convID == "" {
		adapter.Send(chatID, msgs.ErrorMissingUUID, SendOptions{ReplyToMessageID: replyTo})
		return
	}
	turns := loadTranscript(convID)
	text := fmt.Sprintf("📋 현재 agy 대화\n\n대화 ID: %s\n기록된 턴 수: %d", convID, len(turns))
	if QuotaActive() {
		text += fmt.Sprintf("\n⏳ quota 제한: %s 후 해제", formatQuotaDuration(QuotaRemaining()))
	}
	adapter.Send(chatID, text, SendOptions{ReplyToMessageID: replyTo})
}

func summaryConversation(cfg *Config, adapter Messenger, chatID string, replyTo int, msgs *Messages) {
	convID := cfg.ConversationID()
	if convID == "" {
		adapter.Send(chatID, msgs.ErrorMissingUUID, SendOptions{ReplyToMessageID: replyTo})
		return
	}
	turns := loadTranscript(convID)
	if len(turns) == 0 {
		adapter.Send(chatID, "📄 아직 기록된 대화가 없습니다.", SendOptions{ReplyToMessageID: replyTo})
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
	adapter.Send(chatID, text, SendOptions{ReplyToMessageID: replyTo})
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

func versionInfo(adapter Messenger, chatID string, replyTo int, msgs *Messages) {
	agyVersion := "확인 불가"
	out, err := exec.Command("agy", "--version").Output()
	if err != nil {
		log.Printf("Failed to get agy version: %v", err)
	} else if v := strings.TrimSpace(string(out)); v != "" {
		agyVersion = v
	}
	adapter.Send(chatID, fmt.Sprintf("ℹ️ 버전 정보\n\n커넥터: %s\nagy: %s", version, agyVersion), SendOptions{ReplyToMessageID: replyTo})
}

func listConversations(cfg *Config, adapter Messenger, chatID, pageStr string, replyTo int, msgs *Messages) {
	entries, err := loadConversationCache()
	if err != nil {
		adapter.Send(chatID, fmt.Sprintf("⚠️ 대화 캐시를 읽지 못했습니다: %v", err), SendOptions{ReplyToMessageID: replyTo})
		return
	}
	if len(entries) == 0 {
		adapter.Send(chatID, "📭 캐시된 agy 대화가 없습니다.", SendOptions{ReplyToMessageID: replyTo})
		return
	}

	// Active conversation first, then the rest by workspace (stable).
	activeID := cfg.ConversationID()
	activeInCache := false
	for _, e := range entries {
		if e.ID == activeID {
			activeInCache = true
			break
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		iActive := entries[i].ID == activeID
		jActive := entries[j].ID == activeID
		if iActive != jActive {
			return iActive
		}
		return entries[i].Workspace < entries[j].Workspace
	})

	total := len(entries)
	totalPages := (total + listMaxEntries - 1) / listMaxEntries
	page := 1
	if p, perr := strconv.Atoi(strings.TrimSpace(pageStr)); perr == nil && p > 1 {
		page = p
	}
	if page > totalPages {
		adapter.Send(chatID, fmt.Sprintf("⚠️ 페이지가 범위를 벗어났습니다. (1~%d 페이지)", totalPages), SendOptions{ReplyToMessageID: replyTo})
		return
	}

	start := (page - 1) * listMaxEntries
	end := min(start+listMaxEntries, total)

	var b strings.Builder
	if activeID != "" && !activeInCache {
		b.WriteString(fmt.Sprintf("📌 현재 대화: %s (캐시에 없음)\n\n", activeID))
	}
	if totalPages > 1 {
		b.WriteString(fmt.Sprintf("📁 agy 대화 캐시 (총 %d개, %d/%d 페이지)\n\n", total, page, totalPages))
	} else {
		b.WriteString(fmt.Sprintf("📁 agy 대화 캐시 (총 %d개)\n\n", total))
	}
	for i := start; i < end; i++ {
		conv := entries[i]
		marker := ""
		if conv.ID == activeID {
			marker = "  ★ 현재"
		}
		b.WriteString(fmt.Sprintf("%d. %s%s\n   📂 %s — %s\n\n", i+1, conv.ID, marker, filepath.Base(conv.Workspace), conv.Workspace))
	}
	if page < totalPages {
		b.WriteString(fmt.Sprintf("/list %d 로 다음 페이지 · ", page+1))
	}
	b.WriteString("/switch <ID> 로 대화를 전환할 수 있습니다.")
	adapter.Send(chatID, b.String(), SendOptions{Plain: true, ReplyToMessageID: replyTo})
}

func switchConversation(cfg *Config, adapter Messenger, chatID, args string, replyTo int, msgs *Messages) {
	newID := strings.TrimSpace(args)
	if newID == "" {
		adapter.Send(chatID, "ℹ️ 사용법: /switch <대화 ID>\n예: /switch 1234abcd-…", SendOptions{ReplyToMessageID: replyTo})
		return
	}
	if err := updateEnvKey(cfg.envPath, "AGY_CONVERSATION_ID", newID); err != nil {
		log.Printf("Failed to update .env with new conversation ID: %v", err)
	}
	cfg.applyNewConversation(newID)
	adapter.Send(chatID, fmt.Sprintf("✅ 대화를 전환했습니다. (새 대화 ID: %s)", truncateString(newID, 8)), SendOptions{ReplyToMessageID: replyTo})
}
