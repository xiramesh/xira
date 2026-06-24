package progress

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// chatcontext.go: ChatContext is the per-chat-key event delivery context
// (per-chat-key RFC #48). It replaces Forwarder — events are delivered
// DIRECTLY (not via global bus subscription), rendered, throttle/dedupe/quota
// applied, and sent via the Sender.
//
// Key difference from Forwarder:
//   - No scopeMatcher (per-chat-key isolation is natural)
//   - No global bus subscription (events arrive via Deliver(), not a channel
//     from EventBus.Subscribe)
//   - Simpler lifecycle (one goroutine for send, no separate consumeLoop)
//
// Same as Forwarder:
//   - Queue with eviction (burst absorber)
//   - Throttle / dedup / quota
//   - AssistantFinal → drain
//   - Stop() waits for in-flight

const chatContextQueueCapacity = 256

// ChatContextConfig configures a ChatContext.
type ChatContextConfig struct {
	Sender   Sender
	MaxChars int
	Policy   Policy
}

// ChatContext receives events for one chat key, renders them, and delivers
// progress to the Sender. One instance per active turn.
type ChatContext struct {
	sender   Sender
	maxChars int
	policy   Policy

	ctx    context.Context
	cancel context.CancelFunc

	// queue is the burst absorber between Deliver() and the send goroutine.
	queueMu     sync.Mutex
	queue       []runtime.Event
	queueCh     chan struct{}
	queueClosed bool

	senderWg sync.WaitGroup
	stopOnce sync.Once

	// throttle/dedupe/quota state (protected by mu)
	mu           sync.Mutex
	progressSent int
	dedup        map[string]struct{}
	drained      bool
}

// NewChatContext creates a ChatContext. If Sender is nil, the context is
// a no-op (like Forwarder disabled).
func NewChatContext(parent context.Context, cfg ChatContextConfig) *ChatContext {
	cc := &ChatContext{
		sender:   cfg.Sender,
		maxChars: cfg.MaxChars,
		policy:   cfg.Policy,
		queue:    make([]runtime.Event, 0, chatContextQueueCapacity),
		queueCh:  make(chan struct{}, 1),
		dedup:    make(map[string]struct{}),
	}
	cc.ctx, cc.cancel = context.WithCancel(parent)
	return cc
}

// Start spawns the send goroutine. Must be called before Deliver.
func (cc *ChatContext) Start() {
	if cc.sender == nil {
		return
	}
	cc.senderWg.Add(1)
	go cc.sendLoop()
}

// Deliver hands an event to this ChatContext for rendering and delivery.
// Non-blocking: if the queue is full, low-priority events are evicted.
// Safe for concurrent use (called by the turn's event dispatch).
func (cc *ChatContext) Deliver(evt runtime.Event) {
	if cc == nil || cc.sender == nil {
		return
	}

	// Check drained FIRST: if AssistantFinal already arrived, drop everything
	// after it. This check is in Deliver (not dispatch) so events already in
	// the queue before drain are NOT affected — they were accepted before
	// drain and should still be delivered.
	cc.mu.Lock()
	if cc.drained {
		cc.mu.Unlock()
		return
	}
	cc.mu.Unlock()

	// AssistantFinal → set drain flag (events already queued are unaffected).
	if _, ok := evt.(runtime.AssistantFinal); ok {
		cc.mu.Lock()
		cc.drained = true
		cc.mu.Unlock()
		return
	}

	cc.queueMu.Lock()
	if cc.queueClosed {
		cc.queueMu.Unlock()
		return
	}
	if len(cc.queue) < chatContextQueueCapacity {
		cc.queue = append(cc.queue, evt)
		cc.queueMu.Unlock()
		select {
		case cc.queueCh <- struct{}{}:
		default:
		}
		return
	}
	// Queue full — priority eviction.
	evicted := cc.evictFor(evt)
	if evicted {
		cc.queue = append(cc.queue, evt)
	}
	cc.queueMu.Unlock()
	select {
	case cc.queueCh <- struct{}{}:
	default:
	}
	if !evicted {
		slog.Warn("chat context queue full; dropping event",
			"kind", evt.Kind(), "event_id", evt.ID())
	}
}

// evictFor tries to make room by evicting a lower-priority event.
// Returns true if room was made. Caller must hold queueMu.
func (cc *ChatContext) evictFor(incoming runtime.Event) bool {
	incomingPri := incoming.Priority()
	for i, existing := range cc.queue {
		if existing.Priority() < incomingPri {
			cc.queue = append(cc.queue[:i], cc.queue[i+1:]...)
			slog.Warn("chat context queue full; evicted lower-priority event",
				"evicted_kind", existing.Kind(),
				"evicted_event_id", existing.ID(),
				"kind", incoming.Kind(),
				"event_id", incoming.ID())
			return true
		}
	}
	return false
}

// sendLoop drains the queue and delivers rendered events.
func (cc *ChatContext) sendLoop() {
	defer cc.senderWg.Done()
	for {
		evt, ok := cc.dequeue()
		if !ok {
			return
		}
		cc.dispatch(evt)
	}
}

