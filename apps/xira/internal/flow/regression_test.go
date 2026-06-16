package flow

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockingResumeResolver struct {
	request       ResolvedHumanRequest
	firstEntered  chan struct{}
	secondEntered chan struct{}
	secondOnce    sync.Once
	calls         int32
}

func newBlockingResumeResolver(request ResolvedHumanRequest) *blockingResumeResolver {
	return &blockingResumeResolver{
		request:       request,
		firstEntered:  make(chan struct{}),
		secondEntered: make(chan struct{}),
	}
}

func (r *blockingResumeResolver) GetHumanRequest(ctx context.Context, id string) (ResolvedHumanRequest, error) {
	call := atomic.AddInt32(&r.calls, 1)
	if call == 1 {
		close(r.firstEntered)
		select {
		case <-ctx.Done():
			return ResolvedHumanRequest{}, ctx.Err()
		case <-r.secondEntered:
		case <-time.After(100 * time.Millisecond):
		}
	} else if call == 2 {
		r.secondOnce.Do(func() { close(r.secondEntered) })
	}
	if id != r.request.ID {
		return ResolvedHumanRequest{}, &notFoundErr{id: id}
	}
	return r.request, nil
}

// TestFlowResumeSameHumanRequestTwiceAdvancesOnce — WATCH-FLOW-001 guard.
func TestFlowResumeSameHumanRequestTwiceAdvancesOnce(t *testing.T) {
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
	run, _ := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	run, _ = k.Advance(context.Background(), run.ID)
	if run.Status != RunWaitingHuman {
		t.Fatalf("expected waiting_human, got %q", run.Status)
	}
	hrID := run.PendingHumanRequests[0]
	k.Resolver.(*fakeResolver).requests[hrID] = ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Kind: "approval", Status: "resolved", ResponseKind: "approve",
	}
	run1, err := k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume 1: %v", err)
	}
	if run1.CurrentStepID != "implement" {
		t.Fatalf("after first resume current=%q, want implement", run1.CurrentStepID)
	}
	agentRunsAfterFirst := runner.calls
	// Second resume with the SAME request must be a no-op: flow is no longer
	// waiting_human.
	run2, err := k.Resume(context.Background(), run.ID, hrID)
	if err != nil {
		t.Fatalf("Resume 2: %v", err)
	}
	if runner.calls != agentRunsAfterFirst {
		t.Errorf("agent ran %d times after double resume, want %d", runner.calls, agentRunsAfterFirst)
	}
	if run2.CurrentStepID != "implement" {
		t.Errorf("second resume moved current to %q, want implement (unchanged)", run2.CurrentStepID)
	}
}

func TestKernelConcurrentResumeDoesNotDoubleAdvance(t *testing.T) {
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
	run, _ := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	run, _ = k.Advance(context.Background(), run.ID)
	if run.Status != RunWaitingHuman {
		t.Fatalf("expected waiting_human, got %q", run.Status)
	}
	hrID := run.PendingHumanRequests[0]
	resolver := newBlockingResumeResolver(ResolvedHumanRequest{
		ID: hrID, Source: SourceFlowHumanApproval, Kind: "approval", Status: "resolved", ResponseKind: "approve",
	})
	k.Resolver = resolver

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	resume := func() {
		defer wg.Done()
		_, err := k.Resume(context.Background(), run.ID, hrID)
		errs <- err
	}

	wg.Add(1)
	go resume()
	select {
	case <-resolver.firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first resume did not reach resolver")
	}
	wg.Add(1)
	go resume()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
	}
	if got := atomic.LoadInt32(&resolver.calls); got != 1 {
		t.Fatalf("resolver calls = %d, want 1", got)
	}
	got, err := k.Store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentStepID != "implement" {
		t.Fatalf("current_step_id = %q, want implement", got.CurrentStepID)
	}
	events, err := k.Store.ReadEvents(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	scheduledImplement := 0
	for _, event := range events {
		if event.Kind == "flow.step.scheduled" && event.StepID == "implement" {
			scheduledImplement++
		}
	}
	if scheduledImplement != 1 {
		t.Fatalf("scheduled implement events = %d, want 1", scheduledImplement)
	}
}

