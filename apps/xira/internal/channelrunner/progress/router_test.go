package progress

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// router_test.go: tests the per-chat-key Router. Handle now takes a per-call
// onNewTurn callback (so the caller can capture per-message context like
// account/sender that Router's lifecycle can't know about).

func TestRouterNoActiveTurnStartsNew(t *testing.T) {
	var started bool
	var gotMsg string
	var mu sync.Mutex
	done := make(chan struct{})
	router := NewRouter()
	router.Handle(
		runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"},
		"",
		"hello",
		context.Background(),
		func(key runtime.ChatKey, msg string, ctx context.Context) {
			mu.Lock()
			started = true
			gotMsg = msg
			mu.Unlock()
			close(done)
		},
	)
	<-done
	if !started {
		t.Error("onNewTurn not called")
	}
	if gotMsg != "hello" {
		t.Errorf("got %q, want hello", gotMsg)
	}
}

func TestRouterActiveTurnSteersSecondMessage(t *testing.T) {
	key := runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}
	block := make(chan struct{})
	router := NewRouter()

	// First message → starts turn (blocks in goroutine).
	router.Handle(key, "", "first", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		<-block
	})

	// Second message while turn active → should steer.
	router.Handle(key, "", "second", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		t.Error("onNewTurn should NOT be called for steered message")
	})

	sq := router.SteeringQueue(key)
	msgs := sq.DrainAll()
	if len(msgs) != 1 || msgs[0] != "second" {
		t.Errorf("steering queue = %v, want [second]", msgs)
	}

	close(block)
}

func TestRouterTurnCompletesThenNewTurnStarts(t *testing.T) {
	key := runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}
	var callCount int
	var mu sync.Mutex
	done := make(chan struct{}, 2)

	router := NewRouter()
	router.Handle(key, "", "first", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		mu.Lock()
		callCount++
		mu.Unlock()
		done <- struct{}{}
	})
	<-done
	time.Sleep(10 * time.Millisecond) // let markComplete run

	router.Handle(key, "", "second", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		mu.Lock()
		callCount++
		mu.Unlock()
		done <- struct{}{}
	})
	<-done

	if callCount != 2 {
		t.Errorf("onNewTurn called %d times, want 2", callCount)
	}
}

func TestRouterDifferentChatKeysIndependent(t *testing.T) {
	keyA := runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}
	keyB := runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u2"}

	block := make(chan struct{})
	var turns sync.WaitGroup
	router := NewRouter()

	// Add BEFORE Handle launches the goroutine (Add inside the goroutine races
	// with Wait below — round-6 race fix).
	turns.Add(2)
	router.Handle(keyA, "", "msgA", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		defer turns.Done()
		<-block
	})
	router.Handle(keyB, "", "msgB", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		defer turns.Done()
		<-block
	})

	router.Handle(keyA, "", "steerA", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {})
	sqA := router.SteeringQueue(keyA)
	msgsA := sqA.DrainAll()
	if len(msgsA) != 1 || msgsA[0] != "steerA" {
		t.Errorf("keyA steering = %v, want [steerA]", msgsA)
	}

	sqB := router.SteeringQueue(keyB)
	if msgs := sqB.DrainAll(); len(msgs) != 0 {
		t.Errorf("keyB steering = %v, want empty", msgs)
	}

	close(block)
	turns.Wait()
}

// TestRouterInjectsSpawnBus verifies Handle wires SpawnBus (the
// SpawnCollector) into the turn ctx alongside SteeringBus, so every channel
// using the Router gets spawn-result collection for free. Without this,
// wait_turn reports "unavailable" and spawned results are dropped.
func TestRouterInjectsSpawnBus(t *testing.T) {
	key := runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}
	router := NewRouter()

	var gotSink runtime.SpawnBus
	done := make(chan struct{})
	router.Handle(key, "", "hello", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		gotSink = runtime.SpawnBusFromContext(ctx)
		close(done)
	})
	<-done

	if gotSink == nil {
		t.Fatal("SpawnBus not injected into turn ctx — wait_turn would report unavailable")
	}
	collector, ok := gotSink.(*SpawnCollector)
	if !ok {
		t.Fatalf("SpawnBus is %T, want *SpawnCollector", gotSink)
	}
	// The injected collector must be the same instance the Router exposes
	// (so the runner's SpawnCollectorFor(key).Reset() clears the same one).
	if router.SpawnCollectorFor(key) != collector {
		t.Error("injected SpawnCollector != Router.SpawnCollectorFor(key) — Reset would miss the active collector")
	}
}

// --- Router entry eviction (lazy prune, mirrors dedupe.MessageDeduper) ---

// TestRouterEvictsIdleEntryAfterTTL verifies that an entry idle (no active
// turn) longer than the TTL is pruned on the next Handle. Before this fix,
// Router.entries grew unboundedly — one entry per distinct chatKey, each
// holding a SteeringQueue + SpawnCollector, never freed (flagged by #51/#52/
// #53 reviews as a cross-cutting follow-up).
func TestRouterEvictsIdleEntryAfterTTL(t *testing.T) {
	router := NewRouter()
	keyA := runtime.ChatKey{Channel: "ilink", ChatID: "cA", SenderID: "u1"}

	// Run a turn for keyA so it gets an entry, then let it complete (entry
	// becomes idle with idleSince set).
	runOneTurn(router, keyA)

	// Sanity: entry exists after the turn.
	if router.SteeringQueue(keyA) == nil {
		t.Fatal("entry for keyA not present after turn")
	}

	// Simulate TTL elapsing: prune with a "now" past the TTL.
	router.prune(time.Now().Add(routerEntryTTL + time.Second))

	if router.SteeringQueue(keyA) != nil {
		t.Error("idle entry survived prune past TTL — entries leak unboundedly")
	}
}

