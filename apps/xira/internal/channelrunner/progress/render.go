package progress

import (
	"encoding/json"
	"fmt"
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
		// Honest: state the timeout fact with the effective ceiling. Do NOT claim
		// "整理已获得的信息" — the child may have failed with no result, in which
		// case that phrase is a lie.
		ms := payloadInt(evt, "effective_max_duration_ms")
		if ms > 0 {
			return fmt.Sprintf("子任务超时（上限 %s），未返回结构化结果。", humanDurationMS(ms)), true
		}
		return "子任务超时，未返回结构化结果。", true
	case "agent.delegate.allowed":
		target := strings.TrimSpace(payloadString(evt, "target_agent_id"))
		ms := payloadInt(evt, "effective_max_duration_ms")
		if target != "" && ms > 0 {
			return fmt.Sprintf("已委派给 %s（最长 %s）。", target, humanDurationMS(ms)), true
		}
		if target != "" {
			return fmt.Sprintf("已委派给 %s。", target), true
		}
		return "已委派子任务。", true
	case "agent.delegate.started":
		return "子任务已启动。", true
	case "agent.delegate.completed":
		return "子任务完成。", true
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

// payloadInt reads a numeric payload field across the types JSON unmarshaling
// produces (float64, json.Number, int, int64).
func payloadInt(evt runtime.RuntimeEvent, key string) int64 {
	if evt.Payload == nil {
		return 0
	}
	switch v := evt.Payload[key].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	}
	return 0
}

// humanDurationMS renders a millisecond count as a compact human string (e.g.
// "120 分钟", "30 秒"). Used in delegate progress so the user sees the real
// post-clamp deadline.
func humanDurationMS(ms int64) string {
	if ms <= 0 {
		return "未知"
	}
	const minute = 60_000
	const hour = 60 * minute
	if ms >= hour && ms%hour == 0 {
		return fmt.Sprintf("%d 小时", ms/hour)
	}
	if ms >= minute {
		return fmt.Sprintf("%d 分钟", ms/minute)
	}
	return fmt.Sprintf("%d 秒", ms/1000)
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
