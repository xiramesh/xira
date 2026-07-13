package runtime

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
	rtools "github.com/xiramesh/xira/internal/tools"
)

// deliverResumeFinal pushes a resumed run's final response back to the
// originating IM channel via the outbound emitter (RFC #27 — stateless HITL
// resume). This is the fix for the resume→IM delivery gap: previously a
// resumed run's final was persisted but never reached the user's IM because
// the resume ran in an HTTP/CLI context with no EventBus/ChatContext.
//
// Now the runtime holds an OutboundEmitter (the channel Manager, injected by
// main.go); resume reconstructs the target (channel/chat/sender) from the
// run's persisted SessionScope and routes the final through the Manager to the
// right channel runner.
//
// Best-effort: if the emitter is absent (no channels — CLI/tests) or delivery
// fails, the run is still persisted correctly (delivery is logged, not fatal).
// Skipped when: outbound nil, empty final, run re-entered waiting_human, or
// no SessionScope (cannot route).
func (s *Service) deliverResumeFinal(ctx context.Context, run TurnResponse) {
	if s == nil || s.outbound == nil {
		return
	}
	if strings.TrimSpace(run.FinalResponse) == "" {
		return
	}
	if run.Status == StatusWaitingHuman {
		// Resumed into another HITL round — no final for the user yet.
		return
	}
	if run.SessionScope == nil {
		slog.Warn("resume final delivery skipped: run has no session scope (cannot route)",
			"run_id", run.RunID)
		return
	}
	target := inboundContextFromScope(run.SessionScope, run.Metadata)
	if strings.TrimSpace(target.Channel) == "" {
		slog.Warn("resume final delivery skipped: session scope has no channel",
			"run_id", run.RunID)
		return
	}
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.RunID = run.RunID
	env.Target = &target
	env.Data = map[string]any{"content": run.FinalResponse}
	if err := s.outbound.Emit(ctx, env); err != nil {
		// Delivery failure does NOT fail the resume — the run is already
		// persisted; the user can retry or inspect history. Log and move on.
		slog.Error("resume final delivery failed",
			"run_id", run.RunID,
			"channel", target.Channel,
			"error", err)
	}
}

