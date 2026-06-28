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

	// B must get a steered ack (not accepted) — it tells B the reply will come
	// under the active turn's request_id, not its own (scheme P).
	hasSteeredAck := false
	for _, f := range bFrames {
		if f.Type == "ack" {
			data, _ := f.Data.(map[string]any)
			status, _ := data["status"].(string)
			if status == "steered" {
				hasSteeredAck = true
			}
			if status == "accepted" {
				t.Errorf("B got accepted ack for a steered message — should be steered")
			}
		}
	}
	if !hasSteeredAck {
		t.Errorf("B frames %v: no steered ack", bTypes)
	}

	// B must receive the response frame, and its request_id must be A's
	// ("reply follows the active turn" — scheme P). B does NOT get a
	// request_id=om_B terminal.
	var respFrame *outboundFrame
	for i := range bFrames {
		if bFrames[i].Type == "response" {
			respFrame = &bFrames[i]
			break
		}
	}
	if respFrame == nil {
		t.Fatalf("connection B got frames %v, want a response frame (steering output must reach the current connection)", bTypes)
	}
	if respFrame.RequestID != "om_A" {
		t.Errorf("B's response RequestID = %q, want %q (steering reply follows the active turn, scheme P)", respFrame.RequestID, "om_A")
	}

	for _, f := range capsA.snapshot() {
		// A may have acks/early frames, but must NOT have the retry's response.
		if f.Type == "response" {
			t.Errorf("connection A received a response frame %v — steering output leaked to the superseded connection", f.Data)
		}
	}

	// B must not leak an activeRequest: B's message was steered, so B's
	// OnTurnResult never runs — the fix must not addActive for steered messages.
	activeMu.Lock()
	leaked := len(active)
	activeMu.Unlock()
	if leaked > 0 {
		t.Errorf("active map has %d leaked entries after steering (B should not own an activeRequest)", leaked)
	}
}

