package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
)

// ParsedToolChoice represents the normalized tool_choice policy.
type ParsedToolChoice struct {
	Mode             string // "none", "auto", "required", "specific"
	SpecificFunction string // name of function when Mode == "specific"
}

// ParseToolChoice parses raw tool_choice (or function_call) value from OpenAI request.
func ParseToolChoice(toolChoice any, tools []ToolDefinition) (ParsedToolChoice, error) {
	if len(tools) == 0 {
		return ParsedToolChoice{Mode: "none"}, nil
	}
	if toolChoice == nil {
		return ParsedToolChoice{Mode: "auto"}, nil
	}

	switch tc := toolChoice.(type) {
	case string:
		s := strings.TrimSpace(tc)
		switch s {
		case "none":
			return ParsedToolChoice{Mode: "none"}, nil
		case "auto", "":
			return ParsedToolChoice{Mode: "auto"}, nil
		case "required":
			return ParsedToolChoice{Mode: "required"}, nil
		default:
			// Could be a function name directly (legacy OpenAI behavior)
			for _, t := range tools {
				if t.Function != nil && t.Function.Name == s {
					return ParsedToolChoice{Mode: "specific", SpecificFunction: s}, nil
				}
			}
			return ParsedToolChoice{}, fmt.Errorf("invalid tool_choice string: %q", s)
		}
	case map[string]any:
		// Check for {"type": "function", "function": {"name": "..."}}
		if fnMap, ok := tc["function"].(map[string]any); ok {
			if name, ok := fnMap["name"].(string); ok && name != "" {
				for _, t := range tools {
					if t.Function != nil && t.Function.Name == name {
						return ParsedToolChoice{Mode: "specific", SpecificFunction: name}, nil
					}
				}
				return ParsedToolChoice{}, fmt.Errorf("tool_choice specifies unknown function: %q", name)
			}
		}
		// Check for legacy {"name": "..."}
		if name, ok := tc["name"].(string); ok && name != "" {
			for _, t := range tools {
				if t.Function != nil && t.Function.Name == name {
					return ParsedToolChoice{Mode: "specific", SpecificFunction: name}, nil
				}
			}
			return ParsedToolChoice{}, fmt.Errorf("tool_choice specifies unknown function: %q", name)
		}
		return ParsedToolChoice{}, fmt.Errorf("unrecognized tool_choice object: %v", tc)
	default:
		return ParsedToolChoice{}, fmt.Errorf("unsupported tool_choice type: %T", toolChoice)
	}
}

// ValidatedToolCall represents a validated tool call intent.
type ValidatedToolCall struct {
	ID           string
	Name         string
	ArgumentsStr string
}

// ValidatedAgyEnvelope represents the parsed and validated discriminated union from AGY.
type ValidatedAgyEnvelope struct {
	Type    string // "final" or "tool_call"
	Content *string
	Calls   []ValidatedToolCall
}

// rawToolCallIntent represents raw items in calls array from AGY JSON.
type rawToolCallIntent struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type rawAgyEnvelope struct {
	Type    string              `json:"type"`
	Content *string             `json:"content,omitempty"`
	Calls   []rawToolCallIntent `json:"calls,omitempty"`
}

var callIDCounter uint64

func generateCallID(toolName string) string {
	idx := atomic.AddUint64(&callIDCounter, 1)
	cleanName := strings.ReplaceAll(toolName, " ", "_")
	return fmt.Sprintf("call_%s_%d", cleanName, idx)
}

// ParseAndValidateAgyEnvelope parses and validates raw AGY output against tools schema and toolChoice policy.
func ParseAndValidateAgyEnvelope(raw string, tools []ToolDefinition, choice ParsedToolChoice) (*ValidatedAgyEnvelope, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "```json") {
		trimmed = strings.TrimPrefix(trimmed, "```json")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)
	} else if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimPrefix(trimmed, "```")
		trimmed = strings.TrimSuffix(trimmed, "```")
		trimmed = strings.TrimSpace(trimmed)
	}
	if trimmed == "" {
		return nil, errors.New("empty AGY output")
	}

	// Raw <tool_call> string check: must NEVER be guess-converted
	if strings.Contains(trimmed, "<tool_call>") || strings.Contains(trimmed, "</tool_call>") {
		// If it does not start with '{', reject it as invalid envelope
		if !strings.HasPrefix(trimmed, "{") {
			return nil, errors.New("raw <tool_call> textual string is not a valid structured tool_call envelope")
		}
	}

	// Must be a JSON object
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return nil, errors.New("AGY output is not a valid JSON object envelope")
	}

	var rawEnv rawAgyEnvelope
	if err := json.Unmarshal([]byte(trimmed), &rawEnv); err != nil {
		return nil, fmt.Errorf("failed to decode AGY envelope JSON: %w", err)
	}

	if rawEnv.Type == "" {
		return nil, errors.New("missing required envelope 'type' field")
	}

	switch rawEnv.Type {
	case "final":
		if choice.Mode == "required" {
			return nil, errors.New("tool_choice is 'required' but AGY returned 'final' response")
		}
		if choice.Mode == "specific" {
			return nil, fmt.Errorf("tool_choice requires function %q but AGY returned 'final' response", choice.SpecificFunction)
		}
		res := &ValidatedAgyEnvelope{
			Type:    "final",
			Content: rawEnv.Content,
		}
		return res, nil

	case "tool_call":
		if choice.Mode == "none" {
			return nil, errors.New("tool_choice is 'none' but AGY returned 'tool_call' intent")
		}
		if len(rawEnv.Calls) == 0 {
			return nil, errors.New("tool_call envelope contains empty 'calls' array")
		}

		// Build allowed tools map
		allowedTools := make(map[string]bool)
		for _, t := range tools {
			if t.Function != nil && t.Function.Name != "" {
				allowedTools[t.Function.Name] = true
			}
		}

		var validatedCalls []ValidatedToolCall
		for idx, call := range rawEnv.Calls {
			if call.Name == "" {
				return nil, fmt.Errorf("call at index %d is missing tool name", idx)
			}
			if !allowedTools[call.Name] {
				return nil, fmt.Errorf("unknown tool %q not present in request tools list", call.Name)
			}
			if choice.Mode == "specific" && call.Name != choice.SpecificFunction {
				return nil, fmt.Errorf("tool call %q does not match required specific tool %q", call.Name, choice.SpecificFunction)
			}

			// Validate arguments is a valid JSON object
			argsRaw := strings.TrimSpace(string(call.Arguments))
			if argsRaw == "" {
				argsRaw = "{}"
			}
			if !strings.HasPrefix(argsRaw, "{") || !strings.HasSuffix(argsRaw, "}") {
				return nil, fmt.Errorf("arguments for tool %q must be a JSON object, got: %s", call.Name, argsRaw)
			}
			var objCheck map[string]any
			if err := json.Unmarshal([]byte(argsRaw), &objCheck); err != nil {
				return nil, fmt.Errorf("malformed arguments JSON object for tool %q: %w", call.Name, err)
			}

			callID := call.ID
			if callID == "" {
				callID = generateCallID(call.Name)
			}

			validatedCalls = append(validatedCalls, ValidatedToolCall{
				ID:           callID,
				Name:         call.Name,
				ArgumentsStr: argsRaw,
			})
		}

		return &ValidatedAgyEnvelope{
			Type:  "tool_call",
			Calls: validatedCalls,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported envelope type: %q", rawEnv.Type)
	}
}
