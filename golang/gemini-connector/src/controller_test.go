package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Controller-level characterization tests ---
//
// These pin down the central routing contract: adapters emit events, the
// Controller decides meaning. Teams keeps treating slash-prefixed text as a
// plain agy turn; Telegram commands are handled centrally.

type recordedSendEvt struct {
	chat string
	text string
	opts SendOptions
}

// fakeMessenger implements Messenger + InteractiveSender + AttachmentSender.
type fakeMessenger struct {
	mu       sync.Mutex
	platform string
	sent     []recordedSendEvt
	answered []string
}

func (f *fakeMessenger) Init() error { return nil }

func (f *fakeMessenger) Listen() (<-chan InboundEvent, error) {
	ch := make(chan InboundEvent)
	return ch, nil
}

func (f *fakeMessenger) Send(chatID string, text string, opts ...SendOptions) error {
	var o SendOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, recordedSendEvt{chat: chatID, text: text, opts: o})
	return nil
}

func (f *fakeMessenger) StartTyping(chatID string) (stop func()) { return func() {} }

func (f *fakeMessenger) GetFile(fileID string) (string, error) { return "", nil }

func (f *fakeMessenger) SendWithKeyboard(chatID string, text string, kb InlineKeyboard) error {
	return f.Send(chatID, text)
}

func (f *fakeMessenger) AnswerCallbackQuery(callbackID, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, callbackID+"|"+text)
}

func (f *fakeMessenger) EditMessage(chatID string, messageID int, text string, kb InlineKeyboard) {
}

func (f *fakeMessenger) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeMessenger) lastText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1].text
}

