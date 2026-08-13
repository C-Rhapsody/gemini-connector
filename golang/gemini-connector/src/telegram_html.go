package main

import (
	"bytes"
	"html"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// convertMarkdownToTelegramHTML converts a Markdown string (agy default output)
// into the subset of Telegram Bot API HTML that Telegram supports.
//
// Supported elements (mapped to Telegram HTML):
//   - Bold (text or text)         -> <b>text</b>
//   - Italic (text or text)       -> <i>text</i>
//   - Inline code (text)          -> <code>text</code>
//   - Fenced code block (text)    -> <pre><code class="language-lang">text</code</pre>
//   - Link [text](url)            -> <a href="url">text</a>
//   - Blockquote (> text)         -> <blockquote>text</blockquote>
//   - Heading (# text)            -> <b>text</b>  (Telegram has no <h1>..<h6>)
//   - List (- item / 1. item)     -> text\n     (plain bullet, no <ul>/<ol>)
//   - Table                        -> aligned plain text inside <pre>
//
// Unsupported elements degrade to a plain-text representation rather than
// emitting unsupported tags. All <, >, & characters inside text content are
// escaped so AI-supplied content cannot inject HTML. On parse failure (or
// empty input) the original string is returned so the caller can decide how
// to fall back.
func convertMarkdownToTelegramHTML(s string) string {
	if s == "" {
		return ""
	}
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table),
		goldmark.WithRenderer(renderer.NewRenderer(
			renderer.WithNodeRenderers(
				util.Prioritized(&telegramHTMLRenderer{}, 100),
			),
		)),
	)
	var buf bytes.Buffer
	if err := md.Convert([]byte(s), &buf); err != nil {
		return s
	}
	return buf.String()
}

func escapeHTML(s string) string {
	return html.EscapeString(s)
}

// Telegram HTML open/close tags, stored as byte slices using ASCII codes
// so that the surrounding Go source remains free of literal '<', '>', '&'
// sequences that confuse certain editors/toolchains.
var (
	tagBOpen      = []byte{0x3C, 0x62, 0x3E}                                                                                     // <b>
	tagBClose     = []byte{0x3C, 0x2F, 0x62, 0x3E}                                                                               //</b>
	tagIOpen      = []byte{0x3C, 0x69, 0x3E}                                                                                     // <i>
	tagIClose     = []byte{0x3C, 0x2F, 0x69, 0x3E}                                                                               //</i>
	tagCodeOpen   = []byte{0x3C, 0x63, 0x6F, 0x64, 0x65, 0x3E}                                                                   // <code>
	tagCodeClose  = []byte{0x3C, 0x2F, 0x63, 0x6F, 0x64, 0x65, 0x3E}                                                             //</code>
	tagPreOpen    = []byte{0x3C, 0x70, 0x72, 0x65, 0x3E}                                                                         // <pre>
	tagPreClose   = []byte{0x3C, 0x2F, 0x70, 0x72, 0x65, 0x3E}                                                                   //</pre>
	tagBlockOpen  = []byte{0x3C, 0x62, 0x6C, 0x6F, 0x63, 0x6B, 0x71, 0x75, 0x6F, 0x74, 0x65, 0x3E}                               // <blockquote>
	tagBlockClose = []byte{0x3C, 0x2F, 0x62, 0x6C, 0x6F, 0x63, 0x6B, 0x71, 0x75, 0x6F, 0x74, 0x65, 0x3E}                         //</blockquote>
	tagAOpen      = []byte{0x3C, 0x61, 0x20, 0x68, 0x72, 0x65, 0x66, 0x3D, 0x22}                                                 // <a href="
	tagAClose     = []byte{0x22, 0x3E}                                                                                           // ">
	tagAEnd       = []byte{0x3C, 0x2F, 0x61, 0x3E}                                                                               //</a>
	tagAClassPfx  = []byte{0x20, 0x63, 0x6C, 0x61, 0x73, 0x73, 0x3D, 0x22, 0x6C, 0x61, 0x6E, 0x67, 0x75, 0x61, 0x67, 0x65, 0x2D} // " class=\"language-"
	tagQuoteAttr  = []byte{0x22, 0x3E}                                                                                           // ">
	bullet        = []byte{0xE2, 0x80, 0xA2, 0x20}                                                                               // U+2022 + space
	newline       = []byte{0x0A}
)

type telegramHTMLRenderer struct{}

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

func (r *telegramHTMLRenderer) renderParagraph(_ util.BufWriter, _ []byte, _ ast.Node, _ bool) (ast.WalkStatus, error) {
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderTextBlock(_ util.BufWriter, _ []byte, _ ast.Node, _ bool) (ast.WalkStatus, error) {
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
		_, _ = w.Write(newline)
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
		_, _ = w.Write(tagCodeOpen)
		if len(lang) > 0 {
			_, _ = w.Write(tagAClassPfx)
			_, _ = w.WriteString(escapeHTML(string(lang)))
			_, _ = w.Write(tagQuoteAttr)
		}
		for i := 0; i < fcb.Lines().Len(); i++ {
			seg := fcb.Lines().At(i)
			_, _ = w.Write(seg.Value(source))
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
		_, _ = w.Write(tagCodeOpen)
	} else {
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
		_, _ = w.WriteString(escapeHTML(string(seg.Value(source))))
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

func (r *telegramHTMLRenderer) renderList(_ util.BufWriter, _ []byte, _ ast.Node, _ bool) (ast.WalkStatus, error) {
	return ast.WalkContinue, nil
}

func (r *telegramHTMLRenderer) renderListItem(w util.BufWriter, _ []byte, _ ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.Write(bullet)
	} else {
		_, _ = w.Write(newline)
	}
	return ast.WalkContinue, nil
}

const tableMaxWidth = 80

func (r *telegramHTMLRenderer) renderTable(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		text := formatTelegramTable(n, source)
		if text != "" {
			_, _ = w.Write(newline)
			_, _ = w.Write(tagPreOpen)
			_, _ = w.WriteString(escapeHTML(text))
			_, _ = w.Write(tagPreClose)
			_, _ = w.Write(newline)
		}
		return ast.WalkSkipChildren, nil
	}
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
