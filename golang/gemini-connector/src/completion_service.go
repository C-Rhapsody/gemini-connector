package main

import (
	"context"
	"fmt"
	"strings"
)

// CompletionService orchestrates standard chat completion execution without
// coupling to shared messenger conversation sessions, sync trackers, or custom replay ledgers.
// The incoming messages array is the authoritative, self-contained conversation context.
type CompletionService struct {
	executor AgyExecutor
	logger   *APILogger
}

// NewCompletionService creates a new CompletionService instance.
func NewCompletionService(executor AgyExecutor, logger *APILogger) *CompletionService {
	return &CompletionService{
		executor: executor,
		logger:   logger,
	}
}

// Execute orchestrates the prompt rendering and AGY invocation for a normalized request.
func (s *CompletionService) Execute(
	ctx context.Context,
	req *NormalizedCompletionRequest,
	streamCb func(string) error,
	usageCb func(*AgyUsage),
) (*CompletionResult, error) {
	prompt, err := RenderPromptWithTools(req.Messages, req.Tools, req.ToolChoice)
	if err != nil {
		return nil, fmt.Errorf("failed to render prompt: %w", err)
	}

	schemaToPass := ""
	structuredRequested := len(req.Tools) > 0 || req.ResponseFormat != nil
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
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
		}
	}

	opts := AgyCallOptions{
		Profile:                   ProfileAPI,
		Model:                     req.Model,
		Stream:                    req.Stream,
		StreamCallback:            streamCb,
		Logger:                    s.logger,
		JSONSchema:                schemaToPass,
		StructuredOutputRequested: structuredRequested,
		Tools:                     req.Tools,
		ToolChoice:                req.ToolChoice,
		Stop:                      req.Stop,
		UsageCallback:             usageCb,
	}

	// Standard chat completions run with empty conversationID (isolated turn)
	return s.executor.Execute(ctx, prompt, "", opts)
}
