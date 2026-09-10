package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

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

