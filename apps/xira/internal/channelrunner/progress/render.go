package progress

import (
	"strings"
	"unicode/utf8"

	"github.com/xiramesh/xira/internal/runtime"
)

// ProgressRenderer turns an allowlisted runtime event into a channel-neutral
// chat message using fixed, templated copy. It reads ONLY whitelisted fields
// (§14): kind, severity, payload.summary. Raw message/payload fields never
// reach the text, so tool args, paths, secrets and traces cannot leak.
type ProgressRenderer struct {
	MaxChars int
}

// Render returns the message for an allowlisted kind, or ok=false for anything
// else (silence notice is produced by the forwarder timer, not here;
// assistant.final is drain-only and explicitly excluded).
func (r *ProgressRenderer) Render(evt runtime.RuntimeEvent) (Message, bool) {
	text, ok := renderText(evt)
	if !ok {
		return Message{}, false
	}
	if max := r.MaxChars; max > 0 && utf8.RuneCountInString(text) > max {
		text = truncateRunes(text, max)
	}
	return Message{
		EventID: evt.ID,
		Kind:    evt.Kind,
		Text:    text,
		Level:   levelFor(evt),
	}, true
}

func renderText(evt runtime.RuntimeEvent) (string, bool) {
	switch evt.Kind {
	case "run.silence_notice":
		return "我还在处理，会在有结果或需要你确认时继续更新。", true
	case "agent.delegate.failed":
		return "子任务没有成功返回，我会改用当前上下文继续处理。", true
	case "agent.delegate.timeout":
		return "子任务超时，我会继续整理已获得的信息。", true
	case "run.waiting_human":
		summary := strings.TrimSpace(payloadString(evt, "summary"))
		if summary == "" {
			return "这里需要你确认后才能继续。", true
		}
		return "这里需要你确认后才能继续：" + summary, true
	default:
		return "", false
	}
}

func levelFor(evt runtime.RuntimeEvent) string {
	if s := strings.TrimSpace(evt.Severity); s != "" {
		return s
	}
	return "info"
}

func payloadString(evt runtime.RuntimeEvent, key string) string {
	if evt.Payload == nil {
		return ""
	}
	if v, ok := evt.Payload[key].(string); ok {
		return v
	}
	return ""
}

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
