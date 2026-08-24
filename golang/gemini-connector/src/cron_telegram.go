package main

import (
	"log"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// CronTelegramUI is the slice of the Telegram adapter the cron subsystem is
// allowed to touch. Keeping it a narrow interface lets tests stub the whole
// surface and leaves Teams untouched (cron is Telegram-only for now).
type CronTelegramUI interface {
	Messenger
	SendWithKeyboard(chatID string, text string, kb InlineKeyboard) error
	AnswerCallbackQuery(callbackID, text string)
	EditMessage(chatID string, messageID int, text string, kb InlineKeyboard)
}

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
