package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentRunner is the runtime surface the agent executor calls. It is satisfied
// by runtime.Service; the flow package depends on this interface rather than
// the concrete runtime package to keep the dependency direction clean (runtime
// depends on flow in M6, not the other way around).
type AgentRunner interface {
	RunAgent(ctx context.Context, req AgentTurnRequest) (AgentTurnResponse, error)
}

// HumanRequestCreator creates a HumanRequest for flow explicit approvals. It
// is satisfied by runtime.Service.CreateHumanRequest; kept as an interface so
// the flow package does not import runtime. Returns the created request id.
type HumanRequestCreator interface {
	CreateHumanRequest(ctx context.Context, input CreateHumanRequestInput) (HumanRequestView, error)
}

// CreateHumanRequestInput is the flow-side projection of a HumanRequest
// create request.
type CreateHumanRequestInput struct {
	WorkspaceID string
	RunID       string
	AgentID     string
	SessionID   string
	ToolCallID  string
	Source      string
	Kind        string // "approval" or "freeform"
	Question    string
	Options     []string
	DedupeKey   string
	Metadata    map[string]string
}

// HumanRequestView is the flow-side projection of a created HumanRequest.
type HumanRequestView struct {
	ID       string
	Source   string
	Kind     string
	Status   string
	Question string
}

// AgentTurnRequest is the flow-side projection of runtime.TurnRequest that
// the agent executor builds from a step.
type AgentTurnRequest struct {
	AgentID            string                         `json:"agent_id"`
	EntrypointID       string                         `json:"entrypoint_id,omitempty"`
	Message            string                         `json:"message"`
	AllowedToolsSet    bool                           `json:"allowed_tools_set,omitempty"`
	AllowedTools       []string                       `json:"allowed_tools,omitempty"`
	ToolInputAllowlist map[string]map[string][]string `json:"tool_input_allowlist,omitempty"`
	UserID             string                         `json:"user_id,omitempty"`
	SessionID          string                         `json:"session_id,omitempty"`
	Channel            string                         `json:"channel,omitempty"`
	Metadata           map[string]string              `json:"metadata,omitempty"`
}

// AgentTurnResponse is the flow-side projection of runtime.TurnResponse. Only
// the fields Flow v0 consumes are projected.
type AgentTurnResponse struct {
	RunID         string                  `json:"run_id"`
	AgentID       string                  `json:"agent_id"`
	EntrypointID  string                  `json:"entrypoint_id,omitempty"`
	SessionID     string                  `json:"session_id"`
	Status        string                  `json:"status"`
	FinalResponse string                  `json:"final_response"`
	StartedAt     time.Time               `json:"started_at"`
	EndedAt       time.Time               `json:"ended_at"`
	HumanRequests []AgentHumanRequestView `json:"human_requests,omitempty"`
	Interrupt     *AgentInterruptView     `json:"interrupt,omitempty"`
	Artifacts     []string                `json:"artifacts,omitempty"`
}

// AgentHumanRequestView is the flow-side projection of a HumanRequest surfaced
// by an agent run.
type AgentHumanRequestView struct {
	ID     string
	Source string
	Kind   string
	Status string
}

// AgentInterruptView is the flow-side projection of a RunInterrupt.
type AgentInterruptView struct {
	Status    string
	Reason    string
	BlockedBy []AgentBlockedByView
}

// AgentBlockedByView projects runtime.BlockedBy.
type AgentBlockedByView struct {
	Type           string
	HumanRequestID string
	RunID          string
	Reason         string
}

// AgentExecutor implements StepExecutor by delegating work steps to an
// AgentRunner and control steps (human_approval/decision) locally.
type AgentExecutor struct {
	Agent     AgentRunner
	Human     HumanRequestCreator
	Workspace string
}

// AgentStatusWaitingHuman mirrors runtime.StatusWaitingHuman. Kept as a local
// constant so the flow package does not import the runtime package.
const AgentStatusWaitingHuman = "waiting_human"

