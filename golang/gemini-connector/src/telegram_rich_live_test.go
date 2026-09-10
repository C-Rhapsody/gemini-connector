package main

import (
	"os"
	"strconv"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// TestTelegramRich_LiveContract provides an operator-run live contract smoke test against Telegram Bot API.
// It is strictly skipped by default during normal `go test ./...` or CI runs.
//
// To execute:
//
//	TELEGRAM_RICH_LIVE=1 TELEGRAM_BOT_TOKEN="<token>" TELEGRAM_CHAT_ID="<chat_id>" go test -v -run TestTelegramRich_LiveContract ./...
func TestTelegramRich_LiveContract(t *testing.T) {
	if os.Getenv("TELEGRAM_RICH_LIVE") != "1" {
		t.Skip("skipping live Telegram Rich contract test; set TELEGRAM_RICH_LIVE=1 to execute")
	}

	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		t.Skip("skipping live test: TELEGRAM_BOT_TOKEN is not set")
	}

	rawChatID := os.Getenv("TELEGRAM_CHAT_ID")
	if rawChatID == "" {
		t.Skip("skipping live test: TELEGRAM_CHAT_ID is not set")
	}

	chatID, err := strconv.ParseInt(rawChatID, 10, 64)
	if err != nil || chatID == 0 {
		t.Fatalf("invalid TELEGRAM_CHAT_ID")
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		t.Fatalf("failed to initialize live bot client: %v", err)
	}

	adapter := &TelegramAdapter{
		bot:    bot,
		chatID: chatID,
		richConfig: TelegramRichConfig{
			Enabled:    true,
			MathEscape: "numeric",
		},
		makeRequestFn: bot.MakeRequest,
	}

	type probeCase struct {
		name       string
		markdown   string
		mathEscape string
	}

	probes := []probeCase{
		{
			name:       "rich_html_basic_and_nested",
			markdown:   "# Live Contract Probe\n**Bold**, *italic*, [link](https://telegram.org), and > blockquote with `code`.",
			mathEscape: "raw",
		},
		{
			name:       "inline_math_mid_sentence_and_adjacent",
			markdown:   "Inline math: \\(E = mc^2\\) adjacent to **bold** and *italic* \\(a^2 + b^2 = c^2\\).",
			mathEscape: "raw",
		},
		{
			name:       "two_inline_formulas",
			markdown:   "First \\(x + y = z\\) then second \\(f(x) = \\sin(x)\\).",
			mathEscape: "raw",
		},
		{
			name:       "block_math_top_level",
			markdown:   "Top level block formula:\n\\[\n\\int_0^\\infty e^{-x^2} dx = \\frac{\\sqrt{\\pi}}{2}\n\\]",
			mathEscape: "raw",
		},
		{
			name:       "block_math_in_quote_and_list",
			markdown:   "> Quoted block:\n> \\[x = \\frac{-b \\pm \\sqrt{b^2 - 4ac}}{2a}\\]\n- List item with \\[y = mx + b\\]",
			mathEscape: "raw",
		},
		{
			name:       "multiline_dollars_math",
			markdown:   "Multiline dollar formula:\n$$\n\\sum_{n=1}^{\\infty} \\frac{1}{n^2} = \\frac{\\pi^2}{6}\n$$",
			mathEscape: "raw",
		},
		{
			name:       "numeric_escape_inequalities_and_ampersand",
			markdown:   "Inequalities: \\(a < b\\) and \\(c > d\\) with matrix:\n\\[A \\& B_i < C_j\\]",
			mathEscape: "numeric",
		},
		{
			name:       "code_block_protection",
			markdown:   "```latex\n\\(code inside not math\\)\n\\[block inside not math\\]\n$$\n```\nLiteral text preserves formulas as code.",
			mathEscape: "raw",
		},
	}

	t.Logf("starting live contract probe (%d cases) to target chat", len(probes))

	for idx, tc := range probes {
		t.Run(tc.name, func(subT *testing.T) {
			rendered, err := renderRichHTML(tc.markdown, tc.mathEscape)
			if err != nil {
				subT.Fatalf("case %d (%s) render failed: %v", idx+1, tc.name, err)
			}

			units := utf16Units(rendered)
			if units > 4000 {
				subT.Fatalf("case %d (%s) exceeded length prefilter: %d units", idx+1, tc.name, units)
			}

			msg := &inputRichMessage{HTML: &rendered}
			res, err := adapter.sendRichMessage(chatID, msg, 0)
			if err != nil {
				subT.Fatalf("case %d (%s) API rejected message: %v", idx+1, tc.name, err)
			}
			if res == nil || res.MessageID == 0 {
				subT.Fatalf("case %d (%s) returned empty message ID", idx+1, tc.name)
			}

			subT.Logf("case %d (%s) accepted successfully", idx+1, tc.name)
			time.Sleep(500 * time.Millisecond) // avoid flood control
		})
	}
}
