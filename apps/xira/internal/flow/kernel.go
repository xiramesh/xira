package flow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StepExecutor executes a single step and returns its result. The kernel
// invokes it once per Advance; it must be deterministic with respect to the
// run + step inputs. The agent-backed implementation lives in executor.go.
type StepExecutor interface {
	ExecuteStep(ctx context.Context, run *Run, def *Definition, step Step) (StepExecutionResult, error)
}

// StepExecutionResult is what an executor returns for one step.
type StepExecutionResult struct {
	Status          StepStatus
	AgentRunID      string
	HumanRequestIDs []string
	Outputs         map[string]OutputRef
	Artifacts       []ArtifactRef
	Interrupt       map[string]any
	Error           string
}

// DefinitionSource loads a definition. Implemented by a path-based loader in
// executor.go for production, and by a static map in tests.
type DefinitionSource interface {
	Definition(flowID string) (*Definition, error)
}

// PolicyResolver returns runtime policy values used by transition expressions
// of the form ${runtime.policy.<key> == true}. v0 only needs boolean policy
// checks for require_design_approval / require_merge_approval style flags.
type PolicyResolver interface {
	PolicyValue(ctx context.Context, run *Run, key string) (any, bool)
}

// StartRequest starts a new flow run.
type StartRequest struct {
	FlowPath     string
	FlowID       string
	EntrypointID string
	Input        map[string]string
}

// Kernel is the flow state machine: Start -> Advance -> Resume.
type Kernel struct {
	Store       *Store
	Definitions DefinitionSource
	Executor    StepExecutor
	Policy      PolicyResolver
	Resolver    HumanRequestResolver
	AgentStatus AgentStatusResolver
	Clock       func() time.Time

	// registry caches definitions loaded by Start via FlowPath, so subsequent
	// Advance/Resume calls can resolve them by flow_id without the caller
	// re-passing the path. Guarded by registryMu.
	registryMu sync.RWMutex
	registry   map[string]*Definition
}

func (k *Kernel) now() time.Time {
	if k.Clock != nil {
		return k.Clock()
	}
	return time.Now().UTC()
}

// Start loads the definition, resolves the entrypoint, creates a run, and
// initializes the first step as pending with run status running. The loaded
// definition is persisted alongside the run so later Advance/Resume calls in
// a fresh process can reload it.
func (k *Kernel) Start(ctx context.Context, req StartRequest) (*Run, error) {
	if k == nil || k.Store == nil {
		return nil, fmt.Errorf("kernel store is required")
	}
	def, err := k.resolveDefinition(ctx, req)
	if err != nil {
		return nil, err
	}
	ep, startStep, err := def.ResolveEntrypoint(req.EntrypointID)
	if err != nil {
		return nil, err
	}
	if err := validateStartInputs(def, ep, req.Input); err != nil {
		return nil, err
	}
	run, err := k.Store.CreateRun(ctx, CreateRunRequest{
		FlowID:        def.ID,
		FlowVersion:   def.Version,
		EntrypointID:  ep.ID,
		CurrentStepID: startStep.ID,
		Input:         req.Input,
	})
	if err != nil {
		return nil, err
	}
	if err := k.Store.SaveDefinition(run.ID, def); err != nil {
		return nil, err
	}
	k.registerDefinition(def)
	_, err = k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.Status = RunRunning
		r.Steps[startStep.ID] = StepState{Status: StepPending}
		return nil
	})
	if err != nil {
		return nil, err
	}
	updated, err := k.Store.GetRun(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	_ = k.Store.AppendEvents(ctx, run.ID, []Event{{
		Time:      k.now(),
		Kind:      "flow.run.started",
		FlowRunID: run.ID,
		StepID:    startStep.ID,
		Payload: map[string]any{
			"flow_id":       def.ID,
			"entrypoint_id": ep.ID,
		},
	}})
	return updated, nil
}

func (k *Kernel) resolveDefinition(ctx context.Context, req StartRequest) (*Definition, error) {
	if strings.TrimSpace(req.FlowPath) != "" {
		def, err := LoadDefinition(req.FlowPath)
		if err != nil {
			return nil, err
		}
		k.registerDefinition(def)
		return def, nil
	}
	if def, ok := k.lookupDefinition(req.FlowID); ok {
		return def, nil
	}
	if k.Definitions != nil {
		return k.Definitions.Definition(req.FlowID)
	}
	return nil, fmt.Errorf("flow path or definitions source is required")
}

