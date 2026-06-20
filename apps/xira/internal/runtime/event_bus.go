package runtime

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// subscriberBufferSize bounds the per-subscriber buffer. It must absorb a
// burst of events while a slow consumer drains, so a fast publisher cannot
// starve delivery of conversation-critical facts. See AGENTS.md §1.1.
const subscriberBufferSize = 256

// EventBus is a best-effort, per-Service singleton fan-out. Its correctness
// contract (AGENTS.md §1.1) is NOT "never drops" — it is "when it must drop, it
// drops by kind priority and logs Warn, never silently". Priority is explicit
// and kind-based (see eventPriority): critical (run.waiting_human,
// assistant.final) outranks important (agent.delegate.failed/timeout) outranks
// droppable (everything else, including the assistant.status heartbeat). When a
// subscriber buffer is full and a higher-priority event arrives, the oldest
// strictly-lower-priority buffered event is evicted to make room; otherwise the
// incoming event is dropped. Both cases log Warn. This is what makes critical
// events reachable under a noise burst — the drop happens at the bus layer,
// before the forwarder's internal queue can recover it. Note priority is a
// distinct axis from Visibility.Conversation: that selects the *plane*
// (conversation/inspector/audit), not whether dropping is tolerable.
type EventBus struct {
	mu     sync.RWMutex
	subs   map[*subscriber]struct{}
	closed bool
}

func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[*subscriber]struct{})}
}

func (b *EventBus) Publish(evt RuntimeEvent) {
	if b == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for sub := range b.subs {
		sub.enqueue(evt)
	}
}

// Subscribe returns a read-only channel of events for this subscriber. The
// channel is closed when ctx is canceled or the bus is closed. Consumers read
// it with `for evt := range ch`.
func (b *EventBus) Subscribe(ctx context.Context) <-chan RuntimeEvent {
	sub := newSubscriber()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(sub.out)
		return sub.out
	}
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		if _, ok := b.subs[sub]; ok {
			delete(b.subs, sub)
			sub.shutdown()
		}
		b.mu.Unlock()
	}()
	return sub.out
}

func (b *EventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for sub := range b.subs {
		sub.shutdown()
		delete(b.subs, sub)
	}
}

// subscriber is one subscription's buffered queue with priority eviction. It
// is safe for concurrent use: Publish calls enqueue from many goroutines, while
// a single pump goroutine drains buf into out.
type subscriber struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  []RuntimeEvent
	out  chan RuntimeEvent

	closed bool // set under mu when the bus/subscription is being torn down
}

func newSubscriber() *subscriber {
	s := &subscriber{
		buf: make([]RuntimeEvent, 0, subscriberBufferSize),
		out: make(chan RuntimeEvent),
	}
	s.cond = sync.NewCond(&s.mu)
	go s.pump()
	return s
}

// eventPriority ranks an event kind by delivery reliability — how much it
// hurts the IM progress feed if the bus drops it. This is NOT the same axis as
// Visibility.Conversation: that says which *plane* an event belongs to
// (conversation vs inspector vs audit), not whether dropping it is tolerable.
// Using Conversation as a proxy for priority was a bug — it put the
// high-volume `assistant.status` heartbeat in the same tier as the
// must-not-drop `run.waiting_human` interaction signal, so a status burst could
// starve waiting_human. Priority is now explicit and kind-based (AGENTS.md
// §1.1: "kind 优先级").
//
// Tiers, by the forwarder's actual delivery contract:
//   - priorityCritical: run.waiting_human (blocks the user; the one interaction
//     signal that must reach IM) and assistant.final (the forwarder's drain
//     signal; missing it breaks drain ordering). These may evict anything.
//   - priorityImportant: agent.delegate.failed / .timeout (user-facing failure
//     progress the forwarder renders). These evict only droppable events.
//   - priorityDroppable: everything else — assistant.status heartbeat,
//     adk.event, tool.*, llm.*, context.*, model.*, session.*, usage.*. These
//     are dropped (never evict) when the buffer is full.
const (
	priorityDroppable = iota
	priorityImportant
	priorityCritical
)

