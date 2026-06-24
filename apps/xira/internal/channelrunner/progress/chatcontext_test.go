package progress

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// ChatContext tests define the per-chat-key event delivery contract.
// ChatContext replaces Forwarder: it receives events directly (not via
// global bus subscription), renders them, applies throttle/dedupe/quota,
// and sends via the Sender. No scopeMatcher needed (per-chat-key isolation).
//
// RFC: xira-per-chat-key-architecture-rfc-v0.zh.md §2.2

// testSender captures delivered messages for assertion.
type testSender struct {
	mu       sync.Mutex
	messages []Message
}

func (s *testSender) SendProgress(_ context.Context, m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
	return nil
}

func (s *testSender) getMessages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]Message, len(s.messages))
	copy(cp, s.messages)
	return cp
}

func TestChatContextDeliversRenderedEvent(t *testing.T) {
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{
		Sender:   sender,
		MaxChars: 0,
	})
	cc.Start()
	defer cc.Stop()

	cc.Deliver(runtime.AgentTurnFailed{
		MessageIDVal: "e1",
		Error:        "something broke",
	})
	cc.Stop()

	msgs := sender.getMessages()
	if len(msgs) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(msgs))
	}
	want := "任务没有成功完成，我会改用当前上下文继续处理。"
	if msgs[0].Text != want {
		t.Errorf("Text = %q, want %q", msgs[0].Text, want)
	}
}

func TestChatContextDropsUndeliverableEvent(t *testing.T) {
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{
		Sender: sender,
	})
	cc.Start()
	defer cc.Stop()

	// AssistantStatus is progress heartbeat — not delivered to IM.
	cc.Deliver(runtime.AssistantStatus{MessageIDVal: "e1"})
	cc.Stop()

	msgs := sender.getMessages()
	if len(msgs) != 0 {
		t.Errorf("delivered %d messages, want 0 (undeliverable)", len(msgs))
	}
}

func TestChatContextAssistantFinalDrains(t *testing.T) {
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{
		Sender: sender,
	})
	cc.Start()

	// Deliver a failed event first.
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e1", Error: "fail"})
	// Then AssistantFinal → drain (subsequent events dropped).
	cc.Deliver(runtime.AssistantFinal{MessageIDVal: "e2"})
	// This event after drain should be dropped.
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e3", Error: "after drain"})
	cc.Stop()

	msgs := sender.getMessages()
	// Only the first failed event should arrive (AssistantFinal not rendered,
	// post-drain event dropped).
	if len(msgs) != 1 {
		t.Errorf("delivered %d messages, want 1 (drain drops post-final)", len(msgs))
	}
	if msgs[0].EventID != "e1" {
		t.Errorf("first message EventID = %q, want e1", msgs[0].EventID)
	}
}

func TestChatContextThrottleProgressButNotInteraction(t *testing.T) {
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{
		Sender: sender,
		Policy: Policy{
			MaxMessagesPerTurn: 2,
			MinInterval:        0,
		},
	})
	cc.Start()

	// Mix failed + HumanRequested (different kinds → different text → no dedup).
	// Quota=2 → only 2 progress delivered (HumanRequested is interaction, bypasses quota).
	// So: 3 progress events → 2 delivered, 1 HumanRequested → 1 delivered.
	cc.Deliver(runtime.HumanRequested{MessageIDVal: "h1", Question: "confirm A"})
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e1", Error: "fail"})
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e2", Error: "timeout"}) // different text (timeout)
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e3", Error: "other"})   // quota full, same text as e1 → deduped
	// Give sendLoop time to process queued events before Stop.
	time.Sleep(50 * time.Millisecond)
	cc.Stop()

	msgs := sender.getMessages()
	// h1 (interaction, no quota), e1 (progress #1), e2 (progress #2, different text).
	// e3 same text as e1 → deduped. Total: 3 delivered.
	if len(msgs) != 3 {
		t.Errorf("delivered %d, want 3 (interaction + 2 quota progress, 1 deduped)", len(msgs))
	}
}

func TestChatContextDedup(t *testing.T) {
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{
		Sender: sender,
	})
	cc.Start()
	defer cc.Stop()

	// Same kind + text → deduped.
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e1", Error: "fail"})
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e2", Error: "fail"})
	cc.Stop()

	msgs := sender.getMessages()
	if len(msgs) != 1 {
		t.Errorf("delivered %d, want 1 (deduped)", len(msgs))
	}
}

func TestChatContextStopWaitsForInFlight(t *testing.T) {
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{
		Sender: sender,
	})
	cc.Start()

	// Deliver an event, then Stop should wait for it to be sent.
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e1", Error: "fail"})
	cc.Stop()

	msgs := sender.getMessages()
	if len(msgs) != 1 {
		t.Errorf("after Stop: delivered %d, want 1 (Stop waits)", len(msgs))
	}
}

