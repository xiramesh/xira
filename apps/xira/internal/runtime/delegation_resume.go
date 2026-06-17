package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/xiramesh/xira/internal/humanrequest"
	rtools "github.com/xiramesh/xira/internal/tools"
)

func (s *Service) ResumeRunAfterHumanResponse(ctx context.Context, requestID string) error {
	if s == nil {
		return nil
	}
	req, err := s.GetHumanRequest(ctx, requestID)
	if err != nil {
		return err
	}
	if req.Response == nil {
		return nil
	}
	join, callIndex, err := s.findDelegationJoinByHumanRequest(req.ID)
	if err != nil || join == nil || callIndex < 0 {
		if err != nil {
			return err
		}
		if req.Source == "agent_request" {
			return s.resumeDirectHumanRequest(ctx, req)
		}
		return nil
	}
	switch req.Response.Kind {
	case humanrequest.ResponseDeny:
		return s.materializeDeniedDelegation(ctx, join, callIndex, req, "failed")
	case humanrequest.ResponseCancel:
		return s.materializeDeniedDelegation(ctx, join, callIndex, req, "canceled")
	default:
		return s.resumeChildDelegationAfterAnswer(ctx, join, callIndex, req)
	}
}

func (s *Service) resumeChildDelegationAfterAnswer(ctx context.Context, join *DelegationJoinState, callIndex int, req *humanrequest.HumanRequest) error {
	call := join.Calls[callIndex]
	if isTerminalDelegateCallStatus(call.Status) {
		return nil
	}
	childRun, err := s.runs.Load(call.ChildRunID)
	if err != nil {
		return err
	}
	target, ok := s.agents.Get(call.ChildAgentID)
	if !ok {
		return fmt.Errorf("child agent profile %q not found", call.ChildAgentID)
	}
	target = delegateWorkerProfile(target)
	resumeMessage := childResumeMessage(childRun.Message, req)
	childReq := TurnRequest{
		AgentID:   target.ID,
		Message:   resumeMessage,
		SessionID: adkSessionID(childRun.SessionID, childRun.RunID+":resume:"+uuid.NewString()),
		// Resume inherits the child's original trigger identity from its
		// persisted session scope — not a forged "resume" channel — so the
		// resumed turn stays in the same conversation tree.
		Context: inboundContextFromScope(childRun.SessionScope, map[string]string{
			"conversation_session_id": childRun.SessionID,
			"agent_session_id":        childRun.SessionID,
			"human_request_id":        req.ID,
		}),
	}
	base := runtimeEventBase{
		RunID:                 childRun.RunID,
		AgentID:               target.ID,
		ConversationSessionID: childRun.SessionID,
		AgentSessionID:        childRun.SessionID,
		TraceID:               join.ParentRunID,
	}
	var events []RuntimeEvent
	var audits []AuditEvent
	var llmCalls []LLMCallRecord
	recordEvent := func(kind, source, message string, payload map[string]any) {
		evt := newRuntimeEvent(base, kind, source, message, payload, &runtimeEventCorrelationInput{
			ParentRunID: join.ParentRunID,
			ChildRunID:  childRun.RunID,
			ToolCallID:  call.ParentToolCallID,
		})
		events = append(events, evt)
		s.events.Publish(evt)
	}
	recordAudit := func(action, target string, allowed bool, reason string, meta map[string]any) {
		audits = append(audits, AuditEvent{
			ID:      uuid.NewString(),
			RunID:   childRun.RunID,
			Time:    time.Now(),
			Action:  action,
			Actor:   responseActor(req.Response),
			Target:  target,
			Allowed: allowed,
			Reason:  reason,
			Meta:    meta,
		})
	}
	resumeCtx := contextWithToolFailureGuard(ctx)
	resumeCtx = contextWithToolTrace(resumeCtx, childRun.RunID)
	resumeCtx = contextWithRuntimeSuspendCollector(resumeCtx, newRuntimeSuspendCollector())
	resumeCtx = contextWithRunExecution(resumeCtx, runExecutionContext{
		Base:        base,
		Profile:     target,
		Request:     childReq,
		UserMessage: resumeMessage,
	})
	resumeCtx = rtools.WithRunDir(resumeCtx, s.runs.RunDir(childRun.RunID))
	resumeCtx = s.withLLMInstrumentation(resumeCtx, llmInstrumentationInput{
		RunID:          childRun.RunID,
		AgentID:        target.ID,
		SessionID:      childRun.SessionID,
		AgentSessionID: childRun.SessionID,
		ADKSessionID:   childReq.SessionID,
		UserID:         childReq.Context.SenderID,
		Pricing:        s.pricing,
	}, recordEvent, func(call LLMCallRecord) {
		llmCalls = append(llmCalls, call)
	})
	instruction, _, err := s.instructionTextForRun(target)
	if err != nil {
		return err
	}
	final, toolCalls, err := s.generate(resumeCtx, target, instruction, childReq, recordEvent, recordAudit)
	if err != nil {
		return err
	}
	result, err := validateDelegateAgentResult(final, call.ChildAgentID, childRun.RunID, nil, map[string]struct{}{})
	if err != nil {
		return err
	}
	childRun.Message = resumeMessage
	childRun.FinalResponse = final
	childRun.ToolCalls = append(childRun.ToolCalls, toolCalls...)
	childRun.Events = append(childRun.Events, events...)
	childRun.AuditEvents = append(childRun.AuditEvents, audits...)
	childRun.LLMCalls = append(childRun.LLMCalls, llmCalls...)
	childRun.VerificationResult = VerificationResult{Status: "passed", Checks: []string{"delegate_result_v1"}}
	childRun.EndedAt = time.Now()
	childRun.Status = "completed"
	childRun.Usage = summarizeUsage(childRun)
	if err := s.runs.SaveRun(childRun); err != nil {
		return err
	}
	return s.materializeCompletedDelegation(ctx, join, callIndex, result, responseActor(req.Response))
}

