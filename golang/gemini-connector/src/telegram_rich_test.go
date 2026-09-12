package main

import (
	"encoding/json"
	"errors"
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
	t.Run("rich contract verifies paragraphs and intra-paragraph line breaks", func(t *testing.T) {
		in := "Line 1\nLine 2\n\nLine 3"
		got, err := renderRichHTML(in, "raw")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "<p>Line 1<br>Line 2</p><p>Line 3</p>"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("rich contract verifies basic markdown elements", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
			want string
		}{
			{
				name: "single paragraph with bold",
				in:   "Hello **world**",
				want: "<p>Hello <b>world</b></p>",
			},
			{
				name: "heading followed by paragraph with italic and link",
				in:   "# Heading\nParagraph with *italic* and [link](https://example.com)",
				want: "<b>Heading</b>\n<p>Paragraph with <i>italic</i> and <a href=\"https://example.com\">link</a></p>",
			},
			{
				name: "nested unordered list preserves bullet layout without p tags",
				in:   "- item 1\n- item 2\n  - nested",
				want: "• item 1\n• item 2\n  ◦ nested\n",
			},
			{
				name: "blockquote without unwanted p wrapping",
				in:   "> a blockquote with `code`",
				want: "<blockquote>a blockquote with <code>code</code></blockquote>\n",
			},
			{
				name: "fenced code block preserves raw newlines without br or p tags",
				in:   "```go\nfunc main() {}\n```",
				want: "<pre><code class=\"language-go\">func main() {}\n</code></pre>\n",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := renderRichHTML(tc.in, "raw")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Errorf("got:\n  %q\nwant:\n  %q", got, tc.want)
				}
			})
		}
	})

	t.Run("rich contract verifies soft and hard line breaks", func(t *testing.T) {
		t.Run("soft line break produces br", func(t *testing.T) {
			in := "Soft\nBreak\nTest"
			got, err := renderRichHTML(in, "raw")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := "<p>Soft<br>Break<br>Test</p>"
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})

		t.Run("hard line break with trailing spaces produces br", func(t *testing.T) {
			in := "Hard  \nBreak  \nSpaces"
			got, err := renderRichHTML(in, "raw")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := "<p>Hard<br>Break<br>Spaces</p>"
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})

		t.Run("hard line break with trailing backslash produces br", func(t *testing.T) {
			in := "Backslash\\\nBreak"
			got, err := renderRichHTML(in, "raw")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := "<p>Backslash<br>Break</p>"
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})

		t.Run("multiple blank lines between paragraphs collapse into paragraph boundaries", func(t *testing.T) {
			in := "Para 1\n\n\n\nPara 2"
			got, err := renderRichHTML(in, "raw")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := "<p>Para 1</p><p>Para 2</p>"
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	})

	t.Run("rich contract verifies korean and emoji with formatting", func(t *testing.T) {
		in := "안녕하세요 **세계**\n🚀 로켓 발사!"
		got, err := renderRichHTML(in, "raw")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "<p>안녕하세요 <b>세계</b><br>🚀 로켓 발사!</p>"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("rich contract verifies special characters and literal escaping", func(t *testing.T) {
		in := "Values: x < 10 && y > 20 & z != 0\nSecond line: `foo < bar && baz > qux`"
		got, err := renderRichHTML(in, "raw")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "<p>Values: x &lt; 10 &amp;&amp; y &gt; 20 &amp; z != 0<br>Second line: <code>foo &lt; bar &amp;&amp; baz &gt; qux</code></p>"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("standard HTML convertMarkdownToTelegramHTML is completely unchanged", func(t *testing.T) {
		in := "Line 1\nLine 2\n\nLine 3"
		got := convertMarkdownToTelegramHTML(in)
		if strings.Contains(got, "<p>") || strings.Contains(got, "</p>") || strings.Contains(got, "<br>") {
			t.Errorf("standard HTML must NEVER contain <p> or <br> tags, got %q", got)
		}
		want := "Line 1\nLine 2\nLine 3"
		if got != want {
			t.Errorf("standard HTML got %q, want %q", got, want)
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

func TestClassifyRichError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantAction richErrorAction
		wantLatch  bool
	}{
		{
			name:       "nil error",
			err:        nil,
			wantAction: richActionNone,
			wantLatch:  false,
		},
		{
			name:       "definitive 400 bad request",
			err:        &tgbotapi.Error{Code: 400, Message: "Bad Request: can't parse rich_message"},
			wantAction: richActionFallback,
			wantLatch:  false,
		},
		{
			name:       "definitive 404 unknown method triggers latch",
			err:        &tgbotapi.Error{Code: 404, Message: "Not Found: method not found"},
			wantAction: richActionFallback,
			wantLatch:  true,
		},
		{
			name:       "definitive 404 chat not found does not trigger latch",
			err:        &tgbotapi.Error{Code: 404, Message: "Not Found: chat not found"},
			wantAction: richActionFallback,
			wantLatch:  false,
		},
		{
			name:       "rate limit 429 is delivery-uncertain (no fallback)",
			err:        &tgbotapi.Error{Code: 429, Message: "Too Many Requests: retry after 10"},
			wantAction: richActionNone,
			wantLatch:  false,
		},
		{
			name:       "server error 500 is delivery-uncertain (no fallback)",
			err:        &tgbotapi.Error{Code: 500, Message: "Internal Server Error"},
			wantAction: richActionNone,
			wantLatch:  false,
		},
		{
			name:       "server error 502 bad gateway is delivery-uncertain",
			err:        &tgbotapi.Error{Code: 502, Message: "Bad Gateway"},
			wantAction: richActionNone,
			wantLatch:  false,
		},
		{
			name:       "transport error timeout is delivery-uncertain",
			err:        errors.New("context deadline exceeded"),
			wantAction: richActionNone,
			wantLatch:  false,
		},
		{
			name:       "decode error is delivery-uncertain",
			err:        errors.New("failed to decode rich message response: unexpected EOF"),
			wantAction: richActionNone,
			wantLatch:  false,
		},
		{
			name:       "zero code API error is delivery-uncertain",
			err:        &tgbotapi.Error{Code: 0, Message: "unknown error"},
			wantAction: richActionNone,
			wantLatch:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &TelegramAdapter{
				chatID: 12345,
				richConfig: TelegramRichConfig{
					Enabled: true,
					ChatID:  12345,
				},
			}
			action := a.classifyRichError(tc.err)
			if action != tc.wantAction {
				t.Errorf("got action %v, want %v", action, tc.wantAction)
			}
			if a.isRichProcessDisabled() != tc.wantLatch {
				t.Errorf("got latch %v, want %v", a.isRichProcessDisabled(), tc.wantLatch)
			}
		})
	}
}

func TestTelegramAdapter_Send_RichIntegration(t *testing.T) {
	now := time.Now()
	aiOpt := SendOptions{
		AttachAfter:      now,
		Plain:            false,
		ReplyToMessageID: 101,
	}

	t.Run("eligible AI text uses one Rich request and does not call sendOneFn", func(t *testing.T) {
		var richCalled int
		var sendOneCalled int

		adapter := &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				if endpoint == "sendRichMessage" {
					richCalled++
					return &tgbotapi.APIResponse{
						Ok:     true,
						Result: []byte(`{"message_id": 999, "chat": {"id": 12345}}`),
					}, nil
				}
				return &tgbotapi.APIResponse{Ok: false}, errors.New("unexpected endpoint")
			},
			sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
				sendOneCalled++
				return nil
			},
			collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable {
				return nil
			},
		}

		err := adapter.Send("12345", "Here is \\(x = 1\\) math.", aiOpt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if richCalled != 1 {
			t.Errorf("expected 1 rich request, got %d", richCalled)
		}
		if sendOneCalled != 0 {
			t.Errorf("expected sendOneFn NOT to be called, got %d", sendOneCalled)
		}
	})

	t.Run("commands with zero AttachAfter use existing path", func(t *testing.T) {
		var richCalled int
		var sendOneCalled int

		adapter := &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				richCalled++
				return &tgbotapi.APIResponse{Ok: true, Result: []byte(`{"message_id": 999}`)}, nil
			},
			sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
				sendOneCalled++
				return nil
			},
		}

		cmdOpt := SendOptions{
			AttachAfter: time.Time{}, // zero AttachAfter
			Plain:       false,
		}
		err := adapter.Send("12345", "Command reply \\(x = 1\\)", cmdOpt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if richCalled != 0 {
			t.Errorf("expected 0 rich requests for command reply, got %d", richCalled)
		}
		if sendOneCalled == 0 {
			t.Errorf("expected sendOneFn to be called for command reply")
		}
	})

	t.Run("Plain:true bypasses Rich", func(t *testing.T) {
		var richCalled int
		var sendOneCalled int

		adapter := &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				richCalled++
				return &tgbotapi.APIResponse{Ok: true}, nil
			},
			sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
				sendOneCalled++
				return nil
			},
		}

		plainOpt := aiOpt
		plainOpt.Plain = true
		adapter.collectDeliverablesFn = func(after time.Time, exclude exclusionSet) []deliverable { return nil }
		err := adapter.Send("12345", "Plain text \\(x = 1\\)", plainOpt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if richCalled != 0 {
			t.Errorf("expected 0 rich requests when Plain:true, got %d", richCalled)
		}
		if sendOneCalled == 0 {
			t.Errorf("expected sendOneFn to be called when Plain:true")
		}
	})

	t.Run("over-limit text uses the existing HTML/plain chunk path", func(t *testing.T) {
		var richCalled int
		var sendOneCalled int

		adapter := &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				richCalled++
				return &tgbotapi.APIResponse{Ok: true}, nil
			},
			sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
				sendOneCalled++
				return nil
			},
			collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable { return nil },
		}

		longText := strings.Repeat("a", 4500)
		err := adapter.Send("12345", longText, aiOpt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if richCalled != 0 {
			t.Errorf("expected 0 rich requests for over-limit text, got %d", richCalled)
		}
		if sendOneCalled < 2 {
			t.Errorf("expected multiple chunks via sendOneFn, got %d", sendOneCalled)
		}
	})

	t.Run("definitive Rich 4xx invokes the existing HTML/plain path once", func(t *testing.T) {
		var richCalled int
		var sendOneCalled int

		adapter := &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				richCalled++
				return &tgbotapi.APIResponse{
					Ok:          false,
					ErrorCode:   400,
					Description: "Bad Request: can't parse rich_message",
				}, &tgbotapi.Error{Code: 400, Message: "Bad Request: can't parse rich_message"}
			},
			sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
				sendOneCalled++
				return nil
			},
			collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable { return nil },
		}

		err := adapter.Send("12345", "Here is \\(x = 1\\) math.", aiOpt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if richCalled != 1 {
			t.Errorf("expected 1 rich attempt, got %d", richCalled)
		}
		if sendOneCalled != 1 {
			t.Errorf("expected 1 fallback sendOne call, got %d", sendOneCalled)
		}
	})

	t.Run("uncertain Rich errors return without a second text send", func(t *testing.T) {
		var richCalled int
		var sendOneCalled int

		adapter := &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				richCalled++
				return nil, errors.New("connection reset by peer")
			},
			sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
				sendOneCalled++
				return nil
			},
			collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable { return nil },
		}

		err := adapter.Send("12345", "Here is \\(x = 1\\) math.", aiOpt)
		if err == nil {
			t.Fatal("expected error for uncertain transport failure, got nil")
		}
		if richCalled != 1 {
			t.Errorf("expected 1 rich attempt, got %d", richCalled)
		}
		if sendOneCalled != 0 {
			t.Errorf("expected sendOne NOT to be called on delivery-uncertain error, got %d", sendOneCalled)
		}
	})

	t.Run("attachments still run after a successful Rich text send and are not changed", func(t *testing.T) {
		var richCalled int
		var attachmentSent []string

		adapter := &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				richCalled++
				return &tgbotapi.APIResponse{
					Ok:     true,
					Result: []byte(`{"message_id": 999, "chat": {"id": 12345}}`),
				}, nil
			},
			sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
				return nil
			},
			sendAttachmentFn: func(chatID int64, path string, replyToID int) error {
				attachmentSent = append(attachmentSent, path)
				return nil
			},
			collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable {
				return []deliverable{
					{path: "/tmp/chart.png", deletable: false},
				}
			},
		}

		err := adapter.Send("12345", "Here is \\(x = 1\\) with chart.", aiOpt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if richCalled != 1 {
			t.Errorf("expected 1 rich request, got %d", richCalled)
		}
		if len(attachmentSent) != 1 || attachmentSent[0] != "/tmp/chart.png" {
			t.Errorf("expected attachment to be sent, got %v", attachmentSent)
		}
	})

	t.Run("rich wire payload contains Rich HTML with p and br tags", func(t *testing.T) {
		var capturedHTML string
		adapter := &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				if endpoint == "sendRichMessage" {
					var decoded struct {
						HTML *string `json:"html"`
					}
					if err := json.Unmarshal([]byte(params["rich_message"]), &decoded); err == nil && decoded.HTML != nil {
						capturedHTML = *decoded.HTML
					}
					return &tgbotapi.APIResponse{
						Ok:     true,
						Result: []byte(`{"message_id": 999, "chat": {"id": 12345}}`),
					}, nil
				}
				return &tgbotapi.APIResponse{Ok: false}, errors.New("unexpected endpoint")
			},
			collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable { return nil },
		}

		err := adapter.Send("12345", "Line 1\nLine 2\n\nLine 3", aiOpt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "<p>Line 1<br>Line 2</p><p>Line 3</p>"
		if capturedHTML != want {
			t.Errorf("captured rich_message.html = %q, want %q", capturedHTML, want)
		}
	})

	t.Run("definitive 400 fallback sends standard HTML without p and br tags", func(t *testing.T) {
		var fallbackText string
		var fallbackParseMode string
		adapter := &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				return &tgbotapi.APIResponse{
					Ok:          false,
					ErrorCode:   400,
					Description: "Bad Request: can't parse rich_message",
				}, &tgbotapi.Error{Code: 400, Message: "Bad Request: can't parse rich_message"}
			},
			sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
				fallbackText = text
				fallbackParseMode = parseMode
				return nil
			},
			collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable { return nil },
		}

		err := adapter.Send("12345", "Line 1\nLine 2\n\nLine 3", aiOpt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if fallbackParseMode != tgbotapi.ModeHTML {
			t.Errorf("expected fallback parseMode HTML, got %q", fallbackParseMode)
		}
		if strings.Contains(fallbackText, "<p>") || strings.Contains(fallbackText, "<br>") {
			t.Errorf("fallback message must NOT contain rich tags (<p>, <br>), got %q", fallbackText)
		}
		want := "Line 1\nLine 2\nLine 3"
		if fallbackText != want {
			t.Errorf("fallback message got %q, want %q", fallbackText, want)
		}
	})

	t.Run("tag expansion crossing 4000 limit triggers fallback chunk path", func(t *testing.T) {
		var richCalled int
		var sendOneCalled int
		adapter := &TelegramAdapter{
			chatID: 12345,
			richConfig: TelegramRichConfig{
				Enabled:    true,
				ChatID:     12345,
				MathEscape: "raw",
			},
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				richCalled++
				return &tgbotapi.APIResponse{Ok: true}, nil
			},
			sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
				sendOneCalled++
				return nil
			},
			collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable { return nil },
		}

		// 3995 'a's: raw text is <= 4000, but wrapped in <p>...</p> (7 extra bytes) it becomes 4002 > 4000.
		text3995 := strings.Repeat("a", 3995)
		err := adapter.Send("12345", text3995, aiOpt)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if richCalled != 0 {
			t.Errorf("expected Rich to be bypassed when rendered HTML exceeds limit, got %d calls", richCalled)
		}
		if sendOneCalled == 0 {
			t.Errorf("expected standard sendOne path to be called")
		}
	})
}

