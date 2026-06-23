package progress

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// chatcontext_reset_test.go: tests that Reset does NOT deadlock (PR #51
// round 5 CRITICAL 1). Reset must signal queueCh + cancel ctx so sendLoop
// exits, otherwise senderWg.Wait() hangs forever.

func TestChatContextResetDoesNotDeadlock(t *testing.T) {
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{Sender: sender})
	cc.Start()

	// Deliver an event so the queue has something (sendLoop may be processing).
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e1", Error: "fail"})

	// Reset must complete within 5s (deadlock = hang forever).
	done := make(chan struct{})
	go func() {
		cc.Reset()
		close(done)
	}()
	select {
	case <-done:
		// Success — Reset completed.
	case <-time.After(5 * time.Second):
		t.Fatal("Reset() deadlocked: senderWg.Wait() never returned (sendLoop stuck in select)")
	}

	// After Reset, ChatContext should still work (accept new events).
	cc.Deliver(runtime.AgentTurnFailed{MessageIDVal: "e2", Error: "different error"})
	time.Sleep(50 * time.Millisecond)
	cc.Stop()

	msgs := sender.getMessages()
	// e1 was in queue during Reset (may or may not have been delivered before
	// Reset drained it). e2 was delivered after Reset. At least e2 should arrive.
	if len(msgs) == 0 {
		t.Error("no messages delivered after Reset — ChatContext broken")
	}
}

func TestChatContextResetConcurrent(t *testing.T) {
	// Concurrent Reset + Deliver must not race or deadlock.
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{Sender: sender})
	cc.Start()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			cc.Deliver(runtime.AgentTurnFailed{
				MessageIDVal: "e",
				Error:        "concurrent test",
			})
		}
	}()
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		cc.Reset()
	}()

	// If this completes, no deadlock. Use a timeout to catch hangs.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Reset + Deliver deadlocked")
	}

	cc.Stop()
}

func TestChatContextMultipleResets(t *testing.T) {
	// Multiple sequential Resets (chain steering) must not deadlock.
	sender := &testSender{}
	cc := NewChatContext(context.Background(), ChatContextConfig{Sender: sender})
	cc.Start()

	for i := 0; i < 5; i++ {
		done := make(chan struct{})
		go func() {
			cc.Reset()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("Reset #%d deadlocked", i)
		}
	}

	cc.Stop()
}