func (s *Service) materializeCompletedDelegation(ctx context.Context, join *DelegationJoinState, callIndex int, result DelegateAgentResult, actor string) error {
	output := delegateResultOutput(result)
	outputRef, err := s.writeDelegationOutput(join.ParentRunID, join.Calls[callIndex].ParentToolCallID, output)
	if err != nil {
		return err
	}
	now := time.Now()
	join.Calls[callIndex].Status = result.Status
	join.Calls[callIndex].OutputRef = outputRef
	join.Status = joinStatusFromCalls(join.Calls)
	join.UpdatedAt = now
	if err := s.saveDelegationJoinState(join); err != nil {
		return err
	}
	if err := s.materializeParentDelegateToolCall(join.ParentRunID, join.Calls[callIndex].ParentToolCallID, output, ""); err != nil {
		return err
	}
	return s.resumeParentAfterDelegationOutput(ctx, join, output, actor)
}

func (s *Service) materializeDeniedDelegation(ctx context.Context, join *DelegationJoinState, callIndex int, req *humanrequest.HumanRequest, status string) error {
	call := join.Calls[callIndex]
	if isTerminalDelegateCallStatus(call.Status) {
		return nil
	}
	output := map[string]any{
		"agent_id":         call.ChildAgentID,
		"run_id":           call.ChildRunID,
		"status":           status,
		"human_request_id": req.ID,
		"error":            strings.TrimSpace(req.Response.Message),
	}
	outputRef, err := s.writeDelegationOutput(join.ParentRunID, call.ParentToolCallID, output)
	if err != nil {
		return err
	}
	now := time.Now()
	join.Calls[callIndex].Status = status
	join.Calls[callIndex].OutputRef = outputRef
	join.Calls[callIndex].Error = strings.TrimSpace(req.Response.Message)
	join.Status = joinStatusFromCalls(join.Calls)
	join.UpdatedAt = now
	if err := s.saveDelegationJoinState(join); err != nil {
		return err
	}
	if childRun, err := s.runs.Load(call.ChildRunID); err == nil {
		childRun.Status = "failed"
		if childRun.Metadata == nil {
			childRun.Metadata = map[string]string{}
		}
		if status == "canceled" {
			childRun.Metadata["error_type"] = "canceled"
		}
		childRun.EndedAt = now
		childRun.VerificationResult = VerificationResult{Status: "failed", Checks: []string{"human_response_" + string(req.Response.Kind)}}
		if err := s.runs.SaveRun(childRun); err != nil {
			return err
		}
	}
	if err := s.materializeParentDelegateToolCall(join.ParentRunID, call.ParentToolCallID, output, anyString(output["error"])); err != nil {
		return err
	}
	return s.resumeParentAfterDelegationOutput(ctx, join, output, responseActor(req.Response))
}