// ExecuteStep dispatches on the step executor form. Work steps go through the
// agent runner; control steps (decision, human_approval) are handled locally.
func (e *AgentExecutor) ExecuteStep(ctx context.Context, run *Run, def *Definition, step Step) (StepExecutionResult, error) {
	if e == nil {
		return StepExecutionResult{}, fmt.Errorf("executor is required")
	}
	switch {
	case strings.TrimSpace(step.Executor.Agent) != "":
		return e.executeAgentStep(ctx, run, def, step)
	case step.Executor.Type == "decision":
		return e.executeDecisionStep(ctx, run, def, step)
	case step.Executor.Type == "human_approval":
		return e.executeHumanApprovalStep(ctx, run, def, step)
	case step.Executor.Type == "wait_signal":
		return StepExecutionResult{Status: StepWaitingHuman, Interrupt: map[string]any{"reason": "wait_signal", "signal": step.Executor.Signal}}, nil
	case step.Executor.Type == "subflow":
		return StepExecutionResult{Status: StepFailed, Error: "subflow executor is deferred in flow v0"}, nil
	default:
		return StepExecutionResult{Status: StepFailed, Error: fmt.Sprintf("unsupported executor form on step %q", step.ID)}, nil
	}
}

func (e *AgentExecutor) executeAgentStep(ctx context.Context, run *Run, def *Definition, step Step) (StepExecutionResult, error) {
	if e.Agent == nil {
		return StepExecutionResult{Status: StepFailed, Error: "agent runner is not configured"}, nil
	}
	req, err := e.buildTurnRequest(run, step)
	if err != nil {
		return StepExecutionResult{Status: StepFailed, Error: err.Error()}, nil
	}
	resp, err := e.Agent.RunAgent(ctx, req)
	if err != nil {
		return StepExecutionResult{Status: StepFailed, Error: err.Error()}, nil
	}
	return mapAgentResponse(step, resp), nil
}

// buildTurnRequest assembles an AgentTurnRequest from flow input, the step's
// objective/instructions/constraints/required_skills, and resolved input slots.
// It deliberately does not propagate the flow entrypoint id into the agent
// turn request: the flow itself is the entry context, and the runtime's own
// entrypoint registry (loaded from xira.yaml) is independent of flow
// entrypoints. Leaving EntrypointID empty lets runtime.RunAgent fall back to
// the default agent resolution.
func (e *AgentExecutor) buildTurnRequest(run *Run, step Step) (AgentTurnRequest, error) {
	resolved, err := ResolveStepInputs(run, step)
	if err != nil {
		return AgentTurnRequest{}, fmt.Errorf("resolve step %q inputs: %w", step.ID, err)
	}
	message := buildAgentMessage(step, run.Input, resolved)
	req := AgentTurnRequest{
		AgentID:            step.Executor.Agent,
		Message:            message,
		AllowedToolsSet:    step.Executor.toolsConfigured,
		AllowedTools:       append([]string(nil), step.Executor.Tools...),
		ToolInputAllowlist: cloneToolInputAllowlist(step.Executor.ToolInputAllowlist),
		Channel:            "flow",
		Metadata: map[string]string{
			"flow_run_id":  run.ID,
			"flow_id":      run.FlowID,
			"flow_step_id": step.ID,
		},
	}
	return req, nil
}

func cloneToolInputAllowlist(in map[string]map[string][]string) map[string]map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string][]string, len(in))
	for tool, fields := range in {
		if len(fields) == 0 {
			out[tool] = nil
			continue
		}
		out[tool] = make(map[string][]string, len(fields))
		for field, values := range fields {
			out[tool][field] = append([]string(nil), values...)
		}
	}
	return out
}

