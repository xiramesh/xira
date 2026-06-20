package runtime

import (
	"context"
	"testing"
	"time"
)

func timeNow() time.Time        { return time.Now() }
func timeSince(t time.Time) time.Duration { return time.Since(t) }

// lowPriorityEvent and highPriorityEvent build events at opposite ends of the
// bus drop policy: adk.event (conversation=false, droppable noise) vs
// agent.delegate.failed (conversation=true, must-be-delivered fact).
func lowPriorityEvent(id string) RuntimeEvent {
	return RuntimeEvent{
		ID:         id,
		Kind:       "adk.event",
		RunID:      "run-1",
		Visibility: &RuntimeEventVisibility{Conversation: false, Activity: true, Inspector: true, Audit: true},
	}
}

func highPriorityEvent(id string) RuntimeEvent {
	return RuntimeEvent{
		ID:         id,
		Kind:       "agent.delegate.failed",
		RunID:      "run-1",
		Visibility: &RuntimeEventVisibility{Conversation: true, Activity: true, Inspector: true, Audit: true},
	}
}

// drainAll blocks until the channel is idle (no event immediately ready).
func drainAll(ch <-chan RuntimeEvent) []string {
	out := make([]string, 0, 256)
	// Spin-read with a small grace window so the pump can push buffered events.
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, evt.Kind)
		default:
			return out
		}
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// TestEventBusDeliversHighPriorityUnderBurst: the bus must not drop a
// high-priority (conversation) event even when a subscriber's buffer is filled
// with low-priority noise first. This is the core AGENTS.md §1.1 contract and
// the unit-level form of TestForwarderSurvivesEventBusBurst.
func TestEventBusDeliversHighPriorityUnderBurst(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()
	ch := bus.Subscribe(context.Background())

	// Fill the buffer beyond capacity with noise, then publish one critical
	// event. The critical event must still arrive (it evicts a noise event or
	// rides a drained slot).
	for i := 0; i < subscriberBufferSize+50; i++ {
		bus.Publish(lowPriorityEvent("noise"))
	}
	bus.Publish(highPriorityEvent("critical"))

	// The critical event is appended after up to 256 noise events, so we must
	// drain until we see it, not assume it is first. Give the pump time to push.
	deadline := timeNow()
	sawCritical := false
	for !sawCritical && timeSince(deadline) < 2*time.Second {
		if contains(drainAll(ch), "agent.delegate.failed") {
			sawCritical = true
		}
	}
	if !sawCritical {
		t.Fatalf("high-priority event lost after noise burst")
	}
}

// TestEventBusEvictsLowPriorityForHighPriority: when the buffer is full and a
// high-priority event arrives, a low-priority event is evicted to make room
// (not the high-priority event dropped).
func TestEventBusEvictsLowPriorityForHighPriority(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()
	ch := bus.Subscribe(context.Background())

	// Exactly fill with noise.
	for i := 0; i < subscriberBufferSize; i++ {
		bus.Publish(lowPriorityEvent("noise"))
	}
	// One critical arrives: buffer is full, must evict a noise event.
	bus.Publish(highPriorityEvent("critical"))

	sawCritical := false
	deadline := timeNow()
	for !sawCritical && timeSince(deadline) < 2*time.Second {
		if contains(drainAll(ch), "agent.delegate.failed") {
			sawCritical = true
		}
	}
	if !sawCritical {
		t.Fatalf("high-priority event was dropped instead of evicting a low-priority event")
	}
}

// TestEventBusDropsLowPriorityWhenFullOfHighPriority: if the buffer is full of
// high-priority events and another low-priority event arrives, the low-priority
// event is the one dropped (no high-priority eviction).
func TestEventBusDropsLowPriorityWhenFullOfHighPriority(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()
	ch := bus.Subscribe(context.Background())

	// Fill with high-priority events.
	for i := 0; i < subscriberBufferSize; i++ {
		bus.Publish(highPriorityEvent("hp"))
	}
	// Extra low-priority event must be dropped, not evict a high-priority one.
	bus.Publish(lowPriorityEvent("dropped"))

	// Drain and confirm no adk.event leaked through.
	var sawLow bool
	deadline := timeNow()
	for timeSince(deadline) < time.Second {
		kinds := drainAll(ch)
		if contains(kinds, "adk.event") {
			sawLow = true
			break
		}
		if len(kinds) == 0 {
			break
		}
	}
	if sawLow {
		t.Fatalf("low-priority event should have been dropped when buffer was full of high-priority events")
	}
}

// TestEventBusSubscribeCancelledOnContext: canceling the subscribe context
// removes the subscriber and closes its channel.
func TestEventBusSubscribeCancelledOnContext(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ch := bus.Subscribe(ctx)
	cancel()

	if _, ok := <-ch; ok {
		t.Fatalf("subscriber channel should be closed after context cancel")
	}
	// Publishing after cancel must not panic.
	bus.Publish(highPriorityEvent("after-cancel"))
}

// TestEventBusCloseClosesAllSubscribers: Close closes every subscriber channel.
func TestEventBusCloseClosesAllSubscribers(t *testing.T) {
	bus := NewEventBus()
	ch1 := bus.Subscribe(context.Background())
	ch2 := bus.Subscribe(context.Background())
	bus.Close()

	for i, ch := range []<-chan RuntimeEvent{ch1, ch2} {
		if _, ok := <-ch; ok {
			t.Fatalf("subscriber %d channel should be closed after Close", i)
		}
	}
}
