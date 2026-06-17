package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
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
		var role genai.Role = genai.RoleModel
		event.Author = agentID
		if strings.TrimSpace(msg.Role) == "user" {
			role = genai.RoleUser
			event.Author = "user"
		}
		event.LLMResponse = adkmodel.LLMResponse{
			Content: genai.NewContentFromText(msg.Content, role),
		}
		return event, contentChars, true
	}
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
		def, _ := registry.GetDefinition(name)
		if def.Policy.RequireConfirmation {
			callID := strings.TrimSpace(toolCtx.FunctionCallID())
			if callID == "" {
				callID = uuid.NewString()
			}
			rec := ToolCallRecord{
				ID:        callID,
				Name:      name,
				Input:     cloneAnyMap(input),
				StartedAt: time.Now(),
				EndedAt:   time.Now(),
				Output: map[string]any{
					"status":           StatusWaitingHuman,
					"human_request_id": "",
				},
			}
			if err := validateRuntimeToolInputAllowlist(ctx, name, input); err != nil {
				rec.Error = err.Error()
				recordTool(rec)
				return toolOutputForModel(rec), nil
			}
			req, err := s.createRuntimeToolGateHumanRequest(ctx, callID, name, input)
			if err != nil {
				rec.Error = err.Error()
				recordTool(rec)
				return toolOutputForModel(rec), nil
			}
			rec.Output["human_request_id"] = req.ID
			recordTool(rec)
			recordEvent("human.request.created", "runtime", "runtime tool confirmation required", map[string]any{
				"human_request_id": req.ID,
				"kind":             req.Kind,
				"source":           req.Source,
				"tool":             name,
				"tool_call_id":     callID,
			})
			recordAudit("human.request", req.ID, true, "runtime tool confirmation required", map[string]any{
				"tool":         name,
				"tool_call_id": callID,
			})
			return toolOutputForModel(rec), nil
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
			RequireConfirmationProvider: func(_ map[string]any) bool {
				return false
			},
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
