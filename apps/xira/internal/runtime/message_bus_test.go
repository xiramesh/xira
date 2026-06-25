package runtime

import (
	"fmt"
	"testing"
	"time"
)

// These tests lock in the Event sealed-interface contract (the typed signal
// carried by the per-chat-key EventBus). The content-bus types (MessageBus,
// InboundMessage, OutboundMessage) and Filter/Reliable machinery from earlier
// Phase 1/2 drafts have been removed — per-chat-key routing (RFC #48) replaced
// the content bus, and per-chat-key isolation obviates Filter. Content flows
// directly via channel.InboundEnvelope / channel.OutboundMessage.
//
// What this file locks in:
//   - Event is a sealed interface; every kind declares isEvent() and its own
//     Priority() routing tag. EventPriority is the only priority type now.
//   - The 10 Event kinds declare the expected Kind() strings.
//   - Lifecycle/HITL events are PriorityCritical; progress events are
//     PriorityDroppable; AssistantFinal is Critical (drain control signal).

// -----------------------------------------------------------------------------
// EventPriority enum
// -----------------------------------------------------------------------------

func TestEventPriorityOrdering(t *testing.T) {
	// Ordering must hold so ChatContext.evictFor can compare with < / >.
	// Only Droppable < Critical remains (the old PriorityImportant middle tier
	// was removed — no Event returned it).
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
		{AssistantFinal{}, "assistant.final"},
		{ToolCalled{}, "tool.called"},
		{ToolResult{}, "tool.result"},
	}
	for _, c := range cases {
		if got := c.evt.Kind(); got != c.want {
			t.Errorf("%T.Kind() = %q, want %q", c.evt, got, c.want)
		}
	}
}

func TestEventPriority(t *testing.T) {
	// The single source of truth for which events survive eviction under
	// load (ChatContext.evictFor, chatcontext.go). Critical events are
	// protected; Droppable events are evicted first.
	critical := []Event{
		AgentTurnStarted{}, AgentTurnCompleted{}, AgentTurnFailed{}, AgentTurnCanceled{},
		HumanRequested{}, HumanResponded{},
		AssistantFinal{}, // drain control signal — must survive eviction
	}
	for _, e := range critical {
		if e.Priority() != PriorityCritical {
			t.Errorf("%T.Priority() = %d, want Critical", e, e.Priority())
		}
	}
	droppable := []Event{
		AssistantStatus{}, ToolCalled{}, ToolResult{},
	}
	for _, e := range droppable {
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
		AssistantFinal{MessageIDVal: wantID, AgentTurnIDVal: wantTurn, ParentAgentTurnIDVal: wantParent, TimestampVal: now, FinalChars: 42},
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
	_ Event = AssistantFinal{}
	_ Event = ToolCalled{}
	_ Event = ToolResult{}
)
