package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// These tests define the Phase 1 dual-bus type contract (RFC §2.3, revised
// 2026-06-22 PR #41). The contract splits the old single Message into:
//   - Content: InboundMessage / OutboundMessage as PLAIN structs (no interface
//     methods) — carried by the typed MessageBus API.
//   - Event: a sealed interface satisfied by 9 types — carried by EventBus.
//
// What this file locks in:
//   - InboundMessage / OutboundMessage are plain structs (no isMessage, no
//     Kind/ID/AgentTurnID methods).
//   - Event is a sealed interface; every kind declares isEvent() and its own
//     Reliable()/Priority() routing tags. MessagePriority was renamed to
//     EventPriority (only Event uses it).
//   - Filter.Match takes an Event (not a Message). The W2 empty-turn leak
//     guard and IncludeChildren semantics are preserved.
//   - MessageBus is a typed API (PublishInbound / PublishOutbound +
//     InboundChan / OutboundChan) — no Publish(Message), no Filter.
//   - No EventBus interface here: Phase 2 evolves the existing event_bus.go
//     EventBus struct to satisfy the future EventBus interface. Phase 1 only
//     defines Event + Filter so the struct can be migrated later.

// -----------------------------------------------------------------------------
// EventPriority enum
// -----------------------------------------------------------------------------

func TestEventPriorityOrdering(t *testing.T) {
	// Renamed from MessagePriority (only Event uses it now). Ordering must hold
	// so the bus can compare with < / >. PriorityImportant was removed (PR #42
	// review WARNING-2: no Event returned it post dual-bus split); only
	// Droppable < Critical remains.
	if !(PriorityDroppable < PriorityCritical) {
		t.Errorf("priority ordering broken: Droppable=%d Critical=%d",
			PriorityDroppable, PriorityCritical)
	}
}

// Compile-time: EventPriority is a distinct type (not a raw int).
var _ EventPriority = PriorityCritical

// -----------------------------------------------------------------------------
// Event sealed interface — every kind declares itself and its routing tags
// -----------------------------------------------------------------------------

func TestEventKindStrings(t *testing.T) {
	cases := []struct {
		evt  Event
		want string
	}{
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
		if got := c.evt.Kind(); got != c.want {
			t.Errorf("%T.Kind() = %q, want %q", c.evt, got, c.want)
		}
	}
}

func TestEventPriorityRouting(t *testing.T) {
	// RFC §2.3 routing table. This is the single source of truth for which
	// events get WAL-persisted and which get evicted under load.
	critical := []Event{
		AgentTurnStarted{}, AgentTurnCompleted{}, AgentTurnFailed{}, AgentTurnCanceled{},
		HumanRequested{}, HumanResponded{},
	}
	for _, e := range critical {
		if !e.Reliable() {
			t.Errorf("%T.Reliable() = false, want true (lifecycle must persist)", e)
		}
		if e.Priority() != PriorityCritical {
			t.Errorf("%T.Priority() = %d, want Critical", e, e.Priority())
		}
	}
	droppable := []Event{
		AssistantStatus{}, ToolCalled{}, ToolResult{},
	}
	for _, e := range droppable {
		if e.Reliable() {
			t.Errorf("%T.Reliable() = true, want false (progress does not persist)", e)
		}
		if e.Priority() != PriorityDroppable {
			t.Errorf("%T.Priority() = %d, want Droppable", e, e.Priority())
		}
	}
}

