package runtime

import "context"

// event_sink.go: EventSink is the interface for per-chat-key event delivery
// (per-chat-key RFC #48). ChatContext (in channelrunner/progress) implements
// it. The recordEvent closure delivers mapped Events to the sink via
// context.Value — no global bus subscription needed.
//
// This breaks the circular dependency: runtime defines EventSink (interface),
// progress implements it (ChatContext), runner passes it via context.

// EventSink receives an Event for per-chat-key delivery (render + throttle +
// send to IM). Implementations: ChatContext.
type EventSink interface {
	Deliver(evt Event)
}

type eventSinkKey struct{}

// WithEventSink returns a context carrying the EventSink. The recordEvent
// closure retrieves it via EventSinkFromContext and delivers mapped Events
// directly — bypassing the global EventBus for per-chat-key progress.
func WithEventSink(ctx context.Context, sink EventSink) context.Context {
	return context.WithValue(ctx, eventSinkKey{}, sink)
}

// EventSinkFromContext extracts the EventSink, or nil if absent.
func EventSinkFromContext(ctx context.Context) EventSink {
	sink, _ := ctx.Value(eventSinkKey{}).(EventSink)
	return sink
}
