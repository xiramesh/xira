package runtime

import "context"

// spawn_sink.go: SpawnSink is the per-chat-key spawn-result delivery interface
// (Phase 3, RFC §2.4 corrected / D-3). spawn_turn's detached goroutine delivers
// the child turn's result (pendingResult) to the sink. The sink is injected via
// context.Value — same pattern as EventSink and SteeringSink.
//
// Phase 3 defines this interface and spawnCore uses it. No production sink
// implementation exists yet in Phase 3 (fire-and-forget: spawn_turn returns
// "spawned" and the parent LLM continues; child results land in the sink for
// future consumers — Phase 4 steering checkpoint / Phase 5 WAL / future
// wait_turn tool). When no sink is in the context, spawnCore logs Warn and
// drops the result (no panic — silent drop with Warn, per AGENTS.md §1.1).
//
// This breaks the same dependency shape as EventSink: runtime defines the
// interface, a downstream package implements it, the runner injects it.

// SpawnSink receives a pendingResult from a spawned child turn.
// Implementations (future): a per-turn buffered collector, a WAL writer,
// a steering-aware join point.
type SpawnSink interface {
	// Deliver hands off a pendingResult. Must be non-blocking (same contract
	// as EventBus.Publish / EventSink.Deliver): if the sink is full, drop +
	// handle logging on the implementation side, not the caller.
	Deliver(pr pendingResult)
}

type spawnSinkKey struct{}

// WithSpawnSink returns a context carrying the SpawnSink.
func WithSpawnSink(ctx context.Context, sink SpawnSink) context.Context {
	return context.WithValue(ctx, spawnSinkKey{}, sink)
}

// SpawnSinkFromContext extracts the SpawnSink, or nil if absent.
func SpawnSinkFromContext(ctx context.Context) SpawnSink {
	sink, _ := ctx.Value(spawnSinkKey{}).(SpawnSink)
	return sink
}
