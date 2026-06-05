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

	"github.com/ai-daming/xira/internal/agents"
	"github.com/ai-daming/xira/internal/model/deepseek"
	fsession "github.com/ai-daming/xira/internal/session"
)

func (s *Service) generateADK(
	ctx context.Context,
	profile agents.Profile,
	req TurnRequest,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) (string, []ToolCallRecord, error) {
	adkModel, err := deepseek.NewADKModelWithThinking(profile.ModelPolicy.Model, s.deepseek, deepseek.Thinking{Type: thinkingType(profile.ModelPolicy)})
	if err != nil {
		return "", nil, err
	}
	var toolRecords []ToolCallRecord
	tools, err := s.adkTools(ctx, profile, recordEvent, recordAudit, func(rec ToolCallRecord) {
		toolRecords = append(toolRecords, rec)
	})
	if err != nil {
		return "", nil, err
	}
	agent, err := llmagent.New(llmagent.Config{
		Name:                  profile.ID,
		Description:           profile.Description,
		Model:                 adkModel,
		Instruction:           s.instructionText(profile),
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
	conversationSessionID := strings.TrimSpace(req.Metadata["conversation_session_id"])
	agentSessionID := strings.TrimSpace(req.Metadata["agent_session_id"])
	historyMessages, historyChars, err := s.hydrateADKSession(ctx, req.UserID, req.SessionID, profile.ID, conversationSessionID)
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
	for evt, err := range run.Run(ctx, req.UserID, req.SessionID, genai.NewContentFromText(req.Message, genai.RoleUser), adkRunConfig(profile)) {
		if err != nil {
			return final, toolRecords, err
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
	}
	if strings.TrimSpace(final) == "" {
		recordEvent("adk.empty_final", "adk.runner", "final ADK event contained no response text", map[string]any{
			"agent_id": profile.ID,
		})
		return final, toolRecords, fmt.Errorf("ADK runner produced empty final response")
	}
	recordAudit("adk.runner", profile.ID, true, "ADK runner completed", nil)
	return final, toolRecords, nil
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
		t, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{
			Name:         def.Name,
			Description:  def.Description,
			InputSchema:  def.InputSchema,
			OutputSchema: def.OutputSchema,
			RequireConfirmationProvider: func(_ map[string]any) bool {
				return def.Policy.RequireConfirmation
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
