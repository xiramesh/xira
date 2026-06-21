package progress

import (
	"context"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// This file tests the FULL delegation lifecycle event flow as it actually
// happened in the ~/daming-xira session on 2026-06-21 10:38 (see
// docs/architecture/xira-ilink-delegation-rca-2026-06-21.zh.md §"最新复现").
//
// Real sequence observed:
//   user → agent.delegate.allowed → agent.delegate.started → agent.delegate.failed
//        → assistant.final → run.finished
//
// The user saw "已委派 / 已启动 / 最终答案" but NEVER saw the failure, because:
//   1. allowed + started consumed the MaxMessagesPerTurn=2 quota, so failed was
//      silently dropped by dispatch (§dispatch quota gate).
//   2. Even if failed were queued, assistant.final's drain() would drop it.
//
// These tests pin both behaviors as regressions: failure/timeout are
// high-value facts the user MUST see; they must not be starved by low-value
// lifecycle chatter, nor dropped by drain.

// lifecycleEvent builds a delegation lifecycle event matching what
// executeDelegateAgentTool emits, with the expose_progress payload flag and
// Conversation visibility as set by the runtime.
func lifecycleEvent(kind string, expose bool, scope runtime.RuntimeEventScope) runtime.RuntimeEvent {
	v := &runtime.RuntimeEventVisibility{
		// allowed/started/completed default to Conversation=false; failed/timeout
		// are Conversation=true (events.go eventVisibility). expose_progress is a
		// separate payload flag the forwarder checks.
		Conversation: kind == "agent.delegate.failed" || kind == "agent.delegate.timeout",
		Activity:     true, Inspector: true, Audit: true,
	}
	payload := map[string]any{
		"target_agent_id":           "code-agent",
		"effective_max_duration_ms": int64(7200000),
	}
	if expose {
		payload["expose_progress"] = true
	}
	return runtime.RuntimeEvent{
		ID: "evt-" + kind, Kind: kind, RunID: "run-1",
		Scope: &scope, Visibility: v, Payload: payload,
	}
}

// TestDelegateFailedNotStarvedByLifecycleQuota: the core 10:38 regression.
// allowed + started (expose_progress) are delivered first and consume the
// progress quota. Then agent.delegate.failed arrives — it MUST still be
// delivered, because a child failure is a fact the user needs, not optional
// progress chatter.
func TestDelegateFailedNotStarvedByLifecycleQuota(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	policy := testPolicy() // MaxMessagesPerTurn=2
	fwd := Start(context.Background(), Request{
		EventBus: bus, Inbound: inboundFixture(), Policy: policy, Sender: sender,
	})
	sc := scopeFixture()

	// Reproduce the exact 10:38 order: allowed, started (both expose_progress,
	// both delivered, consuming the 2-message quota), THEN failed.
	bus.Publish(lifecycleEvent("agent.delegate.allowed", true, sc))
	bus.Publish(lifecycleEvent("agent.delegate.started", true, sc))
	if !waitUntil(t, 2*time.Second, func() bool { return len(sender.kinds()) >= 2 }) {
		t.Fatalf("allowed+started should be delivered first: %v", sender.kinds())
	}
	// Quota is now full (progressSent=2). The critical event:
	bus.Publish(lifecycleEvent("agent.delegate.failed", true, sc))

	if !waitUntil(t, 2*time.Second, func() bool { return containsKind(sender, "agent.delegate.failed") }) {
		t.Fatalf("agent.delegate.failed was starved by lifecycle quota after allowed+started: %v — "+
			"user saw '已委派/已启动' but never the failure (the 10:38 bug)", sender.kinds())
	}
	fwd.Stop()
}

// TestDelegateTimeoutNotStarvedByLifecycleQuota: same as above but for timeout.
func TestDelegateTimeoutNotStarvedByLifecycleQuota(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	fwd := Start(context.Background(), Request{
		EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender,
	})
	sc := scopeFixture()

	bus.Publish(lifecycleEvent("agent.delegate.allowed", true, sc))
	bus.Publish(lifecycleEvent("agent.delegate.started", true, sc))
	waitUntil(t, 2*time.Second, func() bool { return len(sender.kinds()) >= 2 })
	bus.Publish(lifecycleEvent("agent.delegate.timeout", true, sc))

	if !waitUntil(t, 2*time.Second, func() bool { return containsKind(sender, "agent.delegate.timeout") }) {
		t.Fatalf("agent.delegate.timeout was starved by lifecycle quota: %v", sender.kinds())
	}
	fwd.Stop()
}

// TestDelegateFailedNotDroppedByFinalDrain: if failed is queued but not yet
// delivered when assistant.final arrives, drain() must not drop it — the user
// must learn the child failed before/as the parent answers.
func TestDelegateFailedNotDroppedByFinalDrain(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	// Slow sender so failed queues up before the send loop reaches it.
	sender := &recordingSender{delay: 50 * time.Millisecond}
	fwd := Start(context.Background(), Request{
		EventBus: bus, Inbound: inboundFixture(), Policy: testPolicy(), Sender: sender,
	})
	sc := scopeFixture()

	bus.Publish(lifecycleEvent("agent.delegate.failed", true, sc))
	// Immediately publish assistant.final — drain() fires. failed must survive.
	finalEvt := runtime.RuntimeEvent{
		ID: "evt-final", Kind: "assistant.final", RunID: "run-1",
		Scope: &sc,
		Visibility: &runtime.RuntimeEventVisibility{Conversation: true, Activity: true, Inspector: true, Audit: false},
	}
	bus.Publish(finalEvt)

	if !waitUntil(t, 2*time.Second, func() bool { return containsKind(sender, "agent.delegate.failed") }) {
		t.Fatalf("agent.delegate.failed was dropped by drain() when assistant.final arrived: %v", sender.kinds())
	}
	fwd.Stop()
}

// TestFullLifecycleExposesPathWhenChildSucceeds: the happy path — when
// expose_progress=true and the child succeeds, the user sees allowed→started→
// completed, making the execution path visible (target + real deadline).
func TestFullLifecycleExposesPathWhenChildSucceeds(t *testing.T) {
	bus := runtime.NewEventBus()
	defer bus.Close()
	sender := &recordingSender{}
	policy := testPolicy()
	policy.MaxMessagesPerTurn = 8 // enough for all three lifecycle events
	fwd := Start(context.Background(), Request{
		EventBus: bus, Inbound: inboundFixture(), Policy: policy, Sender: sender,
	})
	sc := scopeFixture()

	bus.Publish(lifecycleEvent("agent.delegate.allowed", true, sc))
	bus.Publish(lifecycleEvent("agent.delegate.started", true, sc))
	bus.Publish(lifecycleEvent("agent.delegate.completed", true, sc))

	if !waitUntil(t, 2*time.Second, func() bool {
		return containsKind(sender, "agent.delegate.allowed") &&
			containsKind(sender, "agent.delegate.started") &&
			containsKind(sender, "agent.delegate.completed")
	}) {
		t.Fatalf("happy-path lifecycle not fully exposed: %v", sender.kinds())
	}
	fwd.Stop()
}
