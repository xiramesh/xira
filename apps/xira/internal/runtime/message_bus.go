package runtime

import (
	"context"
	"time"
)

// This file defines the Phase 1 dual-bus type contract (RFC §2.3, revised
// 2026-06-22 PR #41: single MessageBus → dual bus content vs signal).
//
// Phase 1 is TYPE-ONLY. It splits the old single `Message` interface into:
//   - Content: InboundMessage / OutboundMessage as PLAIN structs, carried by
//     the typed MessageBus API.
//   - Event: a sealed interface (9 types), carried by EventBus.
//
// Phase 2 evolves the existing event_bus.go EventBus struct (fan-out +
// priority eviction, already validated, AGENTS.md §1.1) to satisfy a future
// EventBus interface (add Filter + swap RuntimeEvent→Event + add WAL). Phase 1
// does NOT define that interface — only Event + Filter + MessageBus typed API,
// so Phase 2 can merge new and old concepts cleanly.
//
// Contract locked by message_bus_test.go + sealed_exhaustive_test.go.

// EventPriority ranks how aggressively the bus protects an event under load.
// Ordered: Droppable < Important < Critical. Renamed from MessagePriority
// (PR #41: only Event uses it now; content goes on the typed MessageBus).
type EventPriority int

const (
	// PriorityDroppable: under memory pressure, these are dropped first
	// (with log.Warn). Never persisted to WAL.
	PriorityDroppable EventPriority = iota
	// PriorityCritical: lifecycle + interaction signals. Persisted to WAL
	// and protected from eviction.
	//
	// PR #42 review WARNING-2: the previous PriorityImportant tier (between
	// Droppable and Critical) was removed — no Event returned it after the
	// dual-bus split, and MessageBus (content) has no eviction per RFC
	// §2.3.0a. If a future tier is needed, re-add here; the ordinal gap
	// from removing the middle value is fine (iota just needs monotonic <).
	PriorityCritical
)

// Event is the sealed bus event interface. Every concrete type in this file
// satisfies it. Add a type here AND in sealed_exhaustive_test.go's
// expectedEventTypes.
type Event interface {
	isEvent()
	// Kind is the stable string identifier (e.g. "agent_turn.completed").
	// Used by Filter.Kinds and WAL row tagging.
	Kind() string
	// ID is the unique event identifier. Used for log correlation ONLY —
	// not for correctness (RFC Appendix C.5 CRITICAL 2: at-least-once
	// delivery means ID dedup is not a correctness mechanism).
	ID() string
	// AgentTurnID is the turn this event belongs to. Empty for system-
	// level events (none currently).
	AgentTurnID() AgentTurnID
	// ParentAgentTurnID returns the parent turn of the event's turn, or
	// empty if the event's turn is a root turn. Used by Filter
	// IncludeChildren.
	ParentAgentTurnID() AgentTurnID
	// Timestamp is when the event was produced.
	Timestamp() time.Time
	// Reliable reports whether the bus persists this event to WAL.
	// Lifecycle events return true; progress events return false.
	Reliable() bool
	// Priority ranks the event for eviction under memory pressure.
	Priority() EventPriority
}

// --- Content types (PLAIN structs, carried by typed MessageBus) ---
//
// InboundMessage / OutboundMessage are NOT Event — they have no isEvent(),
// no Kind/ID/Priority. They are plain data structs for the content bus.
// This is the "content vs signal" split (RFC §2.3.0a/0b).

// InboundMessage is an IM → system content message (a user's chat message).
// Plain struct: carried by MessageBus.PublishInbound, no interface methods.
//
// Phase 2 TODO (PR #42 review WARNING-4): the semantics of
// ParentAgentTurnIDVal and TimestampVal are unsettled for content messages.
// Content doesn't form a turn parent-child hierarchy the way Events do
// (Inbound triggers a root turn; Outbound is its reply — there's no
// "parent content message"). Phase 2 must decide: keep these fields (and
// define what they mean for content), or drop them. Until then they're
// carried through opaquely.
type InboundMessage struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	Body                 string
}

// OutboundMessage is a system → IM content message (the agent's reply).
// Plain struct: carried by MessageBus.PublishOutbound, no interface methods.
// Same Phase 2 TODO as InboundMessage re: ParentAgentTurnIDVal/TimestampVal.
type OutboundMessage struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	Body                 string
}

// --- Turn lifecycle (Reliable + Critical, persisted to WAL) ---

// AgentTurnStarted is published when a new turn enters Running.
type AgentTurnStarted struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	// TurnKind is the Kind of the turn that started (flow | agent). Lets a
	// subscriber tell flow-run starts from agent-run starts without loading
	// the turn. PR #31 review W4: carried on all lifecycle events.
	TurnKind AgentTurnKind
}

