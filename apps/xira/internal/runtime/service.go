package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	adksession "google.golang.org/adk/session"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/entrypoints"
	"github.com/xiramesh/xira/internal/model/deepseek"
	fsession "github.com/xiramesh/xira/internal/session"
	rtools "github.com/xiramesh/xira/internal/tools"
)

type Config struct {
	ConfigPath     string
	WorkspaceRoot  string
	DefaultAgentID string
	RunRoot        string
	SessionRoot    string
	StateRoot      string
	DeepSeekClient *deepseek.Client
}

type Service struct {
	agents         *agents.Manager
	events         *EventBus
	runs           *RunStore
	entrypoints    *entrypoints.Registry
	sessions       *fsession.Manager
	usage          *UsageStore
	adkSessions    adksession.Service
	verifier       *VerificationRunner
	evolution      *EvolutionEngine
	deepseek       *deepseek.Client
	configPath     string
	workspace      string
	stateRoot      string
	defaultAgent   string
	profileSource  string
	pricing        UsagePricing
	delegationMu   sync.Mutex
	activeChildren map[string]int
	mu             sync.RWMutex
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
		agents:         manager,
		events:         NewEventBus(),
		runs:           NewRunStore(resolved.RunRoot),
		entrypoints:    entrypoints.NewRegistry(resolved.DefaultAgentID, resolved.Entrypoints),
		sessions:       sessionManager,
		usage:          NewUsageStore(resolved.StateRoot),
		adkSessions:    adksession.InMemoryService(),
		verifier:       NewVerificationRunner(),
		evolution:      NewEvolutionEngine(),
		deepseek:       dsClient,
		configPath:     resolved.ConfigPath,
		workspace:      resolved.WorkspaceRoot,
		stateRoot:      resolved.StateRoot,
		defaultAgent:   resolved.DefaultAgentID,
		profileSource:  profileSource,
		pricing:        resolved.Pricing,
		activeChildren: map[string]int{},
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

func (s *Service) StateRoot() string {
	if s == nil {
		return ""
	}
	return s.stateRoot
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

func (s *Service) AgentSummaries() []ModelPolicySnapshot {
	if s == nil {
		return nil
	}
	profiles := s.Agents()
	out := make([]ModelPolicySnapshot, 0, len(profiles))
	for _, profile := range profiles {
		out = append(out, modelPolicySnapshot(profile, s.profileSource))
	}
	return out
}

func (s *Service) AgentRegistry() []AgentRegistryEntry {
	if s == nil {
		return nil
	}
	profiles := s.Agents()
	out := make([]AgentRegistryEntry, 0, len(profiles))
	for _, profile := range profiles {
		out = append(out, AgentRegistryEntry{
			ID:            profile.ID,
			Name:          profile.Name,
			Version:       profile.Version,
			Description:   profile.Description,
			ProfileSource: s.profileSource,
			Installed:     true,
			Valid:         true,
			Enabled:       true,
			Discoverable:  true,
			Tools:         s.toolRegistry(profile).List(),
			InputSchema:   "delegate_task_v1",
			OutputSchema:  "delegate_result_v1",
		})
	}
	return out
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
		"state_root":     s.stateRoot,
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
	adkRuntimeSessionID := adkSessionID(agentSessionID, runID+":"+uuid.NewString())
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
		ModelPolicy:    modelPolicySnapshot(profile, s.profileSource),
		Message:        req.Message,
		Status:         "running",
		StartedAt:      now,
		Metadata:       req.Metadata,
	}
	eventBase := runtimeEventBase{
		RunID:                 runID,
		AgentID:               profile.ID,
		EntrypointID:          inbound.Context.EntrypointID,
		Channel:               inbound.Context.Channel,
		Account:               inbound.Context.Account,
		ChannelAppID:          inbound.Context.ChannelAppID,
		BotID:                 inbound.Context.BotID,
		ConversationSessionID: req.SessionID,
		AgentSessionID:        agentSessionID,
		ChatID:                inbound.Context.ChatID,
		ChatType:              inbound.Context.ChatType,
		TopicID:               inbound.Context.TopicID,
		SpaceID:               inbound.Context.SpaceID,
		SpaceType:             inbound.Context.SpaceType,
		SenderID:              inbound.Context.SenderID,
		MessageID:             inbound.Context.MessageID,
		ReplyToMessageID:      inbound.Context.ReplyToMessageID,
		ReplyToSenderID:       inbound.Context.ReplyToSenderID,
		TraceID:               runID,
	}
	recordEvent := func(kind, source, message string, payload map[string]any) {
		evt := newRuntimeEvent(eventBase, kind, source, message, payload, nil)
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
		"adk_session_id":   adkRuntimeSessionID,
		"matched_by":       entrypointDecision.MatchedBy,
	})
	recordAudit("agent.run", profile.ID, true, "runtime accepted agent run", map[string]any{
		"matched_by":    entrypointDecision.MatchedBy,
		"entrypoint_id": inbound.Context.EntrypointID,
	})
	recordEvent("model.policy_resolved", "runtime", "model policy resolved", map[string]any{
		"provider":         resp.ModelPolicy.Provider,
		"model":            resp.ModelPolicy.Model,
		"stream":           resp.ModelPolicy.Stream,
		"temperature":      resp.ModelPolicy.Temperature,
		"thinking_type":    resp.ModelPolicy.ThinkingType,
		"tools":            resp.ModelPolicy.Tools,
		"profile_source":   resp.ModelPolicy.ProfileSource,
		"instruction_hash": resp.ModelPolicy.InstructionHash,
	})

	agentReq := req
	agentReq.Metadata = copyTurnMetadata(req.Metadata)
	if agentReq.Metadata == nil {
		agentReq.Metadata = map[string]string{}
	}
	agentReq.Metadata["conversation_session_id"] = req.SessionID
	agentReq.Metadata["agent_session_id"] = agentSessionID
	agentReq.SessionID = adkRuntimeSessionID
	if err := s.runs.InitRun(runID); err != nil {
		return TurnResponse{}, err
	}
	ctx = contextWithToolFailureGuard(ctx)
	ctx = contextWithToolTrace(ctx, runID)
	ctx = contextWithRunExecution(ctx, runExecutionContext{
		Base:        eventBase,
		Profile:     profile,
		Request:     req,
		UserMessage: req.Message,
	})
	ctx = rtools.WithRunDir(ctx, s.runs.RunDir(runID))
	ctx = s.withLLMInstrumentation(ctx, llmInstrumentationInput{
		RunID:          runID,
		AgentID:        profile.ID,
		EntrypointID:   inbound.Context.EntrypointID,
		Channel:        inbound.Context.Channel,
		SessionID:      req.SessionID,
		AgentSessionID: agentSessionID,
		ADKSessionID:   adkRuntimeSessionID,
		UserID:         req.UserID,
		Pricing:        s.pricing,
	}, recordEvent, func(call LLMCallRecord) {
		resp.LLMCalls = append(resp.LLMCalls, call)
	})
	final, toolCalls, runErr := s.generate(ctx, profile, agentReq, recordEvent, recordAudit)
	resp.FinalResponse = final
	resp.ToolCalls = toolCalls
	resp.VerificationResult = s.verifier.Verify(final, profile.Verification.DefaultChecks)
	resp.EndedAt = time.Now()
	resp.Usage = summarizeUsage(resp)
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
		"llm_calls", len(resp.LLMCalls),
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"total_tokens", resp.Usage.TotalTokens,
		"final_response_chars", utf8.RuneCountInString(resp.FinalResponse),
		"duration", resp.EndedAt.Sub(resp.StartedAt),
	}
	if resp.Usage.Cost != nil {
		logAttrs = append(logAttrs, "cost", *resp.Usage.Cost, "currency", resp.Usage.Currency)
	}
	if runErr != nil {
		logAttrs = append(logAttrs, "error", runErr)
		slog.Error("agent run finished with error", logAttrs...)
	} else {
		slog.Info("agent run finished", logAttrs...)
	}
	if runErr == nil && resp.VerificationResult.Status == "passed" {
		recordEvent("assistant.final", "runtime", "assistant final response", map[string]any{
			"response_chars": utf8.RuneCountInString(final),
		})
		sessionTurn := fsession.AgentTurnInput{
			SessionID:      req.SessionID,
			AgentID:        profile.ID,
			AgentSessionID: agentSessionID,
			RunID:          runID,
			Context:        inbound.Context,
			Scope:          &scope,
			UserMessage:    req.Message,
			AssistantReply: final,
		}
		if err := s.sessions.AppendAgentMessages(sessionTurn, sessionMessagesForRun(req.Message, final, profile.ID, runID, toolCalls)); err != nil {
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
		} else {
			messagesPath := s.sessions.AgentMessagesPath(sessionTurn)
			slog.Info("session history persisted",
				"run_id", runID,
				"agent_id", profile.ID,
				"session_id", req.SessionID,
				"agent_session_id", agentSessionID,
				"messages_path", messagesPath,
			)
			recordEvent("session.persisted", "runtime", "session history persisted", map[string]any{
				"session_id":       req.SessionID,
				"agent_session_id": agentSessionID,
				"messages_path":    messagesPath,
			})
		}
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
		recordEvent("llm.usage_summary", "runtime", "llm usage summarized", payload)
		if s.usage != nil {
			if err := s.usage.AppendCalls(resp.LLMCalls); err != nil {
				slog.Warn("usage ledger append failed", "run_id", runID, "error", err)
				recordEvent("usage.ledger_failed", "runtime", "usage ledger append failed", map[string]any{"error": err.Error()})
			} else {
				recordEvent("usage.ledger_appended", "runtime", "usage ledger appended", map[string]any{
					"calls": len(resp.LLMCalls),
					"path":  filepathJoinSlash(s.usage.Root(), "usage-ledger.jsonl"),
				})
			}
		}
	}
	recordEvent("run.finished", "runtime", "agent run finished", map[string]any{
		"status":              resp.Status,
		"verification_status": resp.VerificationResult.Status,
	})
	resp.Artifacts = []string{"artifacts/"}
	if err := s.runs.SaveRun(resp); err != nil && runErr == nil {
		runErr = err
	}
	return resp, runErr
}

