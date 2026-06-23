package progress

import (
	"strings"
	"unicode/utf8"

	"github.com/xiramesh/xira/internal/runtime"
)

// truncateRunes truncates s to max runes, appending an ellipsis if truncated.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	out := make([]rune, 0, max)
	for _, r := range s {
		if len(out) >= max-1 {
			break
		}
		out = append(out, r)
	}
	return string(out) + "…"
}

// render_event.go: RenderEvent is the Event-typed pure function renderer
// (per-chat-key RFC #48). It replaces the old ProgressRenderer.Render which
// consumed RuntimeEvent. This version type-switches on Event sealed structs.
//
// Text templates are EXACTLY the same as the old renderText (zero behavior
// change for users). The only difference: input type (Event vs RuntimeEvent)
// and field access (typed struct fields vs Payload map).

// RenderEvent renders an Event into a channel-neutral progress Message.
// Returns ok=false for events that are not delivered to IM (progress
// heartbeats, lifecycle signals, AssistantFinal drain-only).
//
// maxChars > 0 truncates the text to that many runes (with ellipsis).
func RenderEvent(evt runtime.Event, maxChars int) (Message, bool) {
	text, kind, ok := renderEventText(evt)
	if !ok {
		return Message{}, false
	}
	if maxChars > 0 && utf8.RuneCountInString(text) > maxChars {
		text = truncateRunes(text, maxChars)
	}
	return Message{
		EventID: evt.ID(),
		Kind:    kind,
		Text:    text,
		Level:   "info",
	}, true
}

// renderEventText type-switches on Event sealed structs. Returns (text, kind, ok).
// kind is the stable string identifier for the Message (for dedup key).
func renderEventText(evt runtime.Event) (string, string, bool) {
	switch e := evt.(type) {
	case runtime.AgentTurnFailed:
		if strings.Contains(strings.ToLower(e.Error), "timeout") {
			return "子任务超时，我会继续整理已获得的信息。", "agent.delegate.timeout", true
		}
		return "子任务没有成功返回，我会改用当前上下文继续处理。", "agent.delegate.failed", true

	case runtime.HumanRequested:
		question := strings.TrimSpace(e.Question)
		if question == "" {
			return "这里需要你确认后才能继续。", "run.waiting_human", true
		}
		return "这里需要你确认后才能继续：" + question, "run.waiting_human", true

	case runtime.AssistantFinal:
		// Drain-only — not rendered as text. The caller uses it as a signal
		// to stop delivering (like the old forwarder.drain()).
		return "", "", false

	default:
		// All other Event types (AssistantStatus, ToolCalled, ToolResult,
		// AgentTurnStarted, AgentTurnCompleted, AgentTurnCanceled,
		// HumanResponded) are not delivered to IM in the v0 progress feed.
		return "", "", false
	}
}
