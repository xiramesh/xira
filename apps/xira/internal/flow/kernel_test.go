package flow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExecutor returns canned results per step id.
type fakeExecutor struct {
	results   map[string]StepExecutionResult
	execOrder []string
	execCount map[string]int
	err       error
}

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{
		results:   map[string]StepExecutionResult{},
		execCount: map[string]int{},
	}
}

func (f *fakeExecutor) ExecuteStep(ctx context.Context, run *Run, def *Definition, step Step) (StepExecutionResult, error) {
	f.execOrder = append(f.execOrder, step.ID)
	f.execCount[step.ID]++
	if f.err != nil {
		return StepExecutionResult{}, f.err
	}
	if r, ok := f.results[step.ID]; ok {
		return r, nil
	}
	// Default to completed with no outputs.
	return StepExecutionResult{Status: StepCompleted}, nil
}

// staticDefinitions implements DefinitionSource for tests.
type staticDefinitions struct {
	defs map[string]*Definition
}

func (s staticDefinitions) Definition(id string) (*Definition, error) {
	if d, ok := s.defs[id]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("flow %q not found", id)
}

// policyMap implements PolicyResolver for tests.
type policyMap struct {
	values map[string]any
}

func (p policyMap) PolicyValue(ctx context.Context, run *Run, key string) (any, bool) {
	v, ok := p.values[key]
	return v, ok
}

// newTestKernel builds a kernel wired to a temp store, fake executor, and the
// given static definitions + policy.
func newTestKernel(t *testing.T, defs map[string]*Definition, policy map[string]any) (*Kernel, *fakeExecutor) {
	t.Helper()
	k := &Kernel{
		Store:       newTestStore(t),
		Definitions: staticDefinitions{defs: defs},
		Executor:    newFakeExecutor(),
		Policy:      policyMap{values: policy},
	}
	return k, k.Executor.(*fakeExecutor)
}

func testStartRequest(entry string) StartRequest {
	return StartRequest{FlowID: "test", EntrypointID: entry, Input: map[string]string{"request": "x"}}
}

// linearFlow builds a 3-step flow: a -> b -> c, all agent steps with on_success.
func linearFlow() *Definition {
	return &Definition{
		SchemaVersion: SchemaVersionDefinition,
		ID:            "test",
		Name:          "Test",
		Version:       "0.1.0",
		Objective:     "test",
		Entrypoints:   []Entrypoint{{ID: "ad_hoc", StartStep: "a"}},
		Steps: []Step{
			{ID: "a", Objective: "a", Executor: Executor{Agent: "dev-intake"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "out"}}}, Transitions: Transitions{OnSuccess: "b"}},
			{ID: "b", Objective: "b", Executor: Executor{Agent: "dev-intake"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "out"}}}, Transitions: Transitions{OnSuccess: "c"}},
			{ID: "c", Objective: "c", Executor: Executor{Agent: "dev-intake"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "out"}}}},
		},
	}
}

func TestKernelStartFlowFromAdHocEntrypoint(t *testing.T) {
	k, _ := newTestKernel(t, map[string]*Definition{"test": linearFlow()}, nil)
	run, err := k.Start(context.Background(), testStartRequest("ad_hoc"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.Status != RunRunning {
		t.Errorf("status = %q, want running", run.Status)
	}
	if run.CurrentStepID != "a" {
		t.Errorf("current_step_id = %q, want a", run.CurrentStepID)
	}
	if run.Steps["a"].Status != StepPending {
		t.Errorf("step a status = %q, want pending", run.Steps["a"].Status)
	}
}

func TestKernelRejectsUnknownEntrypoint(t *testing.T) {
	k, _ := newTestKernel(t, map[string]*Definition{"test": linearFlow()}, nil)
	_, err := k.Start(context.Background(), testStartRequest("nope"))
	if err == nil {
		t.Fatal("expected error for unknown entrypoint")
	}
}

func TestKernelStartRejectsMissingRequiredEntrypointInput(t *testing.T) {
	def := linearFlow()
	def.Inputs = &InputSpec{Required: []string{"repo"}}
	def.Entrypoints[0].RequiredInputs = []string{"repo", "request"}
	k, _ := newTestKernel(t, map[string]*Definition{"test": def}, nil)
	_, err := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	if err == nil {
		t.Fatal("expected missing repo input to be rejected")
	}
	if !strings.Contains(err.Error(), "repo") {
		t.Fatalf("error = %v, want repo mentioned", err)
	}
}

func TestKernelCompletesFlowWhenNoNextStep(t *testing.T) {
	k, fake := newTestKernel(t, map[string]*Definition{"test": linearFlow()}, nil)
	fake.results["a"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"out": {Value: "a"}}}
	fake.results["b"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"out": {Value: "b"}}}
	fake.results["c"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"out": {Value: "c"}}}

	run, err := k.Start(context.Background(), testStartRequest("ad_hoc"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 5 && run.Status != RunCompleted; i++ {
		run, err = k.Advance(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("Advance[%d]: %v", i, err)
		}
	}
	if run.Status != RunCompleted {
		t.Fatalf("status = %q, want completed", run.Status)
	}
	if run.CurrentStepID != "" {
		t.Errorf("current_step_id = %q, want empty on completion", run.CurrentStepID)
	}
	for _, id := range []string{"a", "b", "c"} {
		if run.Steps[id].Status != StepCompleted {
			t.Errorf("step %s status = %q, want completed", id, run.Steps[id].Status)
		}
	}
	if len(fake.execOrder) != 3 {
		t.Errorf("exec order = %v, want 3 steps", fake.execOrder)
	}
}

func TestKernelAdvancesToExplicitNextStep(t *testing.T) {
	k, fake := newTestKernel(t, map[string]*Definition{"test": linearFlow()}, nil)
	fake.results["a"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"out": {Value: "a"}}}
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.CurrentStepID != "b" {
		t.Errorf("current_step_id = %q, want b", run.CurrentStepID)
	}
	if run.Steps["a"].Status != StepCompleted {
		t.Errorf("a status = %q", run.Steps["a"].Status)
	}
	if run.Steps["b"].Status != StepPending {
		t.Errorf("b status = %q, want pending", run.Steps["b"].Status)
	}
}