// TestAllEventAccessors walks every Event type with a fully-populated instance
// and asserts every accessor returns the populated value. Also calls isEvent()
// so the cover counter records the empty sealed-marker body.
func TestAllEventAccessors(t *testing.T) {
	now := time.Now()
	wantID := "msg_x"
	wantTurn := AgentTurnID("aturn_self")
	wantParent := AgentTurnID("aturn_parent")

	populated := []Event{
		AgentTurnStarted{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		AgentTurnCompleted{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		AgentTurnFailed{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		AgentTurnCanceled{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		HumanRequested{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		HumanResponded{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		AssistantStatus{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		ToolCalled{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
		ToolResult{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now},
	}
	for _, e := range populated {
		t.Run(fmt.Sprintf("%T", e), func(t *testing.T) {
			e.isEvent()
			if e.ID() != wantID {
				t.Errorf("%T.ID() = %q, want %q", e, e.ID(), wantID)
			}
			if e.AgentTurnID() != wantTurn {
				t.Errorf("%T.AgentTurnID() = %q, want %q", e, e.AgentTurnID(), wantTurn)
			}
			if e.ParentAgentTurnID() != wantParent {
				t.Errorf("%T.ParentAgentTurnID() = %q, want %q", e, e.ParentAgentTurnID(), wantParent)
			}
			if !e.Timestamp().Equal(now) {
				t.Errorf("%T.Timestamp() = %v, want %v", e, e.Timestamp(), now)
			}
		})
	}
}

// Lifecycle messages carry TurnKind (W4 from PR #31).
func TestLifecycleEventsCarryTurnKind(t *testing.T) {
	cases := []struct {
		name string
		evt  Event
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
			switch e := c.evt.(type) {
			case AgentTurnStarted:
				got = e.TurnKind
			case AgentTurnCompleted:
				got = e.TurnKind
			case AgentTurnFailed:
				got = e.TurnKind
			case AgentTurnCanceled:
				got = e.TurnKind
			default:
				t.Fatalf("%s is not a lifecycle event carrying TurnKind: %T", c.name, c.evt)
			}
			if got != want[c.name] {
				t.Errorf("%s.TurnKind = %q, want %q", c.name, got, want[c.name])
			}
		})
	}
}

// Compile-time: every declared Event type satisfies the interface.
var (
	_ Event = AgentTurnStarted{}
	_ Event = AgentTurnCompleted{}
	_ Event = AgentTurnFailed{}
	_ Event = AgentTurnCanceled{}
	_ Event = HumanRequested{}
	_ Event = HumanResponded{}
	_ Event = AssistantStatus{}
	_ Event = ToolCalled{}
	_ Event = ToolResult{}
)

// -----------------------------------------------------------------------------
// Content types are PLAIN structs (no interface methods)
// -----------------------------------------------------------------------------

func TestInboundMessageIsPlainStruct(t *testing.T) {
	// InboundMessage is a plain struct carried by MessageBus's typed API. It
	// must NOT satisfy the Event interface (no isEvent method). The
	// "no isEvent method" property is enforced by sealed_exhaustive_test.go's
	// TestEventSealIsClosedAgainstSource — if InboundMessage ever grew
	// isEvent(), the source scan would find it and flag it as an undeclared
	// Event type.
	m := InboundMessage{
		MessageIDVal:   "im_1",
		AgentTurnIDVal: "aturn_root",
		Body:           "hello",
	}
	if m.MessageIDVal != "im_1" {
		t.Errorf("MessageIDVal = %q", m.MessageIDVal)
	}
	if m.Body != "hello" {
		t.Errorf("Body = %q", m.Body)
	}
}

func TestOutboundMessageIsPlainStruct(t *testing.T) {
	m := OutboundMessage{
		MessageIDVal: "om_1",
		Body:         "reply",
	}
	if m.MessageIDVal != "om_1" {
		t.Errorf("MessageIDVal = %q", m.MessageIDVal)
	}
	if m.Body != "reply" {
		t.Errorf("Body = %q", m.Body)
	}
}

// -----------------------------------------------------------------------------
// MessageBus typed API (compile-time interface shape)
// -----------------------------------------------------------------------------

func TestMessageBusTypedInterfaceShape(t *testing.T) {
	// MessageBus is a typed API for content. No Publish(Message), no Filter —
	// each content type has its own Publish method and typed channel.
	var _ MessageBus = fakeMessageBus{}
}

type fakeMessageBus struct{}

func (fakeMessageBus) PublishInbound(_ context.Context, _ InboundMessage) error   { return nil }
func (fakeMessageBus) PublishOutbound(_ context.Context, _ OutboundMessage) error { return nil }
func (fakeMessageBus) InboundChan() <-chan InboundMessage {
	ch := make(chan InboundMessage)
	close(ch)
	return ch
}
func (fakeMessageBus) OutboundChan() <-chan OutboundMessage {
	ch := make(chan OutboundMessage)
	close(ch)
	return ch
}
func (fakeMessageBus) Close() error { return nil }

// -----------------------------------------------------------------------------
// Filter.Match — takes an Event (not a Message). W2 empty-turn guard preserved.
// -----------------------------------------------------------------------------

func TestFilterMatch_EmptyFilterMatchesAll(t *testing.T) {
	f := Filter{}
	if !f.Match(AgentTurnStarted{AgentTurnIDVal: "a"}) {
		t.Error("empty Filter should match any event")
	}
}

func TestFilterMatch_ByAgentTurnID(t *testing.T) {
	f := Filter{AgentTurnID: turnPtr("aturn_1")}
	cases := []struct {
		name  string
		evt   Event
		match bool
	}{
		{"own turn", AgentTurnStarted{AgentTurnIDVal: "aturn_1"}, true},
		{"other turn", AgentTurnStarted{AgentTurnIDVal: "aturn_2"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := f.Match(c.evt); got != c.match {
				t.Errorf("Match = %v, want %v", got, c.match)
			}
		})
	}
}

func TestFilterMatch_IncludeChildren(t *testing.T) {
	f := Filter{AgentTurnID: turnPtr("aturn_parent"), IncludeChildren: true}
	childCompleted := AgentTurnCompleted{
		AgentTurnIDVal:       "aturn_child",
		ParentAgentTurnIDVal: "aturn_parent",
	}
	if !f.Match(childCompleted) {
		t.Error("parent filter with IncludeChildren must match child's event")
	}
	fNoChildren := Filter{AgentTurnID: turnPtr("aturn_parent"), IncludeChildren: false}
	if fNoChildren.Match(childCompleted) {
		t.Error("parent filter without IncludeChildren must not match child's event")
	}
}

func TestFilterMatch_SiblingChildExcluded(t *testing.T) {
	fChildA := Filter{AgentTurnID: turnPtr("aturn_childA"), IncludeChildren: true}
	siblingB := AgentTurnCompleted{
		AgentTurnIDVal:       "aturn_childB",
		ParentAgentTurnIDVal: "aturn_parent",
	}
	if fChildA.Match(siblingB) {
		t.Error("child A filter must not match sibling B's event (cross-sibling leak)")
	}
}

func TestFilterMatch_EmptyAgentTurnIDDoesNotLeakRoots(t *testing.T) {
	// W2 regression guard: filter with empty AgentTurnID must match nothing.
	emptyTurn := AgentTurnID("")
	f := Filter{AgentTurnID: &emptyTurn, IncludeChildren: true}
	rootEvt := AgentTurnStarted{AgentTurnIDVal: "aturn_root", ParentAgentTurnIDVal: ""}
	if f.Match(rootEvt) {
		t.Error("filter with empty AgentTurnID must not match root event (W2 leak)")
	}
	other := AgentTurnStarted{AgentTurnIDVal: "aturn_x", ParentAgentTurnIDVal: "aturn_p"}
	if f.Match(other) {
		t.Error("filter with empty AgentTurnID must not match any event")
	}
}

func TestFilterMatch_ByKinds(t *testing.T) {
	f := Filter{Kinds: []string{"agent_turn.completed", "agent_turn.failed"}}
	cases := []struct {
		name  string
		evt   Event
		match bool
	}{
		{"completed", AgentTurnCompleted{}, true},
		{"failed", AgentTurnFailed{}, true},
		{"started excluded", AgentTurnStarted{}, false},
		{"status excluded", AssistantStatus{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := f.Match(c.evt); got != c.match {
				t.Errorf("Match = %v, want %v", got, c.match)
			}
		})
	}
}

func TestFilterMatch_KindsAndAgentTurnIDCombined(t *testing.T) {
	f := Filter{
		AgentTurnID: turnPtr("aturn_1"),
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

func TestFilterMatch_NilAgentTurnIDEventWithAgentTurnFilter(t *testing.T) {
	f := Filter{AgentTurnID: turnPtr("aturn_1")}
	if f.Match(AgentTurnStarted{AgentTurnIDVal: ""}) {
		t.Error("empty AgentTurnID event must not match turn-scoped filter")
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func turnPtr(s string) *AgentTurnID { id := AgentTurnID(s); return &id }
