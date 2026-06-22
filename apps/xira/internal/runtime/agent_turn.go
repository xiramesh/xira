package runtime

import (
	"fmt"
	"time"

	fsession "github.com/xiramesh/xira/internal/session"
)

// This file defines the Phase 1 AgentTurn type contract — the envelope +
// payload first-class execution unit (RFC §2.1). Phase 1 is type-only: no
// runtime behavior changes, no wiring into service.go / RunAgent. Phase 2+
// migrates existing TurnRequest/RunChildAgent paths onto AgentTurn.
//
// Key contract points (locked by agent_turn_test.go):
//   - AgentTurn is the SOLE first-class execution unit. Flow run and agent
//     run are both AgentTurn, distinguished by Kind.
//   - No SubTurn type: a child turn is an AgentTurn with ParentAgentTurnID
//     set. Parent-child is a field, not a type (RFC §2.2).
//   - SessionScope is a pointer (nil allowed) + InheritSession flag —
//     方案 Z (RFC §2.1). nil propagation: nil parent → nil child; non-nil
//     parent + InheritSession=false → nil child (worker uses ephemeral
//     session, aligns with the "ephemeral_worker:" AgentSessionID prefix
//     assigned in delegation.go's RunChildAgent).
//   - Status state machine: IsValidTransition enforces legal transitions
//     AND makes terminal states idempotent (RFC Appendix C.5 CRITICAL 2:
//     at-least-once delivery must be safe to re-apply).

// AgentTurnID uniquely identifies one execution unit. Empty when used as
// ParentAgentTurnID means "root turn" (IM-triggered, no parent).
type AgentTurnID string

// AgentTurnKind distinguishes flow-run turns from agent-run turns. Closed
// enum; add a kind here AND in TestAgentTurnKindIsClosed.
type AgentTurnKind string

const (
	// AgentTurnKindFlow marks a turn that runs a Flow (orchestration). Its
	// payload is FlowPayload. A flow turn spawns agent turns as children.
	AgentTurnKindFlow AgentTurnKind = "flow"
	// AgentTurnKindAgent marks a turn that runs a single agent's LLM loop.
	// Its payload is AgentPayload. Spawned by a flow turn or by another
	// agent turn via spawn_turn (Phase 3).
	AgentTurnKindAgent AgentTurnKind = "agent"
)

// AgentTurnStatus is the lifecycle state of a turn. Closed enum.
type AgentTurnStatus string

const (
	// AgentTurnStatusRequested: turn has been created but not started.
	AgentTurnStatusRequested AgentTurnStatus = "requested"
	// AgentTurnStatusRunning: turn is actively executing.
	AgentTurnStatusRunning AgentTurnStatus = "running"
	// AgentTurnStatusWaitingHuman: turn paused awaiting a human response
	// (HITL). Can resume to Running or exit to a terminal state.
	AgentTurnStatusWaitingHuman AgentTurnStatus = "waiting_human"
	// AgentTurnStatusCompleted: turn finished successfully. Terminal.
	AgentTurnStatusCompleted AgentTurnStatus = "completed"
	// AgentTurnStatusFailed: turn failed (verification, tool, or error).
	// Terminal.
	AgentTurnStatusFailed AgentTurnStatus = "failed"
	// AgentTurnStatusCanceled: turn was canceled (e.g. user denied HITL).
	// Terminal.
	AgentTurnStatusCanceled AgentTurnStatus = "canceled"
	// AgentTurnStatusTimeout: turn exceeded its deadline. Terminal.
	AgentTurnStatusTimeout AgentTurnStatus = "timeout"
)

// IsTerminal reports whether s is a terminal state (no further transitions
// except the idempotent self-transition, see IsValidTransition).
func (s AgentTurnStatus) IsTerminal() bool {
	switch s {
	case AgentTurnStatusCompleted, AgentTurnStatusFailed,
		AgentTurnStatusCanceled, AgentTurnStatusTimeout:
		return true
	default:
		return false
	}
}

// TransitionError describes an illegal status transition. Returned by
// IsValidTransition so callers can distinguish a state-machine rejection
// (retryable via CAS, loggable) from a system error (e.g. WAL failure).
// Exported in Phase 1 debt cleanup (review W7): bus/subscribers in Phase 2
// type-assert on this to decide CAS-retry vs fatal handling.
type TransitionError struct {
	From AgentTurnStatus
	To   AgentTurnStatus
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("invalid agent turn transition: %s → %s", e.From, e.To)
}