// decisionFlow builds a flow with a decision step whose transition branches on
// an upstream output slot value.
func decisionFlow() *Definition {
	return &Definition{
		SchemaVersion: SchemaVersionDefinition,
		ID:            "test",
		Name:          "Test",
		Version:       "0.1.0",
		Objective:     "test",
		Entrypoints:   []Entrypoint{{ID: "ad_hoc", StartStep: "decide"}},
		Steps: []Step{
			{
				ID: "decide", Objective: "decide",
				Executor:       Executor{Type: "decision"},
				OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "approval_signal"}}},
				Transitions: Transitions{Branches: []Branch{
					{When: "${outputs.decide.approval_signal == 'approve'}", Next: "merge"},
					{When: "${outputs.decide.approval_signal == 'revise'}", Next: "revise"},
					{When: "${outputs.decide.approval_signal == 'cancel'}", Next: "report"},
				}},
			},
			{ID: "merge", Objective: "merge", Executor: Executor{Agent: "git-op"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "merge_result"}}}},
			{ID: "revise", Objective: "revise", Executor: Executor{Agent: "impl"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "fix"}}}},
			{ID: "report", Objective: "report", Executor: Executor{Agent: "reporter"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "final"}}}},
		},
	}
}

func TestKernelDecisionBranchApprove(t *testing.T) {
	k, fake := newTestKernel(t, map[string]*Definition{"test": decisionFlow()}, nil)
	fake.results["decide"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"approval_signal": {Value: "approve"}}}
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.CurrentStepID != "merge" {
		t.Errorf("current_step_id = %q, want merge", run.CurrentStepID)
	}
}

func TestKernelDecisionBranchRevise(t *testing.T) {
	k, fake := newTestKernel(t, map[string]*Definition{"test": decisionFlow()}, nil)
	fake.results["decide"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"approval_signal": {Value: "revise"}}}
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.CurrentStepID != "revise" {
		t.Errorf("current_step_id = %q, want revise", run.CurrentStepID)
	}
}

func TestKernelDecisionBranchCancel(t *testing.T) {
	k, fake := newTestKernel(t, map[string]*Definition{"test": decisionFlow()}, nil)
	fake.results["decide"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"approval_signal": {Value: "cancel"}}}
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.CurrentStepID != "report" {
		t.Errorf("current_step_id = %q, want report", run.CurrentStepID)
	}
}

func TestKernelRejectsUnresolvableTransition(t *testing.T) {
	def := decisionFlow()
	// Remove on_success fallback so an unmatched branch set fails.
	k, fake := newTestKernel(t, map[string]*Definition{"test": def}, nil)
	fake.results["decide"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"approval_signal": {Value: "unknown"}}}
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))
	run, err := k.Advance(context.Background(), run.ID)
	if err == nil {
		t.Fatalf("expected error for unresolvable transition; got run %+v", run)
	}
	if run.Status != RunFailed {
		t.Errorf("status = %q, want failed", run.Status)
	}
	if run.Steps["decide"].Status != StepFailed {
		t.Errorf("decide status = %q, want failed", run.Steps["decide"].Status)
	}
}

