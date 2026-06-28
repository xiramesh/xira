package progress

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// im_renderer_test.go: tests IMEventRenderer (the reusable IM renderer that
// replaces ChatContext's forced render). These mirror ChatContext's quota/dedup
// tests — the behavior ilink/feishu rely on — but feed FLAT RuntimeEvents (what
// RawEventSink delivers) instead of sealed Events. Pinning 1:1 parity so the
// ilink/feishu switch is behavior-equivalent.

// --- test capture ---

type rendererCapture struct {
	mu   sync.Mutex
	text []string
}

func (c *rendererCapture) send(_ context.Context, text string) error {
	c.mu.Lock()
	c.text = append(c.text, text)
	c.mu.Unlock()
	return nil
}

// snapshot returns the captured texts (caller must have flushed async sends
// via r.waitSends() first if testing the async renderer).
func (c *rendererCapture) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.text))
	copy(out, c.text)
	return out
}

// newStartedRenderer builds a renderer, starts its sendLoop, and registers
// Stop via t.Cleanup — so tests can just DeliverRaw + waitSends without
// managing the sendLoop lifecycle each time.
func newStartedRenderer(t *testing.T, send func(context.Context, string) error, policy Policy) *IMEventRenderer {
	t.Helper()
	r := NewIMEventRenderer(send, policy)
	r.Start()
	t.Cleanup(r.Stop)
	return r
}

// flat builds a flat RuntimeEvent with the given kind + optional payload. For
// fields the mapping reads off the struct (not payload), use flatMsg instead.
func flat(kind string, payload map[string]any) runtime.RuntimeEvent {
	return runtime.RuntimeEvent{
		ID:      kind + "-" + time.Now().Format("150405.000000"),
		Kind:    kind,
		Time:    time.Now(),
		Payload: payload,
	}
}

// flatMsg builds a flat RuntimeEvent whose .Message field is set (used for
// HumanRequested.Question and AssistantStatus.Text — the mapping reads .Message
// for those, not the payload).
func flatMsg(kind, msg string) runtime.RuntimeEvent {
	return runtime.RuntimeEvent{
		ID:      kind + "-" + time.Now().Format("150405.000000"),
		Kind:    kind,
		Time:    time.Now(),
		Message: msg,
	}
}

// --- tests ---

// TestIMRendererRendersFailedAndTimeout: agent.delegate.failed + timeout render
// to their distinct localized texts (the core render behavior).
func TestIMRendererRendersFailedAndTimeout(t *testing.T) {
	cap := &rendererCapture{}
	r := newStartedRenderer(t, cap.send, DefaultPolicy())
	r.DeliverRaw(flat("agent.delegate.failed", map[string]any{"error": "boom"}))
	r.DeliverRaw(flat("agent.delegate.timeout", map[string]any{"error": "context deadline exceeded (timeout)"}))

	r.waitSends()
	got := cap.snapshot()
	if len(got) != 2 {
		t.Fatalf("delivered %d, want 2", len(got))
	}
	if !strings.Contains(got[0], "没有成功完成") {
		t.Errorf("failed text = %q, want 没有成功完成 variant", got[0])
	}
	if !strings.Contains(got[1], "超时") {
		t.Errorf("timeout text = %q, want 超时 variant", got[1])
	}
}

// TestIMRendererThrottleProgressButNotInteraction: quota=2 caps progress, but
// HumanRequested (waiting_human) bypasses quota. Mirrors
// TestChatContextThrottleProgressButNotInteraction 1:1.
func TestIMRendererThrottleProgressButNotInteraction(t *testing.T) {
	cap := &rendererCapture{}
	r := newStartedRenderer(t, cap.send, Policy{MaxMessagesPerTurn: 2})

	// interaction first (bypasses quota), then 3 progress (2 distinct text +
	// 1 dup) → 2 delivered + the interaction = 3 total.
	r.DeliverRaw(flatMsg("run.waiting_human", "confirm A"))                           // interaction
	r.DeliverRaw(flat("agent.delegate.failed", map[string]any{"error": "fail"}))
	r.DeliverRaw(flat("agent.delegate.timeout", map[string]any{"error": "timeout"})) // distinct text
	r.DeliverRaw(flat("agent.delegate.failed", map[string]any{"error": "other"}))   // dup text of "fail" → deduped

	r.waitSends()
	got := cap.snapshot()
	// interaction (1) + 2 quota progress (failed distinct from timeout) = 3.
	// The 4th is deduped (same kind+text as the failed). Total 3.
	if len(got) != 3 {
		t.Errorf("delivered %d, want 3 (interaction + 2 quota progress, 1 deduped): %+v", len(got), got)
	}
}

