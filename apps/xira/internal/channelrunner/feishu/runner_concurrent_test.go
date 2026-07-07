package feishu

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/xiramesh/xira/internal/entrypoints"
	frt "github.com/xiramesh/xira/internal/runtime"
)

// runner_concurrent_test.go: TDD tests for feishu's ChatKeySession
// integration (#86). These pin per-chatKey single-active-turn semantics that
// were VIOLATED before this PR (lark ws dispatcher spawns one goroutine per
// inbound message; two messages in the same chat raced as two concurrent
// RunAgent calls). After integration, the 2nd message steers instead.
//
// Strategy: inject a fake frt.Runtime into the Runner (Runner.runtime is typed
// frt.Runtime, the interface added in Step 1). The fake counts concurrent
// in-flight RunAgent calls and can block to widen the racing window. This
// avoids standing up a full Service (which requires entrypoints+agents) — we
// only care about turn routing, not LLM execution.

// --- fakes ---

// fakeRuntime implements frt.Runtime. It counts concurrent in-flight
// RunAgent calls (high-water mark) and optionally blocks on a per-chatKey
// gate so a turn can be held open to observe steering.
type fakeRuntime struct {
	mu            sync.Mutex
	concurrent    int32
	maxConcurrent int32
	// hold, if non-nil, is invoked inside RunAgent; the call blocks until the
	// returned unblock func is called. nil = return immediately.
	hold func(unblock chan struct{})
	// respond returns the TurnResponse for each call.
	respond func(req frt.TurnRequest) frt.TurnResponse
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		respond: func(frt.TurnRequest) frt.TurnResponse {
			return frt.TurnResponse{Status: "completed", FinalResponse: "ok"}
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

// newFeishuTestRunner builds a Runner with a fake runtime injected. The lark
// API client is redirected to a closed port (SendFinal fails fast — these
// tests assert routing, not delivery; SendFinal errors are logged+swallowed).
func newFeishuTestRunner(t *testing.T, rt frt.Runtime) *Runner {
	t.Helper()
	def := entrypoints.Definition{
		ID:        "feishu-test",
		Channel:   "feishu",
		AppID:     "cli_test_app",
		AppSecret: "test_secret",
	}
	runner, err := NewRunner(def, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	runner.runtime = rt // inject fake (field is frt.Runtime interface)
	// Redirect lark API client to a closed port so SendFinal fails fast.
	runner.client = lark.NewClient("cli_test_app", "test_secret",
		lark.WithOpenBaseUrl("http://127.0.0.1:9"), // port 9: discard, refuses connections
	)
	return runner
}

func strPtr(s string) *string { return &s }

// makeP2Message builds a minimal lark P2MessageReceiveV1 event for chatID/sender/text.
func makeP2Message(chatID, senderID, messageID, text string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{UserId: strPtr(senderID)},
			},
			Message: &larkim.EventMessage{
				MessageId:   strPtr(messageID),
				ChatId:      strPtr(chatID),
				ChatType:    strPtr("p2p"),
				MessageType: strPtr("text"),
				Content:     strPtr(`{"text":"` + text + `"}`),
			},
		},
	}
}

// chatKeyFor reproduces the ChatKey handleMessageReceive derives. extractSenderID
// returns SenderId.UserId, so SenderID in the key equals the raw senderID.
func chatKeyFor(chatID, senderID string) frt.ChatKey {
	return frt.ChatKey{Channel: "feishu", ChatID: chatID, SenderID: senderID}
}

// TestFeishuConcurrentSameChatDoesNotRace: two messages to the SAME chat,
// fired concurrently — the 2nd must STEER (not start a 2nd turn). Detected via
// fake runtime's max-concurrent-in-flight: if two turns raced, it spikes to 2;
// with Router integration the 2nd steers → stays at 1.
// RED pre-integration (no Router, races to 2); GREEN post-integration (≤1).
func TestFeishuConcurrentSameChatDoesNotRace(t *testing.T) {
	rt := newFakeRuntime()
	// Hold each call ~60ms to guarantee overlap if both goroutines ran.
	var gates []chan struct{}
	var gmu sync.Mutex
	rt.hold = func(unblock chan struct{}) {
		gmu.Lock()
		gates = append(gates, unblock)
		gmu.Unlock()
	}
	runner := newFeishuTestRunner(t, rt)

	const chatID, senderID = "oc_chat1", "userA"
	msg1 := makeP2Message(chatID, senderID, "om_1", "first")
	msg2 := makeP2Message(chatID, senderID, "om_2", "second")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = runner.handleMessageReceive(ctx, msg1) }()
	go func() { defer wg.Done(); _ = runner.handleMessageReceive(ctx, msg2) }()

	// Give both a chance to dispatch, then release any held turns.
	time.Sleep(50 * time.Millisecond)
	gmu.Lock()
	for _, g := range gates {
		close(g)
	}
	gmu.Unlock()
	wg.Wait()

	if got := rt.maxSeen(); got > 1 {
		t.Errorf("max concurrent RunAgent for SAME chat = %d, want <= 1 (2nd should steer, not race)", got)
	}
	// Wait for the async runTurn goroutine to finish before TempDir cleanup.
	// Without this, the goroutine can still be writing to the state dir when
	// t.TempDir() RemoveAll runs → "directory not empty" (flaky on slow CI).
	waitTurnInactive(t, runner, chatKeyFor(chatID, senderID))
}

