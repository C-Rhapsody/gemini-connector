package main

import (
	"encoding/json"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestSendRichMessage_WireFormat(t *testing.T) {
	var capturedEndpoint string
	var capturedParams tgbotapi.Params

	adapter := &TelegramAdapter{
		chatID: 123456,
		makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
			capturedEndpoint = endpoint
			capturedParams = params
			resultJSON := []byte(`{"message_id": 999, "chat": {"id": 123456}}`)
			return &tgbotapi.APIResponse{
				Ok:     true,
				Result: resultJSON,
			}, nil
		},
	}

	htmlContent := "Hello <b>world</b> <tg-math>x &lt; y</tg-math>"
	richMsg := &inputRichMessage{
		HTML: &htmlContent,
	}

	res, err := adapter.sendRichMessage(123456, richMsg, 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.MessageID != 999 || res.Chat.ID != 123456 {
		t.Fatalf("unexpected result: %+v", res)
	}

	if capturedEndpoint != "sendRichMessage" {
		t.Fatalf("expected endpoint 'sendRichMessage', got %q", capturedEndpoint)
	}

	if capturedParams["chat_id"] != "123456" {
		t.Fatalf("expected chat_id 123456, got %q", capturedParams["chat_id"])
	}

	rawRich := capturedParams["rich_message"]
	if rawRich == "" {
		t.Fatal("rich_message parameter is empty")
	}

	var decodedRich struct {
		HTML     *string `json:"html"`
		Markdown *string `json:"markdown"`
		Blocks   any     `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(rawRich), &decodedRich); err != nil {
		t.Fatalf("failed to decode rich_message JSON: %v", err)
	}
	if decodedRich.HTML == nil || *decodedRich.HTML != htmlContent {
		t.Fatalf("expected HTML %q, got %+v", htmlContent, decodedRich.HTML)
	}
	if decodedRich.Markdown != nil {
		t.Fatalf("expected Markdown to be omitted, got %v", *decodedRich.Markdown)
	}
	if decodedRich.Blocks != nil {
		t.Fatalf("expected Blocks to be omitted, got %v", decodedRich.Blocks)
	}

	rawReply := capturedParams["reply_parameters"]
	if rawReply == "" {
		t.Fatal("reply_parameters parameter is empty")
	}
	var decodedReply struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal([]byte(rawReply), &decodedReply); err != nil {
		t.Fatalf("failed to decode reply_parameters JSON: %v", err)
	}
	if decodedReply.MessageID != 42 {
		t.Fatalf("expected reply_parameters.message_id 42, got %d", decodedReply.MessageID)
	}
}

func TestSendRichMessage_NilInputFails(t *testing.T) {
	adapter := &TelegramAdapter{
		chatID: 123456,
		makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
			return &tgbotapi.APIResponse{Ok: true}, nil
		},
	}

	_, err := adapter.sendRichMessage(123456, nil, 0)
	if err == nil {
		t.Fatal("expected error when sending nil rich_message, got nil")
	}

	_, err = adapter.sendRichMessage(123456, &inputRichMessage{HTML: nil}, 0)
	if err == nil {
		t.Fatal("expected error when sending rich_message with nil HTML, got nil")
	}
}
