package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

func isNativeToolStepType(stepType string) bool {
	st := strings.ToLower(strings.TrimSpace(stepType))
	switch st {
	case "tool",
		"tool_call",
		"tool_call_start",
		"tool_call_delta",
		"tool_output",
		"tool_result":
		return true
	default:
		return false
	}
}

func isReasoningStepType(stepType string) bool {
	st := strings.ToLower(strings.TrimSpace(stepType))
	switch st {
	case "plan",
		"thought",
		"thinking",
		"reasoning":
		return true
	default:
		return false
	}
}

func isKnownNonAssistantStepType(stepType string) bool {
	return isNativeToolStepType(stepType) || isReasoningStepType(stepType)
}

// ndjsonStreamWriter parses streaming NDJSON events from agy --output-format stream-json
// in real time as chunks arrive.
type ndjsonStreamWriter struct {
	callback                 func(delta string) error
	buf                      bytes.Buffer
	totalBytes               int64
	maxBytes                 int64
	maxLine                  int64
	sawSuccess               bool
	sawNativeToolStep        bool
	conversationID           string
	usage                    *AgyUsage
	err                      error
	mu                       sync.Mutex
	emittedDeltasCount       int
	structuredMode           bool
	terminalResponse         string
	terminalStructuredOutput []byte
	accumulatedProgress      strings.Builder
	steps                    []AgyStep
}

func (w *ndjsonStreamWriter) processLine(line []byte) error {
	var ev struct {
		Event          string `json:"event"`
		ConversationID string `json:"conversation_id"`
		StepUpdate     *struct {
			StepType  string    `json:"step_type"`
			TextDelta string    `json:"text_delta"`
			Usage     *AgyUsage `json:"usage,omitempty"`
		} `json:"step_update"`
		Result *struct {
			ConversationID   string          `json:"conversation_id"`
			Status           string          `json:"status"`
			Response         string          `json:"response"`
			StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
			Steps            []AgyStep       `json:"steps,omitempty"`
			Error            string          `json:"error"`
			Usage            *AgyUsage       `json:"usage,omitempty"`
		} `json:"result"`
	}

	if jsonErr := json.Unmarshal(bytes.TrimSpace(line), &ev); jsonErr != nil {
		return nil
	}

	if ev.ConversationID != "" {
		w.conversationID = ev.ConversationID
	}

	if ev.Event == "step_update" && ev.StepUpdate != nil {
		if ev.StepUpdate.Usage != nil {
			w.usage = ev.StepUpdate.Usage
		}
		if isNativeToolStepType(ev.StepUpdate.StepType) {
			w.sawNativeToolStep = true
			return nil
		}
		if isReasoningStepType(ev.StepUpdate.StepType) {
			return nil
		}
		if ev.StepUpdate.TextDelta != "" {
			w.accumulatedProgress.WriteString(ev.StepUpdate.TextDelta)
			if !w.structuredMode {
				w.totalBytes += int64(len(ev.StepUpdate.TextDelta))
				if w.totalBytes > w.maxBytes {
					w.err = fmt.Errorf("cumulative assistant output exceeds limit of %d bytes", w.maxBytes)
					return w.err
				}
				if w.callback != nil {
					if cbErr := w.callback(ev.StepUpdate.TextDelta); cbErr != nil {
						w.err = cbErr
						return w.err
					}
				}
				w.emittedDeltasCount++
			}
		}
	} else if ev.Event == "result" && ev.Result != nil {
		if ev.Result.ConversationID != "" {
			w.conversationID = ev.Result.ConversationID
		}
		if ev.Result.Usage != nil {
			w.usage = ev.Result.Usage
		}
		w.steps = ev.Result.Steps
		for _, st := range ev.Result.Steps {
			if isNativeToolStepType(st.StepType) {
				w.sawNativeToolStep = true
				break
			}
		}

		if ev.Result.Status == "SUCCESS" {
			w.sawSuccess = true
			w.terminalResponse = ev.Result.Response
			if len(ev.Result.StructuredOutput) > 0 {
				w.terminalStructuredOutput = ev.Result.StructuredOutput
			}
			termResp := ev.Result.Response
			if termResp == "" && len(ev.Result.StructuredOutput) > 0 {
				termResp = string(ev.Result.StructuredOutput)
			}
			if termResp == "" && w.accumulatedProgress.Len() > 0 {
				termResp = w.accumulatedProgress.String()
			}

			if w.structuredMode {
				if termResp != "" {
					w.totalBytes += int64(len(termResp))
					if w.totalBytes > w.maxBytes {
						w.err = fmt.Errorf("cumulative assistant output exceeds limit of %d bytes", w.maxBytes)
						return w.err
					}
					if w.callback != nil {
						if cbErr := w.callback(termResp); cbErr != nil {
							w.err = cbErr
							return w.err
						}
					}
					w.emittedDeltasCount++
				}
			} else if w.emittedDeltasCount == 0 && termResp != "" {
				w.totalBytes += int64(len(termResp))
				if w.totalBytes > w.maxBytes {
					w.err = fmt.Errorf("cumulative assistant output exceeds limit of %d bytes", w.maxBytes)
					return w.err
				}
				if w.callback != nil {
					if cbErr := w.callback(termResp); cbErr != nil {
						w.err = cbErr
						return w.err
					}
				}
				w.emittedDeltasCount++
			}

			if w.sawNativeToolStep && w.emittedDeltasCount == 0 && termResp == "" {
				w.err = &AgyError{
					Type:   "native_tool_containment_violation",
					Detail: "native AGY tool execution is prohibited in ProfileAPI; tools must be executed by client",
				}
				return w.err
			}
		} else {
			errDetail := ev.Result.Error
			if errDetail == "" {
				errDetail = ev.Result.Status
			}
			w.err = fmt.Errorf("upstream result status: %s", errDetail)
			return w.err
		}
	}
	return nil
}

func (w *ndjsonStreamWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}

	w.buf.Write(p)
	for {
		line, readErr := w.buf.ReadBytes('\n')
		if readErr != nil {
			// Partial line; put it back into w.buf
			w.buf.Write(line)
			if int64(w.buf.Len()) > w.maxLine {
				w.err = fmt.Errorf("stream line exceeds maximum size %d bytes", w.maxLine)
				return len(p), w.err
			}
			break
		}
		if int64(len(line)) > w.maxLine {
			w.err = fmt.Errorf("stream line exceeds maximum size %d bytes", w.maxLine)
			return len(p), w.err
		}

		if err := w.processLine(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

func (w *ndjsonStreamWriter) Finalize() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if w.buf.Len() > 0 {
		line := w.buf.Bytes()
		w.buf.Reset()
		_ = w.processLine(line)
	}
	return w.err
}
