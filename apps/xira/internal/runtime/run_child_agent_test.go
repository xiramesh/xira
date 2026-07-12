package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
	fsession "github.com/xiramesh/xira/internal/session"
)

// run_child_agent_test.go: RunChildAgent is the shared child-run entry point
// (called by spawn_turn's serviceSpawnTarget.Run). Phase 6a (#55) deleted the
// delegate tests that previously covered it (90% → 0%). This restores direct
// coverage via a stubbed DeepSeek client — no live LLM needed.

func TestRunChildAgentCompletesChildTurn(t *testing.T) {
	stateRoot := t.TempDir()
	// Stub LLM: child returns a final text response immediately.
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return deepSeekHTTPResponse(deepSeekTextResponse("child did the work")), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       stateRoot,
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	caller := agents.BuiltinXiraAssistant()
	target := agents.BuiltinResearchAssistant()
	parentBase := runtimeEventBase{
		RunID:    "parent-run-1",
		AgentID:  caller.ID,
		Channel:  "test",
		SenderID: "user-1",
	}
	req := childAgentRequest{
		ParentBase:  parentBase,
		ParentRunID: "parent-run-1",
		ChildRunID:  "child-run-1",
		ToolCallID:  "tool-call-1",
		Target:      target,
		Message:     "do the research task",
		SessionMode: "ephemeral_worker",
		Depth:       1,
	}

	resp, err := rt.RunChildAgent(context.Background(), req)
	if err != nil {
		t.Fatalf("RunChildAgent error: %v", err)
	}
	if resp.Status != "completed" {
		t.Errorf("Status = %q, want 'completed'", resp.Status)
	}
	if resp.FinalResponse == "" {
		t.Error("FinalResponse empty — child LLM output not surfaced")
	}
}

func TestRunChildAgentRecordsChildRun(t *testing.T) {
	// RunChildAgent must init + persist the child run (InitRun is called
	// inside). Verify the run is recorded.
	stateRoot := t.TempDir()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return deepSeekHTTPResponse(deepSeekTextResponse("done")), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       stateRoot,
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	target := agents.BuiltinResearchAssistant()
	req := childAgentRequest{
		ParentBase:  runtimeEventBase{RunID: "parent-run-2", AgentID: agents.BuiltinXiraAssistant().ID, Channel: "test"},
		ParentRunID: "parent-run-2",
		ChildRunID:  "child-run-recorded",
		Target:      target,
		Message:     "task",
		Depth:       1,
	}

	if _, err := rt.RunChildAgent(context.Background(), req); err != nil {
		t.Fatalf("RunChildAgent error: %v", err)
	}

	run, err := rt.RunStore().Load("child-run-recorded")
	if err != nil {
		t.Fatalf("child run not recorded: %v", err)
	}
	if run.AgentID != target.ID {
		t.Errorf("recorded AgentID = %q, want %q", run.AgentID, target.ID)
	}
}

func TestShortIDIsUniqueish(t *testing.T) {
	// shortID is used to build child run IDs. It must be non-empty and vary
	// across calls (uuid-based, so collisions ~never).
	a, b := shortID(), shortID()
	if a == "" || b == "" {
		t.Fatal("shortID returned empty")
	}
	if a == b {
		t.Errorf("shortID collision: %q == %q", a, b)
	}
}

