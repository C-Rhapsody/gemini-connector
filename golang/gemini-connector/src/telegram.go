package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	xproxy "golang.org/x/net/proxy"
)

type TelegramAdapter struct {
	bot         *tgbotapi.BotAPI
	token       string
	chatID      int64
	msgs        *Messages
	convID      func() string
	proxyURL    string
	albumBuffer map[string][]*tgbotapi.Message
	albumTimer  map[string]*time.Timer
	albumMutex  sync.Mutex
	msgChan     chan InboundEvent
}

func NewTelegramAdapter(token string, chatID int64, msgs *Messages, convID func() string, proxyURL string) *TelegramAdapter {
	return &TelegramAdapter{
		token:       token,
		chatID:      chatID,
		msgs:        msgs,
		convID:      convID,
		proxyURL:    strings.TrimSpace(proxyURL),
		albumBuffer: make(map[string][]*tgbotapi.Message),
		albumTimer:  make(map[string]*time.Timer),
		msgChan:     make(chan InboundEvent, 100),
	}
}

func (t *TelegramAdapter) Init() error {
	client, err := newTelegramHTTPClient(t.proxyURL)
	if err != nil {
		return fmt.Errorf("telegram proxy configuration: %w", err)
	}
	bot, err := tgbotapi.NewBotAPIWithClient(t.token, tgbotapi.APIEndpoint, client)
	if err != nil {
		// Telegram API errors embed the request URL, which contains the bot
		// token; scrub it before the message reaches bot.log.
		return fmt.Errorf("bot init error: %s", redactToken(err.Error(), t.token))
	}
	t.bot = bot
	log.Printf("Bot Authorized as: %s", bot.Self.UserName)

	commands := []tgbotapi.BotCommand{
		{Command: "start", Description: "커넥터 시작"},
		{Command: "help", Description: "도움말 및 명령어 목록"},
		{Command: "image", Description: "NIM으로 이미지 생성"},
		{Command: "new", Description: "새 agy 대화 세션 시작"},
		{Command: "reset", Description: "/new 의 별칭"},
		{Command: "clear", Description: "대화 기록을 지우고 새 세션 시작"},
		{Command: "stop", Description: "진행 중인 agy 작업 즉시 중지"},
		{Command: "cron", Description: "예약 작업 관리 (/cron help)"},
		{Command: "status", Description: "현재 대화 정보"},
		{Command: "summary", Description: "최근 대화 미리보기"},
		{Command: "list", Description: "캐시된 대화 목록"},
		{Command: "switch", Description: "대화 전환 (ID 필요)"},
		{Command: "version", Description: "버전 정보"},
	}
	if _, err := t.bot.Request(tgbotapi.NewSetMyCommands(commands...)); err != nil {
		log.Printf("Failed to register bot commands: %v", err)
	}
	return nil
}

// newTelegramHTTPClient creates the single HTTP client used by every Telegram
// API operation. A direct client explicitly disables environment proxy
// variables so an empty --telegram-proxy value has deterministic behavior.
func newTelegramHTTPClient(rawProxyURL string) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("unexpected default HTTP transport type")
	}
	transport := base.Clone()
	transport.Proxy = nil

	rawProxyURL = strings.TrimSpace(rawProxyURL)
	if rawProxyURL == "" {
		log.Println("Telegram proxy disabled; using direct connection")
		return &http.Client{Transport: transport}, nil
	}

	proxyURL, err := url.Parse(rawProxyURL)
	if err != nil {
		// Do not include the raw URL: it may contain proxy credentials.
		return nil, errors.New("invalid proxy URL")
	}
	proxyURL.Scheme = strings.ToLower(proxyURL.Scheme)
	if proxyURL.Host == "" {
		return nil, errors.New("proxy URL must include a host")
	}

	switch proxyURL.Scheme {
	case "http", "https":
		transport.Proxy = http.ProxyURL(proxyURL)
	case "socks5", "socks5h":
		dialer, err := xproxy.FromURL(proxyURL, xproxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("invalid SOCKS5 proxy: %w", err)
		}
		contextDialer, ok := dialer.(xproxy.ContextDialer)
		if !ok {
			return nil, errors.New("SOCKS5 proxy does not support context-aware dialing")
		}
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			return contextDialer.DialContext(ctx, network, address)
		}
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q; use http, https, socks5, or socks5h", proxyURL.Scheme)
	}

	log.Printf("Telegram proxy enabled: %s", proxyURL.Redacted())
	return &http.Client{Transport: transport}, nil
}

