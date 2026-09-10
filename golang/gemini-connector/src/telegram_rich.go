package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// telegramMakeRequest abstracts tgbotapi.BotAPI.MakeRequest for testability.
type telegramMakeRequest func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error)

// inputRichMessage models the Telegram Bot API InputRichMessage type.
// v1 sets only HTML.
type inputRichMessage struct {
	HTML *string `json:"html,omitempty"`
}

// replyParameters models the Telegram Bot API ReplyParameters type for sendRichMessage.
type replyParameters struct {
	MessageID int `json:"message_id"`
}

// richMessageResult models the minimal fields decoded from a successful Rich message response.
type richMessageResult struct {
	MessageID int `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

// sendRichMessage calls the Telegram Bot API "sendRichMessage" endpoint via the makeRequestFn seam.
func (t *TelegramAdapter) sendRichMessage(chatID int64, richMsg *inputRichMessage, replyToID int) (*richMessageResult, error) {
	if richMsg == nil || richMsg.HTML == nil {
		return nil, errors.New("rich_message cannot be nil")
	}
	if t.makeRequestFn == nil {
		return nil, errors.New("makeRequestFn not initialized")
	}

	params := make(tgbotapi.Params)
	params.AddNonZero64("chat_id", chatID)
	if err := params.AddInterface("rich_message", richMsg); err != nil {
		return nil, fmt.Errorf("failed to encode rich_message: %w", err)
	}
	if replyToID != 0 {
		replyParams := replyParameters{MessageID: replyToID}
		if err := params.AddInterface("reply_parameters", replyParams); err != nil {
			return nil, fmt.Errorf("failed to encode reply_parameters: %w", err)
		}
	}

	resp, err := t.makeRequestFn("sendRichMessage", params)
	if err != nil {
		return nil, err
	}
	if !resp.Ok {
		return nil, fmt.Errorf("telegram API error: %s", resp.Description)
	}

	var res richMessageResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("failed to decode rich message response: %w", err)
	}
	return &res, nil
}

// TelegramRichConfig defines the configuration for opt-in Telegram Rich Messages.
type TelegramRichConfig struct {
	Enabled    bool
	MathEscape string
	ChatID     int64
}

// maxRichHTMLUTF16Units is the conservative prefilter threshold for post-restoration Rich HTML.
const maxRichHTMLUTF16Units = 4000

// utf16Units counts the number of UTF-16 code units in a string.
// BMP characters count as 1, supplementary characters (surrogate pairs) count as 2.
func utf16Units(s string) int {
	count := 0
	for _, r := range s {
		if r > 0xFFFF {
			count += 2
		} else {
			count++
		}
	}
	return count
}

// isRichHTMLWithinLimit checks if the post-restoration Rich HTML candidate is within the UTF-16 length limit.
func isRichHTMLWithinLimit(richHTML string) bool {
	return utf16Units(richHTML) <= maxRichHTMLUTF16Units
}

// isRichProcessDisabled returns true if the Rich feature has been disabled process-wide (e.g. 404 unknown method).
func (t *TelegramAdapter) isRichProcessDisabled() bool {
	return atomic.LoadUint32(&t.richDisabledLatch) == 1
}

// disableRichProcess disables Rich delivery process-wide.
func (t *TelegramAdapter) disableRichProcess() {
	atomic.StoreUint32(&t.richDisabledLatch, 1)
}

// richEligible checks whether an outbound message meets all criteria to attempt Rich delivery.
func (t *TelegramAdapter) richEligible(targetChatID int64, text string, opt SendOptions) bool {
	if !t.richConfig.Enabled {
		return false
	}
	if t.richConfig.ChatID == 0 {
		return false
	}
	if targetChatID != t.richConfig.ChatID {
		return false
	}
	if opt.Plain {
		return false
	}
	if opt.AttachAfter.IsZero() {
		return false
	}
	if strings.TrimSpace(text) == "" {
		return false
	}
	if t.isRichProcessDisabled() {
		return false
	}
	return true
}

// MathKind represents the classification of a LaTeX formula.
type MathKind int

const (
	MathInline MathKind = iota // \( ... \)
	MathBlock                  // \[ ... \] or $$ ... $$
)

// MathToken records an extracted math formula and its nonce placeholder.
type MathToken struct {
	Kind        MathKind
	Raw         string
	Formula     string
	Placeholder string
}

func generateNonce() string {
	var b [8]byte
	_, err := rand.Read(b[:])
	if err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// findClosingMathDelimiter scans s starting at start for closeDelim ("\\)", "\\]", or "$$").
// It ignores delimiters occurring inside inline code (`...`), raw HTML tags (<pre>, <code>),
// and for inline math ("\\)") it stops on blank lines (paragraph breaks).
func findClosingMathDelimiter(s string, start int, openDelim string, closeDelim string) int {
	n := len(s)
	p := start
	delimLen := len(closeDelim)

	for p < n {
		// 1. Paragraph break: inline math \(...\) cannot cross blank lines
		if openDelim == "\\(" && s[p] == '\n' {
			k := p + 1
			for k < n && (s[k] == ' ' || s[k] == '\t' || s[k] == '\r') {
				k++
			}
			if k < n && s[k] == '\n' {
				return -1
			}
		}

		// 2. Inline code: skip backtick runs
		if s[p] == '`' {
			runLen := 0
			for p+runLen < n && s[p+runLen] == '`' {
				runLen++
			}
			target := strings.Repeat("`", runLen)
			closeIdx := strings.Index(s[p+runLen:], target)
			if closeIdx != -1 {
				p = p + runLen + closeIdx + runLen
				continue
			}
			p += runLen
			continue
		}

		// 3. Raw HTML tags <pre> or <code>
		if s[p] == '<' && p+5 <= n {
			lower5 := strings.ToLower(s[p : p+5])
			if lower5 == "<pre>" || lower5 == "<pre " {
				closePos := strings.Index(strings.ToLower(s[p+5:]), "</pre>")
				if closePos != -1 {
					p = p + 5 + closePos + 6
					continue
				}
			} else if lower5 == "<code>" || lower5 == "<code" {
				closePos := strings.Index(strings.ToLower(s[p+5:]), "</code>")
				if closePos != -1 {
					p = p + 5 + closePos + 7
					continue
				}
			}
		}

		// 4. Check for close delimiter
		if p+delimLen <= n && s[p:p+delimLen] == closeDelim {
			bs := 0
			q := p - 1
			for q >= start && s[q] == '\\' {
				bs++
				q--
			}
			if bs%2 == 0 {
				return p
			}
		}

		p++
	}

	return -1
}

