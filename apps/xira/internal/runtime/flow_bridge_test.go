package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/entrypoints"
	"github.com/xiramesh/xira/internal/flow"
	"github.com/xiramesh/xira/internal/humanrequest"
	fsession "github.com/xiramesh/xira/internal/session"
)

func TestFlowHumanApprovalUsesRuntimeHumanRequestStore(t *testing.T) {
	rt := newTestService(t, Config{
		StateDir: filepath.Join(t.TempDir(), "state"),
	})
	def := &flow.Definition{
		SchemaVersion: flow.SchemaVersionDefinition,
		ID:            "approval-flow",
		Name:          "Approval Flow",
		Version:       "0.1.0",
		Objective:     "exercise runtime-backed flow approval",
		Entrypoints:   []flow.Entrypoint{{ID: "ad_hoc", StartStep: "approve"}},
		Steps: []flow.Step{
			{
				ID:             "approve",
				Objective:      "approve before work",
				Executor:       flow.Executor{Type: "human_approval", Prompt: "Approve runtime-backed flow?", Options: []string{"approve", "cancel"}},
				OutputContract: flow.OutputContract{RequiredSlots: []flow.OutputSlot{{ID: "approval_signal"}}},
				Transitions:    flow.Transitions{Branches: []flow.Branch{{When: "${outputs.approve.approval_signal == 'approve'}", Next: "report"}, {When: "${outputs.approve.approval_signal == 'cancel'}", Next: "report"}}},
			},
			{ID: "report", Objective: "report", Executor: flow.Executor{Agent: "xira-assistant"}, OutputContract: flow.OutputContract{RequiredSlots: []flow.OutputSlot{{ID: "final_report"}}}},
		},
	}
	k := rt.FlowKernel()
	k.Definitions = flowStaticDefinitions{defs: map[string]*flow.Definition{"approval-flow": def}}

	trigger := channel.InboundContext{Channel: "feishu", EntrypointID: "feishu-default", ChatID: "oc_flow", SenderID: "u_flow", SenderIDType: "open_id", ChatType: "group"}
	run, err := rt.StartFlow(context.Background(), flow.StartRequest{
		FlowID:       "approval-flow",
		EntrypointID: "ad_hoc",
		Input:        map[string]string{"request": "approve"},
		Context:      trigger,
	})
	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	run, err = rt.AdvanceFlow(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("AdvanceFlow: %v", err)
	}
	if run.Status != flow.RunWaitingHuman {
		t.Fatalf("status = %q, want waiting_human; step=%+v", run.Status, run.Steps["approve"])
	}
	if len(run.PendingHumanRequests) != 1 {
		t.Fatalf("pending_human_requests = %+v, want one", run.PendingHumanRequests)
	}
	req, err := rt.GetHumanRequest(context.Background(), run.PendingHumanRequests[0])
	if err != nil {
		t.Fatalf("GetHumanRequest: %v", err)
	}
	if req.Source != flow.SourceFlowHumanApproval || req.Kind != humanrequest.RequestApproval {
		t.Fatalf("human request source/kind = %q/%q", req.Source, req.Kind)
	}
	if req.AgentID == "" || req.SessionID == "" {
		t.Fatalf("flow HumanRequest must satisfy runtime store scope fields: %+v", req)
	}
	if req.Metadata[flow.MetadataFlowRunID] != run.ID || req.Metadata[flow.MetadataFlowStepID] != "approve" {
		t.Fatalf("flow metadata = %+v", req.Metadata)
	}
	wantChatKey := ChatKeyFromInbound(trigger).String()
	if req.ChatKey != wantChatKey {
		t.Fatalf("flow HumanRequest chat_key = %q, want %q", req.ChatKey, wantChatKey)
	}
	if req.Responder.Type != humanrequest.ResponderCurrentSender || req.Responder.EntrypointID != "feishu-default" || req.Responder.SenderID != "u_flow" || req.Responder.SenderIDType != "open_id" {
		t.Fatalf("flow HumanRequest responder = %+v", req.Responder)
	}
	if _, err := rt.ResolveHumanRequest(context.Background(), req.ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseAnswer,
		Actor:   "tester",
		Message: "approve",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest: %v", err)
	}
	run, err = rt.GetFlowRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("GetFlowRun: %v", err)
	}
	if run.Status != flow.RunRunning || run.CurrentStepID != "report" {
		t.Fatalf("after resume status=%q current=%q, want running/report", run.Status, run.CurrentStepID)
	}
	resolvedRequest, err := rt.GetHumanRequest(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedRequest.Resume.Status != humanrequest.ResumeCompleted || resolvedRequest.Resume.Attempts != 1 {
		t.Fatalf("flow HumanRequest resume = %+v", resolvedRequest.Resume)
	}
}

