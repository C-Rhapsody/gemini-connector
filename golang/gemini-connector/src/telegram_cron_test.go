package main

import (
	"strconv"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func testAdapter(t *testing.T) (*TelegramAdapter, chan InboundEvent) {
	t.Helper()
	ch := make(chan InboundEvent, 10)
	return &TelegramAdapter{
		msgs:    &Messages{},
		msgChan: ch,
	}, ch
}

func tgMessage(userID int64, chatID int64, text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		MessageID: 5,
		From:      &tgbotapi.User{ID: userID},
		Chat:      &tgbotapi.Chat{ID: chatID},
		Text:      text,
	}
}

// tgCommandMessage builds a message carrying a real bot_command entity, like
// actual Telegram updates do; without it IsCommand() reports false.
func tgCommandMessage(userID int64, chatID int64, text string, commandLength int) *tgbotapi.Message {
	m := tgMessage(userID, chatID, text)
	m.Entities = []tgbotapi.MessageEntity{{Type: "bot_command", Offset: 0, Length: commandLength}}
	return m
}

// Regression: every Telegram command must carry the sender identity; the
// cron ownership model depends on InboundEvent.UserID being populated.
func TestTelegramCommandsFillUserID(t *testing.T) {
	commands := map[string]InboundEvent{
		"/stop":   {Command: "/stop"},
		"/clear":  {Command: "/clear"},
		"/status": {Command: "/status"},
		"/cron":   {Command: "/cron", Args: "list"},
	}
	for text := range commands {
		adapter, ch := testAdapter(t)
		adapter.handleIncomingMessage(tgMessage(42, -7, text))
		select {
		case m := <-ch:
			if m.UserID != "42" {
				t.Fatalf("%s: UserID = %q", text, m.UserID)
			}
			if m.ChatID != "-7" {
				t.Fatalf("%s: ChatID = %q", text, m.ChatID)
			}
		default:
			t.Fatalf("%s produced no InboundEvent", text)
		}
	}
}

// The adapter identifies commands but must not interpret them: every slash
// command — including /start, /help and unknown ones — is forwarded to the
// central Controller as a command event.
func TestTelegramCommandsForwardVerbatim(t *testing.T) {
	cases := []struct {
		text      string
		cmdLength int
		command   string
		args      string
	}{
		{text: "/start", cmdLength: 6, command: "/start"},
		{text: "/help", cmdLength: 5, command: "/help"},
		{text: "/stop", cmdLength: 5, command: "/stop"},
		{text: "/cron list", cmdLength: 5, command: "/cron", args: "list"},
		{text: "/image a cat", cmdLength: 6, command: "/image", args: "a cat"},
		{text: "/nope extra", cmdLength: 5, command: "/nope", args: "extra"},
	}
	for _, tc := range cases {
		adapter, ch := testAdapter(t)
		adapter.handleIncomingMessage(tgCommandMessage(42, -7, tc.text, tc.cmdLength))
		select {
		case ev := <-ch:
			if ev.RouteKind() != EventCommand {
				t.Fatalf("%s: kind = %q", tc.text, ev.Kind)
			}
			if ev.Command != tc.command || ev.Args != tc.args {
				t.Fatalf("%s: command=%q args=%q", tc.text, ev.Command, ev.Args)
			}
		default:
			t.Fatalf("%s produced no event", tc.text)
		}
	}
}

// Cron button presses reach the pipeline as opaque interaction events.
func TestHandleCallbackQueryRoutesCronData(t *testing.T) {
	adapter, ch := testAdapter(t)
	cq := &tgbotapi.CallbackQuery{
		ID:   "cbq-1",
		From: &tgbotapi.User{ID: 42},
		Message: &tgbotapi.Message{
			MessageID: 9,
			Chat:      &tgbotapi.Chat{ID: -7},
		},
		Data: cronCbConfirm + "abcdef0123456789",
	}
	adapter.handleCallbackQuery(cq)

	select {
	case ev := <-ch:
		if ev.RouteKind() != EventInteraction {
			t.Fatalf("kind = %q", ev.Kind)
		}
		if ev.CallbackID != "cbq-1" || ev.CallbackData != cq.Data {
			t.Fatalf("callback fields wrong: %+v", ev)
		}
		if ev.UserID != "42" || ev.ChatID != "-7" || ev.MessageID != 9 {
			t.Fatalf("scope fields wrong: %+v", ev)
		}
	default:
		t.Fatal("callback was not routed")
	}
}

// Foreign callback payloads are forwarded verbatim too; dropping and
// acknowledging them is the central interaction router's job.
func TestHandleCallbackQueryForwardsForeignData(t *testing.T) {
	adapter, ch := testAdapter(t)
	cq := &tgbotapi.CallbackQuery{
		ID:      "cbq-2",
		From:    &tgbotapi.User{ID: 42},
		Message: &tgbotapi.Message{MessageID: 9, Chat: &tgbotapi.Chat{ID: -7}},
		Data:    "game:score",
	}
	adapter.handleCallbackQuery(cq)
	select {
	case ev := <-ch:
		if ev.RouteKind() != EventInteraction || ev.CallbackData != "game:score" {
			t.Fatalf("foreign callback not forwarded intact: %+v", ev)
		}
	default:
		t.Fatal("foreign callback was not forwarded")
	}
}

func TestHandleCallbackQueryRejectsForeignChat(t *testing.T) {
	adapter, ch := testAdapter(t)
	adapter.chatID = -100 // allowlist mismatch with the query below
	cq := &tgbotapi.CallbackQuery{
		ID:      "cbq-3",
		From:    &tgbotapi.User{ID: 42},
		Message: &tgbotapi.Message{MessageID: 9, Chat: &tgbotapi.Chat{ID: -7}},
		Data:    cronCbCancel + "abcdef0123456789",
	}
	adapter.handleCallbackQuery(cq)
	select {
	case m := <-ch:
		t.Fatalf("unauthorized callback routed: %+v", m)
	default:
	}
}

func TestRedactToken(t *testing.T) {
	token := "123456:ABC-SECRET"
	errText := `Get "https://api.telegram.org/bot` + token + `/getMe": dial tcp 1.2.3.4:443: i/o timeout`
	got := redactToken(errText, token)
	if strings.Contains(got, token) {
		t.Fatalf("token survived redaction: %q", got)
	}
	if !strings.Contains(got, "bot***/getMe") {
		t.Fatalf("redaction changed surrounding text unexpectedly: %q", got)
	}
	if redactToken("no secrets here", "") != "no secrets here" {
		t.Fatal("empty secret must be a no-op")
	}
}

func TestCronCallbackDataBudget(t *testing.T) {
	confirm := cronCbConfirm + "0123456789abcdef"
	cancel := cronCbCancel + "0123456789abcdef"
	selectRef := cronCbSelectRef + "0123456789abcdef" + ":" + "fedcba9876543210"
	for _, d := range []string{confirm, cancel, selectRef} {
		if len(d) > 64 {
			t.Fatalf("data %q exceeds Telegram 64-byte limit (%d)", d, len(d))
		}
	}
	if strconv.Itoa(len(selectRef)) == "" {
		t.Fatal("unreachable")
	}
}
