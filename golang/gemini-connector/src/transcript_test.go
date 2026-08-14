package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendLoadTranscript(t *testing.T) {
	dir := t.TempDir()
	old := transcriptDirOverride
	transcriptDirOverride = dir
	defer func() { transcriptDirOverride = old }()

	appendTranscript("conv-a", "user", "안녕")
	appendTranscript("conv-a", "assistant", "반갑습니다")
	appendTranscript("conv-a", "user", "요약해줘")
	appendTranscript("conv-b", "user", "다른 대화")

	turns := loadTranscript("conv-a")
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(turns))
	}
	if turns[0].Role != "user" || turns[0].Content != "안녕" {
		t.Errorf("unexpected first turn: %+v", turns[0])
	}
	if turns[2].Content != "요약해줘" {
		t.Errorf("unexpected last turn: %+v", turns[2])
	}

	other := loadTranscript("conv-b")
	if len(other) != 1 {
		t.Fatalf("expected 1 turn for conv-b, got %d", len(other))
	}

	deleteTranscript("conv-a")
	if _, err := os.Stat(filepath.Join(dir, "conv-a.json")); !os.IsNotExist(err) {
		t.Errorf("transcript file for conv-a should be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "conv-b.json")); err != nil {
		t.Errorf("transcript file for conv-b should still exist")
	}
}

func TestBuildSummaryPromptEmpty(t *testing.T) {
	if prompt := buildSummaryPrompt("no-such-conv"); prompt != "" {
		t.Errorf("expected empty prompt for unknown conversation, got %q", prompt)
	}
}

func TestBuildSummaryPromptCapsTurns(t *testing.T) {
	dir := t.TempDir()
	old := transcriptDirOverride
	transcriptDirOverride = dir
	defer func() { transcriptDirOverride = old }()

	for i := 0; i < 60; i++ {
		appendTranscript("conv-long", "user", "질문")
		appendTranscript("conv-long", "assistant", "답변")
	}

	prompt := buildSummaryPrompt("conv-long")
	if !strings.Contains(prompt, "요약") {
		t.Errorf("prompt should contain summary instruction")
	}
	if !strings.Contains(prompt, "생략") {
		t.Errorf("prompt should indicate omitted earlier turns")
	}
}