func TestFlowOwnerTextResponseKeepsPersistedOriginContext(t *testing.T) {
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})
	const entrypointID = "ilink-owner"
	rt.entrypoints = entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{{ID: entrypointID, Channel: "ilink", Account: "acct-1"}})
	rt.ownerBindings.Set(ownerBinding{EntrypointID: entrypointID, OwnerSenderID: "owner-1", OwnerSenderIDType: "ilink_user_id"})
	outbound := &fakeHumanRequestOutbound{receiptID: "flow-owner-request"}
	rt.SetOutboundEmitter(outbound)
	definition := &flow.Definition{
		SchemaVersion: flow.SchemaVersionDefinition, ID: "owner-approval-flow", Name: "Owner approval", Version: "0.1.0",
		Entrypoints: []flow.Entrypoint{{ID: "ad_hoc", StartStep: "approve"}},
		Steps: []flow.Step{{
			ID: "approve", Objective: "owner approves",
			Executor:       flow.Executor{Type: "human_approval", Responder: "owner", Question: "Owner approve?", Options: []string{"approve", "cancel"}},
			OutputContract: flow.OutputContract{RequiredSlots: []flow.OutputSlot{{ID: "approval_signal"}}},
		}},
	}
	rt.FlowKernel().Definitions = flowStaticDefinitions{defs: map[string]*flow.Definition{"owner-approval-flow": definition}}
	origin := channel.InboundContext{Channel: "ilink", EntrypointID: entrypointID, Account: "acct-1", ChatID: "origin-group", ChatType: "group", SenderID: "coworker-1", SenderIDType: "ilink_user_id"}
	run, err := rt.StartFlow(context.Background(), flow.StartRequest{FlowID: definition.ID, EntrypointID: "ad_hoc", Context: origin})
	if err != nil {
		t.Fatal(err)
	}
	run, err = rt.AdvanceFlow(context.Background(), run.ID)
	if err != nil || run.Status != flow.RunWaitingHuman || len(run.PendingHumanRequests) != 1 {
		t.Fatalf("owner flow wait = %+v, %v", run, err)
	}
	request, err := rt.GetHumanRequest(context.Background(), run.PendingHumanRequests[0])
	if err != nil || request.Responder.Type != humanrequest.ResponderOwner || request.Delivery.Status != humanrequest.DeliverySent {
		t.Fatalf("owner flow request = %+v, %v", request, err)
	}
	_, err = rt.ResolveHumanTextResponse(context.Background(), humanrequest.TextResponseEnvelope{
		CorrelationToken: request.CorrelationToken, EntrypointID: entrypointID,
		SenderID: "owner-1", SenderIDType: "ilink_user_id",
		ChatKey:  ChatKey{Channel: "ilink", ChatID: "owner-1", SenderID: "owner-1"}.String(),
		ChatType: "direct", Answer: "approve", IdempotencyKey: "flow-owner-answer-1",
	})
	if err != nil {
		t.Fatalf("owner flow response: %v", err)
	}
	resumed, err := rt.GetFlowRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != flow.RunCompleted || resumed.Context == nil || resumed.Context.ChatID != "origin-group" || resumed.Context.SenderID != "coworker-1" {
		t.Fatalf("owner DM replaced flow origin context: %+v", resumed)
	}
}

