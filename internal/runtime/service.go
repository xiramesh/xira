package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	adksession "google.golang.org/adk/session"

	"github.com/ai-daming/xira/internal/agents"
	"github.com/ai-daming/xira/internal/channel"
	"github.com/ai-daming/xira/internal/entrypoints"
	"github.com/ai-daming/xira/internal/model/deepseek"
	"github.com/ai-daming/xira/internal/routing"
	fsession "github.com/ai-daming/xira/internal/session"
	rtools "github.com/ai-daming/xira/internal/tools"
)

type Config struct {
	ConfigPath     string
	WorkspaceRoot  string
	DefaultAgentID string
	RunRoot        string
	SessionRoot    string
	DeepSeekClient *deepseek.Client
}

type Service struct {
	agents        *agents.Manager
	events        *EventBus
	runs          *RunStore
	router        *routing.Router
	entrypoints   *entrypoints.Registry
	sessions      *fsession.Manager
	adkSessions   adksession.Service
	verifier      *VerificationRunner
	evolution     *EvolutionEngine
	deepseek      *deepseek.Client
	configPath    string
	workspace     string
	defaultAgent  string
	profileSource string
	mu            sync.RWMutex
}

func NewService(cfg Config) (*Service, error) {
	resolved, err := resolveRuntimeConfig(cfg)
	if err != nil {
		return nil, err
	}
	manager, profileSource, err := loadAgentManager(resolved)
	if err != nil {
		return nil, err
	}
	if _, ok := manager.Get(resolved.DefaultAgentID); !ok {
		return nil, fmt.Errorf("default agent %q not found", resolved.DefaultAgentID)
	}
	sessionManager, err := fsession.NewManagerWithStore(resolved.SessionRoot)
	if err != nil {
		return nil, err
	}
	dsClient := cfg.DeepSeekClient
	if dsClient == nil {
		if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
			return nil, errors.New("DEEPSEEK_API_KEY is required")
		}
		dsClient = deepseek.New()
	}
	return &Service{
		agents:        manager,
		events:        NewEventBus(),
		runs:          NewRunStore(resolved.RunRoot),
		router:        routing.NewRouterWithRules(resolved.DefaultAgentID, resolved.Routes),
		entrypoints:   entrypoints.NewRegistry(resolved.DefaultAgentID, resolved.Entrypoints),
		sessions:      sessionManager,
		adkSessions:   adksession.InMemoryService(),
		verifier:      NewVerificationRunner(),
		evolution:     NewEvolutionEngine(),
		deepseek:      dsClient,
		configPath:    resolved.ConfigPath,
		workspace:     resolved.WorkspaceRoot,
		defaultAgent:  resolved.DefaultAgentID,
		profileSource: profileSource,
	}, nil
}

func (s *Service) Close() {
	if s != nil && s.events != nil {
		s.events.Close()
	}
}

func (s *Service) EventBus() *EventBus {
	return s.events
}

func (s *Service) RunStore() *RunStore {
	return s.runs
}

func (s *Service) SessionManager() *fsession.Manager {
	return s.sessions
}

func (s *Service) Agents() []agents.Profile {
	list := s.agents.List()
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].ID == s.defaultAgent {
			return true
		}
		if list[j].ID == s.defaultAgent {
			return false
		}
		return list[i].ID < list[j].ID
	})
	return list
}

func (s *Service) Entrypoints() []entrypoints.Definition {
	if s == nil || s.entrypoints == nil {
		return nil
	}
	return s.entrypoints.Definitions()
}

func (s *Service) Status() map[string]any {
	return map[string]any{
		"name":           "xira",
		"config_path":    s.configPath,
		"workspace":      s.workspace,
		"run_root":       s.runs.Root(),
		"session_root":   s.sessions.Root(),
		"agents":         len(s.Agents()),
		"entrypoints":    len(s.entrypoints.Definitions()),
		"default_agent":  s.defaultAgent,
		"profile_source": s.profileSource,
	}
}

