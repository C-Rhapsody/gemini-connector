package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExtractNimImageBytes_ArtifactsShape(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte("fakepngbytes"))
	body := map[string]any{
		"artifacts": []any{
			map[string]any{"base64": raw, "mime_type": "image/png"},
		},
	}
	got := extractNimImageBytes(body)
	if string(got) != "fakepngbytes" {
		t.Fatalf("unexpected bytes: %q", got)
	}
}

func TestExtractNimImageBytes_UnpaddedURLSafe(t *testing.T) {
	// 3 bytes -> unpadded, URL-safe base64 ("aGV-_w" style)
	raw := base64.RawURLEncoding.EncodeToString([]byte("abc"))
	body := map[string]any{"artifacts": []any{map[string]any{"base64": raw}}}
	got := extractNimImageBytes(body)
	if string(got) != "abc" {
		t.Fatalf("url-safe/unpadded decoding failed: %q", got)
	}
}

func TestExtractNimImageBytes_DataMirrorShape(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte("datashape"))
	body := map[string]any{"data": []any{map[string]any{"b64_json": raw}}}
	if got := extractNimImageBytes(body); string(got) != "datashape" {
		t.Fatalf("data shape failed: %q", got)
	}
}

func TestExtractNimImageBytes_SingletonShape(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte("singleton"))
	body := map[string]any{"image": raw}
	if got := extractNimImageBytes(body); string(got) != "singleton" {
		t.Fatalf("singleton shape failed: %q", got)
	}
}

func TestExtractNimImageBytes_NoneOrCorrupt(t *testing.T) {
	if got := extractNimImageBytes(map[string]any{}); got != nil {
		t.Fatalf("empty body should yield nil, got %q", got)
	}
	corrupt := map[string]any{"artifacts": []any{map[string]any{"base64": "!!!not-base64!!!"}}}
	if got := extractNimImageBytes(corrupt); got != nil {
		t.Fatalf("corrupt base64 should be skipped, got %q", got)
	}
}

func TestImageFileExt(t *testing.T) {
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A}, make([]byte, 4)...)
	jpg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	if ext := imageFileExt(png); ext != ".png" {
		t.Fatalf("png magic: %s", ext)
	}
	if ext := imageFileExt(jpg); ext != ".jpg" {
		t.Fatalf("jpg magic: %s", ext)
	}
	if ext := imageFileExt([]byte{1, 2}); ext == "" {
		t.Fatal("unknown bytes should still return a default extension")
	}
}

func TestRandomSeed(t *testing.T) {
	s1, err := randomSeed()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := randomSeed()
	if err != nil {
		t.Fatal(err)
	}
	const maxSeed = int64(4294967295) // NIM validates seed < 2^32
	if s1 < 0 || s1 > maxSeed || s2 < 0 || s2 > maxSeed {
		t.Fatalf("seeds must be in [0, %d]: %d %d", maxSeed, s1, s2)
	}
}

func TestCleanTranslatedPrompt(t *testing.T) {
	in := `  "a cat sitting by the window"  `
	if got := cleanTranslatedPrompt(in); got != "a cat sitting by the window" {
		t.Fatalf("got %q", got)
	}
}

func TestGenerateNimImage_Success(t *testing.T) {
	want := []byte("generated-image-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req nimImageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.Width != nimWidth || req.Height != nimHeight || req.Steps != nimSteps || req.Seed <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"artifacts": []any{map[string]any{
				"base64":    base64.StdEncoding.EncodeToString(want),
				"mime_type": "image/png",
			}},
		})
	}))
	defer srv.Close()

	old := nimInvokeURL
	nimInvokeURL = srv.URL
	defer func() { nimInvokeURL = old }()

	got, err := generateNimImage(context.Background(), "test-key", "a prompt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("image mismatch: %q", got)
	}
}

func TestGenerateNimImage_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	}))
	defer srv.Close()

	old := nimInvokeURL
	nimInvokeURL = srv.URL
	defer func() { nimInvokeURL = old }()

	_, err := generateNimImage(context.Background(), "test-key", "p")
	if err == nil {
		t.Fatal("500 must produce an error")
	}
	if !strings.Contains(err.Error(), "NVIDIA") {
		t.Fatalf("error should be user-facing: %v", err)
	}
}

func TestGenerateNimImage_TimeoutClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	old := nimInvokeURL
	nimInvokeURL = srv.URL
	defer func() { nimInvokeURL = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := generateNimImage(ctx, "test-key", "p")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline classification, got %v", err)
	}
}
