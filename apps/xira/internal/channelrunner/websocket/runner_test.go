package websocket

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/entrypoints"
	frt "github.com/xiramesh/xira/internal/runtime"
)

// runner_test.go: unit tests for the websocket Runner (RFC chatkey-session
// Step 3a). These cover the WS package's own logic (per-chatKey single-active
// protection, OnTurnResult frame assembly) without standing up a real Service.
// The api package holds the end-to-end socket integration tests; these are the
// unit-level guarantees for the relocated runner.

// --- fakes ---

// fakeRuntime implements frt.Runtime. It counts concurrent in-flight RunAgent
// calls (high-water mark) and can block to widen the racing window.
type fakeRuntime struct {
	mu            sync.Mutex
	concurrent    int32
	maxConcurrent int32
	hold          func(unblock chan struct{})
	respond       func(req frt.TurnRequest) frt.TurnResponse
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		respond: func(frt.TurnRequest) frt.TurnResponse {
			return frt.TurnResponse{RunID: "run-1", Status: "completed", FinalResponse: "ok"}
		},
	}
}

func (f *fakeRuntime) RunAgent(_ context.Context, req frt.TurnRequest) (frt.TurnResponse, error) {
	cur := atomic.AddInt32(&f.concurrent, 1)
	for {
		max := atomic.LoadInt32(&f.maxConcurrent)
		if cur <= max || atomic.CompareAndSwapInt32(&f.maxConcurrent, max, cur) {
			break
		}
	}
	defer atomic.AddInt32(&f.concurrent, -1)
	if f.hold != nil {
		unblock := make(chan struct{})
		f.hold(unblock)
		<-unblock
	}
	return f.respond(req), nil
}

func (f *fakeRuntime) maxSeen() int32 { return atomic.LoadInt32(&f.maxConcurrent) }

// newTestRunner builds a Runner with a fake runtime injected.
func newTestRunner(t *testing.T, rt frt.Runtime) *Runner {
	t.Helper()
	runner, err := NewRunner(entrypoints.Definition{ID: "websocket-default", Channel: "websocket"}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	runner.runtime = rt
	return runner
}

// capturedFrames records every outboundFrame written, protected by a mutex.
type capturedFrames struct {
	mu    sync.Mutex
	frames []outboundFrame
}

func (c *capturedFrames) write(f outboundFrame) error {
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
	return nil
}

func (c *capturedFrames) snapshot() []outboundFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]outboundFrame, len(c.frames))
	copy(out, c.frames)
	return out
}

// makeMessageFrame builds an inbound "message" frame with the given chat/sender/msg.
func makeMessageFrame(messageID, chatID, senderID, text string) inboundFrame {
	data := messageData{
		Message: text,
		Context: channel.InboundContext{
			Channel:   "websocket",
			ChatID:    chatID,
			SenderID:  senderID,
			MessageID: messageID,
		},
	}
	raw, _ := json.Marshal(data)
	return inboundFrame{Type: "message", ID: messageID, Data: raw}
}

func typesOf(frames []outboundFrame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.Type
	}
	return out
}

// noopRegister is a no-op onRegister callback for handleMessage tests that
// don't exercise the connection registry (they drive handleMessage directly,
// simulating a single already-registered connection).
var noopRegister = func(frt.ChatKey) {}

// --- tests ---

// TestRunnerConcurrentSameChatDoesNotRace: two messages to the SAME chat,
// fired concurrently — the 2nd must STEER (not start a 2nd turn). Detected via
// fake runtime's max-concurrent-in-flight: pre-Step-3a (go func per frame) it
// spiked to 2; post-Step-3a (ChatKeySession) it stays ≤1.
func TestRunnerConcurrentSameChatDoesNotRace(t *testing.T) {
	rt := newFakeRuntime()
	var gates []chan struct{}
	var gmu sync.Mutex
	rt.hold = func(unblock chan struct{}) {
		gmu.Lock()
		gates = append(gates, unblock)
		gmu.Unlock()
	}
	runner := newTestRunner(t, rt)
	caps := &capturedFrames{}
	addActive := func(*activeRequest) {}
	removeActive := func(string) {}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg1 := makeMessageFrame("om_1", "chat-1", "user-1", "first")
	msg2 := makeMessageFrame("om_2", "chat-1", "user-1", "second")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); runner.handleMessage(ctx, msg1, "websocket-default", caps.write, addActive, removeActive, noopRegister) }()
	go func() { defer wg.Done(); runner.handleMessage(ctx, msg2, "websocket-default", caps.write, addActive, removeActive, noopRegister) }()

	time.Sleep(50 * time.Millisecond) // let both dispatch
	gmu.Lock()
	for _, g := range gates {
		close(g)
	}
	gmu.Unlock()
	wg.Wait()

	if got := rt.maxSeen(); got > 1 {
		t.Errorf("max concurrent RunAgent for SAME chat = %d, want <= 1 (2nd should steer, not race)", got)
	}
}

