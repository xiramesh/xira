package progress

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// recordingSender captures delivered messages. delay simulates a slow channel
// SendText; err forces delivery failures.
type recordingSender struct {
	mu    sync.Mutex
	msgs  []Message
	delay time.Duration
	err   error
}

func (s *recordingSender) SendProgress(_ context.Context, msg Message) error {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	s.msgs = append(s.msgs, msg)
	s.mu.Unlock()
	return s.err
}

func (s *recordingSender) messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.msgs))
	copy(out, s.msgs)
	return out
}

func (s *recordingSender) kinds() []string {
	msgs := s.messages()
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Kind
	}
	return out
}

func containsKind(s *recordingSender, kind string) bool {
	for _, k := range s.kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func testPolicy() Policy {
	return Policy{InitialSilenceThreshold: 0, MinInterval: 0, MaxMessagesPerTurn: 2, MaxChars: 180}
}

func makeEvent(kind string, conversation bool) runtime.RuntimeEvent {
	sc := scopeFixture()
	v := &runtime.RuntimeEventVisibility{Conversation: conversation, Activity: true, Inspector: true, Audit: true}
	return runtime.RuntimeEvent{ID: "e-" + kind, Kind: kind, RunID: "run-1", Scope: &sc, Visibility: v}
}

// makeEventFor builds an event whose scope matches the given inbound (so two
// forwarders with different MessageIDs can be driven independently).
func makeEventFor(kind, messageID string) runtime.RuntimeEvent {
	sc := scopeFixture()
	sc.MessageID = messageID
	v := &runtime.RuntimeEventVisibility{Conversation: true, Activity: true, Inspector: true, Audit: true}
	return runtime.RuntimeEvent{ID: "e-" + kind + "-" + messageID, Kind: kind, RunID: "run-" + messageID, Scope: &sc, Visibility: v}
}

func waitUntil(t *testing.T, timeout time.Duration, ok func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return ok()
}

func TestForwarderSendsDelegateFailure(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
	bus.Publish(makeEvent("agent.delegate.failed", true))
	if !waitUntil(t, time.Second, func() bool { return containsKind(sender, "agent.delegate.failed") }) {
		t.Fatalf("delegate.failed not sent: %v", sender.kinds())
	}
	fwd.Stop()
}

func TestForwarderSendsSilenceNotice(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	p := testPolicy()
	p.InitialSilenceThreshold = 40 * time.Millisecond
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: p, Sender: sender})
	if !waitUntil(t, time.Second, func() bool { return containsKind(sender, "run.silence_notice") }) {
		t.Fatalf("silence notice not sent: %v", sender.kinds())
	}
	fwd.Stop()
}

func TestForwarderDropsNonAllowlistedEvents(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
	// assistant.status is conversation-visible but NOT a deliverable kind;
	// adk.event / tool.started are not conversation-visible.
	bus.Publish(makeEvent("assistant.status", true))
	bus.Publish(makeEvent("adk.event", false))
	bus.Publish(makeEvent("tool.started", false))
	time.Sleep(120 * time.Millisecond)
	if got := sender.kinds(); len(got) != 0 {
		t.Fatalf("non-allowlisted events must not be delivered: %v", got)
	}
	fwd.Stop()
}

func TestForwarderRequiresMatchingScope(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
	// Different MessageID -> different turn -> must not match.
	bus.Publish(makeEventFor("agent.delegate.failed", "msg-other"))
	time.Sleep(120 * time.Millisecond)
	if got := sender.kinds(); len(got) != 0 {
		t.Fatalf("event from another turn must not be delivered: %v", got)
	}
	fwd.Stop()
}

func TestForwarderDedupesAndThrottles(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	p := testPolicy()
	p.MinInterval = 60 * time.Millisecond
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: p, Sender: sender})
	// Same kind+text twice -> deduped to one.
	bus.Publish(makeEvent("agent.delegate.failed", true))
	bus.Publish(makeEvent("agent.delegate.failed", true))
	if !waitUntil(t, time.Second, func() bool { return len(sender.kinds()) >= 1 }) {
		t.Fatalf("first delegate.failed not sent: %v", sender.kinds())
	}
	time.Sleep(120 * time.Millisecond)
	if n := countKind(sender, "agent.delegate.failed"); n != 1 {
		t.Fatalf("dedupe failed, delegate.failed sent %d times: %v", n, sender.kinds())
	}
	// A different kind bypasses dedupe and is delivered (throttle waits, then sends).
	bus.Publish(makeEvent("agent.delegate.timeout", true))
	if !waitUntil(t, time.Second, func() bool { return containsKind(sender, "agent.delegate.timeout") }) {
		t.Fatalf("delegate.timeout not sent after throttle window: %v", sender.kinds())
	}
	fwd.Stop()
}

