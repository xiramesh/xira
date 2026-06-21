package runtime

import (
	"context"
	"time"
)

// This file defines the Phase 1 MessageBus type contract (RFC §2.3). Phase 1
// is type-only: interface + sealed Message types + Filter. Phase 2 implements
// in-memory bus + SQLite WAL.
//
// Contract locked by message_bus_test.go:
//   - Message is a sealed interface; every kind declares its own struct and
//     its own Reliable()/Priority() routing tags.
//   - Routing table (RFC §2.3): lifecycle messages (AgentTurn*Started/
//     Completed/Failed/Canceled, Human*) are Reliable + Critical (persisted
//     to WAL, protected from eviction). Progress messages (AssistantStatus,
//     Tool*) are not Reliable + Droppable (best-effort in memory).
//     Inbound/Outbound are Important (user-facing, reach IM, but not
//     lifecycle-persisted).
//   - Filter.Match implements the AND semantics: AgentTurnID + Kinds +
//     IncludeChildren all hold. IncludeChildren grants a parent visibility
//     into its direct children's events but NOT siblings (RFC §2.3 scope).

// MessagePriority ranks how aggressively the bus protects a message under
// load. Ordered: Droppable < Important < Critical (see
// TestMessagePriorityOrdering).
type MessagePriority int

const (
	// PriorityDroppable: under memory pressure, these are dropped first
	// (with log.Warn). Never persisted to WAL.
	PriorityDroppable MessagePriority = iota
	// PriorityImportant: user-facing, must reach IM, but not lifecycle.
	// Evicted only if a Critical message needs the slot.
	PriorityImportant
	// PriorityCritical: lifecycle + interaction signals. Persisted to WAL
	// and protected from eviction (can evict Important/Droppable).
	PriorityCritical
)

// Message is the sealed bus message interface. Every concrete type in this
// file satisfies it. Add a type here AND in TestMessageSealedExhaustive.
type Message interface {
	isMessage()
	// Kind is the stable string identifier (e.g. "agent_turn.completed").
	// Used by Filter.Kinds and WAL row tagging.
	Kind() string
	// ID is the unique message identifier. Used for log correlation ONLY —
	// not for correctness (RFC Appendix C.5 CRITICAL 2: at-least-once
	// delivery means ID dedup is not a correctness mechanism).
	ID() string
	// AgentTurnID is the turn this message belongs to. Empty for system-
	// level messages (none currently).
	AgentTurnID() AgentTurnID
	// ParentAgentTurnID returns the parent turn of the message's turn, or
	// empty if the message's turn is a root turn. Used by Filter
	// IncludeChildren.
	ParentAgentTurnID() AgentTurnID
	// Timestamp is when the message was produced.
	Timestamp() time.Time
	// Reliable reports whether the bus persists this message to WAL.
	// Lifecycle messages return true; progress messages return false.
	Reliable() bool
	// Priority ranks the message for eviction under memory pressure.
	Priority() MessagePriority
}

// --- Inbound / Outbound (IM boundary) ---

// InboundMessage is an IM → system message (a user's chat message entered
// the system). Important: user-facing, must reach IM consumers, but not a
// lifecycle event driving the turn state machine.
type InboundMessage struct {
	MessageIDVal   string
	AgentTurnIDVal AgentTurnID
	ParentIDVal    AgentTurnID
	TimestampVal   time.Time
	Body           string
}

func (InboundMessage) isMessage()                       {}
func (InboundMessage) Kind() string                     { return "inbound" }
func (m InboundMessage) ID() string                     { return m.MessageIDVal }
func (m InboundMessage) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m InboundMessage) ParentAgentTurnID() AgentTurnID { return m.ParentIDVal }
func (m InboundMessage) Timestamp() time.Time           { return m.TimestampVal }
func (InboundMessage) Reliable() bool                   { return false }
func (InboundMessage) Priority() MessagePriority        { return PriorityImportant }

// OutboundMessage is a system → IM message (the agent's reply back to the
// chat). Same priority tier as InboundMessage.
type OutboundMessage struct {
	MessageIDVal   string
	AgentTurnIDVal AgentTurnID
	ParentIDVal    AgentTurnID
	TimestampVal   time.Time
	Body           string
}

func (OutboundMessage) isMessage()                       {}
func (OutboundMessage) Kind() string                     { return "outbound" }
func (m OutboundMessage) ID() string                     { return m.MessageIDVal }
func (m OutboundMessage) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m OutboundMessage) ParentAgentTurnID() AgentTurnID { return m.ParentIDVal }
func (m OutboundMessage) Timestamp() time.Time           { return m.TimestampVal }
func (OutboundMessage) Reliable() bool                   { return false }
func (OutboundMessage) Priority() MessagePriority        { return PriorityImportant }

// --- Turn lifecycle (Reliable + Critical, persisted to WAL) ---

