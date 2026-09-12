package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// FlushingWriter wraps an io.Writer with a flush callback, mutex, and cancellation guard.
type FlushingWriter struct {
	W         io.Writer
	Flusher   func()
	Mu        *sync.Mutex
	IsDropped func() bool
}

func (fw *FlushingWriter) Write(p []byte) (n int, err error) {
	if fw.Mu != nil {
		fw.Mu.Lock()
		defer fw.Mu.Unlock()
	}
	if fw.IsDropped != nil && fw.IsDropped() {
		return 0, context.Canceled
	}
	n, err = fw.W.Write(p)
	if err == nil && fw.Flusher != nil {
		fw.Flusher()
	}
	return n, err
}

// StreamWriter writes SSE events formatted according to OpenAI streaming specifications.
type StreamWriter struct {
	w       io.Writer
	reqID   string
	model   string
	created int64
}

// NewStreamWriter creates a StreamWriter.
func NewStreamWriter(w io.Writer, reqID string, model string, created int64) *StreamWriter {
	return &StreamWriter{
		w:       w,
		reqID:   reqID,
		model:   model,
		created: created,
	}
}

// WriteRole emits the initial role chunk.
func (s *StreamWriter) WriteRole() error {
	roleChunk := ChatCompletionChunk{
		ID:      "chatcmpl-" + s.reqID,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{Role: "assistant"}, FinishReason: nil}},
	}
	b, err := json.Marshal(roleChunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "data: %s\n\n", string(b))
	return err
}

// WriteContentDelta emits an assistant content text delta.
func (s *StreamWriter) WriteContentDelta(delta string) error {
	chunk := ChatCompletionChunk{
		ID:      "chatcmpl-" + s.reqID,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{Content: delta}, FinishReason: nil}},
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "data: %s\n\n", string(b))
	return err
}

// WriteToolCallIntent emits tool call metadata and arguments in clean, non-split chunks.
// Each tool call produces:
// 1. A header chunk with index, id, type="function", and function name.
// 2. An arguments chunk with index and full UTF-8 arguments string in ONE single delta (avoiding rune splitting).
func (s *StreamWriter) WriteToolCallIntent(calls []ToolCall) error {
	for i, call := range calls {
		callIdx := i
		// 1. Header chunk
		funcName := call.Function.Name
		tcStart := ToolCall{
			Index: &callIdx,
			ID:    call.ID,
			Type:  "function",
			Function: ToolCallFunction{
				Name: funcName,
			},
		}
		cStart := ChatCompletionChunk{
			ID:      "chatcmpl-" + s.reqID,
			Object:  "chat.completion.chunk",
			Created: s.created,
			Model:   s.model,
			Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{ToolCalls: []ToolCall{tcStart}}, FinishReason: nil}},
		}
		bStart, err := json.Marshal(cStart)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(s.w, "data: %s\n\n", string(bStart)); err != nil {
			return err
		}

		// 2. Arguments chunk (emitted as a single delta)
		if call.Function.Arguments != "" {
			tcArgs := ToolCall{
				Index: &callIdx,
				Function: ToolCallFunction{
					Arguments: call.Function.Arguments,
				},
			}
			cArgs := ChatCompletionChunk{
				ID:      "chatcmpl-" + s.reqID,
				Object:  "chat.completion.chunk",
				Created: s.created,
				Model:   s.model,
				Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{ToolCalls: []ToolCall{tcArgs}}, FinishReason: nil}},
			}
			bArgs, err := json.Marshal(cArgs)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(s.w, "data: %s\n\n", string(bArgs)); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteFinish emits the terminal chunk containing finish_reason.
func (s *StreamWriter) WriteFinish(finishReason string) error {
	finishChunk := ChatCompletionChunk{
		ID:      "chatcmpl-" + s.reqID,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{}, FinishReason: &finishReason}},
	}
	b, err := json.Marshal(finishChunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "data: %s\n\n", string(b))
	return err
}

// WriteUsage emits the final usage chunk.
func (s *StreamWriter) WriteUsage(usage *UsageInfo) error {
	if usage == nil {
		return nil
	}
	usageChunk := ChatCompletionChunk{
		ID:      "chatcmpl-" + s.reqID,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []ChunkChoice{},
		Usage:   usage,
	}
	b, err := json.Marshal(usageChunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "data: %s\n\n", string(b))
	return err
}

// WriteError emits an SSE error event frame.
func (s *StreamWriter) WriteError(errMsg string, errCode string) error {
	errEvent := APIErrorResponse{
		Error: APIErrorBody{
			Message: errMsg,
			Type:    "api_error",
			Code:    errCode,
		},
	}
	b, err := json.Marshal(errEvent)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "event: error\ndata: %s\n\n", string(b))
	return err
}

// WriteDone emits the terminal [DONE] event frame.
func (s *StreamWriter) WriteDone() error {
	_, err := fmt.Fprintf(s.w, "data: [DONE]\n\n")
	return err
}
