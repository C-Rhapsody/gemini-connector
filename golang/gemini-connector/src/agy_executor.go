package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// AgyExecutor is the narrow execution contract consumed by the HTTP server
// and the inbound-message controller. The CLI implementation remains behind
// this boundary so callers can be tested without package-level function swaps.
type AgyExecutor interface {
	Execute(ctx context.Context, prompt string, conversationID string, opts AgyCallOptions) (*CompletionResult, error)
}

type cliAgyExecutor struct {
	runner AgyCmdRunner
}

func newAgyExecutor() AgyExecutor {
	return cliAgyExecutor{}
}

func newAgyExecutorWithRunner(runner AgyCmdRunner) AgyExecutor {
	return cliAgyExecutor{runner: runner}
}

func (c cliAgyExecutor) Execute(ctx context.Context, prompt string, conversationID string, opts AgyCallOptions) (*CompletionResult, error) {
	if opts.Runner == nil && c.runner != nil {
		opts.Runner = c.runner
	}
	return executeAgy(ctx, prompt, conversationID, opts)
}

// agyCmdRunner executes the agy exec.Cmd. Overridable in tests.
var agyCmdRunner = func(cmd *exec.Cmd) error {
	return cmd.Run()
}

// agyRetryDelay is the wait duration before retrying on transient stream errors.
var agyRetryDelay = 1500 * time.Millisecond

// isTransientAgyStreamError detects transient stream lag/stalls between the CLI subscriber and daemon.
func isTransientAgyStreamError(detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "subscriber fell behind updates") ||
		strings.Contains(lower, "connection to the agent was interrupted") ||
		strings.Contains(lower, "stalled for")
}

// AgyCallOptions tweaks individual agy invocations.
type AgyCallOptions struct {
	// Profile selects argument assembly and execution policy.
	Profile AgyProfile
	// BypassQuotaGate lets the call proceed even while a 429 quota cooldown
	// is active. Used for explicit commands (/reset, /clear), which should
	// always attempt execution; regular chat turns keep being gated.
	BypassQuotaGate bool
	// DisableRetry turns off automatic retry on transient stream errors.
	DisableRetry bool
	// Model selects the exact model (e.g. gemini-3.8-flash-high).
	Model string
	// Stream enables stream-json output format and streaming callback.
	Stream bool
	// StreamCallback is called for each text_delta during streaming.
	StreamCallback func(chunk string) error
	// Logger is the optional API logger instance.
	Logger *APILogger
	// JSONSchema is the JSON schema string to enforce via agy --json-schema.
	JSONSchema string
	// UsageCallback is called with the actual token usage from agy if available.
	UsageCallback func(*AgyUsage)
	// StructuredOutputRequested is set when structured output / JSON schema was requested.
	StructuredOutputRequested bool
	// Tools is the set of declared tools for validation against model envelope calls.
	Tools []ToolDefinition
	// ToolChoice is the parsed tool choice policy.
	ToolChoice ParsedToolChoice
	// Stop is the list of stop sequences.
	Stop []string
	// Runner provides an optional instance-local command runner.
	Runner AgyCmdRunner
}

