package main

import (
	"encoding/json"
	"errors"
	"fmt"

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
