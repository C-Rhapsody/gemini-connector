package main

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
)

// TestHermes_TwoTurnIntegrationFixture verifies the complete deterministic 2-turn round-trip:
// Turn 1: Hermes sends request with tools -> AGY outputs tool call intent -> connector returns OpenAI tool_calls
// Turn 2: Hermes executes tool -> Hermes sends role=tool message -> AGY outputs final response -> connector returns OpenAI finish_reason=stop
func TestHermes_TwoTurnIntegrationFixture(t *testing.T) {
	// ================= TURN 1 =================
	turn1Body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [
			{"role": "user", "content": "Check disk space on /var/log"}
		],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "hermes_disk_usage",
					"description": "Check filesystem disk usage",
					"parameters": {
						"type": "object",
						"properties": {
							"path": {"type": "string"}
						},
						"required": ["path"]
					}
				}
			}
		],
		"tool_choice": "auto"
	}`

	turn1AgyResponse := `{
		"type": "tool_call",
		"calls": [
			{
				"id": "call_disk_001",
				"name": "hermes_disk_usage",
				"arguments": {"path": "/var/log"}
			}
		]
	}`

	var turn1PromptObserved string
	rec1, raw1 := runContractRequest(t, turn1Body, func(cmd *exec.Cmd) error {
		// Capture prompt passed via Stdin
		if cmd.Stdin != nil {
			var b strings.Builder
			buf := make([]byte, 1024)
			for {
				n, err := cmd.Stdin.Read(buf)
				if n > 0 {
					b.Write(buf[:n])
				}
				if err != nil {
					break
				}
			}
			turn1PromptObserved = b.String()
		}
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: turn1AgyResponse,
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	})

	if rec1.Code != http.StatusOK {
		t.Fatalf("Turn 1 expected 200 OK, got %d: %s", rec1.Code, raw1)
	}

	// Verify Turn 1 prompt contained <HERMES_TOOLS>
	if !strings.Contains(turn1PromptObserved, "<HERMES_TOOLS>") {
		t.Errorf("Turn 1 prompt missing <HERMES_TOOLS> block:\n%s", turn1PromptObserved)
	}
	if !strings.Contains(turn1PromptObserved, "hermes_disk_usage") {
		t.Errorf("Turn 1 prompt missing hermes_disk_usage schema:\n%s", turn1PromptObserved)
	}

	var resp1 ChatCompletionResponse
	if err := json.Unmarshal([]byte(raw1), &resp1); err != nil {
		t.Fatalf("Turn 1 failed to decode response: %v", err)
	}

	if resp1.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("Turn 1 expected finish_reason 'tool_calls', got %q", resp1.Choices[0].FinishReason)
	}
	if resp1.Choices[0].Message.Content != nil {
		t.Fatalf("Turn 1 expected content null, got %v", resp1.Choices[0].Message.Content)
	}
	if len(resp1.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("Turn 1 expected 1 tool call, got %d", len(resp1.Choices[0].Message.ToolCalls))
	}
	tc := resp1.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_disk_001" || tc.Function.Name != "hermes_disk_usage" {
		t.Fatalf("Turn 1 unexpected tool call: %+v", tc)
	}

	// ================= SIMULATED HERMES EXECUTOR =================
	// Hermes executes hermes_disk_usage locally and obtains execution result:
	toolResult := `{"filesystem": "/dev/sda1", "used_percent": "42%", "free_bytes": 10737418240}`
	toolResultJSON, _ := json.Marshal(toolResult)

	// ================= TURN 2 =================
	// Hermes sends the full transcript including the assistant tool call and the role=tool result
	turn2Body := `{
		"model": "gemini-3.8-flash-high",
		"messages": [
			{"role": "user", "content": "Check disk space on /var/log"},
			{
				"role": "assistant",
				"tool_calls": [
					{
						"id": "call_disk_001",
						"type": "function",
						"function": {
							"name": "hermes_disk_usage",
							"arguments": "{\"path\":\"/var/log\"}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "call_disk_001",
				"name": "hermes_disk_usage",
				"content": ` + string(toolResultJSON) + `
			}
		],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "hermes_disk_usage",
					"parameters": {"type": "object"}
				}
			}
		]
	}`

	turn2AgyResponse := `{
		"type": "final",
		"content": "/var/log has 10 GB free space (42% used), which is well within normal operating parameters."
	}`

	var turn2PromptObserved string
	rec2, raw2 := runContractRequest(t, turn2Body, func(cmd *exec.Cmd) error {
		if cmd.Stdin != nil {
			var b strings.Builder
			buf := make([]byte, 1024)
			for {
				n, err := cmd.Stdin.Read(buf)
				if n > 0 {
					b.Write(buf[:n])
				}
				if err != nil {
					break
				}
			}
			turn2PromptObserved = b.String()
		}
		resp := AgyResponse{
			Status:   "SUCCESS",
			Response: turn2AgyResponse,
		}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	})

	if rec2.Code != http.StatusOK {
		t.Fatalf("Turn 2 expected 200 OK, got %d: %s", rec2.Code, raw2)
	}

	// Verify Turn 2 prompt included role=tool round-trip
	for _, expectedNeedle := range []string{"call_id: call_disk_001", "name: hermes_disk_usage", "used_percent"} {
		if !strings.Contains(turn2PromptObserved, expectedNeedle) {
			t.Errorf("Turn 2 prompt missing round-trip element %q:\n%s", expectedNeedle, turn2PromptObserved)
		}
	}

	var resp2 ChatCompletionResponse
	if err := json.Unmarshal([]byte(raw2), &resp2); err != nil {
		t.Fatalf("Turn 2 failed to decode response: %v", err)
	}

	if resp2.Choices[0].FinishReason != "stop" {
		t.Errorf("Turn 2 expected finish_reason 'stop', got %q", resp2.Choices[0].FinishReason)
	}
	content2, ok := resp2.Choices[0].Message.Content.(string)
	if !ok || !strings.Contains(content2, "10 GB free space") {
		t.Errorf("Turn 2 expected final text containing '10 GB free space', got %v", resp2.Choices[0].Message.Content)
	}
	if len(resp2.Choices[0].Message.ToolCalls) > 0 {
		t.Errorf("Turn 2 expected empty tool calls, got %v", resp2.Choices[0].Message.ToolCalls)
	}
}

// TestProfileAPI_ArgvInvariants verifies that ProfileAPI invocations MUST include --sandbox
// and MUST NOT include --dangerously-skip-permissions.
func TestProfileAPI_ArgvInvariants(t *testing.T) {
	body := `{"model": "gemini-3.8-flash-high", "messages": [{"role": "user", "content": "test"}]}`

	var observedArgs []string
	rec, _ := runContractRequest(t, body, func(cmd *exec.Cmd) error {
		observedArgs = cmd.Args
		resp := AgyResponse{Status: "SUCCESS", Response: "ok"}
		b, _ := json.Marshal(resp)
		cmd.Stdout.Write(b)
		return nil
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	hasSandbox := false
	hasDangerousSkip := false
	for _, arg := range observedArgs {
		if arg == "--sandbox" {
			hasSandbox = true
		}
		if arg == "--dangerously-skip-permissions" {
			hasDangerousSkip = true
		}
	}

	if !hasSandbox {
		t.Errorf("ProfileAPI invocation missing required --sandbox flag: %v", observedArgs)
	}
	if hasDangerousSkip {
		t.Errorf("ProfileAPI invocation must NEVER contain --dangerously-skip-permissions: %v", observedArgs)
	}
}