// tokenizeLaTeX scans markdown text, preserves code blocks and bare dollars as literal,
// and extracts valid \(...\), \[...\], and $$...$$ formulas into MathTokens with nonce placeholders.
func tokenizeLaTeX(s string) (string, []MathToken) {
	nonce := generateNonce()
	var tokens []MathToken
	var sb strings.Builder

	n := len(s)
	i := 0
	atLineStart := true
	prevWasEmptyLine := true
	inIndentedCode := false

	for i < n {
		// 1. Check for fenced code block (``` or ~~~) at line start (up to 3 leading spaces)
		if atLineStart {
			leadSpaces := 0
			for i+leadSpaces < n && s[i+leadSpaces] == ' ' && leadSpaces < 3 {
				leadSpaces++
			}
			idx := i + leadSpaces
			if idx < n && (s[idx] == '`' || s[idx] == '~') {
				fenceChar := s[idx]
				fenceLen := 0
				for idx+fenceLen < n && s[idx+fenceLen] == fenceChar {
					fenceLen++
				}
				if fenceLen >= 3 {
					fenceEnd := idx + fenceLen
					lineEnd := strings.IndexByte(s[fenceEnd:], '\n')
					var contentStart int
					if lineEnd == -1 {
						sb.WriteString(s[i:])
						break
					}
					contentStart = fenceEnd + lineEnd + 1
					closeIdx := -1
					searchPos := contentStart
					for searchPos < n {
						lSpaces := 0
						for searchPos+lSpaces < n && s[searchPos+lSpaces] == ' ' && lSpaces < 3 {
							lSpaces++
						}
						cPos := searchPos + lSpaces
						if cPos < n && s[cPos] == fenceChar {
							cLen := 0
							for cPos+cLen < n && s[cPos+cLen] == fenceChar {
								cLen++
							}
							if cLen >= fenceLen {
								nextNL := strings.IndexByte(s[cPos+cLen:], '\n')
								if nextNL == -1 {
									closeIdx = n
								} else {
									closeIdx = cPos + cLen + nextNL + 1
								}
								break
							}
						}
						nextNL := strings.IndexByte(s[searchPos:], '\n')
						if nextNL == -1 {
							break
						}
						searchPos += nextNL + 1
					}
					if closeIdx != -1 {
						sb.WriteString(s[i:closeIdx])
						i = closeIdx
						atLineStart = true
						prevWasEmptyLine = false
						continue
					} else {
						sb.WriteString(s[i:])
						break
					}
				}
			}

			// 2. Check for indented code block (4 spaces or 1 tab) after empty line or continuing
			isIndented := false
			if prevWasEmptyLine || inIndentedCode {
				if i+4 <= n && s[i:i+4] == "    " {
					isIndented = true
				} else if i < n && s[i] == '\t' {
					isIndented = true
				}
			}
			if isIndented {
				inIndentedCode = true
				nextNL := strings.IndexByte(s[i:], '\n')
				if nextNL == -1 {
					sb.WriteString(s[i:])
					break
				}
				sb.WriteString(s[i : i+nextNL+1])
				i += nextNL + 1
				atLineStart = true
				prevWasEmptyLine = false
				continue
			}
			inIndentedCode = false
		}

		// 3. Check for inline code (` or `` etc.)
		if s[i] == '`' {
			runLen := 0
			for i+runLen < n && s[i+runLen] == '`' {
				runLen++
			}
			target := strings.Repeat("`", runLen)
			closePos := strings.Index(s[i+runLen:], target)
			if closePos != -1 {
				end := i + runLen + closePos + runLen
				sb.WriteString(s[i:end])
				i = end
				atLineStart = false
				prevWasEmptyLine = false
				continue
			}
		}

		// 4. Check for raw HTML tags <pre> or <code>
		if s[i] == '<' && i+5 <= n {
			lower5 := strings.ToLower(s[i : i+5])
			if lower5 == "<pre>" || lower5 == "<pre " {
				closePos := strings.Index(strings.ToLower(s[i+5:]), "</pre>")
				if closePos != -1 {
					end := i + 5 + closePos + 6
					sb.WriteString(s[i:end])
					i = end
					atLineStart = false
					prevWasEmptyLine = false
					continue
				}
			} else if lower5 == "<code>" || lower5 == "<code" {
				closePos := strings.Index(strings.ToLower(s[i+5:]), "</code>")
				if closePos != -1 {
					end := i + 5 + closePos + 7
					sb.WriteString(s[i:end])
					i = end
					atLineStart = false
					prevWasEmptyLine = false
					continue
				}
			}
		}

		// 5. Check for LaTeX math delimiters (not escaped by an odd number of backslashes)
		bsCount := 0
		k := i - 1
		for k >= 0 && s[k] == '\\' {
			bsCount++
			k--
		}
		isEscaped := (bsCount % 2) != 0

		if !isEscaped {
			if i+2 <= n && s[i:i+2] == "$$" {
				closePos := findClosingMathDelimiter(s, i+2, "$$", "$$")
				if closePos != -1 {
					raw := s[i : closePos+2]
					formula := s[i+2 : closePos]
					placeholder := fmt.Sprintf("TGMATH%sN%dX", nonce, len(tokens))
					tokens = append(tokens, MathToken{
						Kind:        MathBlock,
						Raw:         raw,
						Formula:     formula,
						Placeholder: placeholder,
					})
					sb.WriteString(placeholder)
					i += len(raw)
					atLineStart = false
					prevWasEmptyLine = false
					continue
				}
			} else if i+2 <= n && s[i:i+2] == "\\(" {
				closePos := findClosingMathDelimiter(s, i+2, "\\(", "\\)")
				if closePos != -1 {
					raw := s[i : closePos+2]
					formula := s[i+2 : closePos]
					placeholder := fmt.Sprintf("TGMATH%sN%dX", nonce, len(tokens))
					tokens = append(tokens, MathToken{
						Kind:        MathInline,
						Raw:         raw,
						Formula:     formula,
						Placeholder: placeholder,
					})
					sb.WriteString(placeholder)
					i += len(raw)
					atLineStart = false
					prevWasEmptyLine = false
					continue
				}
			} else if i+2 <= n && s[i:i+2] == "\\[" {
				closePos := findClosingMathDelimiter(s, i+2, "\\[", "\\]")
				if closePos != -1 {
					raw := s[i : closePos+2]
					formula := s[i+2 : closePos]
					placeholder := fmt.Sprintf("TGMATH%sN%dX", nonce, len(tokens))
					tokens = append(tokens, MathToken{
						Kind:        MathBlock,
						Raw:         raw,
						Formula:     formula,
						Placeholder: placeholder,
					})
					sb.WriteString(placeholder)
					i += len(raw)
					atLineStart = false
					prevWasEmptyLine = false
					continue
				}
			}
		}

		ch := s[i]
		sb.WriteByte(ch)
		if ch == '\n' {
			atLineStart = true
			if i > 0 && s[i-1] == '\n' {
				prevWasEmptyLine = true
			} else {
				prevWasEmptyLine = false
			}
		} else if ch != ' ' && ch != '\t' && ch != '\r' {
			atLineStart = false
			prevWasEmptyLine = false
		}
		i++
	}

	return sb.String(), tokens
}

