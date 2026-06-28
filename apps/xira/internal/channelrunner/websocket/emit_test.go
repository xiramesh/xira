package websocket

import (
	"context"
	"strings"
	"testing"

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

// registerOneKey is a test shorthand: build a wsConn (newConn), register one
// ChatKey under it, cancel any displaced connection. Returns the handle for
// disconnect-cleanup tests (releaseConn). Mirrors what HandleConnection does.
func registerOneKey(runner *Runner, chatID, senderID string, send func(outboundFrame) error, cancel context.CancelFunc) *wsConn {
	conn := runner.newConn(send, cancel)
	if displaced := runner.registerConnKey(conn, keyOf(chatID, senderID)); displaced != nil && displaced.cancel != nil {
		displaced.cancel()
	}
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

// TestNewConnectionReplacesOldForSameChatKey verifies the one-active-connection-
// per-ChatKey contract: when a second connection registers under the same key,
// the first is cancelled (its client is told to back off / reconnect) and only
// the new one remains. Two live connections sharing a ChatKey would race for the
// same Router entry — a per-chatKey single-active-turn contract violation.
func TestNewConnectionReplacesOldForSameChatKey(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())

	oldCtx, oldCancel := context.WithCancel(context.Background())
	defer oldCancel() // safety: ensure cleanup if test fails before replacement
	registerOneKey(runner, "chat-3", "carol", func(outboundFrame) error { return nil }, oldCancel)

	newCaps := &capturedFrames{}
	_, newCancel := context.WithCancel(context.Background())
	defer newCancel()
	registerOneKey(runner, "chat-3", "carol", newCaps.write, newCancel)

	// Old connection's cancel must have fired: registerOneKey cancels the
	// displaced connection returned by registerConnKey.
	if oldCtx.Err() == nil {
		t.Fatal("old connection was not cancelled when replaced")
	}

	// Only the new connection should receive Emit.
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "chat-3", SenderID: "carol"}
	env.Data = map[string]any{"content": "to-new"}
	if err := runner.Emit(context.Background(), env); err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}
	if got := newCaps.snapshot(); len(got) != 1 {
		t.Errorf("new connection got %d frames, want 1", len(got))
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
// ONLY proactive outbound — not interactive human response, whose inbound frame
// (human_response) is still rejected (see HandleConnection). Advertising an
// unimplemented capability would be a lie that callers (resume) might act on.
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
	if has[channel.CapabilityInteractiveHumanResponse] {
		t.Error("should NOT advertise CapabilityInteractiveHumanResponse (human_response frame not yet wired)")
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
	onRegister := func(key frt.ChatKey) {
		if displaced := runner.registerConnKey(wsHandle, key); displaced != nil && displaced.cancel != nil {
			displaced.cancel()
		}
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

// TestReleaseConnDoesNotEvictNewOwner verifies the ownership guard: when a
// newer connection took over a key, the OLD connection's disconnect cleanup
// (releaseConn) must NOT evict the new owner. releaseConn removes only keys
// whose current mapping still points at the disconnecting connection.
func TestReleaseConnDoesNotEvictNewOwner(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())

	oldCaps := &capturedFrames{}
	newCaps := &capturedFrames{}
	_, oldCancel := context.WithCancel(context.Background())
	_, newCancel := context.WithCancel(context.Background())

	oldHandle := runner.newConn(oldCaps.write, oldCancel)
	runner.registerConnKey(oldHandle, keyOf("chat-A", "zoe"))
	// New connection takes over the same key.
	newHandle := runner.newConn(newCaps.write, newCancel)
	displaced := runner.registerConnKey(newHandle, keyOf("chat-A", "zoe"))
	if displaced != oldHandle {
		t.Fatalf("registerConnKey should have returned the displaced old handle")
	}

	// Old connection now disconnects — its defer calls releaseConn. Must NOT
	// evict the new owner, because key chat-A no longer maps to oldHandle.
	runner.releaseConn(oldHandle)
	if got := runner.lookupConn(keyOf("chat-A", "zoe")); got == nil {
		t.Fatal("new owner evicted by old connection's disconnect cleanup")
	}

	// Emit must reach the NEW connection, not error.
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "chat-A", SenderID: "zoe"}
	env.Data = map[string]any{"content": "to-new"}
	if err := runner.Emit(context.Background(), env); err != nil {
		t.Fatalf("Emit should reach new owner: %v", err)
	}
	if got := oldCaps.snapshot(); len(got) != 0 {
		t.Errorf("old connection got %d frames, want 0 (it was replaced)", len(got))
	}
	if got := newCaps.snapshot(); len(got) != 1 {
		t.Errorf("new connection got %d frames, want 1", len(got))
	}
	_ = newHandle
}

// TestOneConnectionMultipleChatKeys verifies a single WS connection can own
// multiple ChatKeys simultaneously. The protocol allows consecutive message
// frames on one connection, each with its own context.chat_id/sender_id. PR #97
// review CRITICAL #2: the original `registered bool` guard only registered the
// FIRST ChatKey, so resume delivery for later chats on the same connection
// failed. The fix lets registerConn recognize "same connection" (by identity)
// so it does NOT cancel itself, and the connection tracks all its keys.
func TestOneConnectionMultipleChatKeys(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := &capturedFrames{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// One connection (one newConn handle) registers two different chat keys.
	// registerConnKey must recognize the same handle (by id) and NOT displace
	// itself — so the connection's cancel never fires.
	wsHandle := runner.newConn(caps.write, cancel)
	runner.registerConnKey(wsHandle, keyOf("chat-1", "alice"))
	runner.registerConnKey(wsHandle, keyOf("chat-2", "alice"))

	// The connection must not have been cancelled by the second register.
	if ctx.Err() != nil {
		t.Fatal("connection was cancelled when registering a second key on the SAME connection")
	}

	// Both keys must resolve to this connection.
	if got := runner.lookupConn(keyOf("chat-1", "alice")); got == nil {
		t.Error("first key not in registry")
	}
	if got := runner.lookupConn(keyOf("chat-2", "alice")); got == nil {
		t.Error("second key not in registry")
	}

	// Emit to EITHER key must reach the connection.
	for _, chatID := range []string{"chat-1", "chat-2"} {
		env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
		env.Target = &channel.InboundContext{Channel: "websocket", ChatID: chatID, SenderID: "alice"}
		env.Data = map[string]any{"content": "to-" + chatID}
		if err := runner.Emit(context.Background(), env); err != nil {
			t.Errorf("Emit to %s on multi-key connection: %v", chatID, err)
		}
	}
	if got := len(caps.snapshot()); got != 2 {
		t.Errorf("multi-key connection got %d frames, want 2 (one per chat)", got)
	}

	// Disconnect cleanup (releaseConn) must remove BOTH keys this connection owns.
	runner.releaseConn(wsHandle)
	if runner.lookupConn(keyOf("chat-1", "alice")) != nil {
		t.Error("first key still in registry after releaseConn")
	}
	if runner.lookupConn(keyOf("chat-2", "alice")) != nil {
		t.Error("second key still in registry after releaseConn")
	}
}

// TestRegisterConnKeyAndReleaseConnNilSafe covers the defensive nil guards.
// registerConnKey(nil, ...) returns nil (no panic); releaseConn(nil) is a
// no-op. These guards keep the registry safe if a caller ever passes a nil
// handle (e.g. a future code path that conditionally constructs a wsConn).
func TestRegisterConnKeyAndReleaseConnNilSafe(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	if got := runner.registerConnKey(nil, keyOf("chat-z", "z")); got != nil {
		t.Errorf("registerConnKey(nil,...) = %v, want nil", got)
	}
	// Must not panic:
	runner.releaseConn(nil)
	if runner.lookupConn(keyOf("chat-z", "z")) != nil {
		t.Error("nil conn should not have registered anything")
	}
}

// TestRegisterConnKeyIdempotentSameKey verifies re-registering the SAME
// connection under an already-owned key is a no-op (returns nil, no
// displacement). This is the path a connection hits when consecutive message
// frames carry the same chat_id/sender_id — registerConnKey must not displace
// itself.
func TestRegisterConnKeyIdempotentSameKey(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := &capturedFrames{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsHandle := runner.newConn(caps.write, cancel)

	if displaced := runner.registerConnKey(wsHandle, keyOf("chat-i", "ian")); displaced != nil {
		t.Fatalf("first register displaced %v, want nil", displaced)
	}
	// Re-register the SAME handle under the SAME key — must be a no-op.
	if displaced := runner.registerConnKey(wsHandle, keyOf("chat-i", "ian")); displaced != nil {
		t.Errorf("idempotent re-register displaced %v, want nil (same connection+key)", displaced)
	}
	if ctx.Err() != nil {
		t.Fatal("connection was cancelled by idempotent re-register")
	}
}
