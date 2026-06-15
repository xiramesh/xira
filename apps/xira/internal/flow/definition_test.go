package flow

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFlowYAML writes the given content to a temp file and returns its path.
func writeFlowYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "flow.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write flow yaml: %v", err)
	}
	return path
}

// devRunFlowPath returns the absolute path to the DevRun example flow.yaml.
func devRunFlowPath(t *testing.T) string {
	t.Helper()
	// test package dir is apps/xira/internal/flow
	abs, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "docs", "examples", "flows", "devrun", "flow.yaml"))
	if err != nil {
		t.Fatalf("resolve devrun path: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("devrun flow.yaml not found at %s: %v", abs, err)
	}
	return abs
}

func TestLoadDefinitionDevRun(t *testing.T) {
	def, err := LoadDefinition(devRunFlowPath(t))
	if err != nil {
		t.Fatalf("LoadDefinition: %v", err)
	}
	if def.SchemaVersion != SchemaVersionDefinition {
		t.Errorf("schema_version = %q, want %q", def.SchemaVersion, SchemaVersionDefinition)
	}
	if def.ID != "devrun" {
		t.Errorf("id = %q, want devrun", def.ID)
	}
	if def.Version != "0.1.0" {
		t.Errorf("version = %q, want 0.1.0", def.Version)
	}
	if len(def.Steps) == 0 {
		t.Fatalf("expected steps, got none")
	}
	// Each step must have a non-empty id and objective.
	ids := map[string]bool{}
	for _, step := range def.Steps {
		if step.ID == "" {
			t.Errorf("step with empty id: %+v", step)
		}
		if step.Objective == "" {
			t.Errorf("step %q has empty objective", step.ID)
		}
		if ids[step.ID] {
			t.Errorf("duplicate step id %q", step.ID)
		}
		ids[step.ID] = true
	}
	// Validate entrypoints present and resolvable.
	if len(def.Entrypoints) == 0 {
		t.Fatalf("expected entrypoints, got none")
	}
	wantEntryIDs := map[string]bool{"ad_hoc": false, "bugfix": false, "issue_pickup": false}
	for _, ep := range def.Entrypoints {
		if _, ok := wantEntryIDs[ep.ID]; ok {
			wantEntryIDs[ep.ID] = true
		}
	}
	for id, found := range wantEntryIDs {
		if !found {
			t.Errorf("entrypoint %q missing", id)
		}
	}
	if err := ValidateDefinition(def); err != nil {
		t.Fatalf("ValidateDefinition: %v", err)
	}
}

func TestValidateDefinitionRejectsDuplicateStepID(t *testing.T) {
	path := writeFlowYAML(t, `
schema_version: xira.flow.v0
id: dup
name: Dup
version: 0.1.0
objective: duplicate step ids
entrypoints:
  - id: ad_hoc
    start_step: intake
steps:
  - id: intake
    objective: first
    executor:
      agent: dev-intake
    output_contract:
      required_slots:
        - id: task_spec
  - id: intake
    objective: second
    executor:
      agent: dev-intake
    output_contract:
      required_slots:
        - id: task_spec
`)
	_, err := LoadDefinition(path)
	if err == nil {
		t.Fatalf("expected error loading definition with duplicate step ids")
	}
}

func TestValidateDefinitionRejectsMissingEntrypointStep(t *testing.T) {
	path := writeFlowYAML(t, `
schema_version: xira.flow.v0
id: badentry
name: BadEntry
version: 0.1.0
objective: missing start step
entrypoints:
  - id: ad_hoc
    start_step: does_not_exist
steps:
  - id: intake
    objective: first
    executor:
      agent: dev-intake
    output_contract:
      required_slots:
        - id: task_spec
`)
	_, err := LoadDefinition(path)
	if err == nil {
		t.Fatalf("expected error for entrypoint pointing to missing step")
	}
}

