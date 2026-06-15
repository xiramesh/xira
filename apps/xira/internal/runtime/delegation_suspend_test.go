package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

func TestDelegateChildWaitingHumanSuspendsParent(t *testing.T) {
	var sawChild bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		system := ""
		if len(req.Messages) > 0 {
			system = deepseek.ContentText(req.Messages[0].Content)
		}
		switch {
		case lastRole(req.Messages) == "tool":
			t.Fatalf("parent model was called after child waiting_human")
		case strings.Contains(system, "Current Xira agent: research-assistant"):
			sawChild = true
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("child-human-call", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Need child analyst input?",
			})), nil
		default:
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("delegate-child-wait", "delegate_agent", map[string]any{
				"agent_id":               agents.ResearchAssistantAgentID,
				"task":                   "Ask a human before finalizing.",
				"expected_output_schema": delegateResultSchemaV1,
			})), nil
		}
		return nil, nil
	})}
	rt := newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		StateRoot:      filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and wait", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if !sawChild {
		t.Fatal("child agent was not invoked")
	}
	if resp.Status != StatusWaitingHuman {
		t.Fatalf("parent status = %q", resp.Status)
	}
	if resp.Interrupt == nil {
		t.Fatal("parent interrupt is nil")
	}
	if len(resp.HumanRequests) != 1 || resp.HumanRequests[0].Status != humanrequest.StatusPending || resp.HumanRequests[0].Question != "Need child analyst input?" {
		t.Fatalf("parent human requests = %+v", resp.HumanRequests)
	}
	if len(resp.Interrupt.BlockedBy) != 1 || resp.Interrupt.BlockedBy[0].Type != "child_human_request" || resp.Interrupt.BlockedBy[0].HumanRequestID != resp.HumanRequests[0].ID {
		t.Fatalf("blocked_by = %+v", resp.Interrupt.BlockedBy)
	}
	if len(resp.Interrupt.SuspendedToolCalls) != 1 || resp.Interrupt.SuspendedToolCalls[0].Name != "delegate_agent" || resp.Interrupt.SuspendedToolCalls[0].ID != "delegate-child-wait" {
		t.Fatalf("suspended_tool_calls = %+v", resp.Interrupt.SuspendedToolCalls)
	}
	childRunID := resp.Interrupt.SuspendedToolCalls[0].RunID
	childRun, err := rt.RunStore().Load(childRunID)
	if err != nil {
		t.Fatalf("load child run %q: %v", childRunID, err)
	}
	if childRun.Status != StatusWaitingHuman || len(childRun.HumanRequests) != 1 {
		t.Fatalf("child run = %+v", childRun)
	}
}

func TestDelegateChildWaitingHumanPersistsJoinState(t *testing.T) {
	rt := newDelegationWaitingTestService(t)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and persist join", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || resp.Interrupt == nil {
		t.Fatalf("response = %+v", resp)
	}
	if len(resp.Interrupt.DelegationJoinIDs) != 1 {
		t.Fatalf("delegation_join_ids = %+v", resp.Interrupt.DelegationJoinIDs)
	}
	join, err := rt.loadDelegationJoinState(resp.RunID, resp.Interrupt.DelegationJoinIDs[0])
	if err != nil {
		t.Fatalf("load join state: %v", err)
	}
	if join.ParentRunID != resp.RunID || join.ParentAgentID != resp.AgentID || join.JoinPolicy != "all" || join.Status != StatusWaitingHuman {
		t.Fatalf("join identity = %+v", join)
	}
	if len(join.Calls) != 1 {
		t.Fatalf("join calls = %+v", join.Calls)
	}
	call := join.Calls[0]
	if call.ParentToolCallID != "delegate-child-wait" || call.ChildRunID == "" || call.ChildAgentID != agents.ResearchAssistantAgentID || call.Status != StatusWaitingHuman {
		t.Fatalf("join call = %+v", call)
	}
	if call.ChildHumanRequestID != resp.HumanRequests[0].ID {
		t.Fatalf("join child request = %q, want %q", call.ChildHumanRequestID, resp.HumanRequests[0].ID)
	}
	if join.SuspendedToolCall.ID != "delegate-child-wait" || join.SuspendedToolCall.Name != "delegate_agent" || join.SuspendedToolCall.RunID != call.ChildRunID {
		t.Fatalf("suspended tool call = %+v", join.SuspendedToolCall)
	}
}

