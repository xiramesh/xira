package progress

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

const (
	// queueCapacity bounds the internal queue between the bus consumer and the
	// sender. It must be large enough to absorb a burst while a slow channel
	// SendText is in flight, so the consumer keeps draining the bus channel and
	// critical events are not silently dropped at the bus layer (§8.4).
	queueCapacity = 256

	// progressSendTimeout bounds a single channel delivery. It is independent
	// of the forwarder lifecycle context so Stop() does not interrupt an
	// in-flight send.
	progressSendTimeout = 10 * time.Second
)

// Forwarder is a request-bound progress projection: one instance lives for one
// RunAgent turn. It subscribes to the runtime EventBus, filters events to the
// inbound scope, renders allowlisted runtime facts, and delivers them through
// the Sender with throttle/dedupe/quota applied.
type Forwarder struct {
	req     Request
	matcher *scopeMatcher
	render  ProgressRenderer

	ctx    context.Context
	cancel context.CancelFunc
	busCh  <-chan runtime.RuntimeEvent

	// queue is the burst absorber between the bus consumer and the sender. It is
	// a slice-backed bounded queue (not a buffered channel) so that a full queue
	// can EVICT a low-priority event to admit a critical one — mirroring the bus
	// contract. The interaction signal run.waiting_human must never be dropped
	// here; a delegate progress burst must not starve it.
	queueMu     sync.Mutex
	queue       []runtime.RuntimeEvent
	queueCh     chan struct{} // signaled when queue becomes non-empty
	queueClosed bool

	consumerWg sync.WaitGroup
	senderWg   sync.WaitGroup
	stopOnce   sync.Once

	mu           sync.Mutex
	progressSent int
	lastSend     time.Time
	dedup        map[string]struct{}
	drained      bool
	silenceTimer *time.Timer
}

// Start binds a forwarder to one inbound request and spawns its goroutines.
// If the inbound request is outside the v0 scope (non-direct chat) or carries
// no MessageID, the forwarder is returned disabled (no subscription, no
// delivery): without MessageID there is no safe turn isolation on the global
// bus, so staying silent is the only correct choice (§8.3).
func Start(parent context.Context, req Request) *Forwarder {
	f := &Forwarder{
		req:     req,
		matcher: newScopeMatcher(req.Inbound),
		render:  ProgressRenderer{MaxChars: req.Policy.MaxChars},
		queue:   make([]runtime.RuntimeEvent, 0, queueCapacity),
		queueCh: make(chan struct{}, 1),
		dedup:   make(map[string]struct{}),
	}
	if req.EventBus == nil || req.Sender == nil || req.Inbound.MessageID == "" || req.Inbound.ChatType != "direct" {
		slog.Warn("progress forwarder disabled",
			"reason", disabledReason(req),
			"entrypoint_id", req.Inbound.EntrypointID,
			"chat_id", req.Inbound.ChatID,
			"sender_id", req.Inbound.SenderID)
		return f
	}
	f.ctx, f.cancel = context.WithCancel(parent)
	f.busCh = req.EventBus.Subscribe(f.ctx)
	f.consumerWg.Add(1)
	f.senderWg.Add(1)
	go f.consumeLoop()
	go f.sendLoop()
	f.scheduleSilence()
	return f
}

func disabledReason(req Request) string {
	switch {
	case req.EventBus == nil:
		return "no event bus"
	case req.Sender == nil:
		return "no sender"
	case req.Inbound.MessageID == "":
		return "inbound request has no MessageID; cannot isolate turn scope"
	case req.Inbound.ChatType != "direct":
		return "non-direct chat; v0 progress is direct-chat only"
	default:
		return "disabled"
	}
}

// Stop tears the forwarder down. It cancels the bus subscription and silence
// timer, waits for the bus consumer to process already-buffered events, then
// closes the sender queue so queued items drain (so a waiting_human published
// just before Stop is delivered, §16.5). It is the fallback stop signal
// covering HITL, pure-failure runs, and assistant.final being dropped by the
// best-effort bus (§8.5).
func (f *Forwarder) Stop() {
	f.stopOnce.Do(func() {
		if f.cancel != nil {
			f.cancel()
		}
		f.stopSilence()
		f.consumerWg.Wait()
		f.closeQueue()
		f.senderWg.Wait()
	})
}

// consumeLoop reads the bus and enqueues allowlisted, scope-matching events.
// It never sends on the channel itself (read/write decoupling, §8.4): a slow
// Sender cannot stall consumption here.
func (f *Forwarder) consumeLoop() {
	defer f.consumerWg.Done()
	for evt := range f.busCh {
		f.handleBusEvent(evt)
	}
}

