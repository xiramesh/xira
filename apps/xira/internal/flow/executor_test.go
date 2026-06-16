package flow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeAgentRunner is a controllable AgentRunner for executor/kernel tests.
type fakeAgentRunner struct {
	resp AgentTurnResponse
	err  error
	last AgentTurnRequest
}

func (f *fakeAgentRunner) RunAgent(ctx context.Context, req AgentTurnRequest) (AgentTurnResponse, error) {
	f.last = req
	if f.err != nil {
		return AgentTurnResponse{}, f.err
	}
	return f.resp, nil
}

// fakeHumanCreator records created approval requests.
type fakeHumanCreator struct {
	created []CreateHumanRequestInput
	ids     map[string]string
	err     error
}

func newFakeHumanCreator() *fakeHumanCreator {
	return &fakeHumanCreator{ids: map[string]string{}}
}

func (f *fakeHumanCreator) CreateHumanRequest(ctx context.Context, input CreateHumanRequestInput) (HumanRequestView, error) {
	if f.err != nil {
		return HumanRequestView{}, f.err
	}
	f.created = append(f.created, input)
	id := "hrq_test_" + input.DedupeKey
	f.ids[input.DedupeKey] = id
	return HumanRequestView{ID: id, Source: input.Source, Kind: input.Kind, Status: "pending", Question: input.Question}, nil
}

func TestAgentExecutorBuildsTurnRequestFromStep(t *testing.T) {
	runner := &fakeAgentRunner{resp: AgentTurnResponse{Status: "completed", RunID: "r1", FinalResponse: "done"}}
	exec := &AgentExecutor{Agent: runner}
	run := &Run{
		ID:     "fr_1",
		FlowID: "test",
		Input:  map[string]string{"repo": "/repo"},
		Steps:  map[string]StepState{},
	}
	step := Step{
		ID: "intake", Objective: "Produce a task spec.",
		Executor:       Executor{Agent: "dev-intake"},
		Instructions:   []string{"Use /skill task-spec.", "Keep implementation out."},
		Constraints:    []string{"Do not modify files."},
		RequiredSkills: []string{"task-spec"},
		Inputs:         map[string]string{"request": "${input.request}", "repo": "${input.repo}"},
		OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "task_spec"}}},
	}
	run.Input["request"] = "fix the bug"
	_, err := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if runner.last.AgentID != "dev-intake" {
		t.Errorf("agent_id = %q, want dev-intake", runner.last.AgentID)
	}
	if !strings.Contains(runner.last.Message, "Produce a task spec.") {
		t.Errorf("message missing objective: %q", runner.last.Message)
	}
	if !strings.Contains(runner.last.Message, "Use /skill task-spec.") {
		t.Errorf("message missing instruction: %q", runner.last.Message)
	}
	if !strings.Contains(runner.last.Message, "fix the bug") {
		t.Errorf("message missing resolved request input: %q", runner.last.Message)
	}
	if !strings.Contains(runner.last.Message, "task_spec") {
		t.Errorf("message should advertise required slots: %q", runner.last.Message)
	}
	if runner.last.Metadata["flow_run_id"] != "fr_1" || runner.last.Metadata["flow_step_id"] != "intake" {
		t.Errorf("metadata = %+v", runner.last.Metadata)
	}
}

func TestAgentExecutorPassesStepAllowedTools(t *testing.T) {
	runner := &fakeAgentRunner{resp: AgentTurnResponse{Status: "completed", RunID: "r1", FinalResponse: "done"}}
	exec := &AgentExecutor{Agent: runner}
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	step := Step{
		ID:        "write",
		Objective: "Write one file.",
		Executor:  Executor{Agent: "writer", Tools: []string{"read_file", "write_file"}, toolsConfigured: true},
	}
	if _, err := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step); err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if got := strings.Join(runner.last.AllowedTools, ","); got != "read_file,write_file" {
		t.Fatalf("allowed tools = %q", got)
	}
	if !runner.last.AllowedToolsSet {
		t.Fatal("allowed tools set = false, want true")
	}
}

func TestAgentExecutorPassesExplicitEmptyStepAllowedTools(t *testing.T) {
	runner := &fakeAgentRunner{resp: AgentTurnResponse{Status: "completed", RunID: "r1", FinalResponse: "done"}}
	exec := &AgentExecutor{Agent: runner}
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	step := Step{
		ID:        "think",
		Objective: "Think without tools.",
		Executor:  Executor{Agent: "planner", Tools: []string{}, toolsConfigured: true},
	}
	if _, err := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step); err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if len(runner.last.AllowedTools) != 0 {
		t.Fatalf("allowed tools = %+v, want empty", runner.last.AllowedTools)
	}
	if !runner.last.AllowedToolsSet {
		t.Fatal("allowed tools set = false, want true")
	}
}

func TestExecutorYAMLTracksExplicitEmptyTools(t *testing.T) {
	var step Step
	if err := yaml.Unmarshal([]byte(`id: think
objective: Think without tools.
executor:
  agent: planner
  tools: []
`), &step); err != nil {
		t.Fatal(err)
	}
	if !step.Executor.toolsConfigured {
		t.Fatal("toolsConfigured = false, want true for explicit tools: []")
	}
	if len(step.Executor.Tools) != 0 {
		t.Fatalf("tools = %+v, want empty", step.Executor.Tools)
	}
}

