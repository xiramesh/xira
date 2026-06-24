package runtime

import (
	"context"
	"strings"
	"testing"
)

// poll_turn_test.go: tests executePollTurn — the parent LLM's NON-BLOCKING
// tool to check a spawned child's result (Phase 4, RFC §2.4 D-3). The ADK tool
// wrapper is thin (it calls executePollTurn); the real logic is here.
//
// Key invariant under test: executePollTurn NEVER blocks. A child that hasn't
// finished returns "pending" immediately. This is what keeps the steering
// checkpoint alive (PR #53 review CRITICAL).

// mockPeeperSink implements both SpawnSink and SpawnSinkPeeper, returning a
// canned result for a given child ID (or nothing, to simulate pending).
type mockPeeperSink struct {
	results map[string]PendingResult
}

func (m *mockPeeperSink) Deliver(pr PendingResult) {
	if m.results == nil {
		m.results = map[string]PendingResult{}
	}
	m.results[pr.TurnID] = pr
}

func (m *mockPeeperSink) TryResult(childID string) (PendingResult, bool) {
	pr, ok := m.results[childID]
	return pr, ok
}

func (m *mockPeeperSink) HasResult() bool {
	return len(m.results) > 0
}

// mockSinkNoPeek implements SpawnSink but NOT SpawnSinkPeeper.
type mockSinkNoPeek struct{}

func (mockSinkNoPeek) Deliver(PendingResult) {}

func TestExecutePollTurnSuccess(t *testing.T) {
	sink := &mockPeeperSink{results: map[string]PendingResult{
		"spawn:abc": {
			TurnID: "spawn:abc",
			Result: DelegateAgentResult{AgentID: "code", Status: "completed", Summary: "the child did the thing"},
		},
	}}
	ctx := WithSpawnSink(context.Background(), sink)

	out := executePollTurn(ctx, "spawn:abc")
	if out["status"] != "completed" {
		t.Errorf("status = %v, want 'completed'", out["status"])
	}
	if out["child_turn_id"] != "spawn:abc" {
		t.Errorf("child_turn_id = %v", out["child_turn_id"])
	}
	if out["result_summary"] != "the child did the thing" {
		t.Errorf("result_summary = %v", out["result_summary"])
	}
}

func TestExecutePollTurnChildFailed(t *testing.T) {
	sink := &mockPeeperSink{results: map[string]PendingResult{
		"spawn:fail": {
			TurnID: "spawn:fail",
			Result: DelegateAgentResult{Status: "failed"},
			Err:    "child agent error",
		},
	}}
	ctx := WithSpawnSink(context.Background(), sink)

	out := executePollTurn(ctx, "spawn:fail")
	if out["status"] != "failed" {
		t.Errorf("status = %v, want 'failed'", out["status"])
	}
	if out["error"] != "child agent error" {
		t.Errorf("error = %v", out["error"])
	}
}

func TestExecutePollTurnPending(t *testing.T) {
	// The CRITICAL case: child hasn't finished. poll_turn MUST return pending
	// immediately, NOT block. (The old wait_turn would block here for 5 min.)
	sink := &mockPeeperSink{results: map[string]PendingResult{}}
	ctx := WithSpawnSink(context.Background(), sink)

	out := executePollTurn(ctx, "spawn:running")
	if out["status"] != "pending" {
		t.Errorf("status = %v, want 'pending'", out["status"])
	}
	if out["child_turn_id"] != "spawn:running" {
		t.Errorf("child_turn_id = %v", out["child_turn_id"])
	}
}

func TestExecutePollTurnNoSink(t *testing.T) {
	// No SpawnSink in context — poll_turn reports unavailable, not panic.
	out := executePollTurn(context.Background(), "spawn:abc")
	if out["status"] != "unavailable" {
		t.Errorf("status = %v, want 'unavailable'", out["status"])
	}
}

func TestExecutePollTurnSinkNotPeeper(t *testing.T) {
	// Sink present but doesn't implement SpawnSinkPeeper.
	ctx := WithSpawnSink(context.Background(), mockSinkNoPeek{})
	out := executePollTurn(ctx, "spawn:abc")
	if out["status"] != "unavailable" {
		t.Errorf("status = %v, want 'unavailable'", out["status"])
	}
}

func TestSanitizePollTurnInput(t *testing.T) {
	spec, clean, unsupported := sanitizePollTurnInput(map[string]any{
		"child_turn_id": "  spawn:xyz  ",
	})
	if spec.ChildTurnID != "spawn:xyz" {
		t.Errorf("ChildTurnID = %q, want 'spawn:xyz' (trimmed)", spec.ChildTurnID)
	}
	if len(unsupported) != 0 {
		t.Errorf("unsupported = %v, want empty", unsupported)
	}
	if clean["child_turn_id"] != "spawn:xyz" {
		t.Errorf("clean child_turn_id = %v", clean["child_turn_id"])
	}

	// Unsupported fields reported, spec still extracted.
	_, _, unsupported = sanitizePollTurnInput(map[string]any{
		"child_turn_id": "spawn:xyz",
		"timeout_ms":    5000,
	})
	if len(unsupported) != 1 || unsupported[0] != "timeout_ms" {
		t.Errorf("unsupported = %v, want [timeout_ms]", unsupported)
	}
}

func TestPollTurnInputValidate(t *testing.T) {
	if err := (pollTurnInput{ChildTurnID: "spawn:1"}).Validate(); err != nil {
		t.Errorf("valid input errored: %v", err)
	}
	if err := (pollTurnInput{}).Validate(); err == nil || !strings.Contains(err.Error(), "child_turn_id") {
		t.Errorf("empty input error = %v, want child_turn_id required", err)
	}
}

// Compile-time: mocks satisfy the interfaces.
var _ SpawnSink = (*mockPeeperSink)(nil)
var _ SpawnSinkPeeper = (*mockPeeperSink)(nil)
var _ SpawnSink = mockSinkNoPeek{}