// IsValidTransition returns nil if `from → to` is a legal state-machine
// transition, otherwise a non-nil error.
//
// Idempotent terminal rule (RFC Appendix C.5 CRITICAL 2): re-applying a
// terminal status to itself is a no-op (returns nil), NOT an error. This
// is what lets an at-least-once bus deliver a duplicate AgentTurnCompleted
// safely — the subscriber's CAS (only-once spawn) relies on this being a
// no-op rather than a failure.
func IsValidTransition(from, to AgentTurnStatus) error {
	// Idempotent terminal self-transition: the one legal "terminal → same
	// terminal" move. Duplicate completion events are no-ops.
	if from == to && to.IsTerminal() {
		return nil
	}
	// No transitions out of a terminal state (other than the idempotent
	// self-transition handled above).
	if from.IsTerminal() {
		return &TransitionError{From: from, To: to}
	}
	// Legal non-terminal transitions.
	switch {
	case from == AgentTurnStatusRequested && to == AgentTurnStatusRunning:
		return nil
	// requested → terminal: a turn can fail/cancel/timeout before it ever
	// reaches Running (agent profile missing, startup rejected, deadline
	// hit during scheduling). PR #31 review W1: these were missing.
	case from == AgentTurnStatusRequested && to == AgentTurnStatusFailed:
		return nil
	case from == AgentTurnStatusRequested && to == AgentTurnStatusCanceled:
		return nil
	case from == AgentTurnStatusRequested && to == AgentTurnStatusTimeout:
		return nil
	case from == AgentTurnStatusRunning && to == AgentTurnStatusWaitingHuman:
		return nil
	case from == AgentTurnStatusRunning && to == AgentTurnStatusCompleted:
		return nil
	case from == AgentTurnStatusRunning && to == AgentTurnStatusFailed:
		return nil
	case from == AgentTurnStatusRunning && to == AgentTurnStatusTimeout:
		return nil
	case from == AgentTurnStatusRunning && to == AgentTurnStatusCanceled:
		return nil
	case from == AgentTurnStatusWaitingHuman && to == AgentTurnStatusRunning:
		return nil
	case from == AgentTurnStatusWaitingHuman && to == AgentTurnStatusCompleted:
		return nil
	case from == AgentTurnStatusWaitingHuman && to == AgentTurnStatusFailed:
		return nil
	case from == AgentTurnStatusWaitingHuman && to == AgentTurnStatusCanceled:
		return nil
	// waiting_human → timeout: a HITL pause can exceed its deadline.
	// PR #31 review W1: this was missing.
	case from == AgentTurnStatusWaitingHuman && to == AgentTurnStatusTimeout:
		return nil
	default:
		return &TransitionError{From: from, To: to}
	}
}

// AgentTurn is the sole first-class execution unit (RFC §2.1 envelope +
// payload). Every flow run and every agent run is an AgentTurn. Parent-child
// is expressed by ParentAgentTurnID, not by a sub-type.
type AgentTurn struct {
	// ID is the unique identifier of this turn. Always non-empty for a
	// created turn.
	ID AgentTurnID
	// ParentAgentTurnID is the ID of the turn that spawned this one. Empty
	// means this is a root turn (IM-triggered, or a flow run started by the
	// system).
	ParentAgentTurnID AgentTurnID
	// Kind distinguishes flow-run turns from agent-run turns. Selects how
	// Payload is interpreted.
	Kind AgentTurnKind
	// Status is the current lifecycle state. Advance via IsValidTransition.
	Status AgentTurnStatus
	// StartedAt is when the turn entered Running. Zero before it starts.
	StartedAt time.Time
	// EndedAt is when the turn entered a terminal state. Zero while active.
	EndedAt time.Time
	// SessionScope carries the conversation-session identity (RFC §2.1
	// 方案 Z). Nil means no IM trigger identity (system-maintenance turn).
	// Nil propagation: nil parent → nil child; non-nil parent +
	// InheritSession=false → nil child (worker ephemeral session).
	SessionScope *fsession.SessionScope
	// InheritSession controls whether a child spawned from this turn
	// inherits SessionScope. flow→agent defaults true (conversational
	// continuity); agent→agent defaults false (ephemeral worker, aligns
	// with the "ephemeral_worker:" AgentSessionID prefix assigned in
	// delegation.go's RunChildAgent).
	InheritSession bool
	// Payload is the kind-specific content. Sealed: only FlowPayload and
	// AgentPayload satisfy AgentTurnPayload.
	Payload AgentTurnPayload
}

// AgentTurnPayload is the sealed content of an AgentTurn. Every kind has its
// own payload struct. Add a payload here AND in TestAgentTurnPayloadSealed.
type AgentTurnPayload interface {
	isAgentTurnPayload()
}

// FlowPayload is the payload of an AgentTurnKindFlow turn (a flow run).
type FlowPayload struct {
	// CurrentStepID is the step the flow is currently executing.
	CurrentStepID string
	// StepIDs is the ordered list of steps in this flow run (snapshot of
	// the flow definition's step order).
	StepIDs []string
}

func (FlowPayload) isAgentTurnPayload() {}

// AgentPayload is the payload of an AgentTurnKindAgent turn (a single agent
// LLM run, including a spawn_turn worker).
type AgentPayload struct {
	// FinalText is the agent's final response text. Empty until the turn
	// completes.
	FinalText string
	// ToolCallCount is the number of tool calls made during this turn.
	ToolCallCount int
}

func (AgentPayload) isAgentTurnPayload() {}
