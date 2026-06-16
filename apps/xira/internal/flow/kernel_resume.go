package flow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ResolvedHumanRequest is the flow-side view of a HumanRequest after the user
// has (or has not yet) responded.
type ResolvedHumanRequest struct {
	ID              string
	Source          string
	Kind            string // "approval" or "freeform"
	Status          string // "pending" or "resolved"
	ResponseKind    string // approve / deny / cancel / answer / revise / reject
	ResponseMessage string
}

// IsResolved reports whether the request has a terminal human response.
func (r ResolvedHumanRequest) IsResolved() bool {
	return strings.EqualFold(r.Status, "resolved")
}

// HumanRequestResolver loads a HumanRequest by id. Satisfied by
// runtime.Service.GetHumanRequest (via a thin adapter in M6).
type HumanRequestResolver interface {
	GetHumanRequest(ctx context.Context, requestID string) (ResolvedHumanRequest, error)
}

// Resume proceeds only when the flow run is waiting_human on a step that owns
// the given human request. It:
//   - loads the HumanRequest and rejects if still pending;
//   - for explicit flow_human_approval: maps the response to an
//     approval_signal output slot, marks the step completed, evaluates the
//     transition (approve/revise/cancel/reject map per the step's branches);
//   - for agent_request / runtime_tool_gate: assumes the runtime has already
//     resumed the agent run through ResolveHumanRequest; reloads the step and
//     only proceeds if the agent run has reached a terminal status. WATCH:
//     Flow must not double-resume the agent run.
//
// Resume is idempotent: resolving the same request twice advances the flow at
// most once.
func (k *Kernel) resume(ctx context.Context, flowRunID, humanRequestID string) (*Run, error) {
	if k == nil || k.Store == nil {
		return nil, fmt.Errorf("kernel store is required")
	}
	if strings.TrimSpace(humanRequestID) == "" {
		return nil, fmt.Errorf("human request id is required")
	}
	if err := validateFlowRunID(flowRunID); err != nil {
		return nil, err
	}
	unlock := k.lockRun(flowRunID)
	defer unlock()

	run, err := k.Store.GetRun(ctx, flowRunID)
	if err != nil {
		return nil, err
	}
	if run.Status != RunWaitingHuman {
		// Already advanced (idempotent resume) — return current state.
		return run, nil
	}

	stepID := run.CurrentStepID
	if stepID == "" {
		return nil, fmt.Errorf("flow run %q is waiting_human but has no current step", flowRunID)
	}
	stepState, ok := run.Steps[stepID]
	if !ok {
		return nil, fmt.Errorf("current step %q not found in run state", stepID)
	}
	if !containsString(stepState.HumanRequestIDs, humanRequestID) {
		return nil, fmt.Errorf("human request %q is not linked to current step %q", humanRequestID, stepID)
	}

	if k.Resolver == nil {
		return nil, fmt.Errorf("human request resolver is not configured")
	}
	resolved, err := k.Resolver.GetHumanRequest(ctx, humanRequestID)
	if err != nil {
		return nil, err
	}
	if !resolved.IsResolved() {
		return nil, fmt.Errorf("%w: human request %q is still pending", ErrResumePending, humanRequestID)
	}

	def, err := k.resolveDefinitionByID(ctx, run)
	if err != nil {
		return nil, err
	}
	defStep, ok := def.StepByID(stepID)
	if !ok {
		return nil, fmt.Errorf("step %q not found in flow definition", stepID)
	}

	switch resolved.Source {
	case SourceFlowHumanApproval:
		return k.resumeExplicitApproval(ctx, run, def, defStep, resolved)
	case SourceAgentRequest, SourceRuntimeToolGate:
		return k.resumeAgentGenerated(ctx, run, def, defStep, resolved)
	default:
		// Unknown source: treat as explicit-approval-style mapping so callers
		// with a custom source still progress. Document the assumption.
		return k.resumeExplicitApproval(ctx, run, def, defStep, resolved)
	}
}