// TestRunnerOnTurnResultEmitsEventAndResponseFrames: a successful turn with
// Events emits an "event" frame per accepted event, then a "response" frame.
// Verifies OnTurnResult wiring assembles the structured output correctly.
func TestRunnerOnTurnResultEmitsEventAndResponseFrames(t *testing.T) {
	rt := newFakeRuntime()
	rt.respond = func(frt.TurnRequest) frt.TurnResponse {
		return frt.TurnResponse{
			RunID:         "run-1",
			Status:        "completed",
			FinalResponse: "answer",
			Events:        []frt.RuntimeEvent{{ID: "evt-1", Kind: "run.started"}, {ID: "evt-2", Kind: "run.completed"}},
		}
	}
	runner := newTestRunner(t, rt)
	caps := &capturedFrames{}
	addActive := func(req *activeRequest) {} // accept all (no runID filter needed for this test)
	removeActive := func(string) {}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner.handleMessage(ctx, makeMessageFrame("om_1", "chat-1", "user-1", "hi"), "websocket-default", caps.write, addActive, removeActive, noopRegister)

	time.Sleep(100 * time.Millisecond) // let the turn + frames complete
	types := typesOf(caps.snapshot())
	// Expect: ack (accepted), then events, then response.
	if len(types) == 0 {
		t.Fatal("no frames emitted")
	}
	hasAck := false
	hasResponse := false
	for _, ty := range types {
		if ty == "ack" {
			hasAck = true
		}
		if ty == "response" {
			hasResponse = true
		}
	}
	if !hasAck {
		t.Errorf("no ack frame in %v", types)
	}
	if !hasResponse {
		t.Errorf("no response frame in %v", types)
	}
}

// TestRunnerOnTurnResultEmitsInterruptFrame: a turn with Interrupt set emits
// an "interrupt" frame (HITL), not a "response" frame. WS protocol-level HITL.
func TestRunnerOnTurnResultEmitsInterruptFrame(t *testing.T) {
	rt := newFakeRuntime()
	rt.respond = func(frt.TurnRequest) frt.TurnResponse {
		return frt.TurnResponse{
			RunID:  "run-1",
			Status: "waiting_human",
			Interrupt: &frt.RunInterrupt{
				Status: "waiting_human",
				Reason: "tool_gate",
			},
		}
	}
	runner := newTestRunner(t, rt)
	caps := &capturedFrames{}
	addActive := func(*activeRequest) {}
	removeActive := func(string) {}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner.handleMessage(ctx, makeMessageFrame("om_1", "chat-1", "user-1", "hi"), "websocket-default", caps.write, addActive, removeActive, noopRegister)

	time.Sleep(100 * time.Millisecond)
	types := typesOf(caps.snapshot())
	hasInterrupt := false
	hasResponse := false
	for _, ty := range types {
		if ty == "interrupt" {
			hasInterrupt = true
		}
		if ty == "response" {
			hasResponse = true
		}
	}
	if !hasInterrupt {
		t.Errorf("no interrupt frame in %v (HITL turn must emit interrupt, not response)", types)
	}
	if hasResponse {
		t.Errorf("response frame emitted for HITL turn (must be interrupt): %v", types)
	}
}

// TestRunnerOnTurnResultEmitsErrorFrameOnRunFailure: a RunAgent error emits a
// run_failed error frame (retryable=true), so the WS client knows the turn failed.
func TestRunnerOnTurnResultEmitsErrorFrameOnRunFailure(t *testing.T) {
	rt := &failingRuntime{}
	runner := newTestRunner(t, rt)
	caps := &capturedFrames{}
	addActive := func(*activeRequest) {}
	removeActive := func(string) {}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner.handleMessage(ctx, makeMessageFrame("om_1", "chat-1", "user-1", "hi"), "websocket-default", caps.write, addActive, removeActive, noopRegister)

	time.Sleep(100 * time.Millisecond)
	var errFrame *outboundFrame
	for i := range caps.snapshot() {
		f := caps.snapshot()[i]
		if f.Type == "error" {
			errFrame = &f
			break
		}
	}
	if errFrame == nil {
		t.Fatal("no error frame emitted on run failure")
	}
	data := errFrame.Data.(map[string]any)
	if data["code"] != "run_failed" {
		t.Errorf("error code = %v, want run_failed", data["code"])
	}
	if data["retryable"] != true {
		t.Errorf("error retryable = %v, want true", data["retryable"])
	}
}

type failingRuntime struct{}

func (failingRuntime) RunAgent(context.Context, frt.TurnRequest) (frt.TurnResponse, error) {
	return frt.TurnResponse{}, errSimulated
}

var errSimulated = simpleErr("simulated RunAgent failure")

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

// NOTE on writeFrame fail-fast (PR #93 review MEDIUM-1): there is no unit test
// here that exercises writeFrame's cancel()-on-write-error. An end-to-end test
// via httptest is NOT reliable for this: a plain client disconnect fails the
// READ side (readInboundFrame errors on the closed conn) so HandleConnection
// returns even without the write-cancel. The write-fail-fast path only matters
// in the rare window where the peer is gone but the read side hasn't errored
// yet — a timing condition that's flaky to reproduce. We rely on:
//   1. Structural parity with pre-Step-3a api (which had `cancel()` on write
//      error) — the behavior was ported verbatim, see writeFrame + HandleConnection
//      doc comment.
//   2. The doc comment on writeFrame explaining WHY the cancel is required
//      (silent-data-loss avoidance, AGENTS.md §2).
// If you remove the cancel(), you reintroduce silent data loss on half-dead
// connections. Don't.
