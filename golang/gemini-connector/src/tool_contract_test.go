package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseAndValidateAgyEnvelope_Final(t *testing.T) {
	tools := []ToolDefinition{
		{Type: "function", Function: &FunctionDefinition{Name: "hermes_probe"}},
	}
	parsedChoice := ParsedToolChoice{Mode: "auto"}

	raw := `{"type":"final","content":"This is the final answer."}`
	env, err := ParseAndValidateAgyEnvelope(raw, tools, parsedChoice)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.Type != "final" {
		t.Errorf("expected type 'final', got %q", env.Type)
	}
	if env.Content == nil || *env.Content != "This is the final answer." {
		t.Errorf("expected content 'This is the final answer.', got %v", env.Content)
	}
	if len(env.Calls) != 0 {
		t.Errorf("expected 0 calls, got %d", len(env.Calls))
	}
}

func TestParseAndValidateAgyEnvelope_SingleToolCall(t *testing.T) {
	tools := []ToolDefinition{
		{Type: "function", Function: &FunctionDefinition{Name: "hermes_probe"}},
	}
	parsedChoice := ParsedToolChoice{Mode: "auto"}

	raw := `{"type":"tool_call","calls":[{"id":"call_probe_1","name":"hermes_probe","arguments":{"target":"system"}}]}`
	env, err := ParseAndValidateAgyEnvelope(raw, tools, parsedChoice)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env.Type != "tool_call" {
		t.Errorf("expected type 'tool_call', got %q", env.Type)
	}
	if len(env.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(env.Calls))
	}
	call := env.Calls[0]
	if call.ID != "call_probe_1" {
		t.Errorf("expected ID 'call_probe_1', got %q", call.ID)
	}
	if call.Name != "hermes_probe" {
		t.Errorf("expected Name 'hermes_probe', got %q", call.Name)
	}

	var argsMap map[string]any
	if err := json.Unmarshal([]byte(call.ArgumentsStr), &argsMap); err != nil {
		t.Fatalf("arguments should be valid JSON: %v", err)
	}
	if argsMap["target"] != "system" {
		t.Errorf("expected target 'system', got %v", argsMap["target"])
	}
}

func TestParseAndValidateAgyEnvelope_MultipleToolCalls(t *testing.T) {
	tools := []ToolDefinition{
		{Type: "function", Function: &FunctionDefinition{Name: "tool_a"}},
		{Type: "function", Function: &FunctionDefinition{Name: "tool_b"}},
	}
	parsedChoice := ParsedToolChoice{Mode: "auto"}

	raw := `{"type":"tool_call","calls":[
		{"id":"call_a_1","name":"tool_a","arguments":{"val":1}},
		{"id":"call_b_2","name":"tool_b","arguments":{"val":2}}
	]}`
	env, err := ParseAndValidateAgyEnvelope(raw, tools, parsedChoice)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(env.Calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(env.Calls))
	}
	if env.Calls[0].Name != "tool_a" || env.Calls[1].Name != "tool_b" {
		t.Errorf("unexpected tool call names: %v, %v", env.Calls[0].Name, env.Calls[1].Name)
	}
}

func TestParseAndValidateAgyEnvelope_GeneratedCallID(t *testing.T) {
	tools := []ToolDefinition{
		{Type: "function", Function: &FunctionDefinition{Name: "hermes_probe"}},
	}
	parsedChoice := ParsedToolChoice{Mode: "auto"}

	raw := `{"type":"tool_call","calls":[{"name":"hermes_probe","arguments":{}}]}`
	env, err := ParseAndValidateAgyEnvelope(raw, tools, parsedChoice)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(env.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(env.Calls))
	}
	if env.Calls[0].ID == "" {
		t.Errorf("expected auto-generated ID, got empty string")
	}
	if !strings.HasPrefix(env.Calls[0].ID, "call_") {
		t.Errorf("expected ID with prefix 'call_', got %q", env.Calls[0].ID)
	}
}

func TestParseAndValidateAgyEnvelope_MalformedEnvelope(t *testing.T) {
	tools := []ToolDefinition{
		{Type: "function", Function: &FunctionDefinition{Name: "hermes_probe"}},
	}
	parsedChoice := ParsedToolChoice{Mode: "auto"}

	// Not valid JSON
	_, err := ParseAndValidateAgyEnvelope(`not json`, tools, parsedChoice)
	if err == nil {
		t.Errorf("expected error for non-JSON input")
	}

	// Missing type
	_, err = ParseAndValidateAgyEnvelope(`{"content":"no type"}`, tools, parsedChoice)
	if err == nil {
		t.Errorf("expected error for missing type")
	}

	// Unknown type
	_, err = ParseAndValidateAgyEnvelope(`{"type":"unknown","content":"hi"}`, tools, parsedChoice)
	if err == nil {
		t.Errorf("expected error for unknown type")
	}
}

