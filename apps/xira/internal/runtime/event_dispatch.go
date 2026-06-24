package runtime

import (
	"context"
	"log/slog"
)

// event_dispatch.go: dispatchEvent is the event delivery chokepoint. Called
// from ALL recordEvent/recordChildEvent closures. Maps a RuntimeEvent to a
// typed Event and delivers to the per-chat-key EventSink (ChatContext) via
// context.Value. The global EventBus was removed (Phase 6b, #56) —
// per-chat-key routing is the only delivery path.
//
// Non-signal kinds (observability/audit, ~34 kinds) are slog.Debug'd until #43
// extracts them formally. recorder.appendEvent still happens in the closure
// BEFORE this function — run history (for session hydrate) is unaffected.

func dispatchEvent(ctx context.Context, evt RuntimeEvent) {
	event, ok := runtimeEventToEvent(evt)
	if !ok {
		slog.Debug("runtime event (non-signal, not delivered to sink)",
			"kind", evt.Kind,
			"event_id", evt.ID,
			"run_id", evt.RunID,
			"source", evt.Source)
		return
	}
	if sink := EventSinkFromContext(ctx); sink != nil {
		sink.Deliver(event)
	} else {
		// No per-chat-key sink in ctx (e.g. detached child turn, or a path
		// that doesn't wire EventSink). Symmetric with the non-signal Debug
		// log above — never silently drop a signal without a trace.
		slog.Debug("signal event dropped (no EventSink in context)",
			"kind", evt.Kind,
			"event_id", evt.ID,
			"run_id", evt.RunID,
			"source", evt.Source)
	}
}
