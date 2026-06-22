package runtime

import "time"

// event_mapping.go: runtimeEventToEvent maps the ~14 signal-kind RuntimeEvents
// to typed Event structs (A2a #44). Non-signal kinds (observability/audit/
// internal, ~34 of them) return ok=false — they go to slog (temporary, #43
// strips them formally).
//
// The kind set here MUST stay in sync with message_bus.go's mapping table
// (the file-header old→new table) and with event_bus_evolution_test.go's
// TestRuntimeEventToEvent_*. Verify with the dual-path grep in message_bus.go.

// runtimeEventToEvent converts a RuntimeEvent to its typed Event equivalent.
// Returns ok=false for non-signal kinds (observability/audit/internal) — the
// caller routes those to slog (temporary until #43 strips them).
//
// Identity mapping:
//
//	RuntimeEvent.ID          → Event.MessageIDVal (→ ID() accessor)
//	RuntimeEvent.RunID       → Event.AgentTurnIDVal (→ AgentTurnID() accessor)
//	RuntimeEvent.Time        → Event.TimestampVal (→ Timestamp() accessor)
//	Correlation.ParentRunID  → Event.ParentAgentTurnIDVal (→ ParentAgentTurnID())
func runtimeEventToEvent(evt RuntimeEvent) (Event, bool) {
	base := eventIdentity{
		id:     evt.ID,
		turnID: AgentTurnID(evt.RunID),
		parent: parentRunID(evt),
		ts:     evt.Time,
	}

	switch evt.Kind {
	// --- turn lifecycle: started ---
	case "run.started", "agent.delegate.started":
		return AgentTurnStarted{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			TurnKind:             AgentTurnKindAgent,
		}, true

	// --- turn lifecycle: completed ---
	case "agent.delegate.completed":
		return AgentTurnCompleted{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			TurnKind:             AgentTurnKindAgent,
			Summary:              evt.Message,
		}, true

	// --- turn lifecycle: failed ---
	case "agent.delegate.failed", "agent.delegate.timeout":
		return AgentTurnFailed{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			TurnKind:             AgentTurnKindAgent,
			Error:                errorMessage(evt),
		}, true

	// --- run.finished: multi-status split ---
	case "run.finished":
		return mapRunFinished(evt, base)

	// --- HITL: requested ---
	case "run.waiting_human", "agent.delegate.waiting_human", "human.request.created":
		return HumanRequested{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			Question:             evt.Message,
		}, true

	// --- progress: assistant status / final ---
	case "assistant.status":
		return AssistantStatus{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			Text:                 evt.Message,
		}, true

	case "assistant.final":
		// AssistantFinal is not yet a distinct Event type (Phase 2 TODO).
		// Map to AssistantStatus with the final text — the forwarder's drain
		// logic (forwarder.go) recognizes assistant.final by Kind, and A2b
		// migration will carry that through SubscribeFiltered + rendering.
		// A dedicated AssistantFinal type will replace this when added.
		return AssistantStatus{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			Text:                 evt.Message,
		}, true

	// --- tool lifecycle ---
	case "tool.started":
		return ToolCalled{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			ToolName:             stringField(evt, "tool_name"),
		}, true

	case "tool.completed", "tool.finished":
		return ToolResult{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			ToolName:             stringField(evt, "tool_name"),
		}, true

	default:
		// Non-signal kind (observability/audit/internal) — caller routes to slog.
		return nil, false
	}
}

// mapRunFinished splits run.finished by payload["status"] into the
// corresponding terminal Event. status=timeout maps to AgentTurnFailed
// (Error="timeout") — there's no AgentTurnTimeout type; the turn's
// Status=timeout is the distinguishing state.
func mapRunFinished(evt RuntimeEvent, base eventIdentity) (Event, bool) {
	status := stringField(evt, "status")
	switch status {
	case "completed":
		return AgentTurnCompleted{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			TurnKind:             AgentTurnKindAgent,
			Summary:              evt.Message,
		}, true
	case "failed":
		return AgentTurnFailed{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			TurnKind:             AgentTurnKindAgent,
			Error:                errorMessage(evt),
		}, true
	case "canceled":
		return AgentTurnCanceled{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			TurnKind:             AgentTurnKindAgent,
			Reason:               evt.Message,
		}, true
	case "timeout":
		return AgentTurnFailed{
			MessageIDVal:         base.id,
			AgentTurnIDVal:       base.turnID,
			ParentAgentTurnIDVal: base.parent,
			TimestampVal:         base.ts,
			TurnKind:             AgentTurnKindAgent,
			Error:                "timeout",
		}, true
	default:
		// Unknown status — treat as non-signal (shouldn't happen, but safe).
		return nil, false
	}
}

// eventIdentity carries the common identity fields extracted from RuntimeEvent.
type eventIdentity struct {
	id     string
	turnID AgentTurnID
	parent AgentTurnID
	ts     time.Time
}

// parentRunID extracts the parent run ID from Correlation, if present.
func parentRunID(evt RuntimeEvent) AgentTurnID {
	if evt.Correlation != nil {
		return AgentTurnID(evt.Correlation.ParentRunID)
	}
	return ""
}

// errorMessage extracts an error message from the RuntimeEvent, preferring
// Payload["error"] then falling back to Message.
func errorMessage(evt RuntimeEvent) string {
	if e := stringField(evt, "error"); e != "" {
		return e
	}
	return evt.Message
}

// stringField extracts a string field from RuntimeEvent.Payload, returning ""
// if absent or not a string.
func stringField(evt RuntimeEvent, key string) string {
	if evt.Payload == nil {
		return ""
	}
	v, ok := evt.Payload[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
