package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func (s *OpenAICompatibleServer) handleChatCompletions(w http.ResponseWriter, r *http.Request, reqID string, startTime time.Time, clientClass string) {
	// t0 starts immediately after successful authentication
	reqCtx, reqCancel := context.WithTimeout(r.Context(), defaultMaxRequestDuration)
	defer reqCancel()

	// Content-Type validation
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Content-Type must be application/json", "invalid_content_type", strPtr("Content-Type"), false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_content_type", startTime, false, nil, 0, clientClass)
		return
	}

	// Bounded body reader (1 MiB)
	bodyReader := http.MaxBytesReader(w, r.Body, defaultMaxBodyBytes)
	bodyBytes, err := io.ReadAll(bodyReader)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Request body exceeds maximum size of 1 MiB", "request_too_large", strPtr("body"), false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "request_too_large", startTime, false, nil, 0, clientClass)
		return
	}

	// Parse into raw map to detect rejected/unsupported fields
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &rawMap); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Malformed JSON body", "invalid_json", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_json", startTime, false, nil, 0, clientClass)
		return
	}

	for _, field := range rejectedFields {
		if _, ok := rawMap[field]; ok {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Field %q is not supported", field), "unsupported_field", strPtr(field), false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "unsupported_field", startTime, false, nil, 0, clientClass)
			return
		}
	}

	if nRaw, ok := rawMap["n"]; ok {
		var n int
		if err := json.Unmarshal(nRaw, &n); err == nil && n > 1 {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "n > 1 is not supported", "unsupported_value", strPtr("n"), false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "unsupported_value", startTime, false, nil, 0, clientClass)
			return
		}
	}

	var effectiveMaxCompletionTokens *int

	// Validate max_tokens if present in raw JSON
	if rawMaxTokens, ok := rawMap["max_tokens"]; ok {
		var mt int
		if err := json.Unmarshal(rawMaxTokens, &mt); err != nil || mt <= 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens must be a positive integer", "invalid_value", strPtr("max_tokens"), false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_value", startTime, false, nil, 0, clientClass)
			return
		}
		effectiveMaxCompletionTokens = &mt
	}

	// Validate max_completion_tokens if present in raw JSON
	if rawMCT, ok := rawMap["max_completion_tokens"]; ok {
		var mct int
		if err := json.Unmarshal(rawMCT, &mct); err != nil || mct <= 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "max_completion_tokens must be a positive integer", "invalid_value", strPtr("max_completion_tokens"), false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_value", startTime, false, nil, 0, clientClass)
			return
		}
		// Modern max_completion_tokens takes precedence if both provided
		effectiveMaxCompletionTokens = &mct
	}
	_ = effectiveMaxCompletionTokens

	var req ChatCompletionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Failed to decode completion request", "invalid_request", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_request", startTime, false, nil, 0, clientClass)
		return
	}

	// Normalize legacy functions to tools
	if len(req.Functions) > 0 && len(req.Tools) == 0 {
		for _, fn := range req.Functions {
			fnCopy := fn
			req.Tools = append(req.Tools, ToolDefinition{
				Type:     "function",
				Function: &fnCopy,
			})
		}
	}
	if req.FunctionCall != nil && req.ToolChoice == nil {
		req.ToolChoice = req.FunctionCall
	}

	// Validate response_format and extract schema if requested
	var schemaToPass string
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
		case "", "text":
			// plain text
		case "json_object":
			schemaToPass = `{"type":"object"}`
		case "json_schema":
			if req.ResponseFormat.JSONSchema != nil && len(req.ResponseFormat.JSONSchema.Schema) > 0 {
				schemaToPass = strings.TrimSpace(string(req.ResponseFormat.JSONSchema.Schema))
			} else if len(req.ResponseFormat.Schema) > 0 {
				schemaToPass = strings.TrimSpace(string(req.ResponseFormat.Schema))
			} else {
				schemaToPass = `{"type":"object"}`
			}
		default:
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Unsupported response_format type %q", req.ResponseFormat.Type), "unsupported_response_format_type", strPtr("response_format.type"), false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "unsupported_response_format_type", startTime, req.Stream, &req.Model, 0, clientClass)
			return
		}
	}

	if schemaToPass != "" {
		var jsTest any
		if err := json.Unmarshal([]byte(schemaToPass), &jsTest); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "response_format schema is not valid JSON", "invalid_response_format_schema", strPtr("response_format"), false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_response_format_schema", startTime, req.Stream, &req.Model, 0, clientClass)
			return
		}
	}

	// Model validation
	if req.Model == "" || !strings.HasPrefix(req.Model, "gemini-") {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "A valid Gemini model ID is required", "model_not_found", strPtr("model"), false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "model_not_found", startTime, req.Stream, nil, 0, clientClass)
		return
	}

	ok, valErr := s.catalog.ValidateModel(reqCtx, req.Model)
	if valErr != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "api_error", "Catalog unavailable", "catalog_unavailable", nil, true)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusServiceUnavailable, "catalog_unavailable", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Model %q not found in catalog", req.Model), "model_not_found", strPtr("model"), false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "model_not_found", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}

	// Messages validation
	if len(req.Messages) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "messages must be a non-empty array", "invalid_messages", strPtr("messages"), false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_messages", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}
	if len(req.Messages) > defaultMaxMessages {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("messages count exceeds maximum %d", defaultMaxMessages), "messages_too_many", strPtr("messages"), false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "messages_too_many", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}

	totalMsgBytes := 0
	for idx, msg := range req.Messages {
		switch msg.Role {
		case "developer", "system", "user", "assistant", "tool":
		default:
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Unsupported role %q at messages[%d]", msg.Role, idx), "invalid_role", strPtr("role"), false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_role", startTime, req.Stream, &req.Model, 0, clientClass)
			return
		}
		msgStr := stringifyMessageContent(msg.Content)
		msgLen := len(msgStr)
		if msgLen > defaultMaxMessageBytes {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Message at index %d exceeds maximum %d bytes", idx, defaultMaxMessageBytes), "message_too_large", strPtr("content"), false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "message_too_large", startTime, req.Stream, &req.Model, 0, clientClass)
			return
		}
		totalMsgBytes += msgLen
	}
	if totalMsgBytes > defaultMaxAggregateBytes {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Aggregate messages content exceeds maximum %d bytes", defaultMaxAggregateBytes), "messages_aggregate_too_large", strPtr("messages"), false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "messages_aggregate_too_large", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}

	// Drain check
	if atomic.LoadInt32(&s.draining) == 1 {
		writeAPIError(w, http.StatusServiceUnavailable, "api_error", "Server is shutting down", "server_draining", nil, true)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusServiceUnavailable, "server_draining", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}

	// Render messages into role-preserving structured prompt
	prompt, err := RenderPrompt(req.Messages)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "api_error", "Internal error rendering prompt", "internal_error", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusInternalServerError, "internal_error", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}

	stopSeqs := parseStopSequences(req.Stop)
	wantUsage := false
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		wantUsage = true
	} else if req.IncludeUsage != nil && *req.IncludeUsage {
		wantUsage = true
	}

	// Prepare queue admission
	queueStart := time.Now()
	var queueWaitMS atomic.Int64
	var streamHeadersSent atomic.Bool

	jobStarted := make(chan struct{})
	var startOnce sync.Once
	markStarted := func() {
		startOnce.Do(func() {
			close(jobStarted)
		})
	}

	jobDone := make(chan struct{})
	var turnErr error
	var turnResp string
	var turnUsage *AgyUsage

	s.activeJobs.Add(1)
	defer s.activeJobs.Done()

	// Enqueue in TurnCoordinator
	_, submitErr := s.turns.SubmitAPI(reqCtx, func(jobCtx context.Context) {
		markStarted()
		queueWaitMS.Store(time.Since(queueStart).Milliseconds())
		defer close(jobDone)

		// Re-check snapshot validity at worker admission
		exists, expired := s.catalog.CheckModelSnapshotValid(req.Model)
		if expired {
			turnErr = &AgyError{Type: "catalog_unavailable", Detail: "model catalog snapshot expired during queue wait"}
			return
		}
		if !exists {
			turnErr = &AgyError{Type: "model_unavailable", Detail: "model disappeared from catalog"}
			return
		}

		if jobCtx.Err() != nil || reqCtx.Err() != nil {
			turnErr = jobCtx.Err()
			return
		}

		// Execution context bounded by 90s execution cap
		execCtx, execCancel := context.WithTimeout(jobCtx, defaultMaxExecutionDuration)
		defer execCancel()

		if req.Stream {
			flusher, ok := w.(http.Flusher)
			if !ok {
				turnErr = errors.New("streaming unsupported by response writer")
				return
			}
			safeFlush := func() {
				defer func() {
					_ = recover()
				}()
				flusher.Flush()
			}

			var writeMu sync.Mutex
			writeEvent := func(data []byte) error {
				writeMu.Lock()
				defer writeMu.Unlock()
				if jobCtx.Err() != nil || reqCtx.Err() != nil {
					return jobCtx.Err()
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", string(data)); err != nil {
					return err
				}
				safeFlush()
				return nil
			}
			writeRaw := func(format string, args ...any) error {
				writeMu.Lock()
				defer writeMu.Unlock()
				if jobCtx.Err() != nil || reqCtx.Err() != nil {
					return jobCtx.Err()
				}
				if _, err := fmt.Fprintf(w, format, args...); err != nil {
					return err
				}
				safeFlush()
				return nil
			}

			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			w.Header().Set("X-Request-ID", reqID)
			w.WriteHeader(http.StatusOK)
			streamHeadersSent.Store(true)
			safeFlush()

			// Heartbeat ticker with explicit termination synchronization
			stopHeartbeat := make(chan struct{})
			heartbeatDone := make(chan struct{})
			var stopHeartbeatOnce sync.Once
			shutdownHeartbeat := func() {
				stopHeartbeatOnce.Do(func() {
					close(stopHeartbeat)
					<-heartbeatDone
				})
			}
			defer shutdownHeartbeat()

			hbInterval := s.heartbeatInterval
			if hbInterval <= 0 {
				hbInterval = defaultHeartbeatInterval
			}

			go func() {
				defer close(heartbeatDone)
				ticker := time.NewTicker(hbInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						if jobCtx.Err() != nil || reqCtx.Err() != nil {
							return
						}
						_ = writeRaw(": heartbeat\n\n")
					case <-stopHeartbeat:
						return
					case <-jobCtx.Done():
						return
					case <-reqCtx.Done():
						return
					}
				}
			}()

			created := time.Now().Unix()
			// Role chunk is the first SSE frame
			roleChunk := ChatCompletionChunk{
				ID:      "chatcmpl-" + reqID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   req.Model,
				Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{Role: "assistant"}, FinishReason: nil}},
			}
			roleBytes, _ := json.Marshal(roleChunk)
			if err := writeEvent(roleBytes); err != nil {
				turnErr = err
				return
			}

			var cumText strings.Builder
			stopped := false
			streamCb := func(delta string) error {
				if jobCtx.Err() != nil || reqCtx.Err() != nil {
					return jobCtx.Err()
				}
				if stopped {
					return nil
				}
				prevLen := cumText.Len()
				cumText.WriteString(delta)
				curStr := cumText.String()

				if len(stopSeqs) > 0 {
					truncated, hitStop := applyStopSequences(curStr, stopSeqs)
					if hitStop {
						stopped = true
						if len(truncated) > prevLen {
							emitDelta := truncated[prevLen:]
							chunk := ChatCompletionChunk{
								ID:      "chatcmpl-" + reqID,
								Object:  "chat.completion.chunk",
								Created: created,
								Model:   req.Model,
								Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{Content: emitDelta}, FinishReason: nil}},
							}
							b, _ := json.Marshal(chunk)
							return writeEvent(b)
						}
						return nil
					}
				}

				chunk := ChatCompletionChunk{
					ID:      "chatcmpl-" + reqID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   req.Model,
					Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{Content: delta}, FinishReason: nil}},
				}
				b, mErr := json.Marshal(chunk)
				if mErr != nil {
					return mErr
				}
				return writeEvent(b)
			}

			var actualUsage *AgyUsage
			_, streamErr := s.executor.Execute(execCtx, prompt, "", AgyCallOptions{
				Profile:        ProfileAPI,
				Model:          req.Model,
				Stream:         true,
				StreamCallback: streamCb,
				Logger:         s.logger,
				JSONSchema:     schemaToPass,
				UsageCallback: func(u *AgyUsage) {
					actualUsage = u
				},
			})
			if streamErr != nil {
				// If the stream ended because the client disconnected or context timed out/canceled,
				// do NOT attempt to write error frames or flush to the response writer!
				if jobCtx.Err() != nil || reqCtx.Err() != nil || errors.Is(streamErr, context.Canceled) {
					turnErr = streamErr
					return
				}
				// Send post-header error frame and close without finish/[DONE]
				errEvent := APIErrorResponse{
					Error: APIErrorBody{
						Message: "Upstream error during stream",
						Type:    "api_error",
						Code:    "upstream_stream_error",
					},
				}
				errBytes, _ := json.Marshal(errEvent)
				_ = writeRaw("event: error\ndata: %s\n\n", string(errBytes))
				turnErr = streamErr
				return
			}

			// Terminal finish chunk
			stopStr := "stop"
			finishChunk := ChatCompletionChunk{
				ID:      "chatcmpl-" + reqID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   req.Model,
				Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{}, FinishReason: &stopStr}},
			}
			fBytes, _ := json.Marshal(finishChunk)
			if err := writeEvent(fBytes); err != nil {
				turnErr = err
				return
			}

			// Usage chunk before [DONE] if requested and available
			if wantUsage && actualUsage != nil {
				usageChunk := ChatCompletionChunk{
					ID:      "chatcmpl-" + reqID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   req.Model,
					Choices: []ChunkChoice{},
					Usage:   mapAgyUsageToOpenAI(actualUsage),
				}
				uBytes, _ := json.Marshal(usageChunk)
				if err := writeEvent(uBytes); err != nil {
					turnErr = err
					return
				}
			}

			_ = writeRaw("data: [DONE]\n\n")
		} else {
			var actualUsage *AgyUsage
			resp, nonStreamErr := s.executor.Execute(execCtx, prompt, "", AgyCallOptions{
				Profile:    ProfileAPI,
				Model:      req.Model,
				Stream:     false,
				Logger:     s.logger,
				JSONSchema: schemaToPass,
				UsageCallback: func(u *AgyUsage) {
					actualUsage = u
				},
			})
			if len(stopSeqs) > 0 && nonStreamErr == nil {
				resp, _ = applyStopSequences(resp, stopSeqs)
			}
			turnResp = resp
			turnErr = nonStreamErr
			if nonStreamErr == nil {
				turnUsage = actualUsage
			}
		}
	})

	if submitErr != nil {
		if errors.Is(submitErr, ErrAPIQueueFull) {
			writeAPIError(w, http.StatusTooManyRequests, "rate_limit_error", "Queue full", "queue_full", nil, true)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusTooManyRequests, "queue_full", startTime, req.Stream, &req.Model, 0, clientClass)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "api_error", "Internal queue error", "internal_error", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusInternalServerError, "internal_error", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}

	// Await completion or timeout/cancellation
	select {
	case <-jobDone:
		// Completed normally or finished with error inside worker
	case <-time.After(defaultQueueWaitTimeout):
		select {
		case <-jobStarted:
			// Job is already running. Wait for it up to client disconnect/timeout,
			// and if cancelled, wait for worker to exit cleanly before handler returns.
			select {
			case <-jobDone:
			case <-reqCtx.Done():
				<-jobDone
			}
		default:
			// Job was still queued and never started.
			// When worker eventually reaches this job, job.ctx.Err() will be checked and dropped.
			writeAPIError(w, http.StatusServiceUnavailable, "api_error", "Queue wait timeout", "queue_wait_timeout", nil, true)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusServiceUnavailable, "queue_wait_timeout", startTime, req.Stream, &req.Model, int64(defaultQueueWaitTimeout/time.Millisecond), clientClass)
			return
		}
	case <-reqCtx.Done():
		select {
		case <-jobStarted:
			// Job already started running. Wait for worker to exit cleanly before handler returns!
			<-jobDone
		default:
			// Job never started running.
			writeAPIError(w, http.StatusGatewayTimeout, "api_error", "Request timeout", "request_timeout", nil, false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusGatewayTimeout, "request_timeout", startTime, req.Stream, &req.Model, 0, clientClass)
			return
		}
	}

	if reqCtx.Err() != nil {
		status := http.StatusGatewayTimeout
		code := "request_timeout"
		if errors.Is(reqCtx.Err(), context.Canceled) {
			status = 499
			code = "client_closed_request"
		}
		if !streamHeadersSent.Load() {
			writeAPIError(w, status, "api_error", "Request canceled or timed out", code, nil, false)
		}
		s.logOutcome(reqID, "POST", "/v1/chat/completions", status, code, startTime, req.Stream, &req.Model, queueWaitMS.Load(), clientClass)
		return
	}

	if turnErr != nil {
		ae, isAgyErr := turnErr.(*AgyError)
		status := http.StatusInternalServerError
		code := "upstream_error"
		retry := false

		if isAgyErr {
			switch ae.Type {
			case "catalog_unavailable":
				status = http.StatusServiceUnavailable
				code = "catalog_unavailable"
				retry = true
			case "model_unavailable":
				status = http.StatusServiceUnavailable
				code = "model_unavailable"
				retry = true
			case "launch_timeout":
				status = http.StatusServiceUnavailable
				code = "launch_timeout"
				retry = true
			case "output_too_large":
				status = http.StatusBadRequest
				code = "output_too_large"
			case "stream_error", "stream_incomplete":
				status = http.StatusBadGateway
				code = ae.Type
			}
		}
		if !streamHeadersSent.Load() {
			writeAPIError(w, status, "api_error", "Execution failed", code, nil, retry)
		}
		s.logOutcome(reqID, "POST", "/v1/chat/completions", status, code, startTime, req.Stream, &req.Model, queueWaitMS.Load(), clientClass)
		return
	}

	if !req.Stream {
		respObj := ChatCompletionResponse{
			ID:      "chatcmpl-" + reqID,
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []ChatCompletionChoice{
				{
					Index: 0,
					Message: ChatMessage{
						Role:    "assistant",
						Content: turnResp,
					},
					FinishReason: "stop",
				},
			},
			Usage: mapAgyUsageToOpenAI(turnUsage),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(respObj)
	}

	s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusOK, "", startTime, req.Stream, &req.Model, queueWaitMS.Load(), clientClass)
}
