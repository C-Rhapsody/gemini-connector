package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// formatMessageContent converts message content into a clean string representation,
// preserving strings, arrays of content blocks, objects, and empty/nil values.
func formatMessageContent(content any) string {
	if content == nil {
		return ""
	}
	switch v := content.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case []any:
		// Check if it's an array of text parts
		allText := true
		var sb strings.Builder
		for i, part := range v {
			if m, ok := part.(map[string]any); ok {
				if t, ok := m["text"].(string); ok && len(m) == 2 && m["type"] == "text" {
					if i > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(t)
					continue
				}
			}
			allText = false
			break
		}
		if allText && sb.Len() > 0 {
			return sb.String()
		}
		b, err := json.Marshal(v)
		if err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", v)
	case map[string]any:
		b, err := json.Marshal(v)
		if err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", v)
	default:
		b, err := json.Marshal(v)
		if err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", v)
	}
}

// splitMessages separates messages into system/developer messages, prior transcript messages,
// and current active turn message(s).
func splitMessages(messages []ChatMessage) (systemMsgs []ChatMessage, transcriptMsgs []ChatMessage, currentTurnMsgs []ChatMessage) {
	if len(messages) == 0 {
		return nil, nil, nil
	}

	lastIdx := len(messages) - 1
	lastMsg := messages[lastIdx]

	var curTurnStart int
	if lastMsg.Role == "tool" {
		// Group all consecutive trailing tool messages as the current turn
		curTurnStart = lastIdx
		for curTurnStart > 0 && messages[curTurnStart-1].Role == "tool" {
			curTurnStart--
		}
	} else if lastMsg.Role == "system" || lastMsg.Role == "developer" {
		// Check if all messages are system/developer
		allSystem := true
		for _, m := range messages {
			if m.Role != "system" && m.Role != "developer" {
				allSystem = false
				break
			}
		}
		if allSystem {
			return messages, nil, nil
		}
		curTurnStart = lastIdx
	} else {
		curTurnStart = lastIdx
	}

	currentTurnMsgs = messages[curTurnStart:]
	earlierMsgs := messages[:curTurnStart]

	for _, m := range earlierMsgs {
		if m.Role == "system" || m.Role == "developer" {
			systemMsgs = append(systemMsgs, m)
		} else {
			transcriptMsgs = append(transcriptMsgs, m)
		}
	}

	return systemMsgs, transcriptMsgs, currentTurnMsgs
}

// renderSingleMessage formats a single message's role, metadata, content, and tool calls.
func renderSingleMessage(header string, msg ChatMessage) string {
	var sb strings.Builder
	sb.WriteString(header + ":\n")

	contentStr := formatMessageContent(msg.Content)
	if contentStr != "" {
		sb.WriteString(contentStr)
	}

	if len(msg.ToolCalls) > 0 {
		if contentStr != "" {
			sb.WriteString("\n")
		}
		sb.WriteString("[Tool Calls]:\n")
		for _, tc := range msg.ToolCalls {
			tcType := tc.Type
			if tcType == "" {
				tcType = "function"
			}
			sb.WriteString(fmt.Sprintf("- Call ID: %s\n  Type: %s\n  Function: %s\n  Arguments: %s\n",
				tc.ID, tcType, tc.Function.Name, tc.Function.Arguments))
		}
	}

	if msg.FunctionCall != nil {
		if contentStr != "" || len(msg.ToolCalls) > 0 {
			sb.WriteString("\n")
		}
		b, _ := json.Marshal(msg.FunctionCall)
		sb.WriteString(fmt.Sprintf("[Function Call]: %s\n", string(b)))
	}

	return strings.TrimRight(sb.String(), "\n")
}

