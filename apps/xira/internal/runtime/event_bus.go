package runtime

import (
	"context"
	"log/slog"
	"sync"
)

// subscriberBufferSize bounds the per-subscriber buffer. It must absorb a
// burst of events while a slow consumer drains, so a fast publisher cannot
// starve delivery of conversation-critical facts. See AGENTS.md §1.1.
const subscriberBufferSize = 256

// EventBus is a best-effort, per-Service singleton fan-out. Its correctness
// contract (AGENTS.md §1.1) is NOT "never drops" — it is "when it must drop,
// it drops by kind priority and logs Warn, never silently". Conversation-facing
// facts (run.waiting_human, agent.delegate.failed/timeout, assistant.final,
// assistant.status, capability_gap — i.e. Visibility.Conversation==true) are
// high-priority: when a subscriber buffer is full and a high-priority event
// arrives, a low-priority buffered event is evicted to make room. If only
// high-priority events are buffered (or the new event is low-priority), the new
// event is dropped. Both cases log Warn. This is what makes critical events
// reachable under a noise burst — the drop happens at the bus layer, before the
// forwarder's internal queue can recover it.
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

// isHighPriority reports whether an event must beat low-priority noise for
// delivery. Conversation-facing facts are the high-priority set; everything
// else (adk.event, model.*, context.*, tool.*) is droppable noise.
func isHighPriority(evt RuntimeEvent) bool {
	return evt.Visibility != nil && evt.Visibility.Conversation
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
	// Buffer full. High-priority events evict the oldest low-priority event to
	// make room; otherwise the incoming event is dropped. Either way, Warn —
	// never silent (AGENTS.md §1.1).
	if isHighPriority(evt) {
		if idx := oldestLowPriorityLocked(s.buf); idx >= 0 {
			evicted := s.buf[idx]
			s.buf = append(s.buf[:idx], s.buf[idx+1:]...)
			s.appendLocked(evt)
			slog.Warn("event bus subscriber buffer full; evicted low-priority event for high-priority event",
				"evicted_kind", evicted.Kind,
				"evicted_event_id", evicted.ID,
				"kind", evt.Kind,
				"event_id", evt.ID)
			return
		}
	}
	slog.Warn("event bus subscriber buffer full; dropping event",
		"kind", evt.Kind, "event_id", evt.ID)
}

func (s *subscriber) appendLocked(evt RuntimeEvent) {
	s.buf = append(s.buf, evt)
	s.cond.Signal()
}

// oldestLowPriorityLocked returns the index of the oldest non-conversation
// (low-priority) event in buf, or -1 if none exists.
func oldestLowPriorityLocked(buf []RuntimeEvent) int {
	for i, evt := range buf {
		if !isHighPriority(evt) {
			return i
		}
	}
	return -1
}

// pump drains buf into out until the subscription is shut down. Keeping buf as
// the burst absorber means a slow consumer (blocked on out) cannot cause the
// publisher to drop — the buffer absorbs the burst first.
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
		// Send out without holding the lock, so a slow consumer does not block
		// enqueue under this lock. out is unbuffered, so this parks until the
		// consumer reads — which is fine, buf has already absorbed the burst.
		s.mu.Unlock()
		s.out <- evt
		s.mu.Lock()
	}
	s.mu.Unlock()
	close(s.out)
}

// shutdown marks the subscriber closed and wakes the pump so it can finish
// draining buf and close out. Called under the bus write lock.
func (s *subscriber) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.cond.Signal()
	s.mu.Unlock()
}
