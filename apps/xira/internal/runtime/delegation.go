package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
	rtools "github.com/xiramesh/xira/internal/tools"
)

const (
	statusToolName         = "emit_status"
	delegateAgentToolName  = "delegate_agent"
	delegateTaskSchemaV1   = "delegate_task_v1"
	delegateResultSchemaV1 = "delegate_result_v1"
)

var delegateForbiddenInputFields = []string{
	"account",
	"agent_session_id",
	"channel",
	"child_agent_id",
	"child_run_id",
	"conversation_session_id",
	"correlation",
	"entrypoint_id",
	"metadata",
	"message_id",
	"parent_run_id",
	"policy",
	"provenance",
	"run_id",
	"scope",
	"sender_id",
	"trace_id",
}

var delegateAllowedInputFields = map[string]struct{}{
	"agent_id":               {},
	"task":                   {},
	"context_refs":           {},
	"expected_output_schema": {},
	"max_duration_ms":        {},
}

type delegateAgentInput struct {
	AgentID              string
	Task                 string
	ContextRefs          []string
	ExpectedOutputSchema string
	MaxDurationMS        int
}

type childAgentRequest struct {
	ParentBase  runtimeEventBase
	ParentRunID string
	ChildRunID  string
	ToolCallID  string
	Target      agents.Profile
	Message     string
	SessionMode string
	Depth       int
}

type delegateContextPacket struct {
	ID        string                `json:"id"`
	Mode      string                `json:"mode"`
	CreatedAt time.Time             `json:"created_at"`
	Caller    map[string]string     `json:"caller"`
	Target    map[string]any        `json:"target"`
	Task      map[string]any        `json:"task"`
	Items     []delegateContextItem `json:"items"`
	Truncated bool                  `json:"truncated"`
}

type delegateContextItem struct {
	ID                 string `json:"id"`
	Kind               string `json:"kind"`
	Source             string `json:"source"`
	SourceRef          string `json:"source_ref,omitempty"`
	SourceRunID        string `json:"source_run_id,omitempty"`
	SourceAgentID      string `json:"source_agent_id,omitempty"`
	SourceToolCallID   string `json:"source_tool_call_id,omitempty"`
	SourceArtifactPath string `json:"source_artifact_path,omitempty"`
	ArtifactRef        string `json:"artifact_ref,omitempty"`
	OwnerAgent         string `json:"owner_agent"`
	Visibility         string `json:"visibility"`
	ContentPreview     string `json:"content_preview"`
	ContentHash        string `json:"content_hash"`
	IncludedChars      int    `json:"included_chars"`
	Redacted           bool   `json:"redacted"`
	Truncated          bool   `json:"truncated"`
	Ref                string `json:"ref"`
}

type resolvedContextArtifact struct {
	SourceRef        string
	SourceRunID      string
	SourceToolCallID string
	RelPath          string
	AbsPath          string
}

type modelDelegateResult struct {
	AgentID        string   `json:"agent_id"`
	RunID          string   `json:"run_id"`
	Status         string   `json:"status"`
	Summary        string   `json:"summary"`
	EvidenceRefs   []string `json:"evidence_refs"`
	Limitations    []string `json:"limitations"`
	Confidence     string   `json:"confidence"`
	FollowupNeeded bool     `json:"followup_needed"`
	Error          string   `json:"error"`
}

type delegateResultValidationError struct {
	Reason string
	Err    error
}

func (e delegateResultValidationError) Error() string {
	if e.Err == nil {
		return "invalid_child_result"
	}
	return "invalid_child_result: " + e.Reason + ": " + e.Err.Error()
}

func (e delegateResultValidationError) Unwrap() error {
	return e.Err
}

func (s *Service) runtimeADKTools(
	ctx context.Context,
	profile agents.Profile,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
	recordTool func(ToolCallRecord),
) ([]adktool.Tool, error) {
	var out []adktool.Tool
	humanRequestTool, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name:         "human.request",
		Description:  "Pause the current agent run and ask a human for freeform input or approval.",
		InputSchema:  humanRequestToolInputSchema(),
		OutputSchema: objectSchema(),
	}, func(toolCtx adktool.Context, args map[string]any) (map[string]any, error) {
		callID := strings.TrimSpace(toolCtx.FunctionCallID())
		req, err := s.createAgentHumanRequest(ctx, callID, args)
		if err != nil {
			return map[string]any{"status": "rejected", "error": err.Error()}, nil
		}
		recordEvent("human.request.created", "runtime", "human request created", map[string]any{
			"human_request_id": req.ID,
			"kind":             req.Kind,
			"source":           req.Source,
			"tool_call_id":     callID,
		})
		recordAudit("human.request", req.ID, true, "agent requested human input", map[string]any{
			"kind":         req.Kind,
			"tool_call_id": callID,
		})
		return map[string]any{
			"status":           StatusWaitingHuman,
			"human_request_id": req.ID,
		}, nil
	})
	if err != nil {
		return nil, err
	}
	out = append(out, humanRequestTool)

	statusTool, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name:         statusToolName,
		Description:  "Emit a user-readable progress status event. The message is an event only and is not final answer content.",
		InputSchema:  statusToolInputSchema(),
		OutputSchema: objectSchema(),
	}, func(_ adktool.Context, args map[string]any) (map[string]any, error) {
		message := strings.TrimSpace(fmt.Sprint(args["message"]))
		if message == "" {
			return map[string]any{"status": "rejected", "error": "message is required"}, nil
		}
		recordEvent("assistant.status", "runtime", message, map[string]any{
			"producer": "runtime.status_tool",
		})
		return map[string]any{"status": "ok"}, nil
	})
	if err != nil {
		return nil, err
	}
	out = append(out, statusTool)

	if !profile.NormalizedDelegationPolicy().Enabled {
		return out, nil
	}
	delegateTool, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name:         delegateAgentToolName,
		Description:  "Delegate a bounded subtask to a runtime-owned worker agent. Inputs are limited to agent_id, task, context_refs, expected_output_schema, and max_duration_ms.",
		InputSchema:  delegateAgentInputSchema(),
		OutputSchema: objectSchema(),
	}, func(toolCtx adktool.Context, args map[string]any) (map[string]any, error) {
		start := time.Now()
		callID := strings.TrimSpace(toolCtx.FunctionCallID())
		if callID == "" {
			callID = uuid.NewString()
		}
		input, cleanInput, forbidden, unsupported := sanitizeDelegateInput(args)
		rec := ToolCallRecord{
			ID:        callID,
			Name:      delegateAgentToolName,
			Input:     cleanInput,
			StartedAt: start,
		}
		if len(forbidden) > 0 {
			rec.Input["rejected_input_fields"] = forbidden
		}
		if len(unsupported) > 0 {
			rec.Input["unsupported_input_fields"] = unsupported
		}
		output, runErr := s.executeDelegateAgentTool(ctx, profile, input, forbidden, unsupported, callID, recordEvent, recordAudit)
		rec.Output = output
		rec.Error = errString(runErr)
		rec.EndedAt = time.Now()
		recordTool(rec)
		return output, nil
	})
	if err != nil {
		return nil, err
	}
	out = append(out, delegateTool)
	return out, nil
}

func objectSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object"}
}

func statusToolInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"message": {Type: "string"},
		},
		Required:             []string{"message"},
		AdditionalProperties: rejectAllSchema(),
	}
}

func humanRequestToolInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"kind":     {Type: "string", Enum: []any{string(humanrequest.RequestFreeform), string(humanrequest.RequestApproval)}},
			"question": {Type: "string"},
			"options": {
				Type: "array",
				Items: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"id":    {Type: "string"},
						"label": {Type: "string"},
					},
					Required:             []string{"id", "label"},
					AdditionalProperties: rejectAllSchema(),
				},
			},
		},
		Required:             []string{"question"},
		AdditionalProperties: rejectAllSchema(),
	}
}

func delegateAgentInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"agent_id":               {Type: "string"},
			"task":                   {Type: "string"},
			"context_refs":           {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
			"expected_output_schema": {Type: "string", Enum: []any{delegateResultSchemaV1}},
			"max_duration_ms":        {Type: "integer"},
		},
		Required:             []string{"agent_id", "task"},
		AdditionalProperties: rejectAllSchema(),
	}
}

func rejectAllSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
}

func sanitizeDelegateInput(args map[string]any) (delegateAgentInput, map[string]any, []string, []string) {
	input := delegateAgentInput{
		AgentID:              stringArg(args, "agent_id"),
		Task:                 stringArg(args, "task"),
		ContextRefs:          stringSliceFromAny(args["context_refs"]),
		ExpectedOutputSchema: stringArg(args, "expected_output_schema"),
	}
	if input.ExpectedOutputSchema == "" {
		input.ExpectedOutputSchema = delegateResultSchemaV1
	}
	if n, ok := intFromAny(args["max_duration_ms"]); ok {
		input.MaxDurationMS = n
	}
	clean := map[string]any{
		"agent_id":               input.AgentID,
		"task":                   input.Task,
		"context_refs":           input.ContextRefs,
		"expected_output_schema": input.ExpectedOutputSchema,
	}
	if input.MaxDurationMS > 0 {
		clean["max_duration_ms"] = input.MaxDurationMS
	}
	var forbidden []string
	for _, field := range delegateForbiddenInputFields {
		if _, ok := args[field]; ok {
			forbidden = append(forbidden, field)
		}
	}
	sort.Strings(forbidden)
	var unsupported []string
	for field := range args {
		if _, ok := delegateAllowedInputFields[field]; ok {
			continue
		}
		unsupported = append(unsupported, field)
	}
	sort.Strings(unsupported)
	return input, clean, forbidden, unsupported
}

func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, ok := args[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func stringSliceFromAny(value any) []string {
	switch v := value.(type) {
	case []string:
		out := append([]string(nil), v...)
		sort.Strings(out)
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				out = append(out, text)
			}
		}
		sort.Strings(out)
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{strings.TrimSpace(v)}
	default:
		return nil
	}
}

