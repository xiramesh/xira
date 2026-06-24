package runtime

import "context"

// event_bus.go: EventBus is the interface for per-chat-key event delivery
// (per-chat-key RFC #48). ChatContext (in channelrunner/progress) implements
// it. The recordEvent closure delivers mapped Events to the sink via
// context.Value — no global bus subscription needed.
//
// This breaks the circular dependency: runtime defines EventBus (interface),
// progress implements it (ChatContext), runner passes it via context.

// EventBus receives an Event for per-chat-key delivery (render + throttle +
// send to IM). Implementations: ChatContext.
type EventBus interface {
	Deliver(evt Event)
}

type eventBusKey struct{}

// WithEventBus returns a context carrying the EventBus. The recordEvent
// closure retrieves it via EventBusFromContext and delivers mapped Events
// to the per-chat-key ChatContext (the only delivery path — the global
// per-Service EventBus was removed in Phase 6b, #60).
func WithEventBus(ctx context.Context, bus EventBus) context.Context {
	return context.WithValue(ctx, eventBusKey{}, bus)
}

// EventBusFromContext extracts the EventBus, or nil if absent.
func EventBusFromContext(ctx context.Context) EventBus {
	bus, _ := ctx.Value(eventBusKey{}).(EventBus)
	return bus
}
