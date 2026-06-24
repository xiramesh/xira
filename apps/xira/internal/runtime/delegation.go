package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	statusToolName = "emit_status"
)

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

	// Phase 6a (#55): delegate_agent retired — spawn_turn replaces it.

	// spawn_turn: async child turn spawn (Phase 3, RFC §2.4).
	spawnTool, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name:        spawnTurnToolName,
		Description: "Asynchronously spawn a child agent turn. Returns immediately with {agent_turn_id, status:spawned}; the child runs in the background. Use this instead of delegate_agent when you do not need to block on the child's result.",
		InputSchema: spawnTurnInputSchema(),
		OutputSchema: objectSchema(),
	}, func(toolCtx adktool.Context, args map[string]any) (map[string]any, error) {
		start := time.Now()
		callID := strings.TrimSpace(toolCtx.FunctionCallID())
		if callID == "" {
			callID = uuid.NewString()
		}
		spec, cleanInput, unsupported := sanitizeSpawnTurnInput(args)
		rec := ToolCallRecord{
			ID:        callID,
			Name:      spawnTurnToolName,
			Input:     cleanInput,
			StartedAt: start,
		}
		if len(unsupported) > 0 {
			rec.Input["unsupported_input_fields"] = unsupported
		}
		exec, ok := runExecutionFromContext(ctx)
		if !ok {
			err := errors.New("spawn_turn requires runtime execution context")
			rec.Output = map[string]any{"status": "rejected", "error": err.Error()}
			rec.Error = err.Error()
			rec.EndedAt = time.Now()
			recordTool(rec)
			return rec.Output, nil
		}
		if err := spec.Validate(); err != nil {
			rec.Output = map[string]any{"status": "rejected", "error": err.Error()}
			rec.Error = err.Error()
			rec.EndedAt = time.Now()
			recordTool(rec)
			return rec.Output, nil
		}
		// Spawn guardrails: mirror delegate_agent's depth/outstanding/parallel
		// limits so spawn cannot bypass them during grey-rollout. The slot is
		// reserved here (synchronous) and released by spawnCore's goroutine
		// (onChildDone) when the child finishes — covering the child's async
		// lifetime. Rejections are visible to the LLM and reserve no slot.
		spawnPolicy := profile.NormalizedDelegationPolicy()
		releaseSlot, effectiveTimeoutMS, guardErr := evaluateSpawnGuardrails(s, spawnPolicy, exec.Base.RunID, exec.Base.DelegationDepth)
		if guardErr != nil {
			rec.Output = map[string]any{"status": "rejected", "error": guardErr.Error()}
			rec.Error = guardErr.Error()
			rec.EndedAt = time.Now()
			recordTool(rec)
			return rec.Output, nil
		}
		target := &serviceSpawnTarget{
			service:     s,
			caller:      profile,
			parentBase:  exec.Base,
			parentRunID: exec.Base.RunID,
			toolCallID:  callID,
			parentDepth: exec.Base.DelegationDepth,
			sessionMode: spawnPolicy.ChildSessionMode,
		}
		spawned := spawnCore(ctx, spec, target, s.events, effectiveTimeoutMS, releaseSlot)
		rec.Output = spawnTurnOutput(spawned.TurnID, spawned.Status)
		rec.EndedAt = time.Now()
		recordTool(rec)
		return rec.Output, nil
	})
	if err != nil {
		return nil, err
	}
	out = append(out, spawnTool)

	// poll_turn: parent LLM checks a spawned child's result NON-BLOCKINGLY
	// (Phase 4, RFC §2.4 D-3). Returns the result if the child finished, or
	// {status:"pending"} if still running — never blocks. Coexists with
	// spawn_turn under the same DelegationPolicy gate.
	//
	// CRITICAL: must NOT block. ADK runs tools synchronously (base_flow.go
	// wg.Wait); a blocking tool freezes the event loop and disables the
	// steering checkpoint. The previous wait_turn blocked → broke steering
	// (PR #53 review). poll_turn peeks instead.
	pollTool, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
		Name:        pollTurnToolName,
		Description: "Check whether a spawned child agent turn has finished and return its result if so. Pass the agent_turn_id from spawn_turn. Returns immediately: {status:completed/failed, result_summary} when done, or {status:pending} when the child is still running. If pending, do other work and poll again later — do NOT block waiting.",
		InputSchema: pollTurnInputSchema(),
		OutputSchema: objectSchema(),
	}, func(toolCtx adktool.Context, args map[string]any) (map[string]any, error) {
		start := time.Now()
		callID := strings.TrimSpace(toolCtx.FunctionCallID())
		if callID == "" {
			callID = uuid.NewString()
		}
		spec, cleanInput, unsupported := sanitizePollTurnInput(args)
		rec := ToolCallRecord{
			ID:        callID,
			Name:      pollTurnToolName,
			Input:     cleanInput,
			StartedAt: start,
		}
		if len(unsupported) > 0 {
			rec.Input["unsupported_input_fields"] = unsupported
		}
		if err := spec.Validate(); err != nil {
			rec.Output = map[string]any{"status": "rejected", "error": err.Error()}
			rec.Error = err.Error()
			rec.EndedAt = time.Now()
			recordTool(rec)
			return rec.Output, nil
		}
		// poll_turn uses the turn's ctx (which carries the SpawnSink injected
		// by Router), NOT the tool ctx. Non-blocking peek via SpawnSinkPeeper.
		rec.Output = executePollTurn(ctx, spec.ChildTurnID)
		rec.EndedAt = time.Now()
		recordTool(rec)
		return rec.Output, nil
	})
	if err != nil {
		return nil, err
	}
	out = append(out, pollTool)

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

func rejectAllSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Not: &jsonschema.Schema{}}
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

// outstandingChildCount returns the number of outstanding (in-flight) child
// runs for a parent (in-memory active-children count). Entry point for
// spawn guardrails (evaluateSpawnGuardrails).
func (s *Service) outstandingChildCount(parentRunID string) (int, error) {
	return s.activeChildCount(parentRunID), nil
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

