package flow

import (
	"context"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
)

// fakeResolver returns canned resolved human requests by id.
type fakeResolver struct {
	requests map[string]ResolvedHumanRequest
	err      error
}

func (f *fakeResolver) GetHumanRequest(ctx context.Context, id string) (ResolvedHumanRequest, error) {
	if f.err != nil {
		return ResolvedHumanRequest{}, f.err
	}
	r, ok := f.requests[id]
	if !ok {
		return ResolvedHumanRequest{}, &notFoundErr{id: id}
	}
	return r, nil
}

type notFoundErr struct{ id string }

func (e *notFoundErr) Error() string { return "not found: " + e.id }

func newApprovalFlowForResume() *Definition {
	return &Definition{
		SchemaVersion: SchemaVersionDefinition,
		ID:            "test",
		Name:          "Test",
		Version:       "0.1.0",
		Objective:     "test",
		Entrypoints:   []Entrypoint{{ID: "ad_hoc", StartStep: "approve_design"}},
		Steps: []Step{
			{
				ID: "approve_design", Objective: "approve",
				Executor:       Executor{Type: "human_approval", Prompt: "Approve?", Options: []string{"approve", "revise", "cancel"}},
				OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "approval_signal"}}},
				Transitions: Transitions{Branches: []Branch{
					{When: "${outputs.approve_design.approval_signal == 'approve'}", Next: "implement"},
					{When: "${outputs.approve_design.approval_signal == 'revise'}", Next: "design"},
					{When: "${outputs.approve_design.approval_signal == 'cancel'}", Next: "report"},
				}},
			},
			{ID: "implement", Objective: "implement", Executor: Executor{Agent: "impl"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "change"}}}},
			{ID: "design", Objective: "design", Executor: Executor{Agent: "arch"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "plan"}}}},
			{ID: "report", Objective: "report", Executor: Executor{Agent: "reporter"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "final"}}}},
		},
	}
}

// startPausedAtApproval runs the flow until it pauses at approve_design.
func startPausedAtApproval(t *testing.T, policy map[string]any) (*Kernel, *Run, string) {
	t.Helper()
	def := newApprovalFlowForResume()
	// Build a kernel whose executor is an *AgentExecutor wired to a fake
	// runner that returns completed for the downstream agent steps.
	runner := &fakeAgentRunner{resp: AgentTurnResponse{
		Status: "completed", RunID: "r1",
		FinalResponse: "```json\n{\"change\":{},\"plan\":{},\"final\":{}}\n```",
	}}
	hr := newFakeHumanCreator()
	exec := &AgentExecutor{Agent: runner, Human: hr}
	k := &Kernel{
		Store:       newTestStore(t),
		Definitions: staticDefinitions{defs: map[string]*Definition{"test": def}},
		Executor:    exec,
		Policy:      policyMap{values: policy},
		Resolver:    &fakeResolver{requests: map[string]ResolvedHumanRequest{}},
	}
	run, err := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	run, err = k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.Status != RunWaitingHuman {
		t.Fatalf("expected waiting_human after advance, got %q", run.Status)
	}
	if len(run.PendingHumanRequests) != 1 {
		t.Fatalf("expected 1 pending human request, got %v", run.PendingHumanRequests)
	}
	hrID := run.PendingHumanRequests[0]
	// Sanity: HR created with flow scope.
	if len(hr.created) != 1 {
		t.Fatalf("expected 1 created HR, got %d", len(hr.created))
	}
	if hr.created[0].Source != SourceFlowHumanApproval {
		t.Errorf("source = %q, want flow_human_approval", hr.created[0].Source)
	}
	if hr.created[0].Metadata[MetadataFlowRunID] != run.ID {
		t.Errorf("metadata flow_run_id = %q", hr.created[0].Metadata[MetadataFlowRunID])
	}
	if hr.created[0].Metadata[MetadataFlowStepID] != "approve_design" {
		t.Errorf("metadata flow_step_id = %q", hr.created[0].Metadata[MetadataFlowStepID])
	}
	return k, run, hrID
}

func TestHumanApprovalStepCreatesHumanRequest(t *testing.T) {
	_, _, hrID := startPausedAtApproval(t, nil)
	if hrID == "" {
		t.Fatal("expected non-empty human request id")
	}
}

