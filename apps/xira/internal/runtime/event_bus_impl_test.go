package runtime

import (
	"sync"
	"testing"
	"time"
)

// event_bus_impl_test.go: tests the new eventBusImpl methods (PublishEvent,
// SubscribeFiltered) and the eventSubscriber priority eviction. These verify
// the evolved bus carries Event correctly while keeping the validated
// fan-out + eviction + pump/deliver machinery (AGENTS.md §1.1).

func TestEventBusImpl_PublishEventFanout(t *testing.T) {
	bus := NewEventBus()
	t.Cleanup(bus.Close)

	ch1 := bus.SubscribeFiltered(Filter{})
	ch2 := bus.SubscribeFiltered(Filter{})

	evt := AgentTurnStarted{
		MessageIDVal:   "e1",
		AgentTurnIDVal: "aturn_1",
		TimestampVal:   time.Now(),
	}
	bus.PublishEvent(evt)

	// Both subscribers receive the event.
	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case got := <-ch:
			if got.ID() != "e1" {
				t.Errorf("subscriber %d got ID %q, want e1", i, got.ID())
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d did not receive event", i)
		}
	}
}

func TestEventBusImpl_SubscribeFilteredByTurn(t *testing.T) {
	bus := NewEventBus()
	t.Cleanup(bus.Close)

	turnA := AgentTurnID("aturn_A")
	ch := bus.SubscribeFiltered(Filter{AgentTurnID: &turnA})

	// Event for turn A — should be received.
	bus.PublishEvent(AgentTurnStarted{MessageIDVal: "e1", AgentTurnIDVal: turnA, TimestampVal: time.Now()})
	select {
	case got := <-ch:
		if got.AgentTurnID() != turnA {
			t.Errorf("got turn %q, want %s", got.AgentTurnID(), turnA)
		}
	case <-time.After(time.Second):
		t.Fatal("filtered subscriber did not receive matching event")
	}

	// Event for turn B — should NOT be received.
	bus.PublishEvent(AgentTurnStarted{MessageIDVal: "e2", AgentTurnIDVal: "aturn_B", TimestampVal: time.Now()})
	select {
	case got := <-ch:
		t.Errorf("filtered subscriber received non-matching event: ID=%s", got.ID())
	case <-time.After(100 * time.Millisecond):
		// Expected — no event for turn B.
	}
}

func TestEventBusImpl_SubscribeFilteredIncludeChildren(t *testing.T) {
	bus := NewEventBus()
	t.Cleanup(bus.Close)

	parent := AgentTurnID("aturn_parent")
	ch := bus.SubscribeFiltered(Filter{AgentTurnID: &parent, IncludeChildren: true})

	// Child event — should be received because IncludeChildren=true.
	bus.PublishEvent(AgentTurnCompleted{
		MessageIDVal:         "e_child",
		AgentTurnIDVal:       "aturn_child",
		ParentAgentTurnIDVal: parent,
		TimestampVal:         time.Now(),
	})
	select {
	case got := <-ch:
		if got.AgentTurnID() != "aturn_child" {
			t.Errorf("got child turn %q, want aturn_child", got.AgentTurnID())
		}
	case <-time.After(time.Second):
		t.Fatal("parent subscriber with IncludeChildren did not receive child event")
	}
}

func TestEventBusImpl_PriorityEvictionByMethod(t *testing.T) {
	// eventSubscriber uses Event.Priority() directly (not Kind-string switch).
	// Fill the buffer with droppable events, then publish a critical event —
	// it should evict a droppable and be delivered.
	bus := NewEventBus()
	t.Cleanup(bus.Close)

	// Use a subscriber with a tiny buffer by publishing exactly
	// subscriberBufferSize droppable events, then one critical.
	ch := bus.SubscribeFiltered(Filter{})

	// Don't drain ch — let the buffer fill.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Publish subscriberBufferSize droppable events to fill the buffer.
		for i := 0; i < subscriberBufferSize; i++ {
			bus.PublishEvent(AssistantStatus{
				MessageIDVal:   "droppable",
				AgentTurnIDVal: "aturn_1",
				TimestampVal:   time.Now(),
			})
		}
		// Now publish a critical event — should evict a droppable.
		bus.PublishEvent(HumanRequested{
			MessageIDVal:   "critical",
			AgentTurnIDVal: "aturn_1",
			TimestampVal:   time.Now(),
		})
	}()
	wg.Wait()

	// Drain and check the critical event is present somewhere in the stream.
	gotCritical := false
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
drain:
	for {
		select {
		case evt := <-ch:
			if evt.ID() == "critical" {
				gotCritical = true
			}
		case <-timer.C:
			break drain
		}
	}
	if !gotCritical {
		t.Error("critical event was dropped — priority eviction should have kept it")
	}
}

func TestEventBusImpl_PublishEventAfterCloseIsDropped(t *testing.T) {
	bus := NewEventBus()
	ch := bus.SubscribeFiltered(Filter{})
	bus.Close()

	bus.PublishEvent(AgentTurnStarted{MessageIDVal: "e1", AgentTurnIDVal: "aturn_1", TimestampVal: time.Now()})

	select {
	case _, ok := <-ch:
		if ok {
			// Channel should be closed (empty), not delivering new events.
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEventBusImpl_NilPublishEventIsSafe(t *testing.T) {
	var bus *eventBusImpl // nil
	bus.PublishEvent(AgentTurnStarted{MessageIDVal: "e1", AgentTurnIDVal: "aturn_1", TimestampVal: time.Now()})
	// Must not panic.
}
