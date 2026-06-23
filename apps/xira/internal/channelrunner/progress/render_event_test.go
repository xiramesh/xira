package progress

import (
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// render_event_test.go tests the new Event-typed renderer (per-chat-key RFC).
// This is a pure function: Event → (Message, bool). It replaces the old
// ProgressRenderer.Render(RuntimeEvent) which read from RuntimeEvent.Kind +
// Payload map. The new version type-switches on Event sealed structs.
//
// Text templates MUST match the old renderText exactly (zero behavior change
// for users — verified by comparing against render.go's old switch cases).

func TestRenderEventAgentTurnFailed(t *testing.T) {
	evt := runtime.AgentTurnFailed{
		MessageIDVal:   "e1",
		AgentTurnIDVal: "aturn_1",
		TimestampVal:   time.Now(),
		Error:          "context deadline exceeded",
	}
	msg, ok := RenderEvent(evt, 0)
	if !ok {
		t.Fatal("RenderEvent(AgentTurnFailed) ok=false, want true")
	}
	// Old text: "子任务没有成功返回，我会改用当前上下文继续处理。"
	want := "子任务没有成功返回，我会改用当前上下文继续处理。"
	if msg.Text != want {
		t.Errorf("Text = %q, want %q", msg.Text, want)
	}
	if msg.EventID != "e1" {
		t.Errorf("EventID = %q, want e1", msg.EventID)
	}
}

func TestRenderEventAgentTurnFailedTimeout(t *testing.T) {
	// AgentTurnFailed with Error containing "timeout" → timeout text
	// (old kind "agent.delegate.timeout").
	evt := runtime.AgentTurnFailed{
		MessageIDVal: "e2",
		Error:        "timeout",
	}
	msg, ok := RenderEvent(evt, 0)
	if !ok {
		t.Fatal("ok=false")
	}
	want := "子任务超时，我会继续整理已获得的信息。"
	if msg.Text != want {
		t.Errorf("Text = %q, want %q", msg.Text, want)
	}
}

func TestRenderEventHumanRequested(t *testing.T) {
	evt := runtime.HumanRequested{
		MessageIDVal: "e3",
		Question:     "请确认是否继续",
	}
	msg, ok := RenderEvent(evt, 0)
	if !ok {
		t.Fatal("ok=false")
	}
	// Old text for waiting_human with summary: "这里需要你确认后才能继续：" + summary
	want := "这里需要你确认后才能继续：请确认是否继续"
	if msg.Text != want {
		t.Errorf("Text = %q, want %q", msg.Text, want)
	}
}

func TestRenderEventHumanRequestedNoQuestion(t *testing.T) {
	evt := runtime.HumanRequested{
		MessageIDVal: "e4",
	}
	msg, ok := RenderEvent(evt, 0)
	if !ok {
		t.Fatal("ok=false")
	}
	want := "这里需要你确认后才能继续。"
	if msg.Text != want {
		t.Errorf("Text = %q, want %q", msg.Text, want)
	}
}

func TestRenderEventAssistantFinalNotRendered(t *testing.T) {
	// AssistantFinal is drain-only — not rendered as text.
	evt := runtime.AssistantFinal{
		MessageIDVal: "e5",
		FinalChars:   42,
	}
	_, ok := RenderEvent(evt, 0)
	if ok {
		t.Error("RenderEvent(AssistantFinal) should return ok=false (drain-only, not rendered)")
	}
}

func TestRenderEventUndeliverableKinds(t *testing.T) {
	// Progress kinds (AssistantStatus, ToolCalled, ToolResult) are not
	// delivered to IM — they're internal progress.
	undeliverable := []runtime.Event{
		runtime.AssistantStatus{MessageIDVal: "e6"},
		runtime.ToolCalled{MessageIDVal: "e7"},
		runtime.ToolResult{MessageIDVal: "e8"},
		runtime.AgentTurnStarted{MessageIDVal: "e9"},
		runtime.AgentTurnCompleted{MessageIDVal: "e10"},
		runtime.AgentTurnCanceled{MessageIDVal: "e11"},
		runtime.HumanResponded{MessageIDVal: "e12"},
	}
	for _, evt := range undeliverable {
		_, ok := RenderEvent(evt, 0)
		if ok {
			t.Errorf("RenderEvent(%T) should return ok=false (not delivered to IM)", evt)
		}
	}
}

func TestRenderEventMaxChars(t *testing.T) {
	// MaxChars truncates the rendered text.
	evt := runtime.AgentTurnFailed{Error: "fail"}
	msg, ok := RenderEvent(evt, 10) // very short limit
	if !ok {
		t.Fatal("ok=false")
	}
	if len([]rune(msg.Text)) > 11 { // max + ellipsis
		t.Errorf("Text not truncated: %d runes, max 10+ellipsis", len([]rune(msg.Text)))
	}
}

func TestRenderEventLevel(t *testing.T) {
	// All rendered events default to "info" level (old levelFor fallback).
	evt := runtime.AgentTurnFailed{MessageIDVal: "e1"}
	msg, _ := RenderEvent(evt, 0)
	if msg.Level != "info" {
		t.Errorf("Level = %q, want info", msg.Level)
	}
}