func TestFlowRuntimePolicyInputRoutesToApproval(t *testing.T) {
	rt := newTestService(t, Config{
		StateDir: filepath.Join(t.TempDir(), "state"),
	})
	def := &flow.Definition{
		SchemaVersion: flow.SchemaVersionDefinition,
		ID:            "policy-flow",
		Name:          "Policy Flow",
		Version:       "0.1.0",
		Objective:     "exercise runtime-backed flow policy",
		Entrypoints:   []flow.Entrypoint{{ID: "ad_hoc", StartStep: "design"}},
		Steps: []flow.Step{
			{
				ID:             "design",
				Objective:      "design",
				Executor:       flow.Executor{Agent: "xira-assistant"},
				OutputContract: flow.OutputContract{RequiredSlots: []flow.OutputSlot{{ID: "design_doc"}}},
				Transitions: flow.Transitions{Branches: []flow.Branch{
					{When: "${runtime.policy.require_design_approval == true}", Next: "approve_design"},
					{When: "${runtime.policy.require_design_approval != true}", Next: "implement"},
				}},
			},
			{ID: "approve_design", Objective: "approve", Executor: flow.Executor{Type: "human_approval"}, OutputContract: flow.OutputContract{RequiredSlots: []flow.OutputSlot{{ID: "approval_signal"}}}},
			{ID: "implement", Objective: "implement", Executor: flow.Executor{Agent: "xira-assistant"}, OutputContract: flow.OutputContract{RequiredSlots: []flow.OutputSlot{{ID: "change"}}}},
		},
	}
	k := rt.FlowKernel()
	k.Definitions = flowStaticDefinitions{defs: map[string]*flow.Definition{"policy-flow": def}}
	run, err := rt.StartFlow(context.Background(), flow.StartRequest{
		FlowID:       "policy-flow",
		EntrypointID: "ad_hoc",
		Input:        map[string]string{"request": "x", "require_design_approval": "true"},
	})
	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	run, err = rt.AdvanceFlow(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("AdvanceFlow: %v", err)
	}
	if run.CurrentStepID != "approve_design" {
		t.Fatalf("current_step_id = %q, want approve_design", run.CurrentStepID)
	}
}

type flowStaticDefinitions struct {
	defs map[string]*flow.Definition
}

func (s flowStaticDefinitions) Definition(id string) (*flow.Definition, error) {
	if def, ok := s.defs[id]; ok {
		return def, nil
	}
	return nil, fmt.Errorf("flow %q not found", id)
}