// registerDefinition caches def by its flow id for later Advance/Resume.
func (k *Kernel) registerDefinition(def *Definition) {
	if def == nil {
		return
	}
	k.registryMu.Lock()
	defer k.registryMu.Unlock()
	if k.registry == nil {
		k.registry = map[string]*Definition{}
	}
	k.registry[def.ID] = def
}

func (k *Kernel) lookupDefinition(flowID string) (*Definition, bool) {
	k.registryMu.RLock()
	defer k.registryMu.RUnlock()
	if k.registry == nil {
		return nil, false
	}
	def, ok := k.registry[flowID]
	return def, ok
}

// Advance executes the current step if it is pending/running, records the
// result, and resolves the transition to the next step. If no next step
// exists, the run completes. If the step result is waiting_human, the run
// pauses without advancing.
func (k *Kernel) Advance(ctx context.Context, flowRunID string) (*Run, error) {
	if k == nil || k.Store == nil {
		return nil, fmt.Errorf("kernel store is required")
	}
	if k.Executor == nil {
		return nil, fmt.Errorf("kernel executor is required")
	}
	run, err := k.Store.GetRun(ctx, flowRunID)
	if err != nil {
		return nil, err
	}
	if run.Status == RunCompleted || run.Status == RunFailed || run.Status == RunCanceled {
		return run, nil
	}
	if run.Status == RunWaitingHuman {
		// Paused on human input; no-op without resume.
		return run, nil
	}
	if strings.TrimSpace(run.CurrentStepID) == "" {
		// Nothing to advance; complete.
		return k.complete(ctx, run)
	}

	def, err := k.resolveDefinitionByID(ctx, run)
	if err != nil {
		return nil, err
	}
	step, ok := def.StepByID(run.CurrentStepID)
	if !ok {
		return nil, fmt.Errorf("current step %q not found in flow %q", run.CurrentStepID, run.FlowID)
	}

	// Already completed step: advance past it (idempotent re-advance).
	curState := run.Steps[step.ID]
	if curState.Status == StepCompleted {
		return k.advanceTransition(ctx, run, def, step, curState)
	}
	if curState.Status == StepFailed {
		return run, nil
	}
	if curState.Status == StepWaitingHuman {
		return k.markWaitingHuman(ctx, run, curState, step)
	}

	// Mark running + execute.
	run, err = k.Store.UpdateRun(ctx, flowRunID, func(r *Run) error {
		return MarkStepRunning(r, step.ID)
	})
	if err != nil {
		return nil, err
	}
	_ = k.Store.AppendEvents(ctx, flowRunID, []Event{{
		Time:      k.now(),
		Kind:      "flow.step.started",
		FlowRunID: flowRunID,
		StepID:    step.ID,
	}})

	result, execErr := k.Executor.ExecuteStep(ctx, run, def, step)
	if execErr != nil {
		result.Status = StepFailed
		result.Error = execErr.Error()
	}

	now := k.now()
	run, err = k.Store.UpdateRun(ctx, flowRunID, func(r *Run) error {
		return applyStepResult(r, step.ID, result, now)
	})
	if err != nil {
		return nil, err
	}
	updated := run.Steps[step.ID]

	switch updated.Status {
	case StepWaitingHuman:
		return k.markWaitingHuman(ctx, run, updated, step)
	case StepFailed:
		return k.handleStepFailure(ctx, run, def, updated, step)
	case StepCompleted:
		return k.advanceTransition(ctx, run, def, step, updated)
	default:
		return run, nil
	}
}

