package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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

	normReq, valErr := ParseAndNormalizeRequest(bodyBytes)
	if valErr != nil {
		writeAPIError(w, valErr.Status, valErr.ErrType, valErr.Message, valErr.Code, valErr.Param, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", valErr.Status, valErr.Code, startTime, false, nil, 0, clientClass)
		return
	}

	// Validate response_format and extract schema if requested
	var schemaToPass string
	if normReq.ResponseFormat != nil {
		switch normReq.ResponseFormat.Type {
		case "", "text":
			// plain text
		case "json_object":
			schemaToPass = `{"type":"object"}`
		case "json_schema":
			if normReq.ResponseFormat.JSONSchema != nil && len(normReq.ResponseFormat.JSONSchema.Schema) > 0 {
				schemaToPass = strings.TrimSpace(string(normReq.ResponseFormat.JSONSchema.Schema))
			} else if len(normReq.ResponseFormat.Schema) > 0 {
				schemaToPass = strings.TrimSpace(string(normReq.ResponseFormat.Schema))
			} else {
				schemaToPass = `{"type":"object"}`
			}
		}
	}

	// Model validation
	if normReq.Model == "" || !strings.HasPrefix(normReq.Model, "gemini-") {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "A valid Gemini model ID is required", "model_not_found", strPtr("model"), false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "model_not_found", startTime, normReq.Stream, nil, 0, clientClass)
		return
	}

	ok, mValErr := s.catalog.ValidateModel(reqCtx, normReq.Model)
	if mValErr != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "api_error", "Catalog unavailable", "catalog_unavailable", nil, true)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusServiceUnavailable, "catalog_unavailable", startTime, normReq.Stream, &normReq.Model, 0, clientClass)
		return
	}
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Model %q not found in catalog", normReq.Model), "model_not_found", strPtr("model"), false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "model_not_found", startTime, normReq.Stream, &normReq.Model, 0, clientClass)
		return
	}

	// Drain check
	if atomic.LoadInt32(&s.draining) == 1 {
		writeAPIError(w, http.StatusServiceUnavailable, "api_error", "Server is shutting down", "server_draining", nil, true)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusServiceUnavailable, "server_draining", startTime, normReq.Stream, &normReq.Model, 0, clientClass)
		return
	}

	req := ChatCompletionRequest{
		Model:               normReq.Model,
		Messages:            normReq.Messages,
		Stream:              normReq.Stream,
		Tools:               normReq.Tools,
		ToolChoice:          normReq.ToolChoice,
		ResponseFormat:      normReq.ResponseFormat,
		Stop:                normReq.Stop,
		MaxCompletionTokens: normReq.MaxCompletionTokens,
		StreamOptions:       normReq.StreamOptions,
		Temperature:         normReq.Temperature,
		TopP:                normReq.TopP,
		Seed:                normReq.Seed,
		PresencePenalty:     normReq.PresencePenalty,
		FrequencyPenalty:    normReq.FrequencyPenalty,
		User:                normReq.User,
		Metadata:            normReq.Metadata,
	}

	hasTools := len(req.Tools) > 0
	parsedToolChoice := normReq.ToolChoice

	remoteHistory, remoteHistoryErr := ParseRemoteHistoryRequest(r.Header)
	if remoteHistoryErr != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", remoteHistoryErr.Error(), "invalid_history_protocol", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_history_protocol", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}

	convID := s.getConversationID()
	if remoteHistory.Enabled && convID == "" {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "Missing AGY_CONVERSATION_ID configuration for remote history", "missing_conversation_id", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusInternalServerError, "missing_conversation_id", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}
	var prompt string
	var renderErr error
	var isResumeTurn bool

	if remoteHistory.Enabled {
		systemMsgs, currentTurnMsgs, splitErr := SplitRemoteHistoryMessages(req.Messages, remoteHistory.Sequence)
		if splitErr != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", splitErr.Error(), "invalid_history_protocol", nil, false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_history_protocol", startTime, req.Stream, &req.Model, 0, clientClass)
			return
		}
		isResumeTurn = true
		prompt, renderErr = RenderRemoteHistoryPrompt(systemMsgs, currentTurnMsgs, req.Tools, req.ToolChoice, remoteHistory.Sequence)
	} else if convID != "" && s.syncTracker != nil && s.syncTracker.IsSessionSynced(convID) {
		systemMsgs, _, currentTurnMsgs := ExtractCurrentTurn(req.Messages)
		isResumeTurn = true
		prompt, renderErr = RenderTurnPromptWithTools(systemMsgs, currentTurnMsgs, req.Tools, req.ToolChoice)
	} else {
		// Standard chat completions: full messages array is authoritative context!
		prompt, renderErr = RenderPromptWithTools(req.Messages, req.Tools, req.ToolChoice)
	}

	if renderErr != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Failed to render prompt: %v", renderErr), "invalid_prompt", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_prompt", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}

	stopSeqs := parseStopSequences(req.Stop)
	responseFormatType := ""
	if req.ResponseFormat != nil {
		responseFormatType = req.ResponseFormat.Type
	}
	structuredOutputRequested := schemaToPass != "" || req.ResponseFormat != nil || hasTools
	replayFingerprint := prompt
	if remoteHistory.Enabled {
		replayFingerprint = remoteReplayFingerprint(prompt, req.Model, schemaToPass, stopSeqs, responseFormatType, structuredOutputRequested)
	}
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
	var turnResult *CompletionResult
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

		var replayResult *RemoteReplayResult
		remoteReplayOwner := false
		if remoteHistory.Explicit {
			var replayErr error
			replayResult, remoteReplayOwner, replayErr = s.getRemoteReplayLedger().Acquire(convID, remoteHistory, replayFingerprint)
			if replayErr != nil {
				replayType := "remote_request_capacity"
				if errors.Is(replayErr, ErrRemoteReplayInFlight) {
					replayType = "remote_request_in_progress"
				}
				turnErr = &AgyError{Type: replayType, Detail: replayErr.Error()}
				return
			}
		}

		var replayFinalized bool
		if remoteReplayOwner {
			defer func() {
				if !replayFinalized {
					s.getRemoteReplayLedger().Abort(convID, remoteHistory, replayFingerprint)
				}
			}()
		}

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
			flushingWriter := &FlushingWriter{
				W:       w,
				Flusher: safeFlush,
				Mu:      &writeMu,
				IsDropped: func() bool {
					return jobCtx.Err() != nil || reqCtx.Err() != nil
				},
			}
			writeRaw := func(format string, args ...any) error {
				_, err := fmt.Fprintf(flushingWriter, format, args...)
				return err
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
			streamWriter := NewStreamWriter(flushingWriter, reqID, req.Model, created)
			if err := streamWriter.WriteRole(); err != nil {
				turnErr = err
				return
			}

			var cumText strings.Builder
			if replayResult != nil {
				cumText.WriteString(replayResult.Response)
			}
			stopFilter := NewStopFilter(stopSeqs)
			streamCb := func(delta string) error {
				if jobCtx.Err() != nil || reqCtx.Err() != nil {
					return jobCtx.Err()
				}
				cumText.WriteString(delta)

				if hasTools {
					// In tool-call mode, do NOT leak raw structured envelope deltas into content!
					return nil
				}

				if stopFilter.IsStopped() {
					return nil
				}

				toEmit, _ := stopFilter.Feed(delta)
				if toEmit != "" {
					return streamWriter.WriteContentDelta(toEmit)
				}
				return nil
			}

			var actualUsage *AgyUsage
			var streamErr error
			if replayResult != nil {
				actualUsage = cloneAgyUsage(replayResult.Usage)
				turnResult = &CompletionResult{Status: "SUCCESS", Text: replayResult.Response, Usage: actualUsage}
			} else {
				turnResult, streamErr = s.executor.Execute(execCtx, prompt, convID, AgyCallOptions{
					Profile:                   ProfileAPI,
					Model:                     req.Model,
					Stream:                    true,
					StreamCallback:            streamCb,
					Logger:                    s.logger,
					JSONSchema:                schemaToPass,
					StructuredOutputRequested: structuredOutputRequested,
					UsageCallback: func(u *AgyUsage) {
						actualUsage = u
					},
					Tools:      req.Tools,
					ToolChoice: parsedToolChoice,
					Stop:       stopSeqs,
				})
			}
			if remoteReplayOwner && streamErr != nil {
				s.getRemoteReplayLedger().Abort(convID, remoteHistory, replayFingerprint)
				replayFinalized = true
			}
			if streamErr != nil && isResumeTurn && shouldFallbackAfterRemoteResumeFailure(remoteHistory.Enabled, streamErr) && !streamHeadersSent.Load() {
				var ae *AgyError
				if errors.As(streamErr, &ae) && ae.Type == "session_resume_failed" {
					log.Printf("[Compaction Fallback] session resume failed for %s, falling back to structured compaction", reqID)
					if s.syncTracker != nil {
						s.syncTracker.ResetSession(convID)
					}
					fallbackPrompt, _, _, fbErr := CompactPromptWithTools(req.Messages, req.Tools, req.ToolChoice, DefaultCompactionBudget)
					if fbErr == nil {
						turnResult, streamErr = s.executor.Execute(execCtx, fallbackPrompt, convID, AgyCallOptions{
							Profile:                   ProfileAPI,
							Model:                     req.Model,
							Stream:                    true,
							StreamCallback:            streamCb,
							Logger:                    s.logger,
							JSONSchema:                schemaToPass,
							StructuredOutputRequested: structuredOutputRequested,
							UsageCallback: func(u *AgyUsage) {
								actualUsage = u
							},
							Tools:      req.Tools,
							ToolChoice: parsedToolChoice,
							Stop:       stopSeqs,
						})
					}
				}
			}
			if streamErr == nil && s.syncTracker != nil && remoteHistory.Enabled && convID != "" {
				s.syncTracker.MarkSessionSynced(convID, "", len(req.Messages))
			}
			if turnResult != nil && turnResult.Usage != nil && actualUsage == nil {
				actualUsage = turnResult.Usage
			}
			turnErr = streamErr
			if streamErr == nil && turnResult != nil && turnResult.IsError {
				turnErr = &AgyError{Type: turnResult.ErrorCode, Detail: turnResult.ErrorMessage}
			}
			if turnErr == nil {
				turnUsage = actualUsage
			}
			if streamErr != nil {
				// If the stream ended because the client disconnected or context timed out/canceled,
				// do NOT attempt to write error frames or flush to the response writer!
				if jobCtx.Err() != nil || reqCtx.Err() != nil || errors.Is(streamErr, context.Canceled) {
					turnErr = streamErr
					return
				}
				// Send post-header error frame and close without finish/[DONE]
				errCode := "upstream_stream_error"
				if ae, ok := streamErr.(*AgyError); ok && ae.Type != "" {
					errCode = ae.Type
				}
				_ = streamWriter.WriteError("Upstream error during stream", errCode)
				turnErr = streamErr
				return
			}

			if hasTools {
				if turnResult != nil && len(turnResult.ToolCalls) > 0 {
					if err := streamWriter.WriteToolCallIntent(turnResult.ToolCalls); err != nil {
						turnErr = err
						return
					}
					if err := streamWriter.WriteFinish("tool_calls"); err != nil {
						turnErr = err
						return
					}
				} else {
					contentStr := ""
					if turnResult != nil {
						contentStr = turnResult.Text
					} else {
						contentStr = cumText.String()
					}
					if contentStr != "" {
						if err := streamWriter.WriteContentDelta(contentStr); err != nil {
							turnErr = err
							return
						}
					}
					if err := streamWriter.WriteFinish("stop"); err != nil {
						turnErr = err
						return
					}
				}
			} else {
				// Flush any held back suffix from StopFilter
				if remaining := stopFilter.Flush(); remaining != "" {
					_ = streamWriter.WriteContentDelta(remaining)
				}
				if replayResult != nil && cumText.Len() > 0 {
					content := cumText.String()
					if len(stopSeqs) > 0 {
						content, _ = applyStopSequences(content, stopSeqs)
					}
					if content != "" {
						_ = streamWriter.WriteContentDelta(content)
					}
				}
				_ = streamWriter.WriteFinish("stop")
			}

			if remoteReplayOwner {
				s.getRemoteReplayLedger().Complete(convID, remoteHistory, replayFingerprint, cumText.String(), actualUsage)
				replayFinalized = true
			}

			// Usage chunk before [DONE] if requested and available
			if wantUsage && actualUsage != nil {
				mappedUsage := mapAgyUsageForRequest(actualUsage, remoteHistory.Enabled)
				if mappedUsage != nil {
					_ = streamWriter.WriteUsage(mappedUsage)
				}
			}

			_ = streamWriter.WriteDone()
		} else {
			var actualUsage *AgyUsage
			var resp string
			var compResult *CompletionResult
			var nonStreamErr error
			if replayResult != nil {
				resp = replayResult.Response
				actualUsage = cloneAgyUsage(replayResult.Usage)
				compResult = &CompletionResult{Status: "SUCCESS", Text: resp, Usage: actualUsage}
			} else {
				compResult, nonStreamErr = s.executor.Execute(execCtx, prompt, convID, AgyCallOptions{
					Profile:                   ProfileAPI,
					Model:                     req.Model,
					Stream:                    false,
					Logger:                    s.logger,
					JSONSchema:                schemaToPass,
					StructuredOutputRequested: structuredOutputRequested,
					UsageCallback: func(u *AgyUsage) {
						actualUsage = u
					},
					Tools:      req.Tools,
					ToolChoice: parsedToolChoice,
					Stop:       stopSeqs,
				})
				if compResult != nil {
					resp = compResult.Text
					if compResult.Usage != nil && actualUsage == nil {
						actualUsage = compResult.Usage
					}
				}
			}
			if remoteReplayOwner {
				if nonStreamErr != nil || (compResult != nil && compResult.IsError) {
					s.getRemoteReplayLedger().Abort(convID, remoteHistory, replayFingerprint)
					replayFinalized = true
				} else if compResult != nil {
					s.getRemoteReplayLedger().Complete(convID, remoteHistory, replayFingerprint, resp, actualUsage)
					replayFinalized = true
				}
			}
			if nonStreamErr != nil && isResumeTurn && shouldFallbackAfterRemoteResumeFailure(remoteHistory.Enabled, nonStreamErr) {
				var ae *AgyError
				if errors.As(nonStreamErr, &ae) && ae.Type == "session_resume_failed" {
					log.Printf("[Compaction Fallback] session resume failed for %s, falling back to structured compaction", reqID)
					if s.syncTracker != nil && convID != "" {
						s.syncTracker.ResetSession(convID)
					}
					fallbackPrompt, _, _, fbErr := CompactPromptWithTools(req.Messages, req.Tools, req.ToolChoice, DefaultCompactionBudget)
					if fbErr == nil {
						compResult, nonStreamErr = s.executor.Execute(execCtx, fallbackPrompt, convID, AgyCallOptions{
							Profile:                   ProfileAPI,
							Model:                     req.Model,
							Stream:                    false,
							Logger:                    s.logger,
							JSONSchema:                schemaToPass,
							StructuredOutputRequested: structuredOutputRequested,
							UsageCallback: func(u *AgyUsage) {
								actualUsage = u
							},
							Tools:      req.Tools,
							ToolChoice: parsedToolChoice,
							Stop:       stopSeqs,
						})
						if compResult != nil {
							resp = compResult.Text
							if compResult.Usage != nil && actualUsage == nil {
								actualUsage = compResult.Usage
							}
						}
					}
				}
			}
			if nonStreamErr == nil && s.syncTracker != nil && remoteHistory.Enabled && convID != "" {
				s.syncTracker.MarkSessionSynced(convID, "", len(req.Messages))
			}
			turnResp = resp
			turnResult = compResult
			turnErr = nonStreamErr
			if nonStreamErr == nil && compResult != nil {
				if compResult.IsError {
					turnErr = &AgyError{Type: compResult.ErrorCode, Detail: compResult.ErrorMessage}
				}
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
		var ae *AgyError
		isAgyErr := errors.As(turnErr, &ae)
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
			case "native_tool_containment_violation":
				status = http.StatusBadRequest
				code = "native_tool_containment_violation"
			case "invalid_tool_envelope":
				status = http.StatusBadRequest
				code = "invalid_tool_envelope"
			case "upstream_no_output":
				status = http.StatusBadGateway
				code = "upstream_no_output"
			case "remote_request_in_progress":
				status = http.StatusServiceUnavailable
				code = "remote_request_in_progress"
				retry = true
			case "remote_request_capacity":
				status = http.StatusServiceUnavailable
				code = "remote_request_capacity"
				retry = true
			}
		}
		if turnResult != nil && turnResult.HTTPStatus != 0 {
			status = turnResult.HTTPStatus
		}
		if !streamHeadersSent.Load() {
			writeAPIError(w, status, "api_error", "Execution failed", code, nil, retry)
		}
		s.logOutcome(reqID, "POST", "/v1/chat/completions", status, code, startTime, req.Stream, &req.Model, queueWaitMS.Load(), clientClass)
		return
	}

	if !req.Stream {
		if turnResult == nil {
			turnResult = &CompletionResult{
				Text:         turnResp,
				Usage:        turnUsage,
				FinishReason: "stop",
			}
		}
		if turnResult.Usage == nil && turnUsage != nil {
			turnResult.Usage = turnUsage
		}
		_ = WriteNonStreamResponse(w, reqID, req.Model, turnResult, remoteHistory.Enabled)
	}

	s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusOK, "", startTime, req.Stream, &req.Model, queueWaitMS.Load(), clientClass)
}