// resumeExplicitApproval maps the resolved response onto the step's
// approval_signal slot and continues the transition.
func (k *Kernel) resumeExplicitApproval(ctx context.Context, run *Run, def *Definition, step Step, resolved ResolvedHumanRequest) (*Run, error) {
	signal := mapApprovalResponseToSignal(resolved.ResponseKind, step)
	outputs := map[string]OutputRef{}
	// Prefer the slot declared in the step's output contract; fall back to a
	// conventional "approval_signal" key.
	slotID := "approval_signal"
	if len(step.OutputContract.RequiredSlots) > 0 {
		slotID = step.OutputContract.RequiredSlots[0].ID
	}
	outputs[slotID] = OutputRef{Value: signal}
	now := k.now()
	var nextRun *Run
	run, err := k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
		s := r.Steps[step.ID]
		s.Status = StepCompleted
		s.Outputs = outputs
		completed := now
		s.CompletedAt = &completed
		// Clear pending human requests owned by this step.
		r.PendingHumanRequests = removeStrings(r.PendingHumanRequests, s.HumanRequestIDs...)
		s.HumanRequestIDs = nil
		r.Status = RunRunning
		r.Steps[step.ID] = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	nextRun = run

	// Reject/cancel that maps to a flow-level cancel ends the flow as canceled
	// unless the step has an explicit branch for it.
	if signal == "cancel" && !stepHasBranchFor(step, "cancel") {
		return k.cancel(ctx, run, step, resolved)
	}
	_ = k.Store.AppendEvents(ctx, run.ID, []Event{{
		Time: k.now(), Kind: "flow.step.completed", FlowRunID: run.ID, StepID: step.ID,
		Payload: map[string]any{"approval_signal": signal, "human_request_id": resolved.ID},
	}})

	// Cancel handled by transition branches if present; otherwise advance.
	state := run.Steps[step.ID]
	advanced, advErr := k.advanceTransition(ctx, nextRun, def, step, state)
	if advErr != nil {
		return advanced, advErr
	}
	return advanced, nil
}

// resumeAgentGenerated does not re-execute the agent: the runtime is expected
// to have already resumed the parent agent run via ResolveHumanRequest. Flow
// only reloads the run and, if the step has reached a terminal state,
// evaluates the transition. If the step is still waiting_human (runtime resume
// hasn't completed yet), Flow stays paused.
func (k *Kernel) resumeAgentGenerated(ctx context.Context, run *Run, def *Definition, step Step, resolved ResolvedHumanRequest) (*Run, error) {
	// Deny/cancel on an agent-generated request terminates the step as failed.
	if resolved.ResponseKind == "deny" || resolved.ResponseKind == "cancel" {
		now := k.now()
		run, err := k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
			s := r.Steps[step.ID]
			s.Status = StepFailed
			if resolved.ResponseMessage != "" {
				s.Error = resolved.ResponseMessage
			} else {
				s.Error = "human denied: " + resolved.ResponseKind
			}
			completed := now
			s.CompletedAt = &completed
			r.PendingHumanRequests = removeStrings(r.PendingHumanRequests, s.HumanRequestIDs...)
			s.HumanRequestIDs = nil
			s.Interrupt = nil
			r.Status = RunRunning
			r.Steps[step.ID] = s
			return nil
		})
		if err != nil {
			return nil, err
		}
		return k.markFailed(ctx, run, run.Steps[step.ID], step)
	}

	// For approve/answer: the agent run continues asynchronously in the
	// runtime. Reload the flow run; if the step is no longer waiting_human we
	// advance the transition, otherwise stay paused.
	if k.AgentStatus == nil {
		// Without an agent status hook we cannot tell whether the runtime has
		// finished resuming; stay paused to avoid double-advance.
		return k.Store.GetRun(ctx, run.ID)
	}
	status, err := k.AgentStatus.AgentStepStatus(ctx, run, step)
	if err != nil {
		return nil, err
	}
	switch status {
	case "completed":
		now := k.now()
		run, err = k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
			s := r.Steps[step.ID]
			s.Status = StepCompleted
			completed := now
			s.CompletedAt = &completed
			r.PendingHumanRequests = removeStrings(r.PendingHumanRequests, s.HumanRequestIDs...)
			s.HumanRequestIDs = nil
			s.Interrupt = nil
			r.Status = RunRunning
			r.Steps[step.ID] = s
			return nil
		})
		if err != nil {
			return nil, err
		}
		return k.advanceTransition(ctx, run, def, step, run.Steps[step.ID])
	case "failed":
		now := k.now()
		run, err = k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
			s := r.Steps[step.ID]
			s.Status = StepFailed
			s.Error = "agent run failed after human response"
			completed := now
			s.CompletedAt = &completed
			r.PendingHumanRequests = removeStrings(r.PendingHumanRequests, s.HumanRequestIDs...)
			s.HumanRequestIDs = nil
			s.Interrupt = nil
			r.Status = RunRunning
			r.Steps[step.ID] = s
			return nil
		})
		if err != nil {
			return nil, err
		}
		return k.markFailed(ctx, run, run.Steps[step.ID], step)
	default:
		// Still running or still waiting_human upstream: stay paused.
		return k.Store.GetRun(ctx, run.ID)
	}
}

