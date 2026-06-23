package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

// wait_turn_test.go: tests executeWaitTurn — the parent LLM's tool to
// retrieve a spawned child's result (Phase 4, RFC §2.4 D-3). The ADK tool
// wrapper is thin (it calls executeWaitTurn); the real logic is here.

// mockWaiterSink is a test double that implements both SpawnSink and
// SpawnResultWaiter, returning a canned result for a given child ID.
type mockWaiterSink struct {
	result PendingResult
	err    error
	delay  time.Duration // simulate a child that takes time to finish
}

func (m *mockWaiterSink) Deliver(pr PendingResult) {}

func (m *mockWaiterSink) Wait(ctx context.Context, childID string) (PendingResult, error) {
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return PendingResult{}, ctx.Err()
		}
	}
	if m.err != nil {
		return PendingResult{}, m.err
	}
	return m.result, nil
}

// mockSinkNoWait implements SpawnSink but NOT SpawnResultWaiter — simulates a
// sink that can't support blocking wait (e.g. a fire-and-forget test double).
type mockSinkNoWait struct{}

func (mockSinkNoWait) Deliver(PendingResult) {}

func TestExecuteWaitTurnSuccess(t *testing.T) {
	sink := &mockWaiterSink{result: PendingResult{
		TurnID: "spawn:abc",
		Result: DelegateAgentResult{AgentID: "code", RunID: "r1", Status: "completed", Summary: "the child did the thing"},
	}}
	ctx := WithSpawnSink(context.Background(), sink)

	out, err := executeWaitTurn(ctx, "spawn:abc")
	if err != nil {
		t.Fatalf("executeWaitTurn returned error: %v", err)
	}
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

func TestExecuteWaitTurnChildFailed(t *testing.T) {
	sink := &mockWaiterSink{result: PendingResult{
		TurnID: "spawn:fail",
		Result: DelegateAgentResult{Status: "failed"},
		Err:    "child agent error",
	}}
	ctx := WithSpawnSink(context.Background(), sink)

	out, _ := executeWaitTurn(ctx, "spawn:fail")
	if out["status"] != "failed" {
		t.Errorf("status = %v, want 'failed'", out["status"])
	}
	if out["error"] != "child agent error" {
		t.Errorf("error = %v, want 'child agent error'", out["error"])
	}
}

func TestExecuteWaitTurnNoSink(t *testing.T) {
	// No SpawnSink in context — wait_turn must report unavailable, not panic.
	out, _ := executeWaitTurn(context.Background(), "spawn:abc")
	if out["status"] != "unavailable" {
		t.Errorf("status = %v, want 'unavailable'", out["status"])
	}
}

func TestExecuteWaitTurnSinkNotWaiter(t *testing.T) {
	// Sink present but doesn't implement SpawnResultWaiter.
	ctx := WithSpawnSink(context.Background(), mockSinkNoWait{})
	out, _ := executeWaitTurn(ctx, "spawn:abc")
	if out["status"] != "unavailable" {
		t.Errorf("status = %v, want 'unavailable'", out["status"])
	}
}

func TestExecuteWaitTurnTimeout(t *testing.T) {
	// Child never completes within the (shortened) ctx. wait_turn must
	// return timeout, not hang.
	sink := &mockWaiterSink{delay: 10 * time.Second} // longer than ctx
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ctx = WithSpawnSink(ctx, sink)

	out, _ := executeWaitTurn(ctx, "spawn:slow")
	if out["status"] != "timeout" {
		t.Errorf("status = %v, want 'timeout'", out["status"])
	}
}

func TestSanitizeWaitTurnInput(t *testing.T) {
	spec, clean, unsupported := sanitizeWaitTurnInput(map[string]any{
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
	_, _, unsupported = sanitizeWaitTurnInput(map[string]any{
		"child_turn_id": "spawn:xyz",
		"timeout_ms":    5000,
	})
	if len(unsupported) != 1 || unsupported[0] != "timeout_ms" {
		t.Errorf("unsupported = %v, want [timeout_ms]", unsupported)
	}
}

func TestWaitTurnInputValidate(t *testing.T) {
	if err := (waitTurnInput{ChildTurnID: "spawn:1"}).Validate(); err != nil {
		t.Errorf("valid input errored: %v", err)
	}
	if err := (waitTurnInput{}).Validate(); err == nil || !strings.Contains(err.Error(), "child_turn_id") {
		t.Errorf("empty input error = %v, want child_turn_id required", err)
	}
}

// Compile-time: mocks satisfy the interfaces.
var _ SpawnSink = (*mockWaiterSink)(nil)
var _ SpawnResultWaiter = (*mockWaiterSink)(nil)
var _ SpawnSink = mockSinkNoWait{}
