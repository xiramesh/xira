package runtime

import "context"

// steering_sink.go: SteeringSink is the per-chat-key steering queue interface
// (Phase 4, RFC #48 §5). Channel runner enqueues user interjections;
// generateADK's event loop dequeues them between iterations (checkpoint).
//
// Same context.Value pattern as EventSink — runtime defines interface,
// progress implements (SteeringQueue), runner passes via context.

// SteeringSink collects user messages sent while a turn is running.
// Implemented by progress.SteeringQueue.
type SteeringSink interface {
	// Enqueue adds a user interjection to the steering queue.
	Enqueue(message string)
	// TryDequeue removes and returns the oldest interjection, or ok=false
	// if empty.
	TryDequeue() (message string, ok bool)
	// DrainAll removes and returns all pending interjections.
	DrainAll() []string
}

type steeringSinkKey struct{}

// WithSteeringSink returns a context carrying the SteeringSink.
func WithSteeringSink(ctx context.Context, sink SteeringSink) context.Context {
	return context.WithValue(ctx, steeringSinkKey{}, sink)
}

// SteeringSinkFromContext extracts the SteeringSink, or nil if absent.
func SteeringSinkFromContext(ctx context.Context) SteeringSink {
	sink, _ := ctx.Value(steeringSinkKey{}).(SteeringSink)
	return sink
}
