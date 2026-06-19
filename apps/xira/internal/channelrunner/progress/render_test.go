package progress

import (
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/runtime"
)

func renderEvt(kind string, payload map[string]any) runtime.RuntimeEvent {
	return runtime.RuntimeEvent{
		ID:      "evt-" + kind,
		Kind:    kind,
		Message: "raw internal message that must not leak",
		Payload: payload,
	}
}

func TestRendererRendersDelegateFailureProgress(t *testing.T) {
	r := ProgressRenderer{MaxChars: 180}
	for _, kind := range []string{"agent.delegate.failed", "agent.delegate.timeout"} {
		msg, ok := r.Render(renderEvt(kind, map[string]any{"reason": "boom"}))
		if !ok {
			t.Fatalf("kind %q should render", kind)
		}
		if msg.Kind != kind {
			t.Fatalf("kind = %q", msg.Kind)
		}
		if strings.Contains(msg.Text, "boom") {
			t.Fatalf("renderer leaked raw payload reason: %q", msg.Text)
		}
		if msg.Text == "" {
			t.Fatalf("rendered text empty for %q", kind)
		}
	}
}

func TestRendererRendersWaitingHumanWithSummary(t *testing.T) {
	r := ProgressRenderer{MaxChars: 180}
	msg, ok := r.Render(renderEvt("run.waiting_human", map[string]any{
		"summary": "Approve shipping?",
	}))
	if !ok {
		t.Fatalf("waiting_human should render")
	}
	if !strings.Contains(msg.Text, "Approve shipping?") {
		t.Fatalf("waiting_human text must contain summary: %q", msg.Text)
	}
}

func TestRendererDropsNonAllowlistedKinds(t *testing.T) {
	r := ProgressRenderer{MaxChars: 180}
	for _, kind := range []string{
		"assistant.final", // drain-only, never rendered
		"assistant.status",
		"adk.event",
		"tool.started",
		"tool.completed",
		"run.started",
		"run.finished",
		"model.policy_resolved",
	} {
		if _, ok := r.Render(renderEvt(kind, nil)); ok {
			t.Fatalf("kind %q must NOT render", kind)
		}
	}
}

func TestRendererWaitingHumanWithoutSummaryStillRenders(t *testing.T) {
	r := ProgressRenderer{MaxChars: 180}
	msg, ok := r.Render(renderEvt("run.waiting_human", nil))
	if !ok {
		t.Fatalf("waiting_human without summary should still render")
	}
	if strings.Contains(msg.Text, "raw internal") {
		t.Fatalf("renderer leaked message field: %q", msg.Text)
	}
}

func TestRendererTruncatesToMaxChars(t *testing.T) {
	r := ProgressRenderer{MaxChars: 10}
	long := strings.Repeat("长文", 100) // many runes
	msg, ok := r.Render(renderEvt("run.waiting_human", map[string]any{"summary": long}))
	if !ok {
		t.Fatalf("should render")
	}
	if countRunes(msg.Text) > 10 {
		t.Fatalf("text not truncated to MaxChars: %d runes", countRunes(msg.Text))
	}
}

func countRunes(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
