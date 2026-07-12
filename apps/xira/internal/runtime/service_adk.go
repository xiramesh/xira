package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/model/deepseek"
	fsession "github.com/xiramesh/xira/internal/session"
	rtools "github.com/xiramesh/xira/internal/tools"
)

func (s *Service) generateADK(
	ctx context.Context,
	profile agents.Profile,
	instructionText string,
	req TurnRequest,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) (string, []ToolCallRecord, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx = contextWithRuntimeInterruptCancel(ctx, cancel)

	adkModel, err := deepseek.NewADKModelWithThinking(profile.ModelPolicy.Model, s.deepseek, deepseek.Thinking{Type: thinkingType(profile.ModelPolicy)})
	if err != nil {
		return "", nil, err
	}
	toolRecords := &toolCallRecorder{}
	tools, err := s.adkTools(ctx, profile, recordEvent, recordAudit, func(rec ToolCallRecord) {
		toolRecords.append(rec)
	})
	if err != nil {
		return "", nil, err
	}
	agent, err := llmagent.New(llmagent.Config{
		Name:                  profile.ID,
		Description:           profile.Description,
		Model:                 adkModel,
		Instruction:           instructionText,
		Tools:                 tools,
		GenerateContentConfig: generateContentConfig(profile),
		BeforeModelCallbacks: []llmagent.BeforeModelCallback{
			func(_ adkagent.CallbackContext, _ *adkmodel.LLMRequest) (*adkmodel.LLMResponse, error) {
				if collector := runtimeSuspendCollectorFromContext(ctx); collector != nil && collector.HasInterrupt() {
					return nil, errRuntimeInterrupted
				}
				return nil, nil
			},
		},
	})
	if err != nil {
		return "", nil, err
	}
	run, err := runner.New(runner.Config{
		AppName:           "xira",
		Agent:             agent,
		SessionService:    s.adkSessions,
		AutoCreateSession: true,
	})
	if err != nil {
		return "", nil, err
	}
	conversationSessionID := strings.TrimSpace(req.Context.Raw["conversation_session_id"])
	agentSessionID := strings.TrimSpace(req.Context.Raw["agent_session_id"])
	historyMessages, historyChars, err := s.hydrateADKSession(ctx, req.Context.SenderID, req.SessionID, profile.ID, conversationSessionID)
	if err != nil {
		recordEvent("adk.session_hydrate_failed", "adk.session", "failed to restore session history", map[string]any{
			"agent_id":                profile.ID,
			"agent_session_id":        agentSessionID,
			"adk_session_id":          req.SessionID,
			"conversation_session_id": conversationSessionID,
			"error":                   err.Error(),
		})
	} else {
		recordEvent("adk.session_hydrated", "adk.session", "restored session history", map[string]any{
			"agent_id":                profile.ID,
			"agent_session_id":        agentSessionID,
			"adk_session_id":          req.SessionID,
			"conversation_session_id": conversationSessionID,
			"messages":                historyMessages,
			"content_chars":           historyChars,
		})
	}
	var final string
	var latestText string
	for evt, err := range run.Run(ctx, req.Context.SenderID, req.SessionID, genai.NewContentFromText(req.Message, genai.RoleUser), adkRunConfig(profile)) {
		if err != nil {
			if collector := runtimeSuspendCollectorFromContext(ctx); collector != nil && collector.HasInterrupt() {
				recordEvent("adk.suspended", "adk.runner", "ADK runner suspended by runtime interrupt", map[string]any{
					"agent_id": profile.ID,
				})
				return final, toolRecords.snapshot(), nil
			}
			return final, toolRecords.snapshot(), err
		}
		if evt == nil {
			continue
		}
		text := contentText(evt.Content)
		if strings.TrimSpace(text) != "" {
			latestText = text
		}
		payload := map[string]any{
			"event_id":      evt.ID,
			"invocation_id": evt.InvocationID,
			"partial":       evt.Partial,
			"final":         evt.IsFinalResponse(),
			"parts":         contentPartCount(evt.Content),
			"content_chars": utf8.RuneCountInString(text),
			"turn_complete": evt.TurnComplete,
		}
		if evt.ModelVersion != "" {
			payload["model"] = evt.ModelVersion
		}
		if evt.FinishReason != "" {
			payload["finish_reason"] = string(evt.FinishReason)
		}
		if evt.ErrorCode != "" {
			payload["error_code"] = evt.ErrorCode
		}
		if evt.ErrorMessage != "" {
			payload["error_message"] = evt.ErrorMessage
		}
		recordEvent("adk.event", "adk.runner", evt.Author, payload)
		var parts []*genai.Part
		if evt.Content != nil {
			parts = evt.Content.Parts
		}
		for _, part := range parts {
			if part == nil || part.FunctionCall == nil || !isHumanRequestToolWireName(part.FunctionCall.Name) {
				continue
			}
			req, err := s.createAgentHumanRequest(ctx, part.FunctionCall.ID, part.FunctionCall.Args)
			if err != nil {
				recordAudit("human.request", part.FunctionCall.ID, false, err.Error(), part.FunctionCall.Args)
				return final, toolRecords.snapshot(), err
			}
			recordEvent("human.request.created", "runtime", "human request created", map[string]any{
				"human_request_id": req.ID,
				"kind":             req.Kind,
				"source":           req.Source,
				"tool_call_id":     req.ToolCallID,
				"question":         req.Question, // #109: LLM-written question, surfaced to IM via humanRequestedQuestion
			})
			recordAudit("human.request", req.ID, true, "agent requested human input", map[string]any{
				"kind":         req.Kind,
				"tool_call_id": req.ToolCallID,
			})
			recordEvent("adk.suspended", "adk.runner", "ADK runner suspended by runtime interrupt", map[string]any{
				"agent_id": profile.ID,
			})
			return final, toolRecords.snapshot(), nil
		}
		if evt.IsFinalResponse() {
			final = text
			if strings.TrimSpace(final) == "" {
				final = latestText
			}
		}
		if collector := runtimeSuspendCollectorFromContext(ctx); collector != nil && collector.HasInterrupt() {
			recordEvent("adk.suspended", "adk.runner", "ADK runner suspended by runtime interrupt", map[string]any{
				"agent_id": profile.ID,
			})
			return final, toolRecords.snapshot(), nil
		}
		// Steering checkpoint (Phase 4, RFC #48 §5): if the user sent a
		// message while this turn is running, signal the caller to restart
		// with the interjection. PEEK only (HasPending) — the retry loop
		// in the channel runner CONSUMES via TryDequeue. Do NOT consume
		// here (PR #51 review: double-dequeue bug).
		// Returns ErrSteered sentinel (NOT ctx.Err()) so the retry loop
		// distinguishes "steered" from "real error".
		if sink := SteeringBusFromContext(ctx); sink != nil && sink.HasPending() {
			recordEvent("adk.steered", "adk.runner", "turn steered by user interjection", map[string]any{
				"agent_id": profile.ID,
			})
			return final, toolRecords.snapshot(), ErrSteered
		}
		// Deadline checkpoint (#76): ctx cancelled (e.g. resume WithTimeout
		// fired). ADK's own event loop does NOT check ctx.Done between steps
		// (verified against adk v1.4.0 base_flow.go), so without this peek a
		// cancelled resume would keep running generate to its natural end — an
		// unbounded detached goroutine (answer_child, #68) or an unbounded
		// synchronous resume. Returns ctx.Err() (DeadlineExceeded/Canceled),
		// a distinct error: errors.Is(ctx.Err(), ErrSteered) is always false,
		// matching the steering_checkpoint_test.go "must NOT match
		// context.Canceled" guard. Normal turns never hit this: their ctx
		// carries no deadline (only IM-send chatcontext.go uses WithTimeout).
		if ctx.Err() != nil {
			recordEvent("adk.context_cancelled", "adk.runner", "turn aborted: context deadline/cancel", map[string]any{
				"agent_id": profile.ID,
				"ctx_err":  ctx.Err().Error(),
			})
			return final, toolRecords.snapshot(), ctx.Err()
		}
	}
	if strings.TrimSpace(final) == "" {
		recordEvent("adk.empty_final", "adk.runner", "final ADK event contained no response text", map[string]any{
			"agent_id": profile.ID,
		})
		return final, toolRecords.snapshot(), fmt.Errorf("ADK runner produced empty final response")
	}
	recordAudit("adk.runner", profile.ID, true, "ADK runner completed", nil)
	return final, toolRecords.snapshot(), nil
}