// AgentTurnStarted is published when a new turn enters Running.
type AgentTurnStarted struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	// TurnKind is the Kind of the turn that started (flow | agent). Lets a
	// subscriber tell flow-run starts from agent-run starts without loading
	// the turn. PR #31 review W4: carried on all lifecycle messages.
	TurnKind AgentTurnKind
}

func (AgentTurnStarted) isMessage()                       {}
func (AgentTurnStarted) Kind() string                     { return "agent_turn.started" }
func (m AgentTurnStarted) ID() string                     { return m.MessageIDVal }
func (m AgentTurnStarted) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m AgentTurnStarted) ParentAgentTurnID() AgentTurnID { return m.ParentAgentTurnIDVal }
func (m AgentTurnStarted) Timestamp() time.Time           { return m.TimestampVal }
func (AgentTurnStarted) Reliable() bool                   { return true }
func (AgentTurnStarted) Priority() MessagePriority        { return PriorityCritical }

// AgentTurnCompleted is published when a turn finishes successfully.
type AgentTurnCompleted struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	TurnKind             AgentTurnKind
	Summary              string
}

func (AgentTurnCompleted) isMessage()                       {}
func (AgentTurnCompleted) Kind() string                     { return "agent_turn.completed" }
func (m AgentTurnCompleted) ID() string                     { return m.MessageIDVal }
func (m AgentTurnCompleted) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m AgentTurnCompleted) ParentAgentTurnID() AgentTurnID { return m.ParentAgentTurnIDVal }
func (m AgentTurnCompleted) Timestamp() time.Time           { return m.TimestampVal }
func (AgentTurnCompleted) Reliable() bool                   { return true }
func (AgentTurnCompleted) Priority() MessagePriority        { return PriorityCritical }

// AgentTurnFailed is published when a turn fails.
type AgentTurnFailed struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	TurnKind             AgentTurnKind
	Error                string
}

func (AgentTurnFailed) isMessage()                       {}
func (AgentTurnFailed) Kind() string                     { return "agent_turn.failed" }
func (m AgentTurnFailed) ID() string                     { return m.MessageIDVal }
func (m AgentTurnFailed) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m AgentTurnFailed) ParentAgentTurnID() AgentTurnID { return m.ParentAgentTurnIDVal }
func (m AgentTurnFailed) Timestamp() time.Time           { return m.TimestampVal }
func (AgentTurnFailed) Reliable() bool                   { return true }
func (AgentTurnFailed) Priority() MessagePriority        { return PriorityCritical }

// AgentTurnCanceled is published when a turn is canceled (e.g. user denied
// HITL).
type AgentTurnCanceled struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	TurnKind             AgentTurnKind
	Reason               string
}

func (AgentTurnCanceled) isMessage()                       {}
func (AgentTurnCanceled) Kind() string                     { return "agent_turn.canceled" }
func (m AgentTurnCanceled) ID() string                     { return m.MessageIDVal }
func (m AgentTurnCanceled) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m AgentTurnCanceled) ParentAgentTurnID() AgentTurnID { return m.ParentAgentTurnIDVal }
func (m AgentTurnCanceled) Timestamp() time.Time           { return m.TimestampVal }
func (AgentTurnCanceled) Reliable() bool                   { return true }
func (AgentTurnCanceled) Priority() MessagePriority        { return PriorityCritical }

// --- HITL lifecycle (Reliable + Critical) ---

// HumanRequested is published when a turn pauses for human input.
type HumanRequested struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	Question             string
}

func (HumanRequested) isMessage()                       {}
func (HumanRequested) Kind() string                     { return "human.requested" }
func (m HumanRequested) ID() string                     { return m.MessageIDVal }
func (m HumanRequested) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m HumanRequested) ParentAgentTurnID() AgentTurnID { return m.ParentAgentTurnIDVal }
func (m HumanRequested) Timestamp() time.Time           { return m.TimestampVal }
func (HumanRequested) Reliable() bool                   { return true }
func (HumanRequested) Priority() MessagePriority        { return PriorityCritical }

// HumanResponded is published when a human responds to a HITL request.
type HumanResponded struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	Response             string
}

func (HumanResponded) isMessage()                       {}
func (HumanResponded) Kind() string                     { return "human.responded" }
func (m HumanResponded) ID() string                     { return m.MessageIDVal }
func (m HumanResponded) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m HumanResponded) ParentAgentTurnID() AgentTurnID { return m.ParentAgentTurnIDVal }
func (m HumanResponded) Timestamp() time.Time           { return m.TimestampVal }
func (HumanResponded) Reliable() bool                   { return true }
func (HumanResponded) Priority() MessagePriority        { return PriorityCritical }

// --- Progress (not Reliable, Droppable) ---

// AssistantStatus is a progress heartbeat emitted during a turn.
type AssistantStatus struct {
	MessageIDVal   string
	AgentTurnIDVal AgentTurnID
	ParentIDVal    AgentTurnID
	TimestampVal   time.Time
	Text           string
}