func TestHumanApprovalStepPassesRunContextToHumanRequest(t *testing.T) {
	creator := newFakeHumanCreator()
	exec := &AgentExecutor{Human: creator}
	run := &Run{
		ID:      "fr_1",
		FlowID:  "test",
		Context: &channel.InboundContext{Channel: "feishu", ChatID: "oc_flow", SenderID: "u_flow", ChatType: "group"},
		Steps:   map[string]StepState{},
	}
	step := Step{ID: "approve_design", Executor: Executor{Type: "human_approval", Question: "Approve?"}}
	result, err := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if result.Status != StepWaitingHuman {
		t.Fatalf("status = %q, want waiting_human", result.Status)
	}
	if len(creator.created) != 1 {
		t.Fatalf("created = %d, want 1", len(creator.created))
	}
	got := creator.created[0].Context
	if got.Channel != "feishu" || got.ChatID != "oc_flow" || got.SenderID != "u_flow" {
		t.Fatalf("created context = %+v, want original trigger identity", got)
	}
}

func TestHumanApprovalStepUsesQuestionField(t *testing.T) {
	creator := newFakeHumanCreator()
	exec := &AgentExecutor{Human: creator}
	run := &Run{ID: "fr_1", FlowID: "test", Steps: map[string]StepState{}}
	step := Step{ID: "approve_design", Executor: Executor{Type: "human_approval", Question: "Approve custom question?", Prompt: "Prompt fallback?"}}
	result, err := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if result.Status != StepWaitingHuman {
		t.Fatalf("status = %q, want waiting_human", result.Status)
	}
	if len(creator.created) != 1 {
		t.Fatalf("created = %d, want 1", len(creator.created))
	}
	if creator.created[0].Question != "Approve custom question?" {
		t.Fatalf("question = %q, want custom question", creator.created[0].Question)
	}
}

func TestHumanApprovalStepSetsFlowWaitingHuman(t *testing.T) {
	k, run, _ := startPausedAtApproval(t, nil)
	got, _ := k.Store.GetRun(context.Background(), run.ID)
	if got.Status != RunWaitingHuman {
		t.Errorf("status = %q, want waiting_human", got.Status)
	}
	if got.CurrentStepID != "approve_design" {
		t.Errorf("current_step_id = %q, want approve_design", got.CurrentStepID)
	}
	if got.Steps["approve_design"].Status != StepWaitingHuman {
		t.Errorf("step status = %q, want waiting_human", got.Steps["approve_design"].Status)
	}
}

func TestHumanApprovalStepStoresFlowScopeMetadata(t *testing.T) {
	run := &Run{ID: "fr_1", FlowID: "test", Steps: map[string]StepState{}}
	step := Step{ID: "approve_design", Executor: Executor{Type: "human_approval"}}
	meta := buildFlowScopeMetadata(run, step)
	if meta[MetadataScopeType] != MetadataScopeTypeValue {
		t.Errorf("scope_type = %q", meta[MetadataScopeType])
	}
	if meta[MetadataFlowRunID] != "fr_1" || meta[MetadataFlowStepID] != "approve_design" || meta[MetadataFlowID] != "test" {
		t.Errorf("metadata = %+v", meta)
	}
}

func TestKernelPausesFlowOnHumanApproval(t *testing.T) {
	startPausedAtApproval(t, nil) // assertions inside helper
}

func TestKernelPausesFlowOnAgentGeneratedHumanRequest(t *testing.T) {
	def := linearFlow()
	k, fake := newTestKernel(t, map[string]*Definition{"test": def}, nil)
	// Step a returns waiting_human with an agent-generated HR.
	fake.results["a"] = StepExecutionResult{
		Status:          StepWaitingHuman,
		AgentRunID:      "parent-run-1",
		HumanRequestIDs: []string{"hrq_agent_1"},
		Interrupt:       map[string]any{"reason": "agent_request"},
	}
	run, _ := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.Status != RunWaitingHuman {
		t.Fatalf("status = %q, want waiting_human", run.Status)
	}
	if run.Steps["a"].AgentRunID != "parent-run-1" {
		t.Errorf("agent_run_id = %q, want parent-run-1", run.Steps["a"].AgentRunID)
	}
	if len(run.Steps["a"].HumanRequestIDs) != 1 || run.Steps["a"].HumanRequestIDs[0] != "hrq_agent_1" {
		t.Errorf("human_request_ids = %+v", run.Steps["a"].HumanRequestIDs)
	}
	if len(run.PendingHumanRequests) != 1 || run.PendingHumanRequests[0] != "hrq_agent_1" {
		t.Errorf("pending_human_requests = %+v", run.PendingHumanRequests)
	}
}