func generateContentConfig(profile agents.Profile) *genai.GenerateContentConfig {
	if profile.ModelPolicy.Temp == nil {
		return nil
	}
	temp := *profile.ModelPolicy.Temp
	return &genai.GenerateContentConfig{Temperature: &temp}
}

func adkRunConfig(profile agents.Profile) adkagent.RunConfig {
	if profile.ModelPolicy.Stream {
		return adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE}
	}
	return adkagent.RunConfig{StreamingMode: adkagent.StreamingModeNone}
}

func (s *Service) hydrateADKSession(ctx context.Context, userID, adkSessionID, agentID, conversationSessionID string) (int, int, error) {
	userID = strings.TrimSpace(userID)
	adkSessionID = strings.TrimSpace(adkSessionID)
	agentID = strings.TrimSpace(agentID)
	conversationSessionID = strings.TrimSpace(conversationSessionID)
	if s == nil || s.adkSessions == nil || s.sessions == nil || userID == "" || adkSessionID == "" {
		return 0, 0, nil
	}
	created, err := s.adkSessions.Create(ctx, &adksession.CreateRequest{
		AppName:   "xira",
		UserID:    userID,
		SessionID: adkSessionID,
	})
	if err != nil {
		existing, getErr := s.adkSessions.Get(ctx, &adksession.GetRequest{
			AppName:   "xira",
			UserID:    userID,
			SessionID: adkSessionID,
		})
		if getErr != nil {
			return 0, 0, err
		}
		created = &adksession.CreateResponse{Session: existing.Session}
	}
	if conversationSessionID == "" || agentID == "" {
		return 0, 0, nil
	}
	var restored int
	var contentChars int
	for _, msg := range s.sessions.AgentHistory(conversationSessionID, agentID) {
		// Skip messages from failed runs: their tool events must not leak into
		// the next run's model context. Audit still keeps them on disk.
		if msg.Metadata != nil {
			if rs, _ := msg.Metadata["run_status"].(string); rs == "failed" || rs == "steered" {
				continue
			}
		}
		event, chars, ok := adkEventFromSessionMessage(msg, agentID)
		if !ok {
			continue
		}
		restored++
		contentChars += chars
		if err := s.adkSessions.AppendEvent(ctx, created.Session, event); err != nil {
			return restored, contentChars, err
		}
	}
	return restored, contentChars, nil
}