func (s *Service) executeDelegateAgentTool(
	ctx context.Context,
	caller agents.Profile,
	input delegateAgentInput,
	forbiddenFields []string,
	unsupportedFields []string,
	toolCallID string,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) (map[string]any, error) {
	exec, ok := runExecutionFromContext(ctx)
	if !ok {
		err := errors.New("delegate_agent requires runtime execution context")
		return delegateErrorOutput(input.AgentID, "", "rejected", err), err
	}
	childRunID := "pending-child-" + shortID()
	correlationPayload := delegateCorrelationPayload(exec.Base.RunID, childRunID, toolCallID, caller.ID, input.AgentID)
	if input.AgentID == "" {
		err := errors.New("agent_id is required")
		recordEvent("agent.delegate.rejected", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{"reason": err.Error()}))
		recordAudit("agent.delegate", input.AgentID, false, err.Error(), nil)
		return delegateErrorOutput(input.AgentID, childRunID, "rejected", err), err
	}
	if input.Task == "" {
		err := errors.New("task is required")
		recordEvent("agent.delegate.rejected", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{"reason": err.Error()}))
		recordAudit("agent.delegate", input.AgentID, false, err.Error(), nil)
		return delegateErrorOutput(input.AgentID, childRunID, "rejected", err), err
	}
	target, exists := s.agents.Get(input.AgentID)
	if !exists {
		err := fmt.Errorf("target agent %q is not enabled", input.AgentID)
		recordEvent("agent.delegate.rejected", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{"reason": err.Error()}))
		recordCapabilityGap(recordEvent, correlationPayload, "agent.delegate", input.AgentID, err.Error())
		recordAudit("agent.delegate", input.AgentID, false, err.Error(), nil)
		return delegateErrorOutput(input.AgentID, childRunID, "rejected", err), err
	}
	childRunID = NewRunID(target.ID, time.Now()) + "-" + shortID()
	correlationPayload = delegateCorrelationPayload(exec.Base.RunID, childRunID, toolCallID, caller.ID, target.ID)
	workerTarget := delegateWorkerProfile(target)
	recordEvent("agent.delegate.requested", "runtime", "agent delegation requested", mergeAnyMaps(correlationPayload, map[string]any{
		"task_preview":           previewText(input.Task, 160),
		"expected_output_schema": input.ExpectedOutputSchema,
	}))
	if len(forbiddenFields) > 0 {
		err := fmt.Errorf("delegate request contains runtime-owned field(s): %s", strings.Join(forbiddenFields, ", "))
		recordEvent("agent.delegate.rejected", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{
			"reason":                err.Error(),
			"rejected_input_fields": forbiddenFields,
			"rejected_before_child": true,
		}))
		recordAudit("agent.delegate", input.AgentID, false, err.Error(), map[string]any{"rejected_input_fields": forbiddenFields})
		return delegateErrorOutput(input.AgentID, childRunID, "rejected", err), err
	}
	if len(unsupportedFields) > 0 {
		err := fmt.Errorf("delegate request contains unsupported field(s): %s", strings.Join(unsupportedFields, ", "))
		recordEvent("agent.delegate.rejected", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{
			"reason":                   err.Error(),
			"unsupported_input_fields": unsupportedFields,
			"rejected_before_child":    true,
		}))
		recordAudit("agent.delegate", input.AgentID, false, err.Error(), map[string]any{"unsupported_input_fields": unsupportedFields})
		return delegateErrorOutput(input.AgentID, childRunID, "rejected", err), err
	}
	if input.ExpectedOutputSchema != delegateResultSchemaV1 {
		err := fmt.Errorf("unsupported expected_output_schema %q; only %s is supported in Phase 1", input.ExpectedOutputSchema, delegateResultSchemaV1)
		recordEvent("agent.delegate.rejected", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{
			"reason":                 err.Error(),
			"expected_output_schema": input.ExpectedOutputSchema,
			"supported_schema":       delegateResultSchemaV1,
			"rejected_before_child":  true,
		}))
		recordAudit("agent.delegate", input.AgentID, false, err.Error(), map[string]any{"expected_output_schema": input.ExpectedOutputSchema})
		return delegateErrorOutput(input.AgentID, childRunID, "rejected", err), err
	}
	policy := caller.NormalizedDelegationPolicy()
	if !policy.Enabled || !policy.Allows(target.ID) {
		err := fmt.Errorf("caller agent %q is not allowed to delegate to %q", caller.ID, target.ID)
		recordEvent("agent.delegate.rejected", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{
			"reason":          err.Error(),
			"policy_enabled":  policy.Enabled,
			"policy_allow":    policy.Allow,
			"target_agent_id": target.ID,
		}))
		recordCapabilityGap(recordEvent, correlationPayload, "agent.delegate", target.ID, err.Error())
		recordAudit("agent.delegate", target.ID, false, err.Error(), nil)
		return delegateErrorOutput(target.ID, childRunID, "rejected", err), err
	}
	requestedDepth := exec.Base.DelegationDepth + 1
	if requestedDepth > policy.MaxDepth {
		err := fmt.Errorf("delegation depth %d exceeds max_depth %d", requestedDepth, policy.MaxDepth)
		recordEvent("agent.delegate.rejected", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{"reason": err.Error()}))
		recordAudit("agent.delegate", target.ID, false, err.Error(), nil)
		return delegateErrorOutput(target.ID, childRunID, "rejected", err), err
	}
	outstandingBefore, err := s.outstandingChildCount(exec.Base.RunID)
	if err != nil {
		recordEvent("agent.delegate.failed", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{"error": err.Error()}))
		return delegateErrorOutput(target.ID, childRunID, "failed", err), err
	}
	if outstandingBefore >= policy.MaxOutstanding {
		err := fmt.Errorf("outstanding child count %d exceeds max_outstanding %d", outstandingBefore, policy.MaxOutstanding)
		recordEvent("agent.delegate.rejected", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{
			"reason":                         err.Error(),
			"outstanding_child_count_before": outstandingBefore,
			"max_outstanding":                policy.MaxOutstanding,
		}))
		recordAudit("agent.delegate", target.ID, false, err.Error(), nil)
		return delegateErrorOutput(target.ID, childRunID, "rejected", err), err
	}
	effectiveMaxDurationMS := policy.DefaultMaxDurationMS
	if input.MaxDurationMS > 0 {
		effectiveMaxDurationMS = input.MaxDurationMS
		if effectiveMaxDurationMS > policy.MaxDurationMS {
			effectiveMaxDurationMS = policy.MaxDurationMS
		}
	}
	activeBefore, reserved := s.reserveChildSlot(exec.Base.RunID, policy.MaxParallel)
	if !reserved {
		err := fmt.Errorf("active child count %d exceeds max_parallel %d", activeBefore, policy.MaxParallel)
		recordEvent("agent.delegate.rejected", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{
			"reason":                    err.Error(),
			"active_child_count_before": activeBefore,
			"max_parallel":              policy.MaxParallel,
		}))
		recordAudit("agent.delegate", target.ID, false, err.Error(), nil)
		return delegateErrorOutput(target.ID, childRunID, "rejected", err), err
	}
	defer s.releaseChildSlot(exec.Base.RunID)

	recordEvent("agent.delegate.allowed", "runtime", "agent delegation allowed", mergeAnyMaps(correlationPayload, map[string]any{
		"target_agent_id":             target.ID,
		"target_profile_version":      target.Version,
		"target_allowed_tools":        s.toolRegistry(workerTarget).List(),
		"requested_child_depth":       requestedDepth,
		"active_child_count_before":   activeBefore,
		"max_parallel":                policy.MaxParallel,
		"requested_max_duration_ms":   input.MaxDurationMS,
		"effective_max_duration_ms":   effectiveMaxDurationMS,
		"policy_max_duration_ms":      policy.MaxDurationMS,
		"child_session_mode":          policy.ChildSessionMode,
		"expose_child_output_to_user": policy.ExposeChildOutputToUser,
	}))
	recordAudit("agent.delegate", target.ID, true, "delegation allowed by caller profile", map[string]any{
		"caller_agent_id": caller.ID,
		"child_run_id":    childRunID,
	})
	if err := s.runs.InitRun(childRunID); err != nil {
		recordEvent("agent.delegate.failed", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{"error": err.Error()}))
		return delegateErrorOutput(target.ID, childRunID, "failed", err), err
	}
	packet, packetRefs, err := s.buildDelegateContextPacket(exec, workerTarget, childRunID, input, correlationPayload, recordEvent)
	if err != nil {
		recordEvent("context.packet.failed", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{"error": err.Error()}))
		recordEvent("agent.delegate.failed", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{"error": err.Error()}))
		return delegateErrorOutput(target.ID, childRunID, "failed", err), err
	}
	recordEvent("agent.delegate.started", "runtime", "child agent run started", mergeAnyMaps(correlationPayload, map[string]any{
		"context_packet_id": packet.ID,
		"target_agent_id":   target.ID,
	}))
	childCtx, cancel := context.WithTimeout(ctx, time.Duration(effectiveMaxDurationMS)*time.Millisecond)
	defer cancel()
	childResp, childErr := s.RunChildAgent(childCtx, childAgentRequest{
		ParentBase:  exec.Base,
		ParentRunID: exec.Base.RunID,
		ChildRunID:  childRunID,
		ToolCallID:  toolCallID,
		Target:      workerTarget,
		Message:     delegateWorkerPrompt(input, packet),
		SessionMode: policy.ChildSessionMode,
		Depth:       requestedDepth,
	})
	if childErr != nil {
		kind := "agent.delegate.failed"
		status := "failed"
		if errors.Is(childCtx.Err(), context.DeadlineExceeded) {
			kind = "agent.delegate.timeout"
			status = "timeout"
		}
		recordEvent(kind, "runtime", childErr.Error(), mergeAnyMaps(correlationPayload, map[string]any{
			"status": status,
			"error":  childErr.Error(),
		}))
		return delegateErrorOutput(target.ID, childRunID, status, childErr), childErr
	}
	if childResp.Status == StatusWaitingHuman {
		join, err := s.createWaitingDelegationJoinState(exec.Base.RunID, caller.ID, toolCallID, childRunID, target.ID, childResp.HumanRequests)
		if err != nil {
			recordEvent("agent.delegate.failed", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{
				"status": "failed",
				"error":  err.Error(),
				"reason": "delegation_join_persist_failed",
			}))
			return delegateErrorOutput(target.ID, childRunID, "failed", err), err
		}
		if collector := runtimeSuspendCollectorFromContext(ctx); collector != nil {
			collector.AddDelegationJoin(join.ID)
			for _, req := range childResp.HumanRequests {
				collector.AddChildHumanRequest(req, exec.Base.RunID, toolCallID)
			}
		}
		recordEvent("agent.delegate.waiting_human", "runtime", "child agent run waiting for human input", mergeAnyMaps(correlationPayload, map[string]any{
			"status":               StatusWaitingHuman,
			"child_human_requests": len(childResp.HumanRequests),
			"delegation_join_id":   join.ID,
		}))
		return map[string]any{
			"agent_id":           target.ID,
			"run_id":             childRunID,
			"status":             StatusWaitingHuman,
			"human_requests":     len(childResp.HumanRequests),
			"delegation_join_id": join.ID,
		}, nil
	}
	allowedEvidenceRefs := s.allowedDelegateEvidenceRefs(childRunID, workerTarget, packetRefs, childResp)
	result, err := validateDelegateAgentResult(childResp.FinalResponse, target.ID, childRunID, packetRefs, allowedEvidenceRefs)
	if err != nil {
		validation := delegateResultValidation(err)
		rawPath := s.persistRejectedDelegateResult(childRunID, childResp.FinalResponse, validation)
		recordEvent("agent.delegate.failed", "runtime", err.Error(), mergeAnyMaps(correlationPayload, map[string]any{
			"status":                "failed",
			"error":                 "invalid_child_result",
			"reason":                validation.Reason,
			"raw_child_result_path": rawPath,
		}))
		return delegateInvalidResultOutput(target.ID, childRunID, validation, rawPath), err
	}
	recordEvent("agent.delegate.completed", "runtime", "child agent run completed", mergeAnyMaps(correlationPayload, map[string]any{
		"status":             result.Status,
		"summary_preview":    previewText(result.Summary, 160),
		"evidence_ref_count": len(result.EvidenceRefs),
		"limitations_count":  len(result.Limitations),
		"confidence":         result.Confidence,
		"followup_needed":    result.FollowupNeeded,
	}))
	recordEvent("agent.delegate.result_delivered", "runtime", "delegate result delivered to caller", mergeAnyMaps(correlationPayload, map[string]any{
		"status":             result.Status,
		"result_schema":      delegateResultSchemaV1,
		"evidence_ref_count": len(result.EvidenceRefs),
	}))
	return delegateResultOutput(result), nil
}

