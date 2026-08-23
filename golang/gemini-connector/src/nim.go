package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// nimInvokeURL is the NVIDIA NIM image generation endpoint. It is a package
// variable so tests can point it at a stub server.
var nimInvokeURL = "https://ai.api.nvidia.com/v1/genai/black-forest-labs/flux.2-klein-4b"

const (
	nimRequestTimeout = 120 * time.Second
	nimWidth          = 768
	nimHeight         = 1344
	nimSteps          = 4
	// nimMaxPromptChars is the hard prompt limit enforced by the NIM
	// endpoint (HTTP 422 string_too_long beyond it), counted in Unicode
	// characters.
	nimMaxPromptChars = 800
)

// errNimContentFiltered marks responses rejected by NVIDIA's safety
// guardrail (Cosmos-1.0). Callers classify it via errors.Is.
var errNimContentFiltered = errors.New("nvidia safety filter blocked the request")

var nimHTTPClient = &http.Client{}

type nimImageRequest struct {
	Prompt string `json:"prompt"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Seed   int64  `json:"seed"`
	Steps  int    `json:"steps"`
}

// randomSeed returns a fresh seed so identical prompts yield different
// images on every call. NIM validates seed < 2^32, so only a uint32 range is
// produced.
func randomSeed() (int64, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint32(b[:])), nil
}

// readBodySnippet drains up to max bytes of r for logging / short reports.
func readBodySnippet(r io.Reader, max int) string {
	b, err := io.ReadAll(io.LimitReader(r, int64(max)))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// clampNimPrompt caps the prompt at nimMaxPromptChars Unicode characters so
// the endpoint's hard validation can never reject the request. The full
// original text is never logged; only its length is.
func clampNimPrompt(prompt string) string {
	r := []rune(prompt)
	if len(r) <= nimMaxPromptChars {
		return prompt
	}
	log.Printf("NIM prompt too long (%d chars); truncating to %d", len(r), nimMaxPromptChars)
	return string(r[:nimMaxPromptChars])
}

// generateNimImage posts the prompt to NVIDIA NIM and returns the decoded
// image bytes. Non-2xx replies, network failures and timeouts become
// descriptive errors; the raw response body is logged, never shown whole.
func generateNimImage(ctx context.Context, apiKey string, prompt string) ([]byte, error) {
	seed, err := randomSeed()
	if err != nil {
		return nil, fmt.Errorf("failed to generate seed: %w", err)
	}
	payload, err := json.Marshal(nimImageRequest{
		Prompt: clampNimPrompt(prompt),
		Width:  nimWidth,
		Height: nimHeight,
		Seed:   seed,
		Steps:  nimSteps,
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, nimRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nimInvokeURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := nimHTTPClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// Keep the chain (%w) so callers can classify timeouts via
			// errors.Is and show a dedicated message.
			return nil, fmt.Errorf("NVIDIA 응답 시간 초과 (%s): %w", nimRequestTimeout, err)
		}
		return nil, fmt.Errorf("NVIDIA 연결 실패: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := readBodySnippet(resp.Body, 500)
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			log.Printf("NIM auth rejected (%d): %s", resp.StatusCode, snippet)
			return nil, fmt.Errorf("NVIDIA API 키가 거부되었습니다 (%d). NVIDIA_API_KEY를 확인하세요", resp.StatusCode)
		case resp.StatusCode == 429:
			retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
			if retryAfter != "" {
				return nil, fmt.Errorf("NVIDIA 요청 한도 초과 (429). %s초 후 재시도 가능", retryAfter)
			}
			return nil, errors.New("NVIDIA 요청 한도 초과 (429). 잠시 후 다시 시도해 주세요")
		default:
			log.Printf("NIM HTTP %d: %s", resp.StatusCode, snippet)
			return nil, fmt.Errorf("NVIDIA 서버 오류 (%d)", resp.StatusCode)
		}
	}

	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		log.Printf("NIM response is not valid JSON: %v", err)
		return nil, errors.New("NVIDIA 응답을 해석하지 못했습니다")
	}
	if nimContentFiltered(parsed) {
		log.Printf("NIM generation blocked by NVIDIA safety guardrail (CONTENT_FILTERED)")
		return nil, fmt.Errorf("%w (CONTENT_FILTERED)", errNimContentFiltered)
	}
	img := extractNimImageBytes(parsed)
	if img == nil {
		log.Printf("NIM response carried no image data: %.500s", readBodySnippet(strings.NewReader(mustJSON(parsed)), 1000))
		return nil, errors.New("NVIDIA 응답에 이미지 데이터가 없습니다")
	}
	return img, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// nimContentFiltered reports whether the response indicates that NVIDIA's
// safety guardrail blocked generation. The flag appears as finishReason /
// finish_reason on artifacts or at the top level.
func nimContentFiltered(body map[string]any) bool {
	check := func(v any) bool {
		s, ok := v.(string)
		return ok && strings.Contains(strings.ToUpper(s), "CONTENT_FILTERED")
	}
	if arts, ok := body["artifacts"].([]any); ok {
		for _, a := range arts {
			m, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if check(m["finishReason"]) || check(m["finish_reason"]) {
				return true
			}
		}
	}
	return check(body["finishReason"]) || check(body["finish_reason"])
}

// extractNimImageBytes pulls the first base64-encoded asset out of a NIM
// response body. NIM shapes seen in the wild, checked in order:
//   - {"artifacts": [{"base64": "..."}]}            (SDXL / FLUX standard)
//   - {"data": [{"b64_json": "..."}]}               (OpenAI-mirror shape)
//   - {"image": "..."}                              (singleton convenience)
//
// Base64 may arrive URL-safe or without padding; both are normalised.
func extractNimImageBytes(body map[string]any) []byte {
	if arts, ok := body["artifacts"].([]any); ok {
		for _, a := range arts {
			m, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if b := decodeNimBase64(m["base64"]); b != nil {
				return b
			}
			if b := decodeNimBase64(m["b64_json"]); b != nil {
				return b
			}
		}
	}
	if data, ok := body["data"].([]any); ok {
		for _, d := range data {
			m, ok := d.(map[string]any)
			if !ok {
				continue
			}
			if b := decodeNimBase64(m["b64_json"]); b != nil {
				return b
			}
			if b := decodeNimBase64(m["base64"]); b != nil {
				return b
			}
		}
	}
	for _, key := range []string{"image", "video", "audio"} {
		if b := decodeNimBase64(body[key]); b != nil {
			return b
		}
	}
	return nil
}

func decodeNimBase64(v any) []byte {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	normalized := strings.NewReplacer("-", "+", "_", "/").Replace(s)
	if pad := len(normalized) % 4; pad != 0 {
		normalized += strings.Repeat("=", 4-pad)
	}
	b, err := base64.StdEncoding.DecodeString(normalized)
	if err != nil || len(b) == 0 {
		return nil
	}
	return b
}

// imageFileExt chooses a file extension from image magic bytes.
func imageFileExt(b []byte) string {
	if len(b) >= 8 && bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G'}) {
		return ".png"
	}
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xD8 {
		return ".jpg"
	}
	return ".png"
}
