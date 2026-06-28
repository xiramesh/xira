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

// TestEmitDeliversToRegisteredConnection verifies Emit routes a final to the
// connection registered under the envelope's Target ChatKey. This is the core
// resume-delivery guarantee: a resumed run's final reaches the live WS client.
func TestEmitDeliversToRegisteredConnection(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	caps := &capturedFrames{}

	_, cancel := context.WithCancel(context.Background())
	runner.registerConn(keyOf("chat-1", "alice"), caps.write, cancel)

	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
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
	content, _ := data["content"].(string)
	if content != "final answer" {
		t.Errorf("frame content = %q, want %q", content, "final answer")
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
	runner.registerConn(keyOf("chat-2", "bob"), caps.write, cancel)

	// Simulate disconnect: the connection's HandleConnection returns and
	// unregisters (the defer in HandleConnection).
	runner.unregisterConn(keyOf("chat-2", "bob"))

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
	runner.registerConn(keyOf("chat-3", "carol"), func(outboundFrame) error { return nil }, oldCancel)

	newCaps := &capturedFrames{}
	_, newCancel := context.WithCancel(context.Background())
	defer newCancel()
	runner.registerConn(keyOf("chat-3", "carol"), newCaps.write, newCancel)

	// Old connection's cancel must have fired synchronously inside registerConn.
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
	runner.registerConn(keyOf("chat-4", "dave"), func(outboundFrame) error { return nil }, func() {})

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
	runner.registerConn(keyOf("chat-x", "x"), caps.write, cancel)

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

	// onRegister mirrors HandleConnection's real wiring: register under the
	// chat key with this conn's writeFrame + cancel. We track the handle so we
	// can assert ownership semantics, like HandleConnection's defer would.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var registeredHandle *wsConn
	onRegister := func(key frt.ChatKey) {
		registeredHandle = runner.registerConn(key, caps.write, cancel)
	}

	runner.handleMessage(ctx, makeMessageFrame("om_1", "chat-9", "eve", "hi"),
		"websocket-default", caps.write, addActive, removeActive, onRegister)

	// Give the router goroutine a moment to run the turn (it writes the
	// response frame asynchronously). The registration, however, happens
	// synchronously inside handleMessage before session.Handle returns control
	// — so the registry is populated by the time handleMessage returns.
	if registeredHandle == nil {
		t.Fatal("onRegister did not register a connection handle")
	}
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

	// Now simulate disconnect: HandleConnection's defer would call
	// unsetConnIfOurs(key, handle). Verify it removes the entry we own.
	runner.unsetConnIfOurs(keyOf("chat-9", "eve"), registeredHandle)
	if got := runner.lookupConn(keyOf("chat-9", "eve")); got != nil {
		t.Error("connection still in registry after unsetConnIfOurs on disconnect")
	}

	// Emit after disconnect must error (no live connection).
	if err := runner.Emit(context.Background(), env); err == nil {
		t.Error("Emit after disconnect should error")
	}
}

// TestUnsetConnIfOursDoesNotEvictNewOwner verifies the ownership guard: when a
// newer connection took over the key, the OLD connection's disconnect cleanup
// must NOT evict the new owner. This is the subtle correctness property that
// keeps the registry consistent under concurrent connect/disconnect.
func TestUnsetConnIfOursDoesNotEvictNewOwner(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())

	oldCaps := &capturedFrames{}
	newCaps := &capturedFrames{}
	_, oldCancel := context.WithCancel(context.Background())
	_, newCancel := context.WithCancel(context.Background())

	oldHandle := runner.registerConn(keyOf("chat-A", "zoe"), oldCaps.write, oldCancel)
	// New connection takes over (cancels old).
	newHandle := runner.registerConn(keyOf("chat-A", "zoe"), newCaps.write, newCancel)

	// Old connection now disconnects — its defer calls unsetConnIfOurs with its
	// own (now-stale) handle. Must NOT evict the new owner.
	runner.unsetConnIfOurs(keyOf("chat-A", "zoe"), oldHandle)
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