func (f *Forwarder) handleBusEvent(evt runtime.RuntimeEvent) {
	if !f.matcher.match(evt) {
		return
	}
	if evt.Kind == "assistant.final" {
		f.drain()
		return
	}
	if evt.Visibility == nil || !evt.Visibility.Conversation {
		return
	}
	if !isDeliverableKind(evt.Kind) {
		return
	}
	// A delegated (child) run can also emit run.waiting_human, and because the
	// child inherits the parent's MessageID it matches this forwarder's inbound
	// scope. It carries no summary, so it renders as a context-free prompt that
	// arrives BEFORE the parent's authoritative run.waiting_human (which carries
	// the summary). Two prompts + no dedup (different text) = a confusing
	// duplicate. Only the top-level run (DelegationDepth 0) is the canonical
	// interaction signal; the child's is still audited/logged, just not projected
	// to IM chat. See docs/architecture/xira-conversation-progress-feed-v0.zh.md.
	if evt.Kind == "run.waiting_human" && evt.Scope != nil && evt.Scope.DelegationDepth > 0 {
		return
	}
	if !f.enqueue(evt) {
		slog.Warn("progress forwarder queue full; dropping event",
			"kind", evt.Kind, "event_id", evt.ID)
	}
}

// isDeliverableKind reports whether a kind is rendered+delivered by the v0
// forwarder: the progress kinds (silence is produced internally) plus the
// waiting_human interaction signal.
func isDeliverableKind(kind string) bool {
	switch kind {
	case "agent.delegate.failed", "agent.delegate.timeout", "run.waiting_human":
		return true
	}
	return false
}

// sendLoop drains the internal queue and dispatches each event until Stop()
// closes the queue after the bus consumer has exited.
func (f *Forwarder) sendLoop() {
	defer f.senderWg.Done()
	for {
		evt, ok := f.dequeue()
		if !ok {
			return
		}
		f.dispatch(evt)
	}
}

// dequeue returns the next queued event, blocking until one is available. It
// returns ok=false when the queue is closed AND empty (i.e. shutdown is complete
// and all buffered events have been dispatched).
//
// It blocks on queueCh (woken by enqueue/closeQueue) rather than polling, so a
// canceled ctx between Stop()'s cancel and closeQueue steps does NOT busy-spin:
// the loop parks on queueCh until closeQueue signals. Buffered events are still
// drained first (§16.5), then it exits on close.
func (f *Forwarder) dequeue() (runtime.RuntimeEvent, bool) {
	for {
		f.queueMu.Lock()
		if len(f.queue) > 0 {
			evt := f.queue[0]
			f.queue = f.queue[1:]
			f.queueMu.Unlock()
			return evt, true
		}
		if f.queueClosed {
			f.queueMu.Unlock()
			return runtime.RuntimeEvent{}, false
		}
		f.queueMu.Unlock()
		// Park until enqueue or closeQueue signals. We deliberately do NOT select
		// on ctx.Done() here: doing so caused a busy-spin once ctx was canceled
		// (Stop cancels ctx before closeQueue, and ctx.Done stays ready forever,
		// re-entering the loop at 100% CPU). closeQueue always signals queueCh, so
		// the park reliably ends at shutdown while buffered events still drain.
		<-f.queueCh
	}
}

func (f *Forwarder) dispatch(evt runtime.RuntimeEvent) {
	msg, ok := f.render.Render(evt)
	if !ok {
		return
	}
	isWaiting := evt.Kind == "run.waiting_human"

	for {
		f.mu.Lock()
		if f.drained {
			f.mu.Unlock()
			return
		}
		// waiting_human is an interaction signal: delivered independently of the
		// progress quota (§9.1).
		if !isWaiting && f.progressSent >= f.req.Policy.MaxMessagesPerTurn {
			f.mu.Unlock()
			return
		}
		dedupKey := evt.Kind + "|" + msg.Text
		if _, dup := f.dedup[dedupKey]; dup {
			f.mu.Unlock()
			return
		}
		// Throttle progress; interaction signals bypass MinInterval. Skip the
		// wait once shutting down so queued items are delivered at Stop().
		if !isWaiting && !f.lastSend.IsZero() && f.req.Policy.MinInterval > 0 && f.ctx.Err() == nil {
			if wait := f.req.Policy.MinInterval - time.Since(f.lastSend); wait > 0 {
				f.mu.Unlock()
				select {
				case <-time.After(wait):
				case <-f.ctx.Done():
				}
				continue
			}
		}
		f.dedup[dedupKey] = struct{}{}
		f.lastSend = time.Now()
		if !isWaiting {
			f.progressSent++
		}
		f.mu.Unlock()
		break
	}

	// Deliver with an independent timeout so Stop() (which cancels f.ctx) does
	// not interrupt an in-flight channel send.
	sendCtx, cancel := context.WithTimeout(context.Background(), progressSendTimeout)
	defer cancel()
	if err := f.req.Sender.SendProgress(sendCtx, msg); err != nil {
		slog.Warn("progress delivery failed",
			"kind", evt.Kind, "event_id", evt.ID, "error", err)
	}
}

