package main

import (
	"strconv"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func testAdapter(t *testing.T) (*TelegramAdapter, chan InternalMessage) {
	t.Helper()
	ch := make(chan InternalMessage, 10)
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

// Regression: every Telegram command must carry the sender identity; the
// cron ownership model depends on InternalMessage.UserID being populated.
func TestTelegramCommandsFillUserID(t *testing.T) {
	commands := map[string]InternalMessage{
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
			t.Fatalf("%s produced no InternalMessage", text)
		}
	}
}

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
	case m := <-ch:
		if m.Command != "/cron-callback" {
			t.Fatalf("command = %q", m.Command)
		}
		if m.CallbackID != "cbq-1" || m.CallbackData != cq.Data || m.Args != cq.Data {
			t.Fatalf("callback fields wrong: %+v", m)
		}
		if m.UserID != "42" || m.ChatID != "-7" || m.MessageID != 9 {
			t.Fatalf("scope fields wrong: %+v", m)
		}
	default:
		t.Fatal("callback was not routed")
	}
}

func TestHandleCallbackQueryIgnoresForeignData(t *testing.T) {
	adapter, ch := testAdapter(t)
	cq := &tgbotapi.CallbackQuery{
		ID:      "cbq-2",
		From:    &tgbotapi.User{ID: 42},
		Message: &tgbotapi.Message{MessageID: 9, Chat: &tgbotapi.Chat{ID: -7}},
		Data:    "game:score",
	}
	adapter.handleCallbackQuery(cq) // answered (bot nil-safe) and dropped
	select {
	case m := <-ch:
		t.Fatalf("foreign callback leaked through: %+v", m)
	default:
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