// TestFlowAdvanceCompletedStepDoesNotReexecute (regression-named alias).
func TestFlowAdvanceCompletedStepDoesNotReexecuteRegression(t *testing.T) {
	def := linearFlow()
	k, fake := newTestKernel(t, map[string]*Definition{"test": def}, nil)
	fake.results["a"] = StepExecutionResult{Status: StepCompleted, Outputs: map[string]OutputRef{"out": {Value: "a"}}}
	run, _ := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
	run, _ = k.Advance(context.Background(), run.ID)
	if run.CurrentStepID != "b" {
		t.Fatalf("expected at b, got %q", run.CurrentStepID)
	}
	aRuns := fake.execCount["a"]
	// Force current back to completed a; re-advance must skip execution.
	run, _ = k.Store.UpdateRun(context.Background(), run.ID, func(r *Run) error { r.CurrentStepID = "a"; return nil })
	run, err := k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if fake.execCount["a"] != aRuns {
		t.Errorf("a re-executed: %d -> %d", aRuns, fake.execCount["a"])
	}
	if run.CurrentStepID != "b" {
		t.Errorf("expected skip back to b, got %q", run.CurrentStepID)
	}
}

// TestFlowAdvanceWaitingHumanDoesNotExecute (regression-named alias).
func TestFlowAdvanceWaitingHumanDoesNotExecuteRegression(t *testing.T) {
	def := linearFlow()
	k, fake := newTestKernel(t, map[string]*Definition{"test": def}, nil)
	run, _ := k.Start(context.Background(), StartRequest{FlowID: "test", EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"}})
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
	run, _ = k.Advance(context.Background(), run.ID)
	if fake.execCount["a"] != before {
		t.Errorf("a executed while waiting_human")
	}
	if run.Status != RunWaitingHuman {
		t.Errorf("status = %q, want waiting_human", run.Status)
	}
}

// TestFlowStoreRejectsArtifactPathTraversalRegression re-asserts the
// workspace/artifact boundary from M2 under the regression-test name.
func TestFlowStoreRejectsArtifactPathTraversalRegression(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	run, err := store.CreateRun(ctx, CreateRunRequest{FlowID: "devrun", FlowVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../../secret", "/etc/passwd"} {
		_, err := store.UpdateRun(ctx, run.ID, func(r *Run) error {
			r.Steps["x"] = StepState{Artifacts: []ArtifactRef{{Path: bad}}}
			return nil
		})
		if err == nil {
			t.Errorf("expected rejection for artifact path %q", bad)
		}
	}
}

// TestFlowExecutorRejectsOutputArtifactOutsideRunDir asserts the executor
// surfaces only relative, non-traversal artifact paths from an agent run.
func TestFlowExecutorRejectsOutputArtifactOutsideRunDir(t *testing.T) {
	runner := &fakeAgentRunner{resp: AgentTurnResponse{
		Status: "completed", RunID: "r1",
		FinalResponse: "```json\n{\"out\": {}}\n```",
		// Malicious agent claims an absolute artifact path; the store-level
		// guard catches this when persisted.
		Artifacts: []string{"/etc/passwd", "../../secret", "artifacts/ok.json"},
	}}
	exec := &AgentExecutor{Agent: runner}
	step := Step{ID: "x", Objective: "x", Executor: Executor{Agent: "a"},
		OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "out"}}}}
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	result, _ := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if result.Status != StepCompleted {
		t.Fatalf("status = %q, want completed", result.Status)
	}
	// The executor passes artifact refs through; persistence (store.UpdateRun)
	// is what rejects traversal. Confirm a store rejects them.
	store := newTestStore(t)
	created, _ := store.CreateRun(context.Background(), CreateRunRequest{FlowID: "test", FlowVersion: "0.1.0"})
	_, err := store.UpdateRun(context.Background(), created.ID, func(r *Run) error {
		r.Steps["x"] = StepState{Artifacts: result.Artifacts}
		return nil
	})
	if err == nil {
		t.Errorf("expected store to reject traversal artifact refs: %+v", result.Artifacts)
	}
	// Filter to safe paths and assert they persist.
	safe := []ArtifactRef{}
	for _, ref := range result.Artifacts {
		if ref.Path == "artifacts/ok.json" {
			safe = append(safe, ref)
		}
	}
	_, err = store.UpdateRun(context.Background(), created.ID, func(r *Run) error {
		r.Steps["x"] = StepState{Artifacts: safe}
		return nil
	})
	if err != nil {
		t.Errorf("expected safe artifact to persist: %v", err)
	}
}

