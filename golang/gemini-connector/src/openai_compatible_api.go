package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultModelRefreshTTL      = 5 * time.Minute
	defaultModelRefreshTimeout  = 5 * time.Second
	defaultMaxRequestDuration   = 150 * time.Second
	defaultMaxExecutionDuration = 90 * time.Second
	defaultQueueWaitTimeout     = 30 * time.Second
	defaultMaxBodyBytes         = 1024 * 1024       // 1 MiB
	defaultMaxMessages          = 128
	defaultMaxMessageBytes      = 256 * 1024        // 256 KiB
	defaultMaxAggregateBytes    = 768 * 1024        // 768 KiB
	defaultMaxOutputBytes       = 4 * 1024 * 1024   // 4 MiB
	defaultHeartbeatInterval    = 15 * time.Second
)

type APIErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

type APIErrorResponse struct {
	Error APIErrorBody `json:"error"`
}

func writeAPIError(w http.ResponseWriter, status int, errType, msg, code string, param *string, retryAfter bool) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter {
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIErrorResponse{
		Error: APIErrorBody{
			Message: msg,
			Type:    errType,
			Param:   param,
			Code:    code,
		},
	})
}

func strPtr(s string) *string {
	return &s
}

type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelListResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

type ModelCatalog struct {
	mu             sync.RWMutex
	refreshMu      sync.Mutex
	models         []ModelInfo
	cachedAt       time.Time
	refreshTTL     time.Duration
	refreshTimeout time.Duration
	fetcher        func(ctx context.Context) ([]string, error)
}

func NewModelCatalog() *ModelCatalog {
	c := &ModelCatalog{
		refreshTTL:     defaultModelRefreshTTL,
		refreshTimeout: defaultModelRefreshTimeout,
	}
	c.fetcher = c.defaultFetchAgyModels
	return c
}

func (c *ModelCatalog) defaultFetchAgyModels(ctx context.Context) ([]string, error) {
	// Wait at most min(2000ms, ctx deadline) for the shared launch semaphore
	acquireTimeout := 2 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		rem := time.Until(dl)
		if rem < acquireTimeout {
			acquireTimeout = rem
		}
	}
	if !acquireAgyLaunch(ctx, acquireTimeout) {
		return nil, errors.New("launch lock acquisition timed out")
	}
	defer releaseAgyLaunch()

	fetchCtx, cancel := context.WithTimeout(ctx, c.refreshTimeout)
	defer cancel()

	cmd := exec.CommandContext(fetchCtx, "agy", "models")
	cmd.Env = agyEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var modelIDs []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "Fetching") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) > 0 {
			id := strings.TrimSpace(parts[0])
			if strings.HasPrefix(id, "gemini-") {
				modelIDs = append(modelIDs, id)
			}
		}
	}
	return modelIDs, nil
}

func (c *ModelCatalog) GetModels(ctx context.Context) ([]ModelInfo, error) {
	c.mu.RLock()
	if len(c.models) > 0 && time.Since(c.cachedAt) < c.refreshTTL {
		res := make([]ModelInfo, len(c.models))
		copy(res, c.models)
		c.mu.RUnlock()
		return res, nil
	}
	c.mu.RUnlock()

	// Perform single-flight serialized refresh
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// Double-check under lock
	c.mu.RLock()
	if len(c.models) > 0 && time.Since(c.cachedAt) < c.refreshTTL {
		res := make([]ModelInfo, len(c.models))
		copy(res, c.models)
		c.mu.RUnlock()
		return res, nil
	}
	c.mu.RUnlock()

	ids, err := c.fetcher(ctx)
	if err != nil {
		// If refresh failed but an older snapshot remains within TTL, serve it
		c.mu.RLock()
		if len(c.models) > 0 && time.Since(c.cachedAt) < c.refreshTTL {
			res := make([]ModelInfo, len(c.models))
			copy(res, c.models)
			c.mu.RUnlock()
			return res, nil
		}
		c.mu.RUnlock()
		return nil, err
	}

	now := time.Now()
	var newModels []ModelInfo
	for _, id := range ids {
		newModels = append(newModels, ModelInfo{
			ID:      id,
			Object:  "model",
			Created: now.Unix(),
			OwnedBy: "agy",
		})
	}

	c.mu.Lock()
	c.models = newModels
	c.cachedAt = now
	res := make([]ModelInfo, len(newModels))
	copy(res, newModels)
	c.mu.Unlock()

	return res, nil
}

