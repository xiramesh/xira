package channelrunner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/channelrunner/ingest"
)

// manager_emit_test.go: tests Manager.Emit routing (RFC #27 — unified outbound
// delivery for stateless HITL resume). Manager is the channel container; Emit
// routes an OutboundEnvelope to the runner matching envelope.Target.Channel.

// mockEmitRunner is a Runner that also implements channel.OutboundEmitter. It
// records every Emit call for assertion.
type mockEmitRunner struct {
	id      string
	ch      string
	mu      sync.Mutex
	calls   []channel.OutboundEnvelope
	emitErr error
}

func (m *mockEmitRunner) ID() string                  { return m.id }
func (m *mockEmitRunner) Channel() string             { return m.ch }
func (m *mockEmitRunner) Start(context.Context) error { return nil }
func (m *mockEmitRunner) Stop(context.Context) error  { return nil }
func (m *mockEmitRunner) SetIngest(*ingest.Ingest)    {}

func (m *mockEmitRunner) Capabilities() channel.CapabilitySet {
	return channel.CapabilitySet{channel.CapabilityProactiveOutbound}
}

func (m *mockEmitRunner) Emit(ctx context.Context, env channel.OutboundEnvelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.emitErr != nil {
		return m.emitErr
	}
	m.calls = append(m.calls, env)
	return nil
}

func (m *mockEmitRunner) emitCalls() []channel.OutboundEnvelope {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]channel.OutboundEnvelope, len(m.calls))
	copy(out, m.calls)
	return out
}

// TestManagerEmitRoutesByChannel verifies Manager.Emit finds the runner whose
// Channel() matches envelope.Target.Channel and delegates to its Emit.
func TestManagerEmitRoutesByChannel(t *testing.T) {
	feishu := &mockEmitRunner{id: "feishu-default", ch: "feishu"}
	ilink := &mockEmitRunner{id: "ilink-default", ch: "ilink"}
	mgr := &Manager{runners: []Runner{feishu, ilink}}

	// Emit targeting feishu → only feishu receives it.
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "feishu", ChatID: "c1"}
	env.Data = map[string]any{"content": "hello"}

	if err := mgr.Emit(context.Background(), env); err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}
	if got := feishu.emitCalls(); len(got) != 1 {
		t.Errorf("feishu got %d calls, want 1", len(got))
	}
	if got := ilink.emitCalls(); len(got) != 0 {
		t.Errorf("ilink got %d calls, want 0 (routing must be channel-specific)", len(got))
	}

	// Emit targeting ilink → only ilink receives it.
	env2 := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env2.Target = &channel.InboundContext{Channel: "ilink", ChatID: "c2"}
	env2.Data = map[string]any{"content": "hi"}

	if err := mgr.Emit(context.Background(), env2); err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}
	if got := ilink.emitCalls(); len(got) != 1 {
		t.Errorf("ilink got %d calls, want 1", len(got))
	}
	if got := feishu.emitCalls(); len(got) != 1 {
		t.Errorf("feishu got %d calls, want 1 (unchanged from earlier)", len(got))
	}
}

func TestManagerEmitRoutesByExactEntrypoint(t *testing.T) {
	first := &mockEmitRunner{id: "feishu-first", ch: "feishu"}
	owner := &mockEmitRunner{id: "feishu-owner", ch: "feishu"}
	mgr := &Manager{runners: []Runner{first, owner}}
	env := channel.NewOutboundEnvelope(channel.OutboundProactiveMessage)
	env.Target = &channel.InboundContext{Channel: "feishu", EntrypointID: "feishu-owner"}
	env.Recipient = &channel.OutboundRecipient{ID: "ou_owner", IDType: "open_id"}
	env.Data = map[string]any{"content": "private"}

	if err := mgr.Emit(context.Background(), env); err != nil {
		t.Fatalf("Emit returned error: %v", err)
	}
	if len(first.emitCalls()) != 0 || len(owner.emitCalls()) != 1 {
		t.Fatalf("exact routing calls: first=%d owner=%d", len(first.emitCalls()), len(owner.emitCalls()))
	}
}

func TestManagerEmitRejectsAmbiguousChannelFallback(t *testing.T) {
	mgr := &Manager{runners: []Runner{
		&mockEmitRunner{id: "feishu-first", ch: "feishu"},
		&mockEmitRunner{id: "feishu-second", ch: "feishu"},
	}}
	env := channel.NewOutboundEnvelope(channel.OutboundProactiveMessage)
	env.Target = &channel.InboundContext{Channel: "feishu"}
	env.Data = map[string]any{"content": "private"}

	err := mgr.Emit(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous fallback error = %v, want explicit ambiguity", err)
	}
}