func TestValidateDefinitionRejectsMissingTransitionTarget(t *testing.T) {
	path := writeFlowYAML(t, `
schema_version: xira.flow.v0
id: badtrans
name: BadTrans
version: 0.1.0
objective: missing transition target
entrypoints:
  - id: ad_hoc
    start_step: intake
steps:
  - id: intake
    objective: first
    executor:
      agent: dev-intake
    output_contract:
      required_slots:
        - id: task_spec
    transitions:
      on_success: nowhere
`)
	_, err := LoadDefinition(path)
	if err == nil {
		t.Fatalf("expected error for transition to missing step")
	}
}

func TestValidateDefinitionRejectsUnknownExecutor(t *testing.T) {
	path := writeFlowYAML(t, `
schema_version: xira.flow.v0
id: badexec
name: BadExec
version: 0.1.0
objective: unknown executor
entrypoints:
  - id: ad_hoc
    start_step: intake
steps:
  - id: intake
    objective: first
    executor:
      type: command
    output_contract:
      required_slots:
        - id: task_spec
`)
	_, err := LoadDefinition(path)
	if err == nil {
		t.Fatalf("expected error for command executor")
	}
}

func TestValidateDefinitionRejectsExecutorWithBothAgentAndType(t *testing.T) {
	path := writeFlowYAML(t, `
schema_version: xira.flow.v0
id: ambigexec
name: AmbigExec
version: 0.1.0
objective: both agent and type
entrypoints:
  - id: ad_hoc
    start_step: intake
steps:
  - id: intake
    objective: first
    executor:
      agent: dev-intake
      type: decision
    output_contract:
      required_slots:
        - id: task_spec
`)
	_, err := LoadDefinition(path)
	if err == nil {
		t.Fatalf("expected error for executor with both agent and type")
	}
}

func TestResolveEntrypointAdHoc(t *testing.T) {
	def, err := LoadDefinition(devRunFlowPath(t))
	if err != nil {
		t.Fatalf("LoadDefinition: %v", err)
	}
	ep, step, err := def.ResolveEntrypoint("ad_hoc")
	if err != nil {
		t.Fatalf("ResolveEntrypoint ad_hoc: %v", err)
	}
	if ep.ID != "ad_hoc" {
		t.Errorf("entrypoint id = %q, want ad_hoc", ep.ID)
	}
	if step.ID != "intake" {
		t.Errorf("start step = %q, want intake", step.ID)
	}
}

func TestResolveEntrypointBugfix(t *testing.T) {
	def, err := LoadDefinition(devRunFlowPath(t))
	if err != nil {
		t.Fatalf("LoadDefinition: %v", err)
	}
	ep, step, err := def.ResolveEntrypoint("bugfix")
	if err != nil {
		t.Fatalf("ResolveEntrypoint bugfix: %v", err)
	}
	if ep.ID != "bugfix" {
		t.Errorf("entrypoint id = %q, want bugfix", ep.ID)
	}
	if step.ID != "reproduce" {
		t.Errorf("start step = %q, want reproduce", step.ID)
	}
}

func TestResolveEntrypointIssuePickup(t *testing.T) {
	def, err := LoadDefinition(devRunFlowPath(t))
	if err != nil {
		t.Fatalf("LoadDefinition: %v", err)
	}
	ep, step, err := def.ResolveEntrypoint("issue_pickup")
	if err != nil {
		t.Fatalf("ResolveEntrypoint issue_pickup: %v", err)
	}
	if ep.ID != "issue_pickup" {
		t.Errorf("entrypoint id = %q, want issue_pickup", ep.ID)
	}
	if step.ID != "select_issue" {
		t.Errorf("start step = %q, want select_issue", step.ID)
	}
}

func TestResolveEntrypointRejectsUnknown(t *testing.T) {
	def, err := LoadDefinition(devRunFlowPath(t))
	if err != nil {
		t.Fatalf("LoadDefinition: %v", err)
	}
	if _, _, err := def.ResolveEntrypoint("does_not_exist"); err == nil {
		t.Fatalf("expected error for unknown entrypoint")
	}
}
