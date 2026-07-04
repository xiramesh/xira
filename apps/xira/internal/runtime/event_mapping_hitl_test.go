package runtime

import (
	"strings"
	"testing"
	"time"
)

// event_mapping_hitl_test.go: #109 — HumanRequested.Question 必须从 payload
// 取可读内容(summary / question),而非开发期内部术语 message。
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

// TestHumanRequestedQuestionFallbackToMessage: payload 里既无 summary 也无
// question → fallback 到 message(不崩,保留旧行为兼容)。
func TestHumanRequestedQuestionFallbackToMessage(t *testing.T) {
	evt := helperRuntimeEvent("run.waiting_human", "fallback message", map[string]any{})
	got, _ := runtimeEventToEvent(evt)
	hr := got.(HumanRequested)
	if hr.Question != "fallback message" {
		t.Errorf("Question = %q, want fallback to message when payload has no summary/question", hr.Question)
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
