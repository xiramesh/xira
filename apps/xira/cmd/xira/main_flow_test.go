package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/runtime"
)

// writeFlowTestFile writes a minimal flow definition that uses a single agent
// step and writes the result into one required slot. The agent is the default
// workspace agent so the CLI's fake DeepSeek client can satisfy it.
func writeFlowTestFile(t *testing.T, instance, agentID string) string {
	t.Helper()
	path := filepath.Join(instance, "flow.yaml")
	writeCLIFile(t, path, `schema_version: xira.flow.v0
id: cli-test
name: CLI Test
version: 0.1.0
objective: exercise the flow CLI surface
entrypoints:
  - id: ad_hoc
    start_step: only
steps:
  - id: only
    objective: Produce a one-line task spec.
    executor:
      agent: `+agentID+`
    output_contract:
      required_slots:
        - id: task_spec
    transitions:
      on_success: done
  - id: done
    objective: Report completion.
    executor:
      agent: `+agentID+`
    output_contract:
      required_slots:
        - id: final_report
`)
	return path
}

func writeRequiredInputFlowTestFile(t *testing.T, instance, agentID string) string {
	t.Helper()
	path := filepath.Join(instance, "flow-required.yaml")
	writeCLIFile(t, path, `schema_version: xira.flow.v0
id: cli-required-test
name: CLI Required Input Test
version: 0.1.0
objective: reject missing required input
entrypoints:
  - id: ad_hoc
    start_step: only
    required_inputs:
      - request
steps:
  - id: only
    objective: Produce a one-line task spec.
    executor:
      agent: `+agentID+`
    output_contract:
      required_slots:
        - id: task_spec
`)
	return path
}

func TestFlowRunCommandStartsRun(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	flowPath := writeFlowTestFile(t, instance, "xira-assistant")
	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "flow", "run", "--path", flowPath, "--entrypoint", "ad_hoc", "--input", "request=x")
	var run runtime.FlowRunView
	if err := json.Unmarshal([]byte(out), &run); err != nil {
		t.Fatalf("decode flow run: %v\n%s", err, out)
	}
	if run.FlowID != "cli-test" {
		t.Errorf("flow_id = %q", run.FlowID)
	}
	if run.Status != "running" {
		t.Errorf("status = %q, want running", run.Status)
	}
	if run.CurrentStepID != "only" {
		t.Errorf("current_step_id = %q, want only", run.CurrentStepID)
	}
}

func TestFlowRunCommandRejectsMissingRequiredInput(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	flowPath := writeRequiredInputFlowTestFile(t, instance, "xira-assistant")
	out, err := executeCommandError("--config", filepath.Join(instance, "xira.yaml"), "flow", "run", "--path", flowPath, "--entrypoint", "ad_hoc")
	if err == nil {
		t.Fatalf("flow run without required input succeeded:\n%s", out)
	}
	errText := out + "\n" + err.Error()
	if !strings.Contains(errText, "missing required") || !strings.Contains(errText, "request") {
		t.Fatalf("unexpected error = %v output=%s", err, out)
	}
}

func TestFlowStatusCommandShowsCurrentStep(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	flowPath := writeFlowTestFile(t, instance, "xira-assistant")
	startOut := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "flow", "run", "--path", flowPath, "--entrypoint", "ad_hoc", "--input", "request=x")
	var started runtime.FlowRunView
	if err := json.Unmarshal([]byte(startOut), &started); err != nil {
		t.Fatalf("decode started: %v", err)
	}
	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "flow", "status", started.ID)
	var shown runtime.FlowRunView
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("decode status: %v\n%s", err, out)
	}
	if shown.ID != started.ID {
		t.Errorf("id = %q, want %q", shown.ID, started.ID)
	}
	if shown.CurrentStepID != "only" {
		t.Errorf("current_step_id = %q, want only", shown.CurrentStepID)
	}
}

func TestFlowAdvanceCommandAdvancesStep(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	flowPath := writeFlowTestFile(t, instance, "xira-assistant")
	startOut := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "flow", "run", "--path", flowPath, "--entrypoint", "ad_hoc", "--input", "request=x")
	var started runtime.FlowRunView
	_ = json.Unmarshal([]byte(startOut), &started)

	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "flow", "advance", started.ID)
	var advanced runtime.FlowRunView
	if err := json.Unmarshal([]byte(out), &advanced); err != nil {
		t.Fatalf("decode advance: %v\n%s", err, out)
	}
	if advanced.CurrentStepID != "done" {
		t.Errorf("current_step_id = %q, want done after advance", advanced.CurrentStepID)
	}
	if advanced.Steps["only"].Status != "completed" {
		t.Errorf("only status = %q, want completed", advanced.Steps["only"].Status)
	}
}

