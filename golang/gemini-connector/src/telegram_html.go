package main

import (
	"bufio"
	"bytes"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// convertMarkdownToTelegramHTML converts a Markdown string (agy default output)
// into the subset of Telegram Bot API HTML that Telegram supports.
//
// Supported elements (mapped to Telegram HTML):
//   - Bold (text or text)         -> <b>text</b>
//   - Italic (text or text)       -> <i>text</i>
//   - Inline code (text)          -> <code>text</code>
//   - Fenced code block (text)    -> <pre><code class="language-lang">text</code></pre>
//   - Link [text](url)            -> <a href="url">text</a>
//   - Blockquote (> text)         -> <blockquote>text</blockquote>
//   - Heading (# text)            -> <b>text</b>  (Telegram has no <h1>..<h6>)
//   - List (- item / 1. item)     -> hierarchical bullets with indentation
//
// A custom bold parser relaxes CommonMark flanking rules so that Korean text
// like **"제목"**뒤에조사 or ** 강조 ** still renders as bold.
func convertMarkdownToTelegramHTML(s string) string {
	if s == "" {
		return ""
	}
	var buf bytes.Buffer
	lw := newLineTrackingWriter(&buf)
	r := &telegramHTMLRenderer{out: lw}
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table),
		goldmark.WithParserOptions(
			parser.WithInlineParsers(
				util.Prioritized(koreanBoldParser{}, 450),   // before the default emphasis parser (500)
				util.Prioritized(koreanItalicParser{}, 455), // ditto, for single-asterisk italics
			),
			parser.WithASTTransformers(
				util.Prioritized(delimiterTextTransformer{}, 100),
			),
		),
		goldmark.WithRenderer(renderer.NewRenderer(
			renderer.WithNodeRenderers(
				util.Prioritized(r, 100),
			),
		)),
	)
	if err := md.Convert([]byte(s), lw); err != nil {
		return stripMarkdownFormatting(s)
	}
	return buf.String()
}

// lineTrackingWriter wraps a bufio.Writer and remembers the last byte written,
// so renderers can tell whether the output currently sits at a line start.
// It satisfies goldmark's util.BufWriter interface.
type lineTrackingWriter struct {
	*bufio.Writer
	last byte
	any  bool
}

func newLineTrackingWriter(w io.Writer) *lineTrackingWriter {
	if bw, ok := w.(*bufio.Writer); ok {
		return &lineTrackingWriter{Writer: bw}
	}
	return &lineTrackingWriter{Writer: bufio.NewWriter(w)}
}

func (w *lineTrackingWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.last = p[n-1]
		w.any = true
	}
	return n, err
}

func (w *lineTrackingWriter) WriteString(s string) (int, error) {
	n, err := w.Writer.WriteString(s)
	if n > 0 {
		w.last = s[n-1]
		w.any = true
	}
	return n, err
}

func (w *lineTrackingWriter) WriteByte(c byte) error {
	if err := w.Writer.WriteByte(c); err != nil {
		return err
	}
	w.last = c
	w.any = true
	return nil
}

func (w *lineTrackingWriter) WriteRune(r rune) (int, error) {
	return w.WriteString(string(r))
}

func (w *lineTrackingWriter) atLineStart() bool {
	return !w.any || w.last == '\n'
}

func escapeHTML(s string) string {
	return html.EscapeString(s)
}

