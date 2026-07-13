package runtime

import (
	"time"
)

// This file defines the Event sealed interface — the typed signal contract
// carried by the per-chat-key EventBus (event_bus.go) and delivered via
// ChatContext.Deliver. Per-chat-key routing (RFC #48) replaced the old
// global EventBus + Forwarder; the content-bus types (MessageBus interface,
// InboundMessage, OutboundMessage) and WAL/Filter/Reliable machinery from
// earlier Phase 1/2 drafts have been removed — content flows directly
// (channel.InboundEnvelope → RunAgent, channel.OutboundMessage ← Manager.Emit)
// and per-chat-key isolation obviates Filter.
//
// Contract locked by message_bus_test.go + sealed_exhaustive_test.go.

// EventPriority ranks how aggressively the bus protects an event under load.
// Ordered: Droppable < Critical. Used by ChatContext.evictFor for priority
// eviction when the per-chat-key queue is full (chatcontext.go).
type EventPriority int

const (
	// PriorityDroppable: under memory pressure, these are dropped first
	// (with log.Warn).
	PriorityDroppable EventPriority = iota
	// PriorityCritical: lifecycle + interaction signals + the AssistantFinal
	// drain control signal. Protected from eviction.
	PriorityCritical
)

// Event is the sealed bus event interface. Every concrete type in this file
// satisfies it. Add a type here AND in sealed_exhaustive_test.go's
// expectedEventTypes.
type Event interface {
	isEvent()
	// Kind is the stable string identifier (e.g. "agent_turn.completed").
	Kind() string
	// ID is the unique event identifier. Used for log correlation ONLY —
	// not for correctness (RFC Appendix C.5 CRITICAL 2: at-least-once
	// delivery means ID dedup is not a correctness mechanism).
	ID() string
	// AgentTurnID is the turn this event belongs to. Empty for system-
	// level events (none currently).
	AgentTurnID() AgentTurnID
	// ParentAgentTurnID returns the parent turn of the event's turn, or
	// empty if the event's turn is a root turn.
	ParentAgentTurnID() AgentTurnID
	// Timestamp is when the event was produced.
	Timestamp() time.Time
	// Priority ranks the event for eviction under memory pressure.
	Priority() EventPriority
}

// --- Turn lifecycle (Critical) ---

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
func (AgentTurnCanceled) Priority() EventPriority          { return PriorityCritical }

// --- HITL lifecycle (Critical) ---

// HumanRequested is published when a turn pauses for human input.
type HumanRequested struct {
	MessageIDVal         string
	AgentTurnIDVal       AgentTurnID
	ParentAgentTurnIDVal AgentTurnID
	TimestampVal         time.Time
	Question             string
	RequestID            string
	ResponderType        string
	DeliveryStatus       string
	SignalKind           string
}

func (HumanRequested) isEvent()                         {}
func (HumanRequested) Kind() string                     { return "human.requested" }
func (e HumanRequested) ID() string                     { return e.MessageIDVal }
func (e HumanRequested) AgentTurnID() AgentTurnID       { return e.AgentTurnIDVal }
func (e HumanRequested) ParentAgentTurnID() AgentTurnID { return e.ParentAgentTurnIDVal }
func (e HumanRequested) Timestamp() time.Time           { return e.TimestampVal }
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
func (HumanResponded) Priority() EventPriority          { return PriorityCritical }

// --- Progress (Droppable) ---

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
func (ToolResult) Priority() EventPriority          { return PriorityDroppable }

// AssistantFinal is published when the agent's final reply is ready
// (service.go:610, whitelist: final != "" && status == "completed"). It is
// the ChatContext's DRAIN control signal — NOT turn lifecycle (doesn't drive
// the state machine), NOT progress (doesn't render). Third category:
// control signal. Priority=PriorityCritical (drain must be timely, survives
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
func (AssistantFinal) Priority() EventPriority          { return PriorityCritical }
