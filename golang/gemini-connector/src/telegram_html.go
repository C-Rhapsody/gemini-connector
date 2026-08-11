package main

import (
	"bytes"
	"html"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
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
		goldmark.WithRenderer(renderer.NewRenderer(
			renderer.WithNodeRenderers(
				util.Prioritized(&telegramHTMLRenderer{}, 1000),
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
