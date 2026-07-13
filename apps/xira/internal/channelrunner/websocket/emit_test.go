package websocket

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
	frt "github.com/xiramesh/xira/internal/runtime"
)

// emit_test.go: tests for websocket's connection-addressing registry +
// outbound Emit (RFC chatkey-session Step 3b — resume-to-live-connection).
//
// Before this step, websocket's Emit was a no-op (logged a warning, returned
// nil) and *Runner did NOT implement channel.OutboundEmitter (missing
// Capabilities()). Manager.Emit therefore never reached websocket's Emit. This
// file pins the new contract: a per-Runner connection registry keyed by ChatKey,
// Emit that finds a live connection and writes a response frame, and the
// OutboundEmitter compile-time assertion.

// Compile-time: *Runner implements channel.OutboundEmitter. This line fails to
// compile until Capabilities() is added — that is the red signal for Step 3.
var _ channel.OutboundEmitter = (*Runner)(nil)

// keyOf builds the ChatKey the registry uses, mirroring handleMessage
// (runner.go:334) which derives the key from the eventContext.
func keyOf(chatID, senderID string) frt.ChatKey {
	return frt.ChatKeyFromInbound(channel.InboundContext{
		Channel:  "websocket",
		ChatID:   chatID,
		SenderID: senderID,
	})
}

// registerOneKey is a test shorthand: build a wsConn (newConn) and register
// one ChatKey under it. Returns the handle for disconnect-cleanup tests
// (releaseConn). Under the single-connection contract this assumes no live
// prior owner (fresh key) — the test setup ensures that.
func registerOneKey(runner *Runner, chatID, senderID string, send func(outboundFrame) error, cancel context.CancelFunc) *wsConn {
	conn := runner.newConn(send, cancel)
	_, _ = runner.registerConnKey(conn, keyOf(chatID, senderID))
	return conn
}

// TestEmitDeliversToRegisteredConnection verifies Emit routes a final to the
// connection registered under the envelope's Target ChatKey. This is the core
// resume-delivery guarantee: a resumed run's final reaches the live WS client.
func TestEmitDeliversToRegisteredConnection(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := &capturedFrames{}

	_, cancel := context.WithCancel(context.Background())
	registerOneKey(runner, "chat-1", "alice", caps.write, cancel)

	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.RunID = "run-1"
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "chat-1", SenderID: "alice"}
	env.Data = map[string]any{"content": "final answer"}

	if err := runner.Emit(context.Background(), env); err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}
	frames := caps.snapshot()
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].Type != "response" {
		t.Errorf("frame type = %q, want %q", frames[0].Type, "response")
	}
	data, _ := frames[0].Data.(map[string]any)
	// Schema MUST match responseFrame (runner.go:786) so a WS client reading
	// resumed finals via the same handler as normal turn responses sees the
	// final text. PR #97 review CRITICAL #1: a bespoke "content" field broke
	// this — clients reading "final_response" got empty.
	finalResponse, _ := data["final_response"].(string)
	if finalResponse != "final answer" {
		t.Errorf("frame final_response = %q, want %q (must match responseFrame schema)", finalResponse, "final answer")
	}
	if got, _ := data["content_format"].(string); got != "markdown" {
		t.Errorf("frame content_format = %q, want %q", got, "markdown")
	}
	if got, _ := data["run_id"].(string); got != "run-1" {
		t.Errorf("frame run_id = %q, want %q", got, "run-1")
	}
	if got, _ := data["status"].(string); got != "completed" {
		t.Errorf("frame status = %q, want %q", got, "completed")
	}
	if _, hasContent := data["content"]; hasContent {
		t.Errorf("frame must NOT carry bespoke 'content' field (use final_response); got data=%v", data)
	}
}

func TestEmitRejectsTypedRecipientWithoutLeakingToTriggerConnection(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	frames := &capturedFrames{}
	_, cancel := context.WithCancel(context.Background())
	registerOneKey(runner, "group-1", "non-owner", frames.write, cancel)

	env := channel.NewOutboundEnvelope(channel.OutboundProactiveMessage)
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "group-1", SenderID: "non-owner"}
	env.Recipient = &channel.OutboundRecipient{ID: "owner", IDType: "open_id"}
	env.Data = map[string]any{"content": "owner secret"}

	err := runner.Emit(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "typed recipient") {
		t.Fatalf("Emit typed recipient error = %v, want fail-closed rejection", err)
	}
	if got := frames.snapshot(); len(got) != 0 {
		t.Fatalf("trigger connection received %d private frames, want 0", len(got))
	}
}