func TestChatContextDisabledWhenSenderNil(t *testing.T) {
	// No sender → ChatContext is a no-op (like Forwarder disabled).
	cc := NewChatContext(context.Background(), ChatContextConfig{
		Sender: nil,
	})
	cc.Start()
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e1"})
	cc.Stop()
	// Must not panic.
}

func TestChatContextDeliverAfterStopDropped(t *testing.T) {
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{Sender: sender})
	cc.Start()
	cc.Stop()

	// Deliver after Stop → dropped (queue closed).
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e1", Error: "fail"})
	msgs := sender.getMessages()
	if len(msgs) != 0 {
		t.Errorf("delivered %d after Stop, want 0", len(msgs))
	}
}

func TestChatContextDeliverAfterDrainDropped(t *testing.T) {
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{Sender: sender})
	cc.Start()

	cc.Deliver(runtime.AssistantFinal{MessageIDVal: "final"})
	time.Sleep(20 * time.Millisecond)
	// After drain, new events dropped.
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e1", Error: "fail"})
	cc.Stop()

	msgs := sender.getMessages()
	if len(msgs) != 0 {
		t.Errorf("delivered %d after drain, want 0", len(msgs))
	}
}

func TestChatContextQueueFullEvictsLowerPriority(t *testing.T) {
	// Fill queue beyond capacity with Droppable, then deliver Critical.
	// The Critical event should evict a Droppable.
	sender := &blockingSender{ch: make(chan struct{})} // blocks sends to keep queue full
	cc := NewChatContext(context.Background(), ChatContextConfig{Sender: sender})
	cc.Start()

	// Block the sender so queue fills up.
	// First event enters sendLoop and blocks on sender (which blocks).
	// Subsequent events fill the queue.
	for i := 0; i < chatContextQueueCapacity+5; i++ {
		cc.Deliver(runtime.AssistantStatus{MessageIDVal: "droppable"}) // Droppable priority
	}
	// Now deliver a Critical event — should evict a Droppable.
	cc.Deliver(runtime.HumanRequested{MessageIDVal: "critical", Question: "hi"})

	// Unblock sender and stop.
	close(sender.ch)
	cc.Stop()

	// We can't easily assert exact counts (timing-dependent), but the test
	// exercises the eviction path. The key: no panic, no deadlock.
}

type blockingSender struct {
	ch chan struct{}
}

func (s *blockingSender) SendProgress(_ context.Context, _ Message) error {
	<-s.ch
	return nil
}

func TestChatContextStopWithoutStart(t *testing.T) {
	// Stop before Start should not panic.
	cc := NewChatContext(context.Background(), ChatContextConfig{Sender: nil})
	cc.Stop()
}

// TestChatContextQuotaSharedParentChild pins the KNOWN LIMITATION that parent
// and spawned-child progress share a single per-turn quota (RFC #66 review §1).
//
// A chatty child can starve the parent's progress within MaxMessagesPerTurn.
// This test pins the CURRENT behavior (shared quota, child dropped when full)
// so a future change to per-source quota is a deliberate contract change, not
// an accidental regression. The drop is logged at Debug (not silent).
//
// This is NOT a bug to fix here — per-source quota is a follow-up design.
func TestChatContextQuotaSharedParentChild(t *testing.T) {
	// Capture Debug logs to assert the quota drop is observable.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prevLogger)

	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{
		Sender: sender,
		Policy: Policy{
			MaxMessagesPerTurn: 2,
			MinInterval:        0,
		},
	})
	cc.Start()

	// Two PARENT progress events fill the shared quota (distinct text → no dedup).
	cc.Deliver(runtime.AssistantStatus{
		MessageIDVal:   "p1",
		AgentTurnIDVal: "aturn_parent",
		Text:           "父：分析需求",
	})
	cc.Deliver(runtime.AssistantStatus{
		MessageIDVal:   "p2",
		AgentTurnIDVal: "aturn_parent",
		Text:           "父：编写方案",
	})
	// Third event is a CHILD progress event — shared quota is full → dropped.
	cc.Deliver(runtime.AssistantStatus{
		MessageIDVal:         "c1",
		AgentTurnIDVal:       "aturn_child",
		ParentAgentTurnIDVal: "aturn_parent",
		Text:                 "子：搜索资料",
	})
	time.Sleep(50 * time.Millisecond)
	cc.Stop()

	msgs := sender.getMessages()
	// Known limitation: only 2 delivered (parent filled the shared quota);
	// the child event is dropped.
	if len(msgs) != 2 {
		t.Errorf("delivered %d, want 2 (shared quota: child dropped when parent fills it)", len(msgs))
	}

	// The drop MUST be observable — not silent (AGENTS.md §2).
	logs := logBuf.String()
	if !strings.Contains(logs, "quota reached") {
		t.Errorf("quota drop not logged; logs:\n%s", logs)
	}
	// The dropped event is attributable (child turn id present in the log).
	if !strings.Contains(logs, "aturn_child") {
		t.Errorf("dropped child turn id missing from quota-drop log; logs:\n%s", logs)
	}
}
