package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// CompletionResult encapsulates the unified execution outcome of an AGY invocation.
// It normalizes outputs from both non-streaming (JSON) and streaming (NDJSON) runs,
// distinguishing native tool execution, client tool intents, and plain assistant text.
type CompletionResult struct {
	// Text is the final assistant message content.
	Text string

	// ToolCalls contains unexecuted client tool call intents.
	ToolCalls []ToolCall

	// SawNativeTool is true if AGY executed tools natively in its local environment.
	SawNativeTool bool

	// HasTerminal is true if a terminal result event was received.
	HasTerminal bool

	// TerminalResponse is the raw response string from result.response.
	TerminalResponse string

	// TerminalStructuredOutput contains raw JSON from result.structured_output.
	TerminalStructuredOutput []byte

	// Usage contains token counts reported by AGY.
	Usage *AgyUsage

	// ConversationID is the AGY session ID for the run.
	ConversationID string

	// Status is the raw upstream status ("SUCCESS", "ERROR", etc.).
	Status string

	// FinishReason is the OpenAI finish reason ("stop", "tool_calls", "length").
	FinishReason string

	// WasTruncatedByStop is true if stop sequence matching truncated the text.
	WasTruncatedByStop bool

	// IsError indicates execution or validation failure.
	IsError bool

	// ErrorCode is the machine-readable error code (e.g. "upstream_no_output").
	ErrorCode string

	// ErrorMessage is the human-readable error detail.
	ErrorMessage string

	// HTTPStatus is the recommended HTTP response code (400, 500, 502, 503, etc.).
	HTTPStatus int
}

// BuildCompletionResult evaluates terminal output, intermediate deltas, and native tool execution
// to produce an authoritative CompletionResult adhering to the standard API contract.
func BuildCompletionResult(
	status string,
	termResp string,
	termStructuredOutput []byte,
	steps []AgyStep,
	accumulatedDeltas string,
	hasTerminal bool,
	sawNativeTool bool,
	usage *AgyUsage,
	convID string,
	tools []ToolDefinition,
	toolChoice ParsedToolChoice,
	stopSeqs []string,
) *CompletionResult {
	hasTools := len(tools) > 0
	res := &CompletionResult{
		Status:                   status,
		HasTerminal:              hasTerminal,
		SawNativeTool:            sawNativeTool,
		TerminalResponse:         termResp,
		TerminalStructuredOutput: termStructuredOutput,
		Usage:                    usage,
		ConversationID:           convID,
		FinishReason:             "stop",
	}

	// Check if any steps indicate native tool execution
	for _, st := range steps {
		if isNativeToolStepType(st.StepType) {
			res.SawNativeTool = true
			break
		}
	}

	if !hasTerminal {
		res.IsError = true
		res.ErrorCode = "stream_incomplete"
		res.ErrorMessage = "AGY execution ended without terminal result"
		res.HTTPStatus = 502
		return res
	}

	if status != "SUCCESS" && status != "" {
		res.IsError = true
		res.ErrorCode = "upstream_error"
		res.ErrorMessage = fmt.Sprintf("AGY returned non-success status: %s", status)
		res.HTTPStatus = 500
		return res
	}

	// Select authoritative candidate text / structured output
	candidate := ""
	if len(termStructuredOutput) > 0 {
		candidate = strings.TrimSpace(string(termStructuredOutput))
	} else if termResp != "" {
		candidate = termResp
	} else if accumulatedDeltas != "" {
		candidate = accumulatedDeltas
	}

	// If native tools executed:
	// Any non-empty response is the final text response from the native tool execution.
	// We NEVER return client tool calls for already-executed native tools.
	if res.SawNativeTool {
		if candidate == "" {
			res.IsError = true
			res.ErrorCode = "native_tool_containment_violation"
			res.ErrorMessage = "Native tool execution produced no final response text"
			res.HTTPStatus = 400
			return res
		}

		finalText := candidate
		if len(stopSeqs) > 0 {
			truncated, matched := applyStopSequences(finalText, stopSeqs)
			if matched {
				res.WasTruncatedByStop = true
				finalText = truncated
			}
		}
		res.Text = finalText
		res.FinishReason = "stop"
		return res
	}

	// If no tools in request:
	if !hasTools {
		if candidate == "" {
			res.IsError = true
			res.ErrorCode = "upstream_no_output"
			res.ErrorMessage = "Upstream model produced no output"
			res.HTTPStatus = 502
			return res
		}

		finalText := candidate
		if len(stopSeqs) > 0 {
			truncated, matched := applyStopSequences(finalText, stopSeqs)
			if matched {
				res.WasTruncatedByStop = true
				finalText = truncated
			}
		}
		res.Text = finalText
		res.FinishReason = "stop"
		return res
	}

	// When tools are provided, try parsing tool envelope
	if candidate == "" {
		res.IsError = true
		res.ErrorCode = "upstream_no_output"
		res.ErrorMessage = "Upstream model produced no output"
		res.HTTPStatus = 502
		return res
	}

	// Attempt envelope parsing
	env, envErr := ParseAndValidateAgyEnvelope(candidate, tools, toolChoice)
	if envErr == nil && env != nil {
		if env.Type == "tool_call" {
			var tcList []ToolCall
			for _, call := range env.Calls {
				tcList = append(tcList, ToolCall{
					ID:   call.ID,
					Type: "function",
					Function: ToolCallFunction{
						Name:      call.Name,
						Arguments: call.ArgumentsStr,
					},
				})
			}
			res.ToolCalls = tcList
			res.FinishReason = "tool_calls"
			res.Text = ""
			return res
		}

		// env.Type == "final"
		finalText := ""
		if env.Content != nil {
			finalText = *env.Content
		}
		if len(stopSeqs) > 0 {
			truncated, matched := applyStopSequences(finalText, stopSeqs)
			if matched {
				res.WasTruncatedByStop = true
				finalText = truncated
			}
		}
		res.Text = finalText
		res.FinishReason = "stop"
		return res
	}

	// Not a formal envelope: when tools are enabled and toolChoice != "none", malformed envelope is an error
	if toolChoice.Mode != "none" {
		res.IsError = true
		res.ErrorCode = "invalid_tool_envelope"
		res.ErrorMessage = fmt.Sprintf("Tool envelope validation failed: %v", envErr)
		res.HTTPStatus = 400
		return res
	}

	// Otherwise, plain text response is accepted as final content
	finalText := candidate
	if len(stopSeqs) > 0 {
		truncated, matched := applyStopSequences(finalText, stopSeqs)
		if matched {
			res.WasTruncatedByStop = true
			finalText = truncated
		}
	}
	res.Text = finalText
	res.FinishReason = "stop"
	return res
}

