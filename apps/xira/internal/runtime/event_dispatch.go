package runtime

import (
	"context"
	"log/slog"
)

// event_dispatch.go: dispatchEvent is the event publication chokepoint.
// Called from every recordEvent/recordChildEvent closure. Replaces the old
// `s.events.Publish(evt)` with a three-way dispatch:
//
//  1. Map RuntimeEvent → Event (runtimeEventToEvent). If mappable (signal):
//     a. Deliver to EventSink (per-chat-key, if present in ctx) — direct,
//        no global bus needed.
//     b. Also Publish on the global EventBus (for WS observers / deprecated
//        bridge) — until global bus is retired in Phase 6.
//  2. If NOT mappable (observability/audit, ~34 kinds): slog.Debug.
//     Temporary until #43 extracts them formally.
//
// recorder.appendEvent still happens in the closure BEFORE this function —
// run history (for session hydrate) is unaffected.

func dispatchEvent(ctx context.Context, bus EventBus, evt RuntimeEvent) {
	event, ok := runtimeEventToEvent(evt)
	if !ok {
		// Non-signal kind — temporary slog path (#43 extracts formally).
		slog.Debug("runtime event (non-signal, not on bus)",
			"kind", evt.Kind,
			"event_id", evt.ID,
			"run_id", evt.RunID,
			"source", evt.Source)
		return
	}

	// Per-chat-key delivery: direct to EventSink if present.
	if sink := EventSinkFromContext(ctx); sink != nil {
		sink.Deliver(event)
	}

	// Global bus: dual publish for backward compat during migration.
	// PublishEvent (new Event-typed) for esubs + Publish (deprecated RuntimeEvent)
	// for old subscribers (WS observers, tests). Phase 6 removes the old Publish.
	bus.PublishEvent(event)
	bus.Publish(evt) // deprecated bridge — keeps WS observers working
}
