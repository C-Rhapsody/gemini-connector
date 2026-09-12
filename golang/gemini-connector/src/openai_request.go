package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type RequestValidationError struct {
	Status  int
	ErrType string
	Message string
	Code    string
	Param   *string
}

func (e *RequestValidationError) Error() string {
	return e.Message
}

type NormalizedCompletionRequest struct {
	Model                string
	Messages             []ChatMessage
	Stream               bool
	Tools                []ToolDefinition
	ToolChoice           ParsedToolChoice
	ParallelToolCalls    *bool
	ResponseFormat       *ResponseFormat
	Stop                 []string
	MaxCompletionTokens  *int
	StreamOptions        *StreamOptions
	Temperature          *float64
	TopP                 *float64
	Seed                 *int
	PresencePenalty      *float64
	FrequencyPenalty     *float64
	User                 *string
	Metadata             any
	AgyConversationID    string
}

// ParseAndNormalizeRequest parses raw JSON bytes, validates supported and rejected fields,
// and produces a single authoritative NormalizedCompletionRequest.
func ParseAndNormalizeRequest(bodyBytes []byte) (*NormalizedCompletionRequest, *RequestValidationError) {
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &rawMap); err != nil {
		return nil, &RequestValidationError{
			Status:  400,
			ErrType: "invalid_request_error",
			Message: "Malformed JSON body",
			Code:    "invalid_json",
		}
	}

	// 1. Rejected fields that must be immediately rejected if present
	for _, field := range rejectedFields {
		if _, ok := rawMap[field]; ok {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: fmt.Sprintf("Field %q is not supported", field),
				Code:    "unsupported_field",
				Param:   strPtr(field),
			}
		}
	}

	// 2. Actionable unsupported parameters: logprobs, top_logprobs, logit_bias
	if rawLP, ok := rawMap["logprobs"]; ok {
		var lp bool
		if err := json.Unmarshal(rawLP, &lp); err == nil && lp {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "logprobs is not supported",
				Code:    "unsupported_parameter",
				Param:   strPtr("logprobs"),
			}
		}
	}
	if rawTLP, ok := rawMap["top_logprobs"]; ok {
		var tlp int
		if err := json.Unmarshal(rawTLP, &tlp); err == nil && tlp > 0 {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "top_logprobs is not supported",
				Code:    "unsupported_parameter",
				Param:   strPtr("top_logprobs"),
			}
		}
	}
	if rawLB, ok := rawMap["logit_bias"]; ok {
		var lb map[string]any
		if err := json.Unmarshal(rawLB, &lb); err == nil && len(lb) > 0 {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "logit_bias is not supported",
				Code:    "unsupported_parameter",
				Param:   strPtr("logit_bias"),
			}
		}
	}

	// 3. Decode into standard request DTO
	var req ChatCompletionRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return nil, &RequestValidationError{
			Status:  400,
			ErrType: "invalid_request_error",
			Message: "Failed to parse request JSON: " + err.Error(),
			Code:    "invalid_request_error",
		}
	}

	norm := &NormalizedCompletionRequest{
		Model:    strings.TrimSpace(req.Model),
		Stream:   req.Stream,
		User:     req.User,
		Metadata: req.Metadata,
		Seed:     req.Seed,
	}

	if norm.Model == "" {
		return nil, &RequestValidationError{
			Status:  400,
			ErrType: "invalid_request_error",
			Message: "model is required",
			Code:    "missing_required_parameter",
			Param:   strPtr("model"),
		}
	}

	// 4. n validation (only 1 or omitted is supported)
	if nRaw, ok := rawMap["n"]; ok {
		var n int
		if err := json.Unmarshal(nRaw, &n); err != nil || n != 1 {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "Only n=1 is supported",
				Code:    "unsupported_value",
				Param:   strPtr("n"),
			}
		}
	}

	// 5. max_tokens and max_completion_tokens normalization & conflict check
	var mtVal *int
	if rawMT, ok := rawMap["max_tokens"]; ok {
		var mt int
		if err := json.Unmarshal(rawMT, &mt); err != nil || mt <= 0 {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "max_tokens must be a positive integer",
				Code:    "invalid_value",
				Param:   strPtr("max_tokens"),
			}
		}
		mtVal = &mt
	}

	var mctVal *int
	if rawMCT, ok := rawMap["max_completion_tokens"]; ok {
		var mct int
		if err := json.Unmarshal(rawMCT, &mct); err != nil || mct <= 0 {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "max_completion_tokens must be a positive integer",
				Code:    "invalid_value",
				Param:   strPtr("max_completion_tokens"),
			}
		}
		mctVal = &mct
	}

	if mctVal != nil {
		norm.MaxCompletionTokens = mctVal
	} else if mtVal != nil {
		norm.MaxCompletionTokens = mtVal
	}

	// 6. stop validation & normalization (string or array of strings, max 4)
	if rawStop, ok := rawMap["stop"]; ok && string(rawStop) != "null" {
		var singleStop string
		if err := json.Unmarshal(rawStop, &singleStop); err == nil {
			if strings.TrimSpace(singleStop) == "" {
				return nil, &RequestValidationError{
					Status:  400,
					ErrType: "invalid_request_error",
					Message: "stop sequence cannot be empty",
					Code:    "invalid_value",
					Param:   strPtr("stop"),
				}
			}
			norm.Stop = []string{singleStop}
		} else {
			var stopList []string
			if err := json.Unmarshal(rawStop, &stopList); err != nil {
				return nil, &RequestValidationError{
					Status:  400,
					ErrType: "invalid_request_error",
					Message: "stop must be a string or array of strings",
					Code:    "invalid_value",
					Param:   strPtr("stop"),
				}
			}
			if len(stopList) > 4 {
				return nil, &RequestValidationError{
					Status:  400,
					ErrType: "invalid_request_error",
					Message: "stop array cannot exceed 4 sequences",
					Code:    "invalid_value",
					Param:   strPtr("stop"),
				}
			}
			for _, s := range stopList {
				if s == "" {
					return nil, &RequestValidationError{
						Status:  400,
						ErrType: "invalid_request_error",
						Message: "stop sequence cannot be empty string",
						Code:    "invalid_value",
						Param:   strPtr("stop"),
					}
				}
			}
			norm.Stop = stopList
		}
	}

	// 7. Messages validation
	if len(req.Messages) == 0 {
		return nil, &RequestValidationError{
			Status:  400,
			ErrType: "invalid_request_error",
			Message: "messages array cannot be empty",
			Code:    "missing_required_parameter",
			Param:   strPtr("messages"),
		}
	}
	if len(req.Messages) > defaultMaxMessages {
		return nil, &RequestValidationError{
			Status:  400,
			ErrType: "invalid_request_error",
			Message: fmt.Sprintf("messages count exceeds maximum %d", defaultMaxMessages),
			Code:    "messages_too_many",
			Param:   strPtr("messages"),
		}
	}

	var totalMsgBytes int
	for i, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		switch role {
		case "system", "user", "assistant", "function":
			// valid
		case "tool":
			if strings.TrimSpace(msg.ToolCallID) == "" {
				return nil, &RequestValidationError{
					Status:  400,
					ErrType: "invalid_request_error",
					Message: fmt.Sprintf("messages[%d]: role 'tool' must have a non-empty tool_call_id", i),
					Code:    "missing_tool_call_id",
					Param:   strPtr(fmt.Sprintf("messages[%d].tool_call_id", i)),
				}
			}
		default:
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: fmt.Sprintf("messages[%d]: invalid role %q", i, role),
				Code:    "invalid_role",
				Param:   strPtr(fmt.Sprintf("messages[%d].role", i)),
			}
		}

		// Content blocks validation: only text blocks are supported
		if parts, ok := msg.Content.([]any); ok {
			for partIdx, part := range parts {
				if partMap, isMap := part.(map[string]any); isMap {
					partType, _ := partMap["type"].(string)
					if partType != "text" {
						return nil, &RequestValidationError{
							Status:  400,
							ErrType: "invalid_request_error",
							Message: fmt.Sprintf("messages[%d].content[%d]: unsupported content block type %q", i, partIdx, partType),
							Code:    "unsupported_content_type",
							Param:   strPtr(fmt.Sprintf("messages[%d].content", i)),
						}
					}
				}
			}
		}

		formattedContent := formatMessageContent(msg.Content)
		msgBytes := len(formattedContent)
		if msgBytes > defaultMaxMessageBytes {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: fmt.Sprintf("messages[%d] content exceeds maximum size of 256 KiB", i),
				Code:    "message_too_large",
				Param:   strPtr(fmt.Sprintf("messages[%d].content", i)),
			}
		}
		totalMsgBytes += msgBytes
	}

	if totalMsgBytes > defaultMaxAggregateBytes {
		return nil, &RequestValidationError{
			Status:  400,
			ErrType: "invalid_request_error",
			Message: fmt.Sprintf("Aggregate messages content exceeds maximum %d bytes", defaultMaxAggregateBytes),
			Code:    "messages_aggregate_too_large",
			Param:   strPtr("messages"),
		}
	}
	norm.Messages = req.Messages

	// 8. Tools validation
	if len(req.Tools) > 0 {
		seenToolNames := make(map[string]bool)
		for i, t := range req.Tools {
			if t.Type != "function" || t.Function == nil {
				return nil, &RequestValidationError{
					Status:  400,
					ErrType: "invalid_request_error",
					Message: fmt.Sprintf("tools[%d]: only tool type 'function' with a valid function definition is supported", i),
					Code:    "unsupported_tool_type",
					Param:   strPtr(fmt.Sprintf("tools[%d].type", i)),
				}
			}
			fnName := strings.TrimSpace(t.Function.Name)
			if fnName == "" {
				return nil, &RequestValidationError{
					Status:  400,
					ErrType: "invalid_request_error",
					Message: fmt.Sprintf("tools[%d].function: name is required", i),
					Code:    "missing_required_parameter",
					Param:   strPtr(fmt.Sprintf("tools[%d].function.name", i)),
				}
			}
			if seenToolNames[fnName] {
				return nil, &RequestValidationError{
					Status:  400,
					ErrType: "invalid_request_error",
					Message: fmt.Sprintf("tools[%d]: duplicate function name %q", i, fnName),
					Code:    "duplicate_tool_name",
					Param:   strPtr("tools"),
				}
			}
			seenToolNames[fnName] = true

			// strict validation rejection
			if t.Function.Strict != nil && *t.Function.Strict {
				return nil, &RequestValidationError{
					Status:  400,
					ErrType: "invalid_request_error",
					Message: fmt.Sprintf("tools[%d].function: strict function definition is not supported", i),
					Code:    "unsupported_parameter",
					Param:   strPtr("tools[].function.strict"),
				}
			}
		}
		norm.Tools = req.Tools
	}

	// 9. Tool choice validation
	rawToolChoice := req.ToolChoice
	if len(norm.Tools) == 0 {
		if rawToolChoice != nil {
			if s, ok := rawToolChoice.(string); ok && (s == "none" || s == "") {
				norm.ToolChoice = ParsedToolChoice{Mode: "none"}
			} else {
				return nil, &RequestValidationError{
					Status:  400,
					ErrType: "invalid_request_error",
					Message: "tool_choice cannot be specified when tools array is empty or not provided",
					Code:    "invalid_parameter_combination",
					Param:   strPtr("tool_choice"),
				}
			}
		} else {
			norm.ToolChoice = ParsedToolChoice{Mode: "none"}
		}
	} else {
		parsedTC, err := ParseToolChoice(rawToolChoice, norm.Tools)
		if err != nil {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: err.Error(),
				Code:    "invalid_tool_choice",
				Param:   strPtr("tool_choice"),
			}
		}
		norm.ToolChoice = parsedTC
	}

	norm.ParallelToolCalls = req.ParallelToolCalls

	// 10. Response format validation
	if req.ResponseFormat != nil {
		rfType := strings.TrimSpace(req.ResponseFormat.Type)
		switch rfType {
		case "text", "json_object", "json_schema":
			// valid
		case "":
			rfType = "text"
		default:
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: fmt.Sprintf("Unsupported response_format type: %q", rfType),
				Code:    "unsupported_response_format",
				Param:   strPtr("response_format.type"),
			}
		}

		if rfType == "json_schema" && req.ResponseFormat.JSONSchema != nil {
			if req.ResponseFormat.JSONSchema.Strict != nil && *req.ResponseFormat.JSONSchema.Strict {
				return nil, &RequestValidationError{
					Status:  400,
					ErrType: "invalid_request_error",
					Message: "strict json_schema validation is not supported",
					Code:    "unsupported_parameter",
					Param:   strPtr("response_format.json_schema.strict"),
				}
			}
		}

		// Conflict: tools + non-text response_format
		if len(norm.Tools) > 0 && rfType != "text" {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "combining tools with structured response_format (json_object or json_schema) is not supported",
				Code:    "unsupported_combination",
				Param:   strPtr("response_format"),
			}
		}

		norm.ResponseFormat = req.ResponseFormat
		norm.ResponseFormat.Type = rfType
	}

	// 11. Stream options and include_usage normalization
	if req.StreamOptions != nil {
		norm.StreamOptions = req.StreamOptions
	}
	if req.IncludeUsage != nil && *req.IncludeUsage {
		if norm.StreamOptions == nil {
			norm.StreamOptions = &StreamOptions{IncludeUsage: true}
		} else {
			norm.StreamOptions.IncludeUsage = true
		}
	}

	// 12. Sampling & penalty parameters validation
	if req.Temperature != nil {
		if *req.Temperature < 0.0 || *req.Temperature > 2.0 {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "temperature must be between 0.0 and 2.0",
				Code:    "invalid_value",
				Param:   strPtr("temperature"),
			}
		}
		norm.Temperature = req.Temperature
	}
	if req.TopP != nil {
		if *req.TopP < 0.0 || *req.TopP > 1.0 {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "top_p must be between 0.0 and 1.0",
				Code:    "invalid_value",
				Param:   strPtr("top_p"),
			}
		}
		norm.TopP = req.TopP
	}
	if req.PresencePenalty != nil {
		if *req.PresencePenalty < -2.0 || *req.PresencePenalty > 2.0 {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "presence_penalty must be between -2.0 and 2.0",
				Code:    "invalid_value",
				Param:   strPtr("presence_penalty"),
			}
		}
		norm.PresencePenalty = req.PresencePenalty
	}
	if req.FrequencyPenalty != nil {
		if *req.FrequencyPenalty < -2.0 || *req.FrequencyPenalty > 2.0 {
			return nil, &RequestValidationError{
				Status:  400,
				ErrType: "invalid_request_error",
				Message: "frequency_penalty must be between -2.0 and 2.0",
				Code:    "invalid_value",
				Param:   strPtr("frequency_penalty"),
			}
		}
		norm.FrequencyPenalty = req.FrequencyPenalty
	}

	return norm, nil
}
