package main

import (
	"strings"
)

// StopFilter inspects streaming text deltas and holds back trailing suffixes that
// could be the prefix of any configured stop sequence. This prevents leaking partial
// stop sequences (e.g. "<EN" of "<END>") across streaming chunk boundaries.
type StopFilter struct {
	stopSeqs []string
	buf      string
	stopped  bool
	hitStop  string
}

// NewStopFilter constructs a StopFilter with the specified stop sequences.
// Empty stop sequences are filtered out.
func NewStopFilter(stopSeqs []string) *StopFilter {
	var clean []string
	for _, s := range stopSeqs {
		if s != "" {
			clean = append(clean, s)
		}
	}
	return &StopFilter{
		stopSeqs: clean,
	}
}

// Feed receives a new text chunk and returns the safe-to-emit text and whether a stop sequence was hit.
func (f *StopFilter) Feed(chunk string) (emit string, hitStop bool) {
	if f.stopped || len(f.stopSeqs) == 0 {
		if f.stopped {
			return "", true
		}
		return chunk, false
	}

	f.buf += chunk

	// 1. Check if any stop sequence is fully present in f.buf
	earliestIdx := -1
	matchedStop := ""
	for _, stop := range f.stopSeqs {
		idx := strings.Index(f.buf, stop)
		if idx != -1 {
			if earliestIdx == -1 || idx < earliestIdx {
				earliestIdx = idx
				matchedStop = stop
			}
		}
	}

	if earliestIdx != -1 {
		// Stop sequence matched! Everything before earliestIdx is safe to emit.
		emit = f.buf[:earliestIdx]
		f.buf = ""
		f.stopped = true
		f.hitStop = matchedStop
		return emit, true
	}

	// 2. Check for partial prefix matches at the suffix of f.buf
	longestPrefixLen := 0
	for _, stop := range f.stopSeqs {
		// Check suffixes of f.buf of lengths from min(len(f.buf), len(stop)-1) down to 1
		maxCheck := len(stop) - 1
		if len(f.buf) < maxCheck {
			maxCheck = len(f.buf)
		}
		for l := maxCheck; l >= 1; l-- {
			suffix := f.buf[len(f.buf)-l:]
			if strings.HasPrefix(stop, suffix) {
				if l > longestPrefixLen {
					longestPrefixLen = l
				}
				break
			}
		}
	}

	// Safe to emit everything up to len(f.buf) - longestPrefixLen
	safeLen := len(f.buf) - longestPrefixLen
	if safeLen > 0 {
		emit = f.buf[:safeLen]
		f.buf = f.buf[safeLen:]
	}

	return emit, false
}

// Flush emits any remaining buffered text when the stream ends without hitting a stop sequence.
func (f *StopFilter) Flush() string {
	if f.stopped || len(f.buf) == 0 {
		return ""
	}
	remaining := f.buf
	f.buf = ""
	return remaining
}

// IsStopped returns true if a stop sequence has been matched.
func (f *StopFilter) IsStopped() bool {
	return f.stopped
}

// HitStop returns the stop sequence that triggered the stop, if any.
func (f *StopFilter) HitStop() string {
	return f.hitStop
}
