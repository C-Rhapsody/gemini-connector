package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
)

// ndjsonStreamWriter parses streaming NDJSON events from agy --output-format stream-json
// in real time as chunks arrive.
type ndjsonStreamWriter struct {
	callback   func(delta string) error
	buf        bytes.Buffer
	totalBytes int64
	maxBytes   int64
	maxLine    int64
	sawSuccess bool
	usage      *AgyUsage
	err        error
	mu         sync.Mutex
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

		var ev struct {
			Event      string `json:"event"`
			StepUpdate *struct {
				StepType  string    `json:"step_type"`
				TextDelta string    `json:"text_delta"`
				Usage     *AgyUsage `json:"usage,omitempty"`
			} `json:"step_update"`
			Result *struct {
				Status string    `json:"status"`
				Usage  *AgyUsage `json:"usage,omitempty"`
			} `json:"result"`
		}
		if jsonErr := json.Unmarshal(bytes.TrimSpace(line), &ev); jsonErr == nil {
			if ev.Event == "step_update" && ev.StepUpdate != nil {
				if ev.StepUpdate.Usage != nil {
					w.usage = ev.StepUpdate.Usage
				}
				if ev.StepUpdate.TextDelta != "" {
					w.totalBytes += int64(len(ev.StepUpdate.TextDelta))
					if w.totalBytes > w.maxBytes {
						w.err = fmt.Errorf("cumulative assistant output exceeds limit of %d bytes", w.maxBytes)
						return len(p), w.err
					}
					if w.callback != nil {
						if cbErr := w.callback(ev.StepUpdate.TextDelta); cbErr != nil {
							w.err = cbErr
							return len(p), w.err
						}
					}
				}
			} else if ev.Event == "result" && ev.Result != nil {
				if ev.Result.Usage != nil {
					w.usage = ev.Result.Usage
				}
				if ev.Result.Status == "SUCCESS" {
					w.sawSuccess = true
				} else {
					w.err = fmt.Errorf("upstream result status: %s", ev.Result.Status)
					return len(p), w.err
				}
			}
		}
	}
	return len(p), nil
}
