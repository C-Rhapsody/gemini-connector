package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Shared fakes ---

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t *testing.T) *fakeClock {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	return &fakeClock{now: time.Date(2026, 8, 24, 10, 0, 0, 0, loc)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type recordedSend struct {
	chat string
	text string
	opts SendOptions
	kb   InlineKeyboard
}

type recordedEdit struct {
	chat      string
	messageID int
	text      string
	kb        InlineKeyboard
}

// stubCronUI captures every outbound interaction for assertions.
type stubCronUI struct {
	mu       sync.Mutex
	sends    []recordedSend
	edits    []recordedEdit
	answered []string
}

func (s *stubCronUI) Init() error { return nil }
func (s *stubCronUI) Listen() (<-chan InboundEvent, error) {
	return nil, nil
}
func (s *stubCronUI) GetFile(id string) (string, error) { return "", nil }
func (s *stubCronUI) StartTyping(chatID string) (stop func()) {
	return func() {}
}
func (s *stubCronUI) Send(chatID string, text string, opts ...SendOptions) error {
	var o SendOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends = append(s.sends, recordedSend{chat: chatID, text: text, opts: o})
	return nil
}
func (s *stubCronUI) SendWithKeyboard(chatID string, text string, kb InlineKeyboard) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends = append(s.sends, recordedSend{chat: chatID, text: text, kb: kb})
	return nil
}
func (s *stubCronUI) AnswerCallbackQuery(callbackID, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answered = append(s.answered, callbackID+"|"+text)
}
func (s *stubCronUI) EditMessage(chatID string, messageID int, text string, kb InlineKeyboard) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.edits = append(s.edits, recordedEdit{chat: chatID, messageID: messageID, text: text, kb: kb})
}

func (s *stubCronUI) lastKeyboard() InlineKeyboard {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.sends) - 1; i >= 0; i-- {
		if len(s.sends[i].kb) > 0 {
			return s.sends[i].kb
		}
	}
	return nil
}

func (s *stubCronUI) lastTextContaining(substr string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.sends) - 1; i >= 0; i-- {
		if strings.Contains(s.sends[i].text, substr) {
			return s.sends[i].text, true
		}
	}
	return "", false
}

// plannerStub replays canned agy outputs and counts invocations.
type plannerStub struct {
	mu        sync.Mutex
	responses []string
	errs      []error
	calls     int
	prompts   []string
}

func (p *plannerStub) plan(_ context.Context, prompt string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.calls
	p.calls++
	p.prompts = append(p.prompts, prompt)
	if i < len(p.errs) && p.errs[i] != nil {
		return "", p.errs[i]
	}
	if i < len(p.responses) {
		return p.responses[i], nil
	}
	return "", nil
}

func (p *plannerStub) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// findButtonData returns the callback payload of the first button whose data
// carries the given prefix.
func findButtonData(kb InlineKeyboard, prefix string) (string, bool) {
	for _, row := range kb {
		for _, b := range row {
			if strings.HasPrefix(b.Data, prefix) {
				return b.Data, true
			}
		}
	}
	return "", false
}

// trimPrefix removes a cron callback prefix from a data payload.
func trimCbPrefix(data, prefix string) string {
	return strings.TrimPrefix(data, prefix)
}

func newTestCronStore(t *testing.T) *CronStore {
	t.Helper()
	store, err := OpenCronStore(filepath.Join(t.TempDir(), "cron.db"))
	if err != nil {
		t.Fatalf("open cron store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func testOwner(id string) CronOwner {
	return CronOwner{Platform: "telegram", ChatID: "-100chat", UserID: id}
}

// cronMsg builds a /cron InboundEvent.
func cronMsg(user string, args string, msgID int) InboundEvent {
	return InboundEvent{
		Platform:  "telegram",
		UserID:    user,
		ChatID:    testOwner(user).ChatID,
		Command:   "/cron",
		Args:      args,
		MessageID: msgID,
	}
}

func cbMsg(user, data string, msgID int) InboundEvent {
	return InboundEvent{
		Platform:     "telegram",
		UserID:       user,
		ChatID:       testOwner(user).ChatID,
		Command:      "/cron-callback",
		Args:         data,
		MessageID:    msgID,
		CallbackID:   "cb-" + user + "-" + data,
		CallbackData: data,
	}
}