func TestDelegateChildWaitingHumanReleasesActiveSlot(t *testing.T) {
	rt := newDelegationWaitingTestService(t)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and count slots", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman {
		t.Fatalf("status = %q", resp.Status)
	}
	if active := rt.activeChildCount(resp.RunID); active != 0 {
		t.Fatalf("active child count = %d, want 0 after child suspended", active)
	}
	outstanding, err := rt.outstandingChildCount(resp.RunID)
	if err != nil {
		t.Fatalf("outstanding child count: %v", err)
	}
	if outstanding != 1 {
		t.Fatalf("outstanding child count = %d, want 1 suspended child", outstanding)
	}
}

func TestDelegateChildWaitingHumanCountsAgainstMaxOutstanding(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs"), StateRoot: filepath.Join(t.TempDir(), "state")})
	parentRunID := "parent-outstanding"
	if err := rt.RunStore().InitRun(parentRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.createWaitingDelegationJoinState(parentRunID, agents.DefaultAgentID, "delegate-existing", "child-existing", agents.ResearchAssistantAgentID, []humanrequest.HumanRequest{{
		ID:    "hr-existing",
		RunID: "child-existing",
	}}); err != nil {
		t.Fatal(err)
	}
	caller := agents.BuiltinXiraAssistant()
	caller.Delegation.MaxParallel = 2
	caller.Delegation.MaxOutstanding = 1
	base := runtimeEventBase{
		RunID:        parentRunID,
		AgentID:      agents.DefaultAgentID,
		EntrypointID: "test-default",
		Channel:      "test",
		TraceID:      parentRunID,
	}
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{Base: base, Profile: caller, UserMessage: "parent"})
	output, err := rt.executeDelegateAgentTool(ctx, caller, delegateAgentInput{
		AgentID:              agents.ResearchAssistantAgentID,
		Task:                 "second child should not start",
		ExpectedOutputSchema: delegateResultSchemaV1,
	}, nil, nil, "delegate-second", func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {})
	if err == nil || !strings.Contains(err.Error(), "max_outstanding") {
		t.Fatalf("max_outstanding error = %v output=%+v", err, output)
	}
	assertRejectedChildRunNotCreated(t, rt, output)
}

func TestDelegateResumeAfterChildAnswerMaterializesOutput(t *testing.T) {
	rt := newDelegationResumeTestService(t)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and resume", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || len(resp.HumanRequests) != 1 || resp.Interrupt == nil || len(resp.Interrupt.DelegationJoinIDs) != 1 {
		t.Fatalf("waiting response = %+v", resp)
	}
	requestID := resp.HumanRequests[0].ID

	if _, err := rt.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseAnswer,
		Actor:   "tester",
		Message: "Use the canary window.",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest answer: %v", err)
	}

	join, err := rt.loadDelegationJoinState(resp.RunID, resp.Interrupt.DelegationJoinIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if join.Status != "completed" || len(join.Calls) != 1 || join.Calls[0].Status != "completed" || join.Calls[0].OutputRef == "" {
		t.Fatalf("join after resume = %+v", join)
	}
	childRun, err := rt.RunStore().Load(join.Calls[0].ChildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if childRun.Status != "completed" || !strings.Contains(childRun.FinalResponse, "canary window") {
		t.Fatalf("child run after resume = %+v", childRun)
	}
	parentRun, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var materialized *ToolCallRecord
	for i := range parentRun.ToolCalls {
		if parentRun.ToolCalls[i].ID == "delegate-child-wait" {
			materialized = &parentRun.ToolCalls[i]
			break
		}
	}
	if materialized == nil || materialized.Output["status"] != "completed" || !strings.Contains(anyString(materialized.Output["summary"]), "canary window") {
		t.Fatalf("materialized parent tool call = %+v", parentRun.ToolCalls)
	}
}

func TestDelegateResumeAfterChildApprovedMaterializesOutput(t *testing.T) {
	rt := newDelegationResumeTestService(t)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and approve", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if _, err := rt.ResolveHumanRequest(context.Background(), resp.HumanRequests[0].ID, humanrequest.ResolveRequest{
		Kind:  humanrequest.ResponseApprove,
		Actor: "tester",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest approve: %v", err)
	}
	parentRun, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	status := delegateToolOutputStatus(parentRun, "delegate-child-wait")
	if status != "completed" {
		t.Fatalf("parent delegate output status = %q, tool calls=%+v", status, parentRun.ToolCalls)
	}
}