// resumeDirectHumanRequest resumes a run that paused on a direct human.request
// (#68: typically a spawned child asking its parent a question). The resume
// re-runs generate with the FULL profile — tools/skills kept, native tools NOT
// disabled — because the semantic is "the question was answered, now keep
// working."
//
// # Stateless resume ctx contract
//
//   - EventBus: resume runs in an HTTP/CLI context with no per-chat-key sink
//     (per 861cf17's stateless-resume model). Signal events during resume are
//     dropped with a slog.Debug trace (not silent). Hard-wiring a sink would
//     reintroduce statefulness; the resume is async and the user observes the
//     final via deliverResumeFinal, not real-time progress.
//   - Execution policy: persisted on the run and restored below before generate
//     resumes. Do not reintroduce the old "AllowedTools is missing" gap note.
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
		s.persistResumeSessionMessages(run, req, "")
		return s.runs.SaveRun(run)
	}
	profile, ok := s.agents.Get(run.AgentID)
	if !ok {
		return nil
	}
	resumeMessage := childResumeMessage(run.Message, req)
	// Restore the original trigger facts (including addressed_to=owner) from
	// the persisted run before rebuilding the InboundContext. Runtime-owned
	// correlation keys are written last so stale or caller-controlled metadata
	// cannot override the resume's authoritative IDs.
	resumeRaw := copyTurnMetadata(run.Metadata)
	if resumeRaw == nil {
		resumeRaw = map[string]string{}
	}
	resumeRaw["conversation_session_id"] = run.SessionID
	resumeRaw["agent_session_id"] = run.SessionID
	resumeRaw["human_request_id"] = req.ID
	resumeReq := TurnRequest{
		AgentID:   profile.ID,
		Message:   resumeMessage,
		SessionID: adkSessionID(run.SessionID, run.RunID+":resume:"+uuid.NewString()),
		// Resume inherits the run's original trigger identity from its persisted
		// session scope, not a forged "resume" channel.
		Context: inboundContextFromScope(run.SessionScope, resumeRaw),
	}
	applyExecutionPolicySnapshot(&resumeReq, run.ExecutionPolicy)
	base := runtimeEventBase{
		RunID:                 run.RunID,
		AgentID:               run.AgentID,
		EntrypointID:          run.EntrypointID,
		Channel:               resumeReq.Context.Channel,
		Account:               resumeReq.Context.Account,
		ChannelAppID:          resumeReq.Context.ChannelAppID,
		BotID:                 resumeReq.Context.BotID,
		ConversationSessionID: run.SessionID,
		AgentSessionID:        run.SessionID,
		ChatID:                resumeReq.Context.ChatID,
		ChatType:              resumeReq.Context.ChatType,
		ChatName:              resumeReq.Context.ChatName,
		TopicID:               resumeReq.Context.TopicID,
		SpaceID:               resumeReq.Context.SpaceID,
		SpaceType:             resumeReq.Context.SpaceType,
		SenderID:              resumeReq.Context.SenderID,
		SenderName:            resumeReq.Context.SenderName,
		MessageID:             resumeReq.Context.MessageID,
		ReplyToMessageID:      resumeReq.Context.ReplyToMessageID,
		ReplyToSenderID:       resumeReq.Context.ReplyToSenderID,
		TraceID:               run.RunID,
	}
	var events []RuntimeEvent
	var audits []AuditEvent
	var llmCalls []LLMCallRecord
	recordEvent := func(kind, source, message string, payload map[string]any) {
		evt := newRuntimeEvent(base, kind, source, message, payload, nil)
		events = append(events, evt)
		dispatchEvent(ctx, evt)
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
	if resumeReq.AllowedToolsSet || len(resumeReq.AllowedTools) > 0 {
		resumeCtx = contextWithRuntimeToolAllowlist(resumeCtx, resumeReq.AllowedTools)
	}
	resumeCtx = contextWithRuntimeToolInputAllowlist(resumeCtx, resumeReq.ToolInputAllowlist)
	// #76: bound the resume turn so a hanging generate (multi-round tool loop)
	// cannot run unbounded. MaxDurationMS is the delegation ceiling (default
	// 120s); the ctx-deadline checkpoint in generateADK (service_adk.go)
	// enforces it. Nested under ctx, so a shorter parent deadline (e.g. a test
	// ctx or an HTTP request timeout) still wins.
	resumeDeadline := time.Duration(profile.NormalizedDelegationPolicy().MaxDurationMS) * time.Millisecond
	resumeCtx, resumeCancel := context.WithTimeout(resumeCtx, resumeDeadline)
	defer resumeCancel()
	suspendCollector := newRuntimeSuspendCollector()
	resumeCtx = contextWithRuntimeSuspendCollector(resumeCtx, suspendCollector)
	resumeCtx = contextWithRunExecution(resumeCtx, runExecutionContext{
		Base:        base,
		Profile:     profile,
		Request:     resumeReq,
		UserMessage: resumeMessage,
	})
	resumeCtx = rtools.WithRunDir(resumeCtx, s.runs.RunDir(run.RunID))
	// #114: re-attach chatKey so resume turn can chain-interpret (#107) and
	// hydrate pending HITL (#106). Sourced from req.ChatKey (NOT SessionScope —
	// see withChatKeyFromRequest doc for the lossy-transform CRITICAL).
	resumeCtx = withChatKeyFromRequest(resumeCtx, req)
	resumeCtx = s.withLLMInstrumentation(resumeCtx, llmInstrumentationInput{
		RunID:          run.RunID,
		AgentID:        profile.ID,
		EntrypointID:   run.EntrypointID,
		Channel:        resumeReq.Context.Channel,
		SessionID:      run.SessionID,
		AgentSessionID: run.SessionID,
		ADKSessionID:   resumeReq.SessionID,
		UserID:         resumeReq.Context.SenderID,
		Pricing:        s.pricing,
	}, recordEvent, func(call LLMCallRecord) {
		llmCalls = append(llmCalls, call)
	})
	instruction, _, err := s.instructionTextForRunContext(resumeCtx, profile, resumeReq.Context)
	if err != nil {
		return err
	}
	final, toolCalls, err := s.generate(resumeCtx, profile, instruction, resumeReq, recordEvent, recordAudit)
	if err != nil {
		// #76: if the resume ctx itself was cancelled (deadline fired, or the
		// detached answer_child goroutine's parent ctx died), mark the run
		// failed so it is not left waiting_human forever. We check resumeCtx
		// directly rather than errors.Is(err, context.*) because the error
		// surfaces wrapped from deep down (e.g. "deepseek stream failed: ...")
		// and does not unwrap to a context error.
		if resumeCtx.Err() != nil {
			run.Status = "failed"
			if run.Metadata == nil {
				run.Metadata = map[string]string{}
			}
			run.Metadata["error_type"] = "resume_timeout"
			run.VerificationResult = VerificationResult{Status: "failed", Checks: []string{"resume_context_deadline"}}
			run.EndedAt = time.Now()
			replaceRunHumanRequest(&run, *req)
			return s.runs.SaveRun(run)
		}
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
		run.VerificationResult = s.verifyRunOutcome(final, toolCalls, profile.Verification.DefaultChecks)
		run.Status = "completed"
		if run.VerificationResult.Status != "passed" {
			run.Status = "failed"
		}
	}
	run.EndedAt = time.Now()
	run.Usage = summarizeUsage(run)
	s.persistResumeSessionMessages(run, req, resumeMessage)
	if err := s.runs.SaveRun(run); err != nil {
		return err
	}
	s.deliverResumeFinal(ctx, run)
	return nil
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

// childResumeMessage builds the resume message for a child run after a human
// response, appending the human's verdict to the original message. Relocated
// from delegation_resume.go (Phase 6a, #55).
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

// responseActor returns the actor name for a human response, defaulting to
// "human". Relocated from delegation_resume.go (Phase 6a, #55).
func responseActor(response *humanrequest.HumanResponse) string {
	if response == nil || strings.TrimSpace(response.Actor) == "" {
		return "human"
	}
	return strings.TrimSpace(response.Actor)
}

// withChatKeyFromRequest re-attaches the chatKey to a resume ctx from the
// HumanRequest being resolved (#114). Used by both resume paths so the resume
// turn can chain-interpret (#107 human.interpret reads chatKeyStringFromContext).
//
// # Data source choice (PR #115 review CRITICAL)
//
// We use req.ChatKey (the request's first-class persisted field), NOT
// SessionScope. The two look symmetric but have different semantics:
//   - SessionScope lowercases its values (manager.go:155/170) and applies
//     canonicalSenderID rewriting (manager.go:344-355). Both are lossy
//     transforms that can diverge from the original chatKey string.
//   - The store compares ListByChatKey with a case-sensitive string equality
//     (store.go:430). A lowercased or canonical-rewritten chatKey silently
//     fails to match → chain-interpret rejected → silent data loss.
//
// req.ChatKey is the exact value the store compares against (it was set from
// chatKeyStringFromContext at HR creation, same source the store query uses).
// Parsing it back to a ChatKey via ParseChatKey is lossless. If req.ChatKey is
// empty (older HRs without the field, or flow #112 gap), we leave ctx
// unchanged — same as pre-#114, hydration/interpret gracefully no-op.
func withChatKeyFromRequest(ctx context.Context, req *humanrequest.HumanRequest) context.Context {
	if req == nil {
		return ctx
	}
	key, ok := ParseChatKey(req.ChatKey)
	if !ok {
		return ctx
	}
	return WithChatKey(ctx, key)
}
