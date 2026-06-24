package runtime

import "context"

// spawn_sink.go: SpawnBus is the per-chat-key spawn-result delivery interface
// (Phase 3, RFC §2.4 corrected / D-3). spawn_turn's detached goroutine delivers
// the child turn's result (pendingResult) to the sink. The sink is injected via
// context.Value — same pattern as EventBus and SteeringBus.
//
// Phase 3 defines this interface and spawnCore uses it. No production sink
// implementation exists yet in Phase 3 (fire-and-forget: spawn_turn returns
// "spawned" and the parent LLM continues; child results land in the sink for
// future consumers — Phase 4 steering checkpoint / Phase 5 WAL / future
// wait_turn tool). When no sink is in the context, spawnCore logs Warn and
// drops the result (no panic — silent drop with Warn, per AGENTS.md §1.1).
//
// This breaks the same dependency shape as EventBus: runtime defines the
// interface, a downstream package implements it, the runner injects it.

// SpawnBus receives a PendingResult from a spawned child turn.
// Production implementor: progress.SpawnCollector (per-chat-key, injected via
// Router). Test doubles: mockSpawnBus in spawn_turn_test.go.
type SpawnBus interface {
	// Deliver hands off a PendingResult. Must be non-blocking (same contract
	// as EventBus.Publish / EventBus.Deliver): if the sink is full, drop +
	// handle logging on the implementation side, not the caller.
	Deliver(pr PendingResult)
}

// SpawnResultWaiter was a blocking-Wait capability on SpawnBus. REMOVED in R2
// (PR #53 review): blocking wait inside an ADK tool handler froze the event
// loop, disabling the steering checkpoint. Spawn result delivery is now
// non-blocking: the parent uses poll_turn (SpawnBusPeeper.TryResult) to pull,
// never blocking. Kept as a comment marker so greppers find the rationale.

// SpawnBusPeeper is optionally implemented by SpawnBus implementations that
// support non-blocking result queries (e.g. progress.SpawnCollector). poll_turn
// uses it; plain SpawnBus test doubles need not implement it.
//
// Kept separate from SpawnBus so the core sink contract stays "deliver only"
// (mirroring EventBus/SteeringBus). This is the NON-BLOCKING sibling of the
// deleted SpawnResultWaiter — pull, never block.
type SpawnBusPeeper interface {
	// TryResult returns the child's result if it has completed. Non-blocking:
	// returns (zero, false) immediately if the child is still running or
	// unknown. poll_turn maps false → "pending".
	TryResult(childID string) (PendingResult, bool)
	// HasResult reports whether ANY child result is available (checkpoint
	// peek, mirrors SteeringBus.HasPending).
	HasResult() bool
}

type spawnSinkKey struct{}

// WithSpawnBus returns a context carrying the SpawnBus.
func WithSpawnBus(ctx context.Context, sink SpawnBus) context.Context {
	return context.WithValue(ctx, spawnSinkKey{}, sink)
}

// SpawnBusFromContext extracts the SpawnBus, or nil if absent.
func SpawnBusFromContext(ctx context.Context) SpawnBus {
	sink, _ := ctx.Value(spawnSinkKey{}).(SpawnBus)
	return sink
}
