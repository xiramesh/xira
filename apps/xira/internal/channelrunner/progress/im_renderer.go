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
// RenderEvent) + quota (MaxMessagesPerTurn anti-flood) + dedup (kind+text).
//
// Why this exists (RFC chatkey-session): the old ChatContext forced every
// channel through render→text. That stripped the channel of the choice of how
// to present an event (feishu emoji/card, ilink text, ws frame). IMEventRenderer
// moves that decision to the CHANNEL: ilink/feishu opt in by injecting it
// (getting the same text+quota behavior they had before — zero behavior
// change), while ws injects its own frame-writer instead. channelrunner only
// passes information; the channel decides presentation.
//
// ilink/feishu wire it via ChatKeySessionConfig.OnRawEvent:
//
//	imRenderer := progress.NewIMEventRenderer(func(ctx, text) error { return r.send(...) }, progress.DefaultPolicy())
//	cfg.OnRawEvent = imRenderer.DeliverRaw
//
// Lifecycle: one IMEventRenderer per active turn (its quota/dedup state is
// per-turn). Reset between steering retries is NOT needed — the renderer is
// constructed fresh each turn (same as ChatContext was).

// IMEventRenderer renders raw RuntimeEvents into IM text, with the anti-flood
// quota and dedup that ChatContext previously enforced. Implements RawEventSink.
type IMEventRenderer struct {
	send     func(ctx context.Context, text string) error
	maxChars int
	policy   Policy

	// per-turn state
	mu           sync.Mutex
	progressSent int
	dedup        map[string]struct{}
}

// NewIMEventRenderer constructs a renderer that sends localized text via send
// and enforces policy's MaxMessagesPerTurn + dedup. maxChars truncation comes
// from policy.MaxChars. One instance per active turn.
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
	}
}

// DeliverRaw implements runtime.RawEventSink. It maps the flat RuntimeEvent to
// the sealed Event, renders to text (RenderEvent), enforces quota + dedup, and
// sends. Mirrors ChatContext.dispatch 1:1 — the behavior ilink/feishu rely on.
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

	r.mu.Lock()
	// Quota: progress events capped at MaxMessagesPerTurn; interaction bypasses.
	// KNOWN LIMITATION (carried from ChatContext): quota is shared across a
	// parent turn and its spawned children (child events route to the same
	// renderer). A chatty child can starve the parent. The Debug log below
	// keeps the drop observable (AGENTS.md §2: no silent data loss).
	if !isWaiting && r.policy.MaxMessagesPerTurn > 0 && r.progressSent >= r.policy.MaxMessagesPerTurn {
		r.mu.Unlock()
		slog.Debug("im renderer progress quota reached; dropping event",
			"kind", event.Kind(), "event_id", event.ID(),
			"agent_turn", event.AgentTurnID(), "parent_turn", event.ParentAgentTurnID(),
			"sent", r.progressSent, "cap", r.policy.MaxMessagesPerTurn)
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
		r.progressSent++
	}
	r.mu.Unlock()

	// Send with a timeout independent of the turn ctx (mirrors ChatContext).
	sendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.send(sendCtx, msg.Text); err != nil {
		slog.Warn("im renderer send failed",
			"kind", event.Kind(), "event_id", event.ID(), "error", err)
	}
}