// TestEmitNoLiveConnectionReturnsError verifies Emit does NOT silently drop
// when no connection is registered. Resume delivery is best-effort, but a
// silent nil would hide the gap; an error lets the resume path log it
// (human_request_resume.go:101-107). This replaces the old no-op Emit.
func TestEmitNoLiveConnectionReturnsError(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())

	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "ghost", SenderID: "nobody"}
	env.Data = map[string]any{"content": "x"}

	err := runner.Emit(context.Background(), env)
	if err == nil {
		t.Fatal("Emit with no live connection should return error, not nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "no live connection") {
		t.Errorf("error %q should mention 'no live connection'", err)
	}
}

// TestEmitUnregisteredOnConnClose verifies that when a connection's ctx is
// cancelled (client disconnected), its registry entry is removed so Emit no
// longer targets a dead connection. Without this, Emit could hand a frame to a
// writeFrame closure whose conn is gone — the fail-fast path swallows it
// silently (silent data loss, AGENTS.md §2).
func TestEmitUnregisteredOnConnClose(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := &capturedFrames{}

	_, cancel := context.WithCancel(context.Background())
	conn := registerOneKey(runner, "chat-2", "bob", caps.write, cancel)

	// Simulate disconnect: the connection's HandleConnection returns and
	// releases all its keys (the defer in HandleConnection).
	runner.releaseConn(conn)

	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "chat-2", SenderID: "bob"}
	env.Data = map[string]any{"content": "late"}

	err := runner.Emit(context.Background(), env)
	if err == nil {
		t.Fatal("Emit after disconnect should error (no live connection)")
	}
	if got := caps.snapshot(); len(got) != 0 {
		t.Errorf("dead connection received %d frames, want 0", len(got))
	}
}

