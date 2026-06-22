package runtime

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// runtimeNumGoroutine wraps runtime.NumGoroutine so tests can detect pump exit
// without touching the subscriber's out channel (reading out would unblock a
// blocked send and skip the drainTimeout branch under test).
func runtimeNumGoroutine() int { return runtime.NumGoroutine() }

// deliveredWithin polls the channel until it observes the wanted kind or the
// timeout elapses. The pump drains the slice-backed buffer into the unbuffered
// out channel asynchronously, so a freshly enqueued event may sit behind many
// buffered ones — we poll rather than assume head-of-line ordering.
func deliveredWithin(ch <-chan RuntimeEvent, want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if contains(drainAll(ch), want) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// droppableEvent, importantEvent, and criticalEvent build events at each of the
// three bus priority tiers (AGENTS.md §1.1, event_bus.go eventPriority):
//   - droppable: high-volume noise the forwarder never renders (adk.event) or
//     conversation-visible heartbeats (assistant.status). Dropped, never evicts.
//   - important: agent.delegate.failed/timeout (user-facing failure progress).
//     Evicts droppable events only.
//   - critical: run.waiting_human (interaction signal) / assistant.final
//     (drain signal). Evict anything below them.
func droppableEvent(kind, id string) RuntimeEvent {
	return RuntimeEvent{
		ID:         id,
		Kind:       kind,
		RunID:      "run-1",
		Visibility: &RuntimeEventVisibility{Conversation: kind == "assistant.status", Activity: true, Inspector: true, Audit: true},
	}
}

func importantEvent(id string) RuntimeEvent {
	return RuntimeEvent{
		ID:         id,
		Kind:       "agent.delegate.failed",
		RunID:      "run-1",
		Visibility: &RuntimeEventVisibility{Conversation: true, Activity: true, Inspector: true, Audit: true},
	}
}

func criticalEvent(kind, id string) RuntimeEvent {
	return RuntimeEvent{
		ID:         id,
		Kind:       kind,
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

// TestEventBusDeliversImportantUnderDroppableBurst: the bus must not drop an
// important event (agent.delegate.failed) even when a subscriber's buffer is
// filled with droppable noise (adk.event) first. Important events evict
// droppable ones. This is the core AGENTS.md §1.1 contract and the unit-level
// form of TestForwarderSurvivesEventBusBurst.
func TestEventBusDeliversImportantUnderDroppableBurst(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()
	ch := bus.Subscribe(context.Background())

	for i := 0; i < subscriberBufferSize+50; i++ {
		bus.Publish(droppableEvent("adk.event", "noise"))
	}
	bus.Publish(importantEvent("critical"))

	if !deliveredWithin(ch, "agent.delegate.failed", 2*time.Second) {
		t.Fatalf("important event lost after droppable noise burst")
	}
}

// TestEventBusStatusBurstMustNotStarveWaitingHuman: regression for the
// visibility-as-priority bug. assistant.status is conversation-visible BUT
// droppable (a progress heartbeat the forwarder never renders); run.waiting_human
// is critical (the interaction signal). A status burst must NOT fill the buffer
// and starve waiting_human — both were conversation=true, so the old
// visibility-based priority put them in the same tier and dropped waiting_human.
func TestEventBusStatusBurstMustNotStarveWaitingHuman(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()
	ch := bus.Subscribe(context.Background())

	// Flood with assistant.status (conversation=true, but droppable), then send
	// the critical interaction signal.
	for i := 0; i < subscriberBufferSize+50; i++ {
		bus.Publish(droppableEvent("assistant.status", "status"))
	}
	bus.Publish(criticalEvent("run.waiting_human", "waiting"))

	if !deliveredWithin(ch, "run.waiting_human", 2*time.Second) {
		t.Fatalf("run.waiting_human was not delivered after assistant.status burst")
	}
}

// TestEventBusCriticalEvictsImportant: a critical event (run.waiting_human)
// evicts an important event (agent.delegate.failed) when the buffer holds only
// important events — i.e. critical outranks important, not just droppable.
func TestEventBusCriticalEvictsImportant(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()
	ch := bus.Subscribe(context.Background())

	// Fill with important events.
	for i := 0; i < subscriberBufferSize; i++ {
		bus.Publish(importantEvent("imp"))
	}
	// Critical arrives: must evict an important event to make room.
	bus.Publish(criticalEvent("run.waiting_human", "waiting"))

	if !deliveredWithin(ch, "run.waiting_human", 2*time.Second) {
		t.Fatalf("critical event was dropped instead of evicting an important event")
	}
}

// TestEventBusDroppableNeverEvicts: a droppable event arriving at a full buffer
// is dropped and never evicts anything (important or critical).
//
// Determinism note: the pump always pops one event into a blocking send, so to
// guarantee the buffer is observed full we publish capacity+1 important events
// first (one is in-flight in the pump's blocked send, the rest fill the
// buffer), then the droppable event must be dropped rather than evict an
// important event or be enqueued.
func TestEventBusDroppableNeverEvicts(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()
	ch := bus.Subscribe(context.Background())
	// Never read ch -> the pump's send blocks, keeping the buffer saturated.

	// capacity+1 important events saturate the buffer despite the pump holding
	// one in its blocked send.
	for i := 0; i < subscriberBufferSize+1; i++ {
		bus.Publish(importantEvent("imp"))
	}
	// Droppable event must be dropped at enqueue (buffer full of important).
	bus.Publish(droppableEvent("adk.event", "dropped"))

	// Now drain: every delivered event must be agent.delegate.failed; no
	// adk.event may appear (it was dropped, not enqueued).
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		if got := drainAll(ch); contains(got, "adk.event") {
			t.Fatalf("droppable event was delivered; it should have been dropped when buffer was full of important events: %v", got)
		} else if len(got) == 0 {
			break
		}
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
	bus.Publish(importantEvent("after-cancel"))
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

// TestEventBusNonDrainingConsumerDoesNotLeakPump: regression for the goroutine
// leak. A consumer (e.g. the WebSocket handler returning on a write error) may
// cancel its ctx WITHOUT draining buffered events. The pump must still exit
// (bounded by drainTimeout) and close the channel, rather than blocking forever
// on a send no one will read.
func TestEventBusNonDrainingConsumerDoesNotLeakPump(t *testing.T) {
	bus := NewEventBus()
	defer bus.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ch := bus.Subscribe(ctx)

	// Fill the buffer, then cancel ctx WITHOUT ever reading ch — simulating a
	// consumer that returned mid-stream. The channel must still close within a
	// bounded time (drainTimeout + slack), proving the pump goroutine exited.
	for i := 0; i < subscriberBufferSize; i++ {
		bus.Publish(droppableEvent("adk.event", "noise"))
	}
	cancel()

	deadline := time.Now().Add(currentDrainTimeout() + 3*time.Second)
	for time.Now().Before(deadline) {
		if _, ok := <-ch; !ok {
			return // closed — pump exited, no leak
		}
	}
	t.Fatalf("subscriber channel did not close within drainTimeout+3s; pump goroutine likely leaked")
}

// TestEventBusDeadConsumerShutdownExitsPump: a consumer that has gone away
// (e.g. WebSocket handler returned on a write error) must not strand the pump
// goroutine on a send nobody will read. We publish an event (so the pump is
// mid-deliver, blocked because no one reads out), THEN cancel. deliver's next
// poll notices closed and runs the closed-branch: wait currentDrainTimeout for
// a reader, then give up. With drainTimeout shrunk, the channel closes fast.
// This is the only test that reaches deliver's drainTimeout (return-false) arm.
func TestEventBusDeadConsumerShutdownExitsPump(t *testing.T) {
	prev := setDrainTimeoutForTest(40 * time.Millisecond)
	t.Cleanup(func() { setDrainTimeoutForTest(prev) })

	bus := NewEventBus()
	defer bus.Close()
	ctx, cancel := context.WithCancel(context.Background())
	ch := bus.Subscribe(ctx)
	_ = ch // never read — consumer is "dead"

	bus.Publish(droppableEvent("adk.event", "dead"))
	time.Sleep(30 * time.Millisecond) // let pump enter deliver's send poll
	cancel()

	// The channel must close within ~drainTimeout + slack, proving deliver's
	// dead-consumer escape worked (it did not block forever on out<-evt).
	start := time.Now()
	deadline := time.Now().Add(currentDrainTimeout() + 2*time.Second)
	for time.Now().Before(deadline) {
		select {
		case _, ok := <-ch:
			if !ok {
				if elapsed := time.Since(start); elapsed > currentDrainTimeout()+1*time.Second {
					t.Fatalf("closed but took %v — dead-consumer escape too slow", elapsed)
				}
				return
			}
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("channel did not close within drainTimeout+2s for a dead consumer — pump goroutine leaked")
}

// TestEventBusNilPublishIsSafe: a nil EventBus must be safe to Publish on
// (defensive — callers may hold a nil bus in some test/edge paths). Covers the
// `if b == nil { return }` guard.
func TestEventBusNilPublishIsSafe(t *testing.T) {
	var bus *eventBusImpl                         // nil
	bus.Publish(droppableEvent("adk.event", "x")) // must not panic
}

// TestEventBusPublishAfterCloseIsDropped: publishing to a closed bus is a
// silent no-op (covers the `if b.closed { return }` arm in Publish).
func TestEventBusPublishAfterCloseIsDropped(t *testing.T) {
	bus := NewEventBus()
	bus.Close()
	// Must not panic; event is simply not delivered.
	bus.Publish(droppableEvent("adk.event", "after-close"))
}

// TestEventBusSubscribeAfterCloseReturnsClosedChannel: subscribing to an
// already-closed bus returns an already-closed channel (covers the closed-bus
// arm of Subscribe).
func TestEventBusSubscribeAfterCloseReturnsClosedChannel(t *testing.T) {
	bus := NewEventBus()
	bus.Close()
	ch := bus.Subscribe(context.Background())
	if _, ok := <-ch; ok {
		t.Fatalf("subscribe on a closed bus must return a closed channel")
	}
}

// TestEventBusCloseIsIdempotent: closing an already-closed bus is a no-op and
// must not panic (covers the `if b.closed { return }` arm in Close).
func TestEventBusCloseIsIdempotent(t *testing.T) {
	bus := NewEventBus()
	bus.Close()
	bus.Close() // must not panic
	bus.Close()
}
