package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestReplyPreservation(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		mustContain []string
		plain       bool
		attachAfter bool
	}{
		{
			name:        "inline code with segment filename",
			input:       "첫 번째 세그먼트(`_0000.mp4`)가 약 860MB까지 생성되었습니다.",
			mustContain: []string{"_0000.mp4"},
			attachAfter: true,
		},
		{
			name:        "bare filenames in explanation",
			input:       "결과는 result.csv 와 report.pdf 파일에 저장했습니다.",
			mustContain: []string{"result.csv", "report.pdf"},
			attachAfter: true,
		},
		{
			name:        "repeated filename with prefixes",
			input:       "원본 sample.mp4 와 분할본 sample_0000.mp4, sample_0001.mp4 완료",
			mustContain: []string{"sample.mp4", "sample_0000.mp4", "sample_0001.mp4"},
			attachAfter: true,
		},
		{
			name:        "complex filename with at and dashes",
			input:       "처리 완료: 489155.com@FC2PPV-4973050-1.mp4",
			mustContain: []string{"489155.com@FC2PPV-4973050-1.mp4"},
			attachAfter: true,
		},
		{
			name:        "windows absolute path",
			input:       "경로: C:\\Users\\bitflow\\test_video.mp4",
			mustContain: []string{"test_video.mp4"},
			attachAfter: true,
		},
		{
			name:        "relative path",
			input:       "상대 경로 ./output/summary.csv 참조",
			mustContain: []string{"summary.csv"},
			attachAfter: true,
		},
		{
			name:        "url containing media extension",
			input:       "다운로드 링크: https://example.com/files/archive.zip",
			mustContain: []string{"https://example.com/files/archive.zip"},
			attachAfter: true,
		},
		{
			name:        "markdown table with filenames",
			input:       "| 파일명 | 상태 |\n| clip.mp4 | 완료 |\n| data.csv | 대기 |",
			mustContain: []string{"clip.mp4", "data.csv"},
			attachAfter: true,
		},
		{
			name:        "fenced code block with filenames",
			input:       "```bash\nffmpeg -i input.mp4 output.mp4\n```",
			mustContain: []string{"input.mp4", "output.mp4"},
			attachAfter: true,
		},
		{
			name:        "plain text mode with filenames",
			input:       "파일 목록: test_0000.mp4, output.xlsx",
			mustContain: []string{"test_0000.mp4", "output.xlsx"},
			plain:       true,
			attachAfter: true,
		},
		{
			name:        "attachAfter zero preserved",
			input:       "설명: sample_0000.mp4",
			mustContain: []string{"sample_0000.mp4"},
			attachAfter: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var receivedChunks []string

			adapter := &TelegramAdapter{
				chatID: 12345678,
				sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
					mu.Lock()
					defer mu.Unlock()
					receivedChunks = append(receivedChunks, text)
					return nil
				},
				collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable {
					return nil
				},
			}

			opts := SendOptions{
				Plain: tt.plain,
			}
			if tt.attachAfter {
				opts.AttachAfter = time.Now()
			}

			err := adapter.Send("12345678", tt.input, opts)
			if err != nil {
				t.Fatalf("Send failed: %v", err)
			}

			mu.Lock()
			combined := strings.Join(receivedChunks, "\n")
			mu.Unlock()

			for _, want := range tt.mustContain {
				if !strings.Contains(combined, want) {
					t.Errorf("sent text does not contain %q\nCombined text:\n%s", want, combined)
				}
			}
		})
	}
}