func adkSessionID(agentSessionID, runID string) string {
	agentSessionID = strings.TrimSpace(agentSessionID)
	runID = strings.TrimSpace(runID)
	if agentSessionID == "" {
		agentSessionID = "session:unknown"
	}
	if runID == "" {
		return agentSessionID
	}
	return agentSessionID + ":run:" + runID
}

type toolFailureGuardContextKey struct{}
type toolTraceContextKey struct{}

type toolFailureGuard struct {
	mu       sync.Mutex
	lastKey  string
	failures int
}

type toolTraceContext struct {
	runID string
}

func contextWithToolFailureGuard(ctx context.Context) context.Context {
	return context.WithValue(ctx, toolFailureGuardContextKey{}, &toolFailureGuard{})
}

func contextWithToolTrace(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, toolTraceContextKey{}, toolTraceContext{runID: strings.TrimSpace(runID)})
}

func toolFailureGuardFromContext(ctx context.Context) *toolFailureGuard {
	guard, _ := ctx.Value(toolFailureGuardContextKey{}).(*toolFailureGuard)
	return guard
}

func toolTraceFromContext(ctx context.Context) toolTraceContext {
	trace, _ := ctx.Value(toolTraceContextKey{}).(toolTraceContext)
	return trace
}

func (g *toolFailureGuard) shouldBlock(name string, input map[string]any) bool {
	if g == nil {
		return false
	}
	key := toolFailureKey(name, input)
	if key == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastKey == key && g.failures >= 2
}

