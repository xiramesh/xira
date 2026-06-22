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

// eventBusImpl is the evolved form of the old EventBus struct (A2a #44).
// It satisfies the EventBus interface (PublishEvent/SubscribeFiltered) while
// keeping the validated fan-out + priority eviction + pump/deliver machinery
// (AGENTS.md §1.1). The old Publish(RuntimeEvent) / Subscribe(ctx) are kept
// deprecated until A2b (#45) migrates callers and deletes them.
//
// Its correctness contract (AGENTS.md §1.1) is NOT "never drops" — it is
// "when it must drop, it drops by kind priority and logs Warn, never silently".
// Priority is explicit and kind-based (see eventPriority): critical
// (run.waiting_human, assistant.final) outranks droppable (everything else,
// including the assistant.status heartbeat). When a subscriber buffer is full
// and a higher-priority event arrives, the oldest strictly-lower-priority
// buffered event is evicted to make room; otherwise the incoming event is
// dropped. Both cases log Warn.
type eventBusImpl struct {
	mu     sync.RWMutex
	subs   map[*subscriber]struct{}      // old RuntimeEvent subscribers (deprecated)
	esubs  map[*eventSubscriber]struct{} // new Event subscribers (A2a)
	closed bool
}

// EventBus is now an interface (message_bus.go). NewEventBus returns the
// concrete *eventBusImpl so callers that still use the deprecated
// Publish(RuntimeEvent)/Subscribe(ctx) compile — those methods live on the
// concrete type, not the interface. A2b (#45) migrates callers to the
// interface methods and deletes the deprecated ones.
func NewEventBus() *eventBusImpl {
	return &eventBusImpl{
		subs:  make(map[*subscriber]struct{}),
		esubs: make(map[*eventSubscriber]struct{}),
	}
}