func (c *ModelCatalog) ValidateModel(ctx context.Context, modelID string) (bool, error) {
	models, err := c.GetModels(ctx)
	if err != nil {
		return false, err
	}
	for _, m := range models {
		if m.ID == modelID {
			return true, nil
		}
	}
	return false, nil
}

func (c *ModelCatalog) CheckModelSnapshotValid(modelID string) (exists bool, expired bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if time.Since(c.cachedAt) >= c.refreshTTL || len(c.models) == 0 {
		return false, true
	}
	for _, m := range c.models {
		if m.ID == modelID {
			return true, false
		}
	}
	return false, false
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionRequest struct {
	Model               string        `json:"model"`
	Messages            []ChatMessage `json:"messages"`
	Stream              bool          `json:"stream"`
	Temperature         *float64      `json:"temperature,omitempty"`
	TopP                *float64      `json:"top_p,omitempty"`
	Stop                any           `json:"stop,omitempty"`
	PresencePenalty     *float64      `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64      `json:"frequency_penalty,omitempty"`
	Seed                *int          `json:"seed,omitempty"`
	User                *string       `json:"user,omitempty"`
	MaxCompletionTokens *int          `json:"max_completion_tokens,omitempty"`
}

type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []ChatCompletionChoice `json:"choices"`
}

type ChunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
}

type OpenAICompatibleServer struct {
	apiKeyHash   [32]byte
	catalog      *ModelCatalog
	turns        *TurnCoordinator
	logger       *APILogger
	draining     int32
	activeJobs   sync.WaitGroup
	rootCtx      context.Context
	cancelRoot   context.CancelFunc
}

func NewOpenAICompatibleServer(apiKey string, turns *TurnCoordinator, logger *APILogger) *OpenAICompatibleServer {
	ctx, cancel := context.WithCancel(context.Background())
	s := &OpenAICompatibleServer{
		apiKeyHash: sha256.Sum256([]byte(apiKey)),
		catalog:    NewModelCatalog(),
		turns:      turns,
		logger:     logger,
		rootCtx:    ctx,
		cancelRoot: cancel,
	}
	return s
}

func (s *OpenAICompatibleServer) Drain() {
	atomic.StoreInt32(&s.draining, 1)
	if s.cancelRoot != nil {
		s.cancelRoot()
	}
}

func generateServerRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func isLoopbackHost(host string) bool {
	h := host
	if idx := strings.IndexByte(h, ':'); idx != -1 {
		h = h[:idx]
	}
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	lower := strings.ToLower(origin)
	return strings.HasPrefix(lower, "http://127.0.0.1") ||
		strings.HasPrefix(lower, "https://127.0.0.1") ||
		strings.HasPrefix(lower, "http://localhost") ||
		strings.HasPrefix(lower, "https://localhost")
}

func (s *OpenAICompatibleServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := generateServerRequestID()
	w.Header().Set("X-Request-Id", reqID)

	clientClass := ClassifyClient(r.UserAgent())
	startTime := time.Now()

	// 1. Path and Method matching before authentication
	path := r.URL.Path
	if path != "/v1/models" && path != "/v1/chat/completions" {
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", "Unknown path: "+path, "resource_not_found", nil, false)
		s.logOutcome(reqID, r.Method, "unknown", http.StatusNotFound, "resource_not_found", startTime, false, nil, 0, clientClass)
		return
	}

	if path == "/v1/models" && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed: "+r.Method, "method_not_allowed", nil, false)
		s.logOutcome(reqID, r.Method, path, http.StatusMethodNotAllowed, "method_not_allowed", startTime, false, nil, 0, clientClass)
		return
	}

	if path == "/v1/chat/completions" && r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "Method not allowed: "+r.Method, "method_not_allowed", nil, false)
		s.logOutcome(reqID, r.Method, path, http.StatusMethodNotAllowed, "method_not_allowed", startTime, false, nil, 0, clientClass)
		return
	}

	// 2. Authentication before route-specific processing
	authHeader := r.Header.Get("Authorization")
	var reqKey string
	if strings.HasPrefix(authHeader, "Bearer ") {
		reqKey = strings.TrimPrefix(authHeader, "Bearer ")
	}
	reqKeyHash := sha256.Sum256([]byte(reqKey))
	if subtle.ConstantTimeCompare(reqKeyHash[:], s.apiKeyHash[:]) != 1 {
		writeAPIError(w, http.StatusUnauthorized, "invalid_request_error", "Invalid or missing API key", "invalid_api_key", nil, false)
		s.logOutcome(reqID, r.Method, path, http.StatusUnauthorized, "invalid_api_key", startTime, false, nil, 0, clientClass)
		return
	}

	// 3. Host and Origin validation (Host wins if both invalid)
	hostInvalid := !isLoopbackHost(r.Host)
	originInvalid := r.Header.Get("Origin") != "" && !isAllowedOrigin(r.Header.Get("Origin"))
	if hostInvalid {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid Host header: must be loopback", "invalid_host", strPtr("Host"), false)
		s.logOutcome(reqID, r.Method, path, http.StatusBadRequest, "invalid_host", startTime, false, nil, 0, clientClass)
		return
	}
	if originInvalid {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Disallowed cross-origin request", "disallowed_origin", strPtr("Origin"), false)
		s.logOutcome(reqID, r.Method, path, http.StatusBadRequest, "disallowed_origin", startTime, false, nil, 0, clientClass)
		return
	}

	// Route handling
	if path == "/v1/models" {
		s.handleModels(w, r, reqID, startTime, clientClass)
		return
	}

	s.handleChatCompletions(w, r, reqID, startTime, clientClass)
}

func (s *OpenAICompatibleServer) handleModels(w http.ResponseWriter, r *http.Request, reqID string, startTime time.Time, clientClass string) {
	models, err := s.catalog.GetModels(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "api_error", "Catalog unavailable", "catalog_unavailable", nil, true)
		s.logOutcome(reqID, "GET", "/v1/models", http.StatusServiceUnavailable, "catalog_unavailable", startTime, false, nil, 0, clientClass)
		return
	}

	resp := ModelListResponse{
		Object: "list",
		Data:   models,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
	s.logOutcome(reqID, "GET", "/v1/models", http.StatusOK, "", startTime, false, nil, 0, clientClass)
}

var rejectedFields = []string{
	"max_tokens", "metadata", "stream_options", "include_usage",
	"tools", "tool_choice", "functions", "function_call", "response_format",
	"logprobs", "top_logprobs", "parallel_tool_calls",
}

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
			msg := fmt.Sprintf("Field %q is not supported", field)
			if field == "max_tokens" {
				msg = "max_tokens is not supported; use max_completion_tokens"
			}
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", msg, "unsupported_field", strPtr(field), false)
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

	var req ChatCompletionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Failed to decode completion request", "invalid_request", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_request", startTime, false, nil, 0, clientClass)
		return
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
		case "developer", "system", "user", "assistant":
		default:
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("Unsupported role %q at messages[%d]", msg.Role, idx), "invalid_role", strPtr("role"), false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusBadRequest, "invalid_role", startTime, req.Stream, &req.Model, 0, clientClass)
			return
		}
		msgLen := len(msg.Content)
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

	// Convert messages to deterministic structured JSON
	promptBytes, err := json.Marshal(req.Messages)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "api_error", "Internal error encoding messages", "internal_error", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusInternalServerError, "internal_error", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}
	prompt := string(promptBytes)

	// Prepare queue admission
	queueStart := time.Now()
	var queueWaitDuration time.Duration

	jobDone := make(chan struct{})
	var turnErr error
	var turnResp string

	s.activeJobs.Add(1)
	defer s.activeJobs.Done()

	// Enqueue in TurnCoordinator
	_, submitErr := s.turns.SubmitAPI(reqCtx, func(jobCtx context.Context) {
		queueWaitDuration = time.Since(queueStart)
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

		// Execution context bounded by 90s execution cap
		execCtx, execCancel := context.WithTimeout(jobCtx, defaultMaxExecutionDuration)
		defer execCancel()

		if req.Stream {
			flusher, ok := w.(http.Flusher)
			if !ok {
				turnErr = errors.New("streaming unsupported by response writer")
				return
			}

			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()

			// Send first event: request_id preamble
			_, _ = fmt.Fprintf(w, "event: request_id\ndata: %s\n\n", reqID)
			flusher.Flush()

			created := time.Now().Unix()
			// Role chunk
			roleChunk := ChatCompletionChunk{
				ID:      "chatcmpl-" + reqID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   req.Model,
				Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{Role: "assistant"}, FinishReason: nil}},
			}
			roleBytes, _ := json.Marshal(roleChunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(roleBytes))
			flusher.Flush()

			// Heartbeat ticker
			stopHeartbeat := make(chan struct{})
			defer close(stopHeartbeat)
			go func() {
				ticker := time.NewTicker(defaultHeartbeatInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
						flusher.Flush()
					case <-stopHeartbeat:
						return
					case <-jobCtx.Done():
						return
					}
				}
			}()

			streamCb := func(delta string) error {
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
				if _, wErr := fmt.Fprintf(w, "data: %s\n\n", string(b)); wErr != nil {
					return wErr
				}
				flusher.Flush()
				return nil
			}

			_, streamErr := executeAgy(execCtx, prompt, "", AgyCallOptions{
				Profile:        ProfileAPI,
				Model:          req.Model,
				Stream:         true,
				StreamCallback: streamCb,
				Logger:         s.logger,
			})
			if streamErr != nil {
				// Send post-header error frame and close without finish/[DONE]
				errEvent := APIErrorResponse{
					Error: APIErrorBody{
						Message: "Upstream error during stream",
						Type:    "api_error",
						Code:    "upstream_stream_error",
					},
				}
				errBytes, _ := json.Marshal(errEvent)
				_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", string(errBytes))
				flusher.Flush()
				turnErr = streamErr
				return
			}

			// Terminal finish chunk + [DONE]
			stopStr := "stop"
			finishChunk := ChatCompletionChunk{
				ID:      "chatcmpl-" + reqID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   req.Model,
				Choices: []ChunkChoice{{Index: 0, Delta: ChunkDelta{}, FinishReason: &stopStr}},
			}
			fBytes, _ := json.Marshal(finishChunk)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(fBytes))
			_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
		} else {
			resp, nonStreamErr := executeAgy(execCtx, prompt, "", AgyCallOptions{
				Profile: ProfileAPI,
				Model:   req.Model,
				Stream:  false,
				Logger:  s.logger,
			})
			turnResp = resp
			turnErr = nonStreamErr
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
		// Completed
	case <-time.After(defaultQueueWaitTimeout):
		// If job has not completed within 30s queue wait, check if it was still in queue
		if queueWaitDuration == 0 {
			writeAPIError(w, http.StatusServiceUnavailable, "api_error", "Queue wait timeout", "queue_wait_timeout", nil, true)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusServiceUnavailable, "queue_wait_timeout", startTime, req.Stream, &req.Model, int64(defaultQueueWaitTimeout/time.Millisecond), clientClass)
			return
		}
		// If already running, wait for it up to reqCtx deadline
		select {
		case <-jobDone:
		case <-reqCtx.Done():
			writeAPIError(w, http.StatusGatewayTimeout, "api_error", "Request duration exceeded", "request_timeout", nil, false)
			s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusGatewayTimeout, "request_timeout", startTime, req.Stream, &req.Model, int64(queueWaitDuration/time.Millisecond), clientClass)
			return
		}
	case <-reqCtx.Done():
		writeAPIError(w, http.StatusGatewayTimeout, "api_error", "Request timeout", "request_timeout", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusGatewayTimeout, "request_timeout", startTime, req.Stream, &req.Model, 0, clientClass)
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
		if !req.Stream {
			writeAPIError(w, status, "api_error", "Execution failed", code, nil, retry)
		}
		s.logOutcome(reqID, "POST", "/v1/chat/completions", status, code, startTime, req.Stream, &req.Model, int64(queueWaitDuration/time.Millisecond), clientClass)
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
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(respObj)
	}

	s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusOK, "", startTime, req.Stream, &req.Model, int64(queueWaitDuration/time.Millisecond), clientClass)
}

func (s *OpenAICompatibleServer) logOutcome(reqID, method, route string, status int, code string, startTime time.Time, stream bool, model *string, queueWaitMS int64, clientClass string) {
	if s.logger == nil {
		return
	}
	s.logger.Log(APILogRecord{
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		RequestID:   reqID,
		Method:      method,
		Route:       route,
		Status:      status,
		Code:        code,
		DurationMS:  time.Since(startTime).Milliseconds(),
		Stream:      stream,
		Model:       model,
		QueueWaitMS: queueWaitMS,
		ClientClass: clientClass,
	})
}
