package runtime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/xiramesh/xira/internal/humanrequest"
	rtools "github.com/xiramesh/xira/internal/tools"
)

func (s *Service) materializeApprovedActionSnapshotOutput(req *humanrequest.HumanRequest, output map[string]any) error {
	if s == nil || s.runs == nil || req == nil || req.ActionSnapshot == nil {
		return nil
	}
	run, err := s.runs.Load(req.ActionSnapshot.RunID)
	if err != nil {
		return err
	}
	now := time.Now()
	materialized := cloneAnyMap(output)
	if materialized == nil {
		materialized = map[string]any{}
	}
	materialized["status"] = "completed"
	materialized["human_request_id"] = req.ID
	replaced := false
	for i := range run.ToolCalls {
		if run.ToolCalls[i].ID != req.ActionSnapshot.ToolCallID {
			continue
		}
		run.ToolCalls[i].Name = req.ActionSnapshot.ToolName
		run.ToolCalls[i].Input = cloneAnyMap(req.ActionSnapshot.Arguments)
		run.ToolCalls[i].Output = materialized
		run.ToolCalls[i].Error = ""
		run.ToolCalls[i].EndedAt = now
		replaced = true
		break
	}
	if !replaced {
		run.ToolCalls = append(run.ToolCalls, ToolCallRecord{
			ID:        req.ActionSnapshot.ToolCallID,
			RunID:     req.ActionSnapshot.RunID,
			Name:      req.ActionSnapshot.ToolName,
			Input:     cloneAnyMap(req.ActionSnapshot.Arguments),
			Output:    materialized,
			StartedAt: now,
			EndedAt:   now,
		})
	}
	replaceRunHumanRequest(&run, *req)
	return s.runs.SaveRun(run)
}

