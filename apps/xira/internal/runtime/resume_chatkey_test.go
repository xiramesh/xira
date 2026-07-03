package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// resume_chatkey_test.go: #114 — resume 路径必须 WithChatKey,否则 resume turn
// 内链式 interpret(#107 读 chatKeyStringFromContext)失效。
//
// 数据源用 req.ChatKey(HR 一等持久化字段),NOT SessionScope —— PR #115 review
// CRITICAL:SessionScope 有损(ToLower + canonicalSenderID 重写),会让含大写 ID
// 或 IdentityLinks 映射的 chatKey 与持久化 HR.ChatKey 不匹配,链式 interpret
// 静默失效。req.ChatKey 与 store 比较用的是同一个值,零变换必然匹配。
//
// 这些测试覆盖关键场景:含大写 ID、nil req、空 ChatKey(老数据),并 pin
// ParseChatKey 的逆变换(含 sender 含 "/" 的边界)。

// TestParseChatKey round-trip:String() → ParseChatKey 还原,含 sender "/" 边界。
func TestParseChatKey(t *testing.T) {
	cases := []struct {
		name string
		s    string
		want ChatKey
		ok   bool
	}{
		{"plain", "feishu/chat-9/user-1", ChatKey{"feishu", "chat-9", "user-1"}, true},
		{"uppercase id (CRITICAL 1)", "ilink/Wxid_Abc/User_X", ChatKey{"ilink", "Wxid_Abc", "User_X"}, true},
		{"sender with slash preserved", "ws/topic-3/a/b", ChatKey{"ws", "topic-3", "a/b"}, true},
		{"empty stays empty", "", ChatKey{}, false},
		{"two segments invalid", "feishu/chat", ChatKey{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseChatKey(c.s)
			if ok != c.ok {
				t.Errorf("ParseChatKey(%q) ok = %v, want %v", c.s, ok, c.ok)
			}
			if ok && got != c.want {
				t.Errorf("ParseChatKey(%q) = %+v, want %+v", c.s, got, c.want)
			}
		})
	}
}

// TestWithChatKeyFromRequestPreservesOriginalCase 是 #114 + PR #115 review
// CRITICAL 1 的核心:从 req.ChatKey 还原的 chatKey 必须**保留原始大小写**,
// 不能像 SessionScope 那样 ToLower。否则含大写 ID(ilink wxid / 其他 channel)
// 的 chatKey 与持久化 HR.ChatKey 不匹配,链式 interpret 静默 reject。
func TestWithChatKeyFromRequestPreservesOriginalCase(t *testing.T) {
	req := &humanrequest.HumanRequest{ChatKey: "ilink/Wxid_Abc/User_X"}
	ctx := withChatKeyFromRequest(context.Background(), req)
	got := chatKeyStringFromContext(ctx)
	if got != "ilink/Wxid_Abc/User_X" {
		t.Errorf("chatKey round-trip lost case: got %q, want ilink/Wxid_Abc/User_X (ToLower via SessionScope would give ilink/wxid_abc/user_x → mismatch)", got)
	}
}

// TestWithChatKeyFromRequestHandlesNilAndEmpty 防御:nil req / 空 ChatKey(老数据、
// flow #112 gap)不崩,返回原 ctx(chatKey 空,hydration/interpret 优雅 no-op)。
func TestWithChatKeyFromRequestHandlesNilAndEmpty(t *testing.T) {
	cases := []struct {
		name string
		req  *humanrequest.HumanRequest
	}{
		{"nil req", nil},
		{"empty ChatKey", &humanrequest.HumanRequest{ChatKey: ""}},
		{"malformed ChatKey", &humanrequest.HumanRequest{ChatKey: "feishu/only-two"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := withChatKeyFromRequest(context.Background(), c.req)
			if got := chatKeyStringFromContext(ctx); got != "" {
				t.Errorf("expected empty chatKey for %s, got %q", c.name, got)
			}
		})
	}
}

// TestResumeDirectHumanRequestSetsChatKeyOnCtx 驱动真实 resumeDirectHumanRequest
// (smoke):证 withChatKeyFromRequest 接进 resume 不崩。chatKey 注入的正确性
// (含大小写/IdentityLinks 场景)由 TestWithChatKeyFromRequestPreservesOriginalCase
// 单元覆盖;端到端链式 interpret 由 #107 live test 覆盖。
func TestResumeDirectHumanRequestSetsChatKeyOnCtx(t *testing.T) {
	rt := newResumeDeadlineTestService(t)
	// 用含大写的 chatKey —— 若实现错误地走 SessionScope(ToLower),resume 内
	// chatKey 会被小写化。但本 smoke 不直接断言 chatKey(ctx 内部),只证 resume
	// 跑通不崩。大小写保留由上面的单元测试 pin。
	if err := rt.runs.SaveRun(TurnResponse{
		RunID:   "resume-ck-smoke-1",
		AgentID: "xira-assistant",
		Status:  StatusWaitingHuman,
		Message: "deploy",
	}); err != nil {
		t.Fatal(err)
	}
	hr, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		WorkspaceID: rt.workspace, WorkspaceKey: rt.WorkspaceKey(),
		RunID: "resume-ck-smoke-1", AgentID: "xira-assistant", SessionID: "s-smoke-1",
		Kind: humanrequest.RequestFreeform, Question: "Which window?",
		Source: "agent_request", ChatKey: "ilink/Wxid_Abc/User_X",
	})
	if err != nil {
		t.Fatal(err)
	}
	hr.Response = &humanrequest.HumanResponse{
		RequestID: hr.ID, Kind: humanrequest.ResponseAnswer, Actor: "parent_agent", Message: "Tuesday",
	}
	parentCtx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if err := rt.resumeDirectHumanRequest(parentCtx, hr); err != nil {
		t.Fatalf("resumeDirectHumanRequest: %v", err)
	}
	waitForRunTerminal(t, rt, "resume-ck-smoke-1", 2*time.Second)
}


