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
			want:  preOpen + lt + "code" + " " + "class=\"language-go\"" + gt + "fmt.Println(" + quotE + "hi" + quotE + ")\n" + codeClose + preClose + "\n",
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
		{
			name:  "korean bold closed by particle after quote",
			input: "속성은 **\"정적 문자열\"**만 기록합니다.",
			want:  "속성은 " + bOpen + quotE + "정적 문자열" + quotE + bClose + "만 기록합니다.",
		},
		{
			name:  "korean bold closed by particle after parenthesis",
			input: "**pwsh.exe (PID: 43956)**가 멈춰 있습니다.",
			want:  bOpen + "pwsh.exe (PID: 43956)" + bClose + "가 멈춰 있습니다.",
		},
		{
			name:  "spaced bold",
			input: "이것은 ** 강조 ** 표시입니다.",
			want:  "이것은 " + bOpen + " 강조 " + bClose + " 표시입니다.",
		},
		{
			name:  "bold inside inline code is untouched",
			input: "`code ** not bold`",
			want:  codeOpen + "code ** not bold" + codeClose,
		},
		{
			name:  "unmatched double asterisks stay visible",
			input: "a ** b",
			want:  "a ** b",
		},
		{
			name:  "fenced code block escapes raw html",
			input: "```xml\n<activity android:name=\".Main\">\n```",
			want:  preOpen + lt + "code" + " " + "class=\"language-xml\"" + gt + ltE + "activity" + " android:name=" + quotE + ".Main" + quotE + gtE + "\n" + codeClose + preClose + "\n",
		},
		{
			name:  "paragraphs are separated by newline",
			input: "A\n\nB",
			want:  "A\nB",
		},
		{
			name:  "paragraph followed by list is separated",
			input: "요약:\n\n- 하나\n- 둘\n",
			want:  "요약:\n• 하나\n• 둘\n",
		},
		{
			name:  "nested unordered list uses hierarchical bullets",
			input: "- 부모 항목:\n  - 자식 하나\n  - 자식 둘\n- 다음 항목\n",
			want:  "• 부모 항목:\n  ◦ 자식 하나\n  ◦ 자식 둘\n• 다음 항목\n",
		},
		{
			name:  "ordered list keeps numbers",
			input: "1. 첫째\n2. 둘째\n",
			want:  "1. 첫째\n2. 둘째\n",
		},
		{
			name:  "ordered list honors start number",
			input: "3. 셋\n4. 넷\n",
			want:  "3. 셋\n4. 넷\n",
		},
		{
			name:  "nested ordered list indents and numbers",
			input: "1. 첫째\n   1. 중첩\n2. 둘째\n",
			want:  "1. 첫째\n  1. 중첩\n2. 둘째\n",
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
		preOpen + lt + "code" + " " + "class=\"language-python\"" + gt,
		blockquoteOpen,
		"\u2022 item one",
		"\u2022 item two",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("expected output to contain %q\n  got: %q", frag, got)
		}
	}
}

func TestConvertMarkdownToTelegramHTML_Table(t *testing.T) {
	input := "| 구분 | AUTOMATIC1111 (WebUI) | ComfyUI |\n" +
		"| :--- | :--- | :--- |\n" +
		"| 인터페이스 | 슬라이더와 버튼 중심의 슬롯형 메뉴 | 노드를 선으로 연결하는 흐름도 방식 |\n" +
		"| 난이도 | 직관적이고 입문자에게 쉬움 | 노드 구조 이해가 필요하여 초기 학습 곡선 있음 |"

	got := convertMarkdownToTelegramHTML(input)
	if !strings.HasPrefix(strings.TrimPrefix(got, "\n"), preOpen) || !strings.HasSuffix(got, preClose+"\n") {
		t.Fatalf("table should be rendered as preformatted text, got: %q", got)
	}
	if strings.Contains(got, "<table>") || strings.Contains(got, "<td>") {
		t.Fatalf("Telegram-incompatible table tags should not be emitted: %q", got)
	}
	for _, value := range []string{"구분", "AUTOMATIC1111 (WebUI)", "ComfyUI", "슬라이더와 버튼 중심의", "슬롯형 메뉴", "노드 구조 이해가 필요하여 초기 학습", "곡선 있음"} {
		if !strings.Contains(got, value) {
			t.Errorf("table output should contain %q: %q", value, got)
		}
	}
	if !strings.Contains(got, " | ") {
		t.Errorf("table columns should remain visibly separated: %q", got)
	}
}

func TestConvertMarkdownToTelegramHTML_TablePlainCellsAndEmptyCells(t *testing.T) {
	input := "| Name | Details | Empty |\n" +
		"| --- | --- | --- |\n" +
		"| **bold** | `code` and [link](https://example.com) | |"

	got := convertMarkdownToTelegramHTML(input)
	if strings.Contains(got, bOpen) || strings.Contains(got, codeOpen) || strings.Contains(got, aOpenHref) {
		t.Fatalf("table cell markup should be flattened to plain text: %q", got)
	}
	for _, value := range []string{"bold", "code", "link", "Empty"} {
		if !strings.Contains(got, value) {
			t.Errorf("table output should contain %q: %q", value, got)
		}
	}
}

func TestConvertMarkdownToTelegramHTML_TableWrapsLongCells(t *testing.T) {
	input := "| Key | Description |\n" +
		"| --- | --- |\n" +
		"| long | " + strings.Repeat("word ", 30) + "|"

	got := convertMarkdownToTelegramHTML(input)
	body := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(got, "\n"), preOpen), preClose+"\n")
	for _, line := range strings.Split(body, "\n") {
		if displayWidth(line) > tableMaxWidth {
			t.Errorf("table line exceeds configured width: %d > %d: %q", displayWidth(line), tableMaxWidth, line)
		}
	}
}

func TestConvertMarkdownToTelegramHTML_TableWithoutHeaderRemainsText(t *testing.T) {
	input := "| value one | value two |\n| value three | value four |"
	got := convertMarkdownToTelegramHTML(input)
	if strings.Contains(got, preOpen) {
		t.Fatalf("a headerless table should not be synthesized as a formatted table: %q", got)
	}
	for _, value := range []string{"| value one | value two |", "| value three | value four |"} {
		if !strings.Contains(got, value) {
			t.Errorf("headerless table text should remain visible, missing %q in %q", value, got)
		}
	}
}

func TestStripMarkdownFormatting(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "bold markers removed",
			input: "**중요**한 내용",
			want:  "중요한 내용",
		},
		{
			name:  "inline code markers removed",
			input: "run `npm test` now",
			want:  "run npm test now",
		},
		{
			name:  "link keeps text only",
			input: "see [the docs](https://example.com) here",
			want:  "see the docs here",
		},
		{
			name:  "fenced block keeps content drops fences",
			input: "```go\nfmt.Println()\n```",
			want:  "fmt.Println()\n",
		},
		{
			name:  "heading marker removed",
			input: "# 제목\n본문",
			want:  "제목\n본문",
		},
		{
			name:  "snake_case identifiers preserved",
			input: "use my_file_path_v2 here",
			want:  "use my_file_path_v2 here",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripMarkdownFormatting(tt.input); got != tt.want {
				t.Errorf("stripMarkdownFormatting(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.want)
			}
		})
	}
}