func TestDelegateResumeAfterChildAnswerInjectsOnlyChildOutput(t *testing.T) {
	rt := newDelegationResumeTestService(t)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and answer boundary", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if _, err := rt.ResolveHumanRequest(context.Background(), resp.HumanRequests[0].ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseAnswer,
		Actor:   "tester",
		Message: "Use the canary window.",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest answer: %v", err)
	}
	parentRun, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	for _, rec := range parentRun.ToolCalls {
		if rec.ID == "delegate-child-wait" {
			output = rec.Output
			break
		}
	}
	if output["status"] != "completed" {
		t.Fatalf("parent delegate output = %+v", output)
	}
	for _, forbidden := range []string{"human_request_id", "human_response", "human_requests", "interrupt"} {
		if _, ok := output[forbidden]; ok {
			t.Fatalf("parent delegate output leaked %s: %+v", forbidden, output)
		}
	}
}

func TestDelegateResumeIsIdempotent(t *testing.T) {
	rt := newDelegationResumeTestService(t)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and replay resume event", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	requestID := resp.HumanRequests[0].ID
	if _, err := rt.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseAnswer,
		Actor:   "tester",
		Message: "Use the canary window.",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest answer: %v", err)
	}
	if err := rt.ResumeRunAfterHumanResponse(context.Background(), requestID); err != nil {
		t.Fatalf("duplicate ResumeRunAfterHumanResponse: %v", err)
	}
	parentRun, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, rec := range parentRun.ToolCalls {
		if rec.ID == "delegate-child-wait" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("delegate tool output materialized %d times: %+v", count, parentRun.ToolCalls)
	}
}

func TestDelegateResumeDenyMaterializesFailedOutput(t *testing.T) {
	rt := newDelegationResumeTestService(t)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and deny", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	requestID := resp.HumanRequests[0].ID
	if _, err := rt.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseDeny,
		Actor:   "tester",
		Message: "not approved",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest deny: %v", err)
	}
	join, err := rt.loadDelegationJoinState(resp.RunID, resp.Interrupt.DelegationJoinIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if join.Status != "completed" || join.Calls[0].Status != "failed" || join.Calls[0].OutputRef == "" {
		t.Fatalf("join after deny = %+v", join)
	}
	childRun, err := rt.RunStore().Load(join.Calls[0].ChildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if childRun.Status != "failed" || childRun.Metadata["error_type"] == "canceled" {
		t.Fatalf("child run after deny = %+v", childRun)
	}
	parentRun, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got := delegateToolOutputStatus(parentRun, "delegate-child-wait"); got != "failed" {
		t.Fatalf("delegate output status = %q tool_calls=%+v", got, parentRun.ToolCalls)
	}
}

func TestDelegateResumeCancelMaterializesCanceledOutput(t *testing.T) {
	rt := newDelegationResumeTestService(t)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and cancel", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	requestID := resp.HumanRequests[0].ID
	if _, err := rt.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseCancel,
		Actor:   "tester",
		Message: "stop child",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest cancel: %v", err)
	}
	join, err := rt.loadDelegationJoinState(resp.RunID, resp.Interrupt.DelegationJoinIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if join.Status != "completed" || join.Calls[0].Status != "canceled" || join.Calls[0].OutputRef == "" {
		t.Fatalf("join after cancel = %+v", join)
	}
	childRun, err := rt.RunStore().Load(join.Calls[0].ChildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if childRun.Status != "failed" || childRun.Metadata["error_type"] != "canceled" {
		t.Fatalf("child run after cancel = %+v", childRun)
	}
	parentRun, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got := delegateToolOutputStatus(parentRun, "delegate-child-wait"); got != "canceled" {
		t.Fatalf("delegate output status = %q tool_calls=%+v", got, parentRun.ToolCalls)
	}
}

func TestDelegateResumeAfterProcessRestart(t *testing.T) {
	runRoot := filepath.Join(t.TempDir(), "runs")
	stateRoot := filepath.Join(t.TempDir(), "state")
	rt := newDelegationResumeTestServiceWithRoots(t, runRoot, stateRoot)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and restart", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || len(resp.HumanRequests) != 1 {
		t.Fatalf("waiting response = %+v", resp)
	}

	restarted := newDelegationResumeTestServiceWithRoots(t, runRoot, stateRoot)
	outstanding, err := restarted.outstandingChildCount(resp.RunID)
	if err != nil {
		t.Fatalf("outstanding after restart: %v", err)
	}
	if outstanding != 1 {
		t.Fatalf("outstanding after restart = %d, want 1", outstanding)
	}
	if _, err := restarted.ResolveHumanRequest(context.Background(), resp.HumanRequests[0].ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseAnswer,
		Actor:   "tester",
		Message: "Use the canary window.",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest after restart: %v", err)
	}
	join, err := restarted.loadDelegationJoinState(resp.RunID, resp.Interrupt.DelegationJoinIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if join.Status != "completed" || join.Calls[0].Status != "completed" {
		t.Fatalf("join after restart resume = %+v", join)
	}
}

func TestFindDelegationJoinByHumanRequestSkipsCorruptJoinFiles(t *testing.T) {
	runRoot := filepath.Join(t.TempDir(), "runs")
	rt := newDelegationResumeTestServiceWithRoots(t, runRoot, filepath.Join(t.TempDir(), "state"))
	badDir := filepath.Join(runRoot, "aaa_bad_run", "delegations")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "bad.yaml"), []byte(":\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	join := &DelegationJoinState{
		SchemaVersion: delegationJoinSchemaVersion,
		ID:            "djoin_valid",
		Workspace:     rt.workspace,
		ParentRunID:   "zzz_parent_run",
		ParentAgentID: agents.DefaultAgentID,
		JoinPolicy:    "all",
		Status:        StatusWaitingHuman,
		Calls: []DelegationJoinCall{{
			ParentToolCallID:    "delegate-call",
			ChildRunID:          "child-run",
			ChildAgentID:        agents.ResearchAssistantAgentID,
			Status:              StatusWaitingHuman,
			ChildHumanRequestID: "hrq_target",
		}},
	}
	if err := rt.saveDelegationJoinState(join); err != nil {
		t.Fatal(err)
	}

	found, callIndex, err := rt.findDelegationJoinByHumanRequest("hrq_target")
	if err != nil {
		t.Fatalf("findDelegationJoinByHumanRequest returned corrupt-file error: %v", err)
	}
	if found == nil || found.ID != join.ID || callIndex != 0 {
		t.Fatalf("found join=%+v callIndex=%d", found, callIndex)
	}
}

func delegateToolOutputStatus(run TurnResponse, toolCallID string) string {
	for _, rec := range run.ToolCalls {
		if rec.ID == toolCallID {
			return anyString(rec.Output["status"])
		}
	}
	return ""
}

func newDelegationWaitingTestService(t *testing.T) *Service {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		system := ""
		if len(req.Messages) > 0 {
			system = deepseek.ContentText(req.Messages[0].Content)
		}
		switch {
		case lastRole(req.Messages) == "tool":
			t.Fatalf("parent model was called after child waiting_human")
		case strings.Contains(system, "Current Xira agent: research-assistant"):
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("child-human-call", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Need child analyst input?",
			})), nil
		default:
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("delegate-child-wait", "delegate_agent", map[string]any{
				"agent_id":               agents.ResearchAssistantAgentID,
				"task":                   "Ask a human before finalizing.",
				"expected_output_schema": delegateResultSchemaV1,
			})), nil
		}
		return nil, nil
	})}
	return newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		StateRoot:      filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
}

