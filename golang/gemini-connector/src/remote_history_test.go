package main

import (
	"strings"
	"testing"
	"time"
)

func TestRemoteReplayLedger_ReplaysCompletedSequence(t *testing.T) {
	ledger := NewRemoteReplayLedger()
	request := RemoteHistoryRequest{Enabled: true, TurnID: "turn-1", Sequence: 1}

	_, owner, err := ledger.Acquire("conversation-1", request, "prompt-a")
	if err != nil || !owner {
		t.Fatalf("first request must acquire ownership: owner=%v err=%v", owner, err)
	}
	ledger.Complete("conversation-1", request, "prompt-a", "agy response", &AgyUsage{InputTokens: 12, OutputTokens: 3, TotalTokens: 15})

	result, owner, err := ledger.Acquire("conversation-1", request, "prompt-a")
	if err != nil || owner || result == nil {
		t.Fatalf("same request must replay: owner=%v result=%+v err=%v", owner, result, err)
	}
	if result.Response != "agy response" || result.Usage == nil || result.Usage.InputTokens != 12 {
		t.Fatalf("unexpected replay result: %+v", result)
	}
}

func TestRemoteReplayLedger_DifferentPromptSameSequenceIsNewAGYRequest(t *testing.T) {
	ledger := NewRemoteReplayLedger()
	request := RemoteHistoryRequest{Enabled: true, TurnID: "turn-1", Sequence: 2}
	if _, owner, err := ledger.Acquire("conversation-1", request, "prompt-a"); err != nil || !owner {
		t.Fatalf("first request must acquire ownership: owner=%v err=%v", owner, err)
	}
	ledger.Complete("conversation-1", request, "prompt-a", "response-a", nil)
	if _, owner, err := ledger.Acquire("conversation-1", request, "prompt-b"); err != nil || !owner {
		t.Fatalf("different prompt at same sequence must be sent to AGY: owner=%v err=%v", owner, err)
	}
}

func TestRemoteReplayLedger_ReleasesOversizedReservation(t *testing.T) {
	ledger := NewRemoteReplayLedger()
	request := RemoteHistoryRequest{Enabled: true, TurnID: "turn-oversize", Sequence: 1}
	_, owner, err := ledger.Acquire("conversation-1", request, "prompt")
	if err != nil || !owner {
		t.Fatalf("initial request was not acquired: owner=%v err=%v", owner, err)
	}
	if ledger.Complete("conversation-1", request, "prompt", strings.Repeat("x", maxRemoteReplayResponseSize+1), nil) {
		t.Fatal("oversized result must not be cached")
	}
	_, owner, err = ledger.Acquire("conversation-1", request, "prompt")
	if err != nil || !owner {
		t.Fatalf("oversized result left a reservation behind: owner=%v err=%v", owner, err)
	}
}

func TestRemoteReplayLedger_DoesNotAllowConcurrentExecution(t *testing.T) {
	ledger := NewRemoteReplayLedger()
	request := RemoteHistoryRequest{Enabled: true, TurnID: "turn-1", Sequence: 1}
	if _, owner, err := ledger.Acquire("conversation-1", request, "prompt-a"); err != nil || !owner {
		t.Fatalf("first request must acquire ownership: owner=%v err=%v", owner, err)
	}
	if _, _, err := ledger.Acquire("conversation-1", request, "prompt-a"); err != ErrRemoteReplayInFlight {
		t.Fatalf("expected in-flight error, got %v", err)
	}
}

func TestRemoteReplayLedger_ExpiresStaleReservation(t *testing.T) {
	ledger := NewRemoteReplayLedger()
	request := RemoteHistoryRequest{Enabled: true, TurnID: "turn-expired", Sequence: 1}
	_, owner, err := ledger.Acquire("conversation-1", request, "prompt")
	if err != nil || !owner {
		t.Fatalf("initial request was not acquired: owner=%v err=%v", owner, err)
	}
	key := remoteReplayKey("conversation-1", request, remoteReplayPromptHash("prompt"))
	ledger.mu.Lock()
	entry := ledger.entries[key]
	entry.createdAt = time.Now().Add(-remoteReplayTTL - time.Second)
	ledger.entries[key] = entry
	ledger.mu.Unlock()
	_, owner, err = ledger.Acquire("conversation-1", request, "prompt")
	if err != nil || !owner {
		t.Fatalf("stale reservation was not expired: owner=%v err=%v", owner, err)
	}
}
