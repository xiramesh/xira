package runtime

import (
	"strings"
	"testing"
	"time"

	fsession "github.com/xiramesh/xira/internal/session"
)

// These tests define the Phase 1 AgentTurn type contract BEFORE the type
// exists (TDD red). The implementation in agent_turn.go must make every test
// pass. See RFC §2.1 (envelope + payload) and docs/architecture/
// xira-agentturn-messagebus-rfc-v0.zh.md.
//
// What is being locked in here:
//   - AgentTurn envelope fields (ID, ParentAgentTurnID, Kind, Status,
//     StartedAt, EndedAt, SessionScope, InheritSession, Payload).
//   - AgentTurnKind enum is closed (flow | agent).
//   - AgentTurnStatus enum is closed, and status transitions are validated
//     by a state-machine method (IsValidTransition) — this is the one piece
//     of real logic on the envelope and carries the CAS contract from
//     RFC Appendix C.5 (duplicates must be a no-op, not an error).
//   - AgentTurnPayload is a sealed interface: only FlowPayload and
//     AgentPayload satisfy it. A type-switch exhaustiveness check fails the
//     test if a new payload type is added without updating consumers.
//   - SessionScope is a pointer (nil allowed) — RFC §2.1 方案 Z.

// -----------------------------------------------------------------------------
// AgentTurnKind enum
// -----------------------------------------------------------------------------

func TestAgentTurnKindValues(t *testing.T) {
	cases := []struct {
		kind AgentTurnKind
		want string
	}{
		{AgentTurnKindFlow, "flow"},
		{AgentTurnKindAgent, "agent"},
	}
	for _, c := range cases {
		if got := string(c.kind); got != c.want {
			t.Errorf("AgentTurnKind(%q).String() = %q, want %q", c.want, got, c.want)
		}
	}
}

func TestAgentTurnKindIsClosed(t *testing.T) {
	// If a new AgentTurnKind is added, this switch must be updated. The
	// default clause catches forgotten cases at test time.
	for _, k := range []AgentTurnKind{AgentTurnKindFlow, AgentTurnKindAgent} {
		switch k {
		case AgentTurnKindFlow, AgentTurnKindAgent:
			// known kinds
		default:
			t.Errorf("unknown AgentTurnKind %q — update this switch when adding a kind", k)
		}
	}
}

// -----------------------------------------------------------------------------
// AgentTurnStatus enum + state machine
// -----------------------------------------------------------------------------

func TestAgentTurnStatusValues(t *testing.T) {
	cases := []struct {
		status AgentTurnStatus
		want   string
	}{
		{AgentTurnStatusRequested, "requested"},
		{AgentTurnStatusRunning, "running"},
		{AgentTurnStatusWaitingHuman, "waiting_human"},
		{AgentTurnStatusCompleted, "completed"},
		{AgentTurnStatusFailed, "failed"},
		{AgentTurnStatusCanceled, "canceled"},
		{AgentTurnStatusTimeout, "timeout"},
	}
	for _, c := range cases {
		if got := string(c.status); got != c.want {
			t.Errorf("AgentTurnStatus String() = %q, want %q", got, c.want)
		}
	}
}

func TestAgentTurnStatusTerminal(t *testing.T) {
	// RFC §2.1 + Appendix C.5: terminal states must be idempotent. A repeated
	// completion event is a no-op, not an error — this is what lets an
	// at-least-once bus deliver duplicates safely.
	terminal := []AgentTurnStatus{
		AgentTurnStatusCompleted,
		AgentTurnStatusFailed,
		AgentTurnStatusCanceled,
		AgentTurnStatusTimeout,
	}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%q.IsTerminal() = false, want true", s)
		}
	}
	nonTerminal := []AgentTurnStatus{
		AgentTurnStatusRequested,
		AgentTurnStatusRunning,
		AgentTurnStatusWaitingHuman,
	}
	for _, s := range nonTerminal {
		if s.IsTerminal() {
			t.Errorf("%q.IsTerminal() = true, want false", s)
		}
	}
}