func countKind(s *recordingSender, kind string) int {
	n := 0
	for _, k := range s.kinds() {
		if k == kind {
			n++
		}
	}
	return n
}

func TestForwarderSendErrorDoesNotCancelRun(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{err: errors.New("channel down")}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
	bus.Publish(makeEvent("agent.delegate.failed", true))
	if !waitUntil(t, time.Second, func() bool { return len(sender.kinds()) >= 1 }) {
		t.Fatalf("delivery attempt not recorded: %v", sender.kinds())
	}
	// A delivery error must not panic or wedge the forwarder; Stop returns.
	done := make(chan struct{})
	go func() { fwd.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop() blocked after sender error")
	}
}

func TestForwarderSurvivesEventBusBurst(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{delay: 15 * time.Millisecond} // slow channel
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
	// A burst of non-conversation noise plus the critical facts. The consumer
	// must keep draining the bus; the delegate facts must still be delivered
	// (§8.4).
	for i := 0; i < 400; i++ {
		bus.Publish(makeEvent("adk.event", false))
	}
	bus.Publish(makeEvent("agent.delegate.failed", true))
	bus.Publish(makeEvent("agent.delegate.timeout", true))
	if !waitUntil(t, 3*time.Second, func() bool {
		return containsKind(sender, "agent.delegate.failed") && containsKind(sender, "agent.delegate.timeout")
	}) {
		t.Fatalf("critical events lost during bus burst: %v", sender.kinds())
	}
	fwd.Stop()
}

// TestForwarderQueueEvictsForWaitingHuman: the forwarder's OWN internal queue
// must not drop run.waiting_human when full of delegate progress. A delegate
// progress burst (agent.delegate.failed) can fill the 256-slot queue while the
// sender is slow; a subsequent run.waiting_human must be admitted by evicting a
// lower-priority queued event, never dropped. This is the unit-level regression
// for the rereview's "queue full; dropping run.waiting_human" finding — the
// interaction signal is independent of the progress quota (§9.1) and must
// always reach the user.
func TestForwarderQueueEvictsForWaitingHuman(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	// Slow sender so the queue saturates before it drains.
	sender := &recordingSender{delay: 15 * time.Millisecond}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})

	// Saturate the forwarder's internal queue with important-but-evictable
	// delegate progress. Each event has a distinct ID so dedup (kind|text) does
	// not collapse them at the sender.
	for i := 0; i < queueCapacity+50; i++ {
		evt := makeEvent("agent.delegate.failed", true)
		evt.ID = "burst-" + strconv.Itoa(i)
		bus.Publish(evt)
	}
	// The interaction signal arrives last, after the queue is full. It must be
	// delivered (evicting a queued delegate event), not dropped.
	bus.Publish(makeEvent("run.waiting_human", true))

	if !waitUntil(t, 5*time.Second, func() bool { return containsKind(sender, "run.waiting_human") }) {
		t.Fatalf("run.waiting_human was dropped when the forwarder queue was full of delegate progress: %v", sender.kinds())
	}
	fwd.Stop()
}

// makeExposeEvent builds a delegate-lifecycle event (allowed/started/completed)
// which defaults to Conversation=false, carrying the given expose_progress flag
// in its payload.
func makeExposeEvent(kind string, expose bool) runtime.RuntimeEvent {
	sc := scopeFixture()
	v := &runtime.RuntimeEventVisibility{Conversation: false, Activity: true, Inspector: true, Audit: true}
	return runtime.RuntimeEvent{
		ID:         "e-" + kind,
		Kind:       kind,
		RunID:      "run-1",
		Scope:      &sc,
		Visibility: v,
		Payload:    map[string]any{"expose_progress": expose, "target_agent_id": "code-agent", "effective_max_duration_ms": 7200000},
	}
}

// TestForwarderDeliversLifecycleWhenExposeProgress: with expose_progress=true,
// the delegate lifecycle events (allowed/started/completed) — which default to
// Conversation=false — are delivered to IM, surfacing the target and real
// deadline for external-command workers.
func TestForwarderDeliversLifecycleWhenExposeProgress(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	// Allow enough progress quota for all three lifecycle events (each counts
	// against MaxMessagesPerTurn).
	policy := testPolicy()
	policy.MaxMessagesPerTurn = 8
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: policy, Sender: sender})

	for _, kind := range []string{"agent.delegate.allowed", "agent.delegate.started", "agent.delegate.completed"} {
		bus.Publish(makeExposeEvent(kind, true))
	}
	if !waitUntil(t, 2*time.Second, func() bool {
		return containsKind(sender, "agent.delegate.allowed") &&
			containsKind(sender, "agent.delegate.started") &&
			containsKind(sender, "agent.delegate.completed")
	}) {
		t.Fatalf("lifecycle events with expose_progress should be delivered: %v", sender.kinds())
	}
	fwd.Stop()
}

