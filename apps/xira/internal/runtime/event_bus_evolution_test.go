package runtime

import (
	"fmt"
	"testing"
	"time"
)

// These tests cover the runtimeEventToEvent mapping function (RuntimeEvent →
// sealed Event). The EventBus struct + interface were removed in Phase 6b (#60);
// only the mapping remains (used by dispatchEvent to deliver Events to the
// per-chat-key EventBus).
//   - Old EventBus struct renamed eventBusImpl, satisfies the interface
//   - Old Publish(RuntimeEvent) DELETED — compile-forces migration
//   - runtimeEventToEvent(RuntimeEvent) (Event, bool): maps ~14 signal kinds
//     to Event structs; non-signal kinds return ok=false (→ slog, stripped by #43)
//   - recordEvent closure splits: mapped → PublishEvent, unmapped → slog.Debug

// -----------------------------------------------------------------------------

func TestRuntimeEventToEvent_TurnLifecycle(t *testing.T) {
	now := time.Now()
	cases := []struct {
		kind     string
		wantType string // %T of expected Event
	}{
		{"run.started", "runtime.AgentTurnStarted"},
		{"agent.delegate.started", "runtime.AgentTurnStarted"},
		{"run.finished", ""}, // multi-status — tested separately below
		{"agent.delegate.completed", "runtime.AgentTurnCompleted"},
		{"agent.delegate.failed", "runtime.AgentTurnFailed"},
		{"agent.delegate.timeout", "runtime.AgentTurnFailed"},
		{"run.waiting_human", "runtime.HumanRequested"},
		{"agent.delegate.waiting_human", "runtime.HumanRequested"},
		{"human.request.created", "runtime.HumanRequested"},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			evt := RuntimeEvent{Kind: c.kind, ID: "e1", Time: now, RunID: "run_1"}
			got, ok := runtimeEventToEvent(evt)
			if c.kind == "run.finished" {
				// run.finished depends on payload status — tested in its own test
				return
			}
			if !ok {
				t.Fatalf("runtimeEventToEvent(%q) ok=false, want true (signal kind)", c.kind)
			}
			if gotType := typeName(got); gotType != c.wantType {
				t.Errorf("runtimeEventToEvent(%q) = %s, want %s", c.kind, gotType, c.wantType)
			}
		})
	}
}

func TestRuntimeEventToEvent_RunFinishedMultiStatus(t *testing.T) {
	// run.finished splits by payload["status"]: completed/failed/canceled/timeout
	now := time.Now()
	cases := []struct {
		status   string
		wantType string
	}{
		{"completed", "runtime.AgentTurnCompleted"},
		{"failed", "runtime.AgentTurnFailed"},
		{"canceled", "runtime.AgentTurnCanceled"},
		{"timeout", "runtime.AgentTurnFailed"},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			evt := RuntimeEvent{
				Kind:    "run.finished",
				ID:      "e1",
				Time:    now,
				RunID:   "run_1",
				Payload: map[string]any{"status": c.status},
			}
			got, ok := runtimeEventToEvent(evt)
			if !ok {
				t.Fatalf("ok=false for run.finished status=%s", c.status)
			}
			if gotType := typeName(got); gotType != c.wantType {
				t.Errorf("run.finished status=%s → %s, want %s", c.status, gotType, c.wantType)
			}
		})
	}
}

func TestRuntimeEventToEvent_ProgressAndTools(t *testing.T) {
	now := time.Now()
	cases := []struct {
		kind     string
		wantType string
	}{
		{"assistant.status", "runtime.AssistantStatus"},
		{"assistant.final", "runtime.AssistantFinal"}, // now a real type (PR #46 review)
		{"tool.started", "runtime.ToolCalled"},
		{"tool.completed", "runtime.ToolResult"},
		{"tool.finished", "runtime.ToolResult"},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			evt := RuntimeEvent{Kind: c.kind, ID: "e1", Time: now, RunID: "run_1"}
			got, ok := runtimeEventToEvent(evt)
			if !ok {
				t.Fatalf("ok=false for %s", c.kind)
			}
			if gotType := typeName(got); gotType != c.wantType {
				t.Errorf("%s → %s, want %s", c.kind, gotType, c.wantType)
			}
		})
	}
}

