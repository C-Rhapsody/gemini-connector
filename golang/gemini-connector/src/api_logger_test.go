package main

import (
	"bytes"
	"encoding/json"
	"os"
	"sync"
	"testing"
)

func TestAPILogger_BasicAndAllowlist(t *testing.T) {
	dir := t.TempDir()
	logger := NewAPILogger(dir)

	model := "gemini-3.8-flash-high"
	record := APILogRecord{
		Timestamp:   "2026-09-10T12:00:00Z",
		RequestID:   "req-12345",
		Method:      "POST",
		Route:       "/v1/chat/completions",
		Status:      200,
		Code:        "",
		DurationMS:  150,
		Stream:      false,
		Model:       &model,
		QueueWaitMS: 10,
		ClientClass: "sdk",
	}

	logger.Log(record)

	data, err := os.ReadFile(logger.logPath)
	if err != nil {
		t.Fatalf("expected api.log to exist: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &parsed); err != nil {
		t.Fatalf("failed to parse log JSON: %v", err)
	}

	if parsed["request_id"] != "req-12345" || parsed["route"] != "/v1/chat/completions" || parsed["status"] != float64(200) {
		t.Errorf("unexpected log content: %+v", parsed)
	}
	if _, ok := parsed["prompt"]; ok {
		t.Errorf("prompt must never be logged")
	}
}

func TestAPILogger_ConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	logger := NewAPILogger(dir)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			logger.Log(APILogRecord{
				Timestamp:   "2026-09-10T12:00:00Z",
				RequestID:   "concurrent-id",
				Method:      "GET",
				Route:       "/v1/models",
				Status:      200,
				DurationMS:  5,
				ClientClass: "cli",
			})
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(logger.logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 20 {
		t.Fatalf("expected 20 log lines, got %d", len(lines))
	}
}

func TestAPILogger_SizeLimit(t *testing.T) {
	dir := t.TempDir()
	logger := NewAPILogger(dir)
	var warnBuf bytes.Buffer
	logger.warnWriter = &warnBuf
	logger.maxBytes = 300 // small limit for testing

	for i := 0; i < 10; i++ {
		logger.Log(APILogRecord{
			Timestamp:   "2026-09-10T12:00:00Z",
			RequestID:   "req-bounded-id",
			Method:      "GET",
			Route:       "/v1/models",
			Status:      200,
			DurationMS:  5,
			ClientClass: "cli",
		})
	}

	if !logger.disabled {
		t.Errorf("logger should be disabled after exceeding limit")
	}
	if warnBuf.Len() == 0 {
		t.Errorf("expected warning to be written once")
	}

	// Verify file size does not exceed maxBytes
	fi, err := os.Stat(logger.logPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 300 {
		t.Errorf("file size %d exceeds limit 300", fi.Size())
	}
}

func TestClassifyClient(t *testing.T) {
	tests := []struct {
		ua   string
		want string
	}{
		{"curl/7.88.1", "cli"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64)", "browser"},
		{"OpenAI/Python 1.0.0", "sdk"},
		{"python-requests/2.31.0", "sdk"},
		{"UnknownAgent/1.0", "unknown"},
		{"", "unknown"},
	}
	for _, tc := range tests {
		got := ClassifyClient(tc.ua)
		if got != tc.want {
			t.Errorf("ClassifyClient(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}
