package progress

import (
	"context"
	"sync"
	"testing"

	"github.com/xiramesh/xira/internal/runtime"
)

// steering_test.go: tests the SteeringQueue — a per-chat-key queue that
// collects user messages sent while a turn is running (Phase 4, RFC #48 §5).
//
// When the user sends a message mid-turn, it goes into the SteeringQueue
// instead of starting a new turn. The ADK event loop checks the queue
// between iterations (checkpoint). If non-empty, the current run is
// canceled and restarted with the user's interjection.
//
// SteeringQueue implements runtime.SteeringBus (context.Value, same
// pattern as EventBus).

func TestSteeringQueueEmpty(t *testing.T) {
	sq := NewSteeringQueue()
	if msg, ok := sq.TryDequeue(); ok {
		t.Errorf("TryDequeue on empty queue returned %q, want empty", msg)
	}
}

func TestSteeringQueueEnqueueDequeue(t *testing.T) {
	sq := NewSteeringQueue()
	if sq.HasPending() {
		t.Fatal("new queue HasPending=true, want false")
	}
	sq.Enqueue("等等，换个思路")
	if !sq.HasPending() {
		t.Fatal("queue HasPending=false after Enqueue")
	}
	msg, ok := sq.TryDequeue()
	if !ok {
		t.Fatal("TryDequeue returned ok=false after Enqueue")
	}
	if msg != "等等，换个思路" {
		t.Errorf("got %q, want '等等，换个思路'", msg)
	}
	// Queue should be empty after dequeue.
	if _, ok := sq.TryDequeue(); ok {
		t.Error("queue not empty after dequeue")
	}
	if sq.HasPending() {
		t.Fatal("queue HasPending=true after dequeue")
	}
}

func TestSteeringQueueFIFO(t *testing.T) {
	sq := NewSteeringQueue()
	sq.Enqueue("first")
	sq.Enqueue("second")
	msg1, _ := sq.TryDequeue()
	msg2, _ := sq.TryDequeue()
	if msg1 != "first" || msg2 != "second" {
		t.Errorf("FIFO order wrong: got %q then %q, want first then second", msg1, msg2)
	}
}

func TestSteeringQueueDrainAll(t *testing.T) {
	sq := NewSteeringQueue()
	sq.Enqueue("a")
	sq.Enqueue("b")
	sq.Enqueue("c")
	msgs := sq.DrainAll()
	if len(msgs) != 3 || msgs[0] != "a" || msgs[2] != "c" {
		t.Errorf("DrainAll = %v, want [a b c]", msgs)
	}
	if _, ok := sq.TryDequeue(); ok {
		t.Error("queue not empty after DrainAll")
	}
}

func TestSteeringQueueConcurrentSafe(t *testing.T) {
	sq := NewSteeringQueue()
	var wg sync.WaitGroup
	// Concurrent enqueuers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sq.Enqueue("msg")
		}()
	}
	wg.Wait()
	msgs := sq.DrainAll()
	if len(msgs) != 10 {
		t.Errorf("DrainAll got %d, want 10", len(msgs))
	}
}

func TestSteeringQueueImplementsSteeringBus(t *testing.T) {
	// Compile-time: SteeringQueue satisfies runtime.SteeringBus.
	var _ runtime.SteeringBus = NewSteeringQueue()
}

func TestSteeringQueueWithContext(t *testing.T) {
	// WithSteeringBus / SteeringBusFromContext round-trip.
	sq := NewSteeringQueue()
	ctx := runtime.WithSteeringBus(context.Background(), sq)
	got := runtime.SteeringBusFromContext(ctx)
	if got != sq {
		t.Error("SteeringBusFromContext did not return the same sink")
	}
}

func TestSteeringQueueNilSinkFromContext(t *testing.T) {
	// No sink in context → nil.
	got := runtime.SteeringBusFromContext(context.Background())
	if got != nil {
		t.Error("expected nil SteeringBus from empty context")
	}
}
