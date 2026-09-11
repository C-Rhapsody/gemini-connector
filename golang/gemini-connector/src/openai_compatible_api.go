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
	defaultModelRefreshTimeout  = 20 * time.Second
	defaultMaxRequestDuration   = 150 * time.Second
	defaultMaxExecutionDuration = 90 * time.Second
	defaultQueueWaitTimeout     = 30 * time.Second
	defaultMaxBodyBytes         = 1024 * 1024 // 1 MiB
	defaultMaxMessages          = 1024
	defaultMaxMessageBytes      = 256 * 1024      // 256 KiB
	defaultMaxAggregateBytes    = 768 * 1024      // 768 KiB
	defaultMaxOutputBytes       = 4 * 1024 * 1024 // 4 MiB
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
	// Wait at most min(10000ms, ctx deadline) for the shared launch semaphore
	acquireTimeout := 10 * time.Second
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

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
}

type ChatMessage struct {
	Role         string     `json:"role"`
	Content      any        `json:"content"`
	Name         string     `json:"name,omitempty"`
	ToolCallID   string     `json:"tool_call_id,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FunctionCall any        `json:"function_call,omitempty"`
}

type FunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ToolDefinition struct {
	Type     string              `json:"type"`
	Function *FunctionDefinition `json:"function,omitempty"`
}

type ResponseFormatJSONSchema struct {
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ResponseFormat struct {
	Type       string                    `json:"type"`
	JSONSchema *ResponseFormatJSONSchema `json:"json_schema,omitempty"`
	Schema     json.RawMessage           `json:"schema,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type UsageInfo struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

func mapAgyUsageToOpenAI(u *AgyUsage) *UsageInfo {
	if u == nil {
		return nil
	}
	ui := &UsageInfo{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.CacheReadTokens > 0 {
		ui.PromptTokensDetails = &PromptTokensDetails{
			CachedTokens: u.CacheReadTokens,
		}
	}
	if u.ThinkingTokens > 0 {
		ui.CompletionTokensDetails = &CompletionTokensDetails{
			ReasoningTokens: u.ThinkingTokens,
		}
	}
	return ui
}

func parseStopSequences(stop any) []string {
	if stop == nil {
		return nil
	}
	switch s := stop.(type) {
	case string:
		if s != "" {
			return []string{s}
		}
	case []any:
		var res []string
		for _, item := range s {
			if str, ok := item.(string); ok && str != "" {
				res = append(res, str)
			}
		}
		return res
	case []string:
		return s
	}
	return nil
}

func applyStopSequences(text string, stopSeqs []string) (string, bool) {
	if len(stopSeqs) == 0 {
		return text, false
	}
	earliestIdx := -1
	for _, seq := range stopSeqs {
		idx := strings.Index(text, seq)
		if idx != -1 {
			if earliestIdx == -1 || idx < earliestIdx {
				earliestIdx = idx
			}
		}
	}
	if earliestIdx != -1 {
		return text[:earliestIdx], true
	}
	return text, false
}

func stringifyMessageContent(content any) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

type ChatCompletionRequest struct {
	Model               string               `json:"model"`
	Messages            []ChatMessage        `json:"messages"`
	Stream              bool                 `json:"stream"`
	Temperature         *float64             `json:"temperature,omitempty"`
	TopP                *float64             `json:"top_p,omitempty"`
	Stop                any                  `json:"stop,omitempty"`
	PresencePenalty     *float64             `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64             `json:"frequency_penalty,omitempty"`
	Seed                *int                 `json:"seed,omitempty"`
	User                *string              `json:"user,omitempty"`
	MaxTokens           *int                 `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                 `json:"max_completion_tokens,omitempty"`
	ResponseFormat      *ResponseFormat      `json:"response_format,omitempty"`
	Tools               []ToolDefinition     `json:"tools,omitempty"`
	ToolChoice          any                  `json:"tool_choice,omitempty"`
	Functions           []FunctionDefinition `json:"functions,omitempty"`
	FunctionCall        any                  `json:"function_call,omitempty"`
	ParallelToolCalls   *bool                `json:"parallel_tool_calls,omitempty"`
	StreamOptions       *StreamOptions       `json:"stream_options,omitempty"`
	IncludeUsage        *bool                `json:"include_usage,omitempty"`
	Metadata            any                  `json:"metadata,omitempty"`
	Logprobs            *bool                `json:"logprobs,omitempty"`
	TopLogprobs         *int                 `json:"top_logprobs,omitempty"`
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
	Usage   *UsageInfo             `json:"usage,omitempty"`
}

type ChunkDelta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
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
	Usage   *UsageInfo    `json:"usage,omitempty"`
}

type OpenAICompatibleServer struct {
	apiKeyHash        [32]byte
	catalog           *ModelCatalog
	turns             *TurnCoordinator
	logger            *APILogger
	draining          int32
	activeJobs        sync.WaitGroup
	rootCtx           context.Context
	cancelRoot        context.CancelFunc
	heartbeatInterval time.Duration
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

var rejectedFields = []string{}

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

	// Convert messages to deterministic structured JSON
	promptBytes, err := json.Marshal(req.Messages)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "api_error", "Internal error encoding messages", "internal_error", nil, false)
		s.logOutcome(reqID, "POST", "/v1/chat/completions", http.StatusInternalServerError, "internal_error", startTime, req.Stream, &req.Model, 0, clientClass)
		return
	}
	prompt := string(promptBytes)

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
			_, streamErr := executeAgy(execCtx, prompt, "", AgyCallOptions{
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
			resp, nonStreamErr := executeAgy(execCtx, prompt, "", AgyCallOptions{
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