func (s *Service) writeDelegationOutput(parentRunID, parentToolCallID string, output map[string]any) (string, error) {
	rel := filepath.ToSlash(filepath.Join("delegations", safeToolOutputFileName(parentToolCallID)+".output.json"))
	if err := writeJSONFile(filepath.Join(s.runs.RunDir(parentRunID), filepath.FromSlash(rel)), output); err != nil {
		return "", err
	}
	return rel, nil
}

func (s *Service) materializeParentDelegateToolCall(parentRunID, parentToolCallID string, output map[string]any, errText string) error {
	parent, err := s.runs.Load(parentRunID)
	if err != nil {
		return err
	}
	now := time.Now()
	replaced := false
	for i := range parent.ToolCalls {
		if parent.ToolCalls[i].ID != parentToolCallID {
			continue
		}
		parent.ToolCalls[i].Name = delegateAgentToolName
		parent.ToolCalls[i].Output = output
		parent.ToolCalls[i].Error = strings.TrimSpace(errText)
		parent.ToolCalls[i].EndedAt = now
		replaced = true
		break
	}
	if !replaced {
		parent.ToolCalls = append(parent.ToolCalls, ToolCallRecord{
			ID:        parentToolCallID,
			RunID:     parentRunID,
			Name:      delegateAgentToolName,
			Output:    output,
			Error:     strings.TrimSpace(errText),
			StartedAt: now,
			EndedAt:   now,
		})
	}
	return s.runs.SaveRun(parent)
}