// renderClientTools formats client tool definitions and structured output instructions.
func renderClientTools(tools []ToolDefinition, choice ParsedToolChoice) string {
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<CLIENT_TOOLS>\n")
	sb.WriteString("You have access to the following client-side tools:\n\n")

	for _, t := range tools {
		if t.Function == nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("- Function: %s\n", t.Function.Name))
		if t.Function.Description != "" {
			sb.WriteString(fmt.Sprintf("  Description: %s\n", t.Function.Description))
		}
		if len(t.Function.Parameters) > 0 {
			sb.WriteString(fmt.Sprintf("  Parameters: %s\n", string(t.Function.Parameters)))
		}
		sb.WriteString("\n")
	}

	modeStr := choice.Mode
	if modeStr == "" {
		modeStr = "auto"
	}
	sb.WriteString(fmt.Sprintf("Tool Choice Policy: %s\n", modeStr))
	if choice.SpecificFunction != "" {
		sb.WriteString(fmt.Sprintf("Required Function: %s\n", choice.SpecificFunction))
	}
	sb.WriteString("\nCRITICAL INSTRUCTIONS FOR RESPONSE FORMAT:\n")
	sb.WriteString("You MUST respond ONLY with a valid JSON object adhering to one of the following formats:\n\n")
	sb.WriteString("1. If you decide to call one or more tools (return unexecuted intent for client execution):\n")
	sb.WriteString("```json\n")
	sb.WriteString("{\n")
	sb.WriteString("  \"type\": \"tool_call\",\n")
	sb.WriteString("  \"calls\": [\n")
	sb.WriteString("    {\"id\": \"call_123\", \"name\": \"<tool_name>\", \"arguments\": {<json_arguments>}}\n")
	sb.WriteString("  ]\n")
	sb.WriteString("}\n")
	sb.WriteString("```\n\n")
	sb.WriteString("2. If you are providing the final response to the user:\n")
	sb.WriteString("```json\n")
	sb.WriteString("{\n")
	sb.WriteString("  \"type\": \"final\",\n")
	sb.WriteString("  \"content\": \"<your full textual response here>\"\n")
	sb.WriteString("}\n")
	sb.WriteString("```\n\n")
	sb.WriteString("Output only the JSON envelope matching the schema above.\n")
	sb.WriteString("</CLIENT_TOOLS>\n\n")
	return sb.String()
}

func renderHermesTools(tools []ToolDefinition, choice ParsedToolChoice) string {
	return renderClientTools(tools, choice)
}

// RenderPrompt builds a deterministic, role-preserving prompt representation from OpenAI messages.
func RenderPrompt(messages []ChatMessage) (string, error) {
	return RenderPromptWithTools(messages, nil, nil)
}

// RenderPromptWithTools builds a prompt including Hermes tool definitions and instructions if tools are provided.
func RenderPromptWithTools(messages []ChatMessage, tools []ToolDefinition, toolChoice any) (string, error) {
	if len(messages) == 0 {
		return "", nil
	}

	var parsedChoice ParsedToolChoice
	var err error
	if len(tools) > 0 {
		parsedChoice, err = ParseToolChoice(toolChoice, tools)
		if err != nil {
			return "", fmt.Errorf("invalid tool_choice: %w", err)
		}
	}

	systemMsgs, transcriptMsgs, currentTurnMsgs := splitMessages(messages)

	var sb strings.Builder

	// 0. Hermes Tools (if provided)
	if len(tools) > 0 {
		sb.WriteString(renderHermesTools(tools, parsedChoice))
	}

	// 1. System and Developer Messages
	if len(systemMsgs) > 0 {
		sb.WriteString("<SYSTEM_AND_DEVELOPER_MESSAGES>\n")
		for _, msg := range systemMsgs {
			sb.WriteString(fmt.Sprintf("[%s]\n%s\n\n", msg.Role, formatMessageContent(msg.Content)))
		}
		sb.WriteString("</SYSTEM_AND_DEVELOPER_MESSAGES>\n\n")
	}

	// 2. Conversation Transcript (Past turns in original order)
	if len(transcriptMsgs) > 0 {
		sb.WriteString("<CONVERSATION_TRANSCRIPT>\n")
		for idx, msg := range transcriptMsgs {
			turnNum := idx + 1
			header := fmt.Sprintf("[Turn %d] %s", turnNum, msg.Role)
			if msg.Role == "tool" {
				var meta []string
				if msg.ToolCallID != "" {
					meta = append(meta, fmt.Sprintf("call_id: %s", msg.ToolCallID))
				}
				if msg.Name != "" {
					meta = append(meta, fmt.Sprintf("name: %s", msg.Name))
				}
				if len(meta) > 0 {
					header += fmt.Sprintf(" (%s)", strings.Join(meta, ", "))
				}
			}
			sb.WriteString(renderSingleMessage(header, msg))
			sb.WriteString("\n\n")
		}
		sb.WriteString("</CONVERSATION_TRANSCRIPT>\n\n")
	}

	// 3. Current Active Turn
	if len(currentTurnMsgs) > 0 {
		sb.WriteString("<CURRENT_TURN>\n")
		for i, msg := range currentTurnMsgs {
			if i > 0 {
				sb.WriteString("\n")
			}
			header := fmt.Sprintf("[%s]", msg.Role)
			if msg.Role == "tool" {
				var meta []string
				if msg.ToolCallID != "" {
					meta = append(meta, fmt.Sprintf("call_id: %s", msg.ToolCallID))
				}
				if msg.Name != "" {
					meta = append(meta, fmt.Sprintf("name: %s", msg.Name))
				}
				if len(meta) > 0 {
					header += fmt.Sprintf(" (%s)", strings.Join(meta, ", "))
				}
			}
			sb.WriteString(renderSingleMessage(header, msg))
			sb.WriteString("\n")
		}
		sb.WriteString("</CURRENT_TURN>")
	}

	return strings.TrimSpace(sb.String()), nil
}