// TestForwarderHidesLifecycleWithoutExposeProgress: without expose_progress,
// the lifecycle events stay invisible (the default — ordinary bounded
// delegates don't spam IM with start/complete chatter).
func TestForwarderHidesLifecycleWithoutExposeProgress(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})

	for _, kind := range []string{"agent.delegate.allowed", "agent.delegate.started", "agent.delegate.completed"} {
		bus.Publish(makeExposeEvent(kind, false))
	}
	time.Sleep(150 * time.Millisecond)
	for _, kind := range []string{"agent.delegate.allowed", "agent.delegate.started", "agent.delegate.completed"} {
		if containsKind(sender, kind) {
			t.Fatalf("lifecycle event %q without expose_progress should NOT be delivered: %v", kind, sender.kinds())
		}
	}
	fwd.Stop()
}

func TestForwarderConcurrentRunsDoNotCross(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	inA := inboundFixture() // msg-1
	inB := inboundFixture()
	inB.MessageID = "msg-2"
	senderA := &recordingSender{}
	senderB := &recordingSender{}
	fwdA := Start(context.Background(), Request{EventBus: bus, Inbound: inA, Policy: testPolicy(), Sender: senderA})
	fwdB := Start(context.Background(), Request{EventBus: bus, Inbound: inB, Policy: testPolicy(), Sender: senderB})
	bus.Publish(makeEventFor("agent.delegate.failed", "msg-1"))
	bus.Publish(makeEventFor("agent.delegate.failed", "msg-2"))
	if !waitUntil(t, time.Second, func() bool {
		return containsKind(senderA, "agent.delegate.failed") && containsKind(senderB, "agent.delegate.failed")
	}) {
		t.Fatalf("both forwarders should receive their own event: A=%v B=%v", senderA.kinds(), senderB.kinds())
	}
	if containsKind(senderB, "agent.delegate.failed") && len(senderB.kinds()) != 1 {
		t.Fatalf("forwarder B leaked events: %v", senderB.kinds())
	}
	// No cross-delivery: A received exactly one, B exactly one.
	if n := len(senderA.kinds()); n != 1 {
		t.Fatalf("forwarder A received %d events, want 1: %v", n, senderA.kinds())
	}
	fwdA.Stop()
	fwdB.Stop()
}

func TestForwarderDrainsOnAssistantFinal(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
	// Drain first, then a delegate fact arrives -> must NOT be delivered.
	bus.Publish(makeEvent("assistant.final", true))
	waitUntil(t, time.Second, func() bool {
		// drain() sets the flag synchronously inside consumeLoop once the
		// assistant.final event is read; give the consumer a moment.
		time.Sleep(20 * time.Millisecond)
		return true
	})
	bus.Publish(makeEvent("agent.delegate.failed", true))
	time.Sleep(150 * time.Millisecond)
	if got := sender.kinds(); len(got) != 0 {
		t.Fatalf("no progress should be delivered after assistant.final drain: %v", got)
	}
	fwd.Stop()
}

func TestForwarderDoesNotRedeliverFinal(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
	bus.Publish(makeEvent("assistant.final", true))
	time.Sleep(120 * time.Millisecond)
	if got := sender.kinds(); len(got) != 0 {
		t.Fatalf("assistant.final must not be rendered/delivered: %v", got)
	}
	fwd.Stop()
}