func (s *Service) RunAgent(ctx context.Context, req TurnRequest) (TurnResponse, error) {
	now := time.Now()
	inbound := channel.InboundEnvelope{
		Context:            channel.NewInboundContextWithEntrypoint(req.Channel, req.EntrypointID, req.UserID, req.Metadata),
		Content:            req.Message,
		RequestedAgentID:   req.AgentID,
		SessionIDOverride:  req.SessionID,
		Metadata:           req.Metadata,
		OriginalEntrypoint: "agent",
	}
	entrypointDecision, err := s.entrypoints.Resolve(entrypoints.ResolveInput{
		Context:          inbound.Context,
		EntrypointID:     req.EntrypointID,
		RequestedAgentID: inbound.RequestedAgentID,
	})
	if err != nil {
		return TurnResponse{}, err
	}
	if channelConflict(req.Channel, entrypointDecision.Definition.Channel) {
		return TurnResponse{}, fmt.Errorf("entrypoint %q uses channel %q, got request channel %q", entrypointDecision.Definition.ID, entrypointDecision.Definition.Channel, req.Channel)
	}
	inbound.Context.Channel = entrypointDecision.Definition.Channel
	inbound.Context.EntrypointID = entrypointDecision.Definition.ID
	if inbound.Context.Account == "" {
		inbound.Context.Account = entrypointDecision.Definition.Account
	}
	if inbound.Context.ChannelAppID == "" {
		inbound.Context.ChannelAppID = entrypointDecision.Definition.AppID
	}
	if inbound.Context.BotID == "" {
		inbound.Context.BotID = entrypointDecision.Definition.BotID
	}
	inbound.Context = channel.NormalizeInboundContext(inbound.Context)
	profile, ok := s.agents.Get(entrypointDecision.AgentID)
	if !ok {
		return TurnResponse{}, fmt.Errorf("agent profile %q not found", entrypointDecision.AgentID)
	}
	sessionPolicy := sessionPolicyForProfile(profile, entrypointDecision.SessionPolicy)
	allocation := s.sessions.Allocate(fsession.AllocationInput{
		Context:           inbound.Context,
		SessionPolicy:     sessionPolicy,
		SessionIDOverride: inbound.SessionIDOverride,
	})
	req.AgentID = profile.ID
	req.EntrypointID = inbound.Context.EntrypointID
	req.Channel = inbound.Context.Channel
	req.UserID = inbound.Context.SenderID
	req.SessionID = allocation.SessionID
	agentSessionID := fsession.BuildAgentSessionID(req.SessionID, profile.ID)
	runID := NewRunID(profile.ID, now)
	scope := allocation.Scope
	slog.Info("agent run accepted",
		"run_id", runID,
		"agent_id", profile.ID,
		"entrypoint_id", inbound.Context.EntrypointID,
		"channel", inbound.Context.Channel,
		"channel_app_id", inbound.Context.ChannelAppID,
		"bot_id", inbound.Context.BotID,
		"user_id", req.UserID,
		"session_id", req.SessionID,
		"agent_session_id", agentSessionID,
		"matched_by", entrypointDecision.MatchedBy,
		"message_chars", utf8.RuneCountInString(req.Message),
		"message_preview", previewText(req.Message, 120),
	)
	resp := TurnResponse{
		RunID:          runID,
		AgentID:        profile.ID,
		EntrypointID:   inbound.Context.EntrypointID,
		SessionID:      req.SessionID,
		SessionScope:   &scope,
		RouteMatchedBy: entrypointDecision.MatchedBy,
		Message:        req.Message,
		Status:         "running",
		StartedAt:      now,
		Metadata:       req.Metadata,
	}
	recordEvent := func(kind, source, message string, payload map[string]any) {
		evt := RuntimeEvent{
			ID:       uuid.NewString(),
			RunID:    runID,
			Kind:     kind,
			Time:     time.Now(),
			Source:   source,
			Severity: "info",
			Message:  message,
			Payload:  payload,
		}
		resp.Events = append(resp.Events, evt)
		s.events.Publish(evt)
	}
	recordAudit := func(action, target string, allowed bool, reason string, meta map[string]any) {
		resp.AuditEvents = append(resp.AuditEvents, AuditEvent{
			ID:      uuid.NewString(),
			RunID:   runID,
			Time:    time.Now(),
			Action:  action,
			Actor:   req.UserID,
			Target:  target,
			Allowed: allowed,
			Reason:  reason,
			Meta:    meta,
		})
	}
	recordEvent("run.started", "runtime", "agent run started", map[string]any{
		"agent_id":         profile.ID,
		"entrypoint_id":    inbound.Context.EntrypointID,
		"channel":          inbound.Context.Channel,
		"channel_app_id":   inbound.Context.ChannelAppID,
		"bot_id":           inbound.Context.BotID,
		"session_id":       req.SessionID,
		"agent_session_id": agentSessionID,
		"matched_by":       entrypointDecision.MatchedBy,
	})
	recordAudit("agent.run", profile.ID, true, "runtime accepted agent run", map[string]any{
		"matched_by":    entrypointDecision.MatchedBy,
		"entrypoint_id": inbound.Context.EntrypointID,
	})

	agentReq := req
	agentReq.Metadata = copyTurnMetadata(req.Metadata)
	if agentReq.Metadata == nil {
		agentReq.Metadata = map[string]string{}
	}
	agentReq.Metadata["conversation_session_id"] = req.SessionID
	agentReq.SessionID = agentSessionID
	final, toolCalls, runErr := s.generate(ctx, profile, agentReq, recordEvent, recordAudit)
	resp.FinalResponse = final
	resp.ToolCalls = toolCalls
	resp.VerificationResult = s.verifier.Verify(final, profile.Verification.DefaultChecks)
	resp.EndedAt = time.Now()
	resp.Status = "completed"
	if runErr != nil || resp.VerificationResult.Status != "passed" {
		resp.Status = "failed"
		resp.EvolutionCandidate = s.evolution.CandidateForFailure(runID, "run_failure", resp.VerificationResult, runErr, resp.EndedAt)
	}
	logAttrs := []any{
		"run_id", resp.RunID,
		"agent_id", resp.AgentID,
		"entrypoint_id", resp.EntrypointID,
		"channel", req.Channel,
		"session_id", resp.SessionID,
		"status", resp.Status,
		"verification_status", resp.VerificationResult.Status,
		"tool_calls", len(resp.ToolCalls),
		"events", len(resp.Events),
		"audit_events", len(resp.AuditEvents),
		"final_response_chars", utf8.RuneCountInString(resp.FinalResponse),
		"duration", resp.EndedAt.Sub(resp.StartedAt),
	}
	if runErr != nil {
		logAttrs = append(logAttrs, "error", runErr)
		slog.Error("agent run finished with error", logAttrs...)
	} else {
		slog.Info("agent run finished", logAttrs...)
	}
	if runErr == nil && resp.VerificationResult.Status == "passed" {
		if err := s.sessions.AppendAgentTurn(fsession.AgentTurnInput{
			SessionID:      req.SessionID,
			AgentID:        profile.ID,
			AgentSessionID: agentSessionID,
			RunID:          runID,
			Context:        inbound.Context,
			Scope:          &scope,
			UserMessage:    req.Message,
			AssistantReply: final,
		}); err != nil {
			slog.Warn("session history persistence failed",
				"run_id", runID,
				"agent_id", profile.ID,
				"session_id", req.SessionID,
				"agent_session_id", agentSessionID,
				"error", err,
			)
			recordEvent("session.persist_failed", "runtime", "session history persistence failed", map[string]any{
				"session_id":       req.SessionID,
				"agent_session_id": agentSessionID,
				"error":            err.Error(),
			})
		}
	}
	recordEvent("run.finished", "runtime", "agent run finished", map[string]any{"status": resp.Status})
	resp.Artifacts = []string{"artifacts/"}
	if err := s.runs.SaveRun(resp); err != nil && runErr == nil {
		runErr = err
	}
	return resp, runErr
}

