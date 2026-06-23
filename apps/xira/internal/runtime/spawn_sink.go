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

// SpawnSink receives a PendingResult from a spawned child turn.
// Production implementor: progress.SpawnCollector (per-chat-key, injected via
// Router). Test doubles: mockSpawnSink in spawn_turn_test.go.
type SpawnSink interface {
	// Deliver hands off a PendingResult. Must be non-blocking (same contract
	// as EventBus.Publish / EventSink.Deliver): if the sink is full, drop +
	// handle logging on the implementation side, not the caller.
	Deliver(pr PendingResult)
}

// SpawnResultWaiter is optionally implemented by SpawnSink implementations
// that support blocking wait for a specific child's result (e.g. the
// progress.SpawnCollector). wait_turn uses it to block until a spawned child
// completes; plain SpawnSink test doubles need not implement it.
//
// Kept separate from SpawnSink so the core sink contract stays "deliver only"
// (mirroring EventSink/SteeringSink) — the blocking-wait capability is an
// implementation detail that lives on the production sink, not the contract.
type SpawnResultWaiter interface {
	// Wait blocks until the child identified by childID has delivered its
	// result, or ctx expires. Returns the result, or an error wrapping
	// ctx.Err() if it expired first (never blocks forever).
	Wait(ctx context.Context, childID string) (PendingResult, error)
}

// ShortSpawnID formats a child index into a 4-hex suffix used to synthesize
// spawn turn IDs in tests. Exported for tests that need IDs matching the
// collector's keys deterministically.
func ShortSpawnID(i int) string {
	const hex = "0123456789abcdef"
	if i < 0 {
		i = 0
	}
	return string([]byte{hex[(i>>12)&0xf], hex[(i>>8)&0xf], hex[(i>>4)&0xf], hex[i&0xf]})
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