// cancel marks the flow run as canceled (distinct from failed).
func (k *Kernel) cancel(ctx context.Context, run *Run, step Step, resolved ResolvedHumanRequest) (*Run, error) {
	run, err := k.Store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.Status = RunCanceled
		r.CurrentStepID = ""
		s := r.Steps[step.ID]
		s.Status = StepFailed
		if resolved.ResponseMessage != "" {
			s.Error = resolved.ResponseMessage
		} else {
			s.Error = "flow canceled by human: " + resolved.ResponseKind
		}
		r.PendingHumanRequests = removeStrings(r.PendingHumanRequests, s.HumanRequestIDs...)
		s.HumanRequestIDs = nil
		r.Steps[step.ID] = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = k.Store.AppendEvents(ctx, run.ID, []Event{{
		Time: k.now(), Kind: "flow.run.canceled", FlowRunID: run.ID, StepID: step.ID,
		Payload: map[string]any{"human_request_id": resolved.ID},
	}})
	return run, nil
}

// AgentStatusResolver reports the runtime status of the agent run backing a
// step, used when resuming agent-generated waiting_human. Optional; when nil,
// resume on agent-generated waiting stays paused (safe default).
type AgentStatusResolver interface {
	AgentStepStatus(ctx context.Context, run *Run, step Step) (string, error)
}

// mapApprovalResponseToSignal converts a HumanResponse kind to the value
// written into the step's approval slot. Flow v0 normalizes the option labels
// used in DevRun transitions (approve / revise / cancel / reject / deny).
func mapApprovalResponseToSignal(responseKind string, step Step) string {
	responseKind = strings.TrimSpace(strings.ToLower(responseKind))
	// If the response kind matches a declared option verbatim, use it.
	for _, opt := range step.Executor.Options {
		if strings.EqualFold(opt, responseKind) {
			return strings.ToLower(opt)
		}
	}
	switch responseKind {
	case "approve":
		return "approve"
	case "deny":
		// DevRun uses both deny and reject; normalize to the option set if
		// present, otherwise keep "deny".
		if hasOption(step, "reject") {
			return "reject"
		}
		return "deny"
	case "cancel":
		return "cancel"
	case "answer":
		return "approve"
	default:
		// Allow option labels like "revise" / "reject" that arrive as the
		// response kind.
		return responseKind
	}
}

func hasOption(step Step, opt string) bool {
	for _, o := range step.Executor.Options {
		if strings.EqualFold(o, opt) {
			return true
		}
	}
	return false
}

func stepHasBranchFor(step Step, signal string) bool {
	for _, br := range step.Transitions.Branches {
		if strings.Contains(br.When, "'"+signal+"'") || strings.Contains(br.When, "\""+signal+"\"") {
			return true
		}
	}
	return false
}

func containsString(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func removeStrings(list []string, values ...string) []string {
	if len(list) == 0 {
		return list
	}
	remove := map[string]struct{}{}
	for _, v := range values {
		remove[strings.TrimSpace(v)] = struct{}{}
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		if _, drop := remove[strings.TrimSpace(v)]; drop {
			continue
		}
		out = append(out, v)
	}
	return out
}

// ensure the time import is used if future helpers need it.
var _ = time.Now

// ErrResumePending is returned when Resume is called on a HumanRequest that is
// still pending.
var ErrResumePending = errors.New("human request still pending")