func (g *toolFailureGuard) record(name string, input map[string]any, failed bool) {
	if g == nil {
		return
	}
	key := toolFailureKey(name, input)
	if key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !failed {
		g.lastKey = ""
		g.failures = 0
		return
	}
	if g.lastKey == key {
		g.failures++
		return
	}
	g.lastKey = key
	g.failures = 1
}

func toolFailureKey(name string, input map[string]any) string {
	name = strings.TrimSpace(name)
	cwd := mapString(input, "cwd")
	switch name {
	case "shell.run":
		command := mapString(input, "command")
		if command == "" {
			return ""
		}
		return name + "\x00" + cwd + "\x00" + command
	case "command.run":
		program := mapString(input, "program")
		if program == "" {
			return ""
		}
		args, _ := json.Marshal(input["args"])
		return name + "\x00" + cwd + "\x00" + program + "\x00" + string(args)
	default:
		return ""
	}
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
	recordEvent("model.request", "deepseek", "sending chat completion", map[string]any{"model": modelID})
	first, err := s.deepseek.Chat(ctx, deepseek.ChatRequest{
		Model:       modelID,
		Messages:    messages,
		Tools:       tools,
		Temperature: cloneFloat32(profile.ModelPolicy.Temp),
		Thinking:    &deepseek.Thinking{Type: thinkingType(profile.ModelPolicy)},
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
		contentBytes, _ := json.Marshal(toolOutputForModel(rec))
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
		Temperature: cloneFloat32(profile.ModelPolicy.Temp),
		Thinking:    &deepseek.Thinking{Type: thinkingType(profile.ModelPolicy)},
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
	recordEvent("tool.started", rec.Name, "tool call started", map[string]any{
		"tool":  rec.Name,
		"input": rec.Input,
	})
	recordAudit("tool.call", rec.Name, true, "tool allowed by profile", rec.Input)
	var output map[string]any
	var err error
	if guard := toolFailureGuardFromContext(ctx); guard != nil && guard.shouldBlock(rec.Name, rec.Input) {
		output = repeatedToolFailureOutput(rec.Name, rec.Input)
		err = errors.New("repeated identical failed tool command")
	} else {
		output, err = registry.Execute(ctx, rec.Name, rec.Input)
	}
	rec.Output = output
	rec.Error = errString(err)
	if rec.Output == nil {
		rec.Output = map[string]any{}
	}
	rec.RunID = toolTraceFromContext(ctx).runID
	rec.EndedAt = time.Now()
	if path, persistErr := s.persistRawToolOutput(ctx, rec); persistErr != nil {
		rec.Output["raw_output_error"] = persistErr.Error()
		recordEvent("tool.raw_output_failed", rec.Name, "failed to persist raw tool output", map[string]any{
			"tool":  rec.Name,
			"error": persistErr.Error(),
		})
	} else if path != "" {
		rec.Output["raw_output_path"] = path
		recordEvent("tool.raw_output_persisted", rec.Name, "raw tool output persisted", map[string]any{
			"tool":            rec.Name,
			"raw_output_path": path,
		})
	}
	if guard := toolFailureGuardFromContext(ctx); guard != nil {
		guard.record(rec.Name, rec.Input, rec.Error != "")
	}
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
		recordEvent("tool.completed", rec.Name, "tool call completed", map[string]any{"tool": rec.Name})
		recordEvent("tool.finished", rec.Name, "tool call finished", map[string]any{"tool": rec.Name})
	}
	return rec
}

func (s *Service) persistRawToolOutput(ctx context.Context, rec ToolCallRecord) (string, error) {
	if s == nil || s.runs == nil || rec.Output == nil {
		return "", nil
	}
	if rec.Name != "command.run" && rec.Name != "shell.run" {
		return "", nil
	}
	trace := toolTraceFromContext(ctx)
	runID := strings.TrimSpace(trace.runID)
	if runID == "" {
		return "", nil
	}
	stdout, hasStdout := rec.Output["stdout"].(string)
	stderr, hasStderr := rec.Output["stderr"].(string)
	if !hasStdout && !hasStderr {
		return "", nil
	}
	fileName := safeToolOutputFileName(rec.ID)
	relPath := filepath.ToSlash(filepath.Join("artifacts", "tool-outputs", fileName+".json"))
	absPath := filepath.Join(s.runs.RunDir(runID), filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return "", err
	}
	cwd := mapString(rec.Output, "cwd")
	if cwd == "" {
		cwd = mapString(rec.Input, "cwd")
	}
	payload := map[string]any{
		"run_id":           runID,
		"tool_call_id":     rec.ID,
		"tool":             rec.Name,
		"input":            rec.Input,
		"cwd":              cwd,
		"env_policy":       "process environment inherited; per-tool env overrides are not supported",
		"stdout":           stdout,
		"stderr":           stderr,
		"stdout_bytes":     streamBytes(rec.Output, "stdout", stdout),
		"stderr_bytes":     streamBytes(rec.Output, "stderr", stderr),
		"stdout_truncated": false,
		"stderr_truncated": false,
		"truncated":        false,
		"exit_code":        rec.Output["exit_code"],
		"duration_ms":      rec.Output["duration_ms"],
		"started_at":       rec.StartedAt,
		"ended_at":         rec.EndedAt,
	}
	if rec.Name == "command.run" {
		payload["program"] = rec.Output["program"]
		payload["args"] = rec.Output["args"]
	} else {
		payload["command"] = rec.Output["command"]
	}
	if rec.Error != "" {
		payload["error"] = rec.Error
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(absPath, data, 0o644); err != nil {
		return "", err
	}
	return relPath, nil
}

func safeToolOutputFileName(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return uuid.NewString()
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return uuid.NewString()
	}
	return out
}

func streamBytes(output map[string]any, streamKey, value string) int {
	if n, ok := intFromAny(output[streamKey+"_bytes"]); ok {
		return n
	}
	return len([]byte(value))
}

func repeatedToolFailureOutput(name string, input map[string]any) map[string]any {
	out := map[string]any{
		"status":        "error",
		"tool":          name,
		"cwd":           mapString(input, "cwd"),
		"exit_code":     nil,
		"stdout":        "",
		"stderr":        "",
		"stdout_bytes":  0,
		"stderr_bytes":  0,
		"error":         "repeated identical failed tool command",
		"error_message": "repeated identical failed tool command",
		"retryable":     false,
	}
	if name == "command.run" {
		out["program"] = mapString(input, "program")
		out["args"] = input["args"]
	} else {
		out["command"] = mapString(input, "command")
	}
	return out
}

func mapString(input map[string]any, key string) string {
	if input == nil {
		return ""
	}
	value, ok := input[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func sessionMessagesForRun(userMessage, finalResponse, agentID, runID string, toolCalls []ToolCallRecord) []fsession.Message {
	messages := []fsession.Message{
		{
			Role:    "user",
			Kind:    fsession.MessageKindMessage,
			Content: strings.TrimSpace(userMessage),
			AgentID: agentID,
			RunID:   runID,
		},
	}
	for _, rec := range toolCalls {
		messages = append(messages, fsession.Message{
			Role:       "assistant",
			Kind:       fsession.MessageKindToolCall,
			Content:    mustJSON(rec.Input),
			ToolCallID: rec.ID,
			ToolName:   rec.Name,
			AgentID:    agentID,
			RunID:      runID,
		})
		messages = append(messages, fsession.Message{
			Role:       "tool",
			Kind:       fsession.MessageKindToolResult,
			Content:    mustJSON(toolOutputForModel(rec)),
			ToolCallID: rec.ID,
			ToolName:   rec.Name,
			AgentID:    agentID,
			RunID:      runID,
		})
	}
	messages = append(messages, fsession.Message{
		Role:    "assistant",
		Kind:    fsession.MessageKindMessage,
		Content: strings.TrimSpace(finalResponse),
		AgentID: agentID,
		RunID:   runID,
	})
	return messages
}

func toolOutputForModel(rec ToolCallRecord) map[string]any {
	out := map[string]any{}
	for key, value := range rec.Output {
		if key == "stdout" || key == "stderr" {
			continue
		}
		out[key] = value
	}
	applyStreamPreview(out, rec.Output, rec.Input, "stdout", "max_stdout_bytes", 8192)
	applyStreamPreview(out, rec.Output, rec.Input, "stderr", "max_stderr_bytes", 4096)
	if _, ok := out["raw_output_path"]; ok && (boolFromAny(out["stdout_truncated"]) || boolFromAny(out["stderr_truncated"])) {
		out["raw_output_hint"] = "Use tool_output.read with raw_output_path and stream=stdout or stderr to inspect more of the full output."
	}
	if rec.Error != "" {
		if _, ok := out["status"]; !ok {
			out["status"] = "error"
		}
		if _, ok := out["error"]; !ok {
			out["error"] = rec.Error
		}
		if _, ok := out["error_message"]; !ok {
			out["error_message"] = rec.Error
		}
		if _, ok := out["retryable"]; !ok {
			out["retryable"] = true
		}
	} else if _, ok := out["status"]; !ok {
		out["status"] = "ok"
	}
	return out
}

func boolFromAny(value any) bool {
	v, _ := value.(bool)
	return v
}

func applyStreamPreview(out, output, input map[string]any, streamKey, maxKey string, defaultBytes int) {
	text, ok := output[streamKey].(string)
	if !ok {
		return
	}
	maxBytes := defaultBytes
	if raw, ok := intFromAny(input[maxKey]); ok && raw > 0 {
		maxBytes = raw
	}
	bytesLen := len([]byte(text))
	preview := text
	truncated := false
	if bytesLen > maxBytes {
		preview = string([]byte(text)[:maxBytes])
		truncated = true
	}
	if _, ok := out[streamKey+"_bytes"]; !ok {
		out[streamKey+"_bytes"] = bytesLen
	}
	out[streamKey+"_preview"] = preview
	out[streamKey+"_truncated"] = truncated
	if truncated {
		out["truncated"] = true
	} else if _, ok := out["truncated"]; !ok {
		out["truncated"] = false
	}
}

func intFromAny(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
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
	for _, key := range []string{"path", "root", "query", "program", "command", "cwd", "timeout_seconds", "max_stdout_bytes", "max_stderr_bytes", "max_results"} {
		if value, ok := input[key]; ok {
			out[key] = value
		}
	}
	if args, ok := input["args"].([]any); ok {
		out["args_count"] = len(args)
	} else if args, ok := input["args"].([]string); ok {
		out["args_count"] = len(args)
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
	for _, key := range []string{"path", "root", "bytes", "replacements", "tool", "program", "command", "cwd", "exit_code", "duration_ms", "stdout_bytes", "stderr_bytes", "match_count", "total_matches", "searched_files", "skipped_files", "truncated"} {
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