func TestKernelResumeAgentGeneratedCompletedClearsInterrupt(t *testing.T) {
	k, run, hrID := startPausedAtAgentGeneratedRequest(t)
	k.AgentStatus = staticAgentStatusResolver{status: "completed"}
	k.Resolver.(*fakeResolver).requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceAgentRequest, Status: "resolved", ResponseKind: "approve",
	}

	run, err := k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	step := run.Steps["a"]
	if step.Status != StepCompleted {
		t.Fatalf("step status = %q, want completed", step.Status)
	}
	if step.Interrupt != nil {
		t.Fatalf("completed step kept stale interrupt: %+v", step.Interrupt)
	}
	if len(step.HumanRequestIDs) != 0 || containsString(run.PendingHumanRequests, hrID) {
		t.Fatalf("human request ids not cleared: step=%v pending=%v", step.HumanRequestIDs, run.PendingHumanRequests)
	}
	if run.CurrentStepID != "b" {
		t.Fatalf("current_step_id = %q, want b", run.CurrentStepID)
	}
}

func TestKernelResumeAgentGeneratedFailedClearsInterrupt(t *testing.T) {
	k, run, hrID := startPausedAtAgentGeneratedRequest(t)
	k.AgentStatus = staticAgentStatusResolver{status: "failed"}
	k.Resolver.(*fakeResolver).requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceAgentRequest, Status: "resolved", ResponseKind: "approve",
	}

	run, err := k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	step := run.Steps["a"]
	if run.Status != RunFailed || step.Status != StepFailed {
		t.Fatalf("status run/step = %q/%q, want failed/failed", run.Status, step.Status)
	}
	if step.Interrupt != nil {
		t.Fatalf("failed step kept stale interrupt: %+v", step.Interrupt)
	}
	if len(step.HumanRequestIDs) != 0 || containsString(run.PendingHumanRequests, hrID) {
		t.Fatalf("human request ids not cleared: step=%v pending=%v", step.HumanRequestIDs, run.PendingHumanRequests)
	}
}

func TestKernelResumeAgentGeneratedDeniedClearsInterrupt(t *testing.T) {
	k, run, hrID := startPausedAtAgentGeneratedRequest(t)
	k.Resolver.(*fakeResolver).requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceAgentRequest, Status: "resolved", ResponseKind: "deny",
	}

	run, err := k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	step := run.Steps["a"]
	if run.Status != RunFailed || step.Status != StepFailed {
		t.Fatalf("status run/step = %q/%q, want failed/failed", run.Status, step.Status)
	}
	if step.Interrupt != nil {
		t.Fatalf("denied step kept stale interrupt: %+v", step.Interrupt)
	}
	if len(step.HumanRequestIDs) != 0 || containsString(run.PendingHumanRequests, hrID) {
		t.Fatalf("human request ids not cleared: step=%v pending=%v", step.HumanRequestIDs, run.PendingHumanRequests)
	}
}

func startPausedAtAgentGeneratedRequest(t *testing.T) (*Kernel, *Run, string) {
	t.Helper()
	def := linearFlow()
	k, fake := newTestKernel(t, map[string]*Definition{"test": def}, nil)
	k.Resolver = &fakeResolver{requests: map[string]ResolvedHumanRequest{}}
	fake.results["a"] = StepExecutionResult{
		Status:          StepWaitingHuman,
		AgentRunID:      "parent-run-1",
		HumanRequestIDs: []string{"hrq_agent_1"},
		Interrupt:       map[string]any{"status": "waiting_human", "reason": "agent_request"},
	}
	run, err := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	run, err = k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.Status != RunWaitingHuman {
		t.Fatalf("status = %q, want waiting_human", run.Status)
	}
	if run.Steps["a"].Interrupt == nil {
		t.Fatalf("test setup missing interrupt")
	}
	return k, run, "hrq_agent_1"
}

type staticAgentStatusResolver struct {
	status string
}

