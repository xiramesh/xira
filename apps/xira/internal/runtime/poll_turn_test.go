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

// mockPeeperBus implements both SpawnBus and SpawnBusPeeper, returning a
// canned result for a given child ID (or nothing, to simulate pending).
type mockPeeperBus struct {
	results map[string]PendingResult
}

func (m *mockPeeperBus) Deliver(pr PendingResult) {
	if m.results == nil {
		m.results = map[string]PendingResult{}
	}
	m.results[pr.TurnID] = pr
}

func (m *mockPeeperBus) TryResult(childID string) (PendingResult, bool) {
	pr, ok := m.results[childID]
	return pr, ok
}

func (m *mockPeeperBus) HasResult() bool {
	return len(m.results) > 0
}

// mockBusNoPeek implements SpawnBus but NOT SpawnBusPeeper.
type mockBusNoPeek struct{}

func (mockBusNoPeek) Deliver(PendingResult) {}

func TestExecutePollTurnSuccess(t *testing.T) {
	sink := &mockPeeperBus{results: map[string]PendingResult{
		"spawn:abc": {
			TurnID: "spawn:abc",
			Result: DelegateAgentResult{AgentID: "code", Status: "completed", Summary: "the child did the thing"},
		},
	}}
	ctx := WithSpawnBus(context.Background(), sink)

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
	sink := &mockPeeperBus{results: map[string]PendingResult{
		"spawn:fail": {
			TurnID: "spawn:fail",
			Result: DelegateAgentResult{Status: "failed"},
			Err:    "child agent error",
		},
	}}
	ctx := WithSpawnBus(context.Background(), sink)

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
	sink := &mockPeeperBus{results: map[string]PendingResult{}}
	ctx := WithSpawnBus(context.Background(), sink)

	out := executePollTurn(ctx, "spawn:running")
	if out["status"] != "pending" {
		t.Errorf("status = %v, want 'pending'", out["status"])
	}
	if out["child_turn_id"] != "spawn:running" {
		t.Errorf("child_turn_id = %v", out["child_turn_id"])
	}
}

func TestExecutePollTurnNoSink(t *testing.T) {
	// No SpawnBus in context — poll_turn reports unavailable, not panic.
	out := executePollTurn(context.Background(), "spawn:abc")
	if out["status"] != "unavailable" {
		t.Errorf("status = %v, want 'unavailable'", out["status"])
	}
}

func TestExecutePollTurnSinkNotPeeper(t *testing.T) {
	// Sink present but doesn't implement SpawnBusPeeper.
	ctx := WithSpawnBus(context.Background(), mockBusNoPeek{})
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

// TestExecutePollTurnChildWaitingHumanSurfacesQuestion is the #68 core test:
// when a spawned child enters HITL (human.request), poll_turn MUST surface the
// child's question + the HumanRequestID to the parent LLM. The parent LLM then
// decides: answer itself (answer_child tool, #68-2.3) or stay silent (the
// question escalates to the user in IM via the parent's chat key).
//
// Without surfacing the question, the parent LLM only sees status=waiting_human
// with no context — it cannot decide whether to answer or escalate.
func TestExecutePollTurnChildWaitingHumanSurfacesQuestion(t *testing.T) {
	sink := &mockPeeperBus{results: map[string]PendingResult{
		"spawn:asking": {
			TurnID: "spawn:asking",
			Result: DelegateAgentResult{
				AgentID:  "research",
				Status:   StatusWaitingHuman,
				Summary:  "child needs input",
				PendingQuestions: []PendingQuestion{
					{Question: "Which deployment window should I target?", HumanRequestID: "hr-ask-001"},
				},
			},
		},
	}}
	ctx := WithSpawnBus(context.Background(), sink)

	out := executePollTurn(ctx, "spawn:asking")
	if out["status"] != StatusWaitingHuman {
		t.Errorf("status = %v, want %q", out["status"], StatusWaitingHuman)
	}
	if out["child_turn_id"] != "spawn:asking" {
		t.Errorf("child_turn_id = %v", out["child_turn_id"])
	}
	if out["question"] != "Which deployment window should I target?" {
		t.Errorf("question = %v, want the child's HITL question surfaced to the parent", out["question"])
	}
	if out["human_request_id"] != "hr-ask-001" {
		t.Errorf("human_request_id = %v, want the child's HumanRequestID (parent needs it to answer via answer_child)", out["human_request_id"])
	}
}

// TestExecutePollTurnChildWaitingHumanSurfacesAllPendingQuestions (PR #77
// follow-up): when a spawned child has MULTIPLE pending HumanRequests, poll_turn
// must surface ALL of them — not just [0]. Reviewer flagged "多 HR 丢字段（只取
// [0]）": a turn can produce >1 HR (multiple human.request calls), and silently
// dropping all but the first leaves the parent LLM unable to answer the rest.
func TestExecutePollTurnChildWaitingHumanSurfacesAllPendingQuestions(t *testing.T) {
	sink := &mockPeeperBus{results: map[string]PendingResult{
		"spawn:multi": {
			TurnID: "spawn:multi",
			Result: DelegateAgentResult{
				AgentID: "research",
				Status:  StatusWaitingHuman,
				Summary: "child needs input on two things",
				PendingQuestions: []PendingQuestion{
					{Question: "Which deployment window?", HumanRequestID: "hr-a"},
					{Question: "Roll back or patch forward?", HumanRequestID: "hr-b"},
				},
			},
		},
	}}
	ctx := WithSpawnBus(context.Background(), sink)

	out := executePollTurn(ctx, "spawn:multi")
	if out["status"] != StatusWaitingHuman {
		t.Errorf("status = %v, want %q", out["status"], StatusWaitingHuman)
	}
	// ALL pending questions must surface — not just the first.
	questions, ok := out["pending_questions"].([]map[string]any)
	if !ok {
		t.Fatalf("pending_questions = %#v, want a slice with 2 entries", out["pending_questions"])
	}
	if len(questions) != 2 {
		t.Fatalf("got %d pending questions, want 2 (the second must NOT be dropped)", len(questions))
	}
	if questions[0]["human_request_id"] != "hr-a" || questions[1]["human_request_id"] != "hr-b" {
		t.Errorf("pending_questions ids = %+v, want [hr-a, hr-b]", questions)
	}
}

// Compile-time: mocks satisfy the interfaces.
var _ SpawnBus = (*mockPeeperBus)(nil)
var _ SpawnBusPeeper = (*mockPeeperBus)(nil)
var _ SpawnBus = mockBusNoPeek{}
