package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The 2026-08-25 incident: a user-sent photo (downloads\51867851_26597_01.jpg)
// and agy's temp copy (.tempmediaStorage\media_*.jpg) were both re-delivered
// as "AI attachments". These tests pin the exclusion rules that prevent it.

func TestNormalizeDeliverablePath(t *testing.T) {
	got := normalizeDeliverablePath("downloads\\..\\downloads\\Photo.JPG")
	if !strings.HasSuffix(got, strings.ToLower(filepath.Join("downloads", "photo.jpg"))) {
		t.Fatalf("path not cleaned+lowercased: %q", got)
	}
	if filepath.IsAbs(got) != true {
		t.Fatalf("normalized path must be absolute: %q", got)
	}
}

func TestNewExclusionSetMatchesCaseAndFormVariants(t *testing.T) {
	raw := []string{filepath.Join("C:", "Bot", "Downloads", "51867851_26597_01.JPG")}
	set := newExclusionSet(raw)
	if set == nil {
		t.Fatal("expected non-nil exclusion set")
	}
	variants := []string{
		filepath.Join("C:", "Bot", "Downloads", "51867851_26597_01.jpg"),
		strings.ToLower(filepath.Join("c:", "bot", "downloads", "51867851_26597_01.jpg")),
	}
	for _, v := range variants {
		if !set[normalizeDeliverablePath(v)] {
			t.Fatalf("variant not excluded: %q", v)
		}
	}
}

func TestNewExclusionSetEmptyInputYieldsNil(t *testing.T) {
	if s := newExclusionSet(nil); s != nil {
		t.Fatalf("nil input should yield nil set, got %v", s)
	}
	if s := newExclusionSet([]string{}); s != nil {
		t.Fatalf("empty input should yield nil set, got %v", s)
	}
	if s := newExclusionSet([]string{""}); s == nil || len(s) != 0 {
		t.Fatalf("blank-only input should yield empty set, got %v", s)
	}
}

func TestIsTempMediaStoragePath(t *testing.T) {
	positive := []string{
		filepath.Join("home", ".gemini", "antigravity-cli", "brain", "conv-id", ".tempmediaStorage", "media_1787636101546.jpg"),
		"brain/conv/.tempMediaStorage/media.jpg",
	}
	negative := []string{
		filepath.Join("project", "tempmediastorage_fake", "out.png"), // substring must not match
		filepath.Join("project", "out.png"),
		"",
	}
	for _, p := range positive {
		if !isTempMediaStoragePath(p) {
			t.Fatalf("expected true for %q", p)
		}
	}
	for _, p := range negative {
		if isTempMediaStoragePath(p) {
			t.Fatalf("expected false for %q", p)
		}
	}
}
