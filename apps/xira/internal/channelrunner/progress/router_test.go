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
	router.Handle(key, "first", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		<-block
	})

	// Second message while turn active → should steer.
	router.Handle(key, "second", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
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
	router.Handle(key, "first", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		mu.Lock()
		callCount++
		mu.Unlock()
		done <- struct{}{}
	})
	<-done
	time.Sleep(10 * time.Millisecond) // let markComplete run

	router.Handle(key, "second", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
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

	router.Handle(keyA, "msgA", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		turns.Add(1)
		defer turns.Done()
		<-block
	})
	router.Handle(keyB, "msgB", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		turns.Add(1)
		defer turns.Done()
		<-block
	})

	router.Handle(keyA, "steerA", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {})
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

// TestRouterInjectsSpawnSink verifies Handle wires SpawnSink (the
// SpawnCollector) into the turn ctx alongside SteeringSink, so every channel
// using the Router gets spawn-result collection for free. Without this,
// wait_turn reports "unavailable" and spawned results are dropped.
func TestRouterInjectsSpawnSink(t *testing.T) {
	key := runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}
	router := NewRouter()

	var gotSink runtime.SpawnSink
	done := make(chan struct{})
	router.Handle(key, "hello", context.Background(), func(k runtime.ChatKey, msg string, ctx context.Context) {
		gotSink = runtime.SpawnSinkFromContext(ctx)
		close(done)
	})
	<-done

	if gotSink == nil {
		t.Fatal("SpawnSink not injected into turn ctx — wait_turn would report unavailable")
	}
	collector, ok := gotSink.(*SpawnCollector)
	if !ok {
		t.Fatalf("SpawnSink is %T, want *SpawnCollector", gotSink)
	}
	// The injected collector must be the same instance the Router exposes
	// (so the runner's SpawnCollectorFor(key).Reset() clears the same one).
	if router.SpawnCollectorFor(key) != collector {
		t.Error("injected SpawnCollector != Router.SpawnCollectorFor(key) — Reset would miss the active collector")
	}
}