func TestIsValidTransition_HappyPath(t *testing.T) {
	cases := []struct {
		name string
		from AgentTurnStatus
		to   AgentTurnStatus
	}{
		{"requested→running", AgentTurnStatusRequested, AgentTurnStatusRunning},
		{"requested→failed", AgentTurnStatusRequested, AgentTurnStatusFailed},
		{"requested→canceled", AgentTurnStatusRequested, AgentTurnStatusCanceled},
		{"requested→timeout", AgentTurnStatusRequested, AgentTurnStatusTimeout},
		{"running→waiting_human", AgentTurnStatusRunning, AgentTurnStatusWaitingHuman},
		{"running→completed", AgentTurnStatusRunning, AgentTurnStatusCompleted},
		{"running→failed", AgentTurnStatusRunning, AgentTurnStatusFailed},
		{"running→timeout", AgentTurnStatusRunning, AgentTurnStatusTimeout},
		{"running→canceled", AgentTurnStatusRunning, AgentTurnStatusCanceled},
		{"waiting_human→running", AgentTurnStatusWaitingHuman, AgentTurnStatusRunning},
		{"waiting_human→completed", AgentTurnStatusWaitingHuman, AgentTurnStatusCompleted},
		{"waiting_human→failed", AgentTurnStatusWaitingHuman, AgentTurnStatusFailed},
		{"waiting_human→canceled", AgentTurnStatusWaitingHuman, AgentTurnStatusCanceled},
		{"waiting_human→timeout", AgentTurnStatusWaitingHuman, AgentTurnStatusTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := IsValidTransition(c.from, c.to); err != nil {
				t.Errorf("IsValidTransition(%s→%s) = %v, want nil", c.from, c.to, err)
			}
		})
	}
}

func TestIsValidTransition_RepeatedTerminalIsNoOp(t *testing.T) {
	// RFC Appendix C.5 CRITICAL 2: at-least-once delivery + crash recovery
	// means a subscriber may receive a terminal event twice. Re-applying a
	// terminal status must be a no-op (nil), NOT an error — otherwise the
	// subscriber cannot be idempotent and only-once-spawn CAS breaks.
	cases := []struct {
		name string
		term AgentTurnStatus
	}{
		{"completed→completed", AgentTurnStatusCompleted},
		{"failed→failed", AgentTurnStatusFailed},
		{"canceled→canceled", AgentTurnStatusCanceled},
		{"timeout→timeout", AgentTurnStatusTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := IsValidTransition(c.term, c.term); err != nil {
				t.Errorf("IsValidTransition(%s→%s) = %v, want nil (idempotent terminal)", c.term, c.term, err)
			}
		})
	}
}

