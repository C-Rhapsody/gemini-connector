package main

import (
	"strings"
	"testing"
)

// Helper: HTML entity constants built from ASCII codes so this test file
// can be written without literal '&' / '<' / '>' sequences that confuse
// certain editors.
const (
	amp             = "\x26" // &
	lt              = "\x3C" // <
	gt              = "\x3E" // >
	quot            = "\x22" // "
	semi            = ";"
	ltE             = amp + "lt" + semi
	gtE             = amp + "gt" + semi
	ampE            = amp + "amp" + semi
	quotE           = amp + "#34" + semi
	bOpen           = lt + "b" + gt
	bClose          = lt + "/" + "b" + gt
	iOpen           = lt + "i" + gt
	iClose          = lt + "/" + "i" + gt
	codeOpen        = lt + "code" + gt
	codeClose       = lt + "/" + "code" + gt
	preOpen         = lt + "pre" + gt
	preClose        = lt + "/" + "pre" + gt
	blockquoteOpen  = lt + "blockquote" + gt
	blockquoteClose = lt + "/" + "blockquote" + gt
	aOpenHref       = lt + "a" + " " + "href=" + quot
	aMid            = quot + gt
	aClose          = lt + "/" + "a" + gt
)

func TestConvertMarkdownToTelegramHTML(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain text no markup",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "bold with double asterisks",
			input: "this is **bold** text",
			want:  "this is " + bOpen + "bold" + bClose + " text",
		},
		{
			name:  "bold with double underscores",
			input: "this is __bold__ text",
			want:  "this is " + bOpen + "bold" + bClose + " text",
		},
		{
			name:  "italic with single asterisk",
			input: "this is *italic* text",
			want:  "this is " + iOpen + "italic" + iClose + " text",
		},
		{
			name:  "italic with single underscore",
			input: "this is _italic_ text",
			want:  "this is " + iOpen + "italic" + iClose + " text",
		},
		{
			name:  "italic then bold nesting (goldmark)",
			input: "***bold-italic***",
			want:  iOpen + bOpen + "bold-italic" + bClose + iClose,
		},
		{
			name:  "italic inside bold",
			input: "**bold _italic_ bold**",
			want:  bOpen + "bold " + iOpen + "italic" + iClose + " bold" + bClose,
		},
		{
			name:  "inline code",
			input: "use `fmt.Println` here",
			want:  "use " + codeOpen + "fmt.Println" + codeClose + " here",
		},
		{
			name:  "fenced code block with language",
			input: "```go\nfmt.Println(\"hi\")\n```",
			want:  preOpen + codeOpen + " class=\"language-go\">fmt.Println(\"hi\")\n" + codeClose + preClose + "\n",
		},
		{
			name:  "fenced code block without language",
			input: "```\nraw text\n```",
			want:  preOpen + codeOpen + "raw text\n" + codeClose + preClose + "\n",
		},
		{
			name:  "link with simple url",
			input: "click [here](http://example.com) please",
			want:  "click " + aOpenHref + "http://example.com" + aMid + "here" + aClose + " please",
		},
		{
			name:  "link with ampersand in url is escaped",
			input: "[go](http://x.com?a=1&b=2)",
			want:  aOpenHref + "http://x.com?a=1" + ampE + "b=2" + aMid + "go" + aClose,
		},
		{
			name:  "link with double quote in url is escaped",
			input: "[bad](http://x.com\"y)",
			want:  aOpenHref + "http://x.com" + quotE + "y" + aMid + "bad" + aClose,
		},
		{
			name:  "blockquote single line",
			input: "> quoted text",
			want:  blockquoteOpen + "quoted text" + blockquoteClose + "\n",
		},
		{
			name:  "blockquote multiline preserves newline",
			input: "> line one\n> line two",
			want:  blockquoteOpen + "line one\nline two" + blockquoteClose + "\n",
		},
		{
			name:  "heading degrades to bold",
			input: "# My Heading",
			want:  bOpen + "My Heading" + bClose + "\n",
		},
		{
			name:  "unordered list degrades to bullets",
			input: "- item one\n- item two",
			want:  "\u2022 item one\n\u2022 item two\n",
		},
		{
			name:  "html special chars in text are escaped",
			input: "a < b > c & d",
			want:  "a " + ltE + " b " + gtE + " c " + ampE + " d",
		},
		{
			name:  "html in inline code is escaped",
			input: "`<b>not bold</b>`",
			want:  codeOpen + ltE + "b" + gtE + "not bold" + ltE + "/b" + gtE + codeClose,
		},
		{
			name:  "empty input returns empty",
			input: "",
			want:  "",
		},
		{
			name:  "image degrades to placeholder",
			input: "![alt text](http://x.com/i.png)",
			want:  "[image]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertMarkdownToTelegramHTML(tt.input)
			if got != tt.want {
				t.Errorf("convertMarkdownToTelegramHTML(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestConvertMarkdownToTelegramHTML_RealAgyResponse(t *testing.T) {
	input := "Here is a summary:\n\n" +
		"**Key point:** The system uses **Markdown** which has _italic_ and " + "`inline code`" + ".\n\n" +
		"For more details see [the docs](https://example.com/docs?q=test&v=2).\n\n" +
		"```python\n" +
		"def hello():\n" +
		"    print(\"hi\")\n" +
		"```" + "\n\n" +
		"> Note: this is important.\n\n" +
		"- item one\n- item two\n"

	got := convertMarkdownToTelegramHTML(input)
	t.Logf("Converted output:\n%s", got)

	for _, frag := range []string{
		bOpen + "Key point",
		bOpen + "Markdown" + bClose,
		iOpen + "italic" + iClose,
		codeOpen + "inline code" + codeClose,
		aOpenHref + "https://example.com/docs?q=test" + ampE + "v=2" + aMid + "the docs" + aClose,
		preOpen + codeOpen + " class=\"language-python\">",
		blockquoteOpen,
		"\u2022 item one",
		"\u2022 item two",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("expected output to contain %q\n  got: %q", frag, got)
		}
	}
}
