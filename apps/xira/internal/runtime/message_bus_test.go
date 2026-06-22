package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// These tests define the Phase 1 MessageBus type contract BEFORE the types
// exist (TDD red). Implementation in message_bus.go must make every test pass.
// See RFC §2.3 (MessageBus interface + sealed Message + Filter).
//
// What is locked in here:
//   - MessagePriority enum (Droppable < Important < Critical).
//   - Every Message type implements the full interface (ID, AgentTurnID,
//     Timestamp, Reliable, Priority, Kind, isMessage).
//   - Reliable()/Priority() values match RFC §2.3 — lifecycle = Reliable &
//     Critical, progress = not Reliable & Droppable. This is the bus's
//     routing table; getting it wrong breaks WAL persistence and eviction.
//   - Filter.Match is the one piece of real logic on the bus types: it
//     decides which events a subscriber receives. Covered exhaustively.

// -----------------------------------------------------------------------------
// MessagePriority enum
// -----------------------------------------------------------------------------

func TestMessagePriorityOrdering(t *testing.T) {
	// RFC §2.3: Critical > Important > Droppable. Numeric ordering must match
	// so the bus can compare with < / >.
	if !(PriorityDroppable < PriorityImportant && PriorityImportant < PriorityCritical) {
		t.Errorf("priority ordering broken: Droppable=%d Important=%d Critical=%d",
			PriorityDroppable, PriorityImportant, PriorityCritical)
	}
}

// -----------------------------------------------------------------------------
// Message sealed interface — every kind declares itself and its routing tags
// -----------------------------------------------------------------------------

func TestMessageKindStrings(t *testing.T) {
	cases := []struct {
		msg  Message
		want string
	}{
		{InboundMessage{}, "inbound"},
		{OutboundMessage{}, "outbound"},
		{AgentTurnStarted{}, "agent_turn.started"},
		{AgentTurnCompleted{}, "agent_turn.completed"},
		{AgentTurnFailed{}, "agent_turn.failed"},
		{AgentTurnCanceled{}, "agent_turn.canceled"},
		{HumanRequested{}, "human.requested"},
		{HumanResponded{}, "human.responded"},
		{AssistantStatus{}, "assistant.status"},
		{ToolCalled{}, "tool.called"},
		{ToolResult{}, "tool.result"},
	}
	for _, c := range cases {
		if got := c.msg.Kind(); got != c.want {
			t.Errorf("%T.Kind() = %q, want %q", c.msg, got, c.want)
		}
	}
}

func TestMessagePriorityRouting(t *testing.T) {
	// RFC §2.3 routing table. This is the single source of truth for which
	// messages get WAL-persisted and which get evicted under load. Asserting
	// it here means a future contributor cannot flip a tag without breaking
	// a test that names the RFC contract.
	critical := []Message{
		AgentTurnStarted{}, AgentTurnCompleted{}, AgentTurnFailed{}, AgentTurnCanceled{},
		HumanRequested{}, HumanResponded{},
	}
	for _, m := range critical {
		if !m.Reliable() {
			t.Errorf("%T.Reliable() = false, want true (lifecycle must persist)", m)
		}
		if m.Priority() != PriorityCritical {
			t.Errorf("%T.Priority() = %d, want Critical", m, m.Priority())
		}
	}
	droppable := []Message{
		AssistantStatus{}, ToolCalled{}, ToolResult{},
	}
	for _, m := range droppable {
		if m.Reliable() {
			t.Errorf("%T.Reliable() = true, want false (progress does not persist)", m)
		}
		if m.Priority() != PriorityDroppable {
			t.Errorf("%T.Priority() = %d, want Droppable", m, m.Priority())
		}
	}
}

func TestMessagePriorityImportantTier(t *testing.T) {
	// Inbound/Outbound are Important: user-facing, must reach IM, but not
	// lifecycle events that drive the turn state machine. They are NOT
	// Reliable (not persisted to WAL) — only lifecycle messages are.
	for _, m := range []Message{InboundMessage{}, OutboundMessage{}} {
		if m.Priority() != PriorityImportant {
			t.Errorf("%T.Priority() = %d, want Important", m, m.Priority())
		}
		if m.Reliable() {
			t.Errorf("%T.Reliable() = true, want false (IM boundary messages are not lifecycle-persisted)", m)
		}
	}
}