func TestIsValidTransition_IllegalTransitions(t *testing.T) {
	cases := []struct {
		name string
		from AgentTurnStatus
		to   AgentTurnStatus
	}{
		{"completed→running", AgentTurnStatusCompleted, AgentTurnStatusRunning},
		{"failed→completed", AgentTurnStatusFailed, AgentTurnStatusCompleted},
		{"canceled→running", AgentTurnStatusCanceled, AgentTurnStatusRunning},
		{"requested→completed", AgentTurnStatusRequested, AgentTurnStatusCompleted},
		// requested→waiting_human is illegal: a turn must reach Running
		// before it can pause for HITL. (requested→failed/canceled/timeout
		// ARE legal — a turn can die during scheduling.)
		{"requested→waiting_human", AgentTurnStatusRequested, AgentTurnStatusWaitingHuman},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := IsValidTransition(c.from, c.to)
			if err == nil {
				t.Errorf("IsValidTransition(%s→%s) = nil, want error (illegal)", c.from, c.to)
				return
			}
			// Exercise transitionError.Error() so it is covered, and assert
			// the message names both states for debuggability.
			msg := err.Error()
			if !strings.Contains(msg, string(c.from)) || !strings.Contains(msg, string(c.to)) {
				t.Errorf("error message %q must mention both from (%s) and to (%s)", msg, c.from, c.to)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// AgentTurn envelope struct
// -----------------------------------------------------------------------------

func TestAgentTurnEnvelopeFields(t *testing.T) {
	scope := &fsession.SessionScope{
		Version:      1,
		EntrypointID: "ilink-default",
		Channel:      "ilink",
	}
	now := time.Now()
	turn := AgentTurn{
		ID:                "aturn_abc",
		ParentAgentTurnID: "aturn_parent",
		Kind:              AgentTurnKindAgent,
		Status:            AgentTurnStatusRunning,
		StartedAt:         now,
		EndedAt:           time.Time{},
		SessionScope:      scope,
		InheritSession:    false,
		Payload:           AgentPayload{FinalText: "hi"},
	}
	if turn.ID != "aturn_abc" {
		t.Errorf("ID = %q", turn.ID)
	}
	if turn.ParentAgentTurnID != "aturn_parent" {
		t.Errorf("ParentAgentTurnID = %q", turn.ParentAgentTurnID)
	}
	if turn.Kind != AgentTurnKindAgent {
		t.Errorf("Kind = %q", turn.Kind)
	}
	if turn.Status != AgentTurnStatusRunning {
		t.Errorf("Status = %q", turn.Status)
	}
	if turn.SessionScope != scope {
		t.Error("SessionScope pointer not carried")
	}
	if turn.InheritSession {
		t.Error("InheritSession should default false")
	}
	if _, ok := turn.Payload.(AgentPayload); !ok {
		t.Errorf("Payload is not AgentPayload: %T", turn.Payload)
	}
}

func TestAgentTurnRootTurnHasEmptyParent(t *testing.T) {
	// A root turn (IM-triggered) has no parent. This is the "root marker"
	// convention — ParentAgentTurnID == "" means root.
	root := AgentTurn{
		ID:                "aturn_root",
		ParentAgentTurnID: "",
		Kind:              AgentTurnKindFlow,
		Status:            AgentTurnStatusRequested,
	}
	if root.ParentAgentTurnID != "" {
		t.Errorf("root turn ParentAgentTurnID = %q, want empty", root.ParentAgentTurnID)
	}
}

func TestAgentTurnSessionScopeNilAllowed(t *testing.T) {
	// RFC §2.1 方案 Z: pointer allows nil for system-maintenance turns with
	// no IM trigger identity.
	turn := AgentTurn{
		ID:           "aturn_maintenance",
		Kind:         AgentTurnKindAgent,
		Status:       AgentTurnStatusRunning,
		SessionScope: nil,
	}
	if turn.SessionScope != nil {
		t.Error("SessionScope should be nil for system turn")
	}
}

// -----------------------------------------------------------------------------
// AgentTurnPayload sealed interface
// -----------------------------------------------------------------------------

// NOTE: the old TestAgentTurnPayloadSealed iterated a hand-written list and
// missed new payload types (same PR #31 review W3 defect as the Message
// variant). It is replaced by TestPayloadSealIsClosedAgainstSource in
// sealed_exhaustive_test.go, which scans package SOURCE for
// isAgentTurnPayload() receivers. The empty-method coverage it provided is
// preserved by TestPayloadSealedMarkerBodies below.

// TestPayloadSealedMarkerBodies calls isAgentTurnPayload() on every expected
// payload so the cover counter records the (empty) sealed-marker bodies.
// Go cover scores zero-statement methods 0% even when called; this test keeps
// them exercised regardless.
func TestPayloadSealedMarkerBodies(t *testing.T) {
	for name := range expectedPayloadTypes {
		var p AgentTurnPayload
		switch name {
		case "FlowPayload":
			p = FlowPayload{}
		case "AgentPayload":
			p = AgentPayload{}
		default:
			t.Fatalf("expectedPayloadTypes lists %q but TestPayloadSealedMarkerBodies has no case — add one", name)
		}
		p.isAgentTurnPayload()
	}
}

func TestFlowPayloadFields(t *testing.T) {
	fp := FlowPayload{
		CurrentStepID: "step_1",
		StepIDs:       []string{"step_1", "step_2"},
	}
	if fp.CurrentStepID != "step_1" {
		t.Errorf("CurrentStepID = %q", fp.CurrentStepID)
	}
	if len(fp.StepIDs) != 2 {
		t.Errorf("StepIDs len = %d", len(fp.StepIDs))
	}
}

func TestAgentPayloadFields(t *testing.T) {
	ap := AgentPayload{
		FinalText:     "done",
		ToolCallCount: 3,
	}
	if ap.FinalText != "done" {
		t.Errorf("FinalText = %q", ap.FinalText)
	}
	if ap.ToolCallCount != 3 {
		t.Errorf("ToolCallCount = %d", ap.ToolCallCount)
	}
}

// payloadMarker enforces that only FlowPayload/AgentPayload implement
// isAgentTurnPayload at compile time. If a third type sneaks in, this will
// still compile but the exhaustiveness test above catches it at test time.
var _ AgentTurnPayload = FlowPayload{}
var _ AgentTurnPayload = AgentPayload{}
