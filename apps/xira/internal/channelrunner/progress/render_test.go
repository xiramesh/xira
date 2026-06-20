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

// TestRendererLevelCarriesSeverity: a populated Severity propagates to the
// rendered message Level; an empty Severity falls back to "info".
func TestRendererLevelCarriesSeverity(t *testing.T) {
	r := ProgressRenderer{MaxChars: 180}
	evt := renderEvt("agent.delegate.failed", nil)
	evt.Severity = "warning"
	msg, _ := r.Render(evt)
	if msg.Level != "warning" {
		t.Fatalf("level = %q, want warning", msg.Level)
	}
	// Empty/whitespace severity -> info.
	evt2 := renderEvt("agent.delegate.failed", nil)
	evt2.Severity = "   "
	msg2, _ := r.Render(evt2)
	if msg2.Level != "info" {
		t.Fatalf("level = %q, want info for empty severity", msg2.Level)
	}
}

// TestRendererWaitingHumanSummaryNonStringIsIgnored: a summary payload that is
// not a string (e.g. a number) is ignored, and the generic prompt renders.
func TestRendererWaitingHumanSummaryNonStringIsIgnored(t *testing.T) {
	r := ProgressRenderer{MaxChars: 180}
	msg, ok := r.Render(renderEvt("run.waiting_human", map[string]any{"summary": 42}))
	if !ok {
		t.Fatalf("should render")
	}
	if strings.Contains(msg.Text, "42") {
		t.Fatalf("non-string summary leaked into text: %q", msg.Text)
	}
}

// TestRendererNoTruncationWhenWithinLimit: text at or below MaxChars is returned
// verbatim (covers the truncateRunes early-return branch).
func TestRendererNoTruncationWhenWithinLimit(t *testing.T) {
	r := ProgressRenderer{MaxChars: 180}
	msg, ok := r.Render(renderEvt("agent.delegate.failed", nil))
	if !ok {
		t.Fatalf("should render")
	}
	// Short templated text well under 180 — must not gain a truncation marker.
	if strings.Contains(msg.Text, "…") {
		t.Fatalf("short text was needlessly truncated: %q", msg.Text)
	}
}

func countRunes(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