// drain stops further progress delivery: the final answer is imminent. The
// sender goroutine drops queued items; the silence timer is stopped.
func (f *Forwarder) drain() {
	f.mu.Lock()
	f.drained = true
	f.mu.Unlock()
	f.stopSilence()
}

func (f *Forwarder) scheduleSilence() {
	if f.req.Policy.InitialSilenceThreshold <= 0 {
		return
	}
	f.mu.Lock()
	f.silenceTimer = time.AfterFunc(f.req.Policy.InitialSilenceThreshold, f.enqueueSilence)
	f.mu.Unlock()
}

// enqueueSilence fires once after the initial silence threshold. It only sends
// a working notice if nothing has been delivered yet and the final has not
// arrived (§9.1). It refuses to enqueue once shut down.
func (f *Forwarder) enqueueSilence() {
	if f.ctx.Err() != nil {
		return
	}
	f.mu.Lock()
	sent := f.progressSent
	drained := f.drained
	f.mu.Unlock()
	if sent > 0 || drained {
		return
	}
	evt := runtime.RuntimeEvent{ID: "silence-notice", Kind: "run.silence_notice"}
	if !f.enqueue(evt) {
		slog.Warn("progress forwarder queue full; dropping silence notice")
	}
}

// eventQueuePriority ranks a forwarder-queue event by delivery reliability,
// mirroring the bus contract. Only run.waiting_human is critical (must reach
// the user); the delegate progress kinds are important; the synthetic silence
// notice and anything else are droppable. This is what prevents a delegate
// progress burst from filling the queue and dropping a waiting_human that
// arrives after it (§8.4, §9.1).
func eventQueuePriority(evt runtime.RuntimeEvent) int {
	switch evt.Kind {
	case "run.waiting_human":
		return 2 // critical
	case "agent.delegate.failed", "agent.delegate.timeout":
		return 1 // important
	default:
		return 0 // droppable
	}
}

func (f *Forwarder) enqueue(evt runtime.RuntimeEvent) bool {
	f.queueMu.Lock()
	defer f.queueMu.Unlock()
	if f.queueClosed {
		return false
	}
	if len(f.queue) < queueCapacity {
		f.appendLocked(evt)
		return true
	}
	// Queue full: evict the oldest buffered event STRICTLY lower priority than
	// the incoming one, so critical (waiting_human) always wins over important/
	// droppable and important wins over droppable. A droppable incoming event
	// evicts nothing and is itself dropped. Never silent.
	incoming := eventQueuePriority(evt)
	for i, existing := range f.queue {
		if eventQueuePriority(existing) < incoming {
			evicted := f.queue[i]
			f.queue = append(f.queue[:i], f.queue[i+1:]...)
			f.appendLocked(evt)
			slog.Warn("progress forwarder queue full; evicted lower-priority event",
				"evicted_kind", evicted.Kind, "evicted_event_id", evicted.ID,
				"kind", evt.Kind, "event_id", evt.ID)
			return true
		}
	}
	slog.Warn("progress forwarder queue full; dropping event",
		"kind", evt.Kind, "event_id", evt.ID)
	return false
}

func (f *Forwarder) appendLocked(evt runtime.RuntimeEvent) {
	f.queue = append(f.queue, evt)
	select {
	case f.queueCh <- struct{}{}:
	default:
	}
}

func (f *Forwarder) closeQueue() {
	f.queueMu.Lock()
	defer f.queueMu.Unlock()
	if f.queueClosed {
		return
	}
	f.queueClosed = true
	// Wake a blocked dequeue so it observes closure and can drain the remainder.
	select {
	case f.queueCh <- struct{}{}:
	default:
	}
}

func (f *Forwarder) stopSilence() {
	f.mu.Lock()
	if f.silenceTimer != nil {
		f.silenceTimer.Stop()
		f.silenceTimer = nil
	}
	f.mu.Unlock()
}
