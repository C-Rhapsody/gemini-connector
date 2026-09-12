package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
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
	// Usage is a per-request anchor for Hermes. Values beyond this bound are
	// treated as cumulative/corrupt telemetry, never clamped into a false anchor.
	maxReportedUsageTokens = 16 * 1024 * 1024
)

var invalidRemoteUsageCount atomic.Uint64

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
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
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

func isSaneAgyUsage(u *AgyUsage) bool {
	if u == nil {
		return false
	}
	for _, value := range []int{u.InputTokens, u.OutputTokens, u.ThinkingTokens, u.CacheReadTokens, u.TotalTokens} {
		if value < 0 || value > maxReportedUsageTokens {
			return false
		}
	}
	if u.TotalTokens > 0 && u.TotalTokens < u.InputTokens+u.OutputTokens {
		return false
	}
	return true
}

func mapAgyUsageToOpenAI(u *AgyUsage) *UsageInfo {
	if u == nil {
		return nil
	}
	if !isSaneAgyUsage(u) {
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

func mapAgyUsageToOpenAIRemote(u *AgyUsage) *UsageInfo {
	if u != nil && !isSaneAgyUsage(u) {
		invalidRemoteUsageCount.Add(1)
		log.Printf("[Usage] ignored invalid AGY usage for remote-history route")
		return nil
	}
	return mapAgyUsageToOpenAI(u)
}
func mapAgyUsageForRequest(u *AgyUsage, remoteHistory bool) *UsageInfo {
	if remoteHistory {
		return mapAgyUsageToOpenAIRemote(u)
	}
	return mapAgyUsageToOpenAI(u)
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

type GeminiConnectorExtension struct {
	State     string `json:"state"`
	Retryable bool   `json:"retryable"`
}

type ToolActivityState string

const (
	ToolActivityUnknown  ToolActivityState = "unknown"
	ToolActivityObserved ToolActivityState = "observed"
)

func classifyToolActivity(metadata any) ToolActivityState {
	if metadata == nil {
		return ToolActivityUnknown
	}
	switch m := metadata.(type) {
	case map[string]any:
		if len(m) == 0 {
			return ToolActivityUnknown
		}
		if tools, ok := m["tools"]; ok && tools != nil {
			if slice, ok := tools.([]any); ok && len(slice) > 0 {
				return ToolActivityObserved
			}
		}
		if toolCalls, ok := m["tool_calls"]; ok && toolCalls != nil {
			if slice, ok := toolCalls.([]any); ok && len(slice) > 0 {
				return ToolActivityObserved
			}
		}
		if executed, ok := m["tools_executed"]; ok && executed != nil {
			if b, ok := executed.(bool); ok && b {
				return ToolActivityObserved
			}
		}
		return ToolActivityUnknown
	default:
		return ToolActivityUnknown
	}
}

type ChatCompletionResponse struct {
	ID               string                    `json:"id"`
	Object           string                    `json:"object"`
	Created          int64                     `json:"created"`
	Model            string                    `json:"model"`
	Choices          []ChatCompletionChoice    `json:"choices"`
	Usage            *UsageInfo                `json:"usage,omitempty"`
	XGeminiConnector *GeminiConnectorExtension `json:"x_gemini_connector,omitempty"`
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
	ID               string                    `json:"id"`
	Object           string                    `json:"object"`
	Created          int64                     `json:"created"`
	Model            string                    `json:"model"`
	Choices          []ChunkChoice             `json:"choices"`
	Usage            *UsageInfo                `json:"usage,omitempty"`
	XGeminiConnector *GeminiConnectorExtension `json:"x_gemini_connector,omitempty"`
}

type OpenAICompatibleServer struct {
	apiKeyHash        [32]byte
	catalog           *ModelCatalog
	turns             *TurnCoordinator
	logger            *APILogger
	executor          AgyExecutor
	draining          int32
	activeJobs        sync.WaitGroup
	rootCtx           context.Context
	cancelRoot        context.CancelFunc
	heartbeatInterval time.Duration
	convIDProvider    func() string
	syncTracker       *SessionSyncTracker
	remoteReplayMu    sync.Mutex
	remoteReplay      *RemoteReplayLedger
}

func NewOpenAICompatibleServer(apiKey string, turns *TurnCoordinator, logger *APILogger, convIDProvider ...func() string) *OpenAICompatibleServer {
	ctx, cancel := context.WithCancel(context.Background())
	executor := newAgyExecutor()
	s := &OpenAICompatibleServer{
		apiKeyHash:   sha256.Sum256([]byte(apiKey)),
		catalog:      NewModelCatalog(),
		turns:        turns,
		logger:       logger,
		executor:     executor,
		rootCtx:      ctx,
		cancelRoot:   cancel,
		syncTracker:  NewSessionSyncTracker(""),
		remoteReplay: NewRemoteReplayLedger(),
	}
	if len(convIDProvider) > 0 && convIDProvider[0] != nil {
		s.convIDProvider = convIDProvider[0]
	}
	return s
}

func (s *OpenAICompatibleServer) getConversationID() string {
	if s.convIDProvider != nil {
		return s.convIDProvider()
	}
	return ""
}

func (s *OpenAICompatibleServer) SetConversationIDProvider(provider func() string) {
	s.convIDProvider = provider
}

func (s *OpenAICompatibleServer) getRemoteReplayLedger() *RemoteReplayLedger {
	s.remoteReplayMu.Lock()
	defer s.remoteReplayMu.Unlock()
	if s.remoteReplay == nil {
		s.remoteReplay = NewRemoteReplayLedger()
	}
	return s.remoteReplay
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
	if !validBearerToken(r.Header.Get("Authorization"), s.apiKeyHash) {
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

var rejectedFields = []string{}

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