func (s *Service) resumeRunAfterApprovedToolOutput(ctx context.Context, req *humanrequest.HumanRequest, output map[string]any) error {
	if req == nil || req.ActionSnapshot == nil {
		return nil
	}
	run, err := s.runs.Load(req.ActionSnapshot.RunID)
	if err != nil {
		return err
	}
	if run.Status != StatusWaitingHuman {
		return nil
	}
	profile, ok := s.agents.Get(run.AgentID)
	if !ok {
		return nil
	}
	outputJSON, _ := json.Marshal(output)
	resumeMessage := run.Message + "\n\napproved tool output for " + req.ActionSnapshot.ToolName + ": " + string(outputJSON) + "\n\nThe approved tool call has already executed. Do not call the same tool again; produce the final answer from this approved tool output."
	resumeReq := TurnRequest{
		AgentID:   profile.ID,
		Message:   resumeMessage,
		UserID:    responseActor(req.Response),
		SessionID: adkSessionID(run.SessionID, run.RunID+":tool-replay:"+uuid.NewString()),
		Channel:   "resume",
		Metadata: map[string]string{
			"conversation_session_id": run.SessionID,
			"agent_session_id":        run.SessionID,
			"human_request_id":        req.ID,
		},
	}
	base := runtimeEventBase{
		RunID:                 run.RunID,
		AgentID:               run.AgentID,
		EntrypointID:          run.EntrypointID,
		Channel:               "resume",
		ConversationSessionID: run.SessionID,
		AgentSessionID:        run.SessionID,
		TraceID:               run.RunID,
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
			RunID:   run.RunID,
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
	resumeCtx = contextWithToolTrace(resumeCtx, run.RunID)
	suspendCollector := newRuntimeSuspendCollector()
	resumeCtx = contextWithRuntimeSuspendCollector(resumeCtx, suspendCollector)
	resumeCtx = contextWithRunExecution(resumeCtx, runExecutionContext{
		Base:        base,
		Profile:     profile,
		Request:     resumeReq,
		UserMessage: resumeMessage,
	})
	resumeCtx = rtools.WithRunDir(resumeCtx, s.runs.RunDir(run.RunID))
	resumeCtx = s.withLLMInstrumentation(resumeCtx, llmInstrumentationInput{
		RunID:          run.RunID,
		AgentID:        profile.ID,
		EntrypointID:   run.EntrypointID,
		Channel:        "resume",
		SessionID:      run.SessionID,
		AgentSessionID: run.SessionID,
		ADKSessionID:   resumeReq.SessionID,
		UserID:         resumeReq.UserID,
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
	run.Message = resumeMessage
	run.FinalResponse = final
	run.ToolCalls = append(run.ToolCalls, toolCalls...)
	run.Events = append(run.Events, events...)
	run.AuditEvents = append(run.AuditEvents, audits...)
	run.LLMCalls = append(run.LLMCalls, llmCalls...)
	replaceRunHumanRequest(&run, *req)
	if interrupt := suspendCollector.Interrupt(); interrupt != nil {
		run.Interrupt = interrupt
		run.HumanRequests = append(run.HumanRequests, interrupt.HumanRequests...)
		run.VerificationResult = VerificationResult{Status: StatusWaitingHuman, Checks: []string{"runtime_interrupt"}}
		run.Status = StatusWaitingHuman
	} else {
		run.Interrupt = nil
		run.VerificationResult = s.verifier.Verify(final, profile.Verification.DefaultChecks)
		run.Status = "completed"
		if run.VerificationResult.Status != "passed" {
			run.Status = "failed"
		}
	}
	run.EndedAt = time.Now()
	run.Usage = summarizeUsage(run)
	return s.runs.SaveRun(run)
}

func (s *Service) resumeDirectHumanRequest(ctx context.Context, req *humanrequest.HumanRequest) error {
	if req == nil || req.Response == nil {
		return nil
	}
	run, err := s.runs.Load(req.RunID)
	if err != nil {
		return err
	}
	if run.Status != StatusWaitingHuman {
		return nil
	}
	if req.Response.Kind == humanrequest.ResponseDeny || req.Response.Kind == humanrequest.ResponseCancel {
		run.Status = "failed"
		if run.Metadata == nil {
			run.Metadata = map[string]string{}
		}
		if req.Response.Kind == humanrequest.ResponseCancel {
			run.Metadata["error_type"] = "canceled"
		}
		run.VerificationResult = VerificationResult{Status: "failed", Checks: []string{"human_response_" + string(req.Response.Kind)}}
		run.EndedAt = time.Now()
		replaceRunHumanRequest(&run, *req)
		return s.runs.SaveRun(run)
	}
	profile, ok := s.agents.Get(run.AgentID)
	if !ok {
		return nil
	}
	resumeMessage := childResumeMessage(run.Message, req)
	resumeReq := TurnRequest{
		AgentID:   profile.ID,
		Message:   resumeMessage,
		UserID:    responseActor(req.Response),
		SessionID: adkSessionID(run.SessionID, run.RunID+":resume:"+uuid.NewString()),
		Channel:   "resume",
		Metadata: map[string]string{
			"conversation_session_id": run.SessionID,
			"agent_session_id":        run.SessionID,
			"human_request_id":        req.ID,
		},
	}
	base := runtimeEventBase{
		RunID:                 run.RunID,
		AgentID:               run.AgentID,
		EntrypointID:          run.EntrypointID,
		Channel:               "resume",
		ConversationSessionID: run.SessionID,
		AgentSessionID:        run.SessionID,
		TraceID:               run.RunID,
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
			RunID:   run.RunID,
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
	resumeCtx = contextWithToolTrace(resumeCtx, run.RunID)
	suspendCollector := newRuntimeSuspendCollector()
	resumeCtx = contextWithRuntimeSuspendCollector(resumeCtx, suspendCollector)
	resumeCtx = contextWithRunExecution(resumeCtx, runExecutionContext{
		Base:        base,
		Profile:     profile,
		Request:     resumeReq,
		UserMessage: resumeMessage,
	})
	resumeCtx = rtools.WithRunDir(resumeCtx, s.runs.RunDir(run.RunID))
	resumeCtx = s.withLLMInstrumentation(resumeCtx, llmInstrumentationInput{
		RunID:          run.RunID,
		AgentID:        profile.ID,
		EntrypointID:   run.EntrypointID,
		Channel:        "resume",
		SessionID:      run.SessionID,
		AgentSessionID: run.SessionID,
		ADKSessionID:   resumeReq.SessionID,
		UserID:         resumeReq.UserID,
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
	run.Message = resumeMessage
	run.FinalResponse = final
	run.ToolCalls = append(run.ToolCalls, toolCalls...)
	run.Events = append(run.Events, events...)
	run.AuditEvents = append(run.AuditEvents, audits...)
	run.LLMCalls = append(run.LLMCalls, llmCalls...)
	replaceRunHumanRequest(&run, *req)
	if interrupt := suspendCollector.Interrupt(); interrupt != nil {
		run.Interrupt = interrupt
		run.HumanRequests = append(run.HumanRequests, interrupt.HumanRequests...)
		run.VerificationResult = VerificationResult{Status: StatusWaitingHuman, Checks: []string{"runtime_interrupt"}}
		run.Status = StatusWaitingHuman
	} else {
		run.Interrupt = nil
		run.VerificationResult = s.verifier.Verify(final, profile.Verification.DefaultChecks)
		run.Status = "completed"
		if run.VerificationResult.Status != "passed" {
			run.Status = "failed"
		}
	}
	run.EndedAt = time.Now()
	run.Usage = summarizeUsage(run)
	return s.runs.SaveRun(run)
}

func replaceRunHumanRequest(run *TurnResponse, req humanrequest.HumanRequest) {
	if run == nil {
		return
	}
	for i := range run.HumanRequests {
		if run.HumanRequests[i].ID == req.ID {
			run.HumanRequests[i] = req
			return
		}
	}
	run.HumanRequests = append(run.HumanRequests, req)
}
