package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	adksession "google.golang.org/adk/session"

	"github.com/ai-daming/flowdeck/internal/agents"
	"github.com/ai-daming/flowdeck/internal/channel"
	"github.com/ai-daming/flowdeck/internal/entrypoints"
	"github.com/ai-daming/flowdeck/internal/model/deepseek"
	"github.com/ai-daming/flowdeck/internal/routing"
	fsession "github.com/ai-daming/flowdeck/internal/session"
	rtools "github.com/ai-daming/flowdeck/internal/tools"
)

type Config struct {
	ConfigPath     string
	WorkspaceRoot  string
	DefaultAgentID string
	RunRoot        string
	UseMockModel   bool
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
	useMockModel  bool
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
	dsClient := cfg.DeepSeekClient
	if dsClient == nil {
		dsClient = deepseek.New()
	}
	return &Service{
		agents:        manager,
		events:        NewEventBus(),
		runs:          NewRunStore(resolved.RunRoot),
		router:        routing.NewRouterWithRules(resolved.DefaultAgentID, resolved.Routes),
		entrypoints:   entrypoints.NewRegistry(resolved.DefaultAgentID, resolved.Entrypoints),
		sessions:      fsession.NewManager(),
		adkSessions:   adksession.InMemoryService(),
		verifier:      NewVerificationRunner(),
		evolution:     NewEvolutionEngine(),
		deepseek:      dsClient,
		configPath:    resolved.ConfigPath,
		workspace:     resolved.WorkspaceRoot,
		defaultAgent:  resolved.DefaultAgentID,
		profileSource: profileSource,
		useMockModel:  cfg.UseMockModel || os.Getenv("DEEPSEEK_API_KEY") == "",
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
	return s.agents.List()
}

func (s *Service) Entrypoints() []entrypoints.Definition {
	if s == nil || s.entrypoints == nil {
		return nil
	}
	return s.entrypoints.Definitions()
}

func (s *Service) Status() map[string]any {
	return map[string]any{
		"name":           "flowdeck",
		"config_path":    s.configPath,
		"workspace":      s.workspace,
		"run_root":       s.runs.Root(),
		"agents":         len(s.Agents()),
		"entrypoints":    len(s.entrypoints.Definitions()),
		"default_agent":  s.defaultAgent,
		"profile_source": s.profileSource,
		"mock_model":     s.useMockModel,
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
	if runErr == nil && resp.VerificationResult.Status == "passed" {
		s.sessions.AppendTurn(req.SessionID, req.Message, final)
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
	if s.useMockModel {
		return s.mockGenerate(ctx, profile, req, recordEvent, recordAudit)
	}
	return s.generateADK(ctx, profile, req, recordEvent, recordAudit)
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

func (s *Service) mockGenerate(
	ctx context.Context,
	profile agents.Profile,
	req TurnRequest,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) (string, []ToolCallRecord, error) {
	lower := strings.ToLower(req.Message)
	if strings.Contains(lower, "exec") && s.toolRegistry(profile).Has("exec") {
		call := deepseek.ToolCall{
			ID:   uuid.NewString(),
			Type: "function",
			Function: deepseek.ToolCallFunction{
				Name:      "exec",
				Arguments: `{"action":"run","command":"printf 'hello from FlowDeck exec'"}`,
			},
		}
		rec := s.executeToolCall(ctx, profile, call, recordEvent, recordAudit)
		return fmt.Sprintf("Mock model used exec: %v", rec.Output["stdout"]), []ToolCallRecord{rec}, nil
	}
	return "Mock model response: " + req.Message, nil, nil
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
		recordAudit("tool.call", rec.Name, false, rec.Error, rec.Input)
		return rec
	}
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
		recordEvent("tool.failed", rec.Name, rec.Error, map[string]any{"tool": rec.Name})
	} else {
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
	var capability string
	if len(tools) == 0 {
		capability = "Available tools: none.\nOnly claim capabilities you can perform without tools."
	} else {
		capability = "Available tools: " + strings.Join(tools, ", ") + ".\nOnly claim capabilities you can perform with these tools."
	}
	if base == "" {
		return capability
	}
	return base + "\n\n# Runtime Capabilities\n\n" + capability
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