func TestRichWire_Regression(t *testing.T) {
	t.Run("reply_parameters omitted when replyToID is zero", func(t *testing.T) {
		var capturedParams tgbotapi.Params
		adapter := &TelegramAdapter{
			chatID: 12345,
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				capturedParams = params
				return &tgbotapi.APIResponse{
					Ok:     true,
					Result: []byte(`{"message_id": 100, "chat": {"id": 12345}}`),
				}, nil
			},
		}

		html := "Test"
		_, err := adapter.sendRichMessage(12345, &inputRichMessage{HTML: &html}, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, exists := capturedParams["reply_parameters"]; exists {
			t.Errorf("reply_parameters must not be present when replyToID is 0")
		}
		if _, exists := capturedParams["reply_markup"]; exists {
			t.Errorf("reply_markup must never be present in AI Rich message")
		}
	})

	t.Run("non-BMP text in HTML and formula preserved cleanly", func(t *testing.T) {
		var capturedParams tgbotapi.Params
		adapter := &TelegramAdapter{
			chatID: 12345,
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				capturedParams = params
				return &tgbotapi.APIResponse{
					Ok:     true,
					Result: []byte(`{"message_id": 101, "chat": {"id": 12345}}`),
				}, nil
			},
		}

		html := "Rocket 🚀 <tg-math>\\mathbb{F}_q</tg-math>"
		res, err := adapter.sendRichMessage(12345, &inputRichMessage{HTML: &html}, 50)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.MessageID != 101 {
			t.Errorf("expected message_id 101, got %d", res.MessageID)
		}

		rawRich := capturedParams["rich_message"]
		var decoded struct {
			HTML *string `json:"html"`
		}
		if err := json.Unmarshal([]byte(rawRich), &decoded); err != nil {
			t.Fatalf("failed to decode JSON: %v", err)
		}
		if decoded.HTML == nil || *decoded.HTML != html {
			t.Errorf("expected HTML %q, got %+v", html, decoded.HTML)
		}
	})

	t.Run("carrier is strictly HTML only", func(t *testing.T) {
		html := "Simple"
		msg := &inputRichMessage{HTML: &html}
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		raw := string(b)
		if !strings.Contains(raw, `"html"`) {
			t.Errorf("expected 'html' in %s", raw)
		}
		if strings.Contains(raw, "markdown") || strings.Contains(raw, "blocks") {
			t.Errorf("markdown and blocks must be omitted, got %s", raw)
		}
	})

	t.Run("reply_parameters populated when replyToID is non-zero", func(t *testing.T) {
		var capturedParams tgbotapi.Params
		adapter := &TelegramAdapter{
			chatID: 12345,
			makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
				capturedParams = params
				return &tgbotapi.APIResponse{
					Ok:     true,
					Result: []byte(`{"message_id": 105, "chat": {"id": 12345}}`),
				}, nil
			},
		}

		html := "Test"
		_, err := adapter.sendRichMessage(12345, &inputRichMessage{HTML: &html}, 42)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		rawReply, exists := capturedParams["reply_parameters"]
		if !exists {
			t.Fatalf("reply_parameters must exist when replyToID is non-zero")
		}
		var rp replyParameters
		if err := json.Unmarshal([]byte(rawReply), &rp); err != nil {
			t.Fatalf("failed to parse reply_parameters: %v", err)
		}
		if rp.MessageID != 42 {
			t.Errorf("expected message_id 42, got %d", rp.MessageID)
		}
	})
}

