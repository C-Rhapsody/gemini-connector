package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func requireBalancedChunks(t *testing.T, chunks []string, limit int) {
	t.Helper()
	for i, c := range chunks {
		if len(c) > limit {
			t.Fatalf("chunk %d is %d bytes, exceeds limit %d", i, len(c), limit)
		}
		if !utf8.ValidString(c) {
			t.Fatalf("chunk %d is not valid UTF-8", i)
		}
		if n := strings.Count(c, "```"); n%2 != 0 {
			t.Fatalf("chunk %d has unbalanced fences (%d): %q", i, n, c)
		}
	}
}

func TestSplitTelegramChunks_ShortInputSingleChunk(t *testing.T) {
	got := splitTelegramChunks("hello world", 100)
	if len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("unexpected chunks: %q", got)
	}
}

// Reproduces the reported bug: a fenced Mermaid block starting shortly before
// the size limit used to be cut open because the splitter preferred an inner
// newline. The block must instead move to the next chunk whole.
func TestSplitTelegramChunks_KeepsSmallFencedBlockIntact(t *testing.T) {
	block := "```mermaid\n" +
		"A[\"가\"] --> B[\"나\"]\n" +
		"B[\"나\"] --> C[\"다\"]\n" +
		"C[\"다\"] --> D[\"라\"]\n" +
		"```\n"
	input := strings.Repeat("x", 3950) + "\n" + block + "tail\n"

	chunks := splitTelegramChunks(input, 4000)
	requireBalancedChunks(t, chunks, 4000)

	for _, c := range chunks {
		if strings.Contains(c, block) {
			return
		}
	}
	t.Fatalf("mermaid block was split across chunks: %q", chunks)
}

// Mirrors the production payload: four fence markers at byte offsets
// 1237/1853/3697/4086 across 5047 bytes, where the second Mermaid block used
// to be severed by the 4000-byte cut.
func TestSplitTelegramChunks_ProductionShapeSecondMermaidIntact(t *testing.T) {
	filler := func(n int, ch byte) string { return strings.Repeat(string(rune(ch)), n) }
	// Fillers end with newlines so every fence marker sits on its own line,
	// mirroring real Markdown structure.
	b1 := "```mermaid\n" + filler(600, 'a') + "\n" + "```\n"
	mid := filler(1843, 'm') + "\n"
	b2 := "```mermaid\n" + filler(373, 'd') + "\n" + "```\n"
	tail := filler(960, 't') + "\n"
	input := filler(1236, 'p') + "\n" + b1 + mid + b2 + tail

	if got := len(input); got != 5047 {
		t.Fatalf("fixture length mismatch: %d", got)
	}

	chunks := splitTelegramChunks(input, 4000)
	requireBalancedChunks(t, chunks, 4000)
	if len(chunks) != 2 {
		t.Fatalf("expected exactly 2 chunks, got %d", len(chunks))
	}

	for _, c := range chunks {
		if strings.Contains(c, b2) {
			return
		}
	}
	t.Fatalf("second mermaid block was split across chunks (%d chunks): %q", len(chunks), chunks)
}

// Mirrors the tested JEUS summary response: prose sections interleaved with
// Mermaid and Java blocks, slightly over one message. Blocks must stay beside
// their introducing text, and the split should land on a section boundary so
// the trailing message is a coherent section rather than an orphan.
func TestSplitTelegramChunks_KeepsBlocksBesideProseAndRebalances(t *testing.T) {
	filler := func(n int, ch byte) string { return strings.Repeat(string(rune(ch)), n) }
	b1 := "```mermaid\n" + filler(600, 'a') + "\n```\n"
	java := "```java\n" + filler(360, 'j') + "\n```\n"
	b3 := "```mermaid\n" + filler(340, 'c') + "\n```\n"

	input := filler(300, 'p') + "\n" +
		"---\n# 제목\n\n" +
		b1 +
		"---\n### 섹션1 소개\n" +
		filler(900, 'q') + "\n" +
		java +
		filler(900, 'r') + "\n" +
		"---\n### 마지막 섹션 요약\n" +
		b3 +
		filler(750, 't') + "\n"

	chunks := splitTelegramChunks(input, 4000)
	requireBalancedChunks(t, chunks, 4000)

	if len(chunks) != 2 {
		t.Fatalf("expected exactly 2 chunks, got %d: %q", len(chunks), chunks)
	}
	first, last := chunks[0], chunks[1]
	for _, want := range []string{b1, java, "### 섹션1 소개"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first chunk lost %q: %q", want, first)
		}
	}
	if strings.Contains(first, b3) {
		t.Fatalf("last mermaid block should start the second message: %q", first)
	}
	if len(last) < telegramChunkMinTail {
		t.Fatalf("trailing message too small: %d bytes", len(last))
	}
	if !strings.HasPrefix(last, "### 마지막 섹션 요약") || !strings.Contains(last, b3) {
		t.Fatalf("second message should carry the final section whole: %q", last)
	}
}

// Without any section markers there is nothing to rebalance against; the
// splitter must still terminate with valid, size-bounded chunks.
func TestSplitTelegramChunks_PlainProseWithoutMarkers(t *testing.T) {
	input := strings.Repeat("x", 4300)
	chunks := splitTelegramChunks(input, 4000)

	requireBalancedChunks(t, chunks, 4000)
	if len(chunks) != 2 || strings.Join(chunks, "") != input {
		t.Fatalf("unexpected chunks: %d pieces", len(chunks))
	}
}

// A single fenced block larger than the limit is split, but every fragment
// must carry closed fences and the original language tag.
func TestSplitTelegramChunks_OversizedFenceClosedAndReopened(t *testing.T) {
	line := "    node[\"처리 단계\"] --> next\n"
	block := "```python\n" + strings.Repeat(line, 130) + "```\n"
	input := "intro\n" + block

	chunks := splitTelegramChunks(input, 4000)
	requireBalancedChunks(t, chunks, 4000)

	if len(chunks) < 2 {
		t.Fatalf("oversized block should span multiple chunks, got %d", len(chunks))
	}
	for i := 1; i < len(chunks); i++ {
		if !strings.HasPrefix(chunks[i], "```python\n") {
			head := chunks[i]
			if len(head) > 40 {
				head = head[:40]
			}
			t.Fatalf("continuation chunk %d lost the fence language: %q", i, head)
		}
	}
	if !strings.HasSuffix(chunks[len(chunks)-1], "```\n") {
		t.Fatalf("final chunk should close the fence: %q", chunks[len(chunks)-1])
	}
}

// Prose without fences must survive splitting losslessly, with multi-byte
// runes kept whole.
func TestSplitTelegramChunks_UTF8RunesNeverCut(t *testing.T) {
	input := strings.Repeat("핵심요약검증글자", 700) // 10500 bytes, no newlines
	chunks := splitTelegramChunks(input, 4000)

	requireBalancedChunks(t, chunks, 4000)
	if joined := strings.Join(chunks, ""); joined != input {
		t.Fatalf("chunks are not lossless: got %d bytes, want %d", len(joined), len(input))
	}
}

// An unterminated fence still yields balanced chunks: the last chunk closes
// it synthetically.
func TestSplitTelegramChunks_UnterminatedFenceClosedAtEnd(t *testing.T) {
	input := "앞머리말\n```mermaid\ngraph TD\n" + strings.Repeat("  n --> m\n", 600)

	chunks := splitTelegramChunks(input, 4000)
	requireBalancedChunks(t, chunks, 4000)
	if last := chunks[len(chunks)-1]; !strings.HasSuffix(last, "```\n") {
		t.Fatalf("dangling fence was not closed: %q", last)
	}
}
