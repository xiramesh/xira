package progress

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// im_renderer.go: IMEventRenderer is the reusable IM-channel event renderer.
// It receives a flat runtime.RuntimeEvent (via RawEventSink — channelrunner
// hands it the raw event, NOT a pre-rendered text) and produces the IM-text
// behavior that ChatContext used to bake in: render to localized text (via
// RenderEvent) + parent/child progress quotas + dedup (kind+text).
//
// Why this exists (RFC chatkey-session): the old ChatContext forced every
// channel through render→text. That stripped the channel of the choice of how
// to present an event (feishu emoji/card, ilink text, ws frame). IMEventRenderer
// moves that decision to the CHANNEL: ilink/feishu opt in by injecting it
// (getting the same text+quota behavior they had before — zero behavior
// change), while ws injects its own frame-writer instead. channelrunner only
// passes information; the channel decides presentation.
//
// Async model (mirrors ChatContext): DeliverRaw does NOT call send inline.
// It renders + applies quota/dedup under a lock, then enqueues the text onto a
// FIFO queue; a dedicated sendLoop goroutine drains the queue in order. This is
// critical: DeliverRaw is called inline by dispatchEvent (from the tool/LLM
// execution loop), so a blocking send would stall the turn; and per-send
// goroutines would reorder IM messages. The single sendLoop preserves order
// AND keeps the turn unblocked (PR #94 review WARNING #2).
//
// ilink/feishu wire it via ChatKeySessionConfig.OnRawEvent:
//
//	imRenderer := progress.NewIMEventRenderer(func(ctx, text) error { return r.send(...) }, progress.DefaultPolicy())
//	cfg.OnRawEvent = imRenderer.DeliverRaw
//
// Lifecycle: one IMEventRenderer per active turn. Call Stop() at turn end to
// drain + release the sendLoop (mirrors ChatContext.Stop).

// IMEventRenderer renders raw RuntimeEvents into IM text, with the anti-flood
// quota and dedup that ChatContext previously enforced. Implements RawEventSink.
type IMEventRenderer struct {
	send     func(ctx context.Context, text string) error
	maxChars int
	policy   Policy

	// per-turn render state
	mu                 sync.Mutex
	parentProgressSent int
	childProgressSent  int
	dedup              map[string]struct{}

	// ordered send queue + sendLoop (mirrors ChatContext)
	queueMu   sync.Mutex
	queue     []string
	queueCh   chan struct{}
	queueDone bool
	wg        sync.WaitGroup
}

// NewIMEventRenderer constructs a renderer that sends localized text via send
// and enforces policy's parent/child progress quotas + dedup. maxChars
// truncation comes from policy.MaxChars. One instance per active turn; caller
// MUST call Start() before delivering events and Stop() at turn end.
func NewIMEventRenderer(send func(ctx context.Context, text string) error, policy Policy) *IMEventRenderer {
	maxChars := 0
	if policy.MaxChars > 0 {
		maxChars = policy.MaxChars
	}
	return &IMEventRenderer{
		send:     send,
		maxChars: maxChars,
		policy:   policy,
		dedup:    make(map[string]struct{}),
		queueCh:  make(chan struct{}, 1),
	}
}

// Start launches the sendLoop goroutine. Must be called before DeliverRaw.
func (r *IMEventRenderer) Start() {
	if r == nil || r.send == nil {
		return
	}
	r.wg.Add(1)
	go r.sendLoop()
}

// Stop drains the queue and stops the sendLoop. Safe to call once; idempotent
// via queueDone. Mirrors ChatContext.Stop's "wait for in-flight" contract.
func (r *IMEventRenderer) Stop() {
	if r == nil {
		return
	}
	r.queueMu.Lock()
	r.queueDone = true
	r.queueMu.Unlock()
	select {
	case r.queueCh <- struct{}{}:
	default:
	}
	r.wg.Wait()
}

// waitSends blocks until the sendLoop has flushed all currently-queued sends
// (bounded wait). For tests that need to observe delivered text after feeding
// events; production uses Stop() at turn end.
func (r *IMEventRenderer) waitSends() {
	// Drain by waiting for the queue to empty. Poll briefly — tests only.
	for i := 0; i < 200; i++ {
		r.queueMu.Lock()
		empty := len(r.queue) == 0
		r.queueMu.Unlock()
		if empty {
			// give the active send a moment to record into the capture
			time.Sleep(5 * time.Millisecond)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// DeliverRaw implements runtime.RawEventSink. It maps the flat RuntimeEvent to
// the sealed Event, renders to text (RenderEvent), enforces quota + dedup, and
// ENQUEUES the text (non-blocking — the sendLoop sends in order). Mirrors
// ChatContext.dispatch's governance 1:1.
func (r *IMEventRenderer) DeliverRaw(evt runtime.RuntimeEvent) {
	if r == nil || r.send == nil {
		return
	}
	// Map flat → sealed Event (the form RenderEvent expects). Non-signal kinds
	// return ok=false and are skipped here (they were already slog'd in
	// dispatchEvent before reaching this sink).
	event, ok := runtime.EventFromRuntime(evt)
	if !ok {
		return
	}
	msg, renderOK := RenderEvent(event, r.maxChars)
	if !renderOK {
		// Lifecycle/AssistantFinal/ToolResult etc. — not rendered (matches
		// ChatContext.dispatch's `if !ok { return }`).
		return
	}

	isWaiting := event.Kind() == "human.requested"

	text := ""
	r.mu.Lock()
	child := isChildEvent(event)
	limit := progressQuotaLimit(r.policy, child)
	sent := r.parentProgressSent
	bucket := "parent"
	if child {
		sent = r.childProgressSent
		bucket = "child"
	}
	if !isWaiting && limit > 0 && sent >= limit {
		r.mu.Unlock()
		slog.Debug("im renderer progress quota reached; dropping event",
			"kind", event.Kind(), "event_id", event.ID(),
			"agent_turn", event.AgentTurnID(), "parent_turn", event.ParentAgentTurnID(),
			"bucket", bucket, "sent", sent, "cap", limit)
		return
	}
	// Dedup: same kind + rendered text.
	dedupKey := event.Kind() + "|" + msg.Text
	if _, dup := r.dedup[dedupKey]; dup {
		r.mu.Unlock()
		return
	}
	r.dedup[dedupKey] = struct{}{}
	if !isWaiting {
		if child {
			r.childProgressSent++
		} else {
			r.parentProgressSent++
		}
	}
	text = msg.Text
	r.mu.Unlock()

	// Enqueue for ordered async send (NOT a per-send goroutine — that would
	// reorder IM messages). The sendLoop drains FIFO.
	r.queueMu.Lock()
	if r.queueDone {
		r.queueMu.Unlock()
		return
	}
	r.queue = append(r.queue, text)
	r.queueMu.Unlock()
	select {
	case r.queueCh <- struct{}{}:
	default:
	}
}

// sendLoop drains the queue in FIFO order, sending each text. Single goroutine
// → preserves message ordering (a per-send goroutine would reorder). Runs until
// Stop() sets queueDone and signals.
func (r *IMEventRenderer) sendLoop() {
	defer r.wg.Done()
	for {
		r.queueMu.Lock()
		if len(r.queue) > 0 {
			text := r.queue[0]
			r.queue = r.queue[1:]
			r.queueMu.Unlock()
			sendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := r.send(sendCtx, text); err != nil {
				slog.Warn("im renderer send failed", "error", err)
			}
			cancel()
			continue
		}
		if r.queueDone {
			r.queueMu.Unlock()
			return
		}
		r.queueMu.Unlock()
		<-r.queueCh
	}
}
