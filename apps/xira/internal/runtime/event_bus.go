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

// RawEventSink receives the flat, unmapped RuntimeEvent (carrying scope/
// payload/runID — everything) for a channel to render itself. This is the
// "pass-through, channel decides rendering" contract (RFC chatkey-session):
// channelrunner hands the channel the raw event; the channel decides how to
// present it (feishu emoji/card, ilink text via IMEventRenderer, ws frame).
//
// It exists IN PARALLEL to EventBus (which carries the sealed, scope-stripped
// Event for ChatContext's text rendering). A channel opts into one or the
// other — not both — to avoid double-delivery. dispatchEvent fans out to both
// sinks when present.
type RawEventSink interface {
	DeliverRaw(evt RuntimeEvent)
}

type rawEventSinkKey struct{}

// WithRawEventSink returns a context carrying the RawEventSink. A channel that
// wants to render events itself injects this; dispatchEvent will hand it the
// flat RuntimeEvent before/alongside the sealed-Event EventBus delivery.
func WithRawEventSink(ctx context.Context, sink RawEventSink) context.Context {
	return context.WithValue(ctx, rawEventSinkKey{}, sink)
}

// RawEventSinkFromContext extracts the RawEventSink, or nil if absent.
func RawEventSinkFromContext(ctx context.Context) RawEventSink {
	sink, _ := ctx.Value(rawEventSinkKey{}).(RawEventSink)
	return sink
}

// rawEventSinkFunc adapts a function to RawEventSink (mirrors SenderFunc /
// wschannel patterns — lets a channel inject a closure without a named type).
type rawEventSinkFunc func(RuntimeEvent)

func (f rawEventSinkFunc) DeliverRaw(evt RuntimeEvent) { f(evt) }

// RawEventSinkFunc wraps f as a RawEventSink. Exported so channels can build a
// sink from a closure (ChatKeySessionConfig.OnRawEvent uses this internally).
func RawEventSinkFunc(f func(RuntimeEvent)) RawEventSink { return rawEventSinkFunc(f) }