// TestRouterKeepsEntryWithinTTL verifies an idle entry that is still within
// its TTL is NOT pruned (so an active conversation isn't evicted mid-chat).
func TestRouterKeepsEntryWithinTTL(t *testing.T) {
	router := NewRouter()
	key := runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}
	runOneTurn(router, key)

	// Prune with "now" still within the TTL window.
	router.prune(time.Now())

	if router.SteeringQueue(key) == nil {
		t.Error("idle entry was pruned within TTL — active conversation evicted")
	}
}

// TestRouterDoesNotEvictActiveEntry verifies an entry whose turn is still
// running is never pruned, even past the TTL. active==true must win over age.
func TestRouterDoesNotEvictActiveEntry(t *testing.T) {
	router := NewRouter()
	key := runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}

	// Start a turn but don't let it complete — keep it active.
	block := make(chan struct{})
	router.Handle(key, "", "hello", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		<-block // hold the turn open
	})
	t.Cleanup(func() { close(block) })

	// Give the goroutine a moment to set active=true.
	time.Sleep(50 * time.Millisecond)

	// Prune far in the future — the active entry must survive.
	router.prune(time.Now().Add(2 * routerEntryTTL))

	if router.SteeringQueue(key) == nil {
		t.Error("active entry was pruned — running turn would lose its steering/spawn sinks")
	}
}

// TestRouterHandleTriggersPrune verifies Handle itself triggers pruning
// (lazy, on-access — the dedupe pattern), so entries are reaped without a
// background goroutine. A Handle for keyB should evict an expired keyA.
func TestRouterHandleTriggersPrune(t *testing.T) {
	router := NewRouter()
	keyA := runtime.ChatKey{Channel: "ilink", ChatID: "cA", SenderID: "u1"}
	keyB := runtime.ChatKey{Channel: "ilink", ChatID: "cB", SenderID: "u1"}

	runOneTurn(router, keyA)
	// Warp keyA's idleSince into the past so it's past TTL on the next Handle.
	router.warpIdleSince(keyA, time.Now().Add(-(routerEntryTTL + time.Second)))

	// A new turn for a DIFFERENT key should prune keyA as a side effect.
	runOneTurn(router, keyB)

	if router.SteeringQueue(keyA) != nil {
		t.Error("expired keyA survived a Handle that should have pruned it (lazy prune not wired)")
	}
}

// TestRouterEvictedEntryRebuiltOnNextMessage verifies that after eviction, a
// new message for the same key rebuilds the entry cleanly (fresh sinks, treated
// as a new turn — not lost, not steered).
func TestRouterEvictedEntryRebuiltOnNextMessage(t *testing.T) {
	router := NewRouter()
	key := runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}

	runOneTurn(router, key)
	oldQueue := router.SteeringQueue(key)
	router.prune(time.Now().Add(routerEntryTTL + time.Second))
	if router.SteeringQueue(key) != nil {
		t.Fatal("precondition: entry should be evicted")
	}

	// Next message rebuilds.
	ran := make(chan string, 1)
	router.Handle(key, "", "after-eviction", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		ran <- msg
	})
	select {
	case msg := <-ran:
		if msg != "after-eviction" {
			t.Errorf("rebuilt turn got msg %q, want 'after-eviction'", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("rebuilt entry did not start a new turn")
	}
	// Fresh queue (not the evicted one).
	newQueue := router.SteeringQueue(key)
	if newQueue == nil || newQueue == oldQueue {
		t.Error("rebuilt entry did not get a fresh SteeringQueue")
	}
}

// TestRouterIsActiveReportsTurnState: IsActive reflects whether a turn is
// in-flight for chatKey. Used by cross-package tests (feishu/ws) to wait for
// async turn completion before asserting on side-effects.
func TestRouterIsActiveReportsTurnState(t *testing.T) {
	router := NewRouter()
	key := runtime.ChatKey{Channel: "test", ChatID: "c1", SenderID: "u1"}

	// No entry yet → not active.
	if router.IsActive(key) {
		t.Error("IsActive=true before any Handle (want false)")
	}

	// Start a turn and hold it open.
	block := make(chan struct{})
	closed := false
	closeOnce := func() {
		if !closed {
			closed = true
			close(block)
		}
	}
	t.Cleanup(closeOnce)
	go router.Handle(key, "", "hello", context.Background(), func(runtime.ChatKey, string, context.Context) {
		<-block
	})

	// Wait until active (dispatch is async).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !router.IsActive(key) {
		time.Sleep(time.Millisecond)
	}
	if !router.IsActive(key) {
		t.Fatal("IsActive stayed false while a turn was in-flight")
	}

	// Release the turn; wait for it to go inactive.
	closeOnce()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && router.IsActive(key) {
		time.Sleep(time.Millisecond)
	}
	if router.IsActive(key) {
		t.Error("IsActive stayed true after turn completed (want false)")
	}

	// Unknown key → false.
	if router.IsActive(runtime.ChatKey{Channel: "test", ChatID: "other", SenderID: "u1"}) {
		t.Error("IsActive=true for a key that was never Handle'd")
	}
}

// runOneTurn is a test helper: runs a Handle that completes immediately,
// leaving the entry idle (active=false, idleSince set).
func runOneTurn(router *Router, key runtime.ChatKey) {
	done := make(chan struct{})
	router.Handle(key, "", "hello", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		close(done)
	})
	<-done
	// markComplete runs in the goroutine's defer; give it a moment.
	time.Sleep(20 * time.Millisecond)
}