func (s *Service) buildDelegateContextPacket(
	exec runExecutionContext,
	target agents.Profile,
	childRunID string,
	input delegateAgentInput,
	correlationPayload map[string]any,
	recordEvent func(kind, source, message string, payload map[string]any),
) (delegateContextPacket, []string, error) {
	targetInstruction, _, err := s.instructionTextForRun(target)
	if err != nil {
		return delegateContextPacket{}, nil, err
	}
	targetTools := s.toolRegistry(target).List()
	packet := delegateContextPacket{
		ID:        "ctxpkt_" + shortID(),
		Mode:      "delegate_worker",
		CreatedAt: time.Now(),
		Caller: map[string]string{
			"agent_id":                exec.Profile.ID,
			"run_id":                  exec.Base.RunID,
			"conversation_session_id": exec.Base.ConversationSessionID,
			"agent_session_id":        exec.Base.AgentSessionID,
		},
		Target: map[string]any{
			"agent_id":                 target.ID,
			"profile_version":          target.Version,
			"profile_instruction_hash": instructionHash(targetInstruction),
			"profile_instruction_ref":  "profile://" + target.ID + "/instructions",
			"allowed_tools":            targetTools,
			"allowed_tools_hash":       instructionHash(strings.Join(targetTools, "\n")),
			"run_id":                   childRunID,
			"session_mode":             "ephemeral_worker",
			"delegation_depth":         exec.Base.DelegationDepth + 1,
			"input_schema":             delegateTaskSchemaV1,
			"output_schema":            delegateResultSchemaV1,
		},
		Task: map[string]any{
			"worker_task":            input.Task,
			"expected_output_schema": input.ExpectedOutputSchema,
		},
	}
	recordEvent("context.packet.started", "runtime", "context packet started", mergeAnyMaps(correlationPayload, map[string]any{
		"context_packet_id": packet.ID,
	}))
	refs := map[string]struct{}{}
	for _, ref := range input.ContextRefs {
		refs[ref] = struct{}{}
	}
	includeCurrentUser := len(refs) == 0
	for _, ref := range []string{"conversation://current-turn", "conversation://current-turn/user-message"} {
		if _, ok := refs[ref]; ok {
			includeCurrentUser = true
		}
	}
	var canonicalRefs []string
	if includeCurrentUser {
		item, err := s.materializeContextItem(childRunID, delegateContextItem{
			ID:            "ctxitem_current_user_message",
			Kind:          "user_message",
			Source:        "conversation://current-turn/user-message",
			SourceRef:     "conversation://current-turn/user-message",
			SourceRunID:   exec.Base.RunID,
			SourceAgentID: exec.Profile.ID,
			OwnerAgent:    exec.Profile.ID,
			Visibility:    "child_only",
		}, exec.UserMessage, 2000)
		if err != nil {
			return packet, nil, err
		}
		packet.Items = append(packet.Items, item)
		packet.Truncated = packet.Truncated || item.Truncated
		canonicalRefs = append(canonicalRefs, item.Ref)
		recordEvent("context.item.included", "runtime", "context item included", mergeAnyMaps(correlationPayload, map[string]any{
			"context_packet_id": packet.ID,
			"context_item_id":   item.ID,
			"context_ref":       item.Ref,
			"source":            item.Source,
			"included_chars":    item.IncludedChars,
			"truncated":         item.Truncated,
		}))
		if item.Truncated {
			recordEvent("context.packet.truncated", "runtime", "context packet truncated", mergeAnyMaps(correlationPayload, map[string]any{
				"context_packet_id": packet.ID,
				"reason":            "context item exceeded max chars",
			}))
		}
	}
	for ref := range refs {
		if ref == "conversation://current-turn" || ref == "conversation://current-turn/user-message" {
			continue
		}
		item, materializedRefs, ok, err := s.materializeParentToolOutputContextRef(exec, childRunID, ref, 2000)
		if err != nil {
			return packet, nil, err
		}
		if ok {
			packet.Items = append(packet.Items, item)
			packet.Truncated = packet.Truncated || item.Truncated
			canonicalRefs = append(canonicalRefs, materializedRefs...)
			recordEvent("context.item.included", "runtime", "context item included", mergeAnyMaps(correlationPayload, map[string]any{
				"context_packet_id": packet.ID,
				"context_item_id":   item.ID,
				"context_ref":       item.Ref,
				"artifact_ref":      item.ArtifactRef,
				"source":            item.Source,
				"source_ref":        item.SourceRef,
				"included_chars":    item.IncludedChars,
				"truncated":         item.Truncated,
			}))
			if item.Truncated {
				recordEvent("context.packet.truncated", "runtime", "context packet truncated", mergeAnyMaps(correlationPayload, map[string]any{
					"context_packet_id": packet.ID,
					"reason":            "context item exceeded max chars",
				}))
			}
			continue
		}
		recordEvent("context.item.redacted", "runtime", "context ref redacted", mergeAnyMaps(correlationPayload, map[string]any{
			"context_packet_id": packet.ID,
			"source_ref":        ref,
			"reason":            "unsupported or unauthorized context ref",
		}))
	}
	packetPath := filepath.Join(s.runs.RunDir(childRunID), "artifacts", "context", "context_packet.json")
	if err := writeJSONFile(packetPath, packet); err != nil {
		return packet, nil, err
	}
	recordEvent("context.packet.completed", "runtime", "context packet completed", mergeAnyMaps(correlationPayload, map[string]any{
		"context_packet_id": packet.ID,
		"items":             len(packet.Items),
		"truncated":         packet.Truncated,
		"path":              filepath.ToSlash(filepath.Join("artifacts", "context", "context_packet.json")),
	}))
	return packet, canonicalRefs, nil
}