func TestKernelRoutesRetryExhaustedToConfiguredStep(t *testing.T) {
	def := &Definition{
		SchemaVersion: SchemaVersionDefinition,
		ID:            "test",
		Name:          "Test",
		Version:       "0.1.0",
		Objective:     "test",
		Entrypoints:   []Entrypoint{{ID: "ad_hoc", StartStep: "prepare_branch"}},
		Steps: []Step{
			{
				ID:             "prepare_branch",
				Objective:      "prepare branch",
				Executor:       Executor{Agent: "git-op"},
				OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "branch"}}},
				Retry:          &RetryPolicy{MaxAttempts: 1, OnExhausted: "report"},
				Transitions:    Transitions{OnSuccess: "implement"},
			},
			{ID: "implement", Objective: "implement", Executor: Executor{Agent: "impl"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "change"}}}},
			{ID: "report", Objective: "report", Executor: Executor{Agent: "reporter"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "final_report"}}}},
		},
	}
	k, fake := newTestKernel(t, map[string]*Definition{"test": def}, nil)
	fake.results["prepare_branch"] = StepExecutionResult{Status: StepFailed, Error: "branch exists"}
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.Status != RunRunning {
		t.Fatalf("status = %q, want running after on_exhausted route", run.Status)
	}
	if run.CurrentStepID != "report" {
		t.Fatalf("current_step_id = %q, want report", run.CurrentStepID)
	}
	if run.Steps["prepare_branch"].Status != StepFailed {
		t.Fatalf("prepare_branch status = %q, want failed", run.Steps["prepare_branch"].Status)
	}
	if run.Steps["report"].Status != StepPending {
		t.Fatalf("report status = %q, want pending", run.Steps["report"].Status)
	}
}

func TestKernelRetriesFailedStepUntilMaxAttemptsThenRoutes(t *testing.T) {
	def := &Definition{
		SchemaVersion: SchemaVersionDefinition,
		ID:            "test",
		Name:          "Test",
		Version:       "0.1.0",
		Objective:     "test",
		Entrypoints:   []Entrypoint{{ID: "ad_hoc", StartStep: "prepare_branch"}},
		Steps: []Step{
			{
				ID:             "prepare_branch",
				Objective:      "prepare branch",
				Executor:       Executor{Agent: "git-op"},
				OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "branch"}}},
				Retry:          &RetryPolicy{MaxAttempts: 2, OnExhausted: "report"},
				Transitions:    Transitions{OnSuccess: "implement"},
			},
			{ID: "implement", Objective: "implement", Executor: Executor{Agent: "impl"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "change"}}}},
			{ID: "report", Objective: "report", Executor: Executor{Agent: "reporter"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "final_report"}}}},
		},
	}
	k, fake := newTestKernel(t, map[string]*Definition{"test": def}, nil)
	fake.results["prepare_branch"] = StepExecutionResult{Status: StepFailed, Error: "branch exists"}
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))

	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance first failure: %v", err)
	}
	if run.Status != RunRunning || run.CurrentStepID != "prepare_branch" {
		t.Fatalf("after first failure status=%q current=%q, want running prepare_branch", run.Status, run.CurrentStepID)
	}
	if got := run.Steps["prepare_branch"].Status; got != StepPending {
		t.Fatalf("after first failure prepare_branch status=%q, want pending", got)
	}
	if got := run.Steps["prepare_branch"].Attempts; got != 1 {
		t.Fatalf("after first failure attempts=%d, want 1", got)
	}

	run, err = k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance second failure: %v", err)
	}
	if fake.execCount["prepare_branch"] != 2 {
		t.Fatalf("prepare_branch executed %d times, want 2", fake.execCount["prepare_branch"])
	}
	if run.CurrentStepID != "report" {
		t.Fatalf("current_step_id = %q, want report after exhausted retry", run.CurrentStepID)
	}
	if got := run.Steps["prepare_branch"].Attempts; got != 2 {
		t.Fatalf("after exhausted attempts=%d, want 2", got)
	}
	if run.Steps["report"].Status != StepPending {
		t.Fatalf("report status = %q, want pending", run.Steps["report"].Status)
	}
}

// policyFlow uses a runtime.policy branch like DevRun's design step.
func policyFlow() *Definition {
	return &Definition{
		SchemaVersion: SchemaVersionDefinition,
		ID:            "test",
		Name:          "Test",
		Version:       "0.1.0",
		Objective:     "test",
		Entrypoints:   []Entrypoint{{ID: "ad_hoc", StartStep: "design"}},
		Steps: []Step{
			{
				ID: "design", Objective: "design", Executor: Executor{Agent: "arch"},
				OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "plan"}}},
				Transitions: Transitions{Branches: []Branch{
					{When: "${runtime.policy.require_design_approval == true}", Next: "approve_design"},
					{When: "${runtime.policy.require_design_approval != true}", Next: "implement"},
				}},
			},
			{
				ID: "approve_design", Objective: "approve",
				Executor:       Executor{Type: "human_approval"},
				OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "approval_signal"}}},
			},
			{
				ID: "implement", Objective: "implement",
				Executor:       Executor{Agent: "impl"},
				OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "change"}}},
			},
		},
	}
}

