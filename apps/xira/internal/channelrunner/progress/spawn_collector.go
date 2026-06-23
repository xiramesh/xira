package progress

import (
	"context"
	"errors"
	"sync"

	"github.com/xiramesh/xira/internal/runtime"
)

// ErrSpawnCollectorReset is returned by Wait when the collector was Reset
// while the caller was blocked (e.g. steering retry). Distinguishable from a
// ctx timeout so wait_turn can report "interrupted by retry" vs "child timed out".
var ErrSpawnCollectorReset = errors.New("spawn collector reset (turn retried)")

// spawn_collector.go: SpawnCollector is the production SpawnSink — the
// per-chat-key collector for spawned child-turn results (Phase 4, RFC §2.4
// D-3). spawn_turn's detached goroutine delivers each child's PendingResult
// here via Deliver; the parent turn's wait_turn tool blocks on Wait until a
// given child completes.
//
// Injected by Router.Handle alongside SteeringSink (router.go), so every
// channel that uses the Router gets spawn-result collection for free.
//
// Implements runtime.SpawnSink (Deliver) and runtime.SpawnResultWaiter
// (Wait). The two interfaces are split: Deliver is the core "deliver only"
// contract (mirroring EventSink/SteeringSink); Wait is the blocking-wait
// capability that lives on this production implementation, used by wait_turn.
//
// Lifecycle: one SpawnCollector per active turn (same as SteeringQueue). On
// steering retry, Router/runner calls Reset to clear stale results (mirrors
// ChatContext.Reset) so the retried turn doesn't surface the previous run's
// child results.

// SpawnCollector collects spawned child-turn results and lets waiters block
// on a specific child's completion. Thread-safe.
type SpawnCollector struct {
	mu      sync.Mutex
	results map[string]runtime.PendingResult
	// waiters[childID] = channels for goroutines currently blocked in Wait
	// for that child. Deliver closes + drains the entry to wake them. Reset
	// signals all of them so they return.
	waiters map[string][]chan runtime.PendingResult
}

// NewSpawnCollector creates an empty SpawnCollector.
func NewSpawnCollector() *SpawnCollector {
	return &SpawnCollector{
		results: make(map[string]runtime.PendingResult),
		waiters: make(map[string][]chan runtime.PendingResult),
	}
}

// Deliver stores a child-turn result and wakes any goroutine blocked in Wait
// for that child. Non-blocking (SpawnSink contract): the only work under the
// lock is a map write + closing a handful of channels.
func (c *SpawnCollector) Deliver(pr runtime.PendingResult) {
	c.mu.Lock()
	c.results[pr.TurnID] = pr
	waiters := c.waiters[pr.TurnID]
	delete(c.waiters, pr.TurnID)
	c.mu.Unlock()

	// Wake each waiter (outside the lock). Closing guarantees the blocked
	// select fires; we then rely on the waiter re-reading results under the
	// lock to pick up the stored value (see Wait).
	for _, ch := range waiters {
		close(ch)
	}
}

// Wait blocks until the child identified by childID has delivered its result,
// or ctx expires. If the result is already present, returns immediately.
// Returns ctx.Err() (wrapped via context) if ctx expires first — never blocks
// forever.
func (c *SpawnCollector) Wait(ctx context.Context, childID string) (runtime.PendingResult, error) {
	// Fast path: result already delivered.
	c.mu.Lock()
	if pr, ok := c.results[childID]; ok {
		c.mu.Unlock()
		return pr, nil
	}
	// Slow path: register a waiter and block.
	ch := make(chan runtime.PendingResult, 1)
	c.waiters[childID] = append(c.waiters[childID], ch)
	c.mu.Unlock()

	select {
	case <-ch:
		// Woken by Deliver (result now present) or by Reset (result absent).
		// Distinguish: Deliver always stores the result BEFORE waking, so an
		// absent result means Reset fired — return an explicit error rather
		// than ctx.Err() (which is nil if ctx hasn't expired).
		c.mu.Lock()
		pr, ok := c.results[childID]
		c.mu.Unlock()
		if !ok {
			return runtime.PendingResult{}, ErrSpawnCollectorReset
		}
		return pr, nil
	case <-ctx.Done():
		// ctx expired before the child completed. Leave the waiter
		// registered — a late Deliver will close it harmlessly (the buffered
		// chan absorbs the signal; Reset cleans up).
		return runtime.PendingResult{}, ctx.Err()
	}
}

// Reset clears all stored results and pending waiters. Called on steering
// retry so the retried turn doesn't surface the previous run's child results.
// Waiters blocked at the time of Reset are woken; they re-check results
// (empty) and return their ctx error.
func (c *SpawnCollector) Reset() {
	c.mu.Lock()
	for childID, ws := range c.waiters {
		for _, ch := range ws {
			close(ch)
		}
		delete(c.waiters, childID)
	}
	for childID := range c.results {
		delete(c.results, childID)
	}
	c.mu.Unlock()
}