func TestRunChildAgentWaitingHumanIsDeterministicWithStubbedModel(t *testing.T) {
	var modelCalls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		modelCalls++
		if modelCalls > 1 {
			t.Fatalf("model called again with uncanceled context after child human.request interrupt")
		}
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("child-human-call-1", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Which rollout window should the child use?",
			}))),
		}, nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	target := agents.BuiltinResearchAssistant()
	req := childAgentRequest{
		ParentBase: runtimeEventBase{
			RunID:                 "parent-child-hitl",
			AgentID:               agents.BuiltinXiraAssistant().ID,
			Channel:               "feishu",
			ChatID:                "oc_child_hitl",
			ChatType:              "direct",
			SenderID:              "ou_child_hitl",
			ConversationSessionID: "conversation-child-hitl",
		},
		ParentRunID: "parent-child-hitl",
		ChildRunID:  "child-waiting-human",
		ToolCallID:  "spawn-call-1",
		Target:      target,
		Message:     "ask the human before continuing",
		SessionMode: "ephemeral_worker",
		Depth:       1,
	}

	resp, err := rt.RunChildAgent(context.Background(), req)
	if err != nil {
		t.Fatalf("RunChildAgent error: %v", err)
	}
	if resp.Status != StatusWaitingHuman {
		t.Fatalf("status = %q, want %q", resp.Status, StatusWaitingHuman)
	}
	if resp.Interrupt == nil || resp.Interrupt.Status != StatusWaitingHuman {
		t.Fatalf("interrupt = %+v", resp.Interrupt)
	}
	if len(resp.HumanRequests) != 1 {
		t.Fatalf("human_requests = %+v", resp.HumanRequests)
	}
	hr := resp.HumanRequests[0]
	if hr.Status != humanrequest.StatusPending || hr.Question != "Which rollout window should the child use?" {
		t.Fatalf("human request = %+v", hr)
	}
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("human.request must suspend, not persist as ordinary child tool transcript: %+v", resp.ToolCalls)
	}
	if modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", modelCalls)
	}
	storedRun, err := rt.RunStore().Load("child-waiting-human")
	if err != nil {
		t.Fatalf("load child run: %v", err)
	}
	if storedRun.Status != StatusWaitingHuman || len(storedRun.HumanRequests) != 1 {
		t.Fatalf("stored child run status=%q human_requests=%+v", storedRun.Status, storedRun.HumanRequests)
	}
	storedHR, err := rt.GetHumanRequest(context.Background(), hr.ID)
	if err != nil {
		t.Fatalf("stored human request: %v", err)
	}
	if storedHR.RunID != "child-waiting-human" || storedHR.Status != humanrequest.StatusPending {
		t.Fatalf("stored human request = %+v", storedHR)
	}
}

// TestRunChildAgentPersistsSessionScope verifies the断裂 A fix (issue #68):
// RunChildAgent MUST build and persist a SessionScope for the child run,
// mirroring the parent's trigger identity (channel/chat/sender). Without it,
// a spawned child that enters HITL can never route its resumed final back to
// IM — deliverResumeFinal hits the nil-scope branch and drops the final
// (human_request_resume.go). The scope is built from the parent's trigger
// identity (per-chat-key RFC §2.3: the child belongs to the parent's chat
// tree, users never talk to the child directly).
//
// The sender dimension is asserted in its REAL canonical form
// ("<channel>:<sender>", the product of canonicalSenderID), NOT a hand-cleaned
// value — per AGENTS.md §5.4, tests must use real transformation products.
func TestRunChildAgentPersistsSessionScope(t *testing.T) {
	stateRoot := t.TempDir()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return deepSeekHTTPResponse(deepSeekTextResponse("child did the work")), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       stateRoot,
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	target := agents.BuiltinResearchAssistant()
	// Give the target a session policy with chat+sender dimensions so BuildScope
	// materializes values (builtin profile has none).
	target.Session = agents.SessionPolicy{Dimensions: []string{"chat", "sender"}}

	const (
		parentChannel = "feishu"
		parentChat    = "oc_spawn_scope"
		parentSender  = "ou_sender_scope"
	)
	parentBase := runtimeEventBase{
		RunID:    "parent-run-scope",
		AgentID:  agents.BuiltinXiraAssistant().ID,
		Channel:  parentChannel,
		ChatID:   parentChat,
		ChatType: "direct",
		SenderID: parentSender,
	}
	req := childAgentRequest{
		ParentBase:  parentBase,
		ParentRunID: "parent-run-scope",
		ChildRunID:  "child-run-scope",
		Target:      target,
		Message:     "task",
		Depth:       1,
	}

	if _, err := rt.RunChildAgent(context.Background(), req); err != nil {
		t.Fatalf("RunChildAgent error: %v", err)
	}

	run, err := rt.RunStore().Load("child-run-scope")
	if err != nil {
		t.Fatalf("child run not recorded: %v", err)
	}
	if run.SessionScope == nil {
		t.Fatalf("child run SessionScope is nil — spawned child can never route resumed final to IM (断裂 A)")
	}

	// The child scope must mirror the PARENT's trigger identity (channel/chat/sender).
	if run.SessionScope.Channel != parentChannel {
		t.Errorf("scope Channel = %q, want %q (parent trigger channel)", run.SessionScope.Channel, parentChannel)
	}
	// chat is stored as "<chatType>:<chatID>" (BuildScope). Verify the ID round-trips.
	chatVal := run.SessionScope.Values["chat"]
	if want := "direct:" + parentChat; chatVal != want {
		t.Errorf("scope chat = %q, want %q", chatVal, want)
	}
	// #151：dimensions=[chat]，sender 不在 scope 里（per-sender 数据已独立到 stateDir）。
	// Round-trip: inboundContextFromScope 仍能恢复 Channel + ChatID。
	reconstructed := inboundContextFromScope(run.SessionScope, nil)
	if reconstructed.Channel != parentChannel {
		t.Errorf("reconstructed Channel = %q, want %q", reconstructed.Channel, parentChannel)
	}
	if reconstructed.ChatID != parentChat {
		t.Errorf("reconstructed ChatID = %q, want %q (prefix must strip)", reconstructed.ChatID, parentChat)
	}
}