func TestParseAndValidateAgyEnvelope_UnknownTool(t *testing.T) {
	tools := []ToolDefinition{
		{Type: "function", Function: &FunctionDefinition{Name: "tool_a"}},
	}
	parsedChoice := ParsedToolChoice{Mode: "auto"}

	raw := `{"type":"tool_call","calls":[{"name":"unauthorized_tool","arguments":{}}]}`
	_, err := ParseAndValidateAgyEnvelope(raw, tools, parsedChoice)
	if err == nil {
		t.Fatalf("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected error mentioning unknown tool, got: %v", err)
	}
}

func TestParseAndValidateAgyEnvelope_MalformedArguments(t *testing.T) {
	tools := []ToolDefinition{
		{Type: "function", Function: &FunctionDefinition{Name: "tool_a"}},
	}
	parsedChoice := ParsedToolChoice{Mode: "auto"}

	// Arguments is a string instead of an object
	raw := `{"type":"tool_call","calls":[{"name":"tool_a","arguments":"not an object"}]}`
	_, err := ParseAndValidateAgyEnvelope(raw, tools, parsedChoice)
	if err == nil {
		t.Fatalf("expected error for non-object arguments")
	}

	// Arguments is an array instead of an object
	rawArr := `{"type":"tool_call","calls":[{"name":"tool_a","arguments":[1, 2, 3]}]}`
	_, err = ParseAndValidateAgyEnvelope(rawArr, tools, parsedChoice)
	if err == nil {
		t.Fatalf("expected error for array arguments")
	}
}

func TestParseAndValidateAgyEnvelope_RawTextualToolCallNeverConverted(t *testing.T) {
	tools := []ToolDefinition{
		{Type: "function", Function: &FunctionDefinition{Name: "hermes_probe"}},
	}
	parsedChoice := ParsedToolChoice{Mode: "auto"}

	raw := `<tool_call>{"name": "hermes_probe", "arguments": {}}</tool_call>`
	_, err := ParseAndValidateAgyEnvelope(raw, tools, parsedChoice)
	if err == nil {
		t.Fatalf("expected error: raw <tool_call> string must NEVER be guess-converted to tool call")
	}
}

func TestParseAndValidateAgyEnvelope_ToolChoiceRules(t *testing.T) {
	tools := []ToolDefinition{
		{Type: "function", Function: &FunctionDefinition{Name: "tool_a"}},
		{Type: "function", Function: &FunctionDefinition{Name: "tool_b"}},
	}

	// tool_choice: "none" forbids tool calls
	choiceNone := ParsedToolChoice{Mode: "none"}
	callRaw := `{"type":"tool_call","calls":[{"name":"tool_a","arguments":{}}]}`
	_, err := ParseAndValidateAgyEnvelope(callRaw, tools, choiceNone)
	if err == nil {
		t.Errorf("expected error: tool_choice 'none' must forbid tool calls")
	}

	finalRaw := `{"type":"final","content":"allowed text"}`
	env, err := ParseAndValidateAgyEnvelope(finalRaw, tools, choiceNone)
	if err != nil {
		t.Errorf("tool_choice 'none' should allow final text: %v", err)
	}
	if env.Type != "final" {
		t.Errorf("expected type final, got %v", env.Type)
	}

	// tool_choice: "required" forbids final text
	choiceReq := ParsedToolChoice{Mode: "required"}
	_, err = ParseAndValidateAgyEnvelope(finalRaw, tools, choiceReq)
	if err == nil {
		t.Errorf("expected error: tool_choice 'required' must reject final text")
	}

	// tool_choice: specific function
	choiceSpecific := ParsedToolChoice{Mode: "specific", SpecificFunction: "tool_a"}
	wrongCallRaw := `{"type":"tool_call","calls":[{"name":"tool_b","arguments":{}}]}`
	_, err = ParseAndValidateAgyEnvelope(wrongCallRaw, tools, choiceSpecific)
	if err == nil {
		t.Errorf("expected error: specific function tool_a must reject tool_b")
	}

	rightCallRaw := `{"type":"tool_call","calls":[{"name":"tool_a","arguments":{}}]}`
	env, err = ParseAndValidateAgyEnvelope(rightCallRaw, tools, choiceSpecific)
	if err != nil {
		t.Errorf("specific function tool_a should accept tool_a: %v", err)
	}
	if len(env.Calls) != 1 || env.Calls[0].Name != "tool_a" {
		t.Errorf("unexpected calls: %v", env.Calls)
	}
}
