package flow

import (
	"context"
	"encoding/json"
	"testing"
)

// devRunStepOutputs maps each DevRun agent step to the structured output
// payload the fake agent returns. Slot keys match the step's required_slots.
// These satisfy DevRun's branch expressions.
func devRunStepOutputs() map[string]map[string]any {
	return map[string]map[string]any{
		"select_issue": {"selected_issue": map[string]any{"status": "selected"}, "selection_reason": "fits", "risk_level": "low"},
		"intake":       {"task_spec": "spec", "acceptance_criteria": "criteria"},
		"design":       {"design_doc": "doc", "implementation_plan": "plan", "verification_plan": "vplan"},
		"prepare_branch": {"branch": "xira/devrun-branch"},
		"implement":    {"change_summary": "summary", "changed_files": []any{"a.go"}, "risk_notes": "none"},
		"verify":       {"verification_result": map[string]any{"status": "passed"}},
		"create_pr":    {"pr": map[string]any{"url": "https://example/pr/1"}},
		"review":       {"review_report": "report", "blocking_findings_count": 0, "merge_recommendation": "approve"},
		"fix":          {"fix_summary": "fixed", "changed_files": []any{"a.go"}},
		"merge":        {"merge_result": "merged"},
		"report":       {"final_report": "done", "residual_risks": "none"},
	}
}

// devRunFakeRunner returns canned structured outputs per step id, encoded as a
// fenced json block in FinalResponse so the executor's extractor maps them to
// declared slots. For human_approval steps it returns nothing (those are
// handled by the approval path, not the runner).
type devRunFakeRunner struct {
	outputs  map[string]map[string]any
	override map[string]map[string]any // per-test overrides
	calls    []string
}

func newDevRunFakeRunner() *devRunFakeRunner {
	return &devRunFakeRunner{
		outputs:  devRunStepOutputs(),
		override: map[string]map[string]any{},
	}
}

func (r *devRunFakeRunner) withOverride(stepID string, out map[string]any) *devRunFakeRunner {
	r.override[stepID] = out
	return r
}

func (r *devRunFakeRunner) RunAgent(ctx context.Context, req AgentTurnRequest) (AgentTurnResponse, error) {
	// Identify the step from metadata.
	stepID := req.Metadata["flow_step_id"]
	r.calls = append(r.calls, stepID)
	out, ok := r.override[stepID]
	if !ok {
		out = r.outputs[stepID]
	}
	return AgentTurnResponse{
		Status:        "completed",
		RunID:         "fake-" + stepID,
		FinalResponse: fencedJSON(out),
	}, nil
}

// fencedJSON renders m as a fenced json block for the extractor.
func fencedJSON(m map[string]any) string {
	if m == nil {
		return ""
	}
	b, _ := json.Marshal(m)
	return "```json\n" + string(b) + "\n```"
}

// newDevRunKernel builds a kernel wired to the DevRun flow file and a fake
// runner. policy controls require_design_approval / require_merge_approval.
func newDevRunKernel(t *testing.T, policy map[string]any, runner AgentRunner) (*Kernel, *fakeHumanCreator, *fakeResolver) {
	t.Helper()
	def, err := LoadDefinition(devRunFlowPath(t))
	if err != nil {
		t.Fatalf("LoadDefinition devrun: %v", err)
	}
	if runner == nil {
		runner = newDevRunFakeRunner()
	}
	hr := newFakeHumanCreator()
	exec := &AgentExecutor{Agent: runner, Human: hr}
	resolver := &fakeResolver{requests: map[string]ResolvedHumanRequest{}}
	k := &Kernel{
		Store:       newTestStore(t),
		Definitions: staticDefinitions{defs: map[string]*Definition{"devrun": def}},
		Executor:    exec,
		Policy:      policyMap{values: policy},
		Resolver:    resolver,
	}
	return k, hr, resolver
}