func (AssistantStatus) isMessage()                       {}
func (AssistantStatus) Kind() string                     { return "assistant.status" }
func (m AssistantStatus) ID() string                     { return m.MessageIDVal }
func (m AssistantStatus) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m AssistantStatus) ParentAgentTurnID() AgentTurnID { return m.ParentIDVal }
func (m AssistantStatus) Timestamp() time.Time           { return m.TimestampVal }
func (AssistantStatus) Reliable() bool                   { return false }
func (AssistantStatus) Priority() MessagePriority        { return PriorityDroppable }

// ToolCalled is published when a tool is invoked during a turn.
type ToolCalled struct {
	MessageIDVal   string
	AgentTurnIDVal AgentTurnID
	ParentIDVal    AgentTurnID
	TimestampVal   time.Time
	ToolName       string
}

func (ToolCalled) isMessage()                       {}
func (ToolCalled) Kind() string                     { return "tool.called" }
func (m ToolCalled) ID() string                     { return m.MessageIDVal }
func (m ToolCalled) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m ToolCalled) ParentAgentTurnID() AgentTurnID { return m.ParentIDVal }
func (m ToolCalled) Timestamp() time.Time           { return m.TimestampVal }
func (ToolCalled) Reliable() bool                   { return false }
func (ToolCalled) Priority() MessagePriority        { return PriorityDroppable }

// ToolResult is published when a tool call completes.
type ToolResult struct {
	MessageIDVal   string
	AgentTurnIDVal AgentTurnID
	ParentIDVal    AgentTurnID
	TimestampVal   time.Time
	ToolName       string
}

func (ToolResult) isMessage()                       {}
func (ToolResult) Kind() string                     { return "tool.result" }
func (m ToolResult) ID() string                     { return m.MessageIDVal }
func (m ToolResult) AgentTurnID() AgentTurnID       { return m.AgentTurnIDVal }
func (m ToolResult) ParentAgentTurnID() AgentTurnID { return m.ParentIDVal }
func (m ToolResult) Timestamp() time.Time           { return m.TimestampVal }
func (ToolResult) Reliable() bool                   { return false }
func (ToolResult) Priority() MessagePriority        { return PriorityDroppable }

// Filter selects which messages a subscriber receives. All predicates are
// AND-ed: a message matches only if every non-zero predicate matches.
type Filter struct {
	// AgentTurnID, if non-nil, restricts to messages whose AgentTurnID
	// equals it (or, with IncludeChildren, whose ParentAgentTurnID equals
	// it).
	AgentTurnID *AgentTurnID
	// Kinds, if non-empty, restricts to messages whose Kind() is in the
	// list.
	Kinds []string
	// IncludeChildren extends an AgentTurnID filter to also match messages
	// from direct children of that turn. Does NOT match siblings (RFC §2.3
	// scope/permission: a child must not see its sibling's events).
	IncludeChildren bool
}

// Match reports whether msg passes the filter. AND semantics: every non-zero
// predicate must hold.
func (f Filter) Match(msg Message) bool {
	if f.AgentTurnID != nil {
		// An empty AgentTurnID in the filter is a caller bug (turn ids are
		// always non-empty). Treat it as "matches nothing" rather than as a
		// wildcard — otherwise a filter with turn="" + IncludeChildren leaks
		// every root event (whose ParentAgentTurnID is also "").
		// PR #31 review W2: previously this case matched all root events.
		if *f.AgentTurnID == "" {
			return false
		}
		turn := msg.AgentTurnID()
		// A direct child has ParentAgentTurnID == filter turn AND a different
		// own turn id. The "different own turn" guard prevents a self-event
		// (turn == filter turn, parent == filter turn, which never happens
		// in practice but is defensive) from being counted as a child.
		isDirectChild := msg.ParentAgentTurnID() == *f.AgentTurnID && turn != *f.AgentTurnID
		if turn != *f.AgentTurnID {
			if !f.IncludeChildren || !isDirectChild {
				return false
			}
		}
		// turn == filter turn: own event, passes the turn predicate.
	}
	if len(f.Kinds) > 0 {
		k := msg.Kind()
		matched := false
		for _, want := range f.Kinds {
			if k == want {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// SubID identifies one subscription. Returned by Subscribe, passed to
// Unsubscribe.
type SubID string

// MessageBus is the Phase 1 interface. Phase 2 implements this with an
// in-memory bus + SQLite WAL (RFC §2.3, Appendix C).
type MessageBus interface {
	// Publish delivers msg to all matching subscribers. For Reliable()
	// messages, implementation persists to WAL before returning; a
	// persistence error is returned (caller decides). For non-Reliable
	// messages, publication is best-effort and never returns an error
	// arising from memory pressure (only context/encoding errors).
	Publish(ctx context.Context, msg Message) error
	// Subscribe registers a filter and returns a channel of matching
	// messages plus a SubID. The channel is closed when Unsubscribe is
	// called or the bus shuts down.
	Subscribe(filter Filter) (SubID, <-chan Message)
	// Unsubscribe cancels a subscription and closes its channel.
	Unsubscribe(id SubID)
}