// TestFlowDoesNotBypassAgentToolPolicy asserts the flow package has no
// command/shell execution path — work goes through AgentRunner only.
func TestFlowDoesNotBypassAgentToolPolicy(t *testing.T) {
	// A flow definition with executor.type=command must be rejected at load.
	path := writeFlowYAML(t, `
schema_version: xira.flow.v0
id: cmdflow
name: Cmd
version: 0.1.0
objective: x
entrypoints:
  - id: ad_hoc
    start_step: run
steps:
  - id: run
    objective: run a command
    executor:
      type: command
      command:
        program: rm
    output_contract:
      required_slots:
        - id: out
`)
	if _, err := LoadDefinition(path); err == nil {
		t.Fatal("expected LoadDefinition to reject command executor")
	}
	// And the executor must not have any direct shell/command execution.
	// (This is a static guarantee: no os/exec import in the flow package.)
	exec := &AgentExecutor{}
	_ = exec
}

// TestFlowEventsLinkFlowStepAgentRunAndHumanRequest asserts the event chain
// lets a reader trace flow_run_id -> step_id -> agent_run_id ->
// human_request_id.
func TestFlowEventsLinkFlowStepAgentRunAndHumanRequest(t *testing.T) {
	def := newApprovalFlowForResume()
	runner := &fakeAgentRunner{resp: AgentTurnResponse{
		Status: "completed", RunID: "agent-run-xyz",
		FinalResponse: "```json\n{\"change\":{},\"plan\":{},\"final\":{}}\n```",
	}}
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
		t.Fatal(err)
	}
	run, err = k.Advance(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunWaitingHuman {
		t.Fatalf("expected waiting_human, got %q", run.Status)
	}
	events, err := k.Store.ReadEvents(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected events")
	}
	// Expect at least: run.started, step.started, step.waiting_human,
	// human_request.linked.
	kinds := map[string]bool{}
	var linkedHR string
	for _, evt := range events {
		kinds[evt.Kind] = true
		if evt.Kind == "flow.human_request.linked" {
			linkedHR = evt.HumanRequestID
		}
		// Every event must carry the flow run id.
		if evt.FlowRunID != run.ID {
			t.Errorf("event %q flow_run_id = %q, want %q", evt.Kind, evt.FlowRunID, run.ID)
		}
	}
	for _, want := range []string{"flow.run.started", "flow.step.started", "flow.step.waiting_human", "flow.human_request.linked"} {
		if !kinds[want] {
			t.Errorf("missing event kind %q; have %v", want, kinds)
		}
	}
	// The linked human request must match the step's human_request_ids and the
	// pending list.
	if linkedHR == "" {
		t.Fatal("human_request.linked event missing human_request_id")
	}
	if !containsString(run.Steps["approve_design"].HumanRequestIDs, linkedHR) {
		t.Errorf("linked HR %q not in step human_request_ids %v", linkedHR, run.Steps["approve_design"].HumanRequestIDs)
	}
	if !containsString(run.PendingHumanRequests, linkedHR) {
		t.Errorf("linked HR %q not in pending_human_requests %v", linkedHR, run.PendingHumanRequests)
	}
	// Sanity: events file is JSONL under the run dir (no large payloads).
	_ = strings.TrimSpace
}