func TestAgentExecutorMapsCompletedResponse(t *testing.T) {
	// Agent returns a fenced json block with declared slot.
	runner := &fakeAgentRunner{resp: AgentTurnResponse{
		Status:        "completed",
		RunID:         "r1",
		FinalResponse: "```json\n{\"task_spec\": {\"scope\": \"fix bug\"}}\n```",
		Artifacts:     []string{"artifacts/intake/task_spec.yaml"},
	}}
	exec := &AgentExecutor{Agent: runner}
	step := Step{ID: "intake", Objective: "x", Executor: Executor{Agent: "dev-intake"},
		OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "task_spec"}}}}
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	result, err := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if result.Status != StepCompleted {
		t.Errorf("status = %q, want completed", result.Status)
	}
	if result.AgentRunID != "r1" {
		t.Errorf("agent_run_id = %q, want r1", result.AgentRunID)
	}
	if result.Outputs["task_spec"].Value == nil {
		t.Errorf("task_spec output not extracted: %+v", result.Outputs)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Path != "artifacts/intake/task_spec.yaml" {
		t.Errorf("artifacts = %+v", result.Artifacts)
	}
}

func TestAgentExecutorMapsCompletedYAMLFencedResponse(t *testing.T) {
	runner := &fakeAgentRunner{resp: AgentTurnResponse{
		Status:        "completed",
		RunID:         "r1",
		FinalResponse: "```yaml\ntask_spec: yaml spec\nacceptance_criteria:\n  - works\n```",
	}}
	exec := &AgentExecutor{Agent: runner}
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	step := Step{ID: "intake", Objective: "intake", Executor: Executor{Agent: "dev-intake"}, OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "task_spec"}, {ID: "acceptance_criteria"}}}}
	result, err := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if err != nil {
		t.Fatalf("ExecuteStep: %v", err)
	}
	if result.Status != StepCompleted {
		t.Fatalf("status = %q error=%q, want completed", result.Status, result.Error)
	}
	if result.Outputs["task_spec"].Value != "yaml spec" {
		t.Errorf("task_spec = %#v", result.Outputs["task_spec"].Value)
	}
	if result.Outputs["acceptance_criteria"].Value == nil {
		t.Errorf("acceptance_criteria missing from YAML output: %+v", result.Outputs)
	}
}

func TestAgentExecutorMapsCompletedResponseSingleSlotFallback(t *testing.T) {
	// No structured block; exactly one required slot -> summary fallback.
	runner := &fakeAgentRunner{resp: AgentTurnResponse{
		Status:        "completed",
		RunID:         "r1",
		FinalResponse: "Here is a plain text report.",
	}}
	exec := &AgentExecutor{Agent: runner}
	step := Step{ID: "report", Objective: "x", Executor: Executor{Agent: "dev-reporter"},
		OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "final_report"}}}}
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	result, _ := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if result.Status != StepCompleted {
		t.Errorf("status = %q, want completed", result.Status)
	}
	if result.Outputs["final_report"].Summary != "Here is a plain text report." {
		t.Errorf("final_report summary = %q", result.Outputs["final_report"].Summary)
	}
}

func TestAgentExecutorMapsFailedResponse(t *testing.T) {
	runner := &fakeAgentRunner{resp: AgentTurnResponse{Status: "failed", RunID: "r1", FinalResponse: "boom"}}
	exec := &AgentExecutor{Agent: runner}
	step := Step{ID: "x", Objective: "x", Executor: Executor{Agent: "a"},
		OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "out"}}}}
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	result, _ := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if result.Status != StepFailed {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if result.Error != "boom" {
		t.Errorf("error = %q, want boom", result.Error)
	}
}

func TestAgentExecutorMapsWaitingHumanResponse(t *testing.T) {
	runner := &fakeAgentRunner{resp: AgentTurnResponse{
		Status:        AgentStatusWaitingHuman,
		RunID:         "r1",
		FinalResponse: "need input",
		HumanRequests: []AgentHumanRequestView{{ID: "hrq_agent_1", Source: "agent_request"}},
		Interrupt: &AgentInterruptView{
			Status: AgentStatusWaitingHuman, Reason: "human_request",
			BlockedBy: []AgentBlockedByView{{Type: "human_request", HumanRequestID: "hrq_agent_1"}},
		},
	}}
	exec := &AgentExecutor{Agent: runner}
	step := Step{ID: "x", Objective: "x", Executor: Executor{Agent: "a"},
		OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "out"}}}}
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	result, _ := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if result.Status != StepWaitingHuman {
		t.Errorf("status = %q, want waiting_human", result.Status)
	}
	if len(result.HumanRequestIDs) != 1 || result.HumanRequestIDs[0] != "hrq_agent_1" {
		t.Errorf("human_request_ids = %+v", result.HumanRequestIDs)
	}
	if result.Interrupt == nil || result.Interrupt["reason"] != "human_request" {
		t.Errorf("interrupt = %+v", result.Interrupt)
	}
}

