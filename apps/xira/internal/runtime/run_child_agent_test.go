package runtime

import (
	"context"
	"net/http"
	"testing"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/model/deepseek"
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

// NOTE: RunChildAgent's waiting_human branch (child calls human.request →
// interrupt collector → status flips) is the HITL-sensitive path the Phase 6a
// resume-rewire depends on. It is NOT covered by a unit test here because
// reproducing the ADK-loop interrupt timing with a stubbed LLM is flaky (the
// stub keeps re-issuing the tool call, looping past any ctx deadline). This
// path is covered by the live HITL tests (deepseek_hitl_live_test.go,
// XIRA_DEEPSEEK_LIVE=1) which exercise the real LLM + real interrupt timing.
// Tracking a deterministic unit test for it as a follow-up.

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
	// sender is stored in canonical form "<channel>:<sender>" (canonicalSenderID).
	// This is the REAL transformation product — not a hand-cleaned value (§5.4).
	senderVal := run.SessionScope.Values["sender"]
	if want := parentChannel + ":" + parentSender; senderVal != want {
		t.Errorf("scope sender = %q, want canonical %q", senderVal, want)
	}

	// Round-trip: inboundContextFromScope must recover the raw IDs (strip prefixes).
	reconstructed := inboundContextFromScope(run.SessionScope, nil)
	if reconstructed.Channel != parentChannel {
		t.Errorf("reconstructed Channel = %q, want %q", reconstructed.Channel, parentChannel)
	}
	if reconstructed.ChatID != parentChat {
		t.Errorf("reconstructed ChatID = %q, want %q (prefix must strip)", reconstructed.ChatID, parentChat)
	}
	if reconstructed.SenderID != parentSender {
		t.Errorf("reconstructed SenderID = %q, want %q (canonical prefix must strip)", reconstructed.SenderID, parentSender)
	}
}