// Telegram HTML open/close tags, stored as byte slices using ASCII codes
// so that the surrounding Go source remains free of literal '<', '>', '&'
// sequences that confuse certain editors/toolchains.
var (
	tagBOpen      = []byte{0x3C, 0x62, 0x3E}                                                             // <b>
	tagBClose     = []byte{0x3C, 0x2F, 0x62, 0x3E}                                                       //</b>
	tagIOpen      = []byte{0x3C, 0x69, 0x3E}                                                             // <i>
	tagIClose     = []byte{0x3C, 0x2F, 0x69, 0x3E}                                                       //</i>
	tagCodeOpen   = []byte{0x3C, 0x63, 0x6F, 0x64, 0x65, 0x3E}                                           // <code>
	tagCodeBare   = []byte{0x3C, 0x63, 0x6F, 0x64, 0x65}                                                 // <code (no closing bracket)
	tagCodeClose  = []byte{0x3C, 0x2F, 0x63, 0x6F, 0x64, 0x65, 0x3E}                                     //</code>
	tagPreOpen    = []byte{0x3C, 0x70, 0x72, 0x65, 0x3E}                                                 // <pre>
	tagPreClose   = []byte{0x3C, 0x2F, 0x70, 0x72, 0x65, 0x3E}                                           //</pre>
	tagBlockOpen  = []byte{0x3C, 0x62, 0x6C, 0x6F, 0x63, 0x6B, 0x71, 0x75, 0x6F, 0x74, 0x65, 0x3E}       // <blockquote>
	tagBlockClose = []byte{0x3C, 0x2F, 0x62, 0x6C, 0x6F, 0x63, 0x6B, 0x71, 0x75, 0x6F, 0x74, 0x65, 0x3E} //</blockquote>
	tagAOpen      = []byte{0x3C, 0x61, 0x20, 0x68, 0x72, 0x65, 0x66, 0x3D, 0x22}                         // <a href="
	tagAClose     = []byte{0x22, 0x3E}                                                                   // ">
	tagAEnd       = []byte{0x3C, 0x2F, 0x61, 0x3E}                                                       //</a>
	tagQuoteAttr  = []byte{0x22, 0x3E}                                                                   // ">
	tagGT         = []byte{0x3E}                                                                         // >
	newline       = []byte{0x0A}
)

// Hierarchical bullet markers for nested unordered lists, by depth level.
const (
	bulletLevel0 = "• "
	bulletLevel1 = "◦ "
	bulletLevel2 = "▪ "
	listIndent   = "  "
)

type telegramHTMLRenderer struct {
	// out tracks the rendered output so renderers can avoid duplicated or
	// missing line breaks around lists and paragraphs.
	out *lineTrackingWriter
	// listCounters tracks the next number to print for each ordered list.
	listCounters map[ast.Node]int
	// inCodeSpan counts nesting inside inline code spans, whose literal
	// content must be exempt from prose normalizations (LaTeX arrows etc.).
	inCodeSpan int
}

// ensureLineStart writes a newline unless the output already sits at one.
func (r *telegramHTMLRenderer) ensureLineStart(w util.BufWriter) {
	if !r.out.atLineStart() {
		_, _ = w.Write(newline)
	}
}

// closeLine terminates the current line unless it is already closed.
func (r *telegramHTMLRenderer) closeLine(w util.BufWriter) {
	if !r.out.atLineStart() {
		_, _ = w.Write(newline)
	}
}

func (r *telegramHTMLRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindDocument, r.renderDocument)
	reg.Register(ast.KindParagraph, r.renderParagraph)
	reg.Register(ast.KindHeading, r.renderHeading)
	reg.Register(ast.KindThematicBreak, r.renderThematicBreak)
	reg.Register(ast.KindCodeBlock, r.renderCodeBlock)
	reg.Register(ast.KindFencedCodeBlock, r.renderFencedCodeBlock)
	reg.Register(ast.KindCodeSpan, r.renderCodeSpan)
	reg.Register(ast.KindEmphasis, r.renderEmphasis)
	reg.Register(ast.KindLink, r.renderLink)
	reg.Register(ast.KindAutoLink, r.renderAutoLink)
	reg.Register(ast.KindImage, r.renderImage)
	reg.Register(ast.KindText, r.renderText)
	reg.Register(ast.KindRawHTML, r.renderRawHTML)
	reg.Register(ast.KindString, r.renderString)
	reg.Register(ast.KindBlockquote, r.renderBlockquote)
	reg.Register(ast.KindList, r.renderList)
	reg.Register(ast.KindListItem, r.renderListItem)
	reg.Register(ast.KindTextBlock, r.renderTextBlock)
	reg.Register(extast.KindTable, r.renderTable)
}

func (r *telegramHTMLRenderer) renderDocument(_ util.BufWriter, _ []byte, _ ast.Node, _ bool) (ast.WalkStatus, error) {
	return ast.WalkContinue, nil
}