func (s *Service) resumeParentAfterDelegationOutput(ctx context.Context, join *DelegationJoinState, output map[string]any, actor string) error {
	if join == nil {
		return nil
	}
	parent, err := s.runs.Load(join.ParentRunID)
	if err != nil {
		return err
	}
	if parent.Status != StatusWaitingHuman {
		return nil
	}
	profile, ok := s.agents.Get(parent.AgentID)
	if !ok {
		return nil
	}
	outputJSON, _ := json.Marshal(output)
	resumeMessage := parent.Message + "\n\ndelegate_agent output: " + string(outputJSON) + "\n\nThe delegated child run has already finished and the delegate_agent tool output has already been materialized. Do not call delegate_agent for the same task again; produce the final answer from this delegate output."
	resumeReq := TurnRequest{
		AgentID:   profile.ID,
		Message:   resumeMessage,
		SessionID: adkSessionID(parent.SessionID, parent.RunID+":delegate-resume:"+uuid.NewString()),
		// Parent resume inherits the parent's original trigger identity from its
		// persisted session scope, not a forged "resume" channel.
		Context: inboundContextFromScope(parent.SessionScope, map[string]string{
			"conversation_session_id": parent.SessionID,
			"agent_session_id":        parent.SessionID,
			"delegation_join_id":      join.ID,
		}),
	}
	if resumeReq.Context.SenderID == "" {
		resumeReq.Context.SenderID = "human"
	}
	base := runtimeEventBase{
		RunID:                 parent.RunID,
		AgentID:               parent.AgentID,
		EntrypointID:          parent.EntrypointID,
		Channel:               resumeReq.Context.Channel,
		ConversationSessionID: parent.SessionID,
		AgentSessionID:        parent.SessionID,
		TraceID:               parent.RunID,
	}
	var events []RuntimeEvent
	var audits []AuditEvent
	var llmCalls []LLMCallRecord
	recordEvent := func(kind, source, message string, payload map[string]any) {
		evt := newRuntimeEvent(base, kind, source, message, payload, nil)
		events = append(events, evt)
		s.events.Publish(evt)
	}
	recordAudit := func(action, target string, allowed bool, reason string, meta map[string]any) {
		audits = append(audits, AuditEvent{
			ID:      uuid.NewString(),
			RunID:   parent.RunID,
			Time:    time.Now(),
			Action:  action,
			Actor:   resumeReq.Context.SenderID,
			Target:  target,
			Allowed: allowed,
			Reason:  reason,
			Meta:    meta,
		})
	}
	resumeCtx := contextWithToolFailureGuard(ctx)
	resumeCtx = contextWithToolTrace(resumeCtx, parent.RunID)
	suspendCollector := newRuntimeSuspendCollector()
	resumeCtx = contextWithRuntimeSuspendCollector(resumeCtx, suspendCollector)
	resumeCtx = contextWithRunExecution(resumeCtx, runExecutionContext{
		Base:        base,
		Profile:     profile,
		Request:     resumeReq,
		UserMessage: resumeMessage,
	})
	resumeCtx = rtools.WithRunDir(resumeCtx, s.runs.RunDir(parent.RunID))
	resumeCtx = s.withLLMInstrumentation(resumeCtx, llmInstrumentationInput{
		RunID:          parent.RunID,
		AgentID:        profile.ID,
		EntrypointID:   parent.EntrypointID,
		Channel:        "resume",
		SessionID:      parent.SessionID,
		AgentSessionID: parent.SessionID,
		ADKSessionID:   resumeReq.SessionID,
		UserID:         resumeReq.Context.SenderID,
		Pricing:        s.pricing,
	}, recordEvent, func(call LLMCallRecord) {
		llmCalls = append(llmCalls, call)
	})
	instruction, _, err := s.instructionTextForRun(profile)
	if err != nil {
		return err
	}
	final, toolCalls, err := s.generate(resumeCtx, profile, instruction, resumeReq, recordEvent, recordAudit)
	if err != nil {
		return err
	}
	parent.Message = resumeMessage
	parent.FinalResponse = final
	parent.ToolCalls = append(parent.ToolCalls, toolCalls...)
	parent.Events = append(parent.Events, events...)
	parent.AuditEvents = append(parent.AuditEvents, audits...)
	parent.LLMCalls = append(parent.LLMCalls, llmCalls...)
	if interrupt := suspendCollector.Interrupt(); interrupt != nil {
		parent.Interrupt = interrupt
		parent.HumanRequests = append(parent.HumanRequests, interrupt.HumanRequests...)
		parent.VerificationResult = VerificationResult{Status: StatusWaitingHuman, Checks: []string{"runtime_interrupt"}}
		parent.Status = StatusWaitingHuman
	} else {
		parent.Interrupt = nil
		parent.VerificationResult = s.verifier.Verify(final, profile.Verification.DefaultChecks)
		parent.Status = "completed"
		if parent.VerificationResult.Status != "passed" {
			parent.Status = "failed"
		}
	}
	parent.EndedAt = time.Now()
	parent.Usage = summarizeUsage(parent)
	return s.runs.SaveRun(parent)
}

func childResumeMessage(original string, req *humanrequest.HumanRequest) string {
	message := "Human response received."
	if req != nil && req.Response != nil {
		switch req.Response.Kind {
		case humanrequest.ResponseApprove:
			message = "Human approved the request."
		case humanrequest.ResponseAnswer:
			message = "Human response: " + strings.TrimSpace(req.Response.Message)
		default:
			message = "Human response: " + string(req.Response.Kind) + " " + strings.TrimSpace(req.Response.Message)
		}
	}
	return strings.TrimSpace(original) + "\n\n" + message
}

func responseActor(response *humanrequest.HumanResponse) string {
	if response == nil || strings.TrimSpace(response.Actor) == "" {
		return "human"
	}
	return strings.TrimSpace(response.Actor)
}

func joinStatusFromCalls(calls []DelegationJoinCall) string {
	for _, call := range calls {
		if !isTerminalDelegateCallStatus(call.Status) {
			return StatusWaitingHuman
		}
	}
	return "completed"
}