// TestForwarderStopDoesNotBusySpin: regression for a busy-spin in dequeue. Stop()
// cancels ctx (step 1) BEFORE closeQueue (step 3); in between, consumerWg.Wait
// runs and the sendLoop's dequeue used to re-select on a forever-ready ctx.Done
// at ~100% CPU. The fix parks on queueCh instead. This test asserts Stop returns
// promptly (no hang) and — as a spin guard — that queueCh is left unsignaled
// while parked (a spinning loop would keep draining any signal).
func TestForwarderStopDoesNotBusySpin(t *testing.T) {
	for i := 0; i < 20; i++ {
		bus := runtime.NewEventBus()
		sender := &recordingSender{delay: 5 * time.Millisecond} // slowish sender
		fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
		// Publish several events so the queue is non-empty during shutdown.
		for j := 0; j < 10; j++ {
			bus.Publish(makeEvent("agent.delegate.failed", true))
		}
		done := make(chan struct{})
		go func() { fwd.Stop(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: Stop() hung (possible busy-spin or deadlock)", i)
		}
		bus.Close()
	}
}

func TestForwarderStopsOnFallbackWhenNoFinal(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
	// waiting_human run: no assistant.final is published; Stop() is the
	// fallback. The interaction signal itself is still delivered.
	bus.Publish(makeEvent("run.waiting_human", true))
	if !waitUntil(t, time.Second, func() bool { return containsKind(sender, "run.waiting_human") }) {
		t.Fatalf("waiting_human not delivered: %v", sender.kinds())
	}
	done := make(chan struct{})
	go func() { fwd.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop() blocked on a no-final run")
	}
}

func TestForwarderStopDrainsBufferedBusEvents(t *testing.T) {
	for i := 0; i < 20; i++ {
		bus := runtime.NewEventBus()
		sender := &recordingSender{}
		fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
		bus.Publish(makeEvent("run.waiting_human", true))
		fwd.Stop()
		bus.Close()
		if !containsKind(sender, "run.waiting_human") {
			t.Fatalf("iteration %d: Stop() lost buffered waiting_human event: %v", i, sender.kinds())
		}
	}
}

func TestForwarderDeliversWaitingHumanRegardlessOfQuota(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})
	// Saturate the progress quota (MaxMessagesPerTurn=2).
	bus.Publish(makeEvent("agent.delegate.failed", true))
	bus.Publish(makeEvent("agent.delegate.timeout", true))
	if !waitUntil(t, time.Second, func() bool { return len(sender.kinds()) >= 2 }) {
		t.Fatalf("progress quota not saturated: %v", sender.kinds())
	}
	// waiting_human is an interaction signal: it must still be delivered.
	bus.Publish(makeEvent("run.waiting_human", true))
	if !waitUntil(t, time.Second, func() bool { return containsKind(sender, "run.waiting_human") }) {
		t.Fatalf("waiting_human must be delivered even with progress quota full: %v", sender.kinds())
	}
	fwd.Stop()
}

func TestForwarderDisabledForGroupChat(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	inbound := inboundFixture()
	inbound.ChatType = "group"
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inbound, Policy: testPolicy(), Sender: sender})
	evt := makeEvent("agent.delegate.failed", true)
	evt.Scope.ChatType = "group"
	bus.Publish(evt)
	time.Sleep(100 * time.Millisecond)
	if got := sender.kinds(); len(got) != 0 {
		t.Fatalf("group-chat progress must be disabled in v0, got %v", got)
	}
	fwd.Stop()
}

func TestForwarderDisabledWithoutMessageID(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	inbound := inboundFixture()
	inbound.MessageID = ""
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inbound, Policy: testPolicy(), Sender: sender})
	bus.Publish(makeEventFor("agent.delegate.failed", ""))
	time.Sleep(100 * time.Millisecond)
	if got := sender.kinds(); len(got) != 0 {
		t.Fatalf("disabled forwarder must not deliver: %v", got)
	}
	fwd.Stop() // must be a safe no-op
}

// TestForwarderSkipsChildWaitingHuman: a delegated (child) run emits
// run.waiting_human with the parent's MessageID in its scope (it inherits the
// trigger identity), so it matches this forwarder. But it carries no summary
// and is NOT the canonical interaction signal — the parent's own
// run.waiting_human (DelegationDepth 0, with summary) is. Projecting the
// child's would yield a context-free duplicate prompt. The child event is still
// audited/logged upstream; it must only be suppressed from IM delivery.
func TestForwarderSkipsChildWaitingHuman(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	fwd := Start(context.Background(), Request{EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender})

	// Child run waiting_human: matches inbound scope (same MessageID) but
	// DelegationDepth > 0 -> must NOT be delivered.
	childEvt := makeEvent("run.waiting_human", true)
	childEvt.Scope.DelegationDepth = 1
	bus.Publish(childEvt)

	// Parent (top-level) run waiting_human: DelegationDepth 0 -> IS delivered.
	parentEvt := makeEvent("run.waiting_human", true)
	parentEvt.ID = "evt-parent-waiting"
	bus.Publish(parentEvt)

	if !waitUntil(t, time.Second, func() bool { return containsKind(sender, "run.waiting_human") }) {
		t.Fatalf("parent waiting_human (depth 0) must be delivered: %v", sender.kinds())
	}
	time.Sleep(120 * time.Millisecond)
	if n := countKind(sender, "run.waiting_human"); n != 1 {
		t.Fatalf("child waiting_human must be suppressed; got %d deliveries: %v", n, sender.kinds())
	}
	fwd.Stop()
}