// needsSeparator reports whether a newline must be written after the node
// that just finished, so the next sibling block does not merge into it.
// ThematicBreak and Table already emit their own leading newline.
func needsSeparator(n ast.Node) bool {
	next := n.NextSibling()
	if next == nil {
		return false
	}
	switch next.Kind() {
	case ast.KindThematicBreak, extast.KindTable:
		return false
	}
	return true
}

// separateBlocks closes the current line when another block follows, so the
// next sibling does not merge into the block that just finished.
func (r *telegramHTMLRenderer) separateBlocks(w util.BufWriter, n ast.Node) {
	if needsSeparator(n) {
		r.closeLine(w)
	}
}

func (r *telegramHTMLRenderer) renderParagraph(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		r.separateBlocks(w, n)
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderTextBlock(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		r.separateBlocks(w, n)
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderHeading(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.Write(tagBOpen)
	} else {
		_, _ = w.Write(tagBClose)
		_, _ = w.Write(newline)
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderThematicBreak(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		r.ensureLineStart(w)
		_, _ = w.WriteString("---\n")
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderCodeBlock(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.Write(tagPreOpen)
	} else {
		_, _ = w.Write(tagPreClose)
		_, _ = w.Write(newline)
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderFencedCodeBlock(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	fcb := n.(*ast.FencedCodeBlock)
	if entering {
		lang := fcb.Language(source)
		_, _ = w.Write(tagPreOpen)
		_, _ = w.Write(tagCodeBare)
		if len(lang) > 0 {
			_, _ = w.WriteString(" class=\"language-")
			_, _ = w.WriteString(escapeHTML(string(lang)))
			_, _ = w.WriteString("\"")
		}
		_, _ = w.Write(tagGT)
		for i := 0; i < fcb.Lines().Len(); i++ {
			seg := fcb.Lines().At(i)
			// Code content is escaped so raw <, >, & inside code (a common
			// source of Telegram "unsupported start tag" failures) cannot
			// break the HTML payload.
			_, _ = w.WriteString(escapeHTML(string(seg.Value(source))))
		}
	} else {
		_, _ = w.Write(tagCodeClose)
		_, _ = w.Write(tagPreClose)
		_, _ = w.Write(newline)
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderCodeSpan(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		r.inCodeSpan++
		_, _ = w.Write(tagCodeOpen)
	} else {
		r.inCodeSpan--
		_, _ = w.Write(tagCodeClose)
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderEmphasis(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	level := n.(*ast.Emphasis).Level
	if entering {
		if level == 1 {
			_, _ = w.Write(tagIOpen)
		} else {
			_, _ = w.Write(tagBOpen)
		}
	} else {
		if level == 1 {
			_, _ = w.Write(tagIClose)
		} else {
			_, _ = w.Write(tagBClose)
		}
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderLink(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		url := string(n.(*ast.Link).Destination)
		_, _ = w.Write(tagAOpen)
		_, _ = w.WriteString(escapeHTML(url))
		_, _ = w.Write(tagAClose)
	} else {
		_, _ = w.Write(tagAEnd)
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderAutoLink(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		url := string(n.(*ast.AutoLink).URL(source))
		label := string(n.(*ast.AutoLink).Label(source))
		_, _ = w.Write(tagAOpen)
		_, _ = w.WriteString(escapeHTML(url))
		_, _ = w.Write(tagAClose)
		_, _ = w.WriteString(escapeHTML(label))
		_, _ = w.Write(tagAEnd)
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderImage(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.WriteString("[image]")
	}
	return ast.WalkSkipChildren, nil
}

func (r *telegramHTMLRenderer) renderText(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		textNode := n.(*ast.Text)
		seg := textNode.Segment
		// Inline content is always escaped to keep user/AI-provided text from
		// injecting HTML. Raw text (e.g. inside code spans) is handled by the
		// parent fenced-code-block renderer; here we always escape.
		value := string(seg.Value(source))
		if r.inCodeSpan == 0 {
			// Prose-only normalization: agy occasionally leaks LaTeX arrows
			// ($\rightarrow$ etc.) into answers; render them as Unicode.
			value = normalizeLatexArrows(value)
		}
		_, _ = w.WriteString(escapeHTML(value))
		if textNode.HardLineBreak() {
			_, _ = w.WriteString("\n")
		} else if textNode.SoftLineBreak() {
			_, _ = w.WriteString("\n")
		}
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderRawHTML(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		for c := n.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.String); ok {
				_, _ = w.WriteString(escapeHTML(string(t.Value)))
			}
		}
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderString(_ util.BufWriter, _ []byte, _ ast.Node, _ bool) (ast.WalkStatus, error) {
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderBlockquote(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.Write(tagBlockOpen)
	} else {
		_, _ = w.Write(tagBlockClose)
		_, _ = w.Write(newline)
	}
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderList(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	list := n.(*ast.List)
	if entering {
		if list.IsOrdered() {
			if r.listCounters == nil {
				r.listCounters = make(map[ast.Node]int)
			}
			start := list.Start
			if start < 1 {
				start = 1
			}
			r.listCounters[n] = start
		}
		return ast.WalkContinue, nil
	}
	r.separateBlocks(w, n)
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderListItem(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		r.closeLine(w)
		return ast.WalkContinue, nil
	}
	r.ensureLineStart(w)

	level := 0
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Kind() == ast.KindList {
			level++
		}
	}
	for i := 1; i < level; i++ {
		_, _ = w.WriteString(listIndent)
	}
	list := n.Parent().(*ast.List)
	switch {
	case list.IsOrdered():
		num := r.listCounters[list]
		r.listCounters[list] = num + 1
		_, _ = w.WriteString(fmt.Sprintf("%d. ", num))
	case level <= 1:
		_, _ = w.WriteString(bulletLevel0)
	case level == 2:
		_, _ = w.WriteString(bulletLevel1)
	default:
		_, _ = w.WriteString(bulletLevel2)
	}
	return ast.WalkContinue, nil
}

// renderDelimiter emits leftover (unmatched) emphasis markers as literal text
// instead of dropping them silently.

// delimiterTextTransformer converts leftover (unmatched) emphasis delimiter
// nodes into plain text nodes so their markers stay visible in the output.
// goldmark's Delimiter node kind is unexported, so it cannot be given a
// renderer function directly.
type delimiterTextTransformer struct{}

func (delimiterTextTransformer) Transform(node *ast.Document, _ text.Reader, _ parser.Context) {
	replaceDelimitersWithText(node)
}

func replaceDelimitersWithText(n ast.Node) {
	for c := n.FirstChild(); c != nil; {
		next := c.NextSibling()
		if d, ok := c.(*parser.Delimiter); ok {
			n.InsertBefore(n, d, ast.NewTextSegment(d.Segment))
			n.RemoveChild(n, d)
		} else {
			replaceDelimitersWithText(c)
		}
		c = next
	}
}

// koreanBoldDelimiterProcessor matches '*' runs and produces level-2
// (bold) Emphasis nodes.
type koreanBoldDelimiterProcessor struct{}

func (koreanBoldDelimiterProcessor) IsDelimiter(b byte) bool { return b == '*' }

func (koreanBoldDelimiterProcessor) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	return opener.Char == closer.Char
}

func (koreanBoldDelimiterProcessor) OnMatch(consumes int) ast.Node {
	return ast.NewEmphasis(consumes)
} // koreanBoldParser handles exactly-two-asterisk (**bold**) runs with relaxed
// flanking rules. CommonMark rejects closers that follow punctuation when a
// letter comes next, which is common in Korean text:
//
//	**"정적 문자열"**만   **pwsh.exe (PID: 1)**가   ** 강조 **
//
// Single asterisks and triple asterisks fall through to goldmark's default
// emphasis parser. Code spans and code blocks are parsed before this parser
// runs, so '**' inside code is never touched.
type koreanBoldParser struct{}

func (koreanBoldParser) Trigger() []byte { return []byte{'*'} }

func (koreanBoldParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()
	line, segment := block.PeekLine()
	d := parser.ScanDelimiter(line, before, 2, koreanBoldDelimiterProcessor{})
	if d == nil || d.OriginalLength != 2 {
		return nil
	}
	d.CanOpen = true
	d.CanClose = true
	d.Segment = segment.WithStop(segment.Start + d.OriginalLength)
	block.Advance(d.OriginalLength)
	pc.PushDelimiter(d)
	return d
}

// koreanItalicDelimiterProcessor matches single '*' runs and produces
// level-1 (italic) Emphasis nodes.
type koreanItalicDelimiterProcessor struct{}

func (koreanItalicDelimiterProcessor) IsDelimiter(b byte) bool { return b == '*' }

func (koreanItalicDelimiterProcessor) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	return opener.Char == closer.Char
}

func (koreanItalicDelimiterProcessor) OnMatch(consumes int) ast.Node {
	return ast.NewEmphasis(consumes)
}

// koreanItalicParser handles exactly-one-asterisk (*italic*) runs with the
// same relaxed Korean flanking as koreanBoldParser. CommonMark rejects the
// closer in shapes like *"문자열"*도 (punctuation before, letter after),
// which left literal asterisks in Telegram output. Spaced arithmetic such as
// "2 * 3 * 4" stays literal because neither side gains flanking there.
// Doubles and triples fall through to koreanBoldParser and goldmark's
// default emphasis parser.
type koreanItalicParser struct{}

func (koreanItalicParser) Trigger() []byte { return []byte{'*'} }

func (koreanItalicParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()
	line, segment := block.PeekLine()
	d := parser.ScanDelimiter(line, before, 1, koreanItalicDelimiterProcessor{})
	if d == nil || d.OriginalLength != 1 {
		return nil
	}
	relaxSingleStarFlanking(d, before, line)
	d.Segment = segment.WithStop(segment.Start + d.OriginalLength)
	block.Advance(d.OriginalLength)
	pc.PushDelimiter(d)
	return d
}

// relaxSingleStarFlanking keeps CommonMark flanking except for the two
// Korean-prose shapes strict rules reject:
//   - opener between a letter and punctuation: 단어*"…"
//   - closer between punctuation and a letter: *"…"*도
func relaxSingleStarFlanking(d *parser.Delimiter, before rune, line []byte) {
	after := rune(' ')
	if len(line) > 1 {
		r, _ := utf8.DecodeRune(line[1:])
		after = r
	}
	if util.IsPunctRune(before) && unicode.IsLetter(after) {
		d.CanClose = true
	}
	if unicode.IsLetter(before) && util.IsPunctRune(after) {
		d.CanOpen = true
	}
}

const tableMaxWidth = 80

func (r *telegramHTMLRenderer) renderTable(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		text := formatTelegramTable(n, source)
		if text != "" {
			r.ensureLineStart(w)
			_, _ = w.Write(tagPreOpen)
			_, _ = w.WriteString(escapeHTML(text))
			_, _ = w.Write(tagPreClose)
			_, _ = w.Write(newline)
		}
		return ast.WalkSkipChildren, nil
	}
	r.separateBlocks(w, n)
	return ast.WalkContinue, nil
}

func formatTelegramTable(table ast.Node, source []byte) string {
	rows := make([][]string, 0)
	columnCount := 0
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		if child.Kind() != extast.KindTableHeader && child.Kind() != extast.KindTableRow {
			continue
		}
		row := make([]string, 0)
		for cell := child.FirstChild(); cell != nil; cell = cell.NextSibling() {
			if cell.Kind() != extast.KindTableCell {
				continue
			}
			value := strings.TrimSpace(plainTableText(cell, source))
			row = append(row, value)
		}
		if len(row) > columnCount {
			columnCount = len(row)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 || columnCount == 0 {
		return ""
	}

	for i := range rows {
		for len(rows[i]) < columnCount {
			rows[i] = append(rows[i], "")
		}
	}

	naturalWidths := make([]int, columnCount)
	for _, row := range rows {
		for column, value := range row {
			width := maxTableLineWidth(value)
			if width < 1 {
				width = 1
			}
			if width > naturalWidths[column] {
				naturalWidths[column] = width
			}
		}
	}
	widths := allocateTableWidths(naturalWidths, tableMaxWidth)

	var out strings.Builder
	for _, row := range rows {
		wrapped := make([][]string, columnCount)
		lineCount := 1
		for column, value := range row {
			wrapped[column] = wrapTableCell(value, widths[column])
			if len(wrapped[column]) > lineCount {
				lineCount = len(wrapped[column])
			}
		}

		for line := 0; line < lineCount; line++ {
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			for column := 0; column < columnCount; column++ {
				if column > 0 {
					out.WriteString(" | ")
				}
				value := ""
				if line < len(wrapped[column]) {
					value = wrapped[column][line]
				}
				out.WriteString(value)
				for padding := displayWidth(value); padding < widths[column]; padding++ {
					out.WriteByte(' ')
				}
			}
		}
	}
	return out.String()
}

func plainTableText(node ast.Node, source []byte) string {
	var out strings.Builder
	var visit func(ast.Node)
	visit = func(current ast.Node) {
		switch value := current.(type) {
		case *ast.Text:
			out.Write(value.Segment.Value(source))
			if value.HardLineBreak() || value.SoftLineBreak() {
				out.WriteByte('\n')
			}
		case *ast.String:
			out.Write(value.Value)
		case *ast.AutoLink:
			out.Write(value.URL(source))
		default:
			for child := current.FirstChild(); child != nil; child = child.NextSibling() {
				visit(child)
			}
		}
	}
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		visit(child)
	}
	return out.String()
}

func allocateTableWidths(natural []int, maxWidth int) []int {
	widths := append([]int(nil), natural...)
	if len(widths) == 0 {
		return widths
	}
	available := maxWidth - 3*(len(widths)-1)
	if available < len(widths) {
		available = len(widths)
	}
	total := 0
	for _, width := range widths {
		total += width
	}
	if total <= available {
		return widths
	}

	minimumWidths := make([]int, len(widths))
	for i, width := range natural {
		minimumWidths[i] = width
		if minimumWidths[i] > 12 {
			minimumWidths[i] = 12
		}
		if minimumWidths[i] < 1 {
			minimumWidths[i] = 1
		}
	}
	total = 0
	for i, width := range widths {
		if width > minimumWidths[i] {
			widths[i] = minimumWidths[i]
		}
		total += widths[i]
	}
	if total > available {
		for i := range widths {
			widths[i] = 1
		}
		total = len(widths)
	}
	for total < available {
		best := -1
		for i := range widths {
			if widths[i] >= natural[i] {
				continue
			}
			if best == -1 || natural[i]-widths[i] > natural[best]-widths[best] {
				best = i
			}
		}
		if best == -1 {
			break
		}
		widths[best]++
		total++
	}
	return widths
}

func wrapTableCell(value string, width int) []string {
	value = strings.ReplaceAll(value, "\t", "    ")
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		wrapped = append(wrapped, wrapTableLine(line, width)...)
	}
	if len(wrapped) == 0 {
		return []string{""}
	}
	return wrapped
}

func wrapTableLine(line string, width int) []string {
	if line == "" {
		return []string{""}
	}
	runes := []rune(line)
	result := make([]string, 0, 1)
	for len(runes) > 0 {
		if displayWidth(string(runes)) <= width {
			result = append(result, string(runes))
			break
		}

		usedWidth := 0
		cut := 0
		lastSpace := -1
		for i, r := range runes {
			runeWidth := runeDisplayWidth(r)
			if cut > 0 && usedWidth+runeWidth > width {
				break
			}
			usedWidth += runeWidth
			cut = i + 1
			if unicode.IsSpace(r) && i > 0 {
				lastSpace = i
			}
		}
		if cut == 0 {
			cut = 1
		}
		if lastSpace > 0 && lastSpace < cut {
			result = append(result, strings.TrimRightFunc(string(runes[:lastSpace]), unicode.IsSpace))
			runes = runes[lastSpace+1:]
		} else {
			result = append(result, string(runes[:cut]))
			runes = runes[cut:]
		}
		runes = trimLeadingTableSpaces(runes)
	}
	return result
}

func trimLeadingTableSpaces(runes []rune) []rune {
	for len(runes) > 0 && unicode.IsSpace(runes[0]) {
		runes = runes[1:]
	}
	return runes
}

func maxTableLineWidth(value string) int {
	maximum := 0
	for _, line := range strings.Split(value, "\n") {
		if width := displayWidth(line); width > maximum {
			maximum = width
		}
	}
	return maximum
}

func displayWidth(value string) int {
	width := 0
	for _, r := range value {
		width += runeDisplayWidth(r)
	}
	return width
}

func runeDisplayWidth(r rune) int {
	if r == '\t' {
		return 4
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	if isWideTableRune(r) {
		return 2
	}
	return 1
}

func isWideTableRune(r rune) bool {
	return (r >= 0x1100 && r <= 0x115F) ||
		(r >= 0x2329 && r <= 0x232A) ||
		(r >= 0x2E80 && r <= 0xA4CF) ||
		(r >= 0xAC00 && r <= 0xD7A3) ||
		(r >= 0xF900 && r <= 0xFAFF) ||
		(r >= 0xFE10 && r <= 0xFE6F) ||
		(r >= 0xFF01 && r <= 0xFF60) ||
		(r >= 0xFFE0 && r <= 0xFFE6) ||
		(r >= 0x1F300 && r <= 0x1FAFF) ||
		(r >= 0x20000 && r <= 0x3FFFD)
}

// Regular expressions used to strip Markdown formatting when a message has
// to be delivered as plain text (e.g. the HTML fallback path).
var (
	fencedBlockRe    = regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\\n?(.*?)```")
	imageRe          = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	linkRe           = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	headingRe        = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	blockquoteRe     = regexp.MustCompile(`(?m)^\s*>\s?`)
	boldRe           = regexp.MustCompile(`\*\*([^*]+?)\*\*`)
	boldUnderscoreRe = regexp.MustCompile(`__([^_]+?)__`)
	italicStarRe     = regexp.MustCompile(`\*([^*\n]+)\*`)
	inlineCodeRe     = regexp.MustCompile("`([^`]+)`")
)

// stripMarkdownFormatting removes common Markdown markers from s so that a
// plain-text fallback does not show raw **, `, or link syntax to the user.
// Underscore italics are intentionally left alone to protect snake_case
// identifiers and file paths.
func stripMarkdownFormatting(s string) string {
	s = fencedBlockRe.ReplaceAllString(s, "$1")
	s = imageRe.ReplaceAllString(s, "")
	s = linkRe.ReplaceAllString(s, "$1")
	s = headingRe.ReplaceAllString(s, "")
	s = blockquoteRe.ReplaceAllString(s, "")
	s = boldRe.ReplaceAllString(s, "$1")
	s = boldUnderscoreRe.ReplaceAllString(s, "$1")
	s = italicStarRe.ReplaceAllString(s, "$1")
	s = inlineCodeRe.ReplaceAllString(s, "$1")
	return s
}

// --- LaTeX arrow normalization ---
//
// agy answers sometimes leak inline LaTeX arrow commands (often wrapped in
// math delimiters like $\rightarrow$). They are rendered as their Unicode
// glyphs in prose only; code spans and code blocks keep the literal source.

var latexArrowSymbols = map[string]string{
	"rightarrow":     "\u2192", // →
	"to":             "\u2192", // →
	"gets":           "\u2190", // ←
	"leftarrow":      "\u2190", // ←
	"leftrightarrow": "\u2194", // ↔
	"Rightarrow":     "\u21D2", // ⇒
	"Leftarrow":      "\u21D0", // ⇐
	"Leftrightarrow": "\u21D4", // ⇔
}

// latexArrowRe matches an optional pair of math delimiters around a known
// arrow command. Longer names come first so \leftarrow never shadows
// \Leftrightarrow-style prefixes.
var latexArrowRe = regexp.MustCompile(`\$?\\(Leftrightarrow|leftrightarrow|Rightarrow|Leftarrow|rightarrow|leftarrow|gets|to)\$?`)

func isASCIILetterByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// normalizeLatexArrows rewrites known LaTeX arrow commands into Unicode
// arrows. A command that continues into a longer word (e.g. the \tools of a
// Windows path) is left untouched so filesystem paths stay intact.
func normalizeLatexArrows(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	matches := latexArrowRe.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range matches {
		end := loc[1]
		if end < len(s) && isASCIILetterByte(s[end]) {
			continue // prefix of a longer identifier; keep literal
		}
		sym, ok := latexArrowSymbols[s[loc[2]:loc[3]]]
		if !ok || sym == "" {
			continue
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(sym)
		last = end
	}
	if last == 0 {
		return s // nothing actually replaced
	}
	b.WriteString(s[last:])
	return b.String()
}
