package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// human_interpret_test.go: #107 — agent 理解后的合规 resolve 路径。
//
// human.interpret 镜像 answer_child 的异步 resolve 模式,但加四项校验
// (source 允许 / chatKey 匹配 / request 存在 pending / 无歧义)。校验在
// 同步阶段做,通过才进异步 resolve;不通过同步 rejected。绝不嵌套 turn。
//
// 这些测试 pin 四项校验 + 异步不阻塞 + source 白名单。

// seedPendingHRForInterpret 预置一个 waiting_human run + pending HR,供
// interpret 校验/resolve。返回创建的 HR。chatKey 控制是否匹配当前 turn。
func seedPendingHRForInterpret(t *testing.T, rt *Service, runID, source, chatKey string) *humanrequest.HumanRequest {
	t.Helper()
	ctx := context.Background()
	if err := rt.runs.SaveRun(TurnResponse{RunID: runID, AgentID: "xira-assistant", Status: StatusWaitingHuman}); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	kind := humanrequest.RequestFreeform
	if source == "flow_human_approval" {
		kind = humanrequest.RequestApproval
	}
	hr, err := rt.CreateHumanRequest(ctx, humanrequest.CreateRequest{
		WorkspaceID:  rt.workspace,
		WorkspaceKey: rt.WorkspaceKey(),
		RunID:        runID,
		AgentID:      "xira-assistant",
		SessionID:    "session-" + runID,
		Kind:         kind,
		Question:     "Approve the " + runID + " action?",
		Source:       source,
		ChatKey:      chatKey,
	})
	if err != nil {
		t.Fatalf("CreateHumanRequest: %v", err)
	}
	return hr
}