// TestFeishuConcurrentDifferentChatsDoRunInParallel: two messages to DIFFERENT
// chats must NOT serialize each other — per-chatKey isolation means different
// chatKeys run concurrently. Guards against over-serialization (a global lock
// would break this).
func TestFeishuConcurrentDifferentChatsDoRunInParallel(t *testing.T) {
	rt := newFakeRuntime()
	var gates []chan struct{}
	var gmu sync.Mutex
	rt.hold = func(unblock chan struct{}) {
		gmu.Lock()
		gates = append(gates, unblock)
		gmu.Unlock()
	}
	runner := newFeishuTestRunner(t, rt)

	msg1 := makeP2Message("oc_chat1", "userA", "om_1", "first")
	msg2 := makeP2Message("oc_chat2", "userB", "om_2", "second")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = runner.handleMessageReceive(ctx, msg1) }()
	go func() { defer wg.Done(); _ = runner.handleMessageReceive(ctx, msg2) }()

	time.Sleep(50 * time.Millisecond) // let both turns enter & overlap
	gmu.Lock()
	for _, g := range gates {
		close(g)
	}
	gmu.Unlock()
	wg.Wait()

	if got := rt.maxSeen(); got < 2 {
		t.Errorf("max concurrent RunAgent for DIFFERENT chats = %d, want >= 2 (per-chatKey isolation must allow parallel)", got)
	}
	// Wait for both async runTurn goroutines before TempDir cleanup (#137).
	waitTurnInactive(t, runner, chatKeyFor("oc_chat1", "userA"))
	waitTurnInactive(t, runner, chatKeyFor("oc_chat2", "userB"))
}

// TestFeishuSecondMessageSteersIntoQueue: when a turn is active for chatKey,
// the 2nd message lands in the SteeringQueue (cooperative interrupt material).
func TestFeishuSecondMessageSteersIntoQueue(t *testing.T) {
	rt := newFakeRuntime()
	turnActive := make(chan struct{})
	turnRelease := make(chan struct{})
	rt.hold = func(unblock chan struct{}) {
		close(turnActive) // signal: turn has started
		<-turnRelease     // hold the turn open
		close(unblock)    // then let RunAgent return
	}
	runner := newFeishuTestRunner(t, rt)

	const chatID, senderID = "oc_chat1", "userA"
	msg1 := makeP2Message(chatID, senderID, "om_1", "first")
	msg2 := makeP2Message(chatID, senderID, "om_2", "interjection")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = runner.handleMessageReceive(ctx, msg1) }()
	<-turnActive // wait for turn 1 to be in-flight

	// 2nd message while turn 1 is active → must steer, return immediately.
	_ = runner.handleMessageReceive(ctx, msg2)

	// Release turn 1 so the goroutine can finish.
	close(turnRelease)

	// Wait for turn 1 to become inactive before asserting + letting TempDir
	// cleanup run. Without this, the async goroutine can still be writing to
	// the state dir when t.TempDir() tries to RemoveAll → "directory not empty"
	// (flaky on slow CI runners — see #137).
	waitTurnInactive(t, runner, chatKeyFor(chatID, senderID))

	// The steered interjection must be sitting in the SteeringQueue.
	sq := runner.router.SteeringQueue(chatKeyFor(chatID, senderID))
	msgs := sq.DrainAll()
	if len(msgs) != 1 || msgs[0] != "interjection" {
		t.Errorf("steering queue after 2nd msg = %v, want [interjection]", msgs)
	}
}