func (t *TelegramAdapter) Listen() (<-chan InboundEvent, error) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	u.AllowedUpdates = []string{"message", "callback_query"}
	updates := t.bot.GetUpdatesChan(u)

	go func() {
		for update := range updates {
			if update.CallbackQuery != nil {
				go t.handleCallbackQuery(update.CallbackQuery)
				continue
			}
			if update.Message == nil {
				continue
			}

			if t.chatID != 0 && update.Message.Chat.ID != t.chatID {
				log.Printf("Ignored unauthorized message from Chat ID: %d", update.Message.Chat.ID)
				continue
			}

			go t.handleIncomingMessage(update.Message)
		}
		close(t.msgChan)
	}()

	return t.msgChan, nil
}

// handleCallbackQuery forwards every callback press into the event pipeline
// as an interaction event. The adapter only enforces the chat allowlist and
// acknowledges presses nobody downstream claims (the central interaction
// router owns that decision).
func (t *TelegramAdapter) handleCallbackQuery(cq *tgbotapi.CallbackQuery) {
	if cq.Message == nil {
		t.AnswerCallbackQuery(cq.ID, "")
		return
	}
	if t.chatID != 0 && cq.Message.Chat.ID != t.chatID {
		log.Printf("Ignored unauthorized callback from Chat ID: %d", cq.Message.Chat.ID)
		t.AnswerCallbackQuery(cq.ID, "권한이 없습니다.")
		return
	}

	userID := ""
	if cq.From != nil {
		userID = strconv.FormatInt(cq.From.ID, 10)
	}
	t.msgChan <- InboundEvent{
		Platform:     "telegram",
		UserID:       userID,
		ChatID:       strconv.FormatInt(cq.Message.Chat.ID, 10),
		Kind:         EventInteraction,
		MessageID:    cq.Message.MessageID,
		CallbackID:   cq.ID,
		CallbackData: cq.Data,
	}
}

const maxAttachmentsPerMessage = 10

const maxAttachmentBytes = 50 << 20 // Telegram bot upload limit

// deliverableExtensions is the whitelist of file types eligible for channel
// delivery. Source-code and plain-text extensions are deliberately excluded
// so that filenames merely mentioned in AI answers are never picked up.
var deliverableExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true,
	".mp4": true, ".webm": true, ".mov": true, ".avi": true, ".mkv": true,
	".pdf": true, ".zip": true, ".csv": true, ".xlsx": true, ".docx": true, ".pptx": true,
}

func (t *TelegramAdapter) Send(chatID string, text string, opts ...SendOptions) error {
	var opt SendOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid chat ID: %s", chatID)
	}

	// When the payload may carry deliverables, scan for files produced by
	// the AI during this turn, strip their paths from the reply text, and
	// send them as channel attachments afterwards. Inbound media the user
	// just sent (and agy's temp copies of it) are excluded so the bot never
	// echoes the user's own image back.
	var attachments []deliverable
	if !opt.AttachAfter.IsZero() {
		attachments = t.collectDeliverables(opt.AttachAfter, newExclusionSet(opt.ExcludeAttachments))
		for _, m := range filePathPattern.FindAllString(text, -1) {
			text = strings.ReplaceAll(text, m, "")
		}
		text = strings.TrimSpace(text)
	}

	if text != "" {
		for _, chunk := range splitTelegramChunks(text, 4000) {
			if opt.Plain {
				if err := t.sendOne(id, chunk, "", opt.ReplyToMessageID); err != nil {
					log.Printf("Telegram plain send failed: %v", err)
					return err
				}
				continue
			}
			htmlBody := convertMarkdownToTelegramHTML(chunk)
			if err := t.sendOne(id, htmlBody, tgbotapi.ModeHTML, opt.ReplyToMessageID); err != nil {
				log.Printf("Telegram HTML send failed (%v), retrying as stripped plain text", err)
				// Send formatting-stripped text so raw Markdown markers
				// (**, `, links) are not exposed to the user.
				plain := stripMarkdownFormatting(chunk)
				if err2 := t.sendOne(id, plain, "", opt.ReplyToMessageID); err2 != nil {
					log.Printf("Telegram plain-text fallback also failed: %v", err2)
					return err2
				}
			}
		}
	}

	for _, a := range attachments {
		if err := t.sendAttachmentFile(id, a.path, opt.ReplyToMessageID); err != nil {
			log.Printf("Attachment send failed (%s): %v", a.path, err)
			return err
		}
		if a.deletable {
			if rmErr := os.Remove(a.path); rmErr != nil {
				log.Printf("Failed to remove delivered attachment %s: %v", a.path, rmErr)
			}
		}
	}
	return nil
}