// NOTE: the old TestMessageSealedExhaustive iterated a hand-written list and
// therefore missed new types added to the package (PR #31 review W3). It is
// replaced by TestMessageSealIsClosedAgainstSource in
// sealed_exhaustive_test.go, which scans package SOURCE for isMessage()
// receivers and compares against expectedMessageTypes.

// Compile-time: every declared Message type satisfies the interface.
var (
	_ Message = InboundMessage{}
	_ Message = OutboundMessage{}
	_ Message = AgentTurnStarted{}
	_ Message = AgentTurnCompleted{}
	_ Message = AgentTurnFailed{}
	_ Message = AgentTurnCanceled{}
	_ Message = HumanRequested{}
	_ Message = HumanResponded{}
	_ Message = AssistantStatus{}
	_ Message = ToolCalled{}
	_ Message = ToolResult{}
)

// -----------------------------------------------------------------------------
// Per-type field accessors (ID, AgentTurnID, Timestamp)
// -----------------------------------------------------------------------------

func TestAgentTurnStartedAccessors(t *testing.T) {
	now := time.Now()
	m := AgentTurnStarted{
		MessageIDVal:   "m1",
		AgentTurnIDVal: "aturn_1",
		TimestampVal:   now,
	}
	if m.ID() != "m1" {
		t.Errorf("ID() = %q", m.ID())
	}
	if m.AgentTurnID() != "aturn_1" {
		t.Errorf("AgentTurnID() = %q", m.AgentTurnID())
	}
	if !m.Timestamp().Equal(now) {
		t.Errorf("Timestamp() mismatch")
	}
}

func TestAgentTurnCompletedAccessors(t *testing.T) {
	now := time.Now()
	m := AgentTurnCompleted{
		MessageIDVal:         "m2",
		AgentTurnIDVal:       "aturn_1",
		ParentAgentTurnIDVal: "aturn_0",
		TimestampVal:         now,
		Summary:              "ok",
	}
	if m.ParentAgentTurnID() != "aturn_0" {
		t.Errorf("ParentAgentTurnID() = %q", m.ParentAgentTurnID())
	}
	if m.Summary != "ok" {
		t.Errorf("Summary = %q", m.Summary)
	}
}