// advanceToTerminal advances the run repeatedly until it reaches a terminal
// status or exceeds a step budget.
func advanceToTerminal(t *testing.T, k *Kernel, runID string, budget int) *Run {
	t.Helper()
	run, err := k.Store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	for i := 0; i < budget && run.Status != RunCompleted && run.Status != RunFailed && run.Status != RunCanceled; i++ {
		run, err = k.Advance(context.Background(), runID)
		if err != nil {
			t.Fatalf("Advance[%d]: %v", i, err)
		}
		if run.Status == RunWaitingHuman {
			break
		}
	}
	return run
}

func TestDevRunHappyPathCompletesWithFakeAgent(t *testing.T) {
	// Policy disables design approval; approve_merge is always a human gate,
	// so we resolve it with approve to let merge -> report complete.
	k, _, resolver := newDevRunKernel(t, map[string]any{
		"require_design_approval": false,
	}, nil)
	run, err := k.Start(context.Background(), StartRequest{
		FlowPath: devRunFlowPath(t), EntrypointID: "ad_hoc", Input: map[string]string{"repo": "/r", "request": "fix bug"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	run = advanceToTerminal(t, k, run.ID, 30)
	if run.Status != RunWaitingHuman || run.CurrentStepID != "approve_merge" {
		t.Fatalf("expected waiting_human at approve_merge, got status=%q current=%q", run.Status, run.CurrentStepID)
	}
	// approve_design must NOT have run.
	if s, ok := run.Steps["approve_design"]; ok && s.Status != "" && s.Status != StepSkipped {
		t.Errorf("approve_design unexpectedly ran: %q", s.Status)
	}
	// Resolve approve_merge=approve and complete.
	hrID := run.PendingHumanRequests[0]
	resolver.requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Kind: "approval", Status: "resolved", ResponseKind: "approve",
	}
	run, err = k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	run = advanceToTerminal(t, k, run.ID, 30)
	if run.Status != RunCompleted {
		t.Fatalf("status = %q, want completed", run.Status)
	}
	for _, id := range []string{"intake", "design", "prepare_branch", "implement", "verify", "create_pr", "review", "fix_or_approve", "merge", "report"} {
		if run.Steps[id].Status != StepCompleted {
			t.Errorf("step %s status = %q, want completed", id, run.Steps[id].Status)
		}
	}
	// agent_run_id recorded for agent steps.
	if run.Steps["intake"].AgentRunID == "" {
		t.Errorf("intake missing agent_run_id")
	}
	// required output slots present.
	if run.Steps["design"].Outputs["implementation_plan"].Value == nil {
		t.Errorf("design missing implementation_plan output")
	}
	// StepState carries no large logs; errors are empty on completed steps.
	for id, s := range run.Steps {
		if s.Status == StepCompleted && s.Error != "" {
			t.Errorf("completed step %s has unexpected error %q", id, s.Error)
		}
	}
}

func TestDevRunDesignApprovalPausesAndResumes(t *testing.T) {
	k, hr, resolver := newDevRunKernel(t, map[string]any{
		"require_design_approval": true,
	}, nil)
	run, err := k.Start(context.Background(), StartRequest{
		FlowPath: devRunFlowPath(t), EntrypointID: "ad_hoc", Input: map[string]string{"repo": "/r", "request": "fix bug"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	run = advanceToTerminal(t, k, run.ID, 30)
	if run.Status != RunWaitingHuman {
		t.Fatalf("status = %q, want waiting_human at design gate", run.Status)
	}
	if run.CurrentStepID != "approve_design" {
		t.Fatalf("current_step_id = %q, want approve_design", run.CurrentStepID)
	}
	if len(hr.created) != 1 {
		t.Fatalf("expected 1 created HR, got %d", len(hr.created))
	}
	if hr.created[0].Source != SourceFlowHumanApproval {
		t.Errorf("source = %q, want flow_human_approval", hr.created[0].Source)
	}
	if hr.created[0].Metadata[MetadataFlowStepID] != "approve_design" {
		t.Errorf("metadata flow_step_id = %q", hr.created[0].Metadata[MetadataFlowStepID])
	}
	hrID := run.PendingHumanRequests[0]
	// implement must NOT have executed yet.
	if s, ok := run.Steps["implement"]; ok && (s.Status == StepCompleted || s.Status == StepRunning) {
		t.Errorf("implement executed before design approval: %q", s.Status)
	}
	// Resolve approve and resume.
	resolver.requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Kind: "approval", Status: "resolved", ResponseKind: "approve",
	}
	run, err = k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if run.Status == RunWaitingHuman && run.CurrentStepID == "approve_design" {
		t.Fatalf("still paused at approve_design after resume")
	}
	// Continue; reaches approve_merge (always a gate). Resolve it too.
	run = advanceToTerminal(t, k, run.ID, 30)
	if run.Status != RunWaitingHuman || run.CurrentStepID != "approve_merge" {
		t.Fatalf("expected next pause at approve_merge, got status=%q current=%q", run.Status, run.CurrentStepID)
	}
	if run.Steps["prepare_branch"].Status != StepCompleted {
		t.Errorf("prepare_branch status = %q", run.Steps["prepare_branch"].Status)
	}
	hrID2 := run.PendingHumanRequests[0]
	resolver.requests[hrID2] = ResolvedHumanRequest{
		ID: hrID2, Source: SourceFlowHumanApproval, Kind: "approval", Status: "resolved", ResponseKind: "approve",
	}
	run, err = k.Resume(context.Background(), run.ID, hrID2)
	if err != nil {
		t.Fatalf("Resume merge: %v", err)
	}
	run = advanceToTerminal(t, k, run.ID, 30)
	if run.Status != RunCompleted {
		t.Fatalf("status = %q, want completed after both approvals", run.Status)
	}
}

func TestDevRunMergeApprovalDenyDoesNotMerge(t *testing.T) {
	// approve_merge reject routes to report. In v0 report references
	// ${outputs.merge.merge_result}, which is absent when merge was skipped, so
	// report's input resolution fails and the flow fails — the critical v0
	// invariant is that merge never executes. (A later DevRun revision should
	// make report's inputs tolerant of an absent merge.)
	k, _, resolver := newDevRunKernel(t, map[string]any{
		"require_design_approval": false,
	}, nil)
	run, err := k.Start(context.Background(), StartRequest{
		FlowPath: devRunFlowPath(t), EntrypointID: "ad_hoc", Input: map[string]string{"repo": "/r", "request": "fix bug"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Advance to approve_merge pause.
	run = advanceToTerminal(t, k, run.ID, 30)
	if run.Status != RunWaitingHuman || run.CurrentStepID != "approve_merge" {
		t.Fatalf("expected waiting_human at approve_merge, got status=%q current=%q", run.Status, run.CurrentStepID)
	}
	// Resolve "reject" (DevRun approve_merge options are approve/revise/reject).
	hrID := run.PendingHumanRequests[0]
	resolver.requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Kind: "approval", Status: "resolved", ResponseKind: "reject",
	}
	run, err = k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	run = advanceToTerminal(t, k, run.ID, 30)
	// Critical invariant: merge never executed.
	if s, ok := run.Steps["merge"]; ok && s.Status == StepCompleted {
		t.Errorf("merge executed despite reject: %+v", s)
	}
	// Flow did not complete with a merge; it ends failed at report (missing
	// merge input) per the documented v0 strict-input contract.
	if run.Status != RunFailed {
		t.Errorf("status = %q, want failed (report cannot resolve merge input)", run.Status)
	}
}

func TestDevRunReviewBlockingFindingsEnterFixLoop(t *testing.T) {
	// review returns blocking_findings_count=1 -> fix_or_approve routes to fix.
	// Use a runner that returns blocking findings on review; assert the flow
	// routes into the fix step (the loop entry). v0 does not auto-re-run a
	// completed step on loop-back, so we assert the fix branch is taken rather
	// than the full multi-pass convergence.
	runner := newDevRunFakeRunner().withOverride("review", map[string]any{
		"review_report":           "blocking report",
		"blocking_findings_count": 1,
		"merge_recommendation":    "fix_required",
	})
	k, _, _ := newDevRunKernel(t, map[string]any{
		"require_design_approval": false,
	}, runner)
	run, err := k.Start(context.Background(), StartRequest{
		FlowPath: devRunFlowPath(t), EntrypointID: "ad_hoc", Input: map[string]string{"repo": "/r", "request": "fix bug"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Advance through intake...review, then fix_or_approve routes to fix.
	for i := 0; i < 20; i++ {
		run, err = k.Advance(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("Advance[%d]: %v", i, err)
		}
		if _, ok := run.Steps["fix"]; ok {
			break
		}
	}
	if run.Steps["fix"].Status != StepRunning && run.Steps["fix"].Status != StepCompleted && run.Steps["fix"].Status != StepPending {
		t.Fatalf("fix step not entered; current=%q steps=%+v", run.CurrentStepID, run.Steps)
	}
	// fix_or_approve recorded the decision branch target (fix).
	if run.CurrentStepID != "fix" && run.Steps["fix"].Status == "" {
		t.Errorf("expected flow to route to fix on blocking findings; current=%q", run.CurrentStepID)
	}
	// Verify the review step produced a blocking-findings artifact ref slot.
	if run.Steps["review"].Outputs["blocking_findings_count"].Value != 1 {
		t.Errorf("review blocking_findings_count = %v", run.Steps["review"].Outputs["blocking_findings_count"].Value)
	}
}

func TestDevRunVerifyFailureDoesNotCreatePR(t *testing.T) {
	// Override verify to fail status; verify step completes with a failing
	// result, branches to fix, which then loops back to verify. To keep the
	// test bounded, override fix to also re-fail verify by having verify
	// always fail — the flow enters a verify/fix loop. Cap the budget and
	// assert create_pr never runs and verify failed at least once.
	runner := newDevRunFakeRunner().withOverride("verify", map[string]any{"verification_result": map[string]any{"status": "failed"}})
	k, _, _ := newDevRunKernel(t, map[string]any{
		"require_design_approval": false,
	}, runner)
	run, err := k.Start(context.Background(), StartRequest{
		FlowPath: devRunFlowPath(t), EntrypointID: "ad_hoc", Input: map[string]string{"repo": "/r", "request": "fix bug"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Advance several steps; create_pr must never execute because verify never
	// passes. The fix loop will keep cycling verify->fix; bound it.
	budget := 12
	for i := 0; i < budget; i++ {
		run, err = k.Advance(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("Advance[%d]: %v", i, err)
		}
		if run.Status == RunCompleted || run.Status == RunFailed || run.Status == RunWaitingHuman {
			break
		}
	}
	if s, ok := run.Steps["create_pr"]; ok && s.Status == StepCompleted {
		t.Errorf("create_pr executed despite verify failure")
	}
	// verify ran at least once with a failed-path branch (next=fix).
	verifyRan := false
	for _, c := range runner.callsFromTest() {
		if c == "verify" {
			verifyRan = true
		}
	}
	if !verifyRan {
		t.Errorf("verify never ran")
	}
	// failure includes verify result artifact ref via outputs.
	if run.Steps["verify"].Outputs["verification_result"].Value == nil {
		t.Errorf("verify missing verification_result output")
	}
}

// callsFromTest exposes the recorded call sequence (helper to avoid exposing
// the field directly).
func (r *devRunFakeRunner) callsFromTest() []string {
	return append([]string(nil), r.calls...)
}