func (s *Service) materializeParentToolOutputContextRef(exec runExecutionContext, childRunID, ref string, maxChars int) (delegateContextItem, []string, bool, error) {
	resolved, ok := s.resolveParentToolOutputContextRef(exec, ref)
	if !ok {
		return delegateContextItem{}, nil, false, nil
	}
	data, err := os.ReadFile(resolved.AbsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return delegateContextItem{}, nil, false, nil
		}
		return delegateContextItem{}, nil, false, err
	}
	raw := map[string]any{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return delegateContextItem{}, nil, false, fmt.Errorf("decode context ref %q: %w", ref, err)
	}
	if resolved.SourceToolCallID == "" {
		resolved.SourceToolCallID = anyString(raw["tool_call_id"])
	}
	if resolved.SourceToolCallID == "" {
		resolved.SourceToolCallID = shortID()
	}
	childRelPath := filepath.ToSlash(filepath.Join("artifacts", "tool-outputs", "context_"+safeToolOutputFileName(resolved.SourceToolCallID)+".json"))
	artifactRef := "artifact://" + childRunID + "/" + childRelPath
	copied := copyAnyMap(raw)
	if copied == nil {
		copied = map[string]any{}
	}
	copied["run_id"] = childRunID
	copied["source_run_id"] = resolved.SourceRunID
	copied["source_ref"] = resolved.SourceRef
	copied["source_artifact_path"] = resolved.RelPath
	copied["source_tool_call_id"] = resolved.SourceToolCallID
	copied["materialized_by"] = "delegate_context_resolver"
	if err := writeJSONFile(filepath.Join(s.runs.RunDir(childRunID), filepath.FromSlash(childRelPath)), copied); err != nil {
		return delegateContextItem{}, nil, false, err
	}
	preview, truncated := toolOutputContextPreview(copied, maxChars)
	item, err := s.materializeContextItem(childRunID, delegateContextItem{
		ID:                 "ctxitem_tool_output_" + safeToolOutputFileName(resolved.SourceToolCallID),
		Kind:               "tool_result",
		Source:             "parent_tool_output",
		SourceRef:          resolved.SourceRef,
		SourceRunID:        resolved.SourceRunID,
		SourceAgentID:      exec.Profile.ID,
		SourceToolCallID:   resolved.SourceToolCallID,
		SourceArtifactPath: resolved.RelPath,
		ArtifactRef:        artifactRef,
		OwnerAgent:         exec.Profile.ID,
		Visibility:         "child_only",
		Truncated:          truncated,
	}, preview, maxChars)
	if err != nil {
		return delegateContextItem{}, nil, false, err
	}
	item.Truncated = item.Truncated || truncated
	return item, []string{item.Ref, artifactRef}, true, nil
}

func (s *Service) resolveParentToolOutputContextRef(exec runExecutionContext, ref string) (resolvedContextArtifact, bool) {
	if s == nil || s.runs == nil {
		return resolvedContextArtifact{}, false
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return resolvedContextArtifact{}, false
	}
	parentRunID := strings.TrimSpace(exec.Base.RunID)
	if parentRunID == "" {
		return resolvedContextArtifact{}, false
	}
	sourceRunID := parentRunID
	relPath := ""
	toolCallID := ""
	switch {
	case strings.HasPrefix(ref, "artifact://"):
		rest := strings.TrimPrefix(ref, "artifact://")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) != parentRunID {
			return resolvedContextArtifact{}, false
		}
		relPath = parts[1]
	case strings.HasPrefix(ref, "tool://"):
		rest := strings.TrimPrefix(ref, "tool://")
		parts := strings.Split(rest, "/")
		if len(parts) != 3 || strings.TrimSpace(parts[0]) != parentRunID || parts[2] != "output" {
			return resolvedContextArtifact{}, false
		}
		toolCallID = strings.TrimSpace(parts[1])
		if toolCallID == "" {
			return resolvedContextArtifact{}, false
		}
		relPath = filepath.ToSlash(filepath.Join("artifacts", "tool-outputs", safeToolOutputFileName(toolCallID)+".json"))
	default:
		relPath = ref
	}
	cleanPath, ok := cleanChildArtifactPath(relPath)
	if !ok {
		return resolvedContextArtifact{}, false
	}
	if cleanPath != "artifacts/tool-outputs" && !strings.HasPrefix(cleanPath, "artifacts/tool-outputs/") {
		return resolvedContextArtifact{}, false
	}
	if path.Ext(cleanPath) != ".json" {
		return resolvedContextArtifact{}, false
	}
	absPath := filepath.Join(s.runs.RunDir(sourceRunID), filepath.FromSlash(cleanPath))
	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return resolvedContextArtifact{}, false
	}
	return resolvedContextArtifact{
		SourceRef:        ref,
		SourceRunID:      sourceRunID,
		SourceToolCallID: toolCallID,
		RelPath:          cleanPath,
		AbsPath:          absPath,
	}, true
}