// Deprecated: Publish(RuntimeEvent) is the old API. A2b (#45) deletes it
// after callers migrate to PublishEvent. Meanwhile it still works for
// backward compatibility.
func (b *eventBusImpl) Publish(evt RuntimeEvent) {
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
// Deprecated: Subscribe(ctx) is the old API. A2b (#45) deletes it.
func (b *eventBusImpl) Subscribe(ctx context.Context) <-chan RuntimeEvent {
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

func (b *eventBusImpl) Close() {
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
	for esub := range b.esubs {
		esub.shutdown()
		delete(b.esubs, esub)
	}
}

// PublishEvent is the new Event-typed fan-out (A2a #44). Delivers evt to all
// eventSubscribers whose Filter matches. Priority eviction uses Event.Priority()
// directly (no Kind-string switch — the old eventPriority is for RuntimeEvent
// only). Phase 2-B adds WAL persistence for Reliable()==true events.
func (b *eventBusImpl) PublishEvent(evt Event) {
	if b == nil || evt == nil {
		return
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for esub := range b.esubs {
		if esub.filter.Match(evt) {
			esub.enqueue(evt)
		}
	}
}

// SubscribeFiltered registers a filter and returns a channel of matching
// Events (A2a #44). Replaces the old Subscribe(ctx) for Event consumers.
func (b *eventBusImpl) SubscribeFiltered(filter Filter) <-chan Event {
	esub := newEventSubscriber(filter)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(esub.out)
		return esub.out
	}
	b.esubs[esub] = struct{}{}
	b.mu.Unlock()
	return esub.out
}

// eventSubscriber is the Event-typed counterpart of subscriber. Same burst
// absorber + pump/deliver pattern, but buf is []Event and priority comes from
// Event.Priority() (not a Kind-string switch).
type eventSubscriber struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []Event
	out    chan Event
	filter Filter
	closed bool
}

func newEventSubscriber(filter Filter) *eventSubscriber {
	s := &eventSubscriber{
		buf:    make([]Event, 0, subscriberBufferSize),
		out:    make(chan Event),
		filter: filter,
	}
	s.cond = sync.NewCond(&s.mu)
	go s.pump()
	return s
}

func (s *eventSubscriber) enqueue(evt Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if len(s.buf) < subscriberBufferSize {
		s.appendLocked(evt)
		return
	}
	// Buffer full — priority eviction using Event.Priority() directly.
	incoming := evt.Priority()
	if idx := eventOldestBelowLocked(s.buf, incoming); idx >= 0 {
		evicted := s.buf[idx]
		s.buf = append(s.buf[:idx], s.buf[idx+1:]...)
		s.appendLocked(evt)
		slog.Warn("event bus subscriber buffer full; evicted lower-priority event",
			"evicted_kind", evicted.Kind(),
			"evicted_event_id", evicted.ID(),
			"kind", evt.Kind(),
			"event_id", evt.ID())
		return
	}
	slog.Warn("event bus subscriber buffer full; dropping event",
		"kind", evt.Kind(), "event_id", evt.ID())
}

func (s *eventSubscriber) appendLocked(evt Event) {
	s.buf = append(s.buf, evt)
	s.cond.Signal()
}

// eventOldestBelowLocked finds the oldest buffered Event whose Priority() is
// strictly below `level`. Same eviction logic as oldestBelowLocked but uses
// Event.Priority() instead of eventPriority(RuntimeEvent).
func eventOldestBelowLocked(buf []Event, level EventPriority) int {
	for i, evt := range buf {
		if evt.Priority() < level {
			return i
		}
	}
	return -1
}

func (s *eventSubscriber) pump() {
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
		s.mu.Unlock()
		if !s.deliver(evt) {
			s.mu.Lock()
			s.buf = s.buf[:0]
			break
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	close(s.out)
}

func (s *eventSubscriber) deliver(evt Event) bool {
	const sendWait = 100 * time.Millisecond
	for {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			select {
			case s.out <- evt:
				return true
			case <-time.After(currentDrainTimeout()):
				return false
			}
		}
		select {
		case s.out <- evt:
			return true
		case <-time.After(sendWait):
		}
	}
}

func (s *eventSubscriber) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.cond.Signal()
	s.mu.Unlock()
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
// Send never blocks indefinitely: deliver polls every sendWait, re-checking the
// closed flag. So a consumer that has gone away (e.g. a WebSocket handler that
// returned on a write error without draining) cannot strand this goroutine on a
// send nobody will read — once shutdown flips closed, the next poll notices it,
// abandons the blocked event, drops the remaining buffer, and closes out. A
// draining consumer (the forwarder, whose Stop() keeps consumeLoop reading
// until the channel closes) always wins each send promptly, preserving the
// §16.5 drain guarantee.
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
		s.mu.Unlock()
		if !s.deliver(evt) {
			// Shutdown noticed mid-deliver: a dead consumer can't be satisfied.
			// Drop the remaining buffer and exit so we don't leak.
			s.mu.Lock()
			s.buf = s.buf[:0]
			break
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	close(s.out)
}

// deliver hands one event to the consumer on out. While open it polls every
// sendWait so a shutdown is noticed within ~sendWait; once closed it still
// tries to hand the event to a draining consumer (the forwarder, whose Stop
// keeps reading) but bounded by drainTimeout — so a dead consumer (e.g. a
// WebSocket handler that returned) can't strand this goroutine, while a
// draining consumer still gets every buffered event (§16.5). Returns true if
// delivered, false if abandoned on shutdown.
func (s *subscriber) deliver(evt RuntimeEvent) bool {
	const sendWait = 100 * time.Millisecond
	for {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			// Shutdown: give a draining consumer up to drainTimeout to read this
			// event; if it doesn't (dead consumer), give up and let the caller
			// drop the remaining buffer. This preserves the forwarder's drain
			// guarantee while bounding the dead-consumer stall.
			select {
			case s.out <- evt:
				return true
			case <-time.After(currentDrainTimeout()):
				return false
			}
		}
		select {
		case s.out <- evt:
			return true
		case <-time.After(sendWait):
			// re-check closed on next iteration
		}
	}
}

// drainTimeout bounds how long deliver waits, once closed, for a consumer to
// read a buffered event before giving up (the dead-consumer escape). Long
// enough that a draining forwarder (reads in a tight loop at Stop) always gets
// every buffered event (§16.5); short enough that a dead consumer (e.g. a
// WebSocket handler that returned) can't stall shutdown for long. Guarded by
// drainTimeoutMu so tests can shrink it safely while a pump goroutine reads it.
var (
	drainTimeoutMu sync.RWMutex
	drainTimeout   = 2 * time.Second
)

func currentDrainTimeout() time.Duration {
	drainTimeoutMu.RLock()
	defer drainTimeoutMu.RUnlock()
	return drainTimeout
}

// setDrainTimeoutForTest swaps drainTimeout and returns the previous value;
// tests must restore it (t.Cleanup). Only the test build should call this.
func setDrainTimeoutForTest(d time.Duration) time.Duration {
	drainTimeoutMu.Lock()
	defer drainTimeoutMu.Unlock()
	prev := drainTimeout
	drainTimeout = d
	return prev
}

// shutdown marks the subscriber closed and wakes the pump so it can finish
// draining buf into out (or abandon it if the consumer is gone) and close out.
// Called under the bus write lock.
func (s *subscriber) shutdown() {
	s.mu.Lock()
	s.closed = true
	s.cond.Signal()
	s.mu.Unlock()
}