func TestRichCorpus_Regression(t *testing.T) {
	t.Run("formulas with underscore and ampersand in numeric profile", func(t *testing.T) {
		in := "Check matrix: \\[A \\& B_i < C_j > D_k & E\\]"
		got, err := renderRichHTML(in, "numeric")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "<tg-math-block>A \\&#38; B_i &#60; C_j &#62; D_k &#38; E</tg-math-block>"
		if !strings.Contains(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("currency with bare dollars beside formulas", func(t *testing.T) {
		in := "Price is $50. Formula: \\(x = 50\\) and $10 discount."
		got, err := renderRichHTML(in, "raw")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "$50") || !strings.Contains(got, "$10") {
			t.Errorf("bare currency lost in output: %q", got)
		}
		if !strings.Contains(got, "<tg-math>x = 50</tg-math>") {
			t.Errorf("formula missing in output: %q", got)
		}
	})

	t.Run("formulas in nested list and blockquote contexts", func(t *testing.T) {
		in := "> > Nested quote with \\(x = 1\\)\n- item 1\n  - subitem with \\[y = 2\\]"
		got, err := renderRichHTML(in, "raw")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(got, "<tg-math>x = 1</tg-math>") {
			t.Errorf("inline formula missing in quote: %q", got)
		}
		if !strings.Contains(got, "<tg-math-block>y = 2</tg-math-block>") {
			t.Errorf("block formula missing in subitem: %q", got)
		}
	})

	t.Run("code blocks containing math delimiters remain literal", func(t *testing.T) {
		in := "```\n\\(not math\\)\n\\[not block\\]\n$$not math$$\n```\n`\\(inline code\\)`"
		got, err := renderRichHTML(in, "raw")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(got, "<tg-math") {
			t.Errorf("code blocks must never contain tg-math tags: %q", got)
		}
	})

	t.Run("unbalanced delimiters with complex markdown", func(t *testing.T) {
		in := "**bold** \\(unbalanced text `code \\(not math\\)`"
		got, err := renderRichHTML(in, "raw")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(got, "<tg-math>") {
			t.Errorf("unbalanced text must not generate <tg-math>: %q", got)
		}
		if !strings.Contains(got, "<b>bold</b>") {
			t.Errorf("markdown formatting lost: %q", got)
		}
	})
}
