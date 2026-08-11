package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func executeAgy(prompt string, conversationID string) (string, error) {
	log.Printf("Triggering agy CLI for message (via Stdin): %s", truncateString(prompt, 50))

	args := []string{
		"--output-format", "json",
		"--dangerously-skip-permissions",
		"--print-timeout", "5m",
	}
	if conversationID != "" {
		args = append(args, "--conversation", conversationID)
	}

	cmd := exec.Command("agy", args...)
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if projectRoot := findProjectRoot(); projectRoot != "" {
		cmd.Dir = projectRoot
	}

	if err := cmd.Run(); err != nil {
		stderrMsg := strings.TrimSpace(stderr.String())
		log.Printf("agy CLI execution error: %v\nStderr: %s", err, stderrMsg)

		if strings.Contains(stderrMsg, "authentication required") {
			return "", &AgyError{Type: "authentication_required", Err: err, Detail: stderrMsg}
		}

		detail := stderrMsg
		if len(detail) > 200 {
			detail = detail[len(detail)-200:]
		}
		return "", &AgyError{Type: "cli_failure", Err: err, Detail: detail}
	}

	stdoutBytes := stdout.Bytes()
	var result AgyResponse
	if err := json.Unmarshal(stdoutBytes, &result); err != nil {
		log.Printf("Failed to parse agy JSON response: %v\nStdout: %s", err, string(stdoutBytes))
		return "", &AgyError{Type: "json_parse_fail", Err: err, Detail: string(stdoutBytes)}
	}

	if result.Status != "SUCCESS" {
		detail := result.Error
		if detail == "" {
			detail = "agy returned status: " + result.Status
		}
		log.Printf("agy returned non-success status: %s, error: %s", result.Status, detail)
		return "", &AgyError{Type: "error_status", Detail: detail}
	}

	return result.Response, nil
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