func (s staticAgentStatusResolver) AgentStepStatus(ctx context.Context, run *Run, step Step) (string, error) {
	return s.status, nil
}

func TestKernelDoesNotAdvancePastWaitingHumanStep(t *testing.T) {
	def := linearFlow()
	k, fake := newTestKernel(t, map[string]*Definition{"test": def}, nil)
	fake.results["a"] = StepExecutionResult{Status: StepWaitingHuman, HumanRequestIDs: []string{"hrq_1"}}
	run, _ := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	run, _ = k.Advance(context.Background(), run.ID)
	if run.CurrentStepID != "a" {
		t.Errorf("current_step_id = %q, want a (must not advance)", run.CurrentStepID)
	}
	// Re-advance must be a no-op: no execution, no transition.
	before := fake.execCount["a"]
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance again: %v", err)
	}
	if fake.execCount["a"] != before {
		t.Errorf("step a executed again while waiting_human")
	}
	if run.CurrentStepID != "a" {
		t.Errorf("current_step_id advanced to %q while waiting_human", run.CurrentStepID)
	}
}

func TestKernelResumeApprovalApproveAdvances(t *testing.T) {
	k, run, hrID := startPausedAtApproval(t, nil)
	// Mark the HR resolved with approve.
	resolver := k.Resolver.(*fakeResolver)
	resolver.requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Kind: "approval",
		Status: "resolved", ResponseKind: "approve",
	}
	run, err := k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if run.Status == RunWaitingHuman {
		t.Fatalf("still waiting after approve")
	}
	if run.CurrentStepID != "implement" {
		t.Errorf("current_step_id = %q, want implement", run.CurrentStepID)
	}
}

func TestKernelResumeApprovalReviseBranches(t *testing.T) {
	k, run, hrID := startPausedAtApproval(t, nil)
	resolver := k.Resolver.(*fakeResolver)
	resolver.requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Kind: "approval",
		Status: "resolved", ResponseKind: "revise",
	}
	run, err := k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if run.CurrentStepID != "design" {
		t.Errorf("current_step_id = %q, want design", run.CurrentStepID)
	}
}

func TestKernelResumeApprovalCancelCancelsFlow(t *testing.T) {
	// cancel has an explicit branch to report, so it routes there rather than
	// canceling the flow outright.
	k, run, hrID := startPausedAtApproval(t, nil)
	resolver := k.Resolver.(*fakeResolver)
	resolver.requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Kind: "approval",
		Status: "resolved", ResponseKind: "cancel",
	}
	run, err := k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if run.CurrentStepID != "report" {
		t.Errorf("current_step_id = %q, want report (cancel branch)", run.CurrentStepID)
	}
}

func TestKernelResumeApprovalCancelWithoutBranchCancelsRun(t *testing.T) {
	// gate has no cancel branch -> run canceled.
	def := &Definition{
		SchemaVersion: SchemaVersionDefinition, ID: "test", Name: "Test", Version: "0.1.0", Objective: "x",
		Entrypoints: []Entrypoint{{ID: "ad_hoc", StartStep: "gate"}},
		Steps: []Step{
			{ID: "gate", Objective: "gate", Executor: Executor{Type: "human_approval", Options: []string{"approve", "cancel"}}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "approval_signal"}}}},
		},
	}
	hr := newFakeHumanCreator()
	exec := &AgentExecutor{Agent: &fakeAgentRunner{}, Human: hr}
	k := &Kernel{
		Store:       newTestStore(t),
		Definitions: staticDefinitions{defs: map[string]*Definition{"test": def}},
		Executor:    exec,
		Policy:      policyMap{values: nil},
		Resolver:    &fakeResolver{requests: map[string]ResolvedHumanRequest{}},
	}
	run, _ := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	run, _ = k.Advance(context.Background(), run.ID)
	hrID := run.PendingHumanRequests[0]
	k.Resolver.(*fakeResolver).requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Kind: "approval", Status: "resolved", ResponseKind: "cancel",
	}
	run, err := k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if run.Status != RunCanceled {
		t.Errorf("status = %q, want canceled", run.Status)
	}
	if run.Steps["gate"].Status != StepFailed {
		t.Errorf("gate status = %q, want failed", run.Steps["gate"].Status)
	}
}

