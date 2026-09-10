package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	defaultAPILogMaxBytes int64 = 10 * 1024 * 1024 // 10 MiB
)

// APILogRecord contains only the strictly bounded allowlisted fields for api.log.
type APILogRecord struct {
	Timestamp   string  `json:"timestamp"`
	RequestID   string  `json:"request_id"`
	Method      string  `json:"method"`
	Route       string  `json:"route"`
	Status      int     `json:"status"`
	Code        string  `json:"code,omitempty"`
	DurationMS  int64   `json:"duration_ms"`
	Stream      bool    `json:"stream"`
	Model       *string `json:"model"`
	QueueWaitMS int64   `json:"queue_wait_ms"`
	ClientClass string  `json:"client_class"`
}

// APILogger logs API events to a dedicated api.log file with strict bounds.
type APILogger struct {
	mu         sync.Mutex
	logPath    string
	maxBytes   int64
	disabled   bool
	warnedOnce bool
	warnWriter io.Writer
}

func NewAPILogger(dir string) *APILogger {
	return &APILogger{
		logPath:    filepath.Join(dir, "api.log"),
		maxBytes:   defaultAPILogMaxBytes,
		warnWriter: os.Stderr,
	}
}

// Log writes one allowlisted JSONL record. When size limit is reached or write
// fails, it disables further logging, preserves the file, and emits one sanitized
// warning to stderr. It never writes to bot.log or buffers unbounded data.
func (l *APILogger) Log(r APILogRecord) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.disabled {
		return
	}

	// Check existing file size
	if fi, err := os.Stat(l.logPath); err == nil {
		if fi.Size() >= l.maxBytes {
			l.disableAndWarn("api logger: size limit reached or write failure, disabling further API logs\n")
			return
		}
	}

	data, err := json.Marshal(r)
	if err != nil {
		return
	}
	data = append(data, '\n')

	// Check if this write would exceed maxBytes
	if fi, err := os.Stat(l.logPath); err == nil {
		if fi.Size()+int64(len(data)) > l.maxBytes {
			l.disableAndWarn("api logger: size limit reached or write failure, disabling further API logs\n")
			return
		}
	}

	f, err := os.OpenFile(l.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		l.disableAndWarn("api logger: size limit reached or write failure, disabling further API logs\n")
		return
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		l.disableAndWarn("api logger: size limit reached or write failure, disabling further API logs\n")
		return
	}
}

func (l *APILogger) disableAndWarn(msg string) {
	l.disabled = true
	if !l.warnedOnce {
		l.warnedOnce = true
		if l.warnWriter != nil {
			_, _ = io.WriteString(l.warnWriter, msg)
		}
	}
}

// ClassifyClient assigns User-Agent to a closed client-class enum.
func ClassifyClient(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "curl") || strings.Contains(ua, "wget") || strings.Contains(ua, "httpie") || strings.Contains(ua, "agy"):
		return "cli"
	case strings.Contains(ua, "mozilla") || strings.Contains(ua, "chrome") || strings.Contains(ua, "safari") || strings.Contains(ua, "edge"):
		return "browser"
	case strings.Contains(ua, "openai") || strings.Contains(ua, "python") || strings.Contains(ua, "node") || strings.Contains(ua, "go-http") || strings.Contains(ua, "axios") || strings.Contains(ua, "langchain"):
		return "sdk"
	default:
		return "unknown"
	}
}