// TestIMRendererDedup: same kind + text → deduped. Mirrors TestChatContextDedup.
func TestIMRendererDedup(t *testing.T) {
	cap := &rendererCapture{}
	r := newStartedRenderer(t, cap.send, DefaultPolicy())
	r.DeliverRaw(flat("agent.delegate.failed", map[string]any{"error": "fail"}))
	r.DeliverRaw(flat("agent.delegate.failed", map[string]any{"error": "fail"})) // identical → deduped

	r.waitSends()
	got := cap.snapshot()
	if len(got) != 1 {
		t.Errorf("delivered %d, want 1 (deduped)", len(got))
	}
}

// TestIMRendererQuotaCapsProgress: with MaxMessagesPerTurn=2, three distinct
// progress events → only 2 delivered (3rd dropped, logged at Debug).
func TestIMRendererQuotaCapsProgress(t *testing.T) {
	cap := &rendererCapture{}
	r := newStartedRenderer(t, cap.send, Policy{MaxMessagesPerTurn: 2})
	// Three distinct kinds so dedup doesn't kick in.
	r.DeliverRaw(flat("agent.delegate.failed", map[string]any{"error": "fail"}))
	r.DeliverRaw(flat("agent.delegate.timeout", map[string]any{"error": "timeout"}))
	r.DeliverRaw(flat("tool.started", map[string]any{"tool_name": "fs.read"})) // quota full → dropped

	r.waitSends()
	got := cap.snapshot()
	if len(got) != 2 {
		t.Errorf("delivered %d, want 2 (quota cap)", len(got))
	}
}

// TestIMRendererSkipsNonSignalAndLifecycle: non-signal kinds (observability) and
// lifecycle (run.started) are not rendered. RenderEvent returns ok=false for
// them; IMEventRenderer must skip silently (no panic, no spurious text).
func TestIMRendererSkipsNonSignalAndLifecycle(t *testing.T) {
	cap := &rendererCapture{}
	r := newStartedRenderer(t, cap.send, DefaultPolicy())
	// run.started is a signal that maps to AgentTurnStarted (lifecycle, not rendered).
	r.DeliverRaw(flat("run.started", nil))
	// A non-signal observability kind (won't even reach the sink in dispatchEvent,
	// but IMEventRenderer must be robust if called directly).
	r.DeliverRaw(flat("llm.call.completed", nil))

	r.waitSends()
	got := cap.snapshot()
	if len(got) != 0 {
		t.Errorf("delivered %d, want 0 (lifecycle/non-signal not rendered): %+v", len(got), got)
	}
}

// TestIMRendererNilSafe: nil receiver / nil send must not panic.
func TestIMRendererNilSafe(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("panicked: %v", rec)
		}
	}()
	var r *IMEventRenderer
	r.DeliverRaw(flat("agent.delegate.failed", nil)) // nil receiver
	r = NewIMEventRenderer(nil, DefaultPolicy())
	r.DeliverRaw(flat("agent.delegate.failed", nil)) // nil send
}

// TestIMRendererTruncatesByMaxChars: long text is truncated to MaxChars runes.
func TestIMRendererTruncatesByMaxChars(t *testing.T) {
	cap := &rendererCapture{}
	r := newStartedRenderer(t, cap.send, Policy{MaxChars: 5})
	long := strings.Repeat("x", 200)
	r.DeliverRaw(flatMsg("assistant.status", long))

	r.waitSends()
	got := cap.snapshot()
	if len(got) != 1 {
		t.Fatalf("delivered %d, want 1", len(got))
	}
	// 5 runes + ellipsis ("…") = 6 runes total in byte length, but check it's short.
	if len([]rune(got[0])) > 6 {
		t.Errorf("text not truncated: %d runes", len([]rune(got[0])))
	}
}