func (t *TelegramAdapter) sendOne(chatID int64, text string, parseMode string, replyToID int) error {
	msg := tgbotapi.NewMessage(chatID, text)
	if parseMode != "" {
		msg.ParseMode = parseMode
	}
	if replyToID != 0 {
		msg.ReplyToMessageID = replyToID
	}
	_, err := t.bot.Send(msg)
	return err
}

// deliverable is a candidate file found by the attachment scan.
type deliverable struct {
	path string
	// deletable marks files inside the project tree, whose local copy may be
	// removed after delivery. Files from the agy brain directory are
	// preserved.
	deletable bool
}

// brainDir returns the active conversation's agy brain directory, where tools
// like generate_image store their output. Empty when unknown.
func (t *TelegramAdapter) brainDir() string {
	if t.convID == nil {
		return ""
	}
	id := strings.TrimSpace(t.convID())
	if id == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "brain", id)
}

// exclusionSet holds canonical (cleaned, lowercased) absolute paths that are
// barred from auto-attachment delivery.
type exclusionSet map[string]bool

// newExclusionSet canonicalizes raw paths; nil/empty input yields an empty set.
func newExclusionSet(paths []string) exclusionSet {
	if len(paths) == 0 {
		return nil
	}
	set := make(exclusionSet, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		set[normalizeDeliverablePath(p)] = true
	}
	return set
}

// normalizeDeliverablePath converts a path to its comparison form: absolute,
// cleaned and lowercased so Windows drive/case variance cannot dodge the match.
func normalizeDeliverablePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	return strings.ToLower(abs)
}

// isTempMediaStoragePath reports whether any path component marks agy's
// temporary inbound-media storage (.tempmediaStorage), which mirrors what the
// user just sent and must never be re-delivered as an AI attachment.
func isTempMediaStoragePath(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.EqualFold(part, ".tempmediaStorage") {
			return true
		}
	}
	return false
}

// collectDeliverables finds files produced during the current turn: files
// modified at or after `after` with whitelisted extensions, under the project
// tree (deletable) or under the active agy brain directory (preserved).
// Paths in exclude (inbound media) and anything inside agy's temp media
// storage are never eligible.
func (t *TelegramAdapter) collectDeliverables(after time.Time, exclude exclusionSet) []deliverable {
	root := findProjectRoot()
	if root == "" {
		return nil
	}
	seen := make(map[string]bool)
	var out []deliverable

	collect := func(dir string, deletable bool) {
		if dir == "" {
			return
		}
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // best-effort scan; skip unreadable entries
			}
			if d.IsDir() {
				if d.Name() == ".git" || d.Name() == "node_modules" || strings.EqualFold(d.Name(), ".tempmediaStorage") {
					return filepath.SkipDir
				}
				return nil
			}
			if seen[p] || !deliverableExtensions[strings.ToLower(filepath.Ext(d.Name()))] {
				return nil
			}
			info, err := d.Info()
			if err != nil || info.Size() > maxAttachmentBytes || info.ModTime().Before(after) {
				return nil
			}
			if exclude != nil && exclude[normalizeDeliverablePath(p)] {
				log.Printf("Attachment candidate skipped (inbound media): %s", p)
				return nil
			}
			seen[p] = true
			out = append(out, deliverable{path: p, deletable: deletable})
			return nil
		})
	}

	collect(root, true)
	collect(t.brainDir(), false)

	if len(out) > maxAttachmentsPerMessage {
		out = out[:maxAttachmentsPerMessage]
	}
	for _, a := range out {
		log.Printf("Attachment candidate: %s (deletable=%v)", a.path, a.deletable)
	}
	return out
}

