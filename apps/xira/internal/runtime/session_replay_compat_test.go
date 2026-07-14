package runtime

import (
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	fsession "github.com/xiramesh/xira/internal/session"
)

// session_replay_compat_test.go:固化"老 delegate_agent-era session 能正常回放"这个
// 不变量。delegate_agent 已下线(PR #58),但老 session history 里可能有它的 tool
// call/result 记录。replayer(adkEventFromSessionMessage)是 content-based 的——只按
// msg.Kind switch,tool name 只跳过 "" 和 "exec",其余当 opaque function call 回放。
// 这个测试守住这个不变量:未来如果有人给 replayer 加 tool-name 白名单(只回放已知
// tool),delegate_agent 的老记录会被静默丢弃,这个测试会变红。

func TestSessionReplayDelegateAgentToolCallPreserved(t *testing.T) {
	// A historical delegate_agent tool call must replay as an opaque function
	// call — NOT be dropped (it's valid history the model should see).
	msg := fsession.Message{
		Kind:       fsession.MessageKindToolCall,
		ToolName:   "delegate_agent",
		ToolCallID: "old-delegate-call-1",
		Content:    `{"agent_id":"research-assistant","task":"old task"}`,
	}
	event, chars, ok := adkEventFromSessionMessage(msg, "xira-assistant")
	if !ok {
		t.Fatal("delegate_agent tool call was DROPPED on replay — old sessions would lose history")
	}
	if event == nil {
		t.Fatal("ok=true but event is nil")
	}
	if chars == 0 {
		t.Error("content chars = 0 — tool call args not counted")
	}
}

func TestSessionReplayDelegateAgentToolResultPreserved(t *testing.T) {
	// A historical delegate_agent tool result must replay as an opaque function
	// response — NOT be dropped.
	msg := fsession.Message{
		Kind:       fsession.MessageKindToolResult,
		ToolName:   "delegate_agent",
		ToolCallID: "old-delegate-call-1",
		Content:    `{"status":"completed","summary":"old result"}`,
	}
	event, chars, ok := adkEventFromSessionMessage(msg, "xira-assistant")
	if !ok {
		t.Fatal("delegate_agent tool result was DROPPED on replay — old sessions would lose history")
	}
	if event == nil {
		t.Fatal("ok=true but event is nil")
	}
	if chars == 0 {
		t.Error("content chars = 0 — tool result content not counted")
	}
}

func TestSessionReplayExecToolStillDropped(t *testing.T) {
	// The legacy "exec" tool IS still dropped (as before) — this confirms the
	// replayer's drop-list didn't accidentally widen.
	msg := fsession.Message{
		Kind:     fsession.MessageKindToolCall,
		ToolName: "exec",
		Content:  `{"command":"ls"}`,
	}
	_, _, ok := adkEventFromSessionMessage(msg, "xira-assistant")
	if ok {
		t.Error("exec tool call should be DROPPED on replay (legacy behavior), but was preserved")
	}
}

func TestSessionMessagesForRunPreservesInboundMessageFacts(t *testing.T) {
	inbound := channel.InboundContext{
		SenderID:        "ou_sender",
		SenderName:      "大铭",
		MessageID:       "om_1",
		MessageType:     "text",
		OriginalContent: `{"text":"@_user_1 请处理"}`,
		MentionTargets: []channel.MentionTarget{
			{Key: "@_user_1", ID: "ou_zhangsan", IDType: "open_id", Name: "张三"},
		},
	}
	messages := sessionMessagesForRun("@张三 请处理", inbound, "done", "agent-1", "run-1", nil, nil, "completed")
	if len(messages) < 1 {
		t.Fatal("sessionMessagesForRun returned no user message")
	}
	got := messages[0]
	if got.Content != "@张三 请处理" || got.OriginalContent != inbound.OriginalContent || got.MessageID != "om_1" || got.MessageType != "text" {
		t.Fatalf("user message facts = %+v", got)
	}
	if got.SenderID != "ou_sender" || got.SenderName != "大铭" || len(got.MentionTargets) != 1 || got.MentionTargets[0].Key != "@_user_1" {
		t.Fatalf("user identity/mentions = %+v", got)
	}
}

func TestSessionReplayUsesReadableContentNotRawPlatformPayload(t *testing.T) {
	msg := fsession.Message{
		Role:            "user",
		Kind:            fsession.MessageKindMessage,
		Content:         "@张三 请处理",
		OriginalContent: `{"text":"@_user_1 请处理"}`,
		SenderID:        "ou_sender",
		SenderName:      "大铭",
	}
	event, _, ok := adkEventFromSessionMessage(msg, "agent-1")
	if !ok || event == nil || event.LLMResponse.Content == nil || len(event.LLMResponse.Content.Parts) != 1 {
		t.Fatalf("event = %+v, ok = %v", event, ok)
	}
	if got := event.LLMResponse.Content.Parts[0].Text; got != "[大铭|ou_sender] @张三 请处理" {
		t.Fatalf("replayed text = %q", got)
	}
}