// TestRunChildAgentPreservesNamesInChildContext (PR #132 review): a child run
// spawned by RunChildAgent must inherit ChatName / SenderName (and the
// already-important ChatID / SenderID) from parentBase. Before the fix,
// delegation.go's child InboundContext literal omitted name fields, and the
// resume path's runtimeEventBase omitted ALL context fields except Channel —
// so a resumed run that spawned a child lost names AND chat/sender IDs before
// the child prompt was built. This test pins the child-side inheritance;
// TestResumeDirectHumanRequestPropagatesContextToBase pins the resume-side
// construction.
func TestRunChildAgentPreservesNamesInChildContext(t *testing.T) {
	var capturedChildReq deepseek.ChatRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		capturedChildReq = req
		return deepSeekHTTPResponse(deepSeekTextResponse("child done")), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       t.TempDir(),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	caller := agents.BuiltinXiraAssistant()
	target := agents.BuiltinResearchAssistant()
	parentBase := runtimeEventBase{
		RunID:      "parent-named-1",
		AgentID:    caller.ID,
		Channel:    "feishu",
		ChatID:     "chat-9",
		ChatType:   "group",
		ChatName:   "工作群",
		SenderID:   "user-42",
		SenderName: "张三",
	}
	req := childAgentRequest{
		ParentBase:  parentBase,
		ParentRunID: "parent-named-1",
		ChildRunID:  "child-named-1",
		ToolCallID:  "call-1",
		Target:      target,
		Message:     "research task",
		SessionMode: "ephemeral_worker",
		Depth:       1,
	}
	if _, err := rt.RunChildAgent(context.Background(), req); err != nil {
		t.Fatalf("RunChildAgent error: %v", err)
	}
	if len(capturedChildReq.Messages) < 1 {
		t.Fatal("no messages captured from child LLM call")
	}
	systemInstruction, ok := capturedChildReq.Messages[0].Content.(string)
	if !ok {
		t.Fatalf("system message content type = %T", capturedChildReq.Messages[0].Content)
	}
	// Child must inherit the parent's identity — names AND ids.
	for _, want := range []string{
		"Channel: feishu",
		"Chat: chat-9 (type: group)",
		"ChatName: 工作群",
		"Sender: user-42",
		"SenderName: 张三",
	} {
		if !strings.Contains(systemInstruction, want) {
			t.Errorf("child system instruction missing %q\n--- instruction ---\n%s", want, systemInstruction)
		}
	}
}

