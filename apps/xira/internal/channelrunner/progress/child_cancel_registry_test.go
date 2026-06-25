package progress

import (
	"context"
	"sync"
	"testing"

	"github.com/xiramesh/xira/internal/runtime"
)

// child_cancel_registry_test.go: tests the per-chat-key registry of spawned-
// child cancel funcs. When the parent turn is steered (RFC #67), the channel
// runner calls CancelAll(chatKey) to cancel every outstanding child of that
// conversation so they stop burning tokens.

func keyA() runtime.ChatKey { return runtime.ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"} }
func keyB() runtime.ChatKey { return runtime.ChatKey{Channel: "ilink", ChatID: "c2", SenderID: "u1"} }

// TestChildCancelRegistryCancelAll registers three cancel funcs under one
// chatKey, cancels them, and asserts every registered ctx is Done().
func TestChildCancelRegistryCancelAll(t *testing.T) {
	reg := NewChildCancelRegistry()
	var ctxs []context.Context
	var cancels []context.CancelFunc
	for i := 0; i < 3; i++ {
		cctx, cancel := context.WithCancel(context.Background())
		ctxs = append(ctxs, cctx)
		cancels = append(cancels, cancel)
		reg.Register(keyA(), cancel)
	}

	n := reg.CancelAll(keyA())
	if n != 3 {
		t.Errorf("CancelAll returned %d, want 3", n)
	}
	for i, cctx := range ctxs {
		select {
		case <-cctx.Done():
		default:
			t.Errorf("ctx #%d not Done after CancelAll", i)
		}
	}
}

// TestChildCancelRegistryChatKeyIsolation verifies CancelAll on one chatKey
// does not touch another chatKey's children (per-chat-key isolation, RFC #48).
func TestChildCancelRegistryChatKeyIsolation(t *testing.T) {
	reg := NewChildCancelRegistry()
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	reg.Register(keyA(), cancelA)
	reg.Register(keyB(), cancelB)

	reg.CancelAll(keyA())

	select {
	case <-ctxA.Done():
	default:
		t.Error("ctxA should be canceled")
	}
	select {
	case <-ctxB.Done():
		t.Error("ctxB should NOT be canceled (different chatKey)")
	default:
	}
}

// TestChildCancelRegistryUnregister verifies that the unregister handle
// returned by Register removes the cancel func from the registry, so the
// slice does not grow unboundedly as children complete normally. A completed
// child unregistering itself must not be re-canceled by a later CancelAll.
func TestChildCancelRegistryUnregister(t *testing.T) {
	reg := NewChildCancelRegistry()
	cctx, cancel := context.WithCancel(context.Background())
	unregister := reg.Register(keyA(), cancel)

	unregister()

	// After unregister, CancelAll must report 0 outstanding and must NOT call
	// the unregistered cancel (ctx stays open).
	n := reg.CancelAll(keyA())
	if n != 0 {
		t.Errorf("CancelAll returned %d after unregister, want 0", n)
	}
	select {
	case <-cctx.Done():
		t.Error("unregistered ctx was canceled by CancelAll — unregister failed")
	default:
	}
}

// TestChildCancelRegistryCancelAllIsIdempotent verifies CancelAll can be
// called repeatedly (e.g. multiple steering retries) without panic and
// reports the outstanding count at call time.
func TestChildCancelRegistryCancelAllIdempotent(t *testing.T) {
	reg := NewChildCancelRegistry()
	_, cancel1 := context.WithCancel(context.Background())
	_, cancel2 := context.WithCancel(context.Background())
	reg.Register(keyA(), cancel1)
	reg.Register(keyA(), cancel2)

	if n := reg.CancelAll(keyA()); n != 2 {
		t.Fatalf("first CancelAll = %d, want 2", n)
	}
	// cancels already invoked → the funcs remain registered until unregistered,
	// but calling them again is safe (idempotent). Second CancelAll still sees
	// the count because unregister wasn't called.
	if n := reg.CancelAll(keyA()); n != 2 {
		t.Errorf("second CancelAll = %d, want 2 (funcs still registered)", n)
	}
	cancel1()
	cancel2()
}

// TestChildCancelRegistryReset clears a chatKey's entry (turn-end cleanup),
// preventing leaks across turns.
func TestChildCancelRegistryReset(t *testing.T) {
	reg := NewChildCancelRegistry()
	_, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	_, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	reg.Register(keyA(), cancel1)
	reg.Register(keyA(), cancel2)
	reg.Register(keyB(), cancel2)

	reg.Reset(keyA())

	if n := reg.CancelAll(keyA()); n != 0 {
		t.Errorf("after Reset(keyA), CancelAll(keyA) = %d, want 0", n)
	}
	// keyB untouched.
	if n := reg.CancelAll(keyB()); n != 1 {
		t.Errorf("Reset(keyA) should not affect keyB; CancelAll(keyB) = %d, want 1", n)
	}
}

// TestChildCancelRegistryConcurrent exercises concurrent Register/CancelAll
// to catch data races (run with -race). Mirrors the real access pattern: the
// spawn tool registers on the synchronous side while a steering retry may
// CancelAll concurrently.
func TestChildCancelRegistryConcurrent(t *testing.T) {
	reg := NewChildCancelRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, cancel := context.WithCancel(context.Background())
			unreg := reg.Register(keyA(), cancel)
			_ = reg.CancelAll(keyA())
			unreg()
		}()
	}
	wg.Wait()
}
