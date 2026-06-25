package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	fsession "github.com/xiramesh/xira/internal/session"
)

// resume_delivery_test.go: tests the resume→IM delivery path (RFC #27 —
// stateless HITL resume). When a resumed run produces a final, the runtime
// delivers it back to the originating channel via the injected OutboundEmitter
// (Manager.Emit), so the user actually receives the answer in IM.

// recordingEmitter records every Emit call for assertion.
type recordingEmitter struct {
	mu       sync.Mutex
	calls    []channel.OutboundEnvelope
	emitErr  error
}

func (r *recordingEmitter) Capabilities() channel.CapabilitySet {
	return channel.CapabilitySet{channel.CapabilityProactiveOutbound}
}

func (r *recordingEmitter) Emit(_ context.Context, env channel.OutboundEnvelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.emitErr != nil {
		return r.emitErr
	}
	r.calls = append(r.calls, env)
	return nil
}

func (r *recordingEmitter) emitted() []channel.OutboundEnvelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]channel.OutboundEnvelope, len(r.calls))
	copy(out, r.calls)
	return out
}

// Compile-time: recordingEmitter satisfies channel.OutboundEmitter.
var _ channel.OutboundEmitter = (*recordingEmitter)(nil)

// scopeWith returns a SessionScope mimicking what a persisted waiting_human
// run carries. The sender is stored in its CANONICAL form ("<channel>:<id>"),
// exactly as session/manager.go canonicalSenderID produces — NOT a hand-clean
// id. This ensures deliverResumeFinal's reconstruction goes through the real
// de-prefixing path (PR #71 review CRITICAL: hand-clean senders hid the
// ilink prefix asymmetry).
func scopeWith(channelName, chatID, senderID string) *fsession.SessionScope {
	return &fsession.SessionScope{
		Channel:      channelName,
		EntrypointID: "feishu-default",
		Account:      "acct-1",
		Values: map[string]string{
			"chat":   "p2p:" + chatID,
			"sender": channelName + ":" + senderID, // canonical form (manager.go:165)
		},
	}
}

// TestDeliverResumeFinalSendsEnvelope verifies a completed resumed run's final
// is delivered to the originating channel via the outbound emitter, with the
// envelope carrying the reconstructed target (channel/chat/sender from scope).
func TestDeliverResumeFinalSendsEnvelope(t *testing.T) {
	emitter := &recordingEmitter{}
	s := &Service{outbound: emitter}
	run := TurnResponse{
		RunID:         "run-1",
		FinalResponse: "all done",
		Status:        "completed",
		SessionScope:  scopeWith("feishu", "chat-9", "user-1"),
	}

	s.deliverResumeFinal(context.Background(), run)

	calls := emitter.emitted()
	if len(calls) != 1 {
		t.Fatalf("emitter got %d calls, want 1", len(calls))
	}
	env := calls[0]
	if env.Type != channel.OutboundAssistantFinal {
		t.Errorf("envelope type = %q, want assistant_final", env.Type)
	}
	if env.RunID != "run-1" {
		t.Errorf("envelope RunID = %q, want run-1", env.RunID)
	}
	if env.Target == nil {
		t.Fatal("envelope has no target")
	}
	if env.Target.Channel != "feishu" {
		t.Errorf("target channel = %q, want feishu (reconstructed from scope)", env.Target.Channel)
	}
	if env.Target.ChatID != "chat-9" {
		t.Errorf("target chatID = %q, want chat-9", env.Target.ChatID)
	}
	if env.Target.SenderID != "user-1" {
		t.Errorf("target senderID = %q, want user-1", env.Target.SenderID)
	}
	if content, _ := env.Data["content"].(string); content != "all done" {
		t.Errorf("envelope content = %q, want 'all done'", content)
	}
}

// TestDeliverResumeFinalNilEmitterNoop verifies a nil outbound emitter is a
// safe no-op (backward-compatible: tests/CLI without a channel manager).
func TestDeliverResumeFinalNilEmitterNoop(t *testing.T) {
	s := &Service{} // outbound nil
	run := TurnResponse{FinalResponse: "x", Status: "completed", SessionScope: scopeWith("feishu", "c", "u")}
	// Must not panic.
	s.deliverResumeFinal(context.Background(), run)
}

// TestDeliverResumeFinalWaitingHumanSkipped verifies a run that resumed into
// ANOTHER waiting_human (multi-step HITL) is NOT delivered — there's no final
// for the user yet.
func TestDeliverResumeFinalWaitingHumanSkipped(t *testing.T) {
	emitter := &recordingEmitter{}
	s := &Service{outbound: emitter}
	run := TurnResponse{FinalResponse: "x", Status: StatusWaitingHuman, SessionScope: scopeWith("feishu", "c", "u")}
	s.deliverResumeFinal(context.Background(), run)
	if len(emitter.emitted()) != 0 {
		t.Errorf("waiting_human run should not be delivered; got %d calls", len(emitter.emitted()))
	}
}

// TestDeliverResumeFinalEmptyFinalSkipped verifies a run with no final
// (e.g. failed verification with empty draft) is not delivered (nothing to say).
func TestDeliverResumeFinalEmptyFinalSkipped(t *testing.T) {
	emitter := &recordingEmitter{}
	s := &Service{outbound: emitter}
	run := TurnResponse{FinalResponse: "   ", Status: "failed", SessionScope: scopeWith("feishu", "c", "u")}
	s.deliverResumeFinal(context.Background(), run)
	if len(emitter.emitted()) != 0 {
		t.Errorf("empty-final run should not be delivered; got %d calls", len(emitter.emitted()))
	}
}

// TestDeliverResumeFinalEmitErrorLoggedNotFatal verifies an Emit error does not
// propagate — the run already succeeded and is persisted; delivery failure is
// best-effort (logged), not a resume failure.
func TestDeliverResumeFinalEmitErrorLoggedNotFatal(t *testing.T) {
	emitter := &recordingEmitter{emitErr: &simpleErr{"send failed"}}
	s := &Service{outbound: emitter}
	run := TurnResponse{FinalResponse: "x", Status: "completed", SessionScope: scopeWith("feishu", "c", "u")}
	// Must not panic / not return error (deliverResumeFinal returns nothing).
	s.deliverResumeFinal(context.Background(), run)
}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

// TestDeliverResumeFinalNilScopeSkipped verifies a run with no persisted scope
// cannot be routed (no target channel) and is skipped, not panicked.
func TestDeliverResumeFinalNilScopeSkipped(t *testing.T) {
	emitter := &recordingEmitter{}
	s := &Service{outbound: emitter}
	run := TurnResponse{FinalResponse: "x", Status: "completed", SessionScope: nil}
	s.deliverResumeFinal(context.Background(), run)
	if len(emitter.emitted()) != 0 {
		t.Errorf("run with nil scope should not be delivered (no target); got %d calls", len(emitter.emitted()))
	}
}

// guard against unused import in case strings.Trim usage moves.
var _ = strings.TrimSpace
