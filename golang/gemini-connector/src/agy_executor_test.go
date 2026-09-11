package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"reflect"
	"testing"
)

func TestProfileAPI_UsesInteractiveCLICommandAndWorkingDirectory(t *testing.T) {
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	var observed []*exec.Cmd
	agyCmdRunner = func(cmd *exec.Cmd) error {
		observed = append(observed, cmd)
		payload, err := json.Marshal(AgyResponse{Status: "SUCCESS", Response: "ok"})
		if err != nil {
			return err
		}
		_, err = cmd.Stdout.Write(payload)
		return err
	}

	for _, profile := range []AgyProfile{ProfileInteractive, ProfileAPI} {
		if _, err := executeAgy(context.Background(), "test prompt", "", AgyCallOptions{Profile: profile}); err != nil {
			t.Fatalf("profile %v execution failed: %v", profile, err)
		}
	}

	if len(observed) != 2 {
		t.Fatalf("expected two AGY invocations, got %d", len(observed))
	}

	interactiveArgs := observed[0].Args[1:]
	apiArgs := observed[1].Args[1:]
	if !reflect.DeepEqual(apiArgs, interactiveArgs) {
		t.Fatalf("ProfileAPI must use the same CLI arguments as ProfileInteractive; interactive=%v api=%v", interactiveArgs, apiArgs)
	}

	wantArgs := []string{
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--print-timeout", "5m",
	}
	if !reflect.DeepEqual(apiArgs, wantArgs) {
		t.Fatalf("unexpected shared AGY arguments: got %v want %v", apiArgs, wantArgs)
	}

	if observed[1].Dir != observed[0].Dir {
		t.Fatalf("ProfileAPI must use the same working directory as ProfileInteractive; interactive=%q api=%q", observed[0].Dir, observed[1].Dir)
	}
}