func adkEventFromSessionMessage(msg fsession.Message, agentID string) (*adksession.Event, int, bool) {
	kind := strings.TrimSpace(msg.Kind)
	if kind == "" {
		kind = fsession.MessageKindMessage
	}
	event := adksession.NewEvent("xira-session-restore")
	if !msg.CreatedAt.IsZero() {
		event.Timestamp = msg.CreatedAt
	}
	contentChars := utf8.RuneCountInString(msg.Content)
	switch kind {
	case fsession.MessageKindHumanRequest, fsession.MessageKindHumanResponse:
		// #106: skip PENDING HITL messages so the bare question does not leak
		// into the next turn's model context — injectPendingHITLSummary (called
		// from RunAgent) already provides a structured "Pending Human Requests"
		// block in the user message. Without this skip, the pending question
		// would ALSO reappear here as a de-contextualized assistant text,
		// duplicating the summary and confusing the model.
		//
		// Only run_status=waiting_human (the run paused on this HITL and never
		// resumed) is skipped. Resolved HITL (run_status=completed/failed — the
		// run resumed and finished) is kept as history text so later turns in
		// the same session retain "the user already answered X".
		if rs, _ := msg.Metadata["run_status"].(string); rs == StatusWaitingHuman {
			return nil, 0, false
		}
		return textSessionEvent(msg, agentID, event, contentChars)
	case fsession.MessageKindToolCall:
		name := strings.TrimSpace(msg.ToolName)
		if name == "" || name == "exec" {
			return nil, 0, false
		}
		args := map[string]any{}
		if strings.TrimSpace(msg.Content) != "" {
			_ = json.Unmarshal([]byte(msg.Content), &args)
		}
		content := genai.NewContentFromFunctionCall(name, args, genai.RoleModel)
		if len(content.Parts) > 0 && content.Parts[0].FunctionCall != nil {
			content.Parts[0].FunctionCall.ID = strings.TrimSpace(msg.ToolCallID)
		}
		event.Author = agentID
		event.LLMResponse = adkmodel.LLMResponse{Content: content}
		return event, contentChars, true
	case fsession.MessageKindToolResult:
		name := strings.TrimSpace(msg.ToolName)
		if name == "" || name == "exec" {
			return nil, 0, false
		}
		response := map[string]any{}
		if err := json.Unmarshal([]byte(msg.Content), &response); err != nil {
			response = map[string]any{"result": msg.Content}
		}
		content := genai.NewContentFromFunctionResponse(name, response, genai.RoleUser)
		if len(content.Parts) > 0 && content.Parts[0].FunctionResponse != nil {
			content.Parts[0].FunctionResponse.ID = strings.TrimSpace(msg.ToolCallID)
		}
		event.Author = agentID
		event.LLMResponse = adkmodel.LLMResponse{Content: content}
		return event, contentChars, true
	default:
		return textSessionEvent(msg, agentID, event, contentChars)
	}
}