func (AgentTurnStarted) isEvent()                         {}
func (AgentTurnStarted) Kind() string                     { return "agent_turn.started" }
func (e AgentTurnStarted) ID() string                     { return e.MessageIDVal }
func (e AgentTurnStarted) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e AgentTurnStarted) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e AgentTurnStarted) Timestamp() time.Time           { return e.TimestampVal }
func (AgentTurnStarted) Reliable() bool                   { return true }
func (AgentTurnStarted) Priority() EventPriority          { return PriorityCritical }

// AgentTurnCompleted is published when a turn finishes successfully.
type AgentTurnCompleted struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	TurnKind             AgentTurnKind
	Summary              string
}

func (AgentTurnCompleted) isEvent()                         {}
func (AgentTurnCompleted) Kind() string                     { return "agent_turn.completed" }
func (e AgentTurnCompleted) ID() string                     { return e.MessageIDVal }
func (e AgentTurnCompleted) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e AgentTurnCompleted) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e AgentTurnCompleted) Timestamp() time.Time           { return e.TimestampVal }
func (AgentTurnCompleted) Reliable() bool                   { return true }
func (AgentTurnCompleted) Priority() EventPriority          { return PriorityCritical }

// AgentTurnFailed is published when a turn fails.
type AgentTurnFailed struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	TurnKind             AgentTurnKind
	Error                string
}

func (AgentTurnFailed) isEvent()                         {}
func (AgentTurnFailed) Kind() string                     { return "agent_turn.failed" }
func (e AgentTurnFailed) ID() string                     { return e.MessageIDVal }
func (e AgentTurnFailed) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e AgentTurnFailed) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e AgentTurnFailed) Timestamp() time.Time           { return e.TimestampVal }
func (AgentTurnFailed) Reliable() bool                   { return true }
func (AgentTurnFailed) Priority() EventPriority          { return PriorityCritical }

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

func (AgentTurnCanceled) isEvent()                         {}
func (AgentTurnCanceled) Kind() string                     { return "agent_turn.canceled" }
func (e AgentTurnCanceled) ID() string                     { return e.MessageIDVal }
func (e AgentTurnCanceled) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e AgentTurnCanceled) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e AgentTurnCanceled) Timestamp() time.Time           { return e.TimestampVal }
func (AgentTurnCanceled) Reliable() bool                   { return true }
func (AgentTurnCanceled) Priority() EventPriority          { return PriorityCritical }

// --- HITL lifecycle (Reliable + Critical) ---

// HumanRequested is published when a turn pauses for human input.
type HumanRequested struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	Question             string
}

func (HumanRequested) isEvent()                         {}
func (HumanRequested) Kind() string                     { return "human.requested" }
func (e HumanRequested) ID() string                     { return e.MessageIDVal }
func (e HumanRequested) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e HumanRequested) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e HumanRequested) Timestamp() time.Time           { return e.TimestampVal }
func (HumanRequested) Reliable() bool                   { return true }
func (HumanRequested) Priority() EventPriority          { return PriorityCritical }

// HumanResponded is published when a human responds to a HITL request.
type HumanResponded struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	Response             string
}

func (HumanResponded) isEvent()                         {}
func (HumanResponded) Kind() string                     { return "human.responded" }
func (e HumanResponded) ID() string                     { return e.MessageIDVal }
func (e HumanResponded) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e HumanResponded) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e HumanResponded) Timestamp() time.Time           { return e.TimestampVal }
func (HumanResponded) Reliable() bool                   { return true }
func (HumanResponded) Priority() EventPriority          { return PriorityCritical }

// --- Progress (not Reliable, Droppable) ---

// AssistantStatus is a progress heartbeat emitted during a turn.
type AssistantStatus struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	Text                 string
}

func (AssistantStatus) isEvent()                         {}
func (AssistantStatus) Kind() string                     { return "assistant.status" }
func (e AssistantStatus) ID() string                     { return e.MessageIDVal }
func (e AssistantStatus) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e AssistantStatus) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e AssistantStatus) Timestamp() time.Time           { return e.TimestampVal }
func (AssistantStatus) Reliable() bool                   { return false }
func (AssistantStatus) Priority() EventPriority          { return PriorityDroppable }

// ToolCalled is published when a tool is invoked during a turn.
type ToolCalled struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	ToolName             string
}

func (ToolCalled) isEvent()                         {}
func (ToolCalled) Kind() string                     { return "tool.called" }
func (e ToolCalled) ID() string                     { return e.MessageIDVal }
func (e ToolCalled) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e ToolCalled) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e ToolCalled) Timestamp() time.Time           { return e.TimestampVal }
func (ToolCalled) Reliable() bool                   { return false }
func (ToolCalled) Priority() EventPriority          { return PriorityDroppable }

// ToolResult is published when a tool call completes.
type ToolResult struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	ToolName             string
}

