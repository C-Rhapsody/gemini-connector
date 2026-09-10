package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type AgyResponse struct {
	ConversationID  string  `json:"conversation_id"`
	Status          string  `json:"status"`
	Response        string  `json:"response"`
	Error           string  `json:"error,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	NumTurns        int     `json:"num_turns"`
}

type AgyUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

type AgyError struct {
	Type   string
	Err    error
	Detail string
}

func (e *AgyError) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Detail)
}

// AgyProfile selects the execution policy of one agy invocation. Profiles
// make the intent explicit at the call site instead of leaking boolean
// flags (e.g. planner mode) through the runner boundary.
type AgyProfile int

const (
	// ProfileInteractive is a regular user chat turn.
	ProfileInteractive AgyProfile = iota
	// ProfilePlanner runs under plan/sandbox restrictions for /cron candidate
	// generation: the model must answer with a single JSON object and cannot
	// modify the workspace while producing it.
	ProfilePlanner
	// ProfileScheduled is a cron-triggered background turn.
	ProfileScheduled
	// ProfileBootstrap creates or replays into a fresh conversation.
	ProfileBootstrap
	// ProfileAPI runs stateless OpenAI-compatible completions with sandbox restrictions
	// and no dangerous permissions.
	ProfileAPI
)

// agyLaunchSem is the shared AGY-launch mutex ensuring that at most one AGY
// process (chat turn, model discovery, cron planning) runs at any time.
var agyLaunchSem = make(chan struct{}, 1)

func init() {
	agyLaunchSem <- struct{}{}
}

func acquireAgyLaunch(ctx context.Context, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-agyLaunchSem:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func releaseAgyLaunch() {
	select {
	case agyLaunchSem <- struct{}{}:
	default:
	}
}

// ndjsonStreamWriter parses streaming NDJSON events from agy --output-format stream-json
// in real time as chunks arrive.
type ndjsonStreamWriter struct {
	callback   func(delta string) error
	buf        bytes.Buffer
	totalBytes int64
	maxBytes   int64
	maxLine    int64
	sawSuccess bool
	err        error
	mu         sync.Mutex
}

func (w *ndjsonStreamWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}

	w.buf.Write(p)
	for {
		line, readErr := w.buf.ReadBytes('\n')
		if readErr != nil {
			// Partial line; put it back into w.buf
			w.buf.Write(line)
			if int64(w.buf.Len()) > w.maxLine {
				w.err = fmt.Errorf("stream line exceeds maximum size %d bytes", w.maxLine)
				return len(p), w.err
			}
			break
		}
		if int64(len(line)) > w.maxLine {
			w.err = fmt.Errorf("stream line exceeds maximum size %d bytes", w.maxLine)
			return len(p), w.err
		}

		var ev struct {
			Event      string `json:"event"`
			StepUpdate *struct {
				StepType  string `json:"step_type"`
				TextDelta string `json:"text_delta"`
			} `json:"step_update"`
			Result *struct {
				Status string `json:"status"`
			} `json:"result"`
		}
		if jsonErr := json.Unmarshal(bytes.TrimSpace(line), &ev); jsonErr == nil {
			if ev.Event == "step_update" && ev.StepUpdate != nil && ev.StepUpdate.TextDelta != "" {
				w.totalBytes += int64(len(ev.StepUpdate.TextDelta))
				if w.totalBytes > w.maxBytes {
					w.err = fmt.Errorf("cumulative assistant output exceeds limit of %d bytes", w.maxBytes)
					return len(p), w.err
				}
				if w.callback != nil {
					if cbErr := w.callback(ev.StepUpdate.TextDelta); cbErr != nil {
						w.err = cbErr
						return len(p), w.err
					}
				}
			} else if ev.Event == "result" && ev.Result != nil {
				if ev.Result.Status == "SUCCESS" {
					w.sawSuccess = true
				} else {
					w.err = fmt.Errorf("upstream result status: %s", ev.Result.Status)
					return len(p), w.err
				}
			}
		}
	}
	return len(p), nil
}

// agyInvoker indirection lets controller-level tests stub interactive turns.
var agyInvoker = executeAgy

func swapAgyInvoker(fn func(ctx context.Context, prompt string, conversationID string, opts ...AgyCallOptions) (string, error)) func() {
	old := agyInvoker
	agyInvoker = fn
	return func() { agyInvoker = old }
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
}

// quotaBlockErr returns the rejection error for gated calls during an active
// quota cooldown, or nil when the call may proceed.
func quotaBlockErr(bypass bool) *AgyError {
	if bypass || !QuotaActive() {
		return nil
	}
	log.Printf("agy call blocked by quota cooldown (%s remaining)", formatQuotaDuration(QuotaRemaining()))
	return &AgyError{Type: "quota_cooldown", Detail: QuotaRefreshedDetail()}
}

func executeAgy(ctx context.Context, prompt string, conversationID string, opts ...AgyCallOptions) (string, error) {
	var o AgyCallOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	// While quota cooldown is active, do not spawn agy at all; reply with
	// the stored error text whose time fields show the remaining time.
	if ae := quotaBlockErr(o.BypassQuotaGate); ae != nil {
		return "", ae
	}

	maxAttempts := 2
	if o.DisableRetry || o.Profile == ProfileAPI {
		maxAttempts = 1
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
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

		var args []string
		if o.Profile == ProfileAPI {
			args = []string{
				"--mode", "plan",
				"--sandbox",
				"--disable-slash-commands",
				"--print-timeout", "90s",
			}
			if o.Stream {
				args = append(args, "--output-format", "stream-json")
			} else {
				args = append(args, "--output-format", "json")
			}
			if o.Model != "" {
				args = append(args, "--model", o.Model)
			}
			// Note: API calls are stateless and never inherit conversation ID or dangerous permissions.
		} else {
			args = []string{
				"--output-format", "json",
				"--dangerously-skip-permissions",
				"--print-timeout", "5m",
			}
			if o.Profile == ProfilePlanner {
				args = append(args, "--mode", "plan", "--sandbox", "--disable-slash-commands")
			}
			if conversationID != "" {
				args = append(args, "--conversation", conversationID)
			}
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
		var streamWriter *ndjsonStreamWriter
		if o.Profile == ProfileAPI && o.Stream {
			streamWriter = &ndjsonStreamWriter{
				callback: o.StreamCallback,
				maxBytes: 4 * 1024 * 1024,
				maxLine:  4 * 1024 * 1024,
			}
			cmd.Stdout = streamWriter
		} else {
			cmd.Stdout = &stdout
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		if projectRoot := findProjectRoot(); projectRoot != "" {
			cmd.Dir = projectRoot
		}

		if !acquireAgyLaunch(ctx, 30*time.Second) {
			return "", &AgyError{Type: "launch_timeout", Detail: "timed out waiting for agy launch lock"}
		}
		cmdErr := agyCmdRunner(cmd)
		releaseAgyLaunch()

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
				return "", &AgyError{Type: "cli_failure", Err: cmdErr, Detail: detail}
			}

			if strings.Contains(stderrMsg, "authentication required") {
				return "", &AgyError{Type: "authentication_required", Err: cmdErr, Detail: stderrMsg}
			}

			// agy occasionally reports an error (or process terminates early)
			// even though the model finished the turn in the background; check transcript.
			if !isQuotaExhaustedDetail(stderrMsg) {
				if salvaged := salvageTurnResponse(conversationID, prompt, turnStart); salvaged != "" {
					log.Printf("agy CLI reported error (%s) but transcript salvaged response", truncateString(stderrMsg, 80))
					return salvaged, nil
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
			return "", &AgyError{Type: "cli_failure", Err: cmdErr, Detail: detail}
		}

		if o.Profile == ProfileAPI && o.Stream {
			if streamWriter.err != nil {
				return "", &AgyError{Type: "stream_error", Detail: streamWriter.err.Error()}
			}
			if !streamWriter.sawSuccess {
				return "", &AgyError{Type: "stream_incomplete", Detail: "stream ended without success result"}
			}
			return "", nil
		}

		stdoutBytes := stdout.Bytes()
		if o.Profile == ProfileAPI && int64(len(stdoutBytes)) > 8*1024*1024 {
			return "", &AgyError{Type: "output_too_large", Detail: "raw JSON stdout exceeded 8 MiB limit"}
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
			return "", &AgyError{Type: "json_parse_fail", Err: err, Detail: string(stdoutBytes)}
		}

		if result.Status != "SUCCESS" {
			detail := result.Error
			if detail == "" {
				detail = "agy returned status: " + result.Status
			}
			if o.Profile == ProfileAPI {
				return "", &AgyError{Type: "error_status", Detail: detail}
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
					return salvaged, nil
				}
			}

			if attempt < maxAttempts && isTransientAgyStreamError(detail) && ctx.Err() == nil {
				log.Printf("Transient agy error status detected (%s); retrying...", truncateString(detail, 80))
				continue
			}

			return "", &AgyError{Type: "error_status", Detail: detail}
		}

		if o.Profile == ProfileAPI && int64(len(result.Response)) > 4*1024*1024 {
			return "", &AgyError{Type: "output_too_large", Detail: "decoded assistant response exceeded 4 MiB limit"}
		}

		return result.Response, nil
	}

	return "", &AgyError{Type: "cli_failure", Detail: "agy execution attempts exhausted"}
}

var urlFetchFailurePattern = regexp.MustCompile(`Failed to fetch document content at (\S+)`)


func extractUrlFetchFailure(detail string) string {
	match := urlFetchFailurePattern.FindStringSubmatch(detail)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

func findProjectRoot() string {
	searchDir, err := os.Executable()
	if err != nil {
		return ""
	}
	searchDir = filepath.Dir(searchDir)
	for {
		if info, err := os.Stat(filepath.Join(searchDir, ".gemini")); err == nil && info.IsDir() {
			return searchDir
		}
		parentDir := filepath.Dir(searchDir)
		if parentDir == searchDir {
			break
		}
		searchDir = parentDir
	}
	return ""
}