func validateStartInputs(def *Definition, ep *Entrypoint, input map[string]string) error {
	required := map[string]struct{}{}
	if def != nil && def.Inputs != nil {
		for _, key := range def.Inputs.Required {
			key = strings.TrimSpace(key)
			if key != "" {
				required[key] = struct{}{}
			}
		}
	}
	if ep != nil {
		for _, key := range ep.RequiredInputs {
			key = strings.TrimSpace(key)
			if key != "" {
				required[key] = struct{}{}
			}
		}
	}
	var missing []string
	for key := range required {
		if strings.TrimSpace(input[key]) == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("missing required flow input(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func applyStepResult(run *Run, stepID string, result StepExecutionResult, now time.Time) error {
	if run == nil {
		return fmt.Errorf("run is required")
	}
	if run.Steps == nil {
		run.Steps = map[string]StepState{}
	}
	step := run.Steps[stepID]
	status := result.Status
	if status == "" {
		status = StepCompleted
	}
	step.Status = status
	step.AgentRunID = result.AgentRunID
	if result.HumanRequestIDs != nil {
		step.HumanRequestIDs = append([]string(nil), result.HumanRequestIDs...)
	}
	if result.Outputs != nil {
		step.Outputs = cloneOutputs(result.Outputs)
	}
	if result.Artifacts != nil {
		step.Artifacts = append([]ArtifactRef(nil), result.Artifacts...)
	}
	if result.Interrupt != nil {
		step.Interrupt = cloneAnyMap(result.Interrupt)
	}
	step.Error = strings.TrimSpace(result.Error)
	if status == StepCompleted || status == StepFailed {
		completed := now
		step.CompletedAt = &completed
	}
	run.Steps[stepID] = step
	return nil
}

func (k *Kernel) markWaitingHuman(ctx context.Context, run *Run, step StepState, defStep Step) (*Run, error) {
	run, err := k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.Status = RunWaitingHuman
		r.PendingHumanRequests = appendUniqueStrings(r.PendingHumanRequests, step.HumanRequestIDs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	events := []Event{{
		Time: k.now(), Kind: "flow.step.waiting_human", FlowRunID: run.ID, StepID: defStep.ID,
	}}
	for _, id := range step.HumanRequestIDs {
		events = append(events, Event{
			Time: k.now(), Kind: "flow.human_request.linked", FlowRunID: run.ID, StepID: defStep.ID, HumanRequestID: id,
		})
	}
	_ = k.Store.AppendEvents(ctx, run.ID, events)
	return run, nil
}

func (k *Kernel) markFailed(ctx context.Context, run *Run, step StepState, defStep Step) (*Run, error) {
	run, err := k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.Status = RunFailed
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = k.Store.AppendEvents(ctx, run.ID, []Event{{
		Time: k.now(), Kind: "flow.step.failed", FlowRunID: run.ID, StepID: defStep.ID, Payload: map[string]any{"error": step.Error},
	}})
	return run, nil
}

func (k *Kernel) handleStepFailure(ctx context.Context, run *Run, def *Definition, state StepState, step Step) (*Run, error) {
	if step.Retry != nil {
		maxAttempts := step.Retry.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 1
		}
		if state.Attempts < maxAttempts {
			run, err := k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
				s := r.Steps[step.ID]
				s.Status = StepPending
				s.Error = ""
				s.StartedAt = nil
				s.CompletedAt = nil
				r.Status = RunRunning
				r.Steps[step.ID] = s
				return nil
			})
			if err != nil {
				return nil, err
			}
			_ = k.Store.AppendEvents(ctx, run.ID, []Event{{Time: k.now(), Kind: "flow.step.retry_scheduled", FlowRunID: run.ID, StepID: step.ID}})
			return run, nil
		}
		if step.Retry.OnExhausted != "" {
			return k.routeAfterFailure(ctx, run, step, step.Retry.OnExhausted, "flow.step.retry_exhausted")
		}
	}
	if step.Transitions.OnFailure != "" {
		return k.routeAfterFailure(ctx, run, step, step.Transitions.OnFailure, "flow.step.failed_routed")
	}
	return k.markFailed(ctx, run, state, step)
}