// TestSteeringAtomicOutcomeNoSwallow locks the round-4 CRITICAL #1 fix: the
// routing decision (started vs steered) is ATOMIC under Router's entry lock, so
// a message is never swallowed by a TOCTOU window. The old code read IsActive
// externally, then called Handle with a noop — if the active turn finished
// between those steps, Handle started a new turn with a noop callback and the
// message was lost (dedupe stuck). With the atomic outcome from Handle, B's
// message sent AFTER A's turn completes is correctly started as a new turn.
func TestSteeringAtomicOutcomeNoSwallow(t *testing.T) {
	rt := newFakeRuntime()
	// A's turn: block once so we can sequence, then succeed. Later turns (B)
	// pass through immediately (hold closes their inner chan right away).
	aUnblock := make(chan struct{})
	fired := make(chan struct{})
	firstDone := make(chan struct{})
	rt.hold = func(inner chan struct{}) {
		select {
		case <-firstDone:
			close(inner) // not the first call — release immediately
			return
		default:
		}
		close(fired)
		<-aUnblock
		close(inner)
		close(firstDone)
	}
	rt.respond = func(frt.TurnRequest) (frt.TurnResponse, error) {
		return frt.TurnResponse{RunID: "run-x", Status: "completed", FinalResponse: "ok"}, nil
	}
	runner := newTestRunner(t, rt)
	capsA := &capturedFrames{}
	capsB := &capturedFrames{}
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	handleA := runner.newConn(capsA.write, cancelA)
	handleB := runner.newConn(capsB.write, cancelB)

	mkOnRegister := func(h *wsConn) func(frt.ChatKey) {
		return func(key frt.ChatKey) { runner.registerConnKey(h, key) }
	}
	key := keyOf("chat-t", "tim")

	// A starts a turn (blocks).
	go runner.handleMessage(ctxA, makeMessageFrame("om_A", "chat-t", "tim", "first"),
		"websocket-default", capsA.write, func(*activeRequest) {}, func(string) {}, mkOnRegister(handleA))
	<-fired

	// Let A's turn COMPLETE (markComplete runs) BEFORE B sends.
	close(aUnblock)
	for i := 0; i < 300; i++ {
		if !runner.router.IsActive(key) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if runner.router.IsActive(key) {
		t.Fatal("A's turn should have completed before B sends")
	}

	// Now B sends. Since A is idle, B must START a new turn (not be swallowed).
	runner.handleMessage(ctxB, makeMessageFrame("om_B", "chat-t", "tim", "second"),
		"websocket-default", capsB.write, func(*activeRequest) {}, func(string) {}, mkOnRegister(handleB))

	// B must get an accepted ack (started), not steered.
	time.Sleep(100 * time.Millisecond)
	var gotAccepted bool
	for _, f := range capsB.snapshot() {
		if f.Type == "ack" {
			data, _ := f.Data.(map[string]any)
			if s, _ := data["status"].(string); s == "accepted" {
				gotAccepted = true
			}
		}
	}
	if !gotAccepted {
		t.Fatal("B did not get accepted ack — message may have been swallowed by a TOCTOU race")
	}
	// B must get a response (turn ran). Before the fix, noop swallowed it.
	hasResp := false
	for _, f := range capsB.snapshot() {
		if f.Type == "response" {
			hasResp = true
		}
	}
	if !hasResp {
		t.Fatal("B got no response — turn was not started (message swallowed)")
	}
	_ = handleA
	_ = handleB
}

// TestConcurrentIdleMessagesNoLeak locks the round-4 CRITICAL #1 reverse race:
// two idle messages sent concurrently to the SAME chat. With atomic outcome,
// exactly one STARTS (accepted + activeRequest) and the other is STEERED
// (steered ack, no activeRequest). Neither leaks, neither is swallowed.
func TestConcurrentIdleMessagesNoLeak(t *testing.T) {
	rt := newFakeRuntime()
	// Hold the first RunAgent so the second message definitely sees active=true.
	unblock := make(chan struct{})
	fired := make(chan struct{})
	once := sync.Once{}
	rt.hold = func(inner chan struct{}) {
		once.Do(func() { close(fired); <-unblock; close(inner) })
	}
	rt.respond = func(frt.TurnRequest) (frt.TurnResponse, error) {
		return frt.TurnResponse{RunID: "run-c", Status: "completed", FinalResponse: "done"}, nil
	}
	runner := newTestRunner(t, rt)
	caps := &capturedFrames{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle := runner.newConn(caps.write, cancel)
	onReg := func(key frt.ChatKey) { runner.registerConnKey(handle, key) }

	var activeMu sync.Mutex
	active := map[string]*activeRequest{}
	addActive := func(r *activeRequest) { activeMu.Lock(); active[r.requestID] = r; activeMu.Unlock() }
	removeActive := func(id string) { activeMu.Lock(); delete(active, id); activeMu.Unlock() }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); runner.handleMessage(ctx, makeMessageFrame("m1", "chat-c", "u", "one"), "websocket-default", caps.write, addActive, removeActive, onReg) }()
	// Ensure m1 enters RunAgent (active) before m2 routes.
	<-fired
	go func() { defer wg.Done(); runner.handleMessage(ctx, makeMessageFrame("m2", "chat-c", "u", "two"), "websocket-default", caps.write, addActive, removeActive, onReg) }()

	// Let m2 route (it should be steered). Then unblock m1.
	time.Sleep(50 * time.Millisecond)
	close(unblock)
	wg.Wait()
	time.Sleep(50 * time.Millisecond)

	// Exactly one accepted, one steered (order unspecified).
	acks := map[string]int{}
	for _, f := range caps.snapshot() {
		if f.Type == "ack" {
			data, _ := f.Data.(map[string]any)
			s, _ := data["status"].(string)
			acks[s]++
		}
	}
	if acks["accepted"] != 1 || acks["steered"] != 1 {
		t.Errorf("acks = %+v, want exactly one accepted + one steered", acks)
	}
	// The steered message must not leak an activeRequest; the started one cleans
	// up via OnTurnResult. After everything settles, active must be empty.
	activeMu.Lock()
	leaked := len(active)
	activeMu.Unlock()
	if leaked > 0 {
		t.Errorf("active map leaked %d entries after concurrent idle messages", leaked)
	}
}