// SendAttachment uploads a single local file, choosing the Telegram message
// type by extension (photo / video / document). It implements AttachmentSender.
func (t *TelegramAdapter) SendAttachment(chatID string, path string, replyTo int) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid chat ID: %s", chatID)
	}
	return t.sendAttachmentFile(id, path, replyTo)
}

func (t *TelegramAdapter) sendAttachmentFile(chatID int64, path string, replyToID int) error {
	var cfg tgbotapi.Chattable
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		pc := tgbotapi.NewPhoto(chatID, tgbotapi.FilePath(path))
		if replyToID != 0 {
			pc.ReplyToMessageID = replyToID
		}
		cfg = pc
	case ".mp4", ".webm", ".mov", ".avi", ".mkv":
		vc := tgbotapi.NewVideo(chatID, tgbotapi.FilePath(path))
		if replyToID != 0 {
			vc.ReplyToMessageID = replyToID
		}
		cfg = vc
	default:
		dc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(path))
		if replyToID != 0 {
			dc.ReplyToMessageID = replyToID
		}
		cfg = dc
	}
	if _, err := t.bot.Send(cfg); err != nil {
		return err
	}
	log.Printf("Attachment sent: %s", path)
	return nil
}

// telegramChunkMinTail is the smallest acceptable size of the final message
// produced by splitTelegramChunks. When a size-driven cut would strand a
// smaller remainder, the cut is moved back to the latest Markdown section
// boundary so the trailing message carries a coherent, reasonably sized
// section instead of an orphaned fragment.
const telegramChunkMinTail = 800