func TestAgentExecutorRunAgentErrorMapsFailed(t *testing.T) {
	runner := &fakeAgentRunner{err: errors.New("network")}
	exec := &AgentExecutor{Agent: runner}
	step := Step{ID: "x", Objective: "x", Executor: Executor{Agent: "a"},
		OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "out"}}}}
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	result, err := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if err != nil {
		t.Errorf("ExecuteStep returned error %v, want failed result", err)
	}
	if result.Status != StepFailed {
		t.Errorf("status = %q, want failed", result.Status)
	}
	if result.Error != "network" {
		t.Errorf("error = %q", result.Error)
	}
}

func TestAgentExecutorCompletedMissingRequiredSlotFails(t *testing.T) {
	// Completed but no structured payload and TWO required slots -> missing.
	runner := &fakeAgentRunner{resp: AgentTurnResponse{Status: "completed", RunID: "r1", FinalResponse: "plain"}}
	exec := &AgentExecutor{Agent: runner}
	step := Step{ID: "x", Objective: "x", Executor: Executor{Agent: "a"},
		OutputContract: OutputContract{RequiredSlots: []OutputSlot{{ID: "a"}, {ID: "b"}}}}
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	result, _ := exec.ExecuteStep(context.Background(), run, &Definition{ID: "test"}, step)
	if result.Status != StepFailed {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Error, "a") || !strings.Contains(result.Error, "b") {
		t.Errorf("error should mention missing slots: %q", result.Error)
	}
}

func TestResolveStepInputsFromInput(t *testing.T) {
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{"repo": "/r", "request": "fix"}, Steps: map[string]StepState{}}
	step := Step{ID: "intake", Objective: "x", Executor: Executor{Agent: "a"},
		Inputs: map[string]string{"repo": "${input.repo}", "request": "${input.request}"}}
	got, err := ResolveStepInputs(run, step)
	if err != nil {
		t.Fatalf("ResolveStepInputs: %v", err)
	}
	if got["repo"] != "/r" {
		t.Errorf("repo = %q", got["repo"])
	}
	if got["request"] != "fix" {
		t.Errorf("request = %q", got["request"])
	}
}

func TestResolveStepInputsFromPreviousStepOutput(t *testing.T) {
	run := &Run{
		ID:     "fr_1",
		FlowID: "test",
		Input:  map[string]string{},
		Steps: map[string]StepState{
			"design": {Status: StepCompleted, Outputs: map[string]OutputRef{
				"implementation_plan": {Artifact: "artifacts/design/plan.md"},
				"count":               {Value: 3},
				"status":              {Value: "selected"},
				"summary":             {Summary: "summary output"},
			}},
		},
	}
	step := Step{ID: "implement", Objective: "x", Executor: Executor{Agent: "a"},
		Inputs: map[string]string{
			"plan":   "${outputs.design.implementation_plan}",
			"count":  "${outputs.design.count}",
			"status": "${outputs.design.status}",
			"brief":  "${outputs.design.summary}",
		}}
	got, err := ResolveStepInputs(run, step)
	if err != nil {
		t.Fatalf("ResolveStepInputs: %v", err)
	}
	if got["plan"] != "artifacts/design/plan.md" {
		t.Errorf("plan = %q", got["plan"])
	}
	if got["count"] != "3" {
		t.Errorf("count = %q, want 3", got["count"])
	}
	if got["status"] != "selected" {
		t.Errorf("status = %q", got["status"])
	}
	if got["brief"] != "summary output" {
		t.Errorf("brief = %q, want summary output", got["brief"])
	}
}

func TestResolveStepInputsFailsOnMissingRequiredOutput(t *testing.T) {
	run := &Run{ID: "fr_1", FlowID: "test", Input: map[string]string{}, Steps: map[string]StepState{}}
	step := Step{ID: "implement", Objective: "x", Executor: Executor{Agent: "a"},
		Inputs: map[string]string{"plan": "${outputs.design.missing_slot}"}}
	_, err := ResolveStepInputs(run, step)
	if err == nil {
		t.Fatal("expected error for missing required output slot")
	}
	if !strings.Contains(err.Error(), "design") {
		t.Errorf("error should mention the missing step: %v", err)
	}
}

func TestResolveStepInputsOrFallback(t *testing.T) {
	// ${input.request || outputs.select_issue.selected_issue}
	run := &Run{
		ID: "fr_1", FlowID: "test",
		Input: map[string]string{"request": ""},
		Steps: map[string]StepState{
			"select_issue": {Status: StepCompleted, Outputs: map[string]OutputRef{"selected_issue": {Value: "issue-42"}}},
		},
	}
	step := Step{ID: "intake", Objective: "x", Executor: Executor{Agent: "a"},
		Inputs: map[string]string{"request": "${input.request || outputs.select_issue.selected_issue}"}}
	got, err := ResolveStepInputs(run, step)
	if err != nil {
		t.Fatalf("ResolveStepInputs: %v", err)
	}
	if got["request"] != "issue-42" {
		t.Errorf("request = %q, want issue-42 (fallback)", got["request"])
	}
}