func (k *Kernel) routeAfterFailure(ctx context.Context, run *Run, step Step, next, eventKind string) (*Run, error) {
	run, err := k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.Status = RunRunning
		r.CurrentStepID = next
		if _, ok := r.Steps[next]; !ok {
			r.Steps[next] = StepState{Status: StepPending}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = k.Store.AppendEvents(ctx, run.ID, []Event{{
		Time: k.now(), Kind: eventKind, FlowRunID: run.ID, StepID: step.ID, Payload: map[string]any{"next": next},
	}, {
		Time: k.now(), Kind: "flow.step.scheduled", FlowRunID: run.ID, StepID: next,
	}})
	return run, nil
}

// advanceTransition resolves the transition for a completed step and moves
// current_step_id, or completes the run if there is no next step.
func (k *Kernel) advanceTransition(ctx context.Context, run *Run, def *Definition, step Step, state StepState) (*Run, error) {
	next, err := k.resolveNext(ctx, run, def, step, state)
	if err != nil {
		return k.failTransition(ctx, run, step, err)
	}
	if next == "" {
		return k.complete(ctx, run)
	}
	run, err = k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.CurrentStepID = next
		if _, ok := r.Steps[next]; !ok {
			r.Steps[next] = StepState{Status: StepPending}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = k.Store.AppendEvents(ctx, run.ID, []Event{{
		Time: k.now(), Kind: "flow.step.completed", FlowRunID: run.ID, StepID: step.ID,
	}, {
		Time: k.now(), Kind: "flow.step.scheduled", FlowRunID: run.ID, StepID: next,
	}})
	return run, nil
}

func (k *Kernel) failTransition(ctx context.Context, run *Run, step Step, transitionErr error) (*Run, error) {
	run, err := k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.Status = RunFailed
		if r.Steps == nil {
			r.Steps = map[string]StepState{}
		}
		s := r.Steps[step.ID]
		s.Status = StepFailed
		if s.Error == "" {
			s.Error = transitionErr.Error()
		}
		r.Steps[step.ID] = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return run, transitionErr
}

func (k *Kernel) complete(ctx context.Context, run *Run) (*Run, error) {
	run, err := k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.Status = RunCompleted
		r.CurrentStepID = ""
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = k.Store.AppendEvents(ctx, run.ID, []Event{{
		Time: k.now(), Kind: "flow.run.completed", FlowRunID: run.ID,
	}})
	return run, nil
}

func (k *Kernel) resolveDefinitionByID(ctx context.Context, run *Run) (*Definition, error) {
	if def, ok := k.lookupDefinition(run.FlowID); ok {
		return def, nil
	}
	if k.Definitions != nil {
		if def, err := k.Definitions.Definition(run.FlowID); err == nil {
			return def, nil
		}
	}
	// Fall back to the definition persisted alongside the run (supports
	// Advance/Resume in a fresh process that did not see the original path).
	if def, err := k.Store.LoadDefinitionForRun(run.ID); err == nil {
		k.registerDefinition(def)
		return def, nil
	}
	return nil, fmt.Errorf("no definition source available for flow %q", run.FlowID)
}

// Resume is the entry point for resuming a paused flow after a HumanRequest
// is resolved. Implemented in kernel_resume.go.
func (k *Kernel) Resume(ctx context.Context, flowRunID, humanRequestID string) (*Run, error) {
	return k.resume(ctx, flowRunID, humanRequestID)
}

// resolveNext evaluates the step's transitions in order: branches (first match)
// then on_success. Returns "" if the flow should complete. If branches are
// declared but none match, the transition is rejected as unresolvable — v0
// treats branch sets as exhaustive.
func (k *Kernel) resolveNext(ctx context.Context, run *Run, def *Definition, step Step, state StepState) (string, error) {
	if len(step.Transitions.Branches) > 0 {
		matched := ""
		for _, br := range step.Transitions.Branches {
			match, err := k.evalWhen(ctx, run, def, state, br.When)
			if err != nil {
				return "", fmt.Errorf("branch %q on step %q: %w", br.When, step.ID, err)
			}
			if match {
				matched = br.Next
				break
			}
		}
		if matched == "" {
			return "", fmt.Errorf("%w: step %q has branches but none matched", ErrTransitionUnresolvable, step.ID)
		}
		return matched, nil
	}
	if step.Transitions.OnSuccess != "" {
		return step.Transitions.OnSuccess, nil
	}
	return "", nil
}

// evalWhen evaluates the tiny v0 expression forms. Supported:
//   - ${outputs.<step>.<slot> == 'value'}
//   - ${outputs.<step>.<slot> != 'value'}
//   - ${outputs.<step>.<slot>.<sub> == 'value'}
//   - ${outputs.<step>.<slot> > 0}  /  == 0
//   - ${runtime.policy.<key> == true}  /  != true
func (k *Kernel) evalWhen(ctx context.Context, run *Run, def *Definition, state StepState, expr string) (bool, error) {
	expr = strings.TrimSpace(expr)
	inner, ok := stripTemplate(expr)
	if !ok {
		return false, fmt.Errorf("unsupported expression %q (expected ${...})", expr)
	}
	inner = strings.TrimSpace(inner)
	switch {
	case strings.HasPrefix(inner, "outputs."):
		return k.evalOutputExpr(ctx, run, inner)
	case strings.HasPrefix(inner, "runtime.policy."):
		return k.evalPolicyExpr(ctx, run, inner)
	default:
		return false, fmt.Errorf("unsupported expression form %q", inner)
	}
}

func (k *Kernel) evalOutputExpr(ctx context.Context, run *Run, inner string) (bool, error) {
	left, op, right, err := splitComparison(inner)
	if err != nil {
		return false, err
	}
	actual, err := resolveOutputPath(run, strings.TrimPrefix(left, "outputs."))
	if err != nil {
		return false, err
	}
	return compare(actual, op, right)
}

func (k *Kernel) evalPolicyExpr(ctx context.Context, run *Run, inner string) (bool, error) {
	left, op, right, err := splitComparison(inner)
	if err != nil {
		return false, err
	}
	key := strings.TrimPrefix(left, "runtime.policy.")
	if k.Policy == nil {
		// Default: undefined policy is false.
		return compare(false, op, right)
	}
	value, ok := k.Policy.PolicyValue(ctx, run, key)
	if !ok {
		value = false
	}
	return compare(value, op, right)
}

func stripTemplate(expr string) (string, bool) {
	if !strings.HasPrefix(expr, "${") || !strings.HasSuffix(expr, "}") {
		return "", false
	}
	return expr[2 : len(expr)-1], true
}

func splitComparison(inner string) (left, op, right string, err error) {
	for _, cand := range []string{"==", "!=", ">=", "<=", ">", "<"} {
		if idx := strings.Index(inner, cand); idx >= 0 {
			return strings.TrimSpace(inner[:idx]), cand, strings.TrimSpace(inner[idx+len(cand):]), nil
		}
	}
	return "", "", "", fmt.Errorf("unsupported expression %q (no comparison operator)", inner)
}

func resolveOutputPath(run *Run, path string) (any, error) {
	parts := strings.Split(path, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("output path must be <step>.<slot>")
	}
	stepID := parts[0]
	step, ok := run.Steps[stepID]
	if !ok {
		return nil, fmt.Errorf("step %q has no output", stepID)
	}
	slot, ok := step.Outputs[parts[1]]
	if !ok {
		return nil, fmt.Errorf("step %q slot %q is not set", stepID, parts[1])
	}
	value := slot.Value
	// Support nested map navigation for ${outputs.s.slot.sub} style.
	if len(parts) > 2 {
		current := value
		for _, key := range parts[2:] {
			m, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("step %q slot %q is not a map", stepID, parts[1])
			}
			current, ok = m[key]
			if !ok {
				return nil, fmt.Errorf("step %q slot %q missing key %q", stepID, parts[1], key)
			}
		}
		value = current
	} else if value == nil {
		// Fall back to summary, then artifact path string for comparisons like
		// == 'path'. Agent steps with one required slot often store plain text
		// in Summary.
		value = slot.Summary
		if value == "" {
			value = slot.Artifact
		}
	}
	return value, nil
}

func compare(actual any, op, right string) (bool, error) {
	right = strings.TrimSpace(right)
	switch op {
	case "==":
		return equalsValue(actual, right), nil
	case "!=":
		return !equalsValue(actual, right), nil
	case ">", "<", ">=", "<=":
		return compareNumeric(actual, op, right)
	default:
		return false, fmt.Errorf("unsupported operator %q", op)
	}
}

func equalsValue(actual any, right string) bool {
	right = strings.Trim(strings.TrimSpace(right), `'"`)
	switch v := actual.(type) {
	case string:
		return v == right
	case bool:
		b, err := strconv.ParseBool(right)
		if err != nil {
			return false
		}
		return v == b
	case int:
		n, err := strconv.Atoi(right)
		if err != nil {
			return false
		}
		return v == n
	case int64:
		n, err := strconv.ParseInt(right, 10, 64)
		if err != nil {
			return false
		}
		return v == n
	case float64:
		n, err := strconv.ParseFloat(right, 64)
		if err != nil {
			return false
		}
		return v == n
	case nil:
		return right == "" || right == "null"
	default:
		return fmt.Sprint(v) == right
	}
}

func compareNumeric(actual any, op, right string) (bool, error) {
	actualNum, err := toFloat(actual)
	if err != nil {
		return false, fmt.Errorf("left side is not numeric: %v", actual)
	}
	rightNum, err := strconv.ParseFloat(strings.TrimSpace(right), 64)
	if err != nil {
		return false, fmt.Errorf("right side is not numeric: %q", right)
	}
	switch op {
	case ">":
		return actualNum > rightNum, nil
	case "<":
		return actualNum < rightNum, nil
	case ">=":
		return actualNum >= rightNum, nil
	case "<=":
		return actualNum <= rightNum, nil
	default:
		return false, fmt.Errorf("unsupported numeric operator %q", op)
	}
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	default:
		return strconv.ParseFloat(fmt.Sprint(v), 64)
	}
}

func cloneOutputs(in map[string]OutputRef) map[string]OutputRef {
	if in == nil {
		return nil
	}
	out := make(map[string]OutputRef, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func appendUniqueStrings(dst []string, values ...string) []string {
	seen := map[string]struct{}{}
	for _, v := range dst {
		seen[v] = struct{}{}
	}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		dst = append(dst, v)
	}
	return dst
}

// ErrTransitionUnresolvable is returned when a completed step has branches but
// none match and no on_success fallback is defined.
var ErrTransitionUnresolvable = errors.New("transition unresolvable")
