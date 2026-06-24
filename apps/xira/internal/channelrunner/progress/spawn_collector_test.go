package progress

import (
	"sync"
	"testing"

	"github.com/xiramesh/xira/internal/runtime"
)

// spawn_collector_test.go: tests SpawnCollector — the production SpawnSink.
// spawn_turn's detached goroutine delivers child-turn results here (Deliver);
// the parent turn's poll_turn tool queries them non-blockingly (TryResult).
//
// Design (R2): SpawnCollector is a NON-BLOCKING store, mirroring
// SteeringQueue (HasPending/TryDequeue). The previous Wait (blocking) was a
// dead end: it blocked the ADK event loop, freezing the steering checkpoint
// (PR #53 review CRITICAL). poll_turn pulls instead.
//
// Implements runtime.SpawnSink (Deliver) + runtime.SpawnSinkPeeper
// (TryResult/HasResult/DrainAll).

func TestSpawnCollectorDeliverThenTryResult(t *testing.T) {
	// Deliver stores; TryResult retrieves. The core store/retrieve contract.
	c := NewSpawnCollector()
	want := runtime.PendingResult{
		TurnID: "spawn:full-uuid-1",
		Result: runtime.DelegateAgentResult{AgentID: "code", Status: "completed", Summary: "done"},
	}
	c.Deliver(want)

	got, ok := c.TryResult("spawn:full-uuid-1")
	if !ok {
		t.Fatal("TryResult returned ok=false after Deliver")
	}
	if got.TurnID != want.TurnID || got.Result.Summary != want.Result.Summary {
		t.Errorf("TryResult = %+v, want %+v", got, want)
	}
}

func TestSpawnCollectorTryResultMissing(t *testing.T) {
	// A child that hasn't completed yet (or a bogus ID) returns ok=false,
	// not a zero result. poll_turn uses this to report "pending".
	c := NewSpawnCollector()
	if _, ok := c.TryResult("spawn:never"); ok {
		t.Error("TryResult returned ok=true for a never-delivered child")
	}
}

func TestSpawnCollectorHasResult(t *testing.T) {
	// HasResult is the checkpoint peek — "any child done?" without consuming.
	c := NewSpawnCollector()
	if c.HasResult() {
		t.Error("HasResult=true on empty collector")
	}
	c.Deliver(runtime.PendingResult{TurnID: "spawn:1", Result: runtime.DelegateAgentResult{Status: "completed"}})
	if !c.HasResult() {
		t.Error("HasResult=false after Deliver")
	}
}

func TestSpawnCollectorDrainAll(t *testing.T) {
	// DrainAll returns every completed result and clears the store (for a
	// future checkpoint batch-drain). After drain, HasResult is false.
	c := NewSpawnCollector()
	c.Deliver(runtime.PendingResult{TurnID: "spawn:1", Result: runtime.DelegateAgentResult{Status: "completed", Summary: "a"}})
	c.Deliver(runtime.PendingResult{TurnID: "spawn:2", Result: runtime.DelegateAgentResult{Status: "completed", Summary: "b"}})

	all := c.DrainAll()
	if len(all) != 2 {
		t.Fatalf("DrainAll returned %d results, want 2", len(all))
	}
	if c.HasResult() {
		t.Error("HasResult=true after DrainAll — store not cleared")
	}
}

func TestSpawnCollectorResetClearsResults(t *testing.T) {
	// Reset (called on steering retry) must clear delivered results so
	// stale results from the previous run don't surface in the retry.
	c := NewSpawnCollector()
	c.Deliver(runtime.PendingResult{TurnID: "spawn:stale", Result: runtime.DelegateAgentResult{Status: "completed"}})

	c.Reset()

	if _, ok := c.TryResult("spawn:stale"); ok {
		t.Error("TryResult returned a result after Reset — stale results survived")
	}
	if c.HasResult() {
		t.Error("HasResult=true after Reset")
	}
}

func TestSpawnCollectorConcurrentDeliverAndRead(t *testing.T) {
	// Multiple detached goroutines (concurrent spawns) Deliver while the
	// parent polls (TryResult/HasResult). Must be race-free. Run with -race.
	c := NewSpawnCollector()
	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(id string) {
			defer wg.Done()
			c.Deliver(runtime.PendingResult{
				TurnID: id,
				Result: runtime.DelegateAgentResult{Status: "completed", Summary: "ok"},
			})
		}("spawn:child-" + string(rune('a'+i%26)) + string(rune('0'+i)))
	}
	// Reader goroutine polls concurrently.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_ = c.HasResult()
		}
		close(done)
	}()
	wg.Wait()
	<-done

	all := c.DrainAll()
	if len(all) != n {
		t.Errorf("DrainAll returned %d, want %d after concurrent Deliver", len(all), n)
	}
}

// Compile-time: SpawnCollector satisfies SpawnSink + SpawnSinkPeeper.
var _ runtime.SpawnSink = (*SpawnCollector)(nil)
var _ runtime.SpawnSinkPeeper = (*SpawnCollector)(nil)