// waitForHRResolved polls until the HR is resolved (async resolve done) or
// timeout. Returns the resolved HR or fails.
func waitForHRResolved(t *testing.T, rt *Service, hrID string, timeout time.Duration) *humanrequest.HumanRequest {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got, err := rt.GetHumanRequest(context.Background(), hrID)
		if err == nil && got.Status == humanrequest.StatusResolved {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("HumanRequest %s not resolved within %s", hrID, timeout)
	return nil
}

// TestExecuteHumanInterpretApprovesAgentRequest: 合法 interpret(agent_request
// + chatKey 匹配 + pending) → 异步 resolve 触发,HR 最终 resolved。
func TestExecuteHumanInterpretApprovesAgentRequest(t *testing.T) {
	rt := newTestService(t, configWithStateDir(t))
	const chatKey = "feishu/chat-1/user-1"
	hr := seedPendingHRForInterpret(t, rt, "run-ar-1", "agent_request", chatKey)

	out := executeHumanInterpret(context.Background(), rt, humanInterpretInput{
		RequestID: hr.ID,
		Signal:    "approve",
		ChatKey:   chatKey,
	})
	if out["status"] != "interpreting" {
		t.Fatalf("status = %v, want 'interpreting' (async, non-blocking)", out["status"])
	}
	resolved := waitForHRResolved(t, rt, hr.ID, 5*time.Second)
	if resolved.Response == nil || resolved.Response.Kind != humanrequest.ResponseApprove {
		t.Errorf("response = %+v, want approve", resolved.Response)
	}
	// 等异步 resume 跑完(run 进 terminal),避免 TempDir 竞态。
	waitForRunTerminal(t, rt, "run-ar-1", 5*time.Second)
}

// TestExecuteHumanInterpretRejectsWrongChatKey: HR.ChatKey ≠ 当前 turn chatKey
// → rejected(防跨 chat 注入:agent 不能 interpret 别的 chat 的 HR)。
func TestExecuteHumanInterpretRejectsWrongChatKey(t *testing.T) {
	rt := newTestService(t, configWithStateDir(t))
	hr := seedPendingHRForInterpret(t, rt, "run-chat-1", "agent_request", "feishu/other-chat/other-user")

	out := executeHumanInterpret(context.Background(), rt, humanInterpretInput{
		RequestID: hr.ID,
		Signal:    "approve",
		ChatKey:   "feishu/chat-1/user-1", // 不同的 chatKey
	})
	if out["status"] != "rejected" {
		t.Fatalf("status = %v, want 'rejected' for chatKey mismatch", out["status"])
	}
	got, _ := rt.GetHumanRequest(context.Background(), hr.ID)
	if got != nil && got.Status != humanrequest.StatusPending {
		t.Fatalf("cross-chat HR should stay pending, got %v", got.Status)
	}
}

// TestExecuteHumanInterpretRejectsUnknownRequestID: 伪造的 request_id → rejected
// (prompt injection 防御:模型瞎报一个 id,查不到就拒)。
func TestExecuteHumanInterpretRejectsUnknownRequestID(t *testing.T) {
	rt := newTestService(t, configWithStateDir(t))

	out := executeHumanInterpret(context.Background(), rt, humanInterpretInput{
		RequestID: "hrq_nonexistent_injection_attempt",
		Signal:    "approve",
		ChatKey:   "feishu/chat-1/user-1",
	})
	if out["status"] != "rejected" {
		t.Fatalf("status = %v, want 'rejected' for unknown request_id", out["status"])
	}
}

// TestExecuteHumanInterpretRejectsAlreadyResolved: 非 pending(已 resolved)
// → rejected(不能 interpret 一个已经答过的 HR)。
func TestExecuteHumanInterpretRejectsAlreadyResolved(t *testing.T) {
	rt := newTestService(t, configWithStateDir(t))
	const chatKey = "feishu/chat-1/user-1"
	hr := seedPendingHRForInterpret(t, rt, "run-done-1", "agent_request", chatKey)
	// 先手动 resolve 掉
	if _, err := rt.ResolveHumanRequest(context.Background(), hr.ID, humanrequest.ResolveRequest{
		Kind: humanrequest.ResponseApprove, Actor: "test",
	}); err != nil {
		// resume 会跑(用 fake LLM,可能失败但 HR 会 resolved)。忽略 resume 错误。
		_ = err
	}

	out := executeHumanInterpret(context.Background(), rt, humanInterpretInput{
		RequestID: hr.ID,
		Signal:    "approve",
		ChatKey:   chatKey,
	})
	if out["status"] != "rejected" {
		t.Fatalf("status = %v, want 'rejected' for already-resolved HR", out["status"])
	}
}

// TestExecuteHumanInterpretRejectsMissingInput: 校验 — request_id 必填。
func TestExecuteHumanInterpretRejectsMissingInput(t *testing.T) {
	rt := newTestService(t, configWithStateDir(t))
	out := executeHumanInterpret(context.Background(), rt, humanInterpretInput{
		RequestID: "",
		Signal:    "approve",
		ChatKey:   "feishu/chat-1/user-1",
	})
	if out["status"] != "rejected" {
		t.Fatalf("status = %v, want 'rejected' for empty request_id", out["status"])
	}
}

// TestExecuteHumanInterpretRejectsInvalidSignal: signal 必须是
// approve/deny/answer/cancel 之一(防模型瞎报别的 signal)。
func TestExecuteHumanInterpretRejectsInvalidSignal(t *testing.T) {
	rt := newTestService(t, configWithStateDir(t))
	out := executeHumanInterpret(context.Background(), rt, humanInterpretInput{
		RequestID: "any",
		Signal:    "maybe", // 非法
		ChatKey:   "feishu/chat-1/user-1",
	})
	if out["status"] != "rejected" {
		t.Fatalf("status = %v, want 'rejected' for invalid signal", out["status"])
	}
}

// TestExecuteHumanInterpretRejectsUnsupportedSource: source 既不是
// agent_request/flow_human_approval(未知 source)→ rejected
// (白名单:只允许两种已知 IM-resolvable source)。
func TestExecuteHumanInterpretRejectsUnsupportedSource(t *testing.T) {
	rt := newTestService(t, configWithStateDir(t))
	const chatKey = "feishu/chat-1/user-1"
	hr := seedPendingHRForInterpret(t, rt, "run-unk-1", "mystery_source", chatKey)
	out := executeHumanInterpret(context.Background(), rt, humanInterpretInput{
		RequestID: hr.ID,
		Signal:    "approve",
		ChatKey:   chatKey,
	})
	if out["status"] != "rejected" {
		t.Fatalf("status = %v, want 'rejected' for unsupported source", out["status"])
	}
}

// TestExecuteHumanInterpretRejectsEmptyHRChatKey: HR 没记 chatKey(老数据/
// flow #112 gap)→ rejected,不跨 chat 猜(保守)。
func TestExecuteHumanInterpretRejectsEmptyHRChatKey(t *testing.T) {
	rt := newTestService(t, configWithStateDir(t))
	hr := seedPendingHRForInterpret(t, rt, "run-nochat-1", "agent_request", "") // 空 chatKey
	out := executeHumanInterpret(context.Background(), rt, humanInterpretInput{
		RequestID: hr.ID,
		Signal:    "approve",
		ChatKey:   "feishu/chat-1/user-1",
	})
	if out["status"] != "rejected" {
		t.Fatalf("status = %v, want 'rejected' when HR has no chat_key", out["status"])
	}
}

// TestSanitizeHumanInterpretInput: pin args → struct 抽取(含未知字段忽略)。
func TestSanitizeHumanInterpretInput(t *testing.T) {
	in := sanitizeHumanInterpretInput(map[string]any{
		"request_id": "  hrq_123  ",
		"signal":     "approve",
		"reasoning":  "  user agreed  ",
		"chat_key":   "should-be-ignored", // ChatKey 不从 args 取(runtime 填)
		"bogus":      "ignored",
	})
	if in.RequestID != "hrq_123" {
		t.Errorf("RequestID = %q, want hrq_123 (trimmed)", in.RequestID)
	}
	if in.Signal != "approve" {
		t.Errorf("Signal = %q, want approve", in.Signal)
	}
	if in.Reasoning != "user agreed" {
		t.Errorf("Reasoning = %q, want 'user agreed'", in.Reasoning)
	}
	if in.ChatKey != "" {
		t.Errorf("ChatKey = %q, must be empty (runtime fills it, not model args)", in.ChatKey)
	}
}

// TestSanitizeHumanInterpretInputNormalizesSignalCase: 模型可能输出
// "Approve"/"APPROVE",sanitize 归一化为小写,使后续 isValidInterpretSignal
// 通过(ResponseKind 常量是小写)。否则合法意图会被误拒。
func TestSanitizeHumanInterpretInputNormalizesSignalCase(t *testing.T) {
	for _, raw := range []string{"Approve", "APPROVE", "Approve ", "  DENy  "} {
		in := sanitizeHumanInterpretInput(map[string]any{"request_id": "hrq", "signal": raw})
		if !isValidInterpretSignal(in.Signal) {
			t.Errorf("sanitize+validate rejected signal %q (normalized to %q) — models emit mixed case", raw, in.Signal)
		}
	}
}

// TestIsValidInterpretSignal: pin 四个合法 + 非法 signal。
func TestIsValidInterpretSignal(t *testing.T) {
	for _, s := range []string{"approve", "deny", "answer", "cancel"} {
		if !isValidInterpretSignal(s) {
			t.Errorf("isValidInterpretSignal(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "maybe", "yes", "APPROVE", "approve "} {
		if isValidInterpretSignal(s) {
			t.Errorf("isValidInterpretSignal(%q) = true, want false", s)
		}
	}
}

// TestExecuteHumanInterpretDoesNotBlockTurn: 返回值永远不是 waiting_human —
// interpret 不 suspend run,turn 正常继续。
//
// 注意:合法 interpret 会触发异步 resume(一次 generate),它在 detached
// goroutine 写 run 目录。测试必须等 resume 跑完再返回,否则 TempDir cleanup
// 和 resume 写盘竞态("directory not empty")。模式同 answer_child_test.go:143-155。
func TestExecuteHumanInterpretDoesNotBlockTurn(t *testing.T) {
	rt := newTestService(t, configWithStateDir(t))
	const chatKey = "feishu/chat-1/user-1"
	hr := seedPendingHRForInterpret(t, rt, "run-nb-1", "agent_request", chatKey)

	out := executeHumanInterpret(context.Background(), rt, humanInterpretInput{
		RequestID: hr.ID,
		Signal:    "answer",
		Reasoning: "user said yes",
		ChatKey:   chatKey,
	})
	if out["status"] == StatusWaitingHuman {
		t.Fatal("interpret must NOT return waiting_human — it must not suspend the run")
	}
	if out["status"] != "interpreting" {
		t.Errorf("status = %v, want 'interpreting'", out["status"])
	}
	// 等异步 resume 跑完(run 进 terminal),避免 TempDir 竞态。
	waitForRunTerminal(t, rt, "run-nb-1", 5*time.Second)
}

// waitForRunTerminal polls until the run reaches completed/failed/steered
// (resume done), so the detached goroutine stops writing before TempDir
// cleanup. Mirrors answer_child_test.go:145-155, plus a grace period after
// terminal status because deliverResumeFinal runs after SaveRun (the goroutine
// must fully exit, not just hit terminal) — without the grace, TempDir
// cleanup races with the tail writes ("directory not empty").
func waitForRunTerminal(t *testing.T, rt *Service, runID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		run, err := rt.RunStore().Load(runID)
		if err == nil && (run.Status == "completed" || run.Status == "failed" || run.Status == "steered" || run.Status == StatusWaitingHuman) {
			// Give the detached goroutine's tail (deliverResumeFinal etc.)
			// time to finish after SaveRun flipped the status.
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// configWithStateDir 返回一个带独立 stateDir 的 Config(隔离测试)。
func configWithStateDir(t *testing.T) Config {
	t.Helper()
	return Config{StateDir: filepath.Join(t.TempDir(), "state")}
}
