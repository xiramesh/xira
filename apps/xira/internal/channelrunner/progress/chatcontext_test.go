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

func TestProgressPolicyDefaultsAndLegacyFallback(t *testing.T) {
	policy := DefaultPolicy()
	if got := progressQuotaLimit(policy, false); got != 3 {
		t.Fatalf("default parent progress quota = %d, want 3", got)
	}
	if got := progressQuotaLimit(policy, true); got != 2 {
		t.Fatalf("default child progress quota = %d, want 2", got)
	}

	legacy := Policy{MaxMessagesPerTurn: 2}
	if got := progressQuotaLimit(legacy, false); got != 2 {
		t.Fatalf("legacy parent progress quota = %d, want 2", got)
	}
	if got := progressQuotaLimit(legacy, true); got != 2 {
		t.Fatalf("legacy child progress quota = %d, want 2", got)
	}
}

func TestSenderFunc(t *testing.T) {
	var got Message
	sender := SenderFunc(func(_ context.Context, msg Message) error {
		got = msg
		return nil
	})
	if err := sender.SendProgress(context.Background(), Message{EventID: "e1", Text: "hi"}); err != nil {
		t.Fatalf("SendProgress: %v", err)
	}
	if got.EventID != "e1" || got.Text != "hi" {
		t.Fatalf("sender got %+v", got)
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

func TestChatContextQuotaSplitsParentAndChild(t *testing.T) {
	// Capture Debug logs to assert the quota drop is observable.
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prevLogger)

	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{
		Sender: sender,
		Policy: Policy{
			MaxParentProgressMessagesPerTurn: 2,
			MaxChildProgressMessagesPerTurn:  1,
			MinInterval:                      0,
		},
	})
	cc.Start()

	// Two parent progress events fill only the parent bucket.
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
	// Child still has its own bucket, so this is delivered.
	cc.Deliver(runtime.AssistantStatus{
		MessageIDVal:         "c1",
		AgentTurnIDVal:       "aturn_child",
		ParentAgentTurnIDVal: "aturn_parent",
		Text:                 "子：搜索资料",
	})
	// Parent and child are now both full; one extra event in each bucket drops.
	cc.Deliver(runtime.AssistantStatus{
		MessageIDVal:   "p3",
		AgentTurnIDVal: "aturn_parent",
		Text:           "父：整理结论",
	})
	cc.Deliver(runtime.AssistantStatus{
		MessageIDVal:         "c2",
		AgentTurnIDVal:       "aturn_child",
		ParentAgentTurnIDVal: "aturn_parent",
		Text:                 "子：补充搜索",
	})
	time.Sleep(50 * time.Millisecond)
	cc.Stop()

	msgs := sender.getMessages()
	if len(msgs) != 3 {
		t.Errorf("delivered %d, want 3 (parent bucket 2 + child bucket 1)", len(msgs))
	}

	// The drop MUST be observable — not silent (AGENTS.md §2).
	logs := logBuf.String()
	if !strings.Contains(logs, "quota reached") {
		t.Errorf("quota drop not logged; logs:\n%s", logs)
	}
	if !strings.Contains(logs, "bucket=parent") || !strings.Contains(logs, "bucket=child") {
		t.Errorf("quota-drop logs should identify parent and child buckets; logs:\n%s", logs)
	}
}

func TestChatContextChildQuotaDoesNotStarveParent(t *testing.T) {
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{
		Sender: sender,
		Policy: Policy{
			MaxParentProgressMessagesPerTurn: 1,
			MaxChildProgressMessagesPerTurn:  1,
			MinInterval:                      0,
		},
	})
	cc.Start()

	cc.Deliver(runtime.AssistantStatus{
		MessageIDVal:         "c1",
		AgentTurnIDVal:       "aturn_child",
		ParentAgentTurnIDVal: "aturn_parent",
		Text:                 "子：搜索资料",
	})
	cc.Deliver(runtime.AssistantStatus{
		MessageIDVal:         "c2",
		AgentTurnIDVal:       "aturn_child",
		ParentAgentTurnIDVal: "aturn_parent",
		Text:                 "子：补充搜索",
	})
	cc.Deliver(runtime.AssistantStatus{
		MessageIDVal:   "p1",
		AgentTurnIDVal: "aturn_parent",
		Text:           "父：整理结论",
	})
	time.Sleep(50 * time.Millisecond)
	cc.Stop()

	msgs := sender.getMessages()
	if len(msgs) != 2 {
		t.Fatalf("delivered %d, want 2 (child bucket 1 + parent bucket 1)", len(msgs))
	}
	if !strings.Contains(msgs[1].Text, "父：整理结论") {
		t.Fatalf("parent progress should survive full child bucket, got %+v", msgs)
	}
}