func (s *Service) generate(
	ctx context.Context,
	profile agents.Profile,
	req TurnRequest,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) (string, []ToolCallRecord, error) {
	return s.generateADK(ctx, profile, req, recordEvent, recordAudit)
}

func copyTurnMetadata(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func (s *Service) generateNativeDeepSeek(
	ctx context.Context,
	profile agents.Profile,
	req TurnRequest,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) (string, []ToolCallRecord, error) {
	modelID := profile.ModelPolicy.Model
	if !deepseek.SupportedModel(modelID) {
		return "", nil, fmt.Errorf("unsupported model %q", modelID)
	}
	messages := []deepseek.Message{
		{Role: "system", Content: s.instructionText(profile)},
		{Role: "user", Content: req.Message},
	}
	tools := s.toolDefinitions(profile)
	temp := profile.ModelPolicy.Temp
	recordEvent("model.request", "deepseek", "sending chat completion", map[string]any{"model": modelID})
	first, err := s.deepseek.Chat(ctx, deepseek.ChatRequest{
		Model:       modelID,
		Messages:    messages,
		Tools:       tools,
		Temperature: &temp,
		Thinking:    &deepseek.Thinking{Type: "disabled"},
	})
	if err != nil {
		recordAudit("model.chat", modelID, false, err.Error(), nil)
		return "", nil, err
	}
	recordAudit("model.chat", modelID, true, "chat completion returned", nil)
	if len(first.Choices) == 0 {
		return "", nil, errors.New("deepseek returned no choices")
	}
	msg := first.Choices[0].Message
	if len(msg.ToolCalls) == 0 {
		return messageContent(msg), nil, nil
	}
	var toolRecords []ToolCallRecord
	messages = append(messages, msg)
	for _, call := range msg.ToolCalls {
		rec := s.executeToolCall(ctx, profile, call, recordEvent, recordAudit)
		toolRecords = append(toolRecords, rec)
		contentBytes, _ := json.Marshal(rec.Output)
		if rec.Error != "" {
			contentBytes, _ = json.Marshal(map[string]any{"error": rec.Error})
		}
		messages = append(messages, deepseek.Message{
			Role:       "tool",
			ToolCallID: call.ID,
			Name:       call.Function.Name,
			Content:    string(contentBytes),
		})
	}
	second, err := s.deepseek.Chat(ctx, deepseek.ChatRequest{
		Model:       modelID,
		Messages:    messages,
		Temperature: &temp,
		Thinking:    &deepseek.Thinking{Type: "disabled"},
	})
	if err != nil {
		return "", toolRecords, err
	}
	if len(second.Choices) == 0 {
		return "", toolRecords, errors.New("deepseek returned no final choices")
	}
	return messageContent(second.Choices[0].Message), toolRecords, nil
}

func (s *Service) executeToolCall(
	ctx context.Context,
	profile agents.Profile,
	call deepseek.ToolCall,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) ToolCallRecord {
	start := time.Now()
	registry := s.toolRegistry(profile)
	name := toolNameFromWire(registry, call.Function.Name)
	rec := ToolCallRecord{
		ID:        call.ID,
		Name:      name,
		Input:     map[string]any{},
		StartedAt: start,
	}
	if rec.ID == "" {
		rec.ID = uuid.NewString()
	}
	_ = json.Unmarshal([]byte(call.Function.Arguments), &rec.Input)
	if !registry.Has(rec.Name) {
		rec.Error = "tool is not allowed by agent profile"
		rec.EndedAt = time.Now()
		slog.Warn("tool call rejected",
			"agent_id", profile.ID,
			"tool", rec.Name,
			"call_id", rec.ID,
			"input", toolInputSummary(rec.Input),
			"error", rec.Error,
		)
		recordAudit("tool.call", rec.Name, false, rec.Error, rec.Input)
		return rec
	}
	slog.Info("tool call started",
		"agent_id", profile.ID,
		"tool", rec.Name,
		"call_id", rec.ID,
		"input", toolInputSummary(rec.Input),
	)
	recordEvent("tool.started", rec.Name, "tool call started", rec.Input)
	recordAudit("tool.call", rec.Name, true, "tool allowed by profile", rec.Input)
	output, err := registry.Execute(ctx, rec.Name, rec.Input)
	rec.Output = output
	rec.Error = errString(err)
	if rec.Output == nil {
		rec.Output = map[string]any{}
	}
	rec.EndedAt = time.Now()
	if rec.Error != "" {
		slog.Warn("tool call failed",
			"agent_id", profile.ID,
			"tool", rec.Name,
			"call_id", rec.ID,
			"duration", rec.EndedAt.Sub(rec.StartedAt),
			"input", toolInputSummary(rec.Input),
			"output", toolOutputSummary(rec.Output),
			"error", rec.Error,
		)
		recordEvent("tool.failed", rec.Name, rec.Error, map[string]any{"tool": rec.Name})
	} else {
		slog.Info("tool call finished",
			"agent_id", profile.ID,
			"tool", rec.Name,
			"call_id", rec.ID,
			"duration", rec.EndedAt.Sub(rec.StartedAt),
			"input", toolInputSummary(rec.Input),
			"output", toolOutputSummary(rec.Output),
		)
		recordEvent("tool.finished", rec.Name, "tool call finished", map[string]any{"tool": rec.Name})
	}
	return rec
}

func (s *Service) toolDefinitions(profile agents.Profile) []deepseek.Tool {
	defs := s.toolRegistry(profile).Definitions()
	tools := make([]deepseek.Tool, 0, len(defs))
	for _, def := range defs {
		tools = append(tools, deepseek.Tool{
			Type: "function",
			Function: deepseek.ToolFunction{
				Name:        deepseek.DeepSeekToolName(def.Name),
				Description: def.Description,
				Parameters:  def.Parameters,
			},
		})
	}
	return tools
}

func (s *Service) toolRegistry(profile agents.Profile) *rtools.Registry {
	return rtools.NewBuiltinRegistry(s.workspace, profile.Permissions.Tools)
}

func (s *Service) instructionText(profile agents.Profile) string {
	base := strings.TrimSpace(profile.InstructionText())
	tools := s.toolRegistry(profile).List()
	identity := fmt.Sprintf(
		"Current Xira agent: %s (%s).\nThis agent profile and runtime instruction are authoritative. If prior assistant messages or model defaults conflict with this agent identity, follow the current profile and correct the conflict. When asked who you are or which agent is active, answer as this Xira agent; do not identify as the underlying model provider unless the user explicitly asks about the model provider.",
		profile.ID,
		profile.Name,
	)
	var capability string
	if len(tools) == 0 {
		capability = "Available tools: none.\nOnly claim capabilities you can perform without tools."
	} else {
		capability = "Available tools: " + strings.Join(tools, ", ") + ".\nOnly claim capabilities you can perform with these tools.\nIf a needed tool is available, use it before claiming you cannot access the data. Only say a tool is unavailable or restricted when no appropriate tool exists or an attempted tool call returns an error; when that happens, mention the actual tool error."
	}
	if base == "" {
		return "# Runtime Identity\n\n" + identity + "\n\n# Runtime Capabilities\n\n" + capability
	}
	return base + "\n\n# Runtime Identity\n\n" + identity + "\n\n# Runtime Capabilities\n\n" + capability
}

func toolInputSummary(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{"action", "path", "command", "cwd", "timeout_seconds"} {
		if value, ok := input[key]; ok {
			out[key] = value
		}
	}
	if content, ok := input["content"].(string); ok {
		out["content_chars"] = utf8.RuneCountInString(content)
	}
	if oldText, ok := input["old_text"].(string); ok {
		out["old_text_chars"] = utf8.RuneCountInString(oldText)
	}
	if newText, ok := input["new_text"].(string); ok {
		out["new_text_chars"] = utf8.RuneCountInString(newText)
	}
	if len(out) == 0 {
		return input
	}
	return out
}

func toolOutputSummary(output map[string]any) map[string]any {
	if len(output) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{"path", "bytes", "replacements", "action", "command", "cwd", "exit_code", "duration_ms"} {
		if value, ok := output[key]; ok {
			out[key] = value
		}
	}
	if entries, ok := output["entries"]; ok {
		out["entries_count"] = collectionLen(entries)
	}
	for _, key := range []string{"content", "stdout", "stderr"} {
		if value, ok := output[key].(string); ok {
			out[key+"_chars"] = utf8.RuneCountInString(value)
		}
	}
	if errText, ok := output["error"].(string); ok {
		out["error"] = errText
	}
	if len(out) == 0 {
		out["keys"] = sortedAnyKeys(output)
	}
	return out
}

func collectionLen(value any) int {
	switch v := value.(type) {
	case []map[string]any:
		return len(v)
	case []any:
		return len(v)
	default:
		return 0
	}
}

func sortedAnyKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func toolNameFromWire(registry *rtools.Registry, wireName string) string {
	wireName = strings.TrimSpace(wireName)
	if registry.Has(wireName) {
		return wireName
	}
	for _, name := range registry.List() {
		if deepseek.DeepSeekToolName(name) == wireName {
			return name
		}
	}
	return wireName
}

func messageContent(msg deepseek.Message) string {
	text := deepseek.ContentText(msg.Content)
	if text != "" {
		return text
	}
	if msg.Content == nil {
		return ""
	}
	data, _ := json.Marshal(msg.Content)
	return string(data)
}

func previewText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if limit <= 0 || text == "" {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func channelConflict(requestChannel, entrypointChannel string) bool {
	requestChannel = strings.ToLower(strings.TrimSpace(requestChannel))
	entrypointChannel = strings.ToLower(strings.TrimSpace(entrypointChannel))
	if requestChannel == "" || requestChannel == "local" || entrypointChannel == "" {
		return false
	}
	return requestChannel != entrypointChannel
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