func TestKernelPolicyBranchTrueRoutesToApproval(t *testing.T) {
	k, fake := newTestKernel(t, map[string]*Definition{"test": policyFlow()}, map[string]any{"require_design_approval": true})
	fake.results["design"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"plan": {Value: "p"}}}
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.CurrentStepID != "approve_design" {
		t.Errorf("current_step_id = %q, want approve_design", run.CurrentStepID)
	}
}

func TestKernelPolicyBranchFalseSkipsApproval(t *testing.T) {
	k, fake := newTestKernel(t, map[string]*Definition{"test": policyFlow()}, map[string]any{"require_design_approval": false})
	fake.results["design"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"plan": {Value: "p"}}}
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if run.CurrentStepID != "implement" {
		t.Errorf("current_step_id = %q, want implement", run.CurrentStepID)
	}
}

func TestKernelAdvanceCompletedStepDoesNotReexecute(t *testing.T) {
	k, fake := newTestKernel(t, map[string]*Definition{"test": linearFlow()}, nil)
	fake.results["a"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"out": {Value: "a"}}}
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))
	run, _ = k.Advance(context.Background(), run.ID)
	if run.CurrentStepID != "b" {
		t.Fatalf("expected at b, got %q", run.CurrentStepID)
	}
	// Advance again without re-running b: we're at b pending, so this WILL run b.
	// Instead, simulate the case of calling Advance repeatedly on a completed a:
	// by resetting current back to a.
	run, _ = k.Store.UpdateRun(context.Background(), run.ID, func(r *Run) error {
		r.CurrentStepID = "a"
		return nil
	})
	before := fake.execCount["a"]
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if fake.execCount["a"] != before {
		t.Errorf("step a executed %d times, expected no re-execution (still %d)", fake.execCount["a"], before)
	}
	if run.CurrentStepID != "b" {
		t.Errorf("expected to skip back to b, got %q", run.CurrentStepID)
	}
}

func TestKernelAdvanceWaitingHumanDoesNotExecute(t *testing.T) {
	k, fake := newTestKernel(t, map[string]*Definition{"test": linearFlow()}, nil)
	// Force a into waiting_human manually, then Advance must not re-execute.
	run, _ := k.Start(context.Background(), testStartRequest("ad_hoc"))
	_, _ = k.Store.UpdateRun(context.Background(), run.ID, func(r *Run) error {
		s := r.Steps["a"]
		s.Status = StepWaitingHuman
		s.HumanRequestIDs = []string{"hrq_x"}
		r.Steps["a"] = s
		r.Status = RunWaitingHuman
		r.PendingHumanRequests = []string{"hrq_x"}
		return nil
	})
	before := fake.execCount["a"]
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if fake.execCount["a"] != before {
		t.Errorf("step a executed while waiting_human")
	}
	if run.Status != RunWaitingHuman {
		t.Errorf("status = %q, want waiting_human", run.Status)
	}
	if run.CurrentStepID != "a" {
		t.Errorf("current_step_id = %q, want a (stays on waiting step)", run.CurrentStepID)
	}
}

func TestKernelStartLoadsDevRunFlowByPath(t *testing.T) {
	// Use the real DevRun flow file. Kernel without Definitions source but
	// with FlowPath should load it.
	k := &Kernel{
		Store:    newTestStore(t),
		Executor: newFakeExecutor(),
		Policy:   policyMap{values: map[string]any{}},
	}
	run, err := k.Start(context.Background(), StartRequest{
		FlowPath:     devRunFlowPath(t),
		EntrypointID: "ad_hoc",
		Input:        map[string]string{"repo": "/repo", "request": "x"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.FlowID != "devrun" {
		t.Errorf("flow_id = %q, want devrun", run.FlowID)
	}
	if run.EntrypointID != "ad_hoc" {
		t.Errorf("entrypoint_id = %q", run.EntrypointID)
	}
	if run.CurrentStepID != "intake" {
		t.Errorf("current_step_id = %q, want intake", run.CurrentStepID)
	}
	// Ensure path is absolute resolved.
	if !filepath.IsAbs(devRunFlowPath(t)) {
		// helper already returns abs; sanity only.
	}
}

func TestKernelResumeUnknownFlowRun(t *testing.T) {
	k, _ := newTestKernel(t, map[string]*Definition{"test": linearFlow()}, nil)
	_, err := k.Resume(context.Background(), "fr_does_not_exist", "hrq_1")
	if err == nil {
		t.Fatal("expected error for unknown flow run")
	}
	// resume implementation lands in M5; here we only assert the unknown-run
	// error path.
	_ = errors.Is
}
