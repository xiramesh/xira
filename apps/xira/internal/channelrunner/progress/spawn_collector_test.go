package progress

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// spawn_collector_test.go: tests SpawnCollector — the production SpawnSink
// implementation. spawn_turn's detached goroutine delivers child-turn
// results here (Deliver); the parent turn's wait_turn tool blocks on Wait
// until a given child completes (Phase 4, RFC §2.4 D-3).
//
// SpawnCollector implements runtime.SpawnSink (Deliver) and
// runtime.SpawnResultWaiter (Wait).

func TestSpawnCollectorDeliverThenWait(t *testing.T) {
	// Fast path: result is delivered BEFORE Wait is called. Wait must
	// return it immediately without blocking.
	c := NewSpawnCollector()
	want := runtime.PendingResult{TurnID: "spawn:abc", Result: runtime.DelegateAgentResult{AgentID: "code", Status: "completed", Summary: "done"}}
	c.Deliver(want)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := c.Wait(ctx, "spawn:abc")
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if got.TurnID != want.TurnID || got.Result.Summary != want.Result.Summary {
		t.Errorf("Wait = %+v, want %+v", got, want)
	}
}

func TestSpawnCollectorWaitThenDeliver(t *testing.T) {
	// Slow path: Wait is called BEFORE the result arrives. Wait must
	// block until Deliver wakes it. This is the core producer-consumer
	// contract — without it, wait_turn would busy-poll or miss results.
	c := NewSpawnCollector()

	gotCh := make(chan runtime.PendingResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		got, err := c.Wait(ctx, "spawn:late")
		if err != nil {
			t.Errorf("Wait errored: %v", err)
			gotCh <- runtime.PendingResult{}
			return
		}
		gotCh <- got
	}()

	// Give the goroutine a moment to enter Wait.
	time.Sleep(50 * time.Millisecond)

	c.Deliver(runtime.PendingResult{TurnID: "spawn:late", Result: runtime.DelegateAgentResult{AgentID: "code", Status: "completed", Summary: "late result"}})

	select {
	case got := <-gotCh:
		if got.Result.Summary != "late result" {
			t.Errorf("got summary %q, want 'late result'", got.Result.Summary)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after Deliver within 1s — blocked or missed the result")
	}
}

func TestSpawnCollectorWaitTimeout(t *testing.T) {
	// When the child never completes, Wait must respect ctx and return —
	// never block forever. wait_turn relies on this (tool ctx deadline).
	c := NewSpawnCollector()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Wait(ctx, "spawn:never")
	if err == nil {
		t.Fatal("Wait returned nil error on timeout, want non-nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait error = %v, want context.DeadlineExceeded", err)
	}
}

func TestSpawnCollectorResetClearsResults(t *testing.T) {
	// Reset (called on steering retry) must clear delivered results so
	// stale results from the previous run don't surface in the retry.
	c := NewSpawnCollector()
	c.Deliver(runtime.PendingResult{TurnID: "spawn:stale", Result: runtime.DelegateAgentResult{Status: "completed"}})

	c.Reset()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Wait(ctx, "spawn:stale")
	if err == nil {
		t.Error("Wait returned a result after Reset — stale results survived")
	}
}

func TestSpawnCollectorResetWakesWaiters(t *testing.T) {
	// Reset must unblock goroutines currently in Wait (no Deliver yet) —
	// otherwise steering retry leaks the blocked wait_turn goroutine. The
	// woken waiter re-checks results (empty after Reset) and returns its
	// ctx error.
	c := NewSpawnCollector()
	woken := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := c.Wait(ctx, "spawn:pending")
		woken <- err
	}()

	time.Sleep(50 * time.Millisecond) // let the goroutine enter Wait
	c.Reset()

	select {
	case err := <-woken:
		if !errors.Is(err, ErrSpawnCollectorReset) {
			t.Errorf("woken Wait error = %v, want ErrSpawnCollectorReset", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait was not woken by Reset within 1s — goroutine leaked")
	}
}

func TestSpawnCollectorConcurrentDeliver(t *testing.T) {
	// Multiple detached goroutines (multiple concurrent spawns) may Deliver
	// simultaneously. Must be race-free. Run with -race.
	c := NewSpawnCollector()
	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			c.Deliver(runtime.PendingResult{
				TurnID: "spawn:" + runtime.ShortSpawnID(i),
				Result: runtime.DelegateAgentResult{Status: "completed", Summary: "ok"},
			})
		}(i)
	}
	wg.Wait()

	// All n results should be retrievable.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < n; i++ {
		id := "spawn:" + runtime.ShortSpawnID(i)
		if _, err := c.Wait(ctx, id); err != nil {
			t.Errorf("Wait(%q) error after concurrent Deliver: %v", id, err)
		}
	}
}

// Compile-time: SpawnCollector satisfies SpawnSink + SpawnResultWaiter.
var _ runtime.SpawnSink = (*SpawnCollector)(nil)
var _ runtime.SpawnResultWaiter = (*SpawnCollector)(nil)
