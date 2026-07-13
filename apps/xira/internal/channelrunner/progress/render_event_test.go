package progress

import (
	"strings"
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
	// Source-neutral base text (review #69 §2): the old "子任务没有成功返回"
	// was wrong for root turns and duplicated the （子任务） prefix for child
	// turns. A root turn (no ParentAgentTurnID) renders without a prefix.
	want := "任务没有成功完成，我会改用当前上下文继续处理。"
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
	want := "任务超时，我会继续整理已获得的信息。"
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

func TestRenderEventHumanRequestedHonorsDeliveryPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name string
		evt  runtime.HumanRequested
	}{
		{name: "owner never leaks into origin chat", evt: runtime.HumanRequested{Question: "Owner secret?", ResponderType: "owner", SignalKind: "run.waiting_human", DeliveryStatus: "failed"}},
		{name: "already delivered is not duplicated", evt: runtime.HumanRequested{Question: "Choose", ResponderType: "current_sender", SignalKind: "run.waiting_human", DeliveryStatus: "sent"}},
		{name: "creation signal waits for run-level fallback", evt: runtime.HumanRequested{RequestID: "hrq-1", Question: "Choose", ResponderType: "current_sender", SignalKind: "human.request.created", DeliveryStatus: "failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := RenderEvent(tc.evt, 0); ok {
				t.Fatalf("RenderEvent(%+v) rendered private/duplicate request", tc.evt)
			}
		})
	}

	fallback := runtime.HumanRequested{RequestID: "hrq-1", Question: "Choose", ResponderType: "current_sender", SignalKind: "run.waiting_human", DeliveryStatus: "failed"}
	msg, ok := RenderEvent(fallback, 0)
	if !ok || !strings.Contains(msg.Text, "Choose") {
		t.Fatalf("failed delivery fallback = (%+v, %v)", msg, ok)
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
	// Turn-lifecycle (AgentTurnStarted/Completed/Canceled) and HumanResponded
	// are not delivered to IM in the v0 progress feed — they're lifecycle
	// signals, not user-facing progress. (AssistantStatus/ToolCalled/ToolResult
	// ARE rendered now — see TestRenderEventAssistantStatus / TestRenderEventToolCalled.)
	undeliverable := []runtime.Event{
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

// TestRenderEventAssistantStatus verifies AssistantStatus (progress heartbeat)
// is rendered to IM (RFC #66: spawn child progress visibility). Parent events
// render with no source prefix; child events (ParentAgentTurnID set) render
// with a source attribution so users can tell which agent produced them.
func TestRenderEventAssistantStatus(t *testing.T) {
	// Parent (root turn) status — no prefix.
	evt := runtime.AssistantStatus{
		MessageIDVal:   "s1",
		AgentTurnIDVal: "aturn_parent",
		Text:           "正在分析需求",
	}
	msg, ok := RenderEvent(evt, 0)
	if !ok {
		t.Fatal("RenderEvent(AssistantStatus) ok=false, want true")
	}
	if msg.Text != "正在分析需求" {
		t.Errorf("parent status Text = %q, want %q", msg.Text, "正在分析需求")
	}
}

// TestRenderEventToolCalled verifies ToolCalled is rendered to IM with the
// tool name, and child tool calls get a source prefix.
func TestRenderEventToolCalled(t *testing.T) {
	evt := runtime.ToolCalled{
		MessageIDVal:   "t1",
		AgentTurnIDVal: "aturn_parent",
		ToolName:       "web_search",
	}
	msg, ok := RenderEvent(evt, 0)
	if !ok {
		t.Fatal("RenderEvent(ToolCalled) ok=false, want true")
	}
	if !strings.Contains(msg.Text, "web_search") {
		t.Errorf("tool called Text = %q, want it to contain the tool name", msg.Text)
	}
}

// TestRenderEventAgentTurnFailedRootVsChild verifies AgentTurnFailed text
// distinguishes a root-turn failure from a child-turn failure (review #69 §2):
//   - root turn failing must NOT say "子任务" (it IS the task the user asked of)
//   - child turn failing must not duplicate the prefix: with the （子任务）
//     source prefix applied, the base text must drop "子任务" to avoid
//     "（子任务）子任务没有成功返回".
func TestRenderEventAgentTurnFailedRootVsChild(t *testing.T) {
	// Root turn failed: no ParentAgentTurnID. The generic Phase-2 text "子任务
	// 没有成功返回" was wrong for root turns — it's the turn the user is
	// talking to, not a sub-task. Render as the agent itself failing.
	rootEvt := runtime.AgentTurnFailed{
		MessageIDVal:   "f1",
		AgentTurnIDVal: "aturn_root",
		Error:          "model error",
	}
	rootMsg, ok := RenderEvent(rootEvt, 0)
	if !ok {
		t.Fatal("root AgentTurnFailed ok=false")
	}
	if strings.Contains(rootMsg.Text, "子任务") {
		t.Errorf("root turn failure text contains \"子任务\": %q — root turn IS the task, not a sub-task", rootMsg.Text)
	}

	// Child turn failed: ParentAgentTurnID set. With the source prefix applied,
	// the base text must not itself contain "子任务" (would read
	// "（子任务）子任务没有成功返回").
	childEvt := runtime.AgentTurnFailed{
		MessageIDVal:         "f2",
		AgentTurnIDVal:       "aturn_child",
		ParentAgentTurnIDVal: "aturn_root",
		Error:                "model error",
	}
	childMsg, ok := RenderEvent(childEvt, 0)
	if !ok {
		t.Fatal("child AgentTurnFailed ok=false")
	}
	if !strings.Contains(childMsg.Text, "（子任务）") {
		t.Errorf("child failure missing source prefix: %q", childMsg.Text)
	}
	// The "子任务" should appear at most once (in the prefix), not duplicated.
	if strings.Count(childMsg.Text, "子任务") > 1 {
		t.Errorf("child failure text duplicates \"子任务\": %q", childMsg.Text)
	}
}

// TestRenderEventChildSourcePrefix verifies a child event (ParentAgentTurnID
// non-empty AND distinct from AgentTurnID) gets a source-attribution prefix
// so the user can tell it came from a spawned child, not the parent they're
// talking to. RFC #66 / spawn-parent-child-comm-rfc §3.
func TestRenderEventChildSourcePrefix(t *testing.T) {
	// Child event: AgentTurnID is the child's turn, ParentAgentTurnID is the
	// root turn the user is talking to.
	evt := runtime.AssistantStatus{
		MessageIDVal:         "s2",
		AgentTurnIDVal:       "aturn_child",
		ParentAgentTurnIDVal: "aturn_parent",
		Text:                 "正在搜索资料",
	}
	msg, ok := RenderEvent(evt, 0)
	if !ok {
		t.Fatal("ok=false")
	}
	// A child event must be visually distinct from a parent event. The prefix
	// marks it as coming from a spawned child (not the agent the user is
	// directly talking to).
	if msg.Text == "正在搜索资料" {
		t.Error("child status rendered with no source attribution — indistinguishable from a parent event")
	}
	if !strings.Contains(msg.Text, "正在搜索资料") {
		t.Errorf("child status Text = %q, want it to contain the original text", msg.Text)
	}
}

// TestRenderEventToolResultNotRendered verifies ToolResult (tool completion)
// is NOT rendered — only the call is surfaced, not its result. ToolResult
// pairs 1:1 with ToolCalled and rendering both would double the noise.
func TestRenderEventToolResultNotRendered(t *testing.T) {
	evt := runtime.ToolResult{MessageIDVal: "tr1", ToolName: "web_search"}
	_, ok := RenderEvent(evt, 0)
	if ok {
		t.Error("RenderEvent(ToolResult) should return ok=false (result not rendered, only the call)")
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