func (ToolResult) isEvent()                         {}
func (ToolResult) Kind() string                     { return "tool.result" }
func (e ToolResult) ID() string                     { return e.MessageIDVal }
func (e ToolResult) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e ToolResult) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e ToolResult) Timestamp() time.Time           { return e.TimestampVal }
func (ToolResult) Reliable() bool                   { return false }
func (ToolResult) Priority() EventPriority          { return PriorityDroppable }

// AssistantFinal is published when the agent's final reply is ready
// (service.go:610, whitelist: final != "" && status == "completed"). It is
// the forwarder's DRAIN control signal — NOT turn lifecycle (doesn't drive
// the state machine), NOT progress (doesn't render). Third category:
// control signal. Reliable=false (no WAL — it doesn't drive cross-process
// state machine), Priority=PriorityCritical (drain must be timely, survives
// eviction).
type AssistantFinal struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	FinalChars           int
}

func (AssistantFinal) isEvent()                         {}
func (AssistantFinal) Kind() string                     { return "assistant.final" }
func (e AssistantFinal) ID() string                     { return e.MessageIDVal }
func (e AssistantFinal) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e AssistantFinal) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e AssistantFinal) Timestamp() time.Time           { return e.TimestampVal }
func (AssistantFinal) Reliable() bool                   { return false }
func (AssistantFinal) Priority() EventPriority          { return PriorityCritical }

// Filter selects which events a subscriber receives. All predicates are
// AND-ed: an event matches only if every non-zero predicate matches.
type Filter struct {
	// AgentTurnID, if non-nil, restricts to events whose AgentTurnID
	// equals it (or, with IncludeChildren, whose ParentAgentTurnID equals
	// it).
	AgentTurnID *AgentTurnID
	// Kinds, if non-empty, restricts to events whose Kind() is in the list.
	Kinds []string
	// IncludeChildren extends an AgentTurnID filter to also match events
	// from direct children of that turn. Does NOT match siblings (RFC §2.3
	// scope/permission: a child must not see its sibling's events).
	IncludeChildren bool
}

// Match reports whether evt passes the filter. AND semantics: every non-zero
// predicate must hold.
func (f Filter) Match(evt Event) bool {
	if f.AgentTurnID != nil {
		// An empty AgentTurnID in the filter is a caller bug (turn ids are
		// always non-empty). Treat it as "matches nothing" rather than as a
		// wildcard — otherwise a filter with turn="" + IncludeChildren leaks
		// every root event (whose ParentAgentTurnID is also "").
		// PR #31 review W2: previously this case matched all root events.
		if *f.AgentTurnID == "" {
			return false
		}
		turn := evt.AgentTurnID()
		// A direct child has ParentAgentTurnID == filter turn AND a different
		// own turn id. The "different own turn" guard prevents a self-event
		// from being counted as a child event.
		isDirectChild := evt.ParentAgentTurnID() == *f.AgentTurnID && turn != *f.AgentTurnID
		if turn != *f.AgentTurnID {
			if !f.IncludeChildren || !isDirectChild {
				return false
			}
		}
		// turn == filter turn: own event, passes the turn predicate.
	}
	if len(f.Kinds) > 0 {
		k := evt.Kind()
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

// MessageBus is the content bus interface (RFC §2.3.0a). Typed API: each
// content type has its own Publish method and typed channel. No Filter
// (type = filter).
//
// Phase 1 defines the interface only — no struct implements it yet. Phase 2
// adds the implementation with in-memory channels + SQLite WAL (D-1: persist
// on receipt + dedup by composite key, never trust channel retransmit).
type MessageBus interface {
	// PublishInbound delivers an inbound content message.
	//
	// Phase 2 contract (NOT yet implemented in Phase 1): persists to WAL
	// (D-1) + dedups by (Channel, ChatID, MessageID), then delivers. Blocking
	// (timeout=0, never drops in-memory). Phase 1 has no implementation —
	// do not assume persistence works until Phase 2 lands.
	PublishInbound(ctx context.Context, msg InboundMessage) error
	// PublishOutbound delivers an outbound content message.
	//
	// Phase 2 contract (NOT yet implemented in Phase 1): persists + delivers,
	// blocking. Same caveat as PublishInbound.
	PublishOutbound(ctx context.Context, msg OutboundMessage) error
	// InboundChan returns the typed channel for inbound content.
	//
	// Single-consumer semantics (RFC §2.3.0a): content is NOT fan-out —
	// exactly one consumer drains this channel. Blocking backpressure: if
	// the consumer is slow, Publish blocks (content must not be dropped or
	// competing-consumed). Do NOT implement as fan-out like EventBus —
	// fan-out would let two consumers race on the same inbound and trigger
	// duplicate turns.
	InboundChan() <-chan InboundMessage
	// OutboundChan returns the typed channel for outbound content.
	// Same single-consumer + blocking-backpressure semantics as InboundChan.
	OutboundChan() <-chan OutboundMessage
	// Close shuts the bus down.
	Close() error
}