func TestAgyProfiles_ArgvAndDirPolicies(t *testing.T) {
	oldRunner := agyCmdRunner
	t.Cleanup(func() { agyCmdRunner = oldRunner })

	var observed []*exec.Cmd
	agyCmdRunner = func(cmd *exec.Cmd) error {
		observed = append(observed, cmd)
		for _, a := range cmd.Args {
			if a == "stream-json" {
				_, err := cmd.Stdout.Write([]byte("{\"event\":\"result\",\"result\":{\"status\":\"SUCCESS\",\"response\":\"ok\"}}\n"))
				return err
			}
		}
		payload, err := json.Marshal(AgyResponse{Status: "SUCCESS", Response: "ok"})
		if err != nil {
			return err
		}
		_, err = cmd.Stdout.Write(payload)
		return err
	}

	// 1. ProfileInteractive with conversation ID
	observed = nil
	if _, err := executeAgy(context.Background(), "test prompt", "conv-123", AgyCallOptions{Profile: ProfileInteractive}); err != nil {
		t.Fatalf("ProfileInteractive execution failed: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("expected 1 execution, got %d", len(observed))
	}
	interactiveCmd := observed[0]
	interactiveArgs := interactiveCmd.Args[1:]
	wantInteractiveArgs := []string{
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--print-timeout", "5m",
		"--conversation", "conv-123",
	}
	if !reflect.DeepEqual(interactiveArgs, wantInteractiveArgs) {
		t.Fatalf("ProfileInteractive args mismatch: got %v want %v", interactiveArgs, wantInteractiveArgs)
	}

	// 2. ProfileAPI non-streaming (stateless: conversation ID ignored, model and schema supported)
	observed = nil
	if _, err := executeAgy(context.Background(), "test prompt", "conv-123", AgyCallOptions{
		Profile:    ProfileAPI,
		Model:      "gemini-3.8-flash-high",
		JSONSchema: `{"type":"object"}`,
	}); err != nil {
		t.Fatalf("ProfileAPI execution failed: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("expected 1 execution, got %d", len(observed))
	}
	apiCmd := observed[0]
	apiArgs := apiCmd.Args[1:]
	// Should have output-format json, dangerously-skip-permissions, print-timeout 5m, model, json-schema
	if len(apiArgs) != 9 {
		t.Fatalf("ProfileAPI args length unexpected: got %d, args=%v", len(apiArgs), apiArgs)
	}
	baseAPIArgs := apiArgs[:5]
	wantBaseAPIArgs := []string{
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--print-timeout", "5m",
	}
	if !reflect.DeepEqual(baseAPIArgs, wantBaseAPIArgs) {
		t.Fatalf("ProfileAPI base args mismatch: got %v want %v", baseAPIArgs, wantBaseAPIArgs)
	}
	if apiArgs[5] != "--model" || apiArgs[6] != "gemini-3.8-flash-high" {
		t.Fatalf("ProfileAPI model arg mismatch: %v", apiArgs[5:7])
	}
	if apiArgs[7] != "--json-schema" || apiArgs[8] == "" {
		t.Fatalf("ProfileAPI json-schema arg mismatch: %v", apiArgs[7:])
	}
	if apiCmd.Dir != interactiveCmd.Dir {
		t.Fatalf("ProfileAPI cmd.Dir (%q) != ProfileInteractive cmd.Dir (%q)", apiCmd.Dir, interactiveCmd.Dir)
	}

	// 3. ProfileAPI streaming
	observed = nil
	if _, err := executeAgy(context.Background(), "test prompt", "", AgyCallOptions{
		Profile: ProfileAPI,
		Stream:  true,
	}); err != nil {
		t.Fatalf("ProfileAPI stream execution failed: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("expected 1 execution, got %d", len(observed))
	}
	apiStreamCmd := observed[0]
	apiStreamArgs := apiStreamCmd.Args[1:]
	wantAPIStreamArgs := []string{
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
		"--print-timeout", "5m",
	}
	if !reflect.DeepEqual(apiStreamArgs, wantAPIStreamArgs) {
		t.Fatalf("ProfileAPI stream args mismatch: got %v want %v", apiStreamArgs, wantAPIStreamArgs)
	}
	if apiStreamCmd.Dir != interactiveCmd.Dir {
		t.Fatalf("ProfileAPI stream cmd.Dir (%q) != ProfileInteractive cmd.Dir (%q)", apiStreamCmd.Dir, interactiveCmd.Dir)
	}

	// Invariant checks on API args: no sandbox, no mode plan, no disable-slash-commands, no 90s timeout
	for _, arg := range apiArgs {
		if arg == "--sandbox" || arg == "--mode" || arg == "--disable-slash-commands" || arg == "90s" {
			t.Errorf("ProfileAPI args contain restricted flag %q: %v", arg, apiArgs)
		}
	}
	for _, arg := range apiStreamArgs {
		if arg == "--sandbox" || arg == "--mode" || arg == "--disable-slash-commands" || arg == "90s" {
			t.Errorf("ProfileAPI stream args contain restricted flag %q: %v", arg, apiStreamArgs)
		}
	}

	// 4. ProfilePlanner: must retain mode plan, sandbox, disable-slash-commands
	observed = nil
	if _, err := executeAgy(context.Background(), "test prompt", "conv-planner", AgyCallOptions{
		Profile: ProfilePlanner,
	}); err != nil {
		t.Fatalf("ProfilePlanner execution failed: %v", err)
	}
	if len(observed) != 1 {
		t.Fatalf("expected 1 execution, got %d", len(observed))
	}
	plannerCmd := observed[0]
	plannerArgs := plannerCmd.Args[1:]
	wantPlannerArgs := []string{
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--print-timeout", "5m",
		"--mode", "plan",
		"--sandbox",
		"--disable-slash-commands",
		"--conversation", "conv-planner",
	}
	if !reflect.DeepEqual(plannerArgs, wantPlannerArgs) {
		t.Fatalf("ProfilePlanner args mismatch: got %v want %v", plannerArgs, wantPlannerArgs)
	}
	if plannerCmd.Dir != interactiveCmd.Dir {
		t.Fatalf("ProfilePlanner cmd.Dir (%q) != ProfileInteractive cmd.Dir (%q)", plannerCmd.Dir, interactiveCmd.Dir)
	}
}

type stubAgyExecutor struct {
	execute func(ctx context.Context, prompt string, conversationID string, opts AgyCallOptions) (string, error)
}

func (s stubAgyExecutor) Execute(ctx context.Context, prompt string, conversationID string, opts AgyCallOptions) (string, error) {
	if s.execute == nil {
		return "", nil
	}
	return s.execute(ctx, prompt, conversationID, opts)
}
