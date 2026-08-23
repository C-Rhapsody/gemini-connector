package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const salvageFixtureConv = "11111111-2222-3333-4444-555555555555"

func writeSalvageFixture(t *testing.T, lines []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)

	old := agyBrainDirOverride
	agyBrainDirOverride = filepath.Join(dir, "brain")
	t.Cleanup(func() { agyBrainDirOverride = old })

	path := agyBrainTranscriptPath(salvageFixtureConv)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, b...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// turnStart corresponds to 2026-08-23T08:09:36Z in the fixtures below.
var salvageTurnStart = time.Date(2026, 8, 23, 8, 9, 36, 0, time.UTC)

func baseSalvageLines() []map[string]any {
	return []map[string]any{
		{"step_index": 21, "source": "USER_EXPLICIT", "type": "USER_INPUT", "status": "DONE",
			"created_at": "2026-08-22T10:00:00Z", "content": "<USER_REQUEST>\nolder question\n</USER_REQUEST>"},
		{"step_index": 22, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE",
			"created_at": "2026-08-22T10:00:05Z", "content": "older answer"},
		{"step_index": 24, "source": "USER_EXPLICIT", "type": "USER_INPUT", "status": "DONE",
			"created_at": "2026-08-23T08:09:36Z",
			"content":    "<USER_REQUEST>\nhttps://www.reddit.com/r/opencodeCLI/s/vdTa5Y3os1\n</USER_REQUEST>"},
		{"step_index": 23, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE",
			"created_at": "2026-08-23T08:09:38Z",
			"tool_calls": []map[string]any{{"name": "read_url_content"}}},
		{"step_index": 27, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE",
			"created_at": "2026-08-23T08:09:45Z",
			"tool_calls": []map[string]any{{"name": "grep_search"}}},
		{"step_index": 31, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE",
			"created_at": "2026-08-23T08:09:51Z", "content": "final salvaged answer"},
	}
}

func TestSalvageTurnResponse_RecoversLatestAnswer(t *testing.T) {
	writeSalvageFixture(t, baseSalvageLines())

	got := salvageTurnResponse(salvageFixtureConv,
		"https://www.reddit.com/r/opencodeCLI/s/vdTa5Y3os1", salvageTurnStart)
	if got != "final salvaged answer" {
		t.Fatalf("salvage returned %q", got)
	}
}

func TestSalvageTurnResponse_IgnoresResponsesBeforeTurn(t *testing.T) {
	lines := baseSalvageLines()
	writeSalvageFixture(t, lines)

	got := salvageTurnResponse(salvageFixtureConv, "older question", salvageTurnStart)
	if got != "" {
		t.Fatalf("expected no salvage for an unrelated prompt, got %q", got)
	}
}

func TestSalvageTurnResponse_SkipsToolCallSteps(t *testing.T) {
	lines := baseSalvageLines()
	// Drop the final text step so only tool-call steps remain this turn.
	lines = lines[:len(lines)-1]
	writeSalvageFixture(t, lines)

	got := salvageTurnResponse(salvageFixtureConv,
		"https://www.reddit.com/r/opencodeCLI/s/vdTa5Y3os1", salvageTurnStart)
	if got != "" {
		t.Fatalf("tool-call-only turns must not be salvaged, got %q", got)
	}
}

func TestSalvageTurnResponse_MissingTranscript(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)

	old := agyBrainDirOverride
	agyBrainDirOverride = filepath.Join(dir, "brain")
	t.Cleanup(func() { agyBrainDirOverride = old })

	if got := salvageTurnResponse(salvageFixtureConv, "anything", salvageTurnStart); got != "" {
		t.Fatalf("missing transcript must yield empty result, got %q", got)
	}
}

func TestPromptFragment(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"simple url", "https://example.com/x", "https://example.com/x"},
		{"multiline takes first non-empty line", "\n\n첫 줄\n둘째 줄", "첫 줄"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := promptFragment(c.in); got != c.want && len([]rune(got)) <= 64 {
			t.Fatalf("%s: promptFragment(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	long := ""
	for i := 0; i < 100; i++ {
		long += "가"
	}
	if got := promptFragment(long); len([]rune(got)) != 64 {
		t.Fatalf("fragment not capped: %d runes", len([]rune(got)))
	}
}