// TestResumeDirectHumanRequestPropagatesContextToBase (PR #132 review): the
// resume path must build runtimeEventBase from the FULLY restored
// resumeReq.Context — not just Channel. Before the fix, resume's base omitted
// ChatID/SenderID/ChatName/SenderName, so a resumed run that spawned a child
// lost ALL context fields before the child prompt was built. This test seeds
// a waiting_human run whose SessionScope carries Names, resumes it, and
// verifies the resumed run's own system instruction contains the restored
// names AND chat/sender ids (proving resumeReq.Context was fully populated,
// which is the source the fixed base reads from).
func TestResumeDirectHumanRequestPropagatesContextToBase(t *testing.T) {
	var capturedResumeReq deepseek.ChatRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		capturedResumeReq = req
		return deepSeekHTTPResponse(deepSeekTextResponse("resumed")), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	// Seed a waiting_human run whose SessionScope carries chat_id
	// AND Names (sender_id + display names). #151: sender 在 Names，不在 Values。
	scope := &fsession.SessionScope{
		Version:      1,
		EntrypointID: "ep-resume",
		Channel:      "feishu",
		Values: map[string]string{
			"chat": "group:chat-9",
		},
		Names: map[string]string{
			"chat_name":   "工作群",
			"sender_name": "张三",
			"sender_id":   "feishu:user-42",
		},
	}
	if err := rt.runs.SaveRun(TurnResponse{
		RunID:        "run-resume-1",
		AgentID:      "xira-assistant",
		EntrypointID: "ep-resume",
		Status:       StatusWaitingHuman,
		Message:      "need input",
		SessionID:    "session-resume-1",
		SessionScope: scope,
		// Production owner-addressed runs persist this fact in run.Metadata.
		// The resume path must merge it back into InboundContext.Raw; passing
		// trigger.Raw directly to inboundContextFromScope would not exercise
		// the real resumeDirectHumanRequest data flow.
		Metadata: map[string]string{"addressed_to": "owner"},
	}); err != nil {
		t.Fatal(err)
	}
	hr, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		WorkspaceID:  rt.workspace,
		WorkspaceKey: rt.WorkspaceKey(),
		RunID:        "run-resume-1",
		AgentID:      "xira-assistant",
		SessionID:    "session-resume-1",
		Kind:         humanrequest.RequestFreeform,
		Question:     "continue?",
		Source:       "agent_request",
	})
	if err != nil {
		t.Fatal(err)
	}
	hr.Response = &humanrequest.HumanResponse{
		RequestID: hr.ID,
		Kind:      humanrequest.ResponseAnswer,
		Actor:     "user-42",
		Message:   "yes",
	}
	if err := rt.resumeDirectHumanRequest(context.Background(), hr); err != nil {
		t.Fatalf("resumeDirectHumanRequest: %v", err)
	}
	if len(capturedResumeReq.Messages) < 1 {
		t.Fatal("no messages captured from resume LLM call")
	}
	systemInstruction, ok := capturedResumeReq.Messages[0].Content.(string)
	if !ok {
		t.Fatalf("system message content type = %T", capturedResumeReq.Messages[0].Content)
	}
	// The resumed run's own system instruction must reflect the fully restored
	// context — names AND the de-prefixed ids (chat-9 / user-42, not group:chat-9
	// / feishu:user-42). This proves resumeReq.Context is complete, which is
	// the source the fixed runtimeEventBase reads from.
	for _, want := range []string{
		"Channel: feishu",
		"Chat: chat-9 (type: group)",
		"ChatName: 工作群",
		"Sender: user-42",
		"SenderName: 张三",
		"# Addressing Context",
		"Addressed to: owner",
		"owner's AI intern",
		"Never impersonate the owner",
	} {
		if !strings.Contains(systemInstruction, want) {
			t.Errorf("resumed run system instruction missing %q\n--- instruction ---\n%s", want, systemInstruction)
		}
	}
}