// splitTelegramChunks splits a message into chunks no longer than limit
// bytes. It breaks at line boundaries and keeps fenced code blocks intact:
// a block that fits into the remaining space stays beside the preceding
// text, a block that fits into one chunk but not into this one starts the
// next chunk whole, and only a block larger than the limit is split with its
// fence closed and re-opened (info string preserved). When the natural last
// chunk would be tiny, the cut moves back to the latest section boundary
// (heading or "---"). Multi-byte UTF-8 runes are never cut in half.
func splitTelegramChunks(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}

	var (
		chunks   []string
		buf      []byte
		inFence  bool   // fenced-code state of the source stream
		carry    string // info string of the fence spanning a forced split
		realBase int    // len(buf) excluding a synthetic re-opened fence
		marks    []int  // buf offsets of section-boundary lines outside fences
		pos      int    // current read offset into s
	)

	// overhead is the bytes occupied by a synthetic fence opener at the start
	// of buf after a mid-fence flush.
	overhead := func() int {
		if inFence {
			return len("```") + len(carry) + 1
		}
		return 0
	}
	// free reports how many source bytes still fit into buf. While inside a
	// fence, room for the synthetic closer is reserved.
	free := func() int {
		n := limit - len(buf)
		if inFence {
			n -= len("\n```\n")
		}
		return n
	}
	flush := func() {
		if len(buf) > realBase {
			if inFence && buf[len(buf)-1] != '\n' {
				buf = append(buf, '\n')
			}
			if inFence {
				buf = append(buf, "```\n"...)
			}
			chunks = append(chunks, string(buf))
		}
		buf = buf[:0]
		if inFence {
			buf = append(buf, "```"...)
			buf = append(buf, carry...)
			buf = append(buf, '\n')
		}
		realBase = len(buf)
		marks = marks[:0]
	}
	// relocate moves the imminent split point back to the latest section
	// boundary so that the trailing remainder reaches at least
	// telegramChunkMinTail bytes. It reports whether the stream was rewound.
	relocate := func() bool {
		rem := len(s) - pos
		if rem >= telegramChunkMinTail || len(marks) == 0 {
			return false
		}
		for i := len(marks) - 1; i >= 0; i-- {
			m := marks[i]
			if len(buf)-m+rem < telegramChunkMinTail {
				continue
			}
			head := make([]byte, m)
			copy(head, buf[:m])
			chunks = append(chunks, string(head))
			pending := make([]byte, 0, len(buf)-m+len(s)-pos)
			pending = append(pending, buf[m:]...)
			pending = append(pending, s[pos:]...)
			s = string(pending)
			pos = 0
			// Everything before m lies outside fences by construction, so
			// parsing restarts from a clean state.
			buf = buf[:0]
			inFence = false
			carry = ""
			realBase = 0
			marks = marks[:0]
			return true
		}
		return false
	}

	for pos < len(s) {
		line := s[pos:]
		if nl := strings.IndexByte(s[pos:], '\n'); nl >= 0 {
			line = s[pos : pos+nl+1]
		}

		if len(line) <= free() {
			// Fenced blocks get special treatment so they never dangle at a
			// chunk edge: keep them beside their context when possible.
			if !inFence {
				if _, ok := fenceInfoString(line); ok {
					blk := fencedBlockLen(s, pos)
					switch {
					case blk >= 0 && blk <= free():
						// Whole block fits into the remaining space: append
						// it atomically so it stays in one message.
						buf = append(buf, s[pos:pos+blk]...)
						pos += blk
						continue
					case blk >= 0 && blk <= limit && len(buf) > 0:
						// It fits in a dedicated chunk but not here: start
						// the next message with it intact, avoiding a tiny
						// tail by relocating the cut when possible.
						if !relocate() {
							flush()
						}
						continue
					}
					// blk > limit or unterminated: stream it line by line
					// below, splitting mid-fence when necessary.
				}
			}
			if !inFence && len(buf) > 0 && isSectionBoundary(line) {
				marks = append(marks, len(buf))
			}
			buf = append(buf, line...)
			pos += len(line)
			if inFence {
				if isFenceClose(line) {
					inFence = false
					carry = ""
				}
			} else if lang, ok := fenceInfoString(line); ok {
				inFence = true
				carry = lang
			}
			continue
		}

		// The line does not fit: prefer a semantic cut over a size-driven
		// one when the remainder would end up too small.
		if !relocate() {
			if len(buf) > overhead() {
				flush()
				continue
			}

			// Oversized single line with nothing usable buffered: take as
			// many whole runes as fit. The budget is tracked locally because
			// buf does not grow until after the loop.
			budget := free()
			take := 0
			for take < len(line) {
				_, sz := utf8.DecodeRuneInString(line[take:])
				if sz > budget {
					break
				}
				take += sz
				budget -= sz
			}
			if take == 0 {
				_, sz := utf8.DecodeRuneInString(line)
				take = sz // degenerate limit smaller than one rune: force progress
			}
			buf = append(buf, line[:take]...)
			pos += take
			flush()
		}
	}
	flush()
	return chunks
}

// fenceInfoString reports the info string when the line opens a fenced code
// block (three or more leading backticks outside any fence).
func fenceInfoString(line string) (string, bool) {
	t := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(t, "```") {
		return "", false
	}
	info := strings.TrimSpace(t[3:])
	if strings.Contains(info, "`") {
		return "", false
	}
	return info, true
}

// isFenceClose reports whether the line terminates an open fenced code block:
// only backticks (optionally indented), with no other content.
func isFenceClose(line string) bool {
	t := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(t, "```") {
		return false
	}
	return strings.TrimSpace(t[3:]) == ""
}

// isSectionBoundary reports whether the line starts a top-level Markdown
// section usable as a message-break point: an ATX heading or a thematic
// break. Fenced code content is excluded by the caller.
func isSectionBoundary(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "#") {
		return true
	}
	return t == "---"
}

// fencedBlockLen measures the byte length of the complete fenced code block
// whose opening line starts at off. It returns -1 when the block never closes.
func fencedBlockLen(s string, off int) int {
	end := off
	for first := true; end < len(s); {
		line := s[end:]
		if nl := strings.IndexByte(s[end:], '\n'); nl >= 0 {
			line = s[end : end+nl+1]
		}
		if !first && isFenceClose(line) {
			return end + len(line) - off
		}
		first = false
		end += len(line)
	}
	return -1
}