// buildAgentMessage composes the user-visible message handed to the agent from
// the step objective, instructions, constraints, and resolved inputs.
func buildAgentMessage(step Step, flowInput, resolvedInputs map[string]string) string {
	var b strings.Builder
	if strings.TrimSpace(step.Objective) != "" {
		fmt.Fprintf(&b, "Objective: %s\n", step.Objective)
	}
	if len(step.Instructions) > 0 {
		b.WriteString("Instructions:\n")
		for _, ins := range step.Instructions {
			fmt.Fprintf(&b, "- %s\n", ins)
		}
	}
	if len(step.Constraints) > 0 {
		b.WriteString("Constraints:\n")
		for _, c := range step.Constraints {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if len(step.RequiredSkills) > 0 {
		fmt.Fprintf(&b, "Required skills: %s\n", strings.Join(step.RequiredSkills, ", "))
	}
	if len(flowInput) > 0 {
		b.WriteString("Flow input:\n")
		writeKeyValues(&b, flowInput)
	}
	if len(resolvedInputs) > 0 {
		b.WriteString("Step inputs:\n")
		writeKeyValues(&b, resolvedInputs)
	}
	if len(step.OutputContract.RequiredSlots) > 0 {
		b.WriteString("Required output slots: ")
		slotIDs := make([]string, 0, len(step.OutputContract.RequiredSlots))
		for _, slot := range step.OutputContract.RequiredSlots {
			slotIDs = append(slotIDs, slot.ID)
		}
		b.WriteString(strings.Join(slotIDs, ", "))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeKeyValues(b *strings.Builder, m map[string]string) {
	for k, v := range m {
		fmt.Fprintf(b, "- %s: %s\n", k, v)
	}
}

// mapAgentResponse converts an agent's TurnResponse into a step execution
// result, including output slot extraction and required-slot enforcement.
func mapAgentResponse(step Step, resp AgentTurnResponse) StepExecutionResult {
	hrIDs := make([]string, 0, len(resp.HumanRequests))
	for _, hr := range resp.HumanRequests {
		hrIDs = append(hrIDs, hr.ID)
	}

	result := StepExecutionResult{
		AgentRunID:      resp.RunID,
		HumanRequestIDs: hrIDs,
		Artifacts:       artifactRefsFromResponse(resp),
	}
	if resp.Interrupt != nil {
		result.Interrupt = map[string]any{
			"status":     resp.Interrupt.Status,
			"reason":     resp.Interrupt.Reason,
			"blocked_by": resp.Interrupt.BlockedBy,
		}
	}

	switch strings.TrimSpace(resp.Status) {
	case AgentStatusWaitingHuman:
		result.Status = StepWaitingHuman
		return result
	case "failed":
		result.Status = StepFailed
		result.Error = strings.TrimSpace(resp.FinalResponse)
		if result.Error == "" {
			result.Error = "agent run failed"
		}
		return result
	case "completed", "passed", "":
		// fall through to completed-with-output-extraction
	default:
		result.Status = StepFailed
		result.Error = fmt.Sprintf("unsupported agent status %q", resp.Status)
		return result
	}

	// Completed: extract outputs and enforce required slots.
	outputs := extractOutputSlots(step, resp)
	missing := findMissingRequiredSlots(step, outputs)
	if len(missing) > 0 {
		result.Status = StepFailed
		result.Error = fmt.Sprintf("agent completed but required output slots missing: %s", strings.Join(missing, ", "))
		return result
	}
	result.Status = StepCompleted
	result.Outputs = outputs
	return result
}

func artifactRefsFromResponse(resp AgentTurnResponse) []ArtifactRef {
	seen := map[string]struct{}{}
	out := make([]ArtifactRef, 0, len(resp.Artifacts))
	for _, path := range resp.Artifacts {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, ArtifactRef{Path: path})
	}
	return out
}

// extractOutputSlots maps the agent response onto declared slots. v0 strategy:
//   - Parse the first fenced ```yaml or ```json block in FinalResponse as a
//     structured payload; any keys matching declared slot ids map directly.
//   - Otherwise: if exactly one required slot is declared, put the full
//     FinalResponse into that slot's Summary (conservative fallback).
//   - Otherwise: leave outputs empty (required-slot enforcement will fail).
func extractOutputSlots(step Step, resp AgentTurnResponse) map[string]OutputRef {
	declared := map[string]bool{}
	for _, slot := range step.OutputContract.RequiredSlots {
		declared[slot.ID] = true
	}
	for _, slot := range step.OutputContract.OptionalSlots {
		declared[slot.ID] = true
	}
	outputs := map[string]OutputRef{}

	if structured := extractStructuredBlock(resp.FinalResponse); structured != nil {
		for key, val := range structured {
			if !declared[key] {
				continue
			}
			outputs[key] = OutputRef{Value: val}
		}
	}

	// Attach artifacts referenced by the agent response to matching slots if a
	// structured payload referenced a path; v0 keeps this simple.
	for _, ref := range artifactRefsFromResponse(resp) {
		for slotID := range declared {
			if strings.Contains(ref.Path, slotID) {
				existing := outputs[slotID]
				existing.Artifacts = append(existing.Artifacts, ref.Path)
				outputs[slotID] = existing
			}
		}
	}

	// Conservative summary fallback for a single required slot.
	if len(outputs) == 0 && len(step.OutputContract.RequiredSlots) == 1 {
		slot := step.OutputContract.RequiredSlots[0]
		outputs[slot.ID] = OutputRef{Summary: strings.TrimSpace(resp.FinalResponse)}
	}
	return outputs
}

var (
	fencedJSONRe = regexp.MustCompile("(?s)```(?:json|JSON)?\\s*(\\{.*?\\})\\s*```")
	fencedYAMLRe = regexp.MustCompile("(?s)```(?:yaml|YAML)?\\s*([\\s\\S]*?)```")
)

// extractStructuredBlock parses the first fenced yaml/json object block in the
// final response. Returns nil if none parse.
func extractStructuredBlock(finalResponse string) map[string]any {
	finalResponse = strings.TrimSpace(finalResponse)
	if finalResponse == "" {
		return nil
	}
	if m := fencedJSONRe.FindStringSubmatch(finalResponse); len(m) >= 2 {
		var out map[string]any
		if err := json.Unmarshal([]byte(m[1]), &out); err == nil {
			return out
		}
	}
	if m := fencedYAMLRe.FindStringSubmatch(finalResponse); len(m) >= 2 {
		var out map[string]any
		if err := yaml.Unmarshal([]byte(m[1]), &out); err == nil {
			return out
		}
	}
	// Look for a leading bare JSON object (no fence).
	if strings.HasPrefix(finalResponse, "{") {
		var out map[string]any
		if err := json.Unmarshal([]byte(finalResponse), &out); err == nil {
			return out
		}
	}
	return nil
}

func findMissingRequiredSlots(step Step, outputs map[string]OutputRef) []string {
	var missing []string
	for _, slot := range step.OutputContract.RequiredSlots {
		ref, ok := outputs[slot.ID]
		if !ok {
			missing = append(missing, slot.ID)
			continue
		}
		if ref.Artifact == "" && len(ref.Artifacts) == 0 && ref.Value == nil && strings.TrimSpace(ref.Summary) == "" {
			missing = append(missing, slot.ID)
		}
	}
	return missing
}

// executeDecisionStep treats the decision as driven entirely by transitions:
// it produces no work and marks the step completed with no outputs. The kernel
// evaluates branches against the run's existing outputs.
func (e *AgentExecutor) executeDecisionStep(ctx context.Context, run *Run, def *Definition, step Step) (StepExecutionResult, error) {
	return StepExecutionResult{Status: StepCompleted}, nil
}

// executeHumanApprovalStep is wired in executor_approval.go (M5). The stub
// here returns a clear error so M4 tests can drive the agent path first.
func (e *AgentExecutor) executeHumanApprovalStep(ctx context.Context, run *Run, def *Definition, step Step) (StepExecutionResult, error) {
	return e.doHumanApproval(ctx, run, def, step)
}

// ResolveStepInputs resolves a step's declared input slot references (e.g.
// "${outputs.design.implementation_plan}") against prior step states and flow
// input. Bare "${input.<key>}" references resolve against run.Input.
func ResolveStepInputs(run *Run, step Step) (map[string]string, error) {
	if run == nil {
		return nil, fmt.Errorf("run is required")
	}
	resolved := make(map[string]string, len(step.Inputs))
	for key, expr := range step.Inputs {
		val, err := resolveInputExpression(run, expr)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", key, err)
		}
		resolved[key] = val
	}
	return resolved, nil
}

func resolveInputExpression(run *Run, expr string) (string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", nil
	}
	// Handle the optional "|| default" form used in DevRun (e.g.
	// "${input.request || outputs.select_issue.selected_issue}"). The || lives
	// inside the ${...}, so strip the template first, then split on ||, then
	// resolve each alternative.
	if inner, ok := stripTemplate(expr); ok {
		alternatives := strings.Split(inner, "||")
		var lastErr error
		for _, alt := range alternatives {
			val, err := resolveSingleInner(run, strings.TrimSpace(alt))
			if err == nil && val != "" {
				return val, nil
			}
			if err != nil {
				lastErr = err
			}
		}
		if lastErr != nil {
			return "", lastErr
		}
		return "", nil
	}
	// Bare literal outside a template.
	return expr, nil
}

// resolveSingleInner resolves a single inner expression (no surrounding ${}).
func resolveSingleInner(run *Run, inner string) (string, error) {
	inner = strings.TrimSpace(inner)
	switch {
	case isQuotedLiteral(inner):
		return strings.Trim(inner, `'"`), nil
	case strings.HasPrefix(inner, "input."):
		key := strings.TrimPrefix(inner, "input.")
		val, ok := run.Input[key]
		if !ok {
			return "", nil
		}
		return val, nil
	case strings.HasPrefix(inner, "outputs."):
		val, err := resolveOutputPath(run, strings.TrimPrefix(inner, "outputs."))
		if err != nil {
			return "", err
		}
		return stringifyAny(val), nil
	default:
		return "", fmt.Errorf("unsupported input expression %q", inner)
	}
}

func isQuotedLiteral(value string) bool {
	value = strings.TrimSpace(value)
	return len(value) >= 2 && ((strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) || (strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\"")))
}

func stringifyAny(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		out, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(out)
	}
}