func toolOutputContextPreview(raw map[string]any, maxChars int) (string, bool) {
	preview := copyAnyMap(raw)
	if preview == nil {
		preview = map[string]any{}
	}
	truncated := false
	for _, stream := range []string{"stdout", "stderr"} {
		text, _ := raw[stream].(string)
		delete(preview, stream)
		if text == "" {
			continue
		}
		limit := maxChars / 2
		if limit <= 0 {
			limit = 1000
		}
		streamPreview := previewText(text, limit)
		preview[stream+"_preview"] = streamPreview
		preview[stream+"_bytes"] = len([]byte(text))
		streamTruncated := streamPreview != text
		preview[stream+"_truncated"] = streamTruncated
		truncated = truncated || streamTruncated
	}
	data, err := json.Marshal(preview)
	if err != nil {
		return "", truncated
	}
	text := string(data)
	if maxChars <= 0 {
		return text, truncated
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text, truncated
	}
	return string(runes[:maxChars]) + "...", true
}

func (s *Service) materializeContextItem(childRunID string, item delegateContextItem, content string, maxChars int) (delegateContextItem, error) {
	runes := []rune(content)
	contentTruncated := maxChars > 0 && len(runes) > maxChars
	if contentTruncated {
		runes = runes[:maxChars]
	}
	item.Truncated = item.Truncated || contentTruncated
	item.ContentPreview = string(runes)
	item.ContentHash = instructionHash(content)
	item.IncludedChars = utf8.RuneCountInString(item.ContentPreview)
	item.Redacted = false
	item.Ref = "context://" + childRunID + "/context/" + item.ID
	relPath := filepath.Join("artifacts", "context", item.ID+".json")
	if err := writeJSONFile(filepath.Join(s.runs.RunDir(childRunID), relPath), item); err != nil {
		return item, err
	}
	return item, nil
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func delegateWorkerPrompt(input delegateAgentInput, packet delegateContextPacket) string {
	packetJSON, _ := json.MarshalIndent(packet, "", "  ")
	return strings.TrimSpace(fmt.Sprintf(`You are running as a Xira delegate worker.

Return only one JSON object matching %s:
{
  "summary": "short evidence-backed summary",
  "evidence_refs": [],
  "limitations": [],
  "confidence": "low|medium|high",
  "followup_needed": false
}

Do not include agent_id, run_id, status, error, metadata, scope, policy, provenance, or correlation fields. Runtime owns those fields.

Task:
%s

ContextPacket:
%s`, delegateResultSchemaV1, input.Task, string(packetJSON)))
}

func (s *Service) RunChildAgent(ctx context.Context, req childAgentRequest) (TurnResponse, error) {
	childBase := req.ParentBase
	childBase.RunID = req.ChildRunID
	childBase.AgentID = req.Target.ID
	childBase.ChildAgentID = ""
	childBase.AgentSessionID = "ephemeral_worker:" + req.ChildRunID
	childBase.DelegationDepth = req.Depth
	childBase.TraceID = req.ParentBase.TraceID
	if childBase.TraceID == "" {
		childBase.TraceID = req.ParentRunID
	}
	correlation := &runtimeEventCorrelationInput{
		ParentRunID: req.ParentRunID,
		ChildRunID:  req.ChildRunID,
		ToolCallID:  req.ToolCallID,
	}
	resp := TurnResponse{
		RunID:        req.ChildRunID,
		AgentID:      req.Target.ID,
		EntrypointID: childBase.EntrypointID,
		SessionID:    childBase.AgentSessionID,
		ModelPolicy:  s.modelPolicySnapshot(req.Target),
		Message:      req.Message,
		Status:       "running",
		StartedAt:    time.Now(),
		Metadata: map[string]string{
			"delegation_mode":    "ephemeral_worker",
			"parent_run_id":      req.ParentRunID,
			"delegate_tool_call": req.ToolCallID,
		},
	}
	recordChildEvent := func(kind, source, message string, payload map[string]any) {
		evt := newRuntimeEvent(childBase, kind, source, message, payload, correlation)
		resp.Events = append(resp.Events, evt)
		dispatchEvent(ctx, s.events, evt)
	}
	recordChildAudit := func(action, target string, allowed bool, reason string, meta map[string]any) {
		resp.AuditEvents = append(resp.AuditEvents, AuditEvent{
			ID:      uuid.NewString(),
			RunID:   req.ChildRunID,
			Time:    time.Now(),
			Action:  action,
			Actor:   req.ParentBase.AgentID,
			Target:  target,
			Allowed: allowed,
			Reason:  reason,
			Meta:    meta,
		})
	}
	recordChildEvent("run.started", "runtime", "child agent run started", map[string]any{
		"agent_id":           req.Target.ID,
		"parent_run_id":      req.ParentRunID,
		"delegation_depth":   req.Depth,
		"child_session_mode": req.SessionMode,
	})
	childReq := TurnRequest{
		AgentID:   req.Target.ID,
		Message:   req.Message,
		SessionID: adkSessionID(childBase.AgentSessionID, req.ChildRunID+":"+uuid.NewString()),
		// Child run inherits the parent's trigger identity (channel/chat/sender/
		// space) so its session lands under the same conversation tree, not a
		// forged orchestration channel. Built from the strong-typed event base.
		Context: channel.NormalizeInboundContext(channel.InboundContext{
			Channel:      childBase.Channel,
			EntrypointID: childBase.EntrypointID,
			Account:      childBase.Account,
			ChannelAppID: childBase.ChannelAppID,
			BotID:        childBase.BotID,
			ChatID:       childBase.ChatID,
			ChatType:     childBase.ChatType,
			TopicID:      childBase.TopicID,
			SpaceID:      childBase.SpaceID,
			SpaceType:    childBase.SpaceType,
			SenderID:     childBase.SenderID,
			MessageID:    childBase.MessageID,
		}),
	}
	childCtx := contextWithToolFailureGuard(ctx)
	childCtx = contextWithToolTrace(childCtx, req.ChildRunID)
	childSuspendCollector := newRuntimeSuspendCollector()
	childCtx = contextWithRuntimeSuspendCollector(childCtx, childSuspendCollector)
	childCtx = contextWithRunExecution(childCtx, runExecutionContext{
		Base:        childBase,
		Profile:     req.Target,
		Request:     childReq,
		UserMessage: req.Message,
	})
	childCtx = rtools.WithRunDir(childCtx, s.runs.RunDir(req.ChildRunID))
	childCtx = s.withLLMInstrumentation(childCtx, llmInstrumentationInput{
		RunID:          req.ChildRunID,
		AgentID:        req.Target.ID,
		EntrypointID:   childBase.EntrypointID,
		Channel:        childBase.Channel,
		SessionID:      childBase.AgentSessionID,
		AgentSessionID: childBase.AgentSessionID,
		ADKSessionID:   childReq.SessionID,
		UserID:         childReq.Context.SenderID,
		Pricing:        s.pricing,
	}, recordChildEvent, func(call LLMCallRecord) {
		resp.LLMCalls = append(resp.LLMCalls, call)
	})
	childInstruction, _, activationErr := s.instructionTextForRun(req.Target)
	var final string
	var toolCalls []ToolCallRecord
	var runErr error
	if activationErr != nil {
		runErr = activationErr
	} else {
		final, toolCalls, runErr = s.generate(childCtx, req.Target, childInstruction, childReq, recordChildEvent, recordChildAudit)
	}
	resp.FinalResponse = final
	resp.ToolCalls = toolCalls
	if interrupt := childSuspendCollector.Interrupt(); interrupt != nil {
		resp.Interrupt = interrupt
		resp.HumanRequests = append([]humanrequest.HumanRequest(nil), interrupt.HumanRequests...)
		resp.VerificationResult = VerificationResult{Status: StatusWaitingHuman, Checks: []string{"runtime_interrupt"}}
		recordChildEvent("run.waiting_human", "runtime", "child agent run waiting for human input", map[string]any{
			"human_requests": len(interrupt.HumanRequests),
			"blocked_by":     interrupt.Reason,
		})
	} else {
		resp.VerificationResult = s.verifier.Verify(final, req.Target.Verification.DefaultChecks)
	}
	resp.EndedAt = time.Now()
	resp.Usage = summarizeUsage(resp)
	resp.Status = "completed"
	if resp.Interrupt != nil {
		resp.Status = StatusWaitingHuman
	} else if runErr != nil || resp.VerificationResult.Status != "passed" {
		resp.Status = "failed"
	}
	if len(resp.LLMCalls) > 0 {
		payload := map[string]any{
			"call_count":        resp.Usage.CallCount,
			"completed_calls":   resp.Usage.CompletedCalls,
			"failed_calls":      resp.Usage.FailedCalls,
			"prompt_tokens":     resp.Usage.PromptTokens,
			"completion_tokens": resp.Usage.CompletionTokens,
			"total_tokens":      resp.Usage.TotalTokens,
			"usage_sources":     resp.Usage.UsageSources,
		}
		if resp.Usage.Cost != nil {
			payload["cost"] = *resp.Usage.Cost
			payload["currency"] = resp.Usage.Currency
		}
		recordChildEvent("llm.usage_summary", "runtime", "child llm usage summarized", payload)
		if s.usage != nil {
			if err := s.usage.AppendCalls(resp.LLMCalls); err != nil {
				recordChildEvent("usage.ledger_failed", "runtime", "child usage ledger append failed", map[string]any{"error": err.Error()})
			} else {
				recordChildEvent("usage.ledger_appended", "runtime", "child usage ledger appended", map[string]any{
					"calls": len(resp.LLMCalls),
					"path":  filepathJoinSlash(s.usage.Root(), "usage-ledger.jsonl"),
				})
			}
		}
	}
	recordChildEvent("run.finished", "runtime", "child agent run finished", map[string]any{
		"status":              resp.Status,
		"verification_status": resp.VerificationResult.Status,
	})
	resp.Artifacts = []string{"artifacts/"}
	saveErr := s.runs.SaveRun(resp)
	if runErr != nil {
		return resp, runErr
	}
	if saveErr != nil {
		return resp, saveErr
	}
	if resp.Status == StatusWaitingHuman {
		return resp, nil
	}
	if resp.Status != "completed" {
		return resp, fmt.Errorf("child run failed verification: %s", resp.VerificationResult.Status)
	}
	return resp, nil
}

func validateDelegateAgentResult(raw, targetAgentID, childRunID string, runtimeEvidenceRefs []string, allowedEvidenceRefs map[string]struct{}) (DelegateAgentResult, error) {
	var modelResult modelDelegateResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &modelResult); err != nil {
		return DelegateAgentResult{}, delegateResultValidationError{Reason: "result_parse_failed", Err: err}
	}
	if strings.TrimSpace(modelResult.Summary) == "" {
		return DelegateAgentResult{}, delegateResultValidationError{Reason: "result_schema_failed", Err: errors.New("summary is required")}
	}
	if modelResult.AgentID != "" && modelResult.AgentID != targetAgentID {
		return DelegateAgentResult{}, delegateResultValidationError{Reason: "result_schema_failed", Err: fmt.Errorf("forged delegate result agent_id %q", modelResult.AgentID)}
	}
	if modelResult.RunID != "" && modelResult.RunID != childRunID {
		return DelegateAgentResult{}, delegateResultValidationError{Reason: "result_schema_failed", Err: fmt.Errorf("forged delegate result run_id %q", modelResult.RunID)}
	}
	if modelResult.Status != "" && modelResult.Status != "completed" {
		return DelegateAgentResult{}, delegateResultValidationError{Reason: "result_schema_failed", Err: fmt.Errorf("forged delegate result status %q", modelResult.Status)}
	}
	if modelResult.Error != "" {
		return DelegateAgentResult{}, delegateResultValidationError{Reason: "result_schema_failed", Err: errors.New("forged delegate result error field")}
	}
	evidenceRefs := append([]string(nil), runtimeEvidenceRefs...)
	for _, ref := range modelResult.EvidenceRefs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if _, ok := allowedEvidenceRefs[ref]; !ok {
			return DelegateAgentResult{}, delegateResultValidationError{Reason: "result_schema_failed", Err: fmt.Errorf("forged delegate result evidence ref %q", ref)}
		}
		evidenceRefs = append(evidenceRefs, ref)
	}
	evidenceRefs = uniqueStrings(evidenceRefs)
	return DelegateAgentResult{
		AgentID:        targetAgentID,
		RunID:          childRunID,
		Status:         "completed",
		Summary:        strings.TrimSpace(modelResult.Summary),
		EvidenceRefs:   evidenceRefs,
		Limitations:    compactStringList(modelResult.Limitations),
		Confidence:     normalizeConfidence(modelResult.Confidence),
		FollowupNeeded: modelResult.FollowupNeeded,
	}, nil
}