func executeAgy(ctx context.Context, prompt string, conversationID string, opts ...AgyCallOptions) (*CompletionResult, error) {
	var o AgyCallOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	// While quota cooldown is active, do not spawn agy at all; reply with
	// the stored error text whose time fields show the remaining time.
	if ae := quotaBlockErr(o.BypassQuotaGate); ae != nil {
		return nil, ae
	}

	var tempSchemaPath string
	if o.Profile == ProfileAPI && o.JSONSchema != "" {
		tmpFile, err := os.CreateTemp("", "agy_schema_*.json")
		if err != nil {
			return nil, &AgyError{Type: "schema_error", Detail: "failed to create temporary schema file: " + err.Error()}
		}
		tempSchemaPath = tmpFile.Name()
		defer os.Remove(tempSchemaPath)
		if _, err := tmpFile.WriteString(o.JSONSchema); err != nil {
			_ = tmpFile.Close()
			return nil, &AgyError{Type: "schema_error", Detail: "failed to write temporary schema file: " + err.Error()}
		}
		_ = tmpFile.Close()
	}

	maxAttempts := 2
	if o.DisableRetry || o.Profile == ProfileAPI {
		maxAttempts = 1
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(agyRetryDelay):
			}
			log.Printf("Retrying agy CLI execution (attempt %d/%d) for message: %s", attempt, maxAttempts, truncateString(prompt, 50))
		} else {
			if o.Profile != ProfileAPI {
				log.Printf("Triggering agy CLI for message (via Stdin): %s", truncateString(prompt, 50))
			}
		}

		// Turn start marker for transcript salvage: brain steps created from this
		// point on belong to the turn we are about to spawn.
		turnStart := time.Now()

		// Interactive and API turns share the same execution policy. The only
		// protocol-specific argument is stream-json for API streaming.
		// Both share the configured AGY conversation session.
		outputFormat := "json"
		if o.Profile == ProfileAPI && o.Stream {
			outputFormat = "stream-json"
		}
		args := []string{
			"--output-format", outputFormat,
			"--dangerously-skip-permissions",
			"--print-timeout", "5m",
		}
		if o.Profile == ProfileAPI && o.Model != "" {
			args = append(args, "--model", o.Model)
		}
		if tempSchemaPath != "" {
			args = append(args, "--json-schema", tempSchemaPath)
		}
		if o.Profile == ProfilePlanner {
			args = append(args, "--mode", "plan", "--sandbox", "--disable-slash-commands")
		}
		if conversationID != "" {
			args = append(args, "--conversation", conversationID)
		}

		cmd := exec.CommandContext(ctx, "agy", args...)
		// agy shells out to grep for its grep_search tool; make sure a grep is
		// resolvable no matter which terminal launched the connector.
		cmd.Env = agyEnv()
		configureAgyProcess(cmd)
		// /stop must take down the whole agy process tree, not just the root.
		cmd.Cancel = func() error { return killAgyProcess(cmd.Process) }
		// If the tree kill fails or the process ignores it, WaitDelay force-stops
		// the wait so the caller does not hang forever.
		cmd.WaitDelay = 10 * time.Second
		cmd.Stdin = strings.NewReader(prompt)

		var stdout bytes.Buffer
		stdoutLimit := int64(8 * 1024 * 1024)
		boundedStdout := NewBoundedWriter(&stdout, stdoutLimit)
		var streamWriter *ndjsonStreamWriter
		if o.Profile == ProfileAPI && o.Stream {
			streamWriter = &ndjsonStreamWriter{
				callback:       o.StreamCallback,
				maxBytes:       4 * 1024 * 1024,
				maxLine:        4 * 1024 * 1024,
				structuredMode: o.StructuredOutputRequested || o.JSONSchema != "" || len(o.Tools) > 0,
			}
			cmd.Stdout = streamWriter
		} else {
			cmd.Stdout = boundedStdout
		}
		var stderr bytes.Buffer
		stderrLimit := int64(4 * 1024 * 1024)
		boundedStderr := NewBoundedWriter(&stderr, stderrLimit)
		cmd.Stderr = boundedStderr

		if projectRoot := findProjectRoot(); projectRoot != "" {
			cmd.Dir = projectRoot
		}

		if !acquireAgyLaunch(ctx, 30*time.Second) {
			return nil, &AgyError{Type: "launch_timeout", Detail: "timed out waiting for agy launch lock"}
		}
		runner := resolveRunner(o.Runner, nil)
		cmdErr := runner(cmd)
		releaseAgyLaunch()

		if boundedStdout.Exceeded {
			return nil, &AgyError{Type: "output_too_large", Detail: "raw JSON stdout exceeded 8 MiB limit"}
		}

		if streamWriter != nil {
			_ = streamWriter.Finalize()
		}

		if cmdErr != nil {
			stderrMsg := strings.TrimSpace(stderr.String())
			if o.Profile != ProfileAPI {
				log.Printf("agy CLI execution error: %v\nStderr: %s", cmdErr, stderrMsg)
			}

			if o.Profile == ProfileAPI {
				detail := stderrMsg
				if detail == "" {
					detail = cmdErr.Error()
				}
				if len(detail) > 200 {
					detail = detail[len(detail)-200:]
				}
				return nil, &AgyError{Type: "cli_failure", Err: cmdErr, Detail: detail}
			}

			if strings.Contains(stderrMsg, "authentication required") {
				return nil, &AgyError{Type: "authentication_required", Err: cmdErr, Detail: stderrMsg}
			}

			// agy occasionally reports an error (or process terminates early)
			// even though the model finished the turn in the background; check transcript.
			if !isQuotaExhaustedDetail(stderrMsg) {
				if salvaged := salvageTurnResponse(conversationID, prompt, turnStart); salvaged != "" {
					log.Printf("agy CLI reported error (%s) but transcript salvaged response", truncateString(stderrMsg, 80))
					return &CompletionResult{Status: "SUCCESS", Text: salvaged, ConversationID: conversationID, FinishReason: "stop"}, nil
				}
			}

			// Transient stream lag / stall: retry once after a short backoff if context is still active.
			if attempt < maxAttempts && isTransientAgyStreamError(stderrMsg) && ctx.Err() == nil {
				log.Printf("Transient agy stream lag error detected (%s); retrying...", truncateString(stderrMsg, 80))
				continue
			}

			detail := stderrMsg
			if len(detail) > 200 {
				detail = detail[len(detail)-200:]
			}
			return nil, &AgyError{Type: "cli_failure", Err: cmdErr, Detail: detail}
		}

		if o.Profile == ProfileAPI && o.Stream {
			if streamWriter.err != nil {
				if ae, ok := streamWriter.err.(*AgyError); ok {
					return nil, ae
				}
				return nil, &AgyError{Type: "stream_error", Detail: streamWriter.err.Error()}
			}
			if !streamWriter.sawSuccess {
				return nil, &AgyError{Type: "stream_incomplete", Detail: "stream ended without success result"}
			}
			if conversationID != "" {
				stderrStr := stderr.String()
				if strings.Contains(stderrStr, "not found") && strings.Contains(stderrStr, "conversation") {
					return nil, &AgyError{
						Type:   "session_resume_failed",
						Detail: "AGY conversation resume failed: session not found",
					}
				}
			}
			if o.UsageCallback != nil && streamWriter.usage != nil {
				o.UsageCallback(streamWriter.usage)
			}

			res := BuildCompletionResult(
				"SUCCESS",
				streamWriter.terminalResponse,
				streamWriter.terminalStructuredOutput,
				streamWriter.steps,
				streamWriter.accumulatedProgress.String(),
				true,
				streamWriter.sawNativeToolStep,
				streamWriter.usage,
				streamWriter.conversationID,
				o.Tools,
				o.ToolChoice,
				o.Stop,
			)
			if res.IsError {
				return res, &AgyError{Type: res.ErrorCode, Detail: res.ErrorMessage}
			}
			return res, nil
		}

		stdoutBytes := stdout.Bytes()
		if o.Profile == ProfileAPI && int64(len(stdoutBytes)) > 8*1024*1024 {
			return nil, &AgyError{Type: "output_too_large", Detail: "raw JSON stdout exceeded 8 MiB limit"}
		}

		if o.Profile == ProfileAPI {
			if conversationID != "" {
				stderrStr := stderr.String()
				if strings.Contains(stderrStr, "not found") && strings.Contains(stderrStr, "conversation") {
					return nil, &AgyError{
						Type:   "session_resume_failed",
						Detail: "AGY conversation resume failed: session not found",
					}
				}
			}
			compRes, parseErr := ParseAgyExecutionOutput(stdoutBytes, o.Tools, o.ToolChoice, o.Stop)
			if parseErr != nil {
				return nil, parseErr
			}
			if compRes.IsError {
				return compRes, &AgyError{Type: compRes.ErrorCode, Detail: compRes.ErrorMessage}
			}
			if o.UsageCallback != nil && compRes.Usage != nil {
				o.UsageCallback(compRes.Usage)
			}
			return compRes, nil
		}

		var result AgyResponse
		if err := json.Unmarshal(stdoutBytes, &result); err != nil {
			if o.Profile != ProfileAPI {
				log.Printf("Failed to parse agy JSON response: %v\nStdout: %s", err, string(stdoutBytes))
				if attempt < maxAttempts && (isTransientAgyStreamError(stderr.String()) || isTransientAgyStreamError(string(stdoutBytes))) && ctx.Err() == nil {
					log.Printf("Transient agy error in JSON response; retrying...")
					continue
				}
			}
			return nil, &AgyError{Type: "json_parse_fail", Err: err, Detail: string(stdoutBytes)}
		}

		if conversationID != "" {
			stderrStr := stderr.String()
			if strings.Contains(stderrStr, "not found") && strings.Contains(stderrStr, "conversation") {
				return nil, &AgyError{
					Type:   "session_resume_failed",
					Detail: "AGY conversation resume failed: session not found",
				}
			}
		}

		if result.Status != "SUCCESS" {
			detail := result.Error
			if detail == "" {
				detail = "agy returned status: " + result.Status
			}
			if o.Profile == ProfileAPI {
				return nil, &AgyError{Type: "error_status", Detail: detail}
			}

			log.Printf("agy returned non-success status: %s, error: %s", result.Status, detail)

			// agy occasionally reports ERROR because an intermediate tool failed
			// (e.g. its built-in grep_search exiting non-zero) while the model
			// still finished the turn; the brain transcript then holds the real
			// answer. Quota exhaustion outranks salvage: a 429 turn produces no
			// answer and the cooldown flow must stay authoritative.
			if !isQuotaExhaustedDetail(detail) {
				if salvaged := salvageTurnResponse(conversationID, prompt, turnStart); salvaged != "" {
					log.Printf("agy reported an error (%s) but the turn completed; delivering the recovered response", truncateString(detail, 80))
					return &CompletionResult{Status: "SUCCESS", Text: salvaged, ConversationID: conversationID, FinishReason: "stop"}, nil
				}
			}

			if attempt < maxAttempts && isTransientAgyStreamError(detail) && ctx.Err() == nil {
				log.Printf("Transient agy error status detected (%s); retrying...", truncateString(detail, 80))
				continue
			}

			return nil, &AgyError{Type: "error_status", Detail: detail}
		}

		respText := result.Response
		if respText == "" && len(result.StructuredOutput) > 0 {
			respText = string(result.StructuredOutput)
		}

		if o.UsageCallback != nil && result.Usage != nil {
			o.UsageCallback(result.Usage)
		}

		convID := result.ConversationID
		if convID == "" {
			convID = conversationID
		}
		return &CompletionResult{
			Status:         result.Status,
			Text:           respText,
			Usage:          result.Usage,
			ConversationID: convID,
			FinishReason:   "stop",
		}, nil
	}

	return nil, &AgyError{Type: "cli_failure", Detail: "agy execution attempts exhausted"}
}
