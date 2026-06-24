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
// RFC #66 (spawn-parent-child-comm-rfc §3): AssistantStatus + ToolCalled are
// rendered to IM so users see a spawned child's progress (and the parent's).
// Child events — those with a ParentAgentTurnID distinct from AgentTurnID —
// get a "（子任务）" source prefix so users can attribute them.

// childSourcePrefix marks an event as coming from a spawned child turn (not
// the agent the user is directly talking to). Applied when
// ParentAgentTurnID is set and differs from AgentTurnID.
const childSourcePrefix = "（子任务）"

// isChildEvent reports whether evt originates from a spawned child turn: its
// ParentAgentTurnID is set and distinct from its own AgentTurnID. Root turns
// have an empty ParentAgentTurnID; a child carries the parent's turn id.
func isChildEvent(evt runtime.Event) bool {
	parent := string(evt.ParentAgentTurnID())
	self := string(evt.AgentTurnID())
	return parent != "" && parent != self
}

// RenderEvent renders an Event into a channel-neutral progress Message.
// Returns ok=false for events that are not delivered to IM (lifecycle signals,
// AssistantFinal drain-only, ToolResult).
//
// maxChars > 0 truncates the text to that many runes (with ellipsis).
func RenderEvent(evt runtime.Event, maxChars int) (Message, bool) {
	text, kind, ok := renderEventText(evt)
	if !ok {
		return Message{}, false
	}
	// Child events get a source-attribution prefix so the user can tell they
	// came from a spawned child, not the agent they're talking to. Applied
	// after rendering so it prefixes the final text exactly once.
	if isChildEvent(evt) {
		text = childSourcePrefix + text
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
		// Base text is source-neutral; the （子任务） prefix (applied in
		// RenderEvent for child turns) marks a spawned-child failure. The old
		// Phase-2 text "子任务没有成功返回" was wrong for root turns (the root
		// turn IS the task) and duplicated the prefix for child turns
		// ("（子任务）子任务没有成功返回") — review #69 §2.
		if strings.Contains(strings.ToLower(e.Error), "timeout") {
			return "任务超时，我会继续整理已获得的信息。", "agent.delegate.timeout", true
		}
		return "任务没有成功完成，我会改用当前上下文继续处理。", "agent.delegate.failed", true

	case runtime.HumanRequested:
		question := strings.TrimSpace(e.Question)
		if question == "" {
			return "这里需要你确认后才能继续。", "run.waiting_human", true
		}
		return "这里需要你确认后才能继续：" + question, "run.waiting_human", true

	case runtime.AssistantStatus:
		// Progress heartbeat (RFC #66): surface what the agent — parent or
		// spawned child — is doing. Empty text is skipped (no heartbeat to show).
		text := strings.TrimSpace(e.Text)
		if text == "" {
			return "", "", false
		}
		return text, "assistant.status", true

	case runtime.ToolCalled:
		// Tool invocation (RFC #66): surface the tool name. ToolResult (the
		// completion) is NOT rendered — it pairs 1:1 with ToolCalled and
		// rendering both would double the noise.
		name := strings.TrimSpace(e.ToolName)
		if name == "" {
			return "", "", false
		}
		return "调用工具：" + name, "tool.called", true

	case runtime.AssistantFinal:
		// Drain-only — not rendered as text. The caller uses it as a signal
		// to stop delivering (like the old forwarder.drain()).
		return "", "", false

	default:
		// Turn lifecycle (AgentTurnStarted/Completed/Canceled) and
		// HumanResponded are not delivered to IM in the v0 progress feed —
		// they're lifecycle signals, not user-facing progress. ToolResult is
		// handled above (not rendered); the rest fall through here.
		return "", "", false
	}
}