// TestFeishuDedupeCompleteOnSuccess: successful turn → DedupeComplete fires
// (the dedupe entry is retained for TTL against lark redelivery). Detected by
// the fake runtime completing normally (non-empty final) and checking the
// dedupe store's state.
func TestFeishuDedupeCompleteOnSuccess(t *testing.T) {
	// Use an EMPTY final response: per ChatKeySession semantics, empty final
	// counts as success (intentional silence) → DedupeComplete. This also
	// avoids invoking SendFinal (which would hit the port-9 lark stub and
	// fail, turning the turn into a failure→Forget). A non-empty-final success
	// test would require a real lark client fake, out of scope here.
	rt := &fakeRuntime{respond: func(frt.TurnRequest) frt.TurnResponse {
		return frt.TurnResponse{Status: "completed", FinalResponse: "   "} // empty → success, no SendFinal
	}}
	runner := newFeishuTestRunner(t, rt)

	msg := makeP2Message("oc_chat1", "userA", "om_success", "hello")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = runner.handleMessageReceive(ctx, msg)
	waitTurnInactive(t, runner, chatKeyFor("oc_chat1", "userA"))

	// After a successful (empty-final) turn, the dedupe key should be in
	// "completed" state (Begin on the same key returns false = still tracked,
	// not forgotten).
	dk := runner.messageDedupeKey("om_success")
	if runner.messages.Begin(dk, time.Now()) {
		t.Error("dedupe key not retained after success (Begin succeeded = entry was forgotten, not completed)")
	}
}

// TestFeishuDedupeForgetOnRunError: failed turn → DedupeForget fires (entry
// deleted, allowing retry). Detected by the fake runtime returning an error.
func TestFeishuDedupeForgetOnRunError(t *testing.T) {
	runner := newFeishuTestRunner(t, &failingRuntime{})

	msg := makeP2Message("oc_chat1", "userA", "om_fail", "hello")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = runner.handleMessageReceive(ctx, msg)
	waitTurnInactive(t, runner, chatKeyFor("oc_chat1", "userA"))

	// After a failed turn with Forget, the dedupe key must be GONE (Begin
	// succeeds = entry was forgotten, retry allowed).
	dk := runner.messageDedupeKey("om_fail")
	if !runner.messages.Begin(dk, time.Now()) {
		t.Error("dedupe key retained after failure (Begin failed = entry still tracked, want forgotten for retry)")
	}
}

// waitTurnInactive polls the Router until the turn for chatKey is no longer
// active (markComplete flipped it). Necessary because Session.Handle is
// non-blocking — the turn's deferred DedupeComplete/Forget runs async.
func waitTurnInactive(t *testing.T, runner *Runner, key frt.ChatKey) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !runner.router.IsActive(key) {
			// Give the deferred dedupe callback a moment to run after
			// markComplete (defers fire in LIFO; DedupeComplete is early
			// but not necessarily last).
			time.Sleep(10 * time.Millisecond)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("turn for %s never became inactive", key)
}

// failingRuntime always returns an error.
type failingRuntime struct{}

func (failingRuntime) RunAgent(context.Context, frt.TurnRequest) (frt.TurnResponse, error) {
	return frt.TurnResponse{}, errors.New("simulated RunAgent failure")
}

// TestFeishuRejectsUnauthorizedSenderSilently (#121): when AllowedSenderIDs is
// set and a sender outside the list messages the bot, the message is silently
// ignored (handleMessageReceive returns nil) and RunAgent is NEVER called.
// This pins the dedupe-before + silent-ignore contract.
func TestFeishuRejectsUnauthorizedSenderSilently(t *testing.T) {
	rt := newFakeRuntime()
	def := entrypoints.Definition{
		ID:               "feishu-allowlist",
		Channel:          "feishu",
		AppID:            "cli_test_app",
		AppSecret:        "test_secret",
		AllowedSenderIDs: []string{"ou_allowed"},
	}
	runner, err := NewRunner(def, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	runner.runtime = rt
	runner.client = lark.NewClient("cli_test_app", "test_secret",
		lark.WithOpenBaseUrl("http://127.0.0.1:9"),
	)
	// Sender NOT in allowlist, no ownerResolver → must be rejected.
	err = runner.handleMessageReceive(context.Background(), makeP2Message("c1", "ou_blocked", "m1", "hi"))
	if err != nil {
		t.Fatalf("handleMessageReceive returned error for unauthorized sender (should be silent nil): %v", err)
	}
	if rt.maxSeen() != 0 {
		t.Fatalf("RunAgent was called %d times for unauthorized sender (should be 0)", rt.maxSeen())
	}
	// Sanity: authorized sender DOES trigger RunAgent.
	err = runner.handleMessageReceive(context.Background(), makeP2Message("c1", "ou_allowed", "m2", "hi"))
	if err != nil {
		t.Fatalf("handleMessageReceive authorized sender error: %v", err)
	}
	// Wait for the async turn to complete (more reliable than time.Sleep,
	// also prevents TempDir cleanup race on slow CI — #137).
	waitTurnInactive(t, runner, chatKeyFor("c1", "ou_allowed"))
	if rt.maxSeen() != 1 {
		t.Fatalf("RunAgent should be called once for authorized sender, got %d", rt.maxSeen())
	}
}