func delegateResultOutput(result DelegateAgentResult) map[string]any {
	return map[string]any{
		"agent_id":        result.AgentID,
		"run_id":          result.RunID,
		"status":          result.Status,
		"summary":         result.Summary,
		"evidence_refs":   result.EvidenceRefs,
		"limitations":     result.Limitations,
		"confidence":      result.Confidence,
		"followup_needed": result.FollowupNeeded,
	}
}

func delegateErrorOutput(agentID, runID, status string, err error) map[string]any {
	return map[string]any{
		"agent_id": agentID,
		"run_id":   runID,
		"status":   status,
		"error":    errString(err),
	}
}

func delegateInvalidResultOutput(agentID, runID string, validation delegateResultValidationError, rawPath string) map[string]any {
	return map[string]any{
		"agent_id":              agentID,
		"run_id":                runID,
		"status":                "failed",
		"error":                 "invalid_child_result",
		"reason":                validation.Reason,
		"message":               validation.Error(),
		"raw_child_result_path": rawPath,
	}
}

func delegateResultValidation(err error) delegateResultValidationError {
	var validation delegateResultValidationError
	if errors.As(err, &validation) {
		return validation
	}
	return delegateResultValidationError{Reason: "result_schema_failed", Err: err}
}

func (s *Service) persistRejectedDelegateResult(childRunID, raw string, validation delegateResultValidationError) string {
	if s == nil || s.runs == nil || strings.TrimSpace(childRunID) == "" {
		return ""
	}
	relPath := filepath.ToSlash(filepath.Join("artifacts", "delegate-result", "rejected.json"))
	absPath := filepath.Join(s.runs.RunDir(childRunID), filepath.FromSlash(relPath))
	payload := map[string]any{
		"run_id":         childRunID,
		"error":          "invalid_child_result",
		"reason":         validation.Reason,
		"message":        validation.Error(),
		"raw_final_text": raw,
	}
	if err := writeJSONFile(absPath, payload); err != nil {
		return ""
	}
	return relPath
}

