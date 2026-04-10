package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type GeminiResponse struct {
	Response string `json:"response"`
	Error    string `json:"error,omitempty"`
}

type GeminiError struct {
	Type   string // "cli_failure", "no_valid_json", "json_parse_fail", "system_error"
	Err    error
	Detail string
}

func (e *GeminiError) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Detail)
}

func executeGemini(prompt string, sessionUUID string) (string, error) {
	log.Printf("Triggering Gemini CLI for message (via Stdin+Headless): %s", truncateString(prompt, 50))

	// -p 옵션에 최소한의 지시어를 주어 비대화형(Headless) 모드를 강제하고,
	// 실제 본문은 Stdin으로 전달하여 모든 특수문자와 길이 제한을 우회함.
	cmd := exec.Command("gemini", "-y", "-o", "json", "--resume", sessionUUID, "-p", "Process the following telegram message:")
	cmd.Stdin = strings.NewReader(prompt)

	if projectRoot := findProjectRoot(); projectRoot != "" {
		cmd.Dir = projectRoot
	}

	outputBytes, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Gemini CLI execution error: %v\nOutput: %s", err, string(outputBytes))
		errMsg := string(outputBytes)
		if len(errMsg) > 200 {
			errMsg = errMsg[len(errMsg)-200:]
		}
		return "", &GeminiError{Type: "cli_failure", Err: err, Detail: errMsg}
	}

	outputStr := string(outputBytes)
	re := regexp.MustCompile(`(?s){\s*"session_id"|{\s*"response"`)
	loc := re.FindStringIndex(outputStr)

	if loc == nil {
		log.Printf("No valid JSON structure found in output. Raw Output: %s", outputStr)
		return "", &GeminiError{Type: "no_valid_json"}
	}

	jsonStr := extractJSONObject(outputStr[loc[0]:])

	var result GeminiResponse
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		log.Printf("Failed to parse JSON response: %v\nCleaned JSON String: %s", err, jsonStr)
		return "", &GeminiError{Type: "json_parse_fail"}
	}

	if result.Error != "" {
		log.Printf("Gemini CLI returned error in JSON: %s", result.Error)
		return "", &GeminiError{Type: "system_error", Detail: result.Error}
	}

	return result.Response, nil
}

func extractJSONObject(s string) string {
	depth := 0
	inString := false
	escaped := false
	for i, ch := range s {
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '{' {
			depth++
		} else if ch == '}' {
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return s
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
