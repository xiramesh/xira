package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// event_mapping_hitl_test.go: #109 — HumanRequested.Question 必须从 payload
// 取可读内容(summary / tool),而非开发期内部术语 message。
//
// 现场证据:run 20260703-024952 用户在飞书收到
//   "这里需要你确认后才能继续：runtime tool confirmation required"
//   "这里需要你确认后才能继续：agent run waiting for human input"
// —— message 是 log 字符串,真问题在 payload(summary / tool)但 mapping 没读。
//
// 本测试 pin:runtimeEventToEvent 对 run.waiting_human / human.request.created
// 映射时,HumanRequested.Question 优先取 payload,取不到才 fallback message。

// helperRuntimeEvent 构造带 payload 的 RuntimeEvent,减少重复。
func helperRuntimeEvent(kind, message string, payload map[string]any) RuntimeEvent {
	return RuntimeEvent{Kind: kind, ID: "e1", Time: time.Now(), RunID: "run_1", Message: message, Payload: payload}
}

// TestHumanRequestedQuestionFromSummary: run.waiting_human 的 payload 含
// summary(= waitingHumanSummary,已是真问题)→ Question 必须取 summary,不是
// 写死的 "agent run waiting for human input"。
func TestHumanRequestedQuestionFromSummary(t *testing.T) {
	evt := helperRuntimeEvent("run.waiting_human",
		"agent run waiting for human input",
		map[string]any{"summary": "Which deployment window should I use?", "human_requests": 1})
	got, ok := runtimeEventToEvent(evt)
	if !ok {
		t.Fatal("runtimeEventToEvent returned ok=false")
	}
	hr, isHR := got.(HumanRequested)
	if !isHR {
		t.Fatalf("got %T, want HumanRequested", got)
	}
	if hr.Question != "Which deployment window should I use?" {
		t.Errorf("Question = %q, want the payload summary (not the internal message)", hr.Question)
	}
}

// TestHumanRequestedQuestionFromTool: human.request.created (runtime_tool_gate)
// 的 payload 含 tool 名 → Question 必须含 tool 名(可读),不是写死的
// "runtime tool confirmation required"。
func TestHumanRequestedQuestionFromTool(t *testing.T) {
	evt := helperRuntimeEvent("human.request.created",
		"runtime tool confirmation required",
		map[string]any{
			"human_request_id": "hr_1",
			"source":           "runtime_tool_gate",
			"tool":             "write_file",
		})
	got, ok := runtimeEventToEvent(evt)
	if !ok {
		t.Fatal("runtimeEventToEvent returned ok=false")
	}
	hr := got.(HumanRequested)
	if !strings.Contains(strings.ToLower(hr.Question), "write_file") {
		t.Errorf("Question = %q, want it to mention the tool name write_file (readable)", hr.Question)
	}
	if strings.Contains(hr.Question, "runtime tool confirmation required") {
		t.Errorf("Question = %q, must NOT be the raw internal message", hr.Question)
	}
}

// TestHumanRequestedQuestionFallbackToMessage: payload 里既无 summary 也无
// tool → fallback 到 message(不崩,保留旧行为兼容)。
func TestHumanRequestedQuestionFallbackToMessage(t *testing.T) {
	evt := helperRuntimeEvent("run.waiting_human", "fallback message", map[string]any{})
	got, _ := runtimeEventToEvent(evt)
	hr := got.(HumanRequested)
	if hr.Question != "fallback message" {
		t.Errorf("Question = %q, want fallback to message when payload has no summary/tool", hr.Question)
	}
}

// TestHumanRequestedQuestionFromAgentRequest: agent_request 来源的
// human.request.created,payload 无 tool(纯问答),Question 应可读。
// agent_request 的 message 是 "human request created"(可接受),但如果 payload
// 带了 question 字段(LLM 写的),应优先用它。
func TestHumanRequestedQuestionFromAgentRequest(t *testing.T) {
	evt := helperRuntimeEvent("human.request.created",
		"human request created",
		map[string]any{
			"source":   "agent_request",
			"question": "你要修的是哪个 repo?",
		})
	got, _ := runtimeEventToEvent(evt)
	hr := got.(HumanRequested)
	// agent_request payload 带 question → 优先取它
	if !strings.Contains(hr.Question, "哪个 repo") {
		t.Errorf("Question = %q, want the agent's natural-language question from payload", hr.Question)
	}
}