func TestReplyPreservationWithAttachmentsAndExclusions(t *testing.T) {
	var mu sync.Mutex
	var sentTexts []string
	var sentAttachments []string
	var receivedExclude exclusionSet

	adapter := &TelegramAdapter{
		chatID: 12345678,
		sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
			mu.Lock()
			defer mu.Unlock()
			sentTexts = append(sentTexts, text)
			return nil
		},
		sendAttachmentFn: func(chatID int64, path string, replyToID int) error {
			mu.Lock()
			defer mu.Unlock()
			sentAttachments = append(sentAttachments, path)
			return nil
		},
		collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable {
			mu.Lock()
			defer mu.Unlock()
			receivedExclude = exclude
			return []deliverable{
				{path: "C:\\mock\\output_0000.mp4", deletable: false},
				{path: "C:\\mock\\result.csv", deletable: false},
			}
		},
	}

	inputText := "인코딩 결과 `output_0000.mp4` 와 `result.csv` 가 생성되었습니다."
	opts := SendOptions{
		AttachAfter:        time.Now(),
		ExcludeAttachments: []string{"C:\\mock\\user_inbound.jpg"},
	}

	if err := adapter.Send("12345678", inputText, opts); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Verify attachments were delivered
	if len(sentAttachments) != 2 {
		t.Errorf("expected 2 attachments sent, got %d (%v)", len(sentAttachments), sentAttachments)
	}

	// Verify exclusion set contained user inbound file
	if receivedExclude == nil || !receivedExclude[normalizeDeliverablePath("C:\\mock\\user_inbound.jpg")] {
		t.Errorf("user inbound file was not properly excluded")
	}

	// Verify reply text preserved filenames
	combined := strings.Join(sentTexts, "\n")
	for _, want := range []string{"output_0000.mp4", "result.csv"} {
		if !strings.Contains(combined, want) {
			t.Errorf("sent text does not contain %q\nCombined text:\n%s", want, combined)
		}
	}
}

func TestReplyPreservation_RichAIResponseAndCommands(t *testing.T) {
	var mu sync.Mutex
	var richCalls int
	var ordinaryCalls int
	var sentAttachments []string

	adapter := &TelegramAdapter{
		chatID: 12345678,
		richConfig: TelegramRichConfig{
			Enabled:    true,
			MathEscape: "numeric",
			ChatID:     12345678,
		},
		makeRequestFn: func(endpoint string, params tgbotapi.Params) (*tgbotapi.APIResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			richCalls++
			return &tgbotapi.APIResponse{
				Ok:     true,
				Result: []byte(`{"message_id": 999, "chat": {"id": 12345678}}`),
			}, nil
		},
		sendOneFn: func(chatID int64, text string, parseMode string, replyToID int) error {
			mu.Lock()
			defer mu.Unlock()
			ordinaryCalls++
			return nil
		},
		sendAttachmentFn: func(chatID int64, path string, replyToID int) error {
			mu.Lock()
			defer mu.Unlock()
			sentAttachments = append(sentAttachments, path)
			return nil
		},
		collectDeliverablesFn: func(after time.Time, exclude exclusionSet) []deliverable {
			return []deliverable{
				{path: "C:\\mock\\output.mp4", deletable: false},
			}
		},
	}

	// 1. Command send (AttachAfter is zero): MUST use ordinary sendOneFn, never rich
	cmdOpts := SendOptions{
		AttachAfter: time.Time{},
	}
	err := adapter.Send("12345678", "/status: running normally", cmdOpts)
	if err != nil {
		t.Fatalf("command send failed: %v", err)
	}
	mu.Lock()
	if richCalls != 0 {
		t.Errorf("command send must not invoke Rich API, got %d rich calls", richCalls)
	}
	if ordinaryCalls != 1 {
		t.Errorf("command send must invoke ordinary sendOneFn once, got %d", ordinaryCalls)
	}
	mu.Unlock()

	// 2. AI response with math and attachment: MUST invoke Rich API and still deliver attachment
	aiOpts := SendOptions{
		AttachAfter: time.Now(),
	}
	err = adapter.Send("12345678", "Here is result `output.mp4` with formula \\(E = mc^2\\)", aiOpts)
	if err != nil {
		t.Fatalf("AI send failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if richCalls != 1 {
		t.Errorf("AI rich send should invoke Rich API once, got %d", richCalls)
	}
	if len(sentAttachments) != 1 || sentAttachments[0] != "C:\\mock\\output.mp4" {
		t.Errorf("expected attachment output.mp4 delivered, got %v", sentAttachments)
	}
}
