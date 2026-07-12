package runtime

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/model/deepseek"
	fsession "github.com/xiramesh/xira/internal/session"
)

// resume_delivery_test.go: tests the resume→IM delivery path (RFC #27 —
// stateless HITL resume). When a resumed run produces a final, the runtime
// delivers it back to the originating channel via the injected OutboundEmitter
// (Manager.Emit), so the user actually receives the answer in IM.

// recordingEmitter records every Emit call for assertion.
type recordingEmitter struct {
	mu      sync.Mutex
	calls   []channel.OutboundEnvelope
	emitErr error
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
// run carries. #151: sender 的 canonical id 存在 Names["sender_id"]（不再在 Values）。
func scopeWith(channelName, chatID, senderID string) *fsession.SessionScope {
	return &fsession.SessionScope{
		Channel:      channelName,
		EntrypointID: "feishu-default",
		Account:      "acct-1",
		Values: map[string]string{
			"chat": "p2p:" + chatID,
		},
		Names: map[string]string{
			"sender_id": channelName + ":" + senderID, // canonical form
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

type attemptedErrorEmitter struct {
	recordingEmitter
	err error
}

func (e *attemptedErrorEmitter) Emit(ctx context.Context, env channel.OutboundEnvelope) error {
	e.recordingEmitter.Emit(ctx, env)
	return e.err
}

func TestDeliverResumeFinalWebsocketNoLiveConnectionIsBestEffort(t *testing.T) {
	emitter := &attemptedErrorEmitter{err: &simpleErr{"websocket Emit: no live connection for chat \"chat-9\""}}
	s := &Service{outbound: emitter}
	run := TurnResponse{
		RunID:         "ws-resume-1",
		FinalResponse: "resumed websocket final",
		Status:        "completed",
		SessionScope:  scopeWith("websocket", "chat-9", "alice"),
	}

	s.deliverResumeFinal(context.Background(), run)

	calls := emitter.emitted()
	if len(calls) != 1 {
		t.Fatalf("emitter got %d calls, want 1 attempt even when websocket has no live connection", len(calls))
	}
	env := calls[0]
	if env.Target == nil || env.Target.Channel != "websocket" || env.Target.ChatID != "chat-9" || env.Target.SenderID != "alice" {
		t.Fatalf("target = %+v, want websocket/chat-9/alice", env.Target)
	}
	if content, _ := env.Data["content"].(string); content != "resumed websocket final" {
		t.Fatalf("content = %q, want resumed websocket final", content)
	}
}

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

// TestSpawnedChildRunScopeSupportsResumeDelivery is the断裂 A end-to-end guard
// (issue #68). The existing tests above all hand-construct SessionScope
// (scopeWith). This test does NOT — it runs a REAL RunChildAgent, loads the
// persisted child run, and verifies the child's SessionScope is non-nil and
// faithfully routes a resumed final back to the PARENT's chat key.
//
// Without the断裂 A fix (RunChildAgent building resp.SessionScope), the loaded
// child run has a nil scope and deliverResumeFinal silently drops the final —
// a spawned child entering HITL could never answer back in IM. This test wires
// the whole chain: spawn → persist → load → resume-deliver, using the REAL
// canonical scope product (no hand-clean values, per AGENTS.md §5.4).
func TestSpawnedChildRunScopeSupportsResumeDelivery(t *testing.T) {
	stateRoot := t.TempDir()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return deepSeekHTTPResponse(deepSeekTextResponse("child final answer")), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       stateRoot,
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	target := agents.BuiltinResearchAssistant()
	target.Session = agents.SessionPolicy{Dimensions: []string{"chat", "sender"}}

	const (
		parentChannel = "feishu"
		parentChat    = "oc_child_resume_e2e"
		parentSender  = "ou_child_resume_e2e"
	)
	parentBase := runtimeEventBase{
		RunID:    "parent-resume-e2e",
		AgentID:  agents.BuiltinXiraAssistant().ID,
		Channel:  parentChannel,
		ChatID:   parentChat,
		ChatType: "direct",
		SenderID: parentSender,
	}
	if _, err := rt.RunChildAgent(context.Background(), childAgentRequest{
		ParentBase:  parentBase,
		ParentRunID: "parent-resume-e2e",
		ChildRunID:  "child-resume-e2e",
		Target:      target,
		Message:     "task",
		Depth:       1,
	}); err != nil {
		t.Fatalf("RunChildAgent error: %v", err)
	}

	// Load the REAL persisted child run — its scope came from RunChildAgent +
	// BuildScope, not a test helper.
	childRun, err := rt.RunStore().Load("child-resume-e2e")
	if err != nil {
		t.Fatalf("child run not recorded: %v", err)
	}
	if childRun.SessionScope == nil {
		t.Fatalf("断裂 A regression: child run has nil SessionScope, resume final cannot route")
	}

	// Simulate the resume path: the child was waiting_human, got resumed, and
	// now has a final to deliver. deliverResumeFinal must reconstruct the
	// PARENT's chat key from the child's scope and Emit.
	emitter := &recordingEmitter{}
	rt.SetOutboundEmitter(emitter)
	resumed := TurnResponse{
		RunID:         childRun.RunID,
		FinalResponse: "child resumed final",
		Status:        "completed",
		SessionScope:  childRun.SessionScope, // the REAL scope from RunChildAgent
	}
	rt.deliverResumeFinal(context.Background(), resumed)

	calls := emitter.emitted()
	if len(calls) != 1 {
		t.Fatalf("deliverResumeFinal emitted %d envelopes, want 1 (child final must route back to parent's IM)", len(calls))
	}
	env := calls[0]
	if env.Target.Channel != parentChannel {
		t.Errorf("resumed child final routed to channel %q, want parent's %q", env.Target.Channel, parentChannel)
	}
	if env.Target.ChatID != parentChat {
		t.Errorf("resumed child final routed to chat %q, want parent's %q", env.Target.ChatID, parentChat)
	}
	if env.Target.SenderID != parentSender {
		t.Errorf("resumed child final routed to sender %q, want parent's %q", env.Target.SenderID, parentSender)
	}
}