// TestAllMessageAccessors walks every Message type with a fully-populated
// instance and asserts every accessor returns the populated value. This
// drives ID()/AgentTurnID()/ParentAgentTurnID()/Timestamp() coverage on every
// type (the routing-table tests above only exercise Kind/Reliable/Priority).
func TestAllMessageAccessors(t *testing.T) {
	now := time.Now()
	wantID := "msg_x"
	wantTurn := AgentTurnID("aturn_self")
	wantParent := AgentTurnID("aturn_parent")

	// build one populated instance per Message type. Each uses the shared
	// field names (MessageIDVal / AgentTurnIDVal / ParentIDVal or
	// ParentAgentTurnIDVal / TimestampVal).
	populated := []Message{
		InboundMessage{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentIDVal: wantParent, TimestampVal: now},
		OutboundMessage{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentIDVal: wantParent, TimestampVal: now},
		AgentTurnStarted{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		AgentTurnCompleted{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		AgentTurnFailed{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		AgentTurnCanceled{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		HumanRequested{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		HumanResponded{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		AssistantStatus{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentIDVal: wantParent, TimestampVal: now},
		ToolCalled{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentIDVal: wantParent, TimestampVal: now},
		ToolResult{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentIDVal: wantParent, TimestampVal: now},
	}
	for _, m := range populated {
		t.Run(fmt.Sprintf("%T", m), func(t *testing.T) {
			// Call isMessage() so the cover counter records the (empty)
			// sealed-marker method body. Without this, Go cover reports 0%
			// on zero-statement methods even though they are exercised.
			m.isMessage()
			if m.ID() != wantID {
				t.Errorf("%T.ID() = %q, want %q", m, m.ID(), wantID)
			}
			if m.AgentTurnID() != wantTurn {
				t.Errorf("%T.AgentTurnID() = %q, want %q", m, m.AgentTurnID(), wantTurn)
			}
			if m.ParentAgentTurnID() != wantParent {
				t.Errorf("%T.ParentAgentTurnID() = %q, want %q", m, m.ParentAgentTurnID(), wantParent)
			}
			if !m.Timestamp().Equal(now) {
				t.Errorf("%T.Timestamp() = %v, want %v", m, m.Timestamp(), now)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Lifecycle turn-kind field (PR #31 review W4)
// -----------------------------------------------------------------------------

func TestLifecycleMessagesCarryTurnKind(t *testing.T) {
	// Every turn-lifecycle message carries TurnKind so a subscriber can tell
	// flow-run events from agent-run events without loading the turn.
	cases := []struct {
		name string
		msg  Message
	}{
		{"started", AgentTurnStarted{TurnKind: AgentTurnKindFlow}},
		{"completed", AgentTurnCompleted{TurnKind: AgentTurnKindAgent}},
		{"failed", AgentTurnFailed{TurnKind: AgentTurnKindAgent}},
		{"canceled", AgentTurnCanceled{TurnKind: AgentTurnKindFlow}},
	}
	want := map[string]AgentTurnKind{
		"started":   AgentTurnKindFlow,
		"completed": AgentTurnKindAgent,
		"failed":    AgentTurnKindAgent,
		"canceled":  AgentTurnKindFlow,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AgentTurnKind("")
			switch m := c.msg.(type) {
			case AgentTurnStarted:
				got = m.TurnKind
			case AgentTurnCompleted:
				got = m.TurnKind
			case AgentTurnFailed:
				got = m.TurnKind
			case AgentTurnCanceled:
				got = m.TurnKind
			default:
				t.Fatalf("%s is not a lifecycle message carrying TurnKind: %T", c.name, c.msg)
			}
			if got != want[c.name] {
				t.Errorf("%s.TurnKind = %q, want %q", c.name, got, want[c.name])
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Filter.Match — the one piece of real logic
// -----------------------------------------------------------------------------

func TestFilterMatch_EmptyFilterMatchesAll(t *testing.T) {
	f := Filter{}
	if !f.Match(AgentTurnStarted{AgentTurnIDVal: "a"}) {
		t.Error("empty Filter should match any message")
	}
}

func TestFilterMatch_ByAgentTurnID(t *testing.T) {
	f := Filter{AgentTurnID: strPtr("aturn_1")}
	cases := []struct {
		name  string
		msg   Message
		match bool
	}{
		{"own turn", AgentTurnStarted{AgentTurnIDVal: "aturn_1"}, true},
		{"other turn", AgentTurnStarted{AgentTurnIDVal: "aturn_2"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := f.Match(c.msg); got != c.match {
				t.Errorf("Match = %v, want %v", got, c.match)
			}
		})
	}
}

func TestFilterMatch_IncludeChildren(t *testing.T) {
	// A parent subscribes to its own turn AND its children's events. RFC §2.3
	// subscription scope: parent must observe child lifecycle.
	f := Filter{AgentTurnID: strPtr("aturn_parent"), IncludeChildren: true}
	childCompleted := AgentTurnCompleted{
		AgentTurnIDVal:       "aturn_child",
		ParentAgentTurnIDVal: "aturn_parent",
	}
	if !f.Match(childCompleted) {
		t.Error("parent filter with IncludeChildren must match child's event")
	}

	// Without IncludeChildren, child's event does NOT match a filter scoped
	// to the parent turn.
	fNoChildren := Filter{AgentTurnID: strPtr("aturn_parent"), IncludeChildren: false}
	if fNoChildren.Match(childCompleted) {
		t.Error("parent filter without IncludeChildren must not match child's event")
	}
}

func TestFilterMatch_SiblingChildExcluded(t *testing.T) {
	// RFC §2.3 scope/permission: a child must NOT subscribe to its sibling's
	// events (information leak). Filter scoped to child A must not match an
	// event from child B even though both share the same parent.
	fChildA := Filter{AgentTurnID: strPtr("aturn_childA"), IncludeChildren: true}
	siblingB := AgentTurnCompleted{
		AgentTurnIDVal:       "aturn_childB",
		ParentAgentTurnIDVal: "aturn_parent",
	}
	if fChildA.Match(siblingB) {
		t.Error("child A filter must not match sibling B's event (cross-sibling leak)")
	}
}

func TestFilterMatch_EmptyAgentTurnIDDoesNotLeakRoots(t *testing.T) {
	// PR #31 review W2 regression: a filter whose AgentTurnID is "" (caller
	// bug — turn ids are always non-empty) must NOT match anything, even
	// with IncludeChildren=true. Previously it matched every root event
	// (whose ParentAgentTurnID is also ""), leaking the whole root tier.
	emptyTurn := AgentTurnID("")
	f := Filter{AgentTurnID: &emptyTurn, IncludeChildren: true}
	rootEvt := AgentTurnStarted{AgentTurnIDVal: "aturn_root", ParentAgentTurnIDVal: ""}
	if f.Match(rootEvt) {
		t.Error("filter with empty AgentTurnID must not match root event (W2 leak)")
	}
	// Also must not match a non-root event.
	other := AgentTurnStarted{AgentTurnIDVal: "aturn_x", ParentAgentTurnIDVal: "aturn_p"}
	if f.Match(other) {
		t.Error("filter with empty AgentTurnID must not match any event")
	}
}

func TestFilterMatch_ByKinds(t *testing.T) {
	f := Filter{Kinds: []string{"agent_turn.completed", "agent_turn.failed"}}
	cases := []struct {
		name  string
		msg   Message
		match bool
	}{
		{"completed", AgentTurnCompleted{}, true},
		{"failed", AgentTurnFailed{}, true},
		{"started excluded", AgentTurnStarted{}, false},
		{"status excluded", AssistantStatus{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := f.Match(c.msg); got != c.match {
				t.Errorf("Match = %v, want %v", got, c.match)
			}
		})
	}
}

func TestFilterMatch_KindsAndAgentTurnIDCombined(t *testing.T) {
	// Both predicates must hold (AND semantics).
	f := Filter{
		AgentTurnID: strPtr("aturn_1"),
		Kinds:       []string{"agent_turn.completed"},
	}
	if !f.Match(AgentTurnCompleted{AgentTurnIDVal: "aturn_1"}) {
		t.Error("both predicates match → should Match")
	}
	if f.Match(AgentTurnStarted{AgentTurnIDVal: "aturn_1"}) {
		t.Error("kind mismatch → should not Match even if turn matches")
	}
	if f.Match(AgentTurnCompleted{AgentTurnIDVal: "aturn_2"}) {
		t.Error("turn mismatch → should not Match even if kind matches")
	}
}

func TestFilterMatch_NilAgentTurnIDMsgWithAgentTurnFilter(t *testing.T) {
	// A message whose AgentTurnID accessor returns "" but the filter requires
	// a specific turn must NOT match (defensive — should not happen in
	// practice but the filter must be safe).
	f := Filter{AgentTurnID: strPtr("aturn_1")}
	if f.Match(AgentTurnStarted{AgentTurnIDVal: ""}) {
		t.Error("empty AgentTurnID message must not match turn-scoped filter")
	}
}

// -----------------------------------------------------------------------------
// MessageBus interface (compile-time only — Phase 1 declares the interface,
// Phase 2 implements it. We verify the interface shape via a fake.)
// -----------------------------------------------------------------------------

func TestMessageBusInterfaceShape(t *testing.T) {
	// The interface must accept a minimal fake. If Publish/Subscribe/
	// Unsubscribe signatures drift, this stops compiling.
	var _ MessageBus = fakeBus{}
}

type fakeBus struct{}

func (fakeBus) Publish(_ context.Context, _ Message) error { return nil }
func (fakeBus) Subscribe(_ Filter) (SubID, <-chan Message) {
	ch := make(chan Message)
	close(ch)
	return SubID(""), ch
}
func (fakeBus) Unsubscribe(_ SubID) {}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func strPtr(s string) *AgentTurnID { id := AgentTurnID(s); return &id }