// TestFlowAgentStepPersistsSessionInTriggerChannel is the end-to-end (non-live)
// proof that a flow-invoked agent turn lands under the trigger channel's
// session tree, not a forged "flow" channel. Uses the fake DeepSeek client —
// session placement is determined by InboundContext, independent of the LLM.
func TestFlowAgentStepPersistsSessionInTriggerChannel(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	rt := newTestService(t, Config{
		StateDir: stateRoot,
	})
	def := &flow.Definition{
		SchemaVersion: flow.SchemaVersionDefinition,
		ID:            "session-channel-flow",
		Name:          "Session Channel Flow",
		Version:       "0.1.0",
		Objective:     "verify flow agent step session channel",
		Entrypoints:   []flow.Entrypoint{{ID: "ad_hoc", StartStep: "work"}},
		Steps: []flow.Step{
			{
				ID:             "work",
				Objective:      "produce the smoke summary",
				Executor:       flow.Executor{Agent: "xira-assistant"},
				OutputContract: flow.OutputContract{RequiredSlots: []flow.OutputSlot{{ID: "summary"}}},
			},
		},
	}
	k := rt.FlowKernel()
	k.Definitions = flowStaticDefinitions{defs: map[string]*flow.Definition{"session-channel-flow": def}}

	// Start the flow carrying a real feishu trigger identity.
	started, err := rt.StartFlow(context.Background(), flow.StartRequest{
		FlowID:       "session-channel-flow",
		EntrypointID: "ad_hoc",
		Input:        map[string]string{"request": "smoke"},
		Context: channel.InboundContext{
			Channel:  "feishu",
			ChatID:   "oc_flow_smoke",
			SenderID: "u_flow_smoke",
			ChatType: "group",
		},
	})
	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	advanced, err := rt.AdvanceFlow(context.Background(), started.ID)
	if err != nil {
		t.Fatalf("AdvanceFlow: %v", err)
	}
	step := advanced.Steps["work"]
	if step.Status != flow.StepCompleted || strings.TrimSpace(step.AgentRunID) == "" {
		t.Fatalf("work step not completed: status=%q agent_run_id=%q error=%q", step.Status, step.AgentRunID, step.Error)
	}

	// The agent run's session must reflect the feishu trigger channel.
	agentRun, err := rt.RunStore().Load(step.AgentRunID)
	if err != nil {
		t.Fatalf("load agent run: %v", err)
	}
	if agentRun.SessionScope == nil {
		t.Fatal("agent run session scope is nil")
	}
	if agentRun.SessionScope.Channel != "feishu" {
		t.Fatalf("agent run session channel = %q, want feishu (flow must not forge its own channel)", agentRun.SessionScope.Channel)
	}
	if got := agentRun.SessionScope.Values["chat"]; !strings.Contains(got, "oc_flow_smoke") {
		t.Fatalf("agent run session chat = %q, want it to contain the real chat oc_flow_smoke", got)
	}
	// #151：dimensions=[chat]，sender 不在 scope 里。

	// And the messages.jsonl must physically live under sessions/feishu/...
	// (not sessions/flow/...).
	scope := agentRun.SessionScope
	msgPath := rt.SessionManager().AgentMessagesPath(fsession.AgentTurnInput{
		SessionID: agentRun.SessionID,
		AgentID:   agentRun.AgentID,
		Context:   channel.InboundContext{Channel: scope.Channel, EntrypointID: agentRun.EntrypointID, ChatID: scopeChatID(scope.Values["chat"]), SenderID: scope.Values["sender"]},
		Scope:     scope,
	})
	rel := strings.TrimPrefix(filepath.ToSlash(msgPath), filepath.ToSlash(stateRoot)+"/")
	if !strings.HasPrefix(rel, "sessions/feishu/") {
		t.Fatalf("messages path = %q, want it under sessions/feishu/ (was sessions/flow/ before the fix)", rel)
	}
}

func scopeChatID(scopeChat string) string {
	if idx := strings.Index(scopeChat, ":"); idx >= 0 {
		return scopeChat[idx+1:]
	}
	return scopeChat
}

func TestCloneFlowToolInputAllowlistDeepCopies(t *testing.T) {
	if got := cloneFlowToolInputAllowlist(nil); got != nil {
		t.Fatalf("nil allowlist = %+v, want nil", got)
	}
	in := map[string]map[string][]string{
		"command.run": {"program": {"git", "go"}},
	}
	out := cloneFlowToolInputAllowlist(in)
	in["command.run"]["program"][0] = "mutated"
	if out["command.run"]["program"][0] != "git" {
		t.Fatalf("allowlist was not deep copied: %+v", out)
	}
}

func TestMapTurnResponseToFlowCopiesProjection(t *testing.T) {
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ended := started.Add(time.Second)
	resp := TurnResponse{
		RunID:         "run-1",
		AgentID:       "agent-1",
		EntrypointID:  "entry-1",
		SessionID:     "session-1",
		Status:        "waiting_human",
		FinalResponse: "draft",
		StartedAt:     started,
		EndedAt:       ended,
		Artifacts:     []string{"artifact-a"},
		HumanRequests: []humanrequest.HumanRequest{{
			ID:     "hrq-1",
			Source: "tool",
			Kind:   humanrequest.RequestApproval,
			Status: humanrequest.StatusPending,
		}},
		Interrupt: &RunInterrupt{
			Status: "waiting_human",
			Reason: "approval",
			BlockedBy: []BlockedBy{{
				Type:           "human_request",
				HumanRequestID: "hrq-1",
				RunID:          "run-1",
				Reason:         "needs approval",
			}},
		},
	}
	got := mapTurnResponseToFlow(resp)
	resp.Artifacts[0] = "mutated"

	if got.RunID != "run-1" || got.StartedAt != started || got.EndedAt != ended {
		t.Fatalf("flow response identity/times wrong: %+v", got)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0] != "artifact-a" {
		t.Fatalf("artifacts projection wrong: %+v", got.Artifacts)
	}
	if len(got.HumanRequests) != 1 || got.HumanRequests[0].Kind != "approval" {
		t.Fatalf("human request projection wrong: %+v", got.HumanRequests)
	}
	if got.Interrupt == nil || len(got.Interrupt.BlockedBy) != 1 || got.Interrupt.BlockedBy[0].HumanRequestID != "hrq-1" {
		t.Fatalf("interrupt projection wrong: %+v", got.Interrupt)
	}
}