func eventPriority(evt RuntimeEvent) int {
	switch evt.Kind {
	case "run.waiting_human", "assistant.final":
		return priorityCritical
	case "agent.delegate.failed", "agent.delegate.timeout":
		return priorityImportant
	default:
		return priorityDroppable
	}
}

// enqueue appends evt to the buffer, applying the priority-eviction policy
// when full. Called under the bus RLock by every Publish, so it must be fast
// and never block on the consumer.
func (s *subscriber) enqueue(evt RuntimeEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if len(s.buf) < subscriberBufferSize {
		s.appendLocked(evt)
		return
	}
	// Buffer full. Evict the oldest buffered event STRICTLY lower priority than
	// the incoming one (so a critical event can evict important/droppable, an
	// important event can evict droppable, but a droppable event evicts nothing
	// and is itself dropped). Either way, Warn — never silent (AGENTS.md §1.1).
	incoming := eventPriority(evt)
	if idx := oldestBelowLocked(s.buf, incoming); idx >= 0 {
		evicted := s.buf[idx]
		s.buf = append(s.buf[:idx], s.buf[idx+1:]...)
		s.appendLocked(evt)
		slog.Warn("event bus subscriber buffer full; evicted lower-priority event",
			"evicted_kind", evicted.Kind,
			"evicted_event_id", evicted.ID,
			"kind", evt.Kind,
			"event_id", evt.ID)
		return
	}
	slog.Warn("event bus subscriber buffer full; dropping event",
		"kind", evt.Kind, "event_id", evt.ID)
}

func (s *subscriber) appendLocked(evt RuntimeEvent) {
	s.buf = append(s.buf, evt)
	s.cond.Signal()
}

// oldestBelowLocked returns the index of the oldest buffered event whose
// priority is strictly below `level`, or -1 if none exists. This is the
// eviction victim: the least-valuable event that the incoming event outranks.
func oldestBelowLocked(buf []RuntimeEvent, level int) int {
	for i, evt := range buf {
		if eventPriority(evt) < level {
			return i
		}
	}
	return -1
}

// pump drains buf into out until the subscription is shut down. Keeping buf as
// the burst absorber means a slow consumer (blocked on out) cannot cause the
// publisher to drop — the buffer absorbs the burst first.
//
// Send is a blocking send that yields to a shutdown timer: at shutdown the
// consumer may have stopped reading (e.g. the WebSocket handler returned on a
// write error without draining). To avoid stranding this goroutine forever on
// such a consumer, the send races against drainTimeout after shutdown is set —
// but only after shutdown, so a still-draining consumer (the forwarder, whose
// Stop() waits for consumeLoop to finish) always gets the buffered events
// (§16.5). Events are never dropped before shutdown.
func (s *subscriber) pump() {
	s.mu.Lock()
	for {
		if s.closed && len(s.buf) == 0 {
			break
		}
		if len(s.buf) == 0 {
			s.cond.Wait()
			continue
		}
		evt := s.buf[0]
		s.buf = s.buf[1:]
		closed := s.closed
		s.mu.Unlock()
		if !closed {
			s.out <- evt
		} else {
			// Shutting down: prefer the consumer (forwarder drains at Stop),
			// but bound the wait so a non-draining consumer can't leak this
			// goroutine.
			select {
			case s.out <- evt:
			case <-time.After(drainTimeout):
			}
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	close(s.out)
}

// drainTimeout bounds how long pump waits for a consumer to read a buffered
// event during shutdown. Long enough that a draining forwarder (which reads in
// a tight loop at Stop) always wins; short enough that a dead consumer (e.g. a
// WebSocket handler that returned) can't leak the goroutine for long.
const drainTimeout = 2 * time.Second

// shutdown marks the subscriber closed and wakes the pump so it can finish
// draining buf into out (bounded by drainTimeout) and close out. Called under
// the bus write lock.
func (s *subscriber) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.cond.Signal()
	s.mu.Unlock()
}
