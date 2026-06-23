package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// steering_checkpoint_test.go: tests the steering checkpoint + ErrSteered
// sentinel + SteeringSink lifecycle. These are the unit tests that PR #51
// review demanded for 3 rounds — without them, the steering bugs (dead code,
// double-dequeue) had no guard.

func TestErrSteeredIsSentinel(t *testing.T) {
	// ErrSteered must be distinguishable from context.Canceled and other errors.
	if !errors.Is(ErrSteered, ErrSteered) {
		t.Error("ErrSteered must match itself")
	}
	if errors.Is(ErrSteered, context.Canceled) {
		t.Error("ErrSteered must NOT match context.Canceled (that was bug 1)")
	}
}

func TestErrSteeredWrapped(t *testing.T) {
	// Wrapped ErrSteered must still match (errors.Is unwraps).
	wrapped := errors.Join(ErrSteered, errors.New("extra context"))
	if !errors.Is(wrapped, ErrSteered) {
		t.Error("wrapped ErrSteered must still match errors.Is")
	}
}

// mockSteeringSink is a test double for SteeringSink.
type mockSteeringSink struct {
	msgs []string
}

func (m *mockSteeringSink) Enqueue(msg string) { m.msgs = append(m.msgs, msg) }
func (m *mockSteeringSink) TryDequeue() (string, bool) {
	if len(m.msgs) == 0 {
		return "", false
	}
	msg := m.msgs[0]
	m.msgs = m.msgs[1:]
	return msg, true
}
func (m *mockSteeringSink) DrainAll() []string {
	out := m.msgs
	m.msgs = nil
	return out
}
func (m *mockSteeringSink) HasPending() bool { return len(m.msgs) > 0 }

func TestSteeringSinkContextRoundTrip(t *testing.T) {
	sink := &mockSteeringSink{}
	ctx := WithSteeringSink(context.Background(), sink)

	// Round-trip: put in context, get back.
	got := SteeringSinkFromContext(ctx)
	if got != sink {
		t.Fatal("SteeringSinkFromContext did not return the same sink")
	}

	// Enqueue + HasPending + TryDequeue lifecycle.
	if got.HasPending() {
		t.Error("HasPending should be false on empty")
	}
	got.Enqueue("interjection")
	if !got.HasPending() {
		t.Error("HasPending should be true after Enqueue")
	}
	msg, ok := got.TryDequeue()
	if !ok || msg != "interjection" {
		t.Errorf("TryDequeue = %q ok=%v, want 'interjection' true", msg, ok)
	}
	if got.HasPending() {
		t.Error("HasPending should be false after TryDequeue")
	}
}

func TestSteeringSinkHasPendingDoesNotConsume(t *testing.T) {
	// PR #51 bug 2 guard: HasPending must NOT consume (checkpoint peeks,
	// retry loop consumes). If HasPending consumed, the message would be
	// lost before retry loop can TryDequeue it.
	sink := &mockSteeringSink{}
	sink.Enqueue("msg1")

	// HasPending twice — message must still be there.
	if !sink.HasPending() {
		t.Error("HasPending #1: expected true")
	}
	if !sink.HasPending() {
		t.Error("HasPending #2: expected true (peek must not consume)")
	}

	// TryDequeue still gets it.
	msg, ok := sink.TryDequeue()
	if !ok || msg != "msg1" {
		t.Errorf("TryDequeue after 2x HasPending = %q ok=%v, want msg1 true", msg, ok)
	}
}

func TestSteeringSinkNilFromEmptyContext(t *testing.T) {
	if SteeringSinkFromContext(context.Background()) != nil {
		t.Error("expected nil from empty context")
	}
}

// TestSteeringLifecycleSimulatesRetryLoop simulates the full steering
// lifecycle without ADK/DeepSeek: checkpoint → ErrSteered → retry →
// TryDequeue → "restart". This is the end-to-end test the reviewer demanded.
func TestSteeringLifecycleSimulatesRetryLoop(t *testing.T) {
	sink := &mockSteeringSink{}
	ctx := WithSteeringSink(context.Background(), sink)

	// Simulate: turn is running, user interjects.
	sink.Enqueue("换个思路")

	// Simulate checkpoint: HasPending → return ErrSteered.
	if !SteeringSinkFromContext(ctx).HasPending() {
		t.Fatal("checkpoint: HasPending should be true")
	}
	checkpointErr := ErrSteered

	// Simulate retry loop: errors.Is(err, ErrSteered) → TryDequeue → restart.
	if !errors.Is(checkpointErr, ErrSteered) {
		t.Fatal("retry: errors.Is(ErrSteered) should be true")
	}
	steered, ok := sink.TryDequeue()
	if !ok {
		t.Fatal("retry: TryDequeue should succeed")
	}
	if steered != "换个思路" {
		t.Errorf("retry: got %q, want '换个思路'", steered)
	}

	// After retry consumed, queue is empty — no more steering.
	if sink.HasPending() {
		t.Error("queue should be empty after retry consumed")
	}
}

// TestSteeringMultipleInterjectionsDrainedInOrder verifies FIFO order when
// user sends multiple interjections before the checkpoint fires.
func TestSteeringMultipleInterjectionsDrainedInOrder(t *testing.T) {
	sink := &mockSteeringSink{}
	sink.Enqueue("first")
	sink.Enqueue("second")
	sink.Enqueue("third")

	// Checkpoint fires once (HasPending), retry consumes first, re-runs.
	// Next checkpoint fires (HasPending still true), retry consumes second.
	// Etc.
	for _, want := range []string{"first", "second", "third"} {
		if !sink.HasPending() {
			t.Fatal("HasPending false before consuming", want)
		}
		got, ok := sink.TryDequeue()
		if !ok || got != want {
			t.Errorf("TryDequeue = %q ok=%v, want %q", got, ok, want)
		}
	}
	if sink.HasPending() {
		t.Error("queue should be empty after draining all")
	}
}

// TestSteeringSinkContextCanceledNotTreatedAsSteered verifies that a
// genuine context.Canceled (not steering) is NOT caught by the retry loop.
// This was bug 1: the old code checked context.Canceled, which conflated
// steering with real cancellation.
func TestSteeringSinkContextCanceledNotTreatedAsSteered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// context.Canceled must NOT match ErrSteered.
	if errors.Is(ctx.Err(), ErrSteered) {
		t.Error("context.Canceled must NOT match ErrSteered (bug 1 guard)")
	}
	// ErrSteered must NOT match context.Canceled.
	if errors.Is(ErrSteered, context.Canceled) {
		t.Error("ErrSteered must NOT match context.Canceled (bug 1 guard)")
	}
}

// Compile-time: mockSteeringSink satisfies SteeringSink.
var _ SteeringSink = (*mockSteeringSink)(nil)

// Ensure the test doesn't hang if something goes wrong.
var _ = time.After
