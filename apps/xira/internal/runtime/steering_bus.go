package runtime

import (
	"context"
	"errors"
)

// steering_sink.go: SteeringBus is the per-chat-key steering queue interface
// (Phase 4, RFC #48 §5). Channel runner enqueues user interjections;
// generateADK's event loop checks the queue between iterations (checkpoint).
//
// Same context.Value pattern as EventBus — runtime defines interface,
// progress implements (SteeringQueue), runner passes via context.

// ErrSteered is returned by generateADK when the steering checkpoint detects
// a pending user interjection. The caller (channel runner retry loop) catches
// this, drains the interjection, and re-runs RunAgent with it.
// NOT context.Canceled — checkpoint does NOT cancel ctx. It returns this
// sentinel so the retry loop can distinguish "steered" from "real error".
var ErrSteered = errors.New("turn steered by user interjection")

// SteeringBus collects user messages sent while a turn is running.
// Implemented by progress.SteeringQueue.
type SteeringBus interface {
	// Enqueue adds a user interjection to the steering queue.
	Enqueue(message string)
	// TryDequeue removes and returns the oldest interjection, or ok=false
	// if empty.
	TryDequeue() (message string, ok bool)
	// DrainAll removes and returns all pending interjections.
	DrainAll() []string
	// HasPending reports whether the queue has any interjections, WITHOUT
	// consuming them. Used by the checkpoint (peek, don't consume — the
	// retry loop consumes).
	HasPending() bool
}

type steeringSinkKey struct{}

// WithSteeringBus returns a context carrying the SteeringBus.
func WithSteeringBus(ctx context.Context, sink SteeringBus) context.Context {
	return context.WithValue(ctx, steeringSinkKey{}, sink)
}

// SteeringBusFromContext extracts the SteeringBus, or nil if absent.
func SteeringBusFromContext(ctx context.Context) SteeringBus {
	sink, _ := ctx.Value(steeringSinkKey{}).(SteeringBus)
	return sink
}