func (cc *ChatContext) dequeue() (runtime.Event, bool) {
	for {
		cc.queueMu.Lock()
		if len(cc.queue) > 0 {
			evt := cc.queue[0]
			cc.queue = cc.queue[1:]
			cc.queueMu.Unlock()
			return evt, true
		}
		if cc.queueClosed {
			cc.queueMu.Unlock()
			return nil, false
		}
		cc.queueMu.Unlock()
		select {
		case <-cc.queueCh:
		case <-cc.ctx.Done():
			// Context canceled — drain remaining then exit.
			cc.queueMu.Lock()
			cc.queueClosed = true
			remaining := cc.queue
			cc.queue = nil
			cc.queueMu.Unlock()
			for _, e := range remaining {
				cc.dispatch(e)
			}
			return nil, false
		}
	}
}

func (cc *ChatContext) dispatch(evt runtime.Event) {
	msg, ok := RenderEvent(evt, cc.maxChars)
	if !ok {
		return
	}

	cc.mu.Lock()
	// Note: drained is checked in Deliver(), not here. Events already in the
	// queue were accepted before drain and should still be delivered.
	isWaiting := evt.Kind() == "human.requested"

	// Quota: progress events limited, interaction bypasses.
	//
	// KNOWN LIMITATION: the quota is shared across the parent turn AND any
	// spawned children (child events route to the same ChatContext, RFC #66).
	// A chatty child can starve the parent's progress within the per-turn cap.
	// This is acceptable for v0 (the cap is what prevents IM flooding), but a
	// per-source quota split is a follow-up design. The Debug log below makes
	// the drop observable for debugging rather than silent (AGENTS.md §2:
	// silent data loss is the most expensive bug — even intentional drops
	// deserve a trace).
	if !isWaiting && cc.policy.MaxMessagesPerTurn > 0 && cc.progressSent >= cc.policy.MaxMessagesPerTurn {
		cc.mu.Unlock()
		slog.Debug("chat context progress quota reached; dropping event",
			"kind", evt.Kind(), "event_id", evt.ID(),
			"agent_turn", evt.AgentTurnID(), "parent_turn", evt.ParentAgentTurnID(),
			"sent", cc.progressSent, "cap", cc.policy.MaxMessagesPerTurn)
		return
	}

	// Dedup: same kind + text.
	dedupKey := evt.Kind() + "|" + msg.Text
	if _, dup := cc.dedup[dedupKey]; dup {
		cc.mu.Unlock()
		return
	}

	cc.dedup[dedupKey] = struct{}{}
	if !isWaiting {
		cc.progressSent++
	}
	cc.mu.Unlock()

	// Deliver with timeout independent of ctx (like Forwarder).
	sendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cc.sender.SendProgress(sendCtx, msg); err != nil {
		slog.Warn("chat context send failed",
			"kind", evt.Kind(), "event_id", evt.ID(), "error", err)
	}
}

// Stop signals shutdown and waits for in-flight events to be delivered.
func (cc *ChatContext) Stop() {
	cc.stopOnce.Do(func() {
		cc.queueMu.Lock()
		cc.queueClosed = true
		cc.queueMu.Unlock()
		select {
		case cc.queueCh <- struct{}{}:
		default:
		}
		cc.senderWg.Wait()
		cc.cancel()
	})
}

// Reset clears ALL state for a steering retry: progress count, dedup keys,
// drain flag, AND the async queue (PR #51 round 4 review: without clearing
// the queue, residual progress from the first run continues delivering
// during retry, causing dedup gaps and quota miscount).
//
// This is a synchronous reset: it stops the sendLoop, drains the queue,
// clears state, then restarts the sendLoop. Safe to call between runs.
//
// PR #51 round 5 CRITICAL 1 fix: must signal queueCh + cancel ctx so
// sendLoop's select unblocks. Without these, senderWg.Wait() hangs forever
// (deterministic deadlock, reviewer reproduced 5/5).
func (cc *ChatContext) Reset() {
	// Signal sendLoop to exit: cancel ctx (dequeue's ctx.Done branch) AND
	// signal queueCh (dequeue's queueCh branch). Both are needed because
	// sendLoop may be blocked on either one.
	cc.cancel() // unblock dequeue's <-ctx.Done()

	cc.queueMu.Lock()
	cc.queueClosed = true
	cc.queue = nil
	cc.queueMu.Unlock()
	select {
	case cc.queueCh <- struct{}{}: // unblock dequeue's <-queueCh
	default:
	}

	cc.senderWg.Wait() // now safe — sendLoop WILL exit

	// Clear throttle/dedupe/quota state.
	cc.mu.Lock()
	cc.progressSent = 0
	cc.dedup = make(map[string]struct{})
	cc.drained = false
	cc.mu.Unlock()

	// Restart for the next run.
	cc.queueMu.Lock()
	cc.queueClosed = false
	cc.queue = make([]runtime.Event, 0, chatContextQueueCapacity)
	cc.queueMu.Unlock()
	cc.stopOnce = sync.Once{} // allow Stop() to work again
	cc.ctx, cc.cancel = context.WithCancel(context.Background())
	if cc.sender != nil {
		cc.senderWg.Add(1)
		go cc.sendLoop()
	}
}
