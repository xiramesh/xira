package websocket

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	frt "github.com/xiramesh/xira/internal/runtime"
)

// steering_test.go: tests for the active-takeover steering output ownership
// fix (PR #97 round-3 review). When connection B sends a message for a ChatKey
// whose turn is active on connection A, B's message is steered (enqueued). The
// retry runs in A's ChatKeySession, but the terminal output must reach the
// CURRENT live connection (B) — not A's captured writeFrame. These tests drive
// the full path handleMessage → Router.Handle(active enqueue) → ErrSteered
// drain → OnTurnResult.

// runSteeredThenSucceed makes RunAgent return ErrSteered on the first call
// (simulating a steering checkpoint firing after B's message lands in the
// queue) and a completed turn on the second call (the retry). The first call
// blocks until the test unblocks it, giving the test a window to inject B's
// message while A's turn is mid-flight. holdFired signals when the first call
// has entered RunAgent (so the test knows A's turn is active).
//
// fakeRuntime's hold contract: the callback receives an `unblock` chan that
// RunAgent waits on AFTER hold returns. The callback must close it to let the
// call proceed. On the first call we block (waiting for the test); on later
// calls (the retry) we close immediately so the retry RunAgent doesn't stall.
func runSteeredThenSucceed(rt *fakeRuntime) (unblock func(), callCount *int32, holdFired chan struct{}) {
	var count int32
	fired := make(chan struct{})
	var firstInner chan struct{}
	var firstMu sync.Mutex
	firstDone := make(chan struct{})
	rt.hold = func(unblockInner chan struct{}) {
		select {
		case <-firstDone:
			// Not the first call (retry): release immediately.
			close(unblockInner)
			return
		default:
		}
		// First call: record the chan, signal fired, block until the test closes it.
		firstMu.Lock()
		firstInner = unblockInner
		firstMu.Unlock()
		close(fired)
		<-unblockInner
		close(firstDone)
	}
	rt.respond = func(frt.TurnRequest) (frt.TurnResponse, error) {
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			// First call: steering checkpoint fired (B's message is queued).
			return frt.TurnResponse{}, frt.ErrSteered
		}
		return frt.TurnResponse{RunID: "run-retry", Status: "completed", FinalResponse: "answer-for-B"}, nil
	}
	return func() {
		firstMu.Lock()
		defer firstMu.Unlock()
		if firstInner != nil {
			close(firstInner)
		}
	}, &count, fired
}

// TestSteeringOutputGoesToCurrentConnection is the round-3 review CRITICAL
// regression. Connection A has an active turn; connection B takes over the
// same ChatKey and sends a message (steered). After the retry, the terminal
// frame MUST reach B (the current registry connection), NOT A. Before the fix,
// A's ChatKeySession captured A's writeFrame, so the retry's output was written
// to A — B got only an ack and never its response.
//
// Also asserts B does not leak an activeRequest (B's OnTurnResult never runs
// because B's message was steered, not run as its own turn).
func TestSteeringOutputGoesToCurrentConnection(t *testing.T) {
	rt := newFakeRuntime()
	unblockFirst, callCount, holdFired := runSteeredThenSucceed(rt)
	runner := newTestRunner(t, rt)

	capsA := &capturedFrames{}
	capsB := &capturedFrames{}
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	handleA := runner.newConn(capsA.write, cancelA)
	handleB := runner.newConn(capsB.write, cancelB)

	// active map to detect B leaking an activeRequest.
	var activeMu sync.Mutex
	active := map[string]*activeRequest{}
	addActive := func(req *activeRequest) { activeMu.Lock(); active[req.requestID] = req; activeMu.Unlock() }
	removeActive := func(id string) { activeMu.Lock(); delete(active, id); activeMu.Unlock() }

	// onRegister mirrors the FIXED HandleConnection wiring: register the key,
	// do NOT cancel the displaced connection.
	mkOnRegister := func(handle *wsConn) func(frt.ChatKey) {
		return func(key frt.ChatKey) { runner.registerConnKey(handle, key) }
	}

	key := keyOf("chat-s", "sam")
	msgA := makeMessageFrame("om_A", "chat-s", "sam", "from-A")
	msgB := makeMessageFrame("om_B", "chat-s", "sam", "from-B")

	// 1. A sends first → A's turn starts (blocked in RunAgent #1).
	go runner.handleMessage(ctxA, msgA, "websocket-default", capsA.write, addActive, removeActive, mkOnRegister(handleA))

	// Wait until A's RunAgent is in-flight (hold fired).
	select {
	case <-holdFired:
	case <-time.After(time.Second):
		t.Fatal("A's turn never started (RunAgent #1 hold never fired)")
	}
	if !runner.router.IsActive(key) {
		t.Fatal("A's turn should be active in the Router")
	}

	// 2. B takes over the same ChatKey and sends a message while A is active.
	//    B's message is steered (enqueued); B must NOT start its own turn.
	runner.handleMessage(ctxB, msgB, "websocket-default", capsB.write, addActive, removeActive, mkOnRegister(handleB))

	// Registry now points to B (takeover).
	if runner.lookupConn(key) != handleB {
		t.Fatal("registry should point to connection B after takeover")
	}

	// 3. Unblock A's first RunAgent → it returns ErrSteered → drains B's message
	//    → retries → succeeds → OnTurnResult writes the terminal frame.
	unblockFirst()

	// Wait for the retry to complete (RunAgent called twice total).
	for i := 0; i < 300; i++ {
		if atomic.LoadInt32(callCount) >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Give the router goroutine a moment to run OnTurnResult + write the frame.
	time.Sleep(100 * time.Millisecond)

	// THE ASSERTION: B receives the terminal response frame (registry pointed
	// to B at retry time). A must NOT receive B's terminal frame.
	bFrames := capsB.snapshot()
	bTypes := typesOf(bFrames)
	hasResponse := false
	for _, ty := range bTypes {
		if ty == "response" {
			hasResponse = true
		}
	}
	if !hasResponse {
		t.Fatalf("connection B got frames %v, want a response frame (steering output must reach the current connection)", bTypes)
	}
	for _, f := range capsA.snapshot() {
		// A may have acks/early frames, but must NOT have the retry's response.
		if f.Type == "response" {
			t.Errorf("connection A received a response frame %v — steering output leaked to the superseded connection", f.Data)
		}
	}

	// B must not leak an activeRequest: B's message was steered, so B's
	// OnTurnResult never runs and removeActive("om_B"...) never fires — UNLESS
	// the fix prevents creating one for steered messages.
	activeMu.Lock()
	leaked := len(active)
	activeMu.Unlock()
	if leaked > 0 {
		t.Errorf("active map has %d leaked entries after steering (B should not own an activeRequest)", leaked)
	}
}