// textSessionEvent renders a session message as a plain text ADK event (the
// default hydration path for non-tool messages, and for resolved HITL messages
// after #106's pending-skip). Role/author are derived from msg.Role.
func textSessionEvent(msg fsession.Message, agentID string, event *adksession.Event, contentChars int) (*adksession.Event, int, bool) {
	var role genai.Role = genai.RoleModel
	event.Author = agentID
	if strings.TrimSpace(msg.Role) == "user" {
		role = genai.RoleUser
		event.Author = "user"
	}
	// #151：群聊 observed 消息带说话人标识（[name|id]\n 前缀），让 LLM 区分谁在说话。
	content := msg.Content
	if msg.SenderID != "" && role == genai.RoleUser {
		name := msg.SenderName
		if name == "" {
			name = msg.SenderID
		}
		content = "[" + name + "|" + msg.SenderID + "] " + content
	}
	event.LLMResponse = adkmodel.LLMResponse{
		Content: genai.NewContentFromText(content, role),
	}
	return event, contentChars, true
}

func (s *Service) adkTools(
	ctx context.Context,
	profile agents.Profile,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
	recordTool func(ToolCallRecord),
) ([]adktool.Tool, error) {
	var out []adktool.Tool
	if !runtimeNativeToolsDisabledFromContext(ctx) {
		runtimeTools, err := s.runtimeADKTools(ctx, profile, recordEvent, recordAudit, recordTool)
		if err != nil {
			return nil, err
		}
		for _, tool := range runtimeTools {
			if !runtimeToolAllowedFromContext(ctx, tool.Name()) {
				continue
			}
			out = append(out, tool)
		}
	}
	registry := s.toolRegistry(profile)
	run := func(toolCtx adktool.Context, name string, input map[string]any) (map[string]any, error) {
		if input == nil {
			input = map[string]any{}
		}
		call := deepseek.ToolCall{
			ID:   strings.TrimSpace(toolCtx.FunctionCallID()),
			Type: "function",
			Function: deepseek.ToolCallFunction{
				Name:      name,
				Arguments: mustJSON(input),
			},
		}
		rec := s.executeToolCall(ctx, profile, call, recordEvent, recordAudit)
		recordTool(rec)
		return toolOutputForModel(rec), nil
	}
	for _, def := range registry.Definitions() {
		def := def
		if !runtimeToolAllowedFromContext(ctx, def.Name) {
			continue
		}
		parameters := constrainedToolParameters(ctx, def.Name, def.Parameters)
		t, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
			Name:         def.Name,
			Description:  def.Description,
			InputSchema:  rtools.SchemaFromMap(parameters),
			OutputSchema: def.OutputSchema,
		}, func(toolCtx adktool.Context, args map[string]any) (map[string]any, error) {
			return run(toolCtx, def.Name, args)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func contentText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	var parts []string
	for _, part := range content.Parts {
		if part != nil && part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "")
}

func contentPartCount(content *genai.Content) int {
	if content == nil {
		return 0
	}
	return len(content.Parts)
}