func newTestController(t *testing.T, cron *CronService) (*Controller, *fakeMessenger, *TurnCoordinator) {
	t.Helper()
	resetQuotaState(t)
	tg := &fakeMessenger{platform: "telegram"}
	teams := &fakeMessenger{platform: "teams"}
	registry := NewAdapterRegistry(map[string]Messenger{
		"telegram": tg,
		"teams":    teams,
	})
	turns := NewTurnCoordinator()
	msgs := &Messages{}
	msgs.applyDefaults()
	cfg := &Config{AgyConversationID: "conv-test"}
	c := NewController(registry, turns, cfg, msgs, cron)
	t.Cleanup(func() {
		for turns.Busy() {
			turns.StopActive()
			time.Sleep(2 * time.Millisecond)
		}
	})
	return c, tg, turns
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met in time: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Teams has no command parser: "/stop" typed by a user must reach agy as a
// plain prompt through the shared queue — never the stop switch.
func TestController_TeamsSlashTextIsPlainTurn(t *testing.T) {
	c, _, turns := newTestController(t, nil)

	var invoked atomicPrompt
	restore := swapAgyInvoker(func(ctx context.Context, prompt string, convID string, opts ...AgyCallOptions) (string, error) {
		invoked.set(prompt)
		return "응답입니다", nil
	})
	defer restore()

	c.Handle(InboundEvent{Platform: "teams", ChatID: "chat-1", Kind: EventMessage, Content: "/stop", MessageID: 3})

	waitCond(t, "agy invoked with /stop prompt", func() bool { return invoked.get() == "/stop" })
	waitCond(t, "queue drained", func() bool { return !turns.Busy() })
}

// Telegram /stop must hit the coordinator's stop switch and never spawn agy.
func TestController_TelegramStopDoesNotSpawnAgy(t *testing.T) {
	c, tg, _ := newTestController(t, nil)

	calls := atomicCounter{}
	restore := swapAgyInvoker(func(ctx context.Context, prompt string, convID string, opts ...AgyCallOptions) (string, error) {
		calls.inc()
		return "", nil
	})
	defer restore()

	// Seed a running turn so StopActive has something to cancel.
	blocked := make(chan struct{})
	unwound := make(chan struct{}, 1)
	c.turns.Submit(func(ctx context.Context) {
		close(blocked)
		<-ctx.Done()
		unwound <- struct{}{}
	})
	<-blocked

	c.Handle(InboundEvent{Platform: "telegram", ChatID: "-7", Kind: EventCommand, Command: "/stop", MessageID: 4})

	waitCond(t, "stop notice sent", func() bool { return strings.Contains(tg.lastText(), "중지") })
	if got := calls.get(); got != 0 {
		t.Fatalf("agy invoked %d times on /stop", got)
	}
	select {
	case <-unwound:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled turn did not unwind")
	}
}

func TestController_UnknownCommandRepliesNotice(t *testing.T) {
	c, tg, _ := newTestController(t, nil)

	c.Handle(InboundEvent{Platform: "telegram", ChatID: "-7", Kind: EventCommand, Command: "/nope", MessageID: 5})

	waitCond(t, "unknown command notice sent", func() bool { return tg.sendCount() == 1 })
	if tg.lastText() != defaultMessages.CommandUnknown {
		t.Fatalf("unexpected reply: %q", tg.lastText())
	}
}

func TestController_StartAndHelpReplyHelpText(t *testing.T) {
	for _, cmd := range []string{"/start", "/help"} {
		c, tg, _ := newTestController(t, nil)
		c.Handle(InboundEvent{Platform: "telegram", ChatID: "-7", Kind: EventCommand, Command: cmd, MessageID: 6})
		waitCond(t, "help sent", func() bool { return tg.sendCount() == 1 })
		if tg.lastText() != defaultMessages.CommandStartHelp {
			t.Fatalf("%s: unexpected reply: %q", cmd, tg.lastText())
		}
	}
}

func TestController_CronDisabledNoticeWhenModuleMissing(t *testing.T) {
	c, tg, _ := newTestController(t, nil)

	c.Handle(InboundEvent{Platform: "telegram", ChatID: "-7", Kind: EventCommand, Command: "/cron", Args: "list", MessageID: 7})

	waitCond(t, "cron disabled notice sent", func() bool { return tg.sendCount() == 1 })
	if tg.lastText() != cronDisabledNotice {
		t.Fatalf("unexpected reply: %q", tg.lastText())
	}
}

// Unclaimed callback payloads are acknowledged and dropped centrally; the
// adapter forwarded them blindly.
func TestController_ForeignInteractionAnsweredSilently(t *testing.T) {
	c, _, _ := newTestController(t, nil)

	c.Handle(InboundEvent{Platform: "telegram", ChatID: "-7", Kind: EventInteraction, CallbackID: "cb-1", CallbackData: "game:score"})

	// No panic and no turn submitted is the contract; answering happens via
	// the registry's interactive capability.
}

// Inbound media paths must ride through to the reply's send options so the
// auto-attachment scan never echoes the user's own photo back.
func TestController_InboundAttachmentPathsExcludedFromReply(t *testing.T) {
	c, tg, turns := newTestController(t, nil)

	restore := swapAgyInvoker(func(ctx context.Context, prompt string, convID string, opts ...AgyCallOptions) (string, error) {
		return "분석 완료", nil
	})
	defer restore()

	inbound := []string{`C:\bot\downloads\51867851_26597_01.jpg`}
	c.Handle(InboundEvent{
		Platform:        "telegram",
		ChatID:          "-7",
		Kind:            EventMessage,
		Content:         "[첨부파일: C:\\bot\\downloads\\51867851_26597_01.jpg] 이거 뭐야?",
		MessageID:       9,
		AttachmentPaths: inbound,
	})

	waitCond(t, "reply sent", func() bool { return !turns.Busy() && tg.sendCount() == 1 })
	tg.mu.Lock()
	excluded := tg.sent[0].opts.ExcludeAttachments
	attachAfter := tg.sent[0].opts.AttachAfter
	tg.mu.Unlock()

	if attachAfter.IsZero() {
		t.Fatal("AI reply must keep AttachAfter for real deliverables")
	}
	if len(excluded) != 1 || excluded[0] != inbound[0] {
		t.Fatalf("exclusion not forwarded: %v", excluded)
	}
}

// --- tiny atomic helpers ---

type atomicPrompt struct {
	mu sync.Mutex
	v  string
}

func (a *atomicPrompt) set(v string) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicPrompt) get() string  { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

type atomicCounter struct {
	mu sync.Mutex
	v  int
}

func (a *atomicCounter) inc()     { a.mu.Lock(); a.v++; a.mu.Unlock() }
func (a *atomicCounter) get() int { a.mu.Lock(); defer a.mu.Unlock(); return a.v }