func TestFlowResumeCommandRequiresHumanRequestID(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	flowPath := writeFlowTestFile(t, instance, "xira-assistant")
	startOut := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "flow", "run", "--path", flowPath, "--entrypoint", "ad_hoc", "--input", "request=x")
	var started runtime.FlowRunView
	_ = json.Unmarshal([]byte(startOut), &started)

	out, err := executeCommandError("--config", filepath.Join(instance, "xira.yaml"), "flow", "resume", started.ID)
	if err == nil {
		t.Fatalf("resume without --human-request succeeded:\n%s", out)
	}
	if !strings.Contains(out, "required flag") && !strings.Contains(err.Error(), "required flag") {
		t.Fatalf("unexpected error = %v output=%s", err, out)
	}
}

// writeRegisteredFlowTestFile writes a flow into workspace/flows/<id>/flow.yaml
// so that the registry discovers it by id. Returns the flow id.
func writeRegisteredFlowTestFile(t *testing.T, instance, id, description string) string {
	t.Helper()
	content := `schema_version: xira.flow.v0
id: ` + id + `
name: ` + id + `
version: 0.1.0
`
	if description != "" {
		content += "description: " + description + "\n"
	}
	content += `entrypoints:
  - id: ad_hoc
    start_step: answer
    required_inputs:
      - request
steps:
  - id: answer
    objective: Answer ${input.request}.
    executor:
      agent: xira-assistant
`
	writeCLIFile(t, filepath.Join(instance, "workspace", "flows", id, "flow.yaml"), content)
	return id
}

func TestFlowRunAcceptsRegisteredFlowID(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	writeRegisteredFlowTestFile(t, instance, "hello", "")

	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "flow", "run", "hello", "--entrypoint", "ad_hoc", "--input", "request=hi")
	var run runtime.FlowRunView
	if err := json.Unmarshal([]byte(out), &run); err != nil {
		t.Fatalf("decode flow run: %v\n%s", err, out)
	}
	if run.FlowID != "hello" {
		t.Fatalf("flow_id = %q, want hello", run.FlowID)
	}
}

func TestFlowRunUnknownIDErrors(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	_, err := executeCommandError("--config", filepath.Join(instance, "xira.yaml"), "flow", "run", "does-not-exist", "--entrypoint", "ad_hoc", "--input", "request=hi")
	if err == nil {
		t.Fatal("expected error for unknown flow id, got nil")
	}
}

func TestFlowListPrintsRegisteredFlows(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	writeRegisteredFlowTestFile(t, instance, "hello", "A hello flow")
	writeRegisteredFlowTestFile(t, instance, "world", "")

	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "flow", "list")
	var refs []runtime.FlowRef
	if err := json.Unmarshal([]byte(out), &refs); err != nil {
		t.Fatalf("decode flow list: %v\n%s", err, out)
	}
	if len(refs) != 2 {
		t.Fatalf("refs len = %d, want 2: %s", len(refs), out)
	}
	if refs[0].ID != "hello" || refs[1].ID != "world" {
		t.Fatalf("refs order = %+v, want [hello world]", refs)
	}
	if refs[0].Description != "A hello flow" {
		t.Fatalf("hello description = %q", refs[0].Description)
	}
}

func TestFlowInspectReturnsFlow(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	writeRegisteredFlowTestFile(t, instance, "hello", "A hello flow")

	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "flow", "inspect", "hello")
	var ref runtime.FlowRef
	if err := json.Unmarshal([]byte(out), &ref); err != nil {
		t.Fatalf("decode flow inspect: %v\n%s", err, out)
	}
	if ref.ID != "hello" {
		t.Fatalf("id = %q, want hello", ref.ID)
	}
	if ref.Description != "A hello flow" {
		t.Fatalf("description = %q", ref.Description)
	}
}

func TestFlowInspectUnknownIDErrors(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	_, err := executeCommandError("--config", filepath.Join(instance, "xira.yaml"), "flow", "inspect", "nope")
	if err == nil {
		t.Fatal("expected error for unknown flow id, got nil")
	}
}
