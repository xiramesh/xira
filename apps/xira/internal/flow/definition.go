package flow

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// controlExecutorTypes are the allowed executor.type values for control steps.
var controlExecutorTypes = map[string]struct{}{
	"human_approval": {},
	"decision":       {},
	"wait_signal":    {},
	"subflow":        {},
}

// LoadDefinition reads and parses a flow definition from path, then validates
// it structurally. It does not evaluate transition expressions.
func LoadDefinition(path string) (*Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read flow definition %s: %w", path, err)
	}
	var raw Definition
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse flow definition %s: %w", path, err)
	}
	if err := ValidateDefinition(&raw); err != nil {
		return nil, err
	}
	return &raw, nil
}

// ValidateDefinition performs structural validation of a flow definition.
func ValidateDefinition(def *Definition) error {
	if def == nil {
		return fmt.Errorf("flow definition is nil")
	}
	if def.SchemaVersion != SchemaVersionDefinition {
		return fmt.Errorf("schema_version must be %q, got %q", SchemaVersionDefinition, def.SchemaVersion)
	}
	if strings.TrimSpace(def.ID) == "" {
		return fmt.Errorf("flow id is required")
	}
	if strings.TrimSpace(def.Name) == "" {
		return fmt.Errorf("flow name is required")
	}
	if strings.TrimSpace(def.Version) == "" {
		return fmt.Errorf("flow version is required")
	}
	if len(def.Steps) == 0 {
		return fmt.Errorf("flow %q must declare at least one step", def.ID)
	}

	// Step id uniqueness + per-step structural rules.
	stepIDs := make(map[string]struct{}, len(def.Steps))
	for i := range def.Steps {
		step := &def.Steps[i]
		if strings.TrimSpace(step.ID) == "" {
			return fmt.Errorf("step at index %d has empty id", i)
		}
		if _, exists := stepIDs[step.ID]; exists {
			return fmt.Errorf("duplicate step id %q", step.ID)
		}
		stepIDs[step.ID] = struct{}{}
		if strings.TrimSpace(step.Objective) == "" {
			return fmt.Errorf("step %q has empty objective", step.ID)
		}
		if err := validateExecutor(step); err != nil {
			return fmt.Errorf("step %q executor: %w", step.ID, err)
		}
		if err := validateStepTransitions(step, stepIDs); err != nil {
			// transition targets may reference steps that come later in the
			// list; collect ids first and validate targets after the loop in a
			// second pass.
			_ = err
		}
	}

	// Transition target resolution (second pass, all ids known now).
	for i := range def.Steps {
		step := &def.Steps[i]
		if err := validateStepTransitions(step, stepIDs); err != nil {
			return fmt.Errorf("step %q transitions: %w", step.ID, err)
		}
	}

	// Entrypoint resolution.
	if len(def.Entrypoints) == 0 {
		return fmt.Errorf("flow %q must declare at least one entrypoint", def.ID)
	}
	seenEP := map[string]struct{}{}
	for i := range def.Entrypoints {
		ep := &def.Entrypoints[i]
		if strings.TrimSpace(ep.ID) == "" {
			return fmt.Errorf("entrypoint at index %d has empty id", i)
		}
		if _, exists := seenEP[ep.ID]; exists {
			return fmt.Errorf("duplicate entrypoint id %q", ep.ID)
		}
		seenEP[ep.ID] = struct{}{}
		if strings.TrimSpace(ep.StartStep) == "" {
			return fmt.Errorf("entrypoint %q has empty start_step", ep.ID)
		}
		if _, ok := stepIDs[ep.StartStep]; !ok {
			return fmt.Errorf("entrypoint %q start_step %q does not exist", ep.ID, ep.StartStep)
		}
	}
	return nil
}

func validateExecutor(step *Step) error {
	hasAgent := strings.TrimSpace(step.Executor.Agent) != ""
	hasType := strings.TrimSpace(step.Executor.Type) != ""
	responder := strings.TrimSpace(step.Executor.Responder)
	if responder != "" && step.Executor.Type != "human_approval" {
		return fmt.Errorf("executor responder is only valid for human_approval")
	}
	if responder != "" && responder != "current_sender" && responder != "owner" {
		return fmt.Errorf("human_approval responder must be current_sender or owner")
	}
	if hasAgent && hasType {
		return fmt.Errorf("executor must specify either agent or type, not both")
	}
	if !hasAgent && !hasType {
		return fmt.Errorf("executor must specify agent (work step) or type (control step)")
	}
	if hasType {
		if _, ok := controlExecutorTypes[step.Executor.Type]; !ok {
			return fmt.Errorf("unknown executor type %q (allowed: human_approval, decision, wait_signal, subflow)", step.Executor.Type)
		}
		// Reject command-style executors explicitly even though they aren't in
		// the allowlist; surface a clearer message.
		if step.Executor.Type == "command" || step.Executor.Type == "shell" {
			return fmt.Errorf("command/shell executors are not allowed in flow v0; use executor.agent")
		}
	}
	return nil
}

func validateStepTransitions(step *Step, knownIDs map[string]struct{}) error {
	if step.Transitions.OnSuccess != "" {
		if _, ok := knownIDs[step.Transitions.OnSuccess]; !ok {
			return fmt.Errorf("on_success target %q does not exist", step.Transitions.OnSuccess)
		}
	}
	if step.Transitions.OnFailure != "" {
		if _, ok := knownIDs[step.Transitions.OnFailure]; !ok {
			return fmt.Errorf("on_failure target %q does not exist", step.Transitions.OnFailure)
		}
	}
	for i, br := range step.Transitions.Branches {
		if strings.TrimSpace(br.When) == "" {
			return fmt.Errorf("branch at index %d has empty when", i)
		}
		if strings.TrimSpace(br.Next) == "" {
			return fmt.Errorf("branch at index %d has empty next", i)
		}
		if _, ok := knownIDs[br.Next]; !ok {
			return fmt.Errorf("branch next target %q does not exist", br.Next)
		}
	}
	if step.Retry != nil && step.Retry.OnExhausted != "" {
		if _, ok := knownIDs[step.Retry.OnExhausted]; !ok {
			return fmt.Errorf("retry on_exhausted target %q does not exist", step.Retry.OnExhausted)
		}
	}
	return nil
}

// StepByID returns the step with the given id, or ok=false if not present.
func (d *Definition) StepByID(id string) (Step, bool) {
	if d == nil {
		return Step{}, false
	}
	for _, step := range d.Steps {
		if step.ID == id {
			return step, true
		}
	}
	return Step{}, false
}

// ResolveEntrypoint returns the entrypoint and its start step for the given id.
func (d *Definition) ResolveEntrypoint(id string) (*Entrypoint, *Step, error) {
	if d == nil {
		return nil, nil, fmt.Errorf("flow definition is nil")
	}
	for i := range d.Entrypoints {
		if d.Entrypoints[i].ID == id {
			ep := &d.Entrypoints[i]
			step, ok := d.StepByID(ep.StartStep)
			if !ok {
				return nil, nil, fmt.Errorf("entrypoint %q start_step %q does not exist", ep.ID, ep.StartStep)
			}
			return ep, &step, nil
		}
	}
	return nil, nil, fmt.Errorf("entrypoint %q not found", id)
}