func (t *TelegramAdapter) StartTyping(chatID string) (stop func()) {
	id, _ := strconv.ParseInt(chatID, 10, 64)
	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		t.bot.Request(tgbotapi.NewChatAction(id, tgbotapi.ChatTyping))
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				t.bot.Request(tgbotapi.NewChatAction(id, tgbotapi.ChatTyping))
			}
		}
	}()

	return func() { close(done) }
}

func (t *TelegramAdapter) GetFile(fileID string) (string, error) {
	file, err := t.bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", err
	}

	fileURL := file.Link(t.bot.Token)
	exePath, _ := os.Executable()
	downloadsDir := filepath.Join(filepath.Dir(exePath), "..", "downloads")
	_ = os.MkdirAll(downloadsDir, 0755)
	destPath := filepath.Join(downloadsDir, filepath.Base(file.FilePath))

	_, dlErr := downloadFile(fileURL, destPath)
	if dlErr != nil {
		return "", dlErr
	}
	return destPath, nil
}

// --- Internal message routing ---

func (t *TelegramAdapter) handleIncomingMessage(msg *tgbotapi.Message) {
	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	userID := ""
	if msg.From != nil {
		userID = strconv.FormatInt(msg.From.ID, 10)
	}

	// The adapter identifies commands but never interprets them: every
	// slash command is forwarded to the central Controller, including
	// /start, /help and unknown ones.
	if msg.IsCommand() {
		t.msgChan <- InboundEvent{
			Platform:  "telegram",
			UserID:    userID,
			ChatID:    chatID,
			Kind:      EventCommand,
			Command:   "/" + msg.Command(),
			Args:      msg.CommandArguments(),
			MessageID: msg.MessageID,
		}
		return
	}

	if msg.Video != nil || msg.VideoNote != nil || msg.Document != nil || msg.Audio != nil || msg.Voice != nil {
		t.Send(chatID, t.msgs.ErrorMediaNotSupported, SendOptions{ReplyToMessageID: msg.MessageID})
		return
	}

	if msg.MediaGroupID != "" {
		t.albumMutex.Lock()
		t.albumBuffer[msg.MediaGroupID] = append(t.albumBuffer[msg.MediaGroupID], msg)

		if timer, exists := t.albumTimer[msg.MediaGroupID]; exists {
			timer.Stop()
		}

		t.albumTimer[msg.MediaGroupID] = time.AfterFunc(2*time.Second, func() {
			t.processAlbum(msg.MediaGroupID, msg.Chat.ID)
		})
		t.albumMutex.Unlock()
		return
	}

	t.processSingleMessage(msg)
}

func (t *TelegramAdapter) processSingleMessage(msg *tgbotapi.Message) {
	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	prompt := msg.Text
	if msg.Caption != "" {
		prompt = msg.Caption
	}

	var inboundPaths []string

	// Live locations keep arriving as edit updates of the same message;
	// process only the initial share and ignore subsequent updates.
	if msg.Location != nil && msg.Location.LivePeriod > 0 && msg.EditDate != 0 {
		return
	}

	// A standalone location message carries no text. Bake a self-describing
	// coordinate tag so agy can interpret it without extra rules.
	if prompt == "" && msg.Location != nil {
		prompt = fmt.Sprintf("[위치: 위도 %.6f, 경도 %.6f]", msg.Location.Latitude, msg.Location.Longitude)
	}

	if msg.Photo != nil {
		mediaPath := t.downloadMediaWithRetry(msg, msg.Chat.ID, 1)
		if mediaPath != "" {
			inboundPaths = append(inboundPaths, mediaPath)
			if prompt == "" {
				prompt = t.msgs.DefaultMediaPrompt
			}
			prompt = fmt.Sprintf("[첨부파일: %s] %s", mediaPath, prompt)
		} else {
			t.Send(chatID, t.msgs.ErrorMediaDownloadFail, SendOptions{ReplyToMessageID: msg.MessageID})
			return
		}
	}

	if prompt == "" {
		return
	}

	userID := ""
	if msg.From != nil {
		userID = strconv.FormatInt(msg.From.ID, 10)
	}

	content := prompt
	if quote, quoteRole := t.extractQuote(msg); quote != "" {
		role := quoteRole
		if role == "" {
			role = "user"
		}
		content = fmt.Sprintf("[인용된 이전 메시지 (%s)]\n%s\n\n---\n\n[새 메시지]\n%s", role, quote, prompt)
	}

	t.msgChan <- InboundEvent{
		Platform:        "telegram",
		UserID:          userID,
		ChatID:          chatID,
		Kind:            EventMessage,
		Content:         content,
		MessageID:       msg.MessageID,
		AttachmentPaths: inboundPaths,
	}
}