// escapeFormula prepares formula content according to the configured math escape profile.
func escapeFormula(formula string, profile string) (string, error) {
	switch profile {
	case "raw":
		// Raw profile: preserve safe formula characters only.
		// If the formula contains HTML-significant characters (<, >, &), fail closed so the caller falls back.
		if strings.ContainsAny(formula, "<>&") {
			return "", fmt.Errorf("formula contains HTML-significant characters (<, >, &) in raw escape profile: %q", formula)
		}
		return formula, nil
	case "numeric":
		// Numeric profile: encode &, <, and > as numeric HTML entities.
		// Replace & first to avoid double-escaping.
		res := strings.ReplaceAll(formula, "&", "&#38;")
		res = strings.ReplaceAll(res, "<", "&#60;")
		res = strings.ReplaceAll(res, ">", "&#62;")
		return res, nil
	default:
		return "", fmt.Errorf("invalid math escape profile: %q", profile)
	}
}

// renderRichHTML tokenizes LaTeX math, renders the remaining markdown to Telegram HTML,
// verifies placeholder integrity, and restores math tokens as <tg-math> or <tg-math-block>.
func renderRichHTML(s string, mathEscape string) (string, error) {
	if s == "" {
		return "", nil
	}

	masked, tokens := tokenizeLaTeX(s)
	if len(tokens) == 0 {
		return convertMarkdownToTelegramHTML(s), nil
	}

	html := convertMarkdownToTelegramHTML(masked)

	// Verify all placeholders are present exactly once and restore them
	for _, tok := range tokens {
		count := strings.Count(html, tok.Placeholder)
		if count != 1 {
			return "", fmt.Errorf("placeholder %s integrity violation: expected exactly 1 occurrence, found %d", tok.Placeholder, count)
		}

		escapedBody, err := escapeFormula(tok.Formula, mathEscape)
		if err != nil {
			return "", err
		}

		var replacement string
		if tok.Kind == MathInline {
			replacement = "<tg-math>" + escapedBody + "</tg-math>"
		} else {
			replacement = "<tg-math-block>" + escapedBody + "</tg-math-block>"
		}

		html = strings.Replace(html, tok.Placeholder, replacement, 1)
	}

	return html, nil
}