func delegateWorkerProfile(profile agents.Profile) agents.Profile {
	profile.Instructions = append(append([]string(nil), profile.Instructions...), delegateWorkerRuntimeContract())
	return profile
}

func delegateWorkerRuntimeContract() string {
	return strings.TrimSpace(fmt.Sprintf(`You are running as a delegate worker for a bounded parent task.
Return only one JSON object matching %s. Do not include markdown, prose outside JSON, or runtime-owned identity fields.
Runtime owns agent_id, run_id, status, error, scope, policy, provenance, and correlation fields.`, delegateResultSchemaV1))
}

func (s *Service) allowedDelegateEvidenceRefs(childRunID string, target agents.Profile, contextRefs []string, childResp TurnResponse) map[string]struct{} {
	allowed := map[string]struct{}{}
	for _, ref := range contextRefs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if strings.HasPrefix(ref, "context://"+childRunID+"/context/") ||
			strings.HasPrefix(ref, "artifact://"+childRunID+"/artifacts/tool-outputs/") {
			allowed[ref] = struct{}{}
		}
	}
	registry := s.toolRegistry(target)
	for _, rec := range childResp.ToolCalls {
		if rec.RunID != childRunID {
			continue
		}
		if !registry.Has(rec.Name) {
			continue
		}
		if ref := s.childToolArtifactEvidenceRef(childRunID, rec.Output); ref != "" {
			allowed[ref] = struct{}{}
		}
	}
	return allowed
}

func (s *Service) childToolArtifactEvidenceRef(childRunID string, output map[string]any) string {
	if s == nil || s.runs == nil {
		return ""
	}
	rawPath := anyString(output["raw_output_path"])
	if rawPath == "" {
		return ""
	}
	cleanPath, ok := cleanChildArtifactPath(rawPath)
	if !ok {
		return ""
	}
	if cleanPath != "artifacts/tool-outputs" && !strings.HasPrefix(cleanPath, "artifacts/tool-outputs/") {
		return ""
	}
	absPath := filepath.Join(s.runs.RunDir(childRunID), filepath.FromSlash(cleanPath))
	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return ""
	}
	return "artifact://" + childRunID + "/" + cleanPath
}

func cleanChildArtifactPath(value string) (string, bool) {
	value = strings.TrimSpace(filepath.ToSlash(value))
	if value == "" || strings.HasPrefix(value, "/") {
		return "", false
	}
	cleanPath := path.Clean(value)
	if cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return "", false
	}
	return cleanPath, true
}

func delegateCorrelationPayload(parentRunID, childRunID, toolCallID, callerAgentID, targetAgentID string) map[string]any {
	return map[string]any{
		"parent_run_id":   parentRunID,
		"child_run_id":    childRunID,
		"tool_call_id":    toolCallID,
		"caller_agent_id": callerAgentID,
		"child_agent_id":  targetAgentID,
		"target_agent_id": targetAgentID,
	}
}

func mergeAnyMaps(left, right map[string]any) map[string]any {
	out := copyAnyMap(left)
	if out == nil {
		out = map[string]any{}
	}
	for key, value := range right {
		out[key] = value
	}
	return out
}

func recordCapabilityGap(
	recordEvent func(kind, source, message string, payload map[string]any),
	correlationPayload map[string]any,
	neededCapability, attemptedAgent, reason string,
) {
	recordEvent("capability_gap", "runtime", "capability gap recorded", mergeAnyMaps(correlationPayload, map[string]any{
		"needed_capability": neededCapability,
		"reason":            reason,
		"attempted_agents":  []string{attemptedAgent},
		"suggested_action":  "ask_user_or_update_delegation_policy",
	}))
}

func (s *Service) reserveChildSlot(parentRunID string, maxParallel int) (int, bool) {
	s.delegationMu.Lock()
	defer s.delegationMu.Unlock()
	active := s.activeChildren[parentRunID]
	if active >= maxParallel {
		return active, false
	}
	s.activeChildren[parentRunID] = active + 1
	return active, true
}

func (s *Service) activeChildCount(parentRunID string) int {
	s.delegationMu.Lock()
	defer s.delegationMu.Unlock()
	return s.activeChildren[strings.TrimSpace(parentRunID)]
}

func (s *Service) releaseChildSlot(parentRunID string) {
	s.delegationMu.Lock()
	defer s.delegationMu.Unlock()
	active := s.activeChildren[parentRunID]
	if active <= 1 {
		delete(s.activeChildren, parentRunID)
		return
	}
	s.activeChildren[parentRunID] = active - 1
}

func shortID() string {
	return strings.ReplaceAll(uuid.NewString()[:8], "-", "")
}

func compactStringList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text := strings.TrimSpace(value); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func normalizeConfidence(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "medium"
	}
}
