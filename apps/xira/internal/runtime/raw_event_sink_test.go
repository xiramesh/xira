package runtime

import (
	"context"
	"testing"
)

// raw_event_sink_test.go: tests the RawEventSink passthrough in dispatchEvent.
// This is the contract that lets channels receive raw (unrendered, scope-bearing)
// RuntimeEvents — channelrunner only passes information; the channel renders.

// captureSink records every RuntimeEvent delivered to it.
type captureSink struct {
	events []RuntimeEvent
}

func (c *captureSink) DeliverRaw(evt RuntimeEvent) {
	c.events = append(c.events, evt)
}

// TestDispatchEventDeliversRawToSink: a signal event is delivered to the
// RawEventSink with its FULL flat form (scope/payload intact), in parallel to
// the EventBus. This is the "原封不动穿透" contract.
func TestDispatchEventDeliversRawToSink(t *testing.T) {
	sink := &captureSink{}
	evt := RuntimeEvent{
		ID:     "evt-1",
		Kind:   "agent.delegate.failed",
		RunID:  "run-1",
		Payload: map[string]any{"error": "boom"},
	}
	ctx := WithRawEventSink(context.Background(), sink)
	dispatchEvent(ctx, evt)

	if len(sink.events) != 1 {
		t.Fatalf("RawEventSink got %d events, want 1", len(sink.events))
	}
	got := sink.events[0]
	if got.ID != "evt-1" || got.Kind != "agent.delegate.failed" {
		t.Errorf("delivered event = %+v, want the original flat event", got)
	}
	// Scope/payload must be intact (not stripped — this is the whole point of
	// raw passthrough vs the sealed Event which loses them).
	if got.Payload["error"] != "boom" {
		t.Errorf("payload lost: %+v", got.Payload)
	}
}

// TestDispatchEventNoRawSinkIsNoop: when no RawEventSink in ctx, dispatchEvent
// must not panic (and still delivers to EventBus if present).
func TestDispatchEventNoRawSinkIsNoop(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked with no RawEventSink: %v", r)
		}
	}()
	evt := RuntimeEvent{ID: "e1", Kind: "agent.delegate.failed"}
	dispatchEvent(context.Background(), evt) // no sink, no panic
}

// TestDispatchEventRawSinkSkipsNonSignal: non-signal (observability) kinds are
// NOT delivered to RawEventSink (they go to slog only). Only signal kinds reach
// the sink — matching what channels render.
func TestDispatchEventRawSinkSkipsNonSignal(t *testing.T) {
	sink := &captureSink{}
	ctx := WithRawEventSink(context.Background(), sink)
	// "llm.call.completed" is a non-signal observability kind.
	dispatchEvent(ctx, RuntimeEvent{ID: "obs-1", Kind: "llm.call.completed"})
	if len(sink.events) != 0 {
		t.Errorf("RawEventSink got non-signal event: %+v (want skipped)", sink.events)
	}
}

// TestDispatchEventDeliversToBothSinks: when both RawEventSink and EventBus are
// wired, the signal event reaches BOTH (parallel fan-out). A channel that
// injects both gets double-delivery — documented as the channel's choice.
func TestDispatchEventDeliversToBothSinks(t *testing.T) {
	rawSink := &captureSink{}
	busSink := &captureEventBus{}
	ctx := WithRawEventSink(context.Background(), rawSink)
	ctx = WithEventBus(ctx, busSink)
	dispatchEvent(ctx, RuntimeEvent{ID: "e1", Kind: "agent.delegate.failed"})

	if len(rawSink.events) != 1 {
		t.Errorf("RawEventSink got %d, want 1", len(rawSink.events))
	}
	if len(busSink.events) != 1 {
		t.Errorf("EventBus got %d, want 1", len(busSink.events))
	}
}

type captureEventBus struct{ events []Event }

func (c *captureEventBus) Deliver(evt Event) { c.events = append(c.events, evt) }

// TestRawEventSinkContextHelpers: With/From round-trip + nil when absent.
func TestRawEventSinkContextHelpers(t *testing.T) {
	if got := RawEventSinkFromContext(context.Background()); got != nil {
		t.Errorf("absent sink = %v, want nil", got)
	}
	sink := &captureSink{}
	ctx := WithRawEventSink(context.Background(), sink)
	if got := RawEventSinkFromContext(ctx); got != RawEventSink(sink) {
		t.Errorf("FromContext round-trip mismatch")
	}
}

// TestRawEventSinkFuncWraps: RawEventSinkFunc adapts a closure.
func TestRawEventSinkFuncWraps(t *testing.T) {
	var got RuntimeEvent
	s := RawEventSinkFunc(func(evt RuntimeEvent) { got = evt })
	evt := RuntimeEvent{ID: "x", Kind: "run.started"}
	s.DeliverRaw(evt)
	if got.ID != "x" {
		t.Errorf("func sink got %+v, want x", got)
	}
}