// TestHumanRequestedQuestionToolWithTarget: runtime_tool_gate payload 带 tool +
// target → Question 拼成 "确认执行 <tool>: <target>"(用户能看到具体文件)。
func TestHumanRequestedQuestionToolWithTarget(t *testing.T) {
	evt := helperRuntimeEvent("human.request.created",
		"runtime tool confirmation required",
		map[string]any{
			"human_request_id": "hr_1",
			"source":           "runtime_tool_gate",
			"tool":             "write_file",
			"target":           "task-20260608-lark-cli-audit.md",
		})
	got, _ := runtimeEventToEvent(evt)
	hr := got.(HumanRequested)
	want := "确认执行 write_file: task-20260608-lark-cli-audit.md"
	if hr.Question != want {
		t.Errorf("Question = %q, want %q", hr.Question, want)
	}
}

// TestShellRunCommandNeverLeaksToIM 是 review 第 3 条 WARNING 的回归门:
// 含凭据的 shell command 经 payload → humanRequestedQuestion → Question,
// 绝不进 IM。这是反向泄漏断言(此前只有正向"取到 target"测试,泄漏面无门)。
func TestShellRunCommandNeverLeaksToIM(t *testing.T) {
	secret := "Bearer SECRET-DO-NOT-LEAK-12345"
	evt := helperRuntimeEvent("human.request.created",
		"runtime tool confirmation required",
		map[string]any{
			"human_request_id": "hr_1",
			"source":           "runtime_tool_gate",
			"tool":             "shell.run",
			"target":           actionTargetSummary(&humanrequest.ActionSnapshot{ToolName: "shell.run", Arguments: map[string]any{"command": "curl -H 'Authorization: " + secret + "' https://api.example.com"}}),
		})
	got, _ := runtimeEventToEvent(evt)
	hr := got.(HumanRequested)
	if strings.Contains(hr.Question, secret) {
		t.Errorf("SHELL CREDENTIAL LEAKED into HumanRequested.Question (→ IM): %q", hr.Question)
	}
	if strings.Contains(hr.Question, "curl") {
		t.Errorf("shell command body leaked into Question: %q (should only say 确认执行 shell.run)", hr.Question)
	}
	// 应该只渲染到 tool 名级别
	if !strings.Contains(hr.Question, "shell.run") {
		t.Errorf("Question should still name the tool: %q", hr.Question)
	}
}

// TestActionTargetSummary: per-tool 提取 + basename + 截断 + 未知工具。
func TestActionTargetSummary(t *testing.T) {
	cases := []struct {
		name     string
		snapshot *humanrequest.ActionSnapshot
		want     string
	}{
		{
			"write_file basename",
			&humanrequest.ActionSnapshot{ToolName: "write_file", Arguments: map[string]any{"path": "/vault/work/xiaoyi/projects/fujian/tasks/task-lark.md"}},
			"task-lark.md",
		},
		{
			"edit_file basename",
			&humanrequest.ActionSnapshot{ToolName: "edit_file", Arguments: map[string]any{"path": "relative/path/file.go"}},
			"file.go",
		},
		{
			"command.run program",
			&humanrequest.ActionSnapshot{ToolName: "command.run", Arguments: map[string]any{"program": "git"}},
			"git",
		},
		{
			"shell.run never renders command (credential leak guard)",
			&humanrequest.ActionSnapshot{ToolName: "shell.run", Arguments: map[string]any{"command": "curl -H 'Authorization: Bearer SECRET' https://api.example.com"}},
			"", // shell commands carry inline credentials; render nothing
		},
		{
			"unknown tool empty",
			&humanrequest.ActionSnapshot{ToolName: "custom_tool", Arguments: map[string]any{"foo": "bar"}},
			"",
		},
		{
			"nil snapshot",
			nil,
			"",
		},
		{
			"missing path arg",
			&humanrequest.ActionSnapshot{ToolName: "write_file", Arguments: map[string]any{}},
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := actionTargetSummary(c.snapshot); got != c.want {
				t.Errorf("actionTargetSummary = %q, want %q", got, c.want)
			}
		})
	}
}