// TestFlowBridgeMergesMetadataIntoContextRaw asserts that flow-internal
// traceability keys (flow_run_id/flow_id/flow_step_id) from
// AgentTurnRequest.Metadata survive into the TurnRequest.Context.Raw, so they
// reach the session and run records. Without this merge, flow step provenance
// is silently dropped at the bridge.
func TestFlowBridgeMergesMetadataIntoContextRaw(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	def := &flow.Definition{
		SchemaVersion: flow.SchemaVersionDefinition,
		ID:            "trace-flow",
		Name:          "Trace Flow",
		Version:       "0.1.0",
		Objective:     "verify metadata merge",
		Entrypoints:   []flow.Entrypoint{{ID: "ad_hoc", StartStep: "work"}},
		Steps: []flow.Step{
			{
				ID:             "work",
				Objective:      "produce output",
				Executor:       flow.Executor{Agent: "xira-assistant"},
				OutputContract: flow.OutputContract{RequiredSlots: []flow.OutputSlot{{ID: "out"}}},
			},
		},
	}
	k := rt.FlowKernel()
	k.Definitions = flowStaticDefinitions{defs: map[string]*flow.Definition{"trace-flow": def}}

	run, err := rt.StartFlow(context.Background(), flow.StartRequest{
		FlowID:       "trace-flow",
		EntrypointID: "ad_hoc",
		Input:        map[string]string{"request": "x"},
		Context:      channel.InboundContext{Channel: "test", SenderID: "u_trace"},
	})
	if err != nil {
		t.Fatalf("StartFlow: %v", err)
	}
	advanced, err := rt.AdvanceFlow(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("AdvanceFlow: %v", err)
	}
	step := advanced.Steps["work"]
	if step.Status != flow.StepCompleted || strings.TrimSpace(step.AgentRunID) == "" {
		t.Fatalf("work step not completed: %+v", step)
	}

	// The agent run's metadata must carry the flow traceability keys — proving
	// they survived the bridge merge into Context.Raw.
	agentRun, err := rt.RunStore().Load(step.AgentRunID)
	if err != nil {
		t.Fatalf("load agent run: %v", err)
	}
	if agentRun.Metadata == nil {
		t.Fatal("agent run metadata is nil; flow traceability keys dropped at bridge")
	}
	for _, key := range []string{"flow_run_id", "flow_id", "flow_step_id"} {
		if v := strings.TrimSpace(agentRun.Metadata[key]); v == "" {
			t.Fatalf("agent run metadata missing %q (dropped at bridge); metadata=%+v", key, agentRun.Metadata)
		}
	}
	if agentRun.Metadata["flow_run_id"] != run.ID {
		t.Fatalf("flow_run_id = %q, want %q", agentRun.Metadata["flow_run_id"], run.ID)
	}
	if agentRun.Metadata["flow_step_id"] != "work" {
		t.Fatalf("flow_step_id = %q, want work", agentRun.Metadata["flow_step_id"])
	}
}

// TestServiceFlowAccessorsNilSafe covers nil-safe + normal paths of
// FlowRegistry / FlowRefs / flowStateRoot.
func TestServiceFlowAccessorsNilSafe(t *testing.T) {
	var nilSvc *Service
	if nilSvc.FlowRegistry() != nil {
		t.Fatalf("nil FlowRegistry should be nil")
	}
	if nilSvc.FlowRefs() != nil {
		t.Fatalf("nil FlowRefs should be nil")
	}
	if got := nilSvc.flowStateRoot(); got != ".xira" {
		t.Fatalf("nil flowStateRoot should default to .xira, got %q", got)
	}
	svc := newTestService(t, Config{})
	if got := svc.flowStateRoot(); got == ".xira" || got == "" {
		t.Fatalf("flowStateRoot with state dir should not be .xira/empty, got %q", got)
	}
	_ = svc.FlowRegistry()
	_ = svc.FlowRefs()
}

