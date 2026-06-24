package progress

import (
	"sync"

	"github.com/xiramesh/xira/internal/runtime"
)

// spawn_collector.go: SpawnCollector is the production SpawnBus — the
// per-chat-key store for spawned child-turn results (Phase 4, RFC §2.4 D-3).
// spawn_turn's detached goroutine delivers each child's PendingResult here via
// Deliver; the parent turn's poll_turn tool queries it NON-BLOCKINGLY via
// TryResult.
//
// Injected by Router.Handle alongside SteeringBus (router.go), so every
// channel that uses the Router gets spawn-result collection for free.
//
// Design (R2, PR #53 review): SpawnCollector is a NON-BLOCKING store, mirroring
// SteeringQueue (HasPending/TryDequeue). The previous blocking Wait was an
// architectural dead end — it blocked the ADK event loop (tools run
// synchronously in ADK v1.4.0, base_flow.go wg.Wait), which froze the steering
// checkpoint. poll_turn pulls instead, so the event loop keeps iterating and
// steering stays responsive.
//
// Lifecycle: one SpawnCollector per active turn (same as SteeringQueue). On
// steering retry, Router/runner calls Reset to clear stale results (mirrors
// ChatContext.Reset) so the retried turn doesn't surface the previous run's
// child results.

// SpawnCollector stores spawned child-turn results for non-blocking polling.
// Thread-safe.
type SpawnCollector struct {
	mu      sync.Mutex
	results map[string]runtime.PendingResult
}

// NewSpawnCollector creates an empty SpawnCollector.
func NewSpawnCollector() *SpawnCollector {
	return &SpawnCollector{
		results: make(map[string]runtime.PendingResult),
	}
}

// Deliver stores a child-turn result, keyed by its TurnID. Non-blocking
// (SpawnBus contract): the only work is a map write under the lock.
func (c *SpawnCollector) Deliver(pr runtime.PendingResult) {
	c.mu.Lock()
	c.results[pr.TurnID] = pr
	c.mu.Unlock()
}

// TryResult returns the child's result if it has completed. Non-blocking:
// returns (zero, false) immediately if the child is still running or unknown.
// poll_turn maps false → "pending".
func (c *SpawnCollector) TryResult(childID string) (runtime.PendingResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pr, ok := c.results[childID]
	return pr, ok
}

// HasResult reports whether ANY child result is available. Mirrors
// SteeringBus.HasPending — the checkpoint peek shape.
func (c *SpawnCollector) HasResult() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.results) > 0
}

// DrainAll returns all completed results and clears the store. For a future
// checkpoint batch-drain; poll_turn uses single-child TryResult instead.
func (c *SpawnCollector) DrainAll() []runtime.PendingResult {
	c.mu.Lock()
	out := make([]runtime.PendingResult, 0, len(c.results))
	for id, pr := range c.results {
		out = append(out, pr)
		delete(c.results, id)
	}
	c.mu.Unlock()
	return out
}

// Reset clears all stored results. Called on steering retry so the retried
// turn doesn't surface the previous run's child results.
func (c *SpawnCollector) Reset() {
	c.mu.Lock()
	for id := range c.results {
		delete(c.results, id)
	}
	c.mu.Unlock()
}