// extractQuote returns the text of the message this message replies to, plus
// the role of its author ("assistant" for the bot's own messages, otherwise
// "user"). Telegram provides at most one level of reply-to message.
func (t *TelegramAdapter) extractQuote(msg *tgbotapi.Message) (string, string) {
	if msg.ReplyToMessage == nil {
		return "", ""
	}
	rt := msg.ReplyToMessage
	text := rt.Text
	if text == "" {
		text = rt.Caption
	}
	if text == "" {
		return "", ""
	}
	role := "user"
	if rt.From != nil && t.bot != nil && rt.From.ID == t.bot.Self.ID {
		role = "assistant"
	}
	return text, role
}

func (t *TelegramAdapter) processAlbum(groupID string, chatID int64) {
	t.albumMutex.Lock()
	messages := t.albumBuffer[groupID]
	delete(t.albumBuffer, groupID)
	delete(t.albumTimer, groupID)
	t.albumMutex.Unlock()

	chatIDStr := strconv.FormatInt(chatID, 10)

	sort.Slice(messages, func(i, j int) bool {
		return messages[i].MessageID < messages[j].MessageID
	})

	var combinedPrompt strings.Builder
	captionText := ""
	var inboundPaths []string

	for i, msg := range messages {
		seqIndex := i + 1
		mediaPath := t.downloadMediaWithRetry(msg, chatID, seqIndex)
		if mediaPath != "" {
			inboundPaths = append(inboundPaths, mediaPath)
			combinedPrompt.WriteString(fmt.Sprintf("[첨부파일: %s] ", mediaPath))
		} else {
			t.Send(chatIDStr, fmt.Sprintf("⚠️ %d번째 미디어 다운로드에 실패했습니다.", seqIndex), SendOptions{ReplyToMessageID: messages[0].MessageID})
		}

		if msg.Caption != "" {
			captionText = msg.Caption
		} else if msg.Text != "" {
			captionText = msg.Text
		}
	}

	if captionText != "" {
		safeCaption := strings.ReplaceAll(captionText, "\n", " ")
		combinedPrompt.WriteString(safeCaption)
	} else {
		combinedPrompt.WriteString(t.msgs.DefaultMediaPrompt)
	}

	var userID string
	if len(messages) > 0 && messages[0].From != nil {
		userID = strconv.FormatInt(messages[0].From.ID, 10)
	}

	t.msgChan <- InboundEvent{
		Platform:        "telegram",
		UserID:          userID,
		ChatID:          chatIDStr,
		Kind:            EventMessage,
		Content:         combinedPrompt.String(),
		MessageID:       messages[0].MessageID,
		AttachmentPaths: inboundPaths,
	}
}

// --- Media download ---