// TestFlowBridgePolicyValue covers every branch: nil run, missing key, valid
// bool, un-parseable value (returns raw).
func TestFlowBridgePolicyValue(t *testing.T) {
	svc := newTestService(t, Config{})
	b := newFlowBridge(svc)
	ctx := context.Background()

	if _, ok := b.PolicyValue(ctx, nil, "k"); ok {
		t.Fatalf("nil run should return ok=false")
	}
	run := &flow.Run{Input: map[string]string{}}
	if _, ok := b.PolicyValue(ctx, run, "missing"); ok {
		t.Fatalf("missing key should return ok=false")
	}
	run.Input["flag"] = " true "
	if v, ok := b.PolicyValue(ctx, run, "flag"); !ok || v != true {
		t.Fatalf("valid bool: got v=%v ok=%v", v, ok)
	}
	run.Input["other"] = "maybe"
	if v, ok := b.PolicyValue(ctx, run, "other"); !ok || v != "maybe" {
		t.Fatalf("un-parseable: got v=%v ok=%v", v, ok)
	}
}

func TestFlowBridgeNilServiceErrors(t *testing.T) {
	var b *flowBridge
	ctx := context.Background()
	if _, err := b.RunAgent(ctx, flow.AgentTurnRequest{}); err == nil {
		t.Fatal("nil bridge RunAgent should fail")
	}
	if _, err := b.CreateHumanRequest(ctx, flow.CreateHumanRequestInput{}); err == nil {
		t.Fatal("nil bridge CreateHumanRequest should fail")
	}
	if _, err := b.GetHumanRequest(ctx, "hrq-1"); err == nil {
		t.Fatal("nil bridge GetHumanRequest should fail")
	}
	if _, err := b.AgentStepStatus(ctx, nil, flow.Step{}); err == nil {
		t.Fatal("nil bridge AgentStepStatus should fail")
	}
}

func TestFlowBridgeAgentStepStatusLoadsRunStatus(t *testing.T) {
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})
	b := newFlowBridge(rt)
	ctx := context.Background()

	if _, err := b.AgentStepStatus(ctx, &flow.Run{Steps: map[string]flow.StepState{}}, flow.Step{ID: "missing"}); err == nil {
		t.Fatal("missing step should fail")
	}
	run := &flow.Run{Steps: map[string]flow.StepState{
		"work": {},
	}}
	if _, err := b.AgentStepStatus(ctx, run, flow.Step{ID: "work"}); err == nil {
		t.Fatal("step without agent run id should fail")
	}

	if err := rt.RunStore().SaveRun(TurnResponse{RunID: "agent-run-1", Status: "completed", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("save agent run: %v", err)
	}
	run.Steps["work"] = flow.StepState{AgentRunID: "agent-run-1"}
	status, err := b.AgentStepStatus(ctx, run, flow.Step{ID: "work"})
	if err != nil {
		t.Fatalf("AgentStepStatus: %v", err)
	}
	if status != "completed" {
		t.Fatalf("status = %q, want completed", status)
	}
}

// TestFlowBridgeAgentStepStatusErrors covers the error arms.
func TestFlowBridgeAgentStepStatusErrors(t *testing.T) {
	svc := newTestService(t, Config{})
	b := newFlowBridge(svc)
	ctx := context.Background()

	if _, err := b.AgentStepStatus(ctx, nil, flow.Step{ID: "s"}); err == nil {
		t.Fatalf("nil run should error")
	}
	run := &flow.Run{Steps: map[string]flow.StepState{}}
	if _, err := b.AgentStepStatus(ctx, run, flow.Step{ID: "nope"}); err == nil {
		t.Fatalf("missing step should error")
	}
	run.Steps["s"] = flow.StepState{}
	if _, err := b.AgentStepStatus(ctx, run, flow.Step{ID: "s"}); err == nil {
		t.Fatalf("step without agent run id should error")
	}
}