// TestEmitRejectsMalformedEnvelope pins the input-validation guards so Emit
// fails loudly on missing Target/content rather than panicking or dropping.
func TestEmitRejectsMalformedEnvelope(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	registerOneKey(runner, "chat-4", "dave", func(outboundFrame) error { return nil }, func() {})

	tests := []struct {
		name string
		env  channel.OutboundEnvelope
		want string
	}{
		{
			name: "nil target",
			env:  channel.NewOutboundEnvelope(channel.OutboundAssistantFinal),
			want: "no target",
		},
		{
			name: "empty chat id",
			env: func() channel.OutboundEnvelope {
				e := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
				e.Target = &channel.InboundContext{Channel: "websocket", SenderID: "dave"}
				return e
			}(),
			want: "no chat_id",
		},
		{
			name: "empty content",
			env: func() channel.OutboundEnvelope {
				e := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
				e.Target = &channel.InboundContext{Channel: "websocket", ChatID: "chat-4", SenderID: "dave"}
				return e
			}(),
			want: "no content",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := runner.Emit(context.Background(), tc.env)
			if err == nil {
				t.Fatalf("Emit %s: expected error, got nil", tc.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Errorf("Emit %s: error %q should contain %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestCapabilitiesAdvertisesProactiveOutbound verifies websocket advertises
// proactive outbound and structured current-sender human responses, but never
// typed-recipient delivery (WebSocket has no trusted platform identity).
func TestCapabilitiesAdvertisesProactiveOutbound(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := runner.Capabilities()
	if len(caps) == 0 {
		t.Fatal("Capabilities returned empty set")
	}
	has := map[channel.Capability]bool{}
	for _, c := range caps {
		has[c] = true
	}
	if !has[channel.CapabilityProactiveOutbound] {
		t.Error("missing CapabilityProactiveOutbound")
	}
	if has[channel.CapabilityTypedRecipientOutbound] {
		t.Error("websocket must not advertise typed recipient outbound")
	}
	if !has[channel.CapabilityInteractiveHumanResponse] {
		t.Error("missing CapabilityInteractiveHumanResponse")
	}
}

// TestEmitUnsupportedTypeErrors covers the switch default branch: only
// assistant_final / outbound_message are deliverable; any other type is a
// caller contract violation and must error (not silently drop).
func TestEmitUnsupportedTypeErrors(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := &capturedFrames{}
	_, cancel := context.WithCancel(context.Background())
	registerOneKey(runner, "chat-x", "x", caps.write, cancel)

	env := channel.NewOutboundEnvelope(channel.OutboundRuntimeEvent) // not a deliverable type
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "chat-x", SenderID: "x"}
	env.Data = map[string]any{"content": "x"}
	if err := runner.Emit(context.Background(), env); err == nil {
		t.Fatal("Emit with unsupported type should error")
	}
	if got := caps.snapshot(); len(got) != 0 {
		t.Errorf("unsupported type emitted %d frames, want 0", len(got))
	}
}

// TestHandleMessageRegistersConnectionForEmit is the wiring test: it drives
// handleMessage with a REAL onRegister callback (not noop) and verifies the
// connection lands in the registry and is reachable via Emit. This closes the
// loop HandleConnection → handleMessage → onRegister → registerConn → Emit,
// minus the real *websocket.Conn (writeFrame is injected as capturedFrames).
// Without this, the unit tests above prove registerConn/Emit in isolation but
// not that HandleConnection actually wires them together.
func TestHandleMessageRegistersConnectionForEmit(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := &capturedFrames{}
	addActive := func(*activeRequest) {}
	removeActive := func(string) {}

	// onRegister mirrors HandleConnection's real wiring: register the
	// connection's handle under the message's chat key. The handle is built
	// once (newConn), like HandleConnection does, and reused per message.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsHandle := runner.newConn(caps.write, cancel)
	onRegister := func(key frt.ChatKey) (*wsConn, bool) {
		return runner.registerConnKey(wsHandle, key)
	}

	runner.handleMessage(ctx, makeMessageFrame("om_1", "chat-9", "eve", "hi"),
		"websocket-default", caps.write, addActive, removeActive, onRegister)

	// Registration happens synchronously inside handleMessage before
	// session.Handle returns control, so the registry is populated by the
	// time handleMessage returns.
	if got := runner.lookupConn(keyOf("chat-9", "eve")); got == nil {
		t.Fatal("connection not in registry after handleMessage")
	}

	// Emit must reach the registered connection.
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "chat-9", SenderID: "eve"}
	env.Data = map[string]any{"content": "delivered"}
	if err := runner.Emit(context.Background(), env); err != nil {
		t.Fatalf("Emit after handleMessage: %v", err)
	}

	// Now simulate disconnect: HandleConnection's defer calls releaseConn,
	// removing every key the handle still owns. Verify the entry is gone.
	runner.releaseConn(wsHandle)
	if got := runner.lookupConn(keyOf("chat-9", "eve")); got != nil {
		t.Error("connection still in registry after releaseConn on disconnect")
	}

	// Emit after disconnect must error (no live connection).
	if err := runner.Emit(context.Background(), env); err == nil {
		t.Error("Emit after disconnect should error")
	}
}

// TestRegisterConnKeyAndReleaseConnNilSafe covers the defensive nil guards.
// registerConnKey(nil, ...) returns nil (no panic); releaseConn(nil) is a
// no-op. These guards keep the registry safe if a caller ever passes a nil
// handle (e.g. a future code path that conditionally constructs a wsConn).
func TestRegisterConnKeyAndReleaseConnNilSafe(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	if displaced, rejected := runner.registerConnKey(nil, keyOf("chat-z", "z")); displaced != nil || rejected {
		t.Errorf("registerConnKey(nil,...) = (%v, %v), want (nil, false)", displaced, rejected)
	}
	// Must not panic:
	runner.releaseConn(nil)
	if runner.lookupConn(keyOf("chat-z", "z")) != nil {
		t.Error("nil conn should not have registered anything")
	}
}

// TestRegisterConnKeyIdempotentSameKey verifies re-registering the SAME
// connection under an already-owned key is a no-op (displaced nil, not
// rejected). This is the path a connection hits when consecutive message frames
// carry the same chat_id/sender_id.
func TestRegisterConnKeyIdempotentSameKey(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := &capturedFrames{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsHandle := runner.newConn(caps.write, cancel)

	if displaced, rejected := runner.registerConnKey(wsHandle, keyOf("chat-i", "ian")); displaced != nil || rejected {
		t.Fatalf("first register = (%v, %v), want (nil, false)", displaced, rejected)
	}
	// Re-register the SAME handle under the SAME key — must be a no-op.
	if displaced, rejected := runner.registerConnKey(wsHandle, keyOf("chat-i", "ian")); displaced != nil || rejected {
		t.Errorf("idempotent re-register = (%v, %v), want (nil, false)", displaced, rejected)
	}
	if ctx.Err() != nil {
		t.Fatal("connection was cancelled by idempotent re-register")
	}
}

// TestEmitMixedCaseChatKeyRoundtrip locks the fix for PR #97 re-review WARNING
// #1. inbound registration stores the ChatKey with the ORIGINAL case the client
// sent (e.g. "RoomA"/"UserA"), but the resume path reconstructs the Target from
// SessionScope, where BuildScope lowercases chat/sender (session/manager.go).
// Without canonicalization, Emit's lookup (lowercase target) misses the
// registry entry (mixed-case key) → "no live connection".
//
// Fix: registry keys are canonicalized (lowercased chat/sender) at every entry
// point, so inbound (mixed-case) and resume (lowercase) resolve to the same key.
func TestEmitMixedCaseChatKeyRoundtrip(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := &capturedFrames{}
	_, cancel := context.WithCancel(context.Background())
	registerOneKey(runner, "RoomA", "UserA", caps.write, cancel) // client sent mixed case

	// Resume reconstructs Target from SessionScope, which lowercases. Simulate
	// that by looking up with the lowercased identity.
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "rooma", SenderID: "usera"}
	env.Data = map[string]any{"content": "found"}
	if err := runner.Emit(context.Background(), env); err != nil {
		t.Fatalf("Emit with lowercased target should reach the mixed-case-registered connection: %v", err)
	}
	if got := caps.snapshot(); len(got) != 1 {
		t.Errorf("mixed-case roundtrip: got %d frames, want 1 (canonicalization mismatch)", len(got))
	}
}

// TestSecondLiveConnectionRejected locks the single-connection contract (PR #97
// round-7): when a ChatKey already has a LIVE owner, a second connection's
// registerConnKey must return rejected=true, and the registry stays with the
// original owner. This is what eliminates the multi-connection takeover cascade
// that drove rounds 2-7.
func TestSecondLiveConnectionRejected(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	capsA := &capturedFrames{}
	capsB := &capturedFrames{}
	_, cancelA := context.WithCancel(context.Background())
	_, cancelB := context.WithCancel(context.Background())
	connA := runner.newConn(capsA.write, cancelA)
	connB := runner.newConn(capsB.write, cancelB)
	key := keyOf("chat-solo", "u")

	// First connection registers (fresh key) — succeeds.
	if _, rejected := runner.registerConnKey(connA, key); rejected {
		t.Fatal("first register on a fresh key should not be rejected")
	}
	// Second LIVE connection → must be rejected; registry stays with connA.
	if _, rejected := runner.registerConnKey(connB, key); !rejected {
		t.Fatal("second live connection should be rejected (single-connection contract)")
	}
	if got := runner.lookupConn(key); got != connA {
		t.Errorf("registry = %p, want connA %p (rejected takeover must not change owner)", got, connA)
	}
}

// TestStaleConnectionReplaced solves the "dead connection traps the chat" risk
// of reject-new: if the prior owner hasn't been seen for > staleThreshold (its
// socket died unnoticed), a new connection may take over. This is what makes
// reject-new survivable for reconnects (PR #97 round-7).
func TestStaleConnectionReplaced(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	capsA := &capturedFrames{}
	capsB := &capturedFrames{}
	_, cancelA := context.WithCancel(context.Background())
	_, cancelB := context.WithCancel(context.Background())
	connA := runner.newConn(capsA.write, cancelA)
	connB := runner.newConn(capsB.write, cancelB)
	key := keyOf("chat-stale", "u")

	// connA registers first.
	runner.registerConnKey(connA, key)
	// Force connA's lastSeen into the past (simulate a dead socket: no frames
	// for longer than staleThreshold).
	runner.connMu.Lock()
	connA.lastSeen = time.Now().Add(-staleThreshold - time.Second)
	runner.connMu.Unlock()

	// connB (new connection) takes over the stale owner — NOT rejected.
	if _, rejected := runner.registerConnKey(connB, key); rejected {
		t.Fatal("new connection should take over a STALE owner, not be rejected")
	}
	if got := runner.lookupConn(key); got != connB {
		t.Errorf("registry = %p after stale takeover, want connB %p", got, connB)
	}
	// Emit now reaches the new owner.
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "chat-stale", SenderID: "u"}
	env.Data = map[string]any{"content": "to-new-owner"}
	if err := runner.Emit(context.Background(), env); err != nil {
		t.Fatalf("Emit after stale takeover: %v", err)
	}
	if got := capsB.snapshot(); len(got) != 1 {
		t.Errorf("new owner got %d frames, want 1", len(got))
	}
	if got := capsA.snapshot(); len(got) != 0 {
		t.Errorf("stale owner got %d frames, want 0", len(got))
	}
}

// TestStaleConnectionWithActiveTurnRejected locks the round-8 WARNING fix: a
// stale owner (lastSeen expired) is only replaced when NO turn is active. If a
// turn is still running on the stale owner's chatKey, the takeover is rejected —
// the turn keeps running on its own ctx, and registry stays put until the turn
// completes (avoiding the "old turn alive, registry moved" boundary).
func TestStaleConnectionWithActiveTurnRejected(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	capsA := &capturedFrames{}
	capsB := &capturedFrames{}
	_, cancelA := context.WithCancel(context.Background())
	_, cancelB := context.WithCancel(context.Background())
	connA := runner.newConn(capsA.write, cancelA)
	connB := runner.newConn(capsB.write, cancelB)
	key := keyOf("chat-stale-active", "u")

	// connA registers + starts a turn that stays active (onNewTurn blocks).
	runner.registerConnKey(connA, key)
	block := make(chan struct{})
	runner.router.Route(key, "req-active", "msg", context.Background(),
		func(frt.ChatKey, string, context.Context) { <-block })
	if !runner.router.IsActive(key) {
		t.Fatal("turn should be active")
	}

	// Force connA stale (lastSeen expired), but the turn is STILL active.
	runner.connMu.Lock()
	connA.lastSeen = time.Now().Add(-staleThreshold - time.Second)
	runner.connMu.Unlock()

	// connB must be REJECTED: stale owner but active turn → no takeover.
	if _, rejected := runner.registerConnKey(connB, key); !rejected {
		t.Fatal("new connection should be rejected when stale owner still has an active turn")
	}
	if got := runner.lookupConn(key); got != connA {
		t.Errorf("registry = %p, want connA (stale-but-active owner must not be displaced)", got)
	}

	close(block) // let the active turn finish (releases the goroutine)
}

// TestHandleMessageRejectsSecondConnection is the end-to-end single-connection
// test: connA owns chat-solo; connB sends a message for the same chat and must
// receive a "rejected" ack (not accepted/steered), with no turn started.
func TestHandleMessageRejectsSecondConnection(t *testing.T) {
	rt := newFakeRuntime()
	runner := newTestRunner(t, rt)
	capsA := &capturedFrames{}
	capsB := &capturedFrames{}
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	connA := runner.newConn(capsA.write, cancelA)
	connB := runner.newConn(capsB.write, cancelB)

	mkOnRegister := func(h *wsConn) func(frt.ChatKey) (*wsConn, bool) {
		return func(key frt.ChatKey) (*wsConn, bool) { return runner.registerConnKey(h, key) }
	}
	addActive := func(*activeRequest) {}
	removeActive := func(string) {}

	// connA sends first → registered + turn starts.
	runner.handleMessage(ctxA, makeMessageFrame("om_A", "chat-solo", "u", "first"),
		"websocket-default", capsA.write, addActive, removeActive, mkOnRegister(connA))

	// connB sends for the SAME chat while connA is live → must be rejected.
	runner.handleMessage(ctxB, makeMessageFrame("om_B", "chat-solo", "u", "second"),
		"websocket-default", capsB.write, addActive, removeActive, mkOnRegister(connB))

	bFrames := capsB.snapshot()
	hasRejected := false
	for _, f := range bFrames {
		if f.Type == "ack" {
			if d, _ := f.Data.(map[string]any); d != nil {
				if s, _ := d["status"].(string); s == "rejected" {
					hasRejected = true
				}
			}
		}
	}
	if !hasRejected {
		t.Errorf("connB frames %v: no rejected ack (single-connection contract)", typesOf(bFrames))
	}
	// Registry must still be connA.
	if got := runner.lookupConn(keyOf("chat-solo", "u")); got != connA {
		t.Errorf("registry = %p after rejected connB, want connA", got)
	}
	_ = sync.Mutex{} // keep sync import if unused after edits
}

// TestRunnerMetadata covers the trivial Runner accessors (ID/Channel/Start/Stop)
// and the OutboundEmitter compile-time assertion. These are simple but were 0%
// because no test instantiated a full Runner and called them.
func TestRunnerMetadata(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	if runner.ID() != "websocket-default" {
		t.Errorf("ID() = %q, want websocket-default", runner.ID())
	}
	if runner.Channel() != "websocket" {
		t.Errorf("Channel() = %q, want websocket", runner.Channel())
	}
	if err := runner.Start(context.Background()); err != nil {
		t.Errorf("Start() error: %v", err)
	}
	if err := runner.Stop(context.Background()); err != nil {
		t.Errorf("Stop() error: %v", err)
	}
}

// TestHandleMessageDuplicateAcks covers the dedupe duplicate branch: a retry
// of the same message_id gets a "duplicate" ack (not accepted/steered) and does
// not start a second turn.
func TestHandleMessageDuplicateAcks(t *testing.T) {
	rt := newFakeRuntime()
	runner := newTestRunner(t, rt)
	caps := &capturedFrames{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsHandle := runner.newConn(caps.write, func() {})
	onReg := func(key frt.ChatKey) (*wsConn, bool) { return runner.registerConnKey(wsHandle, key) }

	// First send: starts a turn.
	runner.handleMessage(ctx, makeMessageFrame("om_dup", "chat-dup", "u", "first"),
		"websocket-default", caps.write, func(*activeRequest) {}, func(string) {}, onReg)
	time.Sleep(20 * time.Millisecond)
	// Second send of SAME message_id: must be duplicate.
	runner.handleMessage(ctx, makeMessageFrame("om_dup", "chat-dup", "u", "retry"),
		"websocket-default", caps.write, func(*activeRequest) {}, func(string) {}, onReg)

	gotDup := false
	for _, f := range caps.snapshot() {
		if f.Type == "ack" {
			if d, _ := f.Data.(map[string]any); d != nil {
				if s, _ := d["status"].(string); s == "duplicate" {
					gotDup = true
				}
			}
		}
	}
	if !gotDup {
		t.Errorf("no duplicate ack; got %v", typesOf(caps.snapshot()))
	}
}

// TestHandleMessageAckFailureForgetsDedupe covers the started-ack-failure
// branch: if the accepted ack write fails, dedupe is forgotten (retriable) and
// the turn does not start.
func TestHandleMessageAckFailureForgetsDedupe(t *testing.T) {
	rt := newFakeRuntime()
	calls := int32(0)
	rt.respond = func(frt.TurnRequest) (frt.TurnResponse, error) {
		atomic.AddInt32(&calls, 1)
		return frt.TurnResponse{RunID: "r", Status: "completed", FinalResponse: "ok"}, nil
	}
	runner := newTestRunner(t, rt)
	failingWrite := func(outboundFrame) error { return errWriteAckFail }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsHandle := runner.newConn(failingWrite, func() {})
	onReg := func(key frt.ChatKey) (*wsConn, bool) { return runner.registerConnKey(wsHandle, key) }

	runner.handleMessage(ctx, makeMessageFrame("om_af", "chat-af", "u", "hi"),
		"websocket-default", failingWrite, func(*activeRequest) {}, func(string) {}, onReg)
	time.Sleep(20 * time.Millisecond)

	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("RunAgent called %d times, want 0 (turn must not start when ack fails)", calls)
	}
	// dedupe must be forgotten so om_af is retriable.
	dk := dedupeKey("websocket-default", "om_af")
	if !runner.dedupe.Begin(dk, time.Now()) {
		t.Error("dedupe key still processing after ack-failure rollback (must be forgotten for retry)")
	}
}

// errWriteAckFail is a sentinel for ack-failure tests.
var errWriteAckFail = errAckWrite{}

type errAckWrite struct{}

func (errAckWrite) Error() string { return "simulated ack write failure" }

// TestHandleMessageBadJSON covers the bad_json error branch + badJSONError.Error.
func TestHandleMessageBadJSON(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := &capturedFrames{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsHandle := runner.newConn(caps.write, func() {})
	onReg := func(key frt.ChatKey) (*wsConn, bool) { return runner.registerConnKey(wsHandle, key) }

	runner.handleMessage(ctx, inboundFrame{Type: "message", ID: "om_bad", Data: []byte("{not json")},
		"websocket-default", caps.write, func(*activeRequest) {}, func(string) {}, onReg)

	hasBadJSON := false
	for _, f := range caps.snapshot() {
		if f.Type == "error" {
			if d, _ := f.Data.(map[string]any); d != nil {
				if s, _ := d["code"].(string); s == "bad_json" {
					hasBadJSON = true
				}
			}
		}
	}
	if !hasBadJSON {
		t.Errorf("no bad_json error frame; got %v", typesOf(caps.snapshot()))
	}
}