func TestRuntimeEventToEvent_NonSignalKindsReturnFalse(t *testing.T) {
	// These ~34 kinds are NOT signals — they're observability/audit/internal.
	// runtimeEventToEvent returns ok=false for them (→ slog, stripped by #43).
	nonSignal := []string{
		"llm.request_traced", "llm.usage_summary", "llm.call_recorded",
		"llm.raw_request_traced", "llm.raw_response_status_traced",
		"llm.raw_trace_failed", "llm.trace_failed",
		"context.packet.started", "context.packet.completed",
		"context.packet.failed", "context.packet.truncated",
		"context.item.included", "context.item.redacted",
		"session.persisted", "session.persist_failed",
		"usage.ledger_appended", "usage.ledger_failed",
		"model.policy_resolved", "model.request", "model.suspended",
		"adk.event", "adk.empty_final", "adk.intentional_silence", "adk.session_hydrated",
		"adk.session_hydrate_failed", "adk.suspended", "agent.silence_declared",
		"agent.delegate.allowed", "agent.delegate.requested",
		"agent.delegate.rejected", "agent.delegate.result_delivered",
		"tool.failed", "tool.raw_output_persisted", "tool.raw_output_failed",
		"capability_gap", "human.request.failed",
	}
	for _, kind := range nonSignal {
		t.Run(kind, func(t *testing.T) {
			evt := RuntimeEvent{Kind: kind, ID: "e1", RunID: "run_1"}
			_, ok := runtimeEventToEvent(evt)
			if ok {
				t.Errorf("runtimeEventToEvent(%q) ok=true, want false (non-signal → slog/#43)", kind)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Identity fields carried through mapping
// -----------------------------------------------------------------------------

func TestRuntimeEventToEvent_CarriesIdentity(t *testing.T) {
	now := time.Now()
	evt := RuntimeEvent{
		Kind:  "agent.delegate.started",
		ID:    "evt_abc",
		Time:  now,
		RunID: "run_child",
		Scope: &RuntimeEventScope{
			MessageID: "msg_1",
		},
		Correlation: &RuntimeEventCorrelation{
			ParentRunID: "run_parent",
		},
	}
	got, ok := runtimeEventToEvent(evt)
	if !ok {
		t.Fatal("ok=false for signal kind")
	}
	if got.ID() != "evt_abc" {
		t.Errorf("ID() = %q, want evt_abc", got.ID())
	}
	if !got.Timestamp().Equal(now) {
		t.Errorf("Timestamp() mismatch")
	}
}

func TestRuntimeEventToEvent_RunFinishedUnknownStatusWarns(t *testing.T) {
	// 🟡 2 (PR #46 review): mapRunFinished default must not silently drop.
	// Unknown status → ok=false + slog.Warn (not silent, not faking failed).
	evt := RuntimeEvent{
		Kind:    "run.finished",
		ID:      "e1",
		RunID:   "run_1",
		Payload: map[string]any{"status": "bogus_status"},
	}
	_, ok := runtimeEventToEvent(evt)
	if ok {
		t.Error("unknown status should return ok=false, not a fake event")
	}
}

func TestRuntimeEventToEvent_AssistantFinalFinalChars(t *testing.T) {
	// AssistantFinal carries FinalChars from payload.
	evt := RuntimeEvent{
		Kind:    "assistant.final",
		ID:      "e1",
		RunID:   "run_1",
		Payload: map[string]any{"final_chars": 42},
	}
	got, ok := runtimeEventToEvent(evt)
	if !ok {
		t.Fatal("ok=false for assistant.final")
	}
	af, isFinal := got.(AssistantFinal)
	if !isFinal {
		t.Fatalf("got %T, want AssistantFinal", got)
	}
	if af.FinalChars != 42 {
		t.Errorf("FinalChars = %d, want 42", af.FinalChars)
	}
}

func TestIntFieldVariants(t *testing.T) {
	// intField handles int, int64, float64, missing, wrong-type.
	cases := []struct {
		name    string
		payload map[string]any
		want    int
	}{
		{"int", map[string]any{"n": 7}, 7},
		{"int64", map[string]any{"n": int64(8)}, 8},
		{"float64", map[string]any{"n": float64(9.7)}, 9},
		{"missing", map[string]any{}, 0},
		{"wrong_type", map[string]any{"n": "not a number"}, 0},
		{"nil_payload", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evt := RuntimeEvent{Payload: c.payload}
			if got := intField(evt, "n"); got != c.want {
				t.Errorf("intField(%s) = %d, want %d", c.name, got, c.want)
			}
		})
	}
}

func TestStringFieldVariants(t *testing.T) {
	// stringField edge cases for coverage.
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"present", map[string]any{"s": "hi"}, "hi"},
		{"missing", map[string]any{}, ""},
		{"wrong_type", map[string]any{"s": 123}, ""},
		{"nil_payload", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evt := RuntimeEvent{Payload: c.payload}
			if got := stringField(evt, "s"); got != c.want {
				t.Errorf("stringField(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// helper
// -----------------------------------------------------------------------------

func typeName(e Event) string {
	return fmt.Sprintf("%T", e)
}
