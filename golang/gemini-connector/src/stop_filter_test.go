package main

import (
	"testing"
)

func TestStopFilter_NoStopSeqs(t *testing.T) {
	f := NewStopFilter(nil)
	emit, hit := f.Feed("hello world")
	if hit || emit != "hello world" {
		t.Fatalf("expected 'hello world', got %q (hit: %v)", emit, hit)
	}
	flushed := f.Flush()
	if flushed != "" {
		t.Fatalf("expected empty flush, got %q", flushed)
	}
}

func TestStopFilter_SingleChunkExactStop(t *testing.T) {
	f := NewStopFilter([]string{"<END>"})
	emit, hit := f.Feed("Hello <END> world")
	if !hit {
		t.Fatalf("expected hit stop")
	}
	if emit != "Hello " {
		t.Fatalf("expected 'Hello ', got %q", emit)
	}
	emit2, hit2 := f.Feed("more text")
	if !hit2 || emit2 != "" {
		t.Fatalf("expected no emit after stopped, got %q", emit2)
	}
	if f.Flush() != "" {
		t.Fatalf("expected empty flush after stop")
	}
}

func TestStopFilter_SplitAcrossChunks(t *testing.T) {
	f := NewStopFilter([]string{"<END>"})

	emit1, hit1 := f.Feed("Hello ")
	if hit1 || emit1 != "Hello " {
		t.Fatalf("chunk 1 expected 'Hello ', got %q (hit: %v)", emit1, hit1)
	}

	emit2, hit2 := f.Feed("<EN")
	if hit2 || emit2 != "" {
		t.Fatalf("chunk 2 expected held back (''), got %q (hit: %v)", emit2, hit2)
	}

	emit3, hit3 := f.Feed("D> world")
	if !hit3 || emit3 != "" {
		t.Fatalf("chunk 3 expected hit stop with empty emit, got %q (hit: %v)", emit3, hit3)
	}

	if f.Flush() != "" {
		t.Fatalf("flush expected empty, got %q", f.Flush())
	}
}

func TestStopFilter_FalseAlarmPrefixFlushed(t *testing.T) {
	f := NewStopFilter([]string{"<END>"})

	emit1, hit1 := f.Feed("Hello <EN")
	if hit1 || emit1 != "Hello " {
		t.Fatalf("expected 'Hello ', got %q", emit1)
	}

	// Next chunk does NOT continue "<END>"
	emit2, hit2 := f.Feed("TRY")
	if hit2 || emit2 != "<ENTRY" {
		t.Fatalf("expected '<ENTRY', got %q", emit2)
	}

	flushed := f.Flush()
	if flushed != "" {
		t.Fatalf("expected empty flush, got %q", flushed)
	}
}

func TestStopFilter_MultipleStopSequences(t *testing.T) {
	f := NewStopFilter([]string{"\n\n", "STOP", "END"})

	emit1, hit1 := f.Feed("Step 1")
	if hit1 || emit1 != "Step 1" {
		t.Fatalf("expected 'Step 1', got %q", emit1)
	}

	emit2, hit2 := f.Feed("\n")
	if hit2 || emit2 != "" {
		t.Fatalf("expected '' (held back newline), got %q", emit2)
	}

	emit3, hit3 := f.Feed("\nmore")
	if !hit3 || emit3 != "" {
		t.Fatalf("expected hit on \\n\\n with empty emit, got %q", emit3)
	}
	if f.HitStop() != "\n\n" {
		t.Fatalf("expected HitStop '\\n\\n', got %q", f.HitStop())
	}
}

func TestStopFilter_FlushRemainingPrefix(t *testing.T) {
	f := NewStopFilter([]string{"<END>"})

	emit1, hit1 := f.Feed("Hello <EN")
	if hit1 || emit1 != "Hello " {
		t.Fatalf("expected 'Hello ', got %q", emit1)
	}

	// Stream ends here
	flushed := f.Flush()
	if flushed != "<EN" {
		t.Fatalf("expected flushed '<EN', got %q", flushed)
	}
}