// richErrorAction dictates the fallback behavior after a sendRichMessage attempt.
type richErrorAction int

const (
	// richActionNone means no alternate send is permitted (success, or delivery-uncertain errors like timeout/429/5xx).
	richActionNone richErrorAction = iota
	// richActionFallback indicates a definitive Telegram API 4xx rejection (excluding 429), allowing a single HTML/plain fallback.
	richActionFallback
)

// classifyRichError evaluates errors from sendRichMessage according to the strict fallback contract:
// - Definitive 4xx (except 429) allows fallback.
// - 404 with method-not-found description sets the process-wide disable latch.
// - Delivery-uncertain errors (transport, timeout, decode, 429, 5xx, zero-code) return richActionNone with NO alternate send.
func (t *TelegramAdapter) classifyRichError(err error) richErrorAction {
	if err == nil {
		return richActionNone
	}

	var apiErr *tgbotapi.Error
	if !errors.As(err, &apiErr) {
		// Transport error, timeout, decode failure, connection reset -> delivery uncertain
		return richActionNone
	}

	code := apiErr.Code
	if code == 0 {
		// Zero-code response is not definitive
		return richActionNone
	}

	// 429 Too Many Requests -> delivery uncertain, do not retry immediately with second format
	if code == 429 {
		return richActionNone
	}

	// 5xx Server Error -> delivery uncertain
	if code >= 500 {
		return richActionNone
	}

	// Check 404 for unknown method (e.g. "Not Found: method not found")
	if code == 404 {
		desc := strings.ToLower(apiErr.Message)
		if strings.Contains(desc, "method") {
			if !t.isRichProcessDisabled() {
				t.disableRichProcess()
				log.Printf("Telegram Rich Messages disabled process-wide: Telegram Bot API rejected method (404 %s)", apiErr.Message)
			}
		}
	}

	// Definitive 4xx rejection (400 <= code < 500, code != 429)
	if code >= 400 && code < 500 {
		return richActionFallback
	}

	return richActionNone
}