func TestKernelResumeRejectsUnresolvedRequest(t *testing.T) {
	k, run, hrID := startPausedAtApproval(t, nil)
	resolver := k.Resolver.(*fakeResolver)
	resolver.requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Status: "pending",
	}
	_, err := k.Resume(context.Background(), run.ID, hrID)
	if err == nil {
		t.Fatal("expected error resuming a pending request")
	}
}

func TestKernelResumeIsIdempotent(t *testing.T) {
	def := newApprovalFlowForResume()
	runner := &countingRunner{}
	hr := newFakeHumanCreator()
	exec := &AgentExecutor{Agent: runner, Human: hr}
	k := &Kernel{
		Store:       newTestStore(t),
		Definitions: staticDefinitions{defs: map[string]*Definition{"test": def}},
		Executor:    exec,
		Policy:      policyMap{values: nil},
		Resolver:    &fakeResolver{requests: map[string]ResolvedHumanRequest{}},
	}
	run, err := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	run, err = k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	hrID := run.PendingHumanRequests[0]
	k.Resolver.(*fakeResolver).requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Kind: "approval", Status: "resolved", ResponseKind: "approve",
	}
	first, err := k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume first: %v", err)
	}
	if first.CurrentStepID != "implement" {
		t.Fatalf("first resume: current_step_id = %q, want implement", first.CurrentStepID)
	}
	// Resume again with the same request: flow is no longer waiting_human, so
	// it must be a no-op that does not double-advance or re-run the agent.
	beforeRuns := runner.calls
	second, err := k.Resume(context.Background(), first.ID, hrID)
	if err != nil {
		t.Fatalf("Resume second: %v", err)
	}
	if runner.calls != beforeRuns {
		t.Errorf("agent runner called %d times after second resume, expected no additional calls", runner.calls-beforeRuns)
	}
	if second.CurrentStepID != "implement" {
		t.Errorf("second resume: current_step_id = %q, want implement (unchanged)", second.CurrentStepID)
	}
}

// countingRunner is an AgentRunner that counts RunAgent calls and returns a
// generic completed response.
type countingRunner struct {
	calls int
}

func (c *countingRunner) RunAgent(ctx context.Context, req AgentTurnRequest) (AgentTurnResponse, error) {
	c.calls++
	return AgentTurnResponse{Status: "completed", RunID: "r", FinalResponse: "ok"}, nil
}

func TestKernelResumeRejectsUnknownHumanRequestForStep(t *testing.T) {
	k, run, hrID := startPausedAtApproval(t, nil)
	_ = hrID
	// Resume with an HR id not linked to the current step.
	_, err := k.Resume(context.Background(), run.ID, "hrq_someone_else")
	if err == nil {
		t.Fatal("expected error for HR not linked to current step")
	}
}

func TestKernelPausesWhenParentAgentRunWaitingOnChildHumanRequest(t *testing.T) {
	def := linearFlow()
	k, fake := newTestKernel(t, map[string]*Definition{"test": def}, nil)
	// Parent agent run surfaces a child HR via Interrupt.BlockedBy.
	fake.results["a"] = StepExecutionResult{
		Status:          StepWaitingHuman,
		AgentRunID:      "parent-run-1",
		HumanRequestIDs: []string{"hr_child_1"},
		Interrupt: map[string]any{
			"status": AgentStatusWaitingHuman,
			"reason": "child_human_request",
			"blocked_by": []AgentBlockedByView{{
				Type: "child_human_request", HumanRequestID: "hr_child_1",
			}},
		},
	}
	run, _ := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.Steps["a"].Status != StepWaitingHuman {
		t.Errorf("step status = %q, want waiting_human", run.Steps["a"].Status)
	}
	if run.Status != RunWaitingHuman {
		t.Errorf("run status = %q, want waiting_human", run.Status)
	}
	if run.Steps["a"].AgentRunID != "parent-run-1" {
		t.Errorf("agent_run_id = %q, want parent-run-1", run.Steps["a"].AgentRunID)
	}
	// Flow must not attempt to inspect child run files or delegation join
	// state: it only records the surfaced HR id.
	if len(run.Steps["a"].HumanRequestIDs) != 1 || run.Steps["a"].HumanRequestIDs[0] != "hr_child_1" {
		t.Errorf("human_request_ids = %+v", run.Steps["a"].HumanRequestIDs)
	}
}