func (t *TelegramAdapter) downloadMediaWithRetry(msg *tgbotapi.Message, chatID int64, seqIndex int) string {
	var fileID string
	var ext string

	if msg.Photo != nil && len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		fileID = photo.FileID
		ext = ".jpg"
	}

	if fileID == "" {
		return ""
	}

	fileName := fmt.Sprintf("%d_%d_%02d%s", chatID, msg.MessageID, seqIndex, ext)

	var fileURL string
	var err error

	for attempt := 1; attempt <= 3; attempt++ {
		file, apiErr := t.bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
		if apiErr == nil {
			fileURL = file.Link(t.bot.Token)
			break
		}
		err = apiErr
		log.Printf("Attempt %d: Error getting file config: %v", attempt, err)

		if tErr, ok := err.(*tgbotapi.Error); ok && tErr.Code == 429 {
			retryAfter := 5
			if tErr.ResponseParameters.RetryAfter > 0 {
				retryAfter = tErr.ResponseParameters.RetryAfter
			}
			log.Printf("Rate limited (429) getting file config. Waiting %d seconds.", retryAfter)
			time.Sleep(time.Duration(retryAfter) * time.Second)
		} else {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}

	if fileURL == "" {
		return ""
	}

	log.Printf("Downloading media from: %s", fileURL)

	exePath, _ := os.Executable()
	downloadsDir := filepath.Join(filepath.Dir(exePath), "..", "downloads")
	_ = os.MkdirAll(downloadsDir, 0755)
	destPath := filepath.Join(downloadsDir, fileName)

	for attempt := 1; attempt <= 3; attempt++ {
		retryAfter, dlErr := downloadFile(fileURL, destPath)
		if dlErr == nil {
			log.Printf("Media downloaded successfully: %s", destPath)
			return destPath
		}
		log.Printf("Attempt %d: Error downloading file: %v", attempt, dlErr)

		if retryAfter > 0 {
			log.Printf("Rate limited (429) downloading file. Waiting %d seconds.", retryAfter)
			time.Sleep(time.Duration(retryAfter) * time.Second)
		} else {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}

	return ""
}

func downloadFile(url string, destPath string) (int, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		retryAfterStr := resp.Header.Get("Retry-After")
		if retryAfter, err := strconv.Atoi(retryAfterStr); err == nil {
			return retryAfter, fmt.Errorf("429 Too Many Requests")
		}
		return 5, fmt.Errorf("429 Too Many Requests (unknown retry-after)")
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("bad status: %s", resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return 0, err
}

// --- Rich interaction capability (InteractiveSender) ---

// SendWithKeyboard posts plain text with inline buttons attached.
func (t *TelegramAdapter) SendWithKeyboard(chatID string, text string, kb InlineKeyboard) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return err
	}
	msg := tgbotapi.NewMessage(id, text)
	msg.ReplyMarkup = buildTGKeyboard(kb)
	if _, err := t.bot.Send(msg); err != nil {
		log.Printf("Telegram keyboard send failed: %v", err)
		return err
	}
	return nil
}

// AnswerCallbackQuery acknowledges a button press so Telegram stops showing
// the loading spinner on the client. Failures are non-fatal.
func (t *TelegramAdapter) AnswerCallbackQuery(callbackID, text string) {
	if t.bot == nil || callbackID == "" {
		return
	}
	cb := tgbotapi.NewCallback(callbackID, text)
	cb.ShowAlert = false
	if _, err := t.bot.Request(cb); err != nil {
		log.Printf("Telegram callback answer failed: %v", err)
	}
}

// EditMessage rewrites an existing bot message in place; a nil keyboard
// removes any buttons, which is how consumed confirmations retire their UI.
func (t *TelegramAdapter) EditMessage(chatID string, messageID int, text string, kb InlineKeyboard) {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil || messageID <= 0 {
		return
	}
	edit := tgbotapi.NewEditMessageText(id, messageID, text)
	if kb != nil {
		markup := buildTGKeyboard(kb)
		edit.ReplyMarkup = &markup
	}
	if _, err := t.bot.Request(edit); err != nil {
		log.Printf("Telegram message edit failed: %v", err)
	}
}

func buildTGKeyboard(kb InlineKeyboard) tgbotapi.InlineKeyboardMarkup {
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(kb))
	for _, row := range kb {
		buttons := make([]tgbotapi.InlineKeyboardButton, 0, len(row))
		for _, b := range row {
			buttons = append(buttons, tgbotapi.NewInlineKeyboardButtonData(b.Text, b.Data))
		}
		rows = append(rows, buttons)
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

// redactToken removes secret material from error strings before they reach
// the log file; Telegram API errors historically embed the full request URL,
// including the bot token.
func redactToken(msg string, secrets ...string) string {
	for _, s := range secrets {
		if s == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, s, "***")
	}
	return msg
}
