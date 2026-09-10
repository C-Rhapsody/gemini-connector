package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestSendRichMessage_WireFormat(t *testing.T) {
	var capturedEndpoint string
	var capturedParams tgbotapi.Params

	adapter := &TelegramAdapter{
		chatID: 123456,
		makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
			capturedEndpoint = endpoint
			capturedParams = params
			resultJSON := []byte(`{"message_id": 999, "chat": {"id": 123456}}`)
			return &tgbotapi.APIResponse{
				Ok:     true,
				Result: resultJSON,
			}, nil
		},
	}

	htmlContent := "Hello <b>world</b> <tg-math>x &lt; y</tg-math>"
	richMsg := &inputRichMessage{
		HTML: &htmlContent,
	}

	res, err := adapter.sendRichMessage(123456, richMsg, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.MessageID != 999 || res.Chat.ID != 123456 {
		t.Fatalf("unexpected result: %+v", res)
	}

	if capturedEndpoint != "sendRichMessage" {
		t.Fatalf("expected endpoint 'sendRichMessage', got %q", capturedEndpoint)
	}

	if capturedParams["chat_id"] != "123456" {
		t.Fatalf("expected chat_id 123456, got %q", capturedParams["chat_id"])
	}

	rawRich := capturedParams["rich_message"]
	if rawRich == "" {
		t.Fatal("rich_message parameter is empty")
	}

	var decodedRich struct {
		HTML     *string `json:"html"`
		Markdown *string `json:"markdown"`
		Blocks   any     `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(rawRich), &decodedRich); err != nil {
		t.Fatalf("failed to decode rich_message JSON: %v", err)
	}
	if decodedRich.HTML == nil || *decodedRich.HTML != htmlContent {
		t.Fatalf("expected HTML %q, got %+v", htmlContent, decodedRich.HTML)
	}
	if decodedRich.Markdown != nil {
		t.Fatalf("expected Markdown to be omitted, got %v", *decodedRich.Markdown)
	}
	if decodedRich.Blocks != nil {
		t.Fatalf("expected Blocks to be omitted, got %v", decodedRich.Blocks)
	}

	rawReply := capturedParams["reply_parameters"]
	if rawReply == "" {
		t.Fatal("reply_parameters parameter is empty")
	}
	var decodedReply struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal([]byte(rawReply), &decodedReply); err != nil {
		t.Fatalf("failed to decode reply_parameters JSON: %v", err)
	}
	if decodedReply.MessageID != 42 {
		t.Fatalf("expected reply_parameters.message_id 42, got %d", decodedReply.MessageID)
	}
}

func TestSendRichMessage_NilInputFails(t *testing.T) {
	adapter := &TelegramAdapter{
		chatID: 123456,
		makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
			return &tgbotapi.APIResponse{Ok: true}, nil
		},
	}

	_, err := adapter.sendRichMessage(123456, nil, 0)
	if err == nil {
		t.Fatal("expected error when sending nil rich_message, got nil")
	}

	_, err = adapter.sendRichMessage(123456, &inputRichMessage{HTML: nil}, 0)
	if err == nil {
		t.Fatal("expected error when sending rich_message with nil HTML, got nil")
	}
}

func TestUtf16Units(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello", 5},
		{"안녕하세요", 5},
		{"🚀", 2},
		{"a🚀b", 4},
		{"𝔽", 2},
		{"hello 𝔽 world", 14},
	}
	for _, c := range cases {
		if got := utf16Units(c.in); got != c.want {
			t.Errorf("utf16Units(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestIsRichHTMLWithinLimit(t *testing.T) {
	// Exact 4000 BMP characters
	str4000 := strings.Repeat("a", 4000)
	if !isRichHTMLWithinLimit(str4000) {
		t.Errorf("expected 4000 ASCII chars to be within limit")
	}

	// 4001 BMP characters
	str4001 := strings.Repeat("a", 4001)
	if isRichHTMLWithinLimit(str4001) {
		t.Errorf("expected 4001 ASCII chars to exceed limit")
	}

	// 2000 emojis (each 2 UTF-16 units = 4000 units)
	emoji2000 := strings.Repeat("🚀", 2000)
	if !isRichHTMLWithinLimit(emoji2000) {
		t.Errorf("expected 2000 emojis (4000 units) to be within limit")
	}

	// 2001 emojis (4002 units)
	emoji2001 := strings.Repeat("🚀", 2001)
	if isRichHTMLWithinLimit(emoji2001) {
		t.Errorf("expected 2001 emojis (4002 units) to exceed limit")
	}
}

func TestRichEligible(t *testing.T) {
	now := time.Now()
	validOpt := SendOptions{
		AttachAfter: now,
		Plain:       false,
	}

	baseAdapter := func() *TelegramAdapter {
		return &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
		}
	}

	t.Run("eligible when all conditions met", func(t *testing.T) {
		a := baseAdapter()
		if !a.richEligible(12345, "valid text", validOpt) {
			t.Errorf("expected eligible")
		}
	})

	t.Run("ineligible when rich disabled", func(t *testing.T) {
		a := baseAdapter()
		a.richConfig.Enabled = false
		if a.richEligible(12345, "valid text", validOpt) {
			t.Errorf("expected ineligible when rich disabled")
		}
	})

	t.Run("ineligible when configured chat ID is zero", func(t *testing.T) {
		a := baseAdapter()
		a.richConfig.ChatID = 0
		if a.richEligible(12345, "valid text", validOpt) {
			t.Errorf("expected ineligible when configured chat ID is 0")
		}
	})

	t.Run("ineligible when target chat ID does not match", func(t *testing.T) {
		a := baseAdapter()
		if a.richEligible(99999, "valid text", validOpt) {
			t.Errorf("expected ineligible for chat ID mismatch")
		}
	})

	t.Run("ineligible when Plain is true", func(t *testing.T) {
		a := baseAdapter()
		opt := validOpt
		opt.Plain = true
		if a.richEligible(12345, "valid text", opt) {
			t.Errorf("expected ineligible when Plain is true")
		}
	})

	t.Run("ineligible when AttachAfter is zero (command/cron)", func(t *testing.T) {
		a := baseAdapter()
		opt := validOpt
		opt.AttachAfter = time.Time{}
		if a.richEligible(12345, "valid text", opt) {
			t.Errorf("expected ineligible when AttachAfter is zero")
		}
	})

	t.Run("ineligible when text is empty", func(t *testing.T) {
		a := baseAdapter()
		if a.richEligible(12345, "", validOpt) {
			t.Errorf("expected ineligible when text is empty")
		}
		if a.richEligible(12345, "   \n\t  ", validOpt) {
			t.Errorf("expected ineligible when text is only whitespace")
		}
	})

	t.Run("ineligible when process-disable latch is active", func(t *testing.T) {
		a := baseAdapter()
		a.disableRichProcess()
		if a.richEligible(12345, "valid text", validOpt) {
			t.Errorf("expected ineligible when process is disabled")
		}
	})
}

func TestTokenizeLaTeX(t *testing.T) {
	t.Run("inline parenthesis formula", func(t *testing.T) {
		in := "The equation is \\(E = mc^2\\) inline."
		masked, tokens := tokenizeLaTeX(in)
		if len(tokens) != 1 {
			t.Fatalf("expected 1 token, got %d", len(tokens))
		}
		if tokens[0].Kind != MathInline || tokens[0].Formula != "E = mc^2" {
			t.Errorf("unexpected token: %+v", tokens[0])
		}
		if !strings.Contains(masked, tokens[0].Placeholder) {
			t.Errorf("masked string %q does not contain placeholder %q", masked, tokens[0].Placeholder)
		}
	})

	t.Run("block bracket formula", func(t *testing.T) {
		in := "Block math:\n\\[\\int_0^1 x dx = \\frac{1}{2}\\]\ndone."
		masked, tokens := tokenizeLaTeX(in)
		if len(tokens) != 1 {
			t.Fatalf("expected 1 token, got %d", len(tokens))
		}
		if tokens[0].Kind != MathBlock || tokens[0].Formula != "\\int_0^1 x dx = \\frac{1}{2}" {
			t.Errorf("unexpected token: %+v", tokens[0])
		}
		if !strings.Contains(masked, tokens[0].Placeholder) {
			t.Errorf("masked string %q does not contain placeholder %q", masked, tokens[0].Placeholder)
		}
	})

	t.Run("block double dollar formula and multiline", func(t *testing.T) {
		in := "Matrix:\n$$\n\\begin{matrix}\n1 & 0 \\\\\n0 & 1\n\\end{matrix}\n$$\nEnd."
		_, tokens := tokenizeLaTeX(in)
		if len(tokens) != 1 {
			t.Fatalf("expected 1 token, got %d", len(tokens))
		}
		if tokens[0].Kind != MathBlock || !strings.Contains(tokens[0].Formula, "\\begin{matrix}") {
			t.Errorf("unexpected token: %+v", tokens[0])
		}
	})

	t.Run("bare dollar currency is not math", func(t *testing.T) {
		in := "Item costs $100 and tax is $10. Total: $110."
		masked, tokens := tokenizeLaTeX(in)
		if len(tokens) != 0 {
			t.Fatalf("expected 0 tokens for bare dollars, got %d: %+v", len(tokens), tokens)
		}
		if masked != in {
			t.Errorf("masked text should match input exactly, got %q", masked)
		}
	})

	t.Run("fenced backtick code block is not math", func(t *testing.T) {
		in := "Here is code:\n```latex\n\\(x + y\\)\n\\[z\\]\n$$\\alpha$$\n```\nend."
		masked, tokens := tokenizeLaTeX(in)
		if len(tokens) != 0 {
			t.Fatalf("expected 0 tokens inside fenced code, got %d", len(tokens))
		}
		if masked != in {
			t.Errorf("masked code should match input exactly")
		}
	})

	t.Run("fenced tilde code block is not math", func(t *testing.T) {
		in := "Here is code:\n~~~latex\n\\(x + y\\)\n~~~\nend."
		_, tokens := tokenizeLaTeX(in)
		if len(tokens) != 0 {
			t.Fatalf("expected 0 tokens inside tilde code, got %d", len(tokens))
		}
	})

	t.Run("indented code block is not math", func(t *testing.T) {
		in := "Normal paragraph:\n\n    \\(x + y\\) in indented code\n    $$block$$\n\nAfter code."
		_, tokens := tokenizeLaTeX(in)
		if len(tokens) != 0 {
			t.Fatalf("expected 0 tokens inside indented code, got %d", len(tokens))
		}
	})

	t.Run("inline code with single backticks is not math", func(t *testing.T) {
		in := "Use `\\(x + y\\)` or `$$z$$` for formulas."
		masked, tokens := tokenizeLaTeX(in)
		if len(tokens) != 0 {
			t.Fatalf("expected 0 tokens inside inline code, got %d", len(tokens))
		}
		if masked != in {
			t.Errorf("masked text should match input")
		}
	})

	t.Run("inline code with variable length double backticks", func(t *testing.T) {
		in := "Use `` `\\(x\\)` `` as code."
		_, tokens := tokenizeLaTeX(in)
		if len(tokens) != 0 {
			t.Fatalf("expected 0 tokens inside variable backticks code, got %d", len(tokens))
		}
	})

	t.Run("raw HTML pre and code tags are not math", func(t *testing.T) {
		in := "<pre>\\(x + y\\)</pre> and <code>$$z$$</code>"
		_, tokens := tokenizeLaTeX(in)
		if len(tokens) != 0 {
			t.Fatalf("expected 0 tokens inside raw HTML, got %d", len(tokens))
		}
	})

	t.Run("two formulas in one line", func(t *testing.T) {
		in := "First \\(a + b\\) and second \\(c + d\\)."
		masked, tokens := tokenizeLaTeX(in)
		if len(tokens) != 2 {
			t.Fatalf("expected 2 tokens, got %d", len(tokens))
		}
		if tokens[0].Formula != "a + b" || tokens[1].Formula != "c + d" {
			t.Errorf("unexpected token formulas: %+v", tokens)
		}
		if tokens[0].Placeholder == tokens[1].Placeholder {
			t.Errorf("placeholders must be unique: %q vs %q", tokens[0].Placeholder, tokens[1].Placeholder)
		}
		if !strings.Contains(masked, tokens[0].Placeholder) || !strings.Contains(masked, tokens[1].Placeholder) {
			t.Errorf("masked missing placeholder")
		}
	})

	t.Run("escaped delimiters do not trigger math", func(t *testing.T) {
		in := `Escaped \\(not math\\) and normal \(math\)`
		_, tokens := tokenizeLaTeX(in)
		if len(tokens) != 1 {
			t.Fatalf("expected 1 token for normal math only, got %d: %+v", len(tokens), tokens)
		}
		if tokens[0].Formula != "math" {
			t.Errorf("expected token formula 'math', got %q", tokens[0].Formula)
		}
	})

	t.Run("unbalanced delimiters remain literal and are never dropped", func(t *testing.T) {
		in := "An unmatched \\(opening without close, and an unmatched \\[block too."
		masked, tokens := tokenizeLaTeX(in)
		if len(tokens) != 0 {
			t.Fatalf("expected 0 tokens for unmatched delimiters, got %d", len(tokens))
		}
		if masked != in {
			t.Errorf("unmatched text should be preserved verbatim, got %q", masked)
		}
	})

	t.Run("placeholder does not collide with common text", func(t *testing.T) {
		in := "Text contains TGMATH and TGMATH0123. Math is \\(x = 1\\)."
		masked, tokens := tokenizeLaTeX(in)
		if len(tokens) != 1 {
			t.Fatalf("expected 1 token, got %d", len(tokens))
		}
		if !strings.HasPrefix(tokens[0].Placeholder, "TGMATH") {
			t.Errorf("expected placeholder to start with TGMATH")
		}
		// Make sure the placeholder is distinct from the text
		if strings.Count(masked, tokens[0].Placeholder) != 1 {
			t.Errorf("placeholder must appear exactly once in masked text")
		}
	})
}

func TestRenderRichHTML(t *testing.T) {
	t.Run("no-math input produces byte-identical output to convertMarkdownToTelegramHTML", func(t *testing.T) {
		inputs := []string{
			"Hello **world**",
			"# Heading\nParagraph with *italic* and [link](https://example.com)",
			"- item 1\n- item 2\n  - nested",
			"> a blockquote with <code>code</code>",
			"```go\nfunc main() {}\n```",
		}
		for _, in := range inputs {
			want := convertMarkdownToTelegramHTML(in)
			got, err := renderRichHTML(in, "raw")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != want {
				t.Errorf("expected byte-identical output:\ngot:  %q\nwant: %q", got, want)
			}
		}
	})

	t.Run("inline and block restoration with numeric escape", func(t *testing.T) {
		in := "Inline \\(a < b & c > d\\) and block:\n\\[x \\le y\\]"
		got, err := renderRichHTML(in, "numeric")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		wantInline := "<tg-math>a &#60; b &#38; c &#62; d</tg-math>"
		wantBlock := "<tg-math-block>x \\le y</tg-math-block>"
		if !strings.Contains(got, wantInline) {
			t.Errorf("missing expected inline math %q in %q", wantInline, got)
		}
		if !strings.Contains(got, wantBlock) {
			t.Errorf("missing expected block math %q in %q", wantBlock, got)
		}
	})

	t.Run("raw escape rejects HTML-significant characters", func(t *testing.T) {
		in := "Formula with less than: \\(a < b\\)"
		_, err := renderRichHTML(in, "raw")
		if err == nil {
			t.Fatal("expected error for formula with '<' in raw escape profile, got nil")
		}

		inAmp := "Formula with amp: \\(A & B\\)"
		_, err = renderRichHTML(inAmp, "raw")
		if err == nil {
			t.Fatal("expected error for formula with '&' in raw escape profile, got nil")
		}

		inSafe := "Safe formula: \\(E = mc^2\\)"
		gotSafe, err := renderRichHTML(inSafe, "raw")
		if err != nil {
			t.Fatalf("unexpected error for safe formula: %v", err)
		}
		if !strings.Contains(gotSafe, "<tg-math>E = mc^2</tg-math>") {
			t.Errorf("expected safe formula in <tg-math>, got %q", gotSafe)
		}
	})

	t.Run("duplicate formulas restore exactly N times", func(t *testing.T) {
		in := "First \\(x = 1\\) and second \\(x = 1\\) and third \\(x = 1\\)."
		got, err := renderRichHTML(in, "raw")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		count := strings.Count(got, "<tg-math>x = 1</tg-math>")
		if count != 3 {
			t.Errorf("expected 3 restorations of '<tg-math>x = 1</tg-math>', got %d in %q", count, got)
		}
	})

	t.Run("math adjacent to markdown constructs", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
			want string
		}{
			{"adjacent to bold", "**bold**\\(x = 1\\)", "<b>bold</b><tg-math>x = 1</tg-math>"},
			{"inside bold", "**\\(x = 1\\)**", "<b><tg-math>x = 1</tg-math></b>"},
			{"adjacent to italic", "*italic*\\(x = 1\\)", "<i>italic</i><tg-math>x = 1</tg-math>"},
			{"adjacent to link", "[link](https://example.com)\\(x = 1\\)", `<a href="https://example.com">link</a><tg-math>x = 1</tg-math>`},
			{"in heading", "# Header \\(x = 1\\)", "<b>Header <tg-math>x = 1</tg-math></b>"},
			{"in list item", "- list item \\(x = 1\\)", "<tg-math>x = 1</tg-math>"},
			{"in blockquote", "> quote \\(x = 1\\)", "<blockquote>quote <tg-math>x = 1</tg-math></blockquote>"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := renderRichHTML(tc.in, "raw")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !strings.Contains(got, tc.want) {
					t.Errorf("got %q, want it to contain %q", got, tc.want)
				}
			})
		}
	})

	t.Run("code containing literal LaTeX remains code and never restores as tg-math", func(t *testing.T) {
		in := "Code: `\\(x = 1\\)` and block:\n```\n\\(y = 2\\)\n```"
		got, err := renderRichHTML(in, "raw")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(got, "<tg-math>") {
			t.Errorf("code blocks must never contain <tg-math>, got %q", got)
		}
		if !strings.Contains(got, "<code>\\(x = 1\\)</code>") {
			t.Errorf("expected inline code with literal LaTeX, got %q", got)
		}
	})
}