// ParseAgyExecutionOutput parses either a single JSON AgyResponse or NDJSON stream output bytes.
func ParseAgyExecutionOutput(
	outputBytes []byte,
	tools []ToolDefinition,
	toolChoice ParsedToolChoice,
	stopSeqs []string,
) (*CompletionResult, error) {
	trimmed := bytes.TrimSpace(outputBytes)
	if len(trimmed) == 0 {
		return &CompletionResult{
			IsError:      true,
			ErrorCode:    "upstream_no_output",
			ErrorMessage: "Empty execution output",
			HTTPStatus:   502,
		}, nil
	}

	// Check if this is NDJSON stream output (starts with {"event": or has multiple json lines)
	if bytes.HasPrefix(trimmed, []byte(`{"event"`)) || bytes.Contains(trimmed, []byte("\n")) {
		return ParseNDJSONBytes(trimmed, tools, toolChoice, stopSeqs)
	}

	// Otherwise, parse as single JSON AgyResponse
	var resp AgyResponse
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		return nil, &AgyError{Type: "json_parse_fail", Err: err, Detail: string(trimmed)}
	}

	sawNative := false
	for _, st := range resp.Steps {
		if isNativeToolStepType(st.StepType) {
			sawNative = true
			break
		}
	}

	return BuildCompletionResult(
		resp.Status,
		resp.Response,
		resp.StructuredOutput,
		resp.Steps,
		"",
		true,
		sawNative,
		resp.Usage,
		resp.ConversationID,
		tools,
		toolChoice,
		stopSeqs,
	), nil
}

// ParseNDJSONBytes parses NDJSON event lines into a CompletionResult.
func ParseNDJSONBytes(
	ndjsonBytes []byte,
	tools []ToolDefinition,
	toolChoice ParsedToolChoice,
	stopSeqs []string,
) (*CompletionResult, error) {
	lines := bytes.Split(ndjsonBytes, []byte("\n"))
	var (
		convID            string
		status            string
		termResp          string
		termStruct        []byte
		steps             []AgyStep
		accumulatedDeltas strings.Builder
		hasTerminal       bool
		sawNative         bool
		usage             *AgyUsage
	)

	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

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

		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}

		if ev.ConversationID != "" {
			convID = ev.ConversationID
		}

		if ev.Event == "step_update" && ev.StepUpdate != nil {
			if ev.StepUpdate.Usage != nil {
				usage = ev.StepUpdate.Usage
			}
			if isNativeToolStepType(ev.StepUpdate.StepType) {
				sawNative = true
				continue
			}
			if isReasoningStepType(ev.StepUpdate.StepType) {
				continue
			}
			if ev.StepUpdate.TextDelta != "" {
				accumulatedDeltas.WriteString(ev.StepUpdate.TextDelta)
			}
		} else if ev.Event == "result" && ev.Result != nil {
			hasTerminal = true
			if ev.Result.ConversationID != "" {
				convID = ev.Result.ConversationID
			}
			status = ev.Result.Status
			termResp = ev.Result.Response
			if len(ev.Result.StructuredOutput) > 0 {
				termStruct = ev.Result.StructuredOutput
			}
			steps = ev.Result.Steps
			if ev.Result.Usage != nil {
				usage = ev.Result.Usage
			}
			for _, st := range ev.Result.Steps {
				if isNativeToolStepType(st.StepType) {
					sawNative = true
					break
				}
			}
			if status != "SUCCESS" && status != "" {
				errDetail := ev.Result.Error
				if errDetail == "" {
					errDetail = status
				}
				return &CompletionResult{
					IsError:      true,
					ErrorCode:    "upstream_error",
					ErrorMessage: errDetail,
					HTTPStatus:   500,
				}, nil
			}
		}
	}

	return BuildCompletionResult(
		status,
		termResp,
		termStruct,
		steps,
		accumulatedDeltas.String(),
		hasTerminal,
		sawNative,
		usage,
		convID,
		tools,
		toolChoice,
		stopSeqs,
	), nil
}
