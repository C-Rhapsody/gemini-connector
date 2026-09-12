package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// BuildChatCompletionResponse converts a CompletionResult into a standard ChatCompletionResponse.
func BuildChatCompletionResponse(reqID string, model string, res *CompletionResult, remoteHistory bool) *ChatCompletionResponse {
	respObj := &ChatCompletionResponse{
		ID:      "chatcmpl-" + reqID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
	}

	if res.Usage != nil {
		respObj.Usage = mapAgyUsageForRequest(res.Usage, remoteHistory)
	}

	var choice ChatCompletionChoice
	choice.Index = 0

	if len(res.ToolCalls) > 0 {
		choice.Message = ChatMessage{
			Role:      "assistant",
			Content:   nil,
			ToolCalls: res.ToolCalls,
		}
		choice.FinishReason = "tool_calls"
	} else {
		choice.Message = ChatMessage{
			Role:    "assistant",
			Content: res.Text,
		}
		choice.FinishReason = res.FinishReason
		if choice.FinishReason == "" {
			choice.FinishReason = "stop"
		}
	}

	respObj.Choices = []ChatCompletionChoice{choice}
	return respObj
}

// WriteNonStreamResponse writes a standard JSON ChatCompletionResponse to the HTTP ResponseWriter.
func WriteNonStreamResponse(w http.ResponseWriter, reqID string, model string, res *CompletionResult, remoteHistory bool) error {
	respObj := BuildChatCompletionResponse(reqID, model, res, remoteHistory)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(respObj)
}