func TestManagerEmitRejectsEntrypointChannelMismatch(t *testing.T) {
	mgr := &Manager{runners: []Runner{
		&mockEmitRunner{id: "feishu-owner", ch: "feishu"},
	}}
	env := channel.NewOutboundEnvelope(channel.OutboundProactiveMessage)
	env.Target = &channel.InboundContext{Channel: "ilink", EntrypointID: "feishu-owner"}
	env.Data = map[string]any{"content": "private"}

	err := mgr.Emit(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "channel") {
		t.Fatalf("entrypoint/channel mismatch error = %v", err)
	}
}

// TestManagerEmitUnknownChannelErrors verifies Emit returns a clear error when
// no runner is registered for the target channel. This Manager has only a
// feishu runner, so Emit to "websocket" finds no match — even though
// *websocket.Runner does implement OutboundEmitter, it isn't registered here.
func TestManagerEmitUnknownChannelErrors(t *testing.T) {
	mgr := &Manager{runners: []Runner{
		&mockEmitRunner{id: "feishu-default", ch: "feishu"},
	}}
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "websocket", ChatID: "c1"}

	err := mgr.Emit(context.Background(), env)
	if err == nil {
		t.Fatal("Emit to channel with no registered runner should error")
	}
}

func TestManagerEmitRejectsMissingTargetAndUnknownEntrypoint(t *testing.T) {
	mgr := &Manager{runners: []Runner{
		&mockEmitRunner{id: "feishu-default", ch: "feishu"},
	}}
	if err := mgr.Emit(context.Background(), channel.NewOutboundEnvelope(channel.OutboundProactiveMessage)); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("missing target error = %v", err)
	}
	env := channel.NewOutboundEnvelope(channel.OutboundProactiveMessage)
	env.Target = &channel.InboundContext{Channel: "feishu", EntrypointID: "feishu-missing"}
	if err := mgr.Emit(context.Background(), env); err == nil || !strings.Contains(err.Error(), "feishu-missing") {
		t.Fatalf("unknown entrypoint error = %v", err)
	}
}

// TestManagerEmitRunnerNotEmitter verifies Emit errors clearly when the matched
// runner does NOT implement OutboundEmitter (defensive — future runner types).
func TestManagerEmitRunnerNotEmitter(t *testing.T) {
	// bareRunner implements Runner but NOT OutboundEmitter.
	bare := &bareRunner{id: "x", ch: "weird"}
	mgr := &Manager{runners: []Runner{bare}}
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "weird", ChatID: "c1"}

	err := mgr.Emit(context.Background(), env)
	if err == nil {
		t.Fatal("Emit to non-emitter runner should error")
	}
}

type bareRunner struct{ id, ch string }

func (b *bareRunner) ID() string                  { return b.id }
func (b *bareRunner) Channel() string             { return b.ch }
func (b *bareRunner) Start(context.Context) error { return nil }
func (b *bareRunner) Stop(context.Context) error  { return nil }
func (b *bareRunner) SetIngest(*ingest.Ingest)    {}

// TestManagerEmitNilSafe verifies Emit on a nil/empty Manager is a clear error,
// not a panic (resume may run before any channel is configured).
func TestManagerEmitNilSafe(t *testing.T) {
	var mgr *Manager
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "feishu", ChatID: "c1"}
	if err := mgr.Emit(context.Background(), env); err == nil {
		t.Fatal("nil Manager.Emit should error, not panic")
	}
}

// TestManagerEmitPropagatesError verifies the runner's Emit error propagates
// (so the caller — resume path — can log it).
func TestManagerEmitPropagatesError(t *testing.T) {
	sentinel := errors.New("send failed")
	feishu := &mockEmitRunner{id: "feishu-default", ch: "feishu", emitErr: sentinel}
	mgr := &Manager{runners: []Runner{feishu}}
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "feishu", ChatID: "c1"}

	if err := mgr.Emit(context.Background(), env); !errors.Is(err, sentinel) {
		t.Errorf("Emit error = %v, want %v", err, sentinel)
	}
}

// TestManagerCapabilities verifies Manager implements OutboundEmitter by
// aggregating runner capabilities (so it can be injected as channel.OutboundEmitter).
func TestManagerCapabilities(t *testing.T) {
	mgr := &Manager{runners: []Runner{
		&mockEmitRunner{id: "feishu-default", ch: "feishu"},
	}}
	caps := mgr.Capabilities()
	if !caps.Supports(channel.CapabilityProactiveOutbound) {
		t.Errorf("Manager.Capabilities() missing proactive_outbound; got %v", caps)
	}
}
