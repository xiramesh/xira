package runtime

import (
	"context"
	"log/slog"
)

// event_dispatch.go: dispatchEvent is the event delivery chokepoint. Called
// from ALL recordEvent/recordChildEvent closures. Maps a RuntimeEvent to a
// typed Event and delivers to the per-chat-key EventBus (ChatContext) via
// context.Value. The global EventBus was removed (Phase 6b, #56) —
// per-chat-key routing is the only delivery path.
//
// Non-signal kinds (observability/audit, ~34 kinds) are slog.Debug'd until #43
// extracts them formally. recorder.appendEvent still happens in the closure
// BEFORE this function — run history (for session hydrate) is unaffected.

func dispatchEvent(ctx context.Context, evt RuntimeEvent) {
	event, ok := runtimeEventToEvent(evt)
	if !ok {
		// Non-signal kind (observability/audit/internal, ~34 of them). These are
		// run-lifecycle facts worth observing (tool started, llm traced, etc.),
		// not conversation signals — so they go to structured slog (level Info,
		// matching service.go's tool/session observability), NOT the EventBus.
		// #43: the payload is included so the observation is actually useful
		// (previously only kind/id/run/source were logged, payload was dropped).
		slog.Info("runtime event (non-signal, observability)",
			"kind", evt.Kind,
			"event_id", evt.ID,
			"run_id", evt.RunID,
			"source", evt.Source,
			"payload", evt.Payload)
		return
	}
	// Raw passthrough (RFC chatkey-session): hand the flat RuntimeEvent (with
	// scope/payload) to a channel that renders itself (feishu emoji/card, ws
	// frame, ilink via IMEventRenderer). Delivered IN PARALLEL to the EventBus
	// below — a channel opts into one sink, not both. Only signal events
	// (those that map to a sealed Event) are delivered raw, matching what
	// channels render; non-signal observability kinds stay slog-only above.
	if rawSink := RawEventSinkFromContext(ctx); rawSink != nil {
		rawSink.DeliverRaw(evt)
	}
	if sink := EventBusFromContext(ctx); sink != nil {
		sink.Deliver(event)
	} else {
		// No per-chat-key sink in ctx (e.g. detached child turn, or a path
		// that doesn't wire EventBus). Symmetric with the non-signal log above
		// — never silently drop a signal without a trace. Debug (not Info):
		// this is a degraded path (no sink), not a normal observability fact.
		slog.Debug("signal event dropped (no EventBus in context)",
			"kind", evt.Kind,
			"event_id", evt.ID,
			"run_id", evt.RunID,
			"source", evt.Source)
	}
}