func newDelegationResumeTestService(t *testing.T) *Service {
	t.Helper()
	return newDelegationResumeTestServiceWithRoots(t, filepath.Join(t.TempDir(), "runs"), filepath.Join(t.TempDir(), "state"))
}

func newDelegationResumeTestServiceWithRoots(t *testing.T, runRoot, stateRoot string) *Service {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		system := ""
		if len(req.Messages) > 0 {
			system = deepseek.ContentText(req.Messages[0].Content)
		}
		switch {
		case strings.Contains(lastUserMessage(req.Messages), "delegate_agent output"):
			return deepSeekHTTPResponse(deepSeekTextResponse("parent finalized from delegate output")), nil
		case strings.Contains(system, "Current Xira agent: research-assistant") && strings.Contains(lastUserMessage(req.Messages), "Human approved the request."):
			return deepSeekHTTPResponse(deepSeekTextResponse(`{"summary":"Human approval accepted.","evidence_refs":[],"limitations":[],"confidence":"high","followup_needed":false}`)), nil
		case strings.Contains(system, "Current Xira agent: research-assistant") && strings.Contains(lastUserMessage(req.Messages), "Use the canary window."):
			return deepSeekHTTPResponse(deepSeekTextResponse(`{"summary":"Use the canary window.","evidence_refs":[],"limitations":[],"confidence":"high","followup_needed":false}`)), nil
		case lastRole(req.Messages) == "tool":
			t.Fatalf("parent model was called before child answer materialized")
		case strings.Contains(system, "Current Xira agent: research-assistant"):
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("child-human-call", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Need child analyst input?",
			})), nil
		default:
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("delegate-child-wait", "delegate_agent", map[string]any{
				"agent_id":               agents.ResearchAssistantAgentID,
				"task":                   "Ask a human before finalizing.",
				"expected_output_schema": delegateResultSchemaV1,
			})), nil
		}
		return nil, nil
	})}
	return newTestService(t, Config{
		RunRoot:        runRoot,
		StateRoot:      stateRoot,
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
}

func deepSeekHTTPResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
