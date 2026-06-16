package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/xiramesh/xira/internal/flow"
	"github.com/xiramesh/xira/internal/humanrequest"
)

func TestFlowHumanApprovalUsesRuntimeHumanRequestStore(t *testing.T) {
	rt := newTestService(t, Config{
		RunRoot:   filepath.Join(t.TempDir(), "runs"),
		StateRoot: filepath.Join(t.TempDir(), "state"),
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

	run, err := rt.StartFlow(context.Background(), flow.StartRequest{FlowID: "approval-flow", EntrypointID: "ad_hoc", Input: map[string]string{"request": "approve"}})
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
	if _, err := rt.ResolveHumanRequest(context.Background(), req.ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseApprove,
		Actor:   "tester",
		Message: "approved",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest: %v", err)
	}
	run, err = rt.ResumeFlow(context.Background(), run.ID, req.ID)
	if err != nil {
		t.Fatalf("ResumeFlow: %v", err)
	}
	if run.Status != flow.RunRunning || run.CurrentStepID != "report" {
		t.Fatalf("after resume status=%q current=%q, want running/report", run.Status, run.CurrentStepID)
	}
}

func TestFlowRuntimePolicyInputRoutesToApproval(t *testing.T) {
	rt := newTestService(t, Config{
		RunRoot:   filepath.Join(t.TempDir(), "runs"),
		StateRoot: filepath.Join(t.TempDir(), "state"),
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
