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
	"github.com/xiramesh/xira/internal/flow"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
	fsession "github.com/xiramesh/xira/internal/session"
	"github.com/xiramesh/xira/internal/skills"
	rtools "github.com/xiramesh/xira/internal/tools"
)

type Config struct {
	ConfigPath     string
	WorkspaceRoot  string
	DefaultAgentID string
	StateDir       string
	DeepSeekClient *deepseek.Client
}

type Service struct {
	agents        *agents.Manager
	flows         *flow.FlowRegistry
	skills        *skills.Manager
	runs          *RunStore
	entrypoints   *entrypoints.Registry
	sessions      *fsession.Manager
	usage         *UsageStore
	humanRequests *humanrequest.Store
	// ownerBindings persists /bind-established ownership (dynamic, overrides
	// static Definition.OwnerID in IsOwner). bindCodes holds the one-time
	// device codes generated at startup for entrypoints without an owner yet.
	ownerBindings *ownerBindingStore
	bindCodes     map[string]string
	bindCodesMu   sync.Mutex // guards bindCodes (concurrent delete on bind)
	// outbound delivers resumed-run final responses back to the originating IM
	// channel (RFC #27 — stateless HITL resume). Injected by main.go as the
	// channel Manager (an OutboundEmitter). nil = resume finals are not
	// delivered to IM (they're still persisted in the run; backward-compatible
	// for tests/CLI without a channel manager).
	outbound       channel.OutboundEmitter
	adkSessions    adksession.Service
	verifier       verifier
	evolution      *EvolutionEngine
	deepseek       *deepseek.Client
	configPath     string
	workspace      string
	stateDir       string
	defaultAgent   string
	profileSource  string
	pricing        UsagePricing
	delegationMu   sync.Mutex
	activeChildren map[string]int
	flowKernel     *flow.Kernel
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
	skillManager, err := skills.LoadFromWorkspace(resolved.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	flowRegistry, err := flow.LoadFromWorkspace(resolved.WorkspaceRoot)
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
	warnIfSplitStateDirs(resolved)
	svc := &Service{
		agents:         manager,
		flows:          flowRegistry,
		skills:         skillManager,
		runs:           NewRunStore(resolved.RunRoot),
		entrypoints:    entrypoints.NewRegistry(resolved.DefaultAgentID, resolved.Entrypoints),
		sessions:       sessionManager,
		usage:          NewUsageStore(resolved.StateDir),
		humanRequests:  mustHumanRequestStore(resolved.StateDir),
		ownerBindings:  newOwnerBindingStore(resolved.StateDir),
		bindCodes:      map[string]string{},
		adkSessions:    adksession.InMemoryService(),
		verifier:       NewVerificationRunner(),
		evolution:      NewEvolutionEngine(),
		deepseek:       dsClient,
		configPath:     resolved.ConfigPath,
		workspace:      resolved.WorkspaceRoot,
		stateDir:       resolved.StateDir,
		defaultAgent:   resolved.DefaultAgentID,
		profileSource:  profileSource,
		pricing:        resolved.Pricing,
		activeChildren: map[string]int{},
	}
	// #123: 为每个尚未绑定 owner 的 entrypoint 生成一次性 device code，
	// 打印到 stdout（不进 slog，避免被日志收集系统归档）。
	svc.generateAndAnnounceBindCodes()
	// Operational visibility (#72 item 3): log pending HumanRequests at startup
	// so an operator restarting the process sees unresolved HITL. Best-effort —
	// a scan failure is warned, never blocks startup. No notify/timeout cleanup
	// here (those are bigger product decisions); HITL resume itself is
	// request-driven and works without this scan (run + request persist to disk).
	svc.logPendingHumanRequestsAtStartup()
	return svc, nil
}

// logPendingHumanRequestsAtStartup scans the HumanRequest store for pending
// requests and logs the count. Operational visibility only — does NOT notify
// users or auto-resolve. Safe to call once during NewService.
func (s *Service) logPendingHumanRequestsAtStartup() {
	if s == nil || s.humanRequests == nil {
		return
	}
	pending, err := s.humanRequests.List(context.Background(), humanrequest.ListQuery{
		WorkspaceKey: s.WorkspaceKey(),
		Status:       humanrequest.StatusPending,
	})
	if err != nil {
		slog.Warn("startup human request scan failed (non-fatal)", "error", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	slog.Info("human requests pending at startup (waiting for resolution)",
		"count", len(pending))
}

// SetOutboundEmitter injects the channel outbound emitter (the channel Manager).
// The resume path uses it to deliver a resumed run's final response back to the
// originating IM channel (RFC #27 — stateless HITL resume). Optional: when nil
// (tests, CLI without channels), resume finals are persisted but not pushed to IM.
func (s *Service) SetOutboundEmitter(e channel.OutboundEmitter) {
	if s == nil {
		return
	}
	s.outbound = e
}

func (s *Service) Close() {
}

func (s *Service) RunStore() *RunStore {
	return s.runs
}

func (s *Service) StateRoot() string {
	return s.StateDir()
}

// StateDir returns the resolved runtime state directory. It contains runs,
// sessions, flow runs, usage ledger, channel state, and workspace-keyed HITL
// state. StateRoot is kept as a temporary compatibility alias for internal
// callers that have not been renamed yet.
func (s *Service) StateDir() string {
	if s == nil {
		return ""
	}
	return s.stateDir
}

func warnIfSplitStateDirs(resolved resolvedRuntimeConfig) {
	legacyDir, stateDir, ok := splitStateDirs(resolved)
	if !ok {
		return
	}
	slog.Warn("legacy repo-root .xira and workspace state_dir both exist; Xira will use state_dir and will not migrate old state automatically",
		"legacy_state_dir", legacyDir,
		"state_dir", stateDir,
	)
}

func splitStateDirs(resolved resolvedRuntimeConfig) (string, string, bool) {
	if !resolved.ConfigLoaded {
		return "", "", false
	}
	legacyDir := filepath.Join(filepath.Dir(resolved.ConfigPath), ".xira")
	if samePath(legacyDir, resolved.StateDir) {
		return "", "", false
	}
	legacyExists, err := dirExists(legacyDir)
	if err != nil || !legacyExists {
		return "", "", false
	}
	stateExists, err := dirExists(resolved.StateDir)
	if err != nil || !stateExists {
		return "", "", false
	}
	return legacyDir, resolved.StateDir, true
}

func dirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.IsDir(), nil
}

func samePath(left, right string) bool {
	if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return false
	}
	return filepath.Clean(left) == filepath.Clean(right)
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
		out = append(out, s.modelPolicySnapshot(profile))
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
			Skills:        compactProfileSkills(profile.Skills),
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

// IsOwner reports whether senderID is the owner of entrypointID.
//
// 查询顺序（#123 契约变更）：
//  1. 先查运行时动态绑定（/bind 建立的，ownerBindings）——命中则用动态 owner。
//  2. 否则 fallback 静态配置（Definition.OwnerID，#122 原行为，向后兼容）。
//
// 动态绑定优先于静态：/bind 绑定后，即使 yaml 配了 owner: 也以运行时绑定的为准。
// 空 OwnerID（A 配置）且无动态绑定 → 对任何 sender 返回 false。
//
// coverage: contract (100% required) — owner determination is a security gate.
func (s *Service) IsOwner(_ context.Context, senderID, entrypointID string) bool {
	if s == nil {
		return false
	}
	senderID = strings.TrimSpace(senderID)
	entrypointID = strings.TrimSpace(entrypointID)
	if senderID == "" || entrypointID == "" {
		return false
	}
	// ① 动态绑定优先（#123）。
	if s.ownerBindings != nil {
		if b, ok := s.ownerBindings.Get(entrypointID); ok {
			return b.OwnerSenderID == senderID // strict equality, NOT glob
		}
	}
	// ② fallback 静态配置（#122 原行为）。
	if s.entrypoints == nil {
		return false
	}
	def, ok := s.entrypoints.Definition(entrypointID)
	if !ok {
		return false
	}
	return def.OwnerID == senderID // strict equality, NOT glob (identity, not pattern)
}

func (s *Service) Status() map[string]any {
	return map[string]any{
		"name":           "xira",
		"config_path":    s.configPath,
		"workspace":      s.workspace,
		"run_root":       s.runs.Root(),
		"session_root":   s.sessions.Root(),
		"state_dir":      s.stateDir,
		"agents":         len(s.Agents()),
		"entrypoints":    len(s.entrypoints.Definitions()),
		"default_agent":  s.defaultAgent,
		"profile_source": s.profileSource,
	}
}

func (s *Service) RunAgent(ctx context.Context, req TurnRequest) (TurnResponse, error) {
	now := time.Now()
	// req.Context is the single source of truth for session identity. We enrich
	// it from the resolved entrypoint (channel/account/app/bot defaults) and use
	// it directly — no flattened Channel/UserID/Metadata reassembly.
	req.Context = channel.NormalizeInboundContext(req.Context)
	inbound := channel.InboundEnvelope{
		Context:            req.Context,
		Content:            req.Message,
		RequestedAgentID:   req.AgentID,
		SessionIDOverride:  req.SessionID,
		Metadata:           req.Context.Raw,
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
	if channelConflict(req.Context.Channel, entrypointDecision.Definition.Channel) {
		return TurnResponse{}, fmt.Errorf("entrypoint %q uses channel %q, got request channel %q", entrypointDecision.Definition.ID, entrypointDecision.Definition.Channel, req.Context.Channel)
	}
	req.Context.Channel = entrypointDecision.Definition.Channel
	req.Context.EntrypointID = entrypointDecision.Definition.ID
	if req.Context.Account == "" {
		req.Context.Account = entrypointDecision.Definition.Account
	}
	if req.Context.ChannelAppID == "" {
		req.Context.ChannelAppID = entrypointDecision.Definition.AppID
	}
	if req.Context.BotID == "" {
		req.Context.BotID = entrypointDecision.Definition.BotID
	}
	req.Context = channel.NormalizeInboundContext(req.Context)
	inbound.Context = req.Context
	// #123: owner 绑定拦截。命中 "/bind <code>" 走绑定流程，不进 agent turn。
	// 此处位于 entrypointDecision 之后（拿到 entrypointID + senderID）、agents.Get 之前
	// （绕过 skill 激活 / session 分配 / usage / runs 等所有副作用）。返回的 FinalResponse
	// 由 ChatKeySession.runTurn 的现有 SendFinal 路径发回 IM（不改 ChatKeySession）。
	if token, ok := parseBindCommand(req.Message); ok {
		msg := s.handleOwnerBind(entrypointDecision.Definition.ID, req.Context.SenderID, token)
		return TurnResponse{FinalResponse: msg, Status: "completed"}, nil
	}
	profile, ok := s.agents.Get(entrypointDecision.AgentID)
	if !ok {
		return TurnResponse{}, fmt.Errorf("agent profile %q not found", entrypointDecision.AgentID)
	}
	runInstruction, activeSkillIDs, err := s.instructionTextForRun(profile, req.Context)
	if err != nil {
		return TurnResponse{}, err
	}
	sessionPolicy := sessionPolicyForProfile(profile, entrypointDecision.SessionPolicy)
	allocation := s.sessions.Allocate(fsession.AllocationInput{
		Context:           req.Context,
		SessionPolicy:     sessionPolicy,
		SessionIDOverride: inbound.SessionIDOverride,
	})
	req.AgentID = profile.ID
	req.EntrypointID = req.Context.EntrypointID
	req.SessionID = allocation.SessionID
	agentSessionID := fsession.BuildAgentSessionID(req.SessionID, profile.ID)
	runID := NewRunID(profile.ID, req.Context.Channel, now)
	adkRuntimeSessionID := adkSessionID(agentSessionID, runID+":"+uuid.NewString())
	scope := allocation.Scope
	slog.Info("agent run accepted",
		"run_id", runID,
		"agent_id", profile.ID,
		"entrypoint_id", inbound.Context.EntrypointID,
		"channel", inbound.Context.Channel,
		"channel_app_id", inbound.Context.ChannelAppID,
		"bot_id", inbound.Context.BotID,
		"user_id", req.Context.SenderID,
		"session_id", req.SessionID,
		"agent_session_id", agentSessionID,
		"matched_by", entrypointDecision.MatchedBy,
		"message_chars", utf8.RuneCountInString(req.Message),
		"message_preview", previewText(req.Message, 120),
	)
	resp := TurnResponse{
		RunID:           runID,
		AgentID:         profile.ID,
		EntrypointID:    inbound.Context.EntrypointID,
		SessionID:       req.SessionID,
		SessionScope:    &scope,
		RouteMatchedBy:  entrypointDecision.MatchedBy,
		ModelPolicy:     s.modelPolicySnapshotForRun(profile, runInstruction, activeSkillIDs),
		ExecutionPolicy: executionPolicySnapshotFromRequest(req),
		Message:         req.Message,
		Status:          "running",
		StartedAt:       now,
		Metadata:        req.Context.Raw,
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
		ChatName:              inbound.Context.ChatName,
		TopicID:               inbound.Context.TopicID,
		SpaceID:               inbound.Context.SpaceID,
		SpaceType:             inbound.Context.SpaceType,
		SenderID:              inbound.Context.SenderID,
		SenderName:            inbound.Context.SenderName,
		MessageID:             inbound.Context.MessageID,
		ReplyToMessageID:      inbound.Context.ReplyToMessageID,
		ReplyToSenderID:       inbound.Context.ReplyToSenderID,
		TraceID:               runID,
	}
	recorder := newRunRecorder(&resp)
	recordEvent := func(kind, source, message string, payload map[string]any) {
		evt := newRuntimeEvent(eventBase, kind, source, message, payload, nil)
		recorder.appendEvent(evt)
		// Per-chat-key delivery (RFC #48): if an EventBus is in the context,
		// deliver mapped Events directly to it. There is no global bus fallback;
		// dispatchEvent logs signal drops when the ctx has no sink.
		dispatchEvent(ctx, evt)
	}
	recordAudit := func(action, target string, allowed bool, reason string, meta map[string]any) {
		recorder.appendAudit(AuditEvent{
			ID:      uuid.NewString(),
			RunID:   runID,
			Time:    time.Now(),
			Action:  action,
			Actor:   req.Context.SenderID,
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
	// model.policy_resolved removed (#43): ModelPolicy is persisted in run.yaml;
	// the runtime event was a redundant notification.

	agentReq := req
	// Runtime-internal correlation keys live on Context.Raw (the InboundContext
	// equivalent of the former Metadata map). They let delegation/HITL tool
	// scopes correlate child runs back to this conversation session.
	agentReq.Context.Raw = copyTurnMetadata(req.Context.Raw)
	if agentReq.Context.Raw == nil {
		agentReq.Context.Raw = map[string]string{}
	}
	agentReq.Context.Raw["conversation_session_id"] = req.SessionID
	agentReq.Context.Raw["agent_session_id"] = agentSessionID
	agentReq.SessionID = adkRuntimeSessionID
	if err := s.runs.InitRun(runID); err != nil {
		return TurnResponse{}, err
	}
	ctx = contextWithToolFailureGuard(ctx)
	ctx = contextWithToolTrace(ctx, runID)
	if req.AllowedToolsSet || len(req.AllowedTools) > 0 {
		ctx = contextWithRuntimeToolAllowlist(ctx, req.AllowedTools)
	}
	ctx = contextWithRuntimeToolInputAllowlist(ctx, req.ToolInputAllowlist)
	suspendCollector := newRuntimeSuspendCollector()
	ctx = contextWithRuntimeSuspendCollector(ctx, suspendCollector)
	ctx = contextWithRunExecution(ctx, runExecutionContext{
		Base:        eventBase,
		Profile:     profile,
		Request:     req,
		UserMessage: req.Message,
	})
	// Carry the chatKey through ctx so spawned children (spawn_turn.go) can
	// register themselves with the per-chat-key cancel registry and be
	// canceled when this turn is steered (RFC #67).
	chatKey := ChatKeyFromInbound(req.Context)
	chatKey.DataIsolation = entrypointDecision.Definition.DataIsolation.Enabled // #126
	ctx = WithChatKey(ctx, chatKey)
	// #106: inject pending HITL summary into the user message so the agent
	// knows what human input is currently awaiting an answer for this chatKey.
	// This is the "agent 理解" entry point — without it the model has no idea a
	// HITL is waiting and cannot interpret whether the user's reply answers it.
	// Only RunAgent injects; resume/child turns are untouched (their chatKey or
	// resolved-HITL semantics differ, see human_request_hydration.go).
	pending, herr := s.ListPendingHumanRequestsByChatKey(ctx, chatKeyStringFromContext(ctx))
	if herr != nil {
		// Fail-open: store error should not block the turn. But log it — a
		// silent return here leaves the agent blind to pending HITL with no
		// trace, which is the worst kind of failure to debug (§2 silent data
		// loss). The turn proceeds without the summary; the user can still
		// resolve HITL via HTTP/CLI.
		slog.Warn("pending HITL hydration skipped: store lookup failed (agent runs blind to pending HITL this turn)",
			"run_id", runID, "error", herr)
	} else if len(pending) > 0 {
		agentReq.Message = injectPendingHITLSummary(agentReq.Message, pending)
	}
	ctx = rtools.WithRunDir(ctx, s.runs.RunDir(runID))
	ctx = s.withLLMInstrumentation(ctx, llmInstrumentationInput{
		RunID:          runID,
		AgentID:        profile.ID,
		EntrypointID:   inbound.Context.EntrypointID,
		Channel:        inbound.Context.Channel,
		SessionID:      req.SessionID,
		AgentSessionID: agentSessionID,
		ADKSessionID:   adkRuntimeSessionID,
		UserID:         req.Context.SenderID,
		Pricing:        s.pricing,
	}, recordEvent, func(call LLMCallRecord) {
		recorder.appendLLMCall(call)
	})
	final, toolCalls, runErr := s.generate(ctx, profile, runInstruction, agentReq, recordEvent, recordAudit)
	resp.FinalResponse = final
	resp.ToolCalls = toolCalls
	if interrupt := suspendCollector.Interrupt(); interrupt != nil {
		resp.Interrupt = interrupt
		resp.HumanRequests = append([]humanrequest.HumanRequest(nil), interrupt.HumanRequests...)
		resp.VerificationResult = VerificationResult{Status: StatusWaitingHuman, Checks: []string{"runtime_interrupt"}}
		recordEvent("run.waiting_human", "runtime", "agent run waiting for human input", map[string]any{
			"human_requests": len(interrupt.HumanRequests),
			"blocked_by":     interrupt.Reason,
			"summary":        waitingHumanSummary(interrupt),
		})
	} else {
		resp.VerificationResult = s.verifier.Verify(final, profile.Verification.DefaultChecks)
	}
	resp.EndedAt = time.Now()
	resp.Usage = summarizeUsage(resp)
	resp.Status = "completed"
	if resp.Interrupt != nil {
		resp.Status = StatusWaitingHuman
	} else if runErr != nil && errors.Is(runErr, ErrSteered) {
		// Steering is NOT a failure — it's a normal user behavior (user
		// interjected mid-turn). Don't set Status=failed, don't log ERROR,
		// don't create EvolutionCandidate. The caller (retry loop) catches
		// ErrSteered and re-runs. (PR #51 round 4 review: ErrSteered going
		// through failed path polluted monitoring + evolution samples.)
		resp.Status = "steered"
	} else if runErr != nil || resp.VerificationResult.Status != "passed" {
		resp.Status = "failed"
		resp.EvolutionCandidate = s.evolution.CandidateForFailure(runID, "run_failure", resp.VerificationResult, runErr, resp.EndedAt)
	}
	logAttrs := []any{
		"run_id", resp.RunID,
		"agent_id", resp.AgentID,
		"entrypoint_id", resp.EntrypointID,
		"channel", req.Context.Channel,
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
	if runErr != nil && !errors.Is(runErr, ErrSteered) {
		logAttrs = append(logAttrs, "error", runErr)
		slog.Error("agent run finished with error", logAttrs...)
	} else {
		slog.Info("agent run finished", logAttrs...)
	}
	// Session history must persist for EVERY run, not only passing ones. A run
	// that pauses for human input (waiting_human) still had a user message, tool
	// calls, and a reason to ask a human — all audit evidence that must not wait
	// for a (possibly never-coming) human reply. Likewise failed runs. See
	// docs/guide/xira-flow-v0-usage.zh.md §6.1.
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
	if err := s.sessions.AppendAgentMessages(sessionTurn, sessionMessagesForRun(req.Message, final, profile.ID, runID, toolCalls, resp.HumanRequests, resp.Status)); err != nil {
		slog.Warn("session history persistence failed",
			"run_id", runID,
			"agent_id", profile.ID,
			"session_id", req.SessionID,
			"agent_session_id", agentSessionID,
			"error", err,
		)
		// session.persist_failed removed (#43): covered by the slog.Warn above.
	} else {
		messagesPath := s.sessions.AgentMessagesPath(sessionTurn)
		slog.Info("session history persisted",
			"run_id", runID,
			"agent_id", profile.ID,
			"session_id", req.SessionID,
			"agent_session_id", agentSessionID,
			"messages_path", messagesPath,
			"run_status", resp.Status,
		)
		// session.persisted removed (#43): covered by the slog.Info above.
	}
	if len(resp.LLMCalls) > 0 {
		// llm.usage_summary removed (#43): Usage is persisted in run.yaml's
		// Usage field; summary tokens/cost already logged at run end.
		if s.usage != nil {
			if err := s.usage.AppendCalls(resp.LLMCalls); err != nil {
				// usage.ledger_failed removed (#43): covered by the slog.Warn.
				slog.Warn("usage ledger append failed", "run_id", runID, "error", err)
			}
			// usage.ledger_appended removed (#43): success path, already written
			// to usage-ledger.jsonl by AppendCalls — redundant notification.
		}
	}
	// assistant.final: a live "final answer ready" signal. Published only when a
	// final response was produced AND the run succeeded — this is a whitelist
	// (status == completed), not a blacklist. A failed run can have a non-empty
	// final (e.g. verification failed on a populated draft), and in that case the
	// forwarder must NOT drain: the delegate failed/timeout progress sitting in
	// its queue is exactly the signal the user needs. Emitting assistant.final
	// there would drain it away. HITL (waiting_human) has no final answer ready
	// either. See docs/architecture/xira-conversation-progress-feed-v0.zh.md §8.5
	// and the failed-run regression test TestRunDoesNotEmitAssistantFinalOnFailed.
	if final != "" && resp.Status == "completed" {
		recordEvent("assistant.final", "runtime", "assistant final response ready", map[string]any{
			"final_chars": utf8.RuneCountInString(final),
		})
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
type runtimeNativeToolsDisabledContextKey struct{}
type runtimeToolAllowlistContextKey struct{}
type runtimeToolInputAllowlistContextKey struct{}

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

func contextWithRuntimeNativeToolsDisabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, runtimeNativeToolsDisabledContextKey{}, true)
}

func contextWithRuntimeToolAllowlist(ctx context.Context, tools []string) context.Context {
	allowed := map[string]struct{}{}
	for _, tool := range tools {
		tool = strings.TrimSpace(tool)
		if tool == "" {
			continue
		}
		allowed[tool] = struct{}{}
		allowed[deepseek.DeepSeekToolName(tool)] = struct{}{}
	}
	if len(allowed) == 0 {
		allowed["__xira_no_runtime_tools__"] = struct{}{}
	}
	return context.WithValue(ctx, runtimeToolAllowlistContextKey{}, allowed)
}

func contextWithRuntimeToolInputAllowlist(ctx context.Context, allowlist map[string]map[string][]string) context.Context {
	if len(allowlist) == 0 {
		return ctx
	}
	copied := map[string]map[string]map[string]struct{}{}
	for tool, fields := range allowlist {
		tool = strings.TrimSpace(tool)
		if tool == "" || len(fields) == 0 {
			continue
		}
		copied[tool] = map[string]map[string]struct{}{}
		for field, values := range fields {
			field = strings.TrimSpace(field)
			if field == "" || len(values) == 0 {
				continue
			}
			copied[tool][field] = map[string]struct{}{}
			for _, value := range values {
				value = strings.TrimSpace(value)
				if value != "" {
					copied[tool][field][value] = struct{}{}
				}
			}
		}
	}
	return context.WithValue(ctx, runtimeToolInputAllowlistContextKey{}, copied)
}

func executionPolicySnapshotFromRequest(req TurnRequest) ExecutionPolicySnapshot {
	return ExecutionPolicySnapshot{
		AllowedToolsSet:    req.AllowedToolsSet,
		AllowedTools:       append([]string(nil), req.AllowedTools...),
		ToolInputAllowlist: cloneToolInputAllowlist(req.ToolInputAllowlist),
	}
}

func applyExecutionPolicySnapshot(req *TurnRequest, policy ExecutionPolicySnapshot) {
	if req == nil {
		return
	}
	req.AllowedToolsSet = policy.AllowedToolsSet
	req.AllowedTools = append([]string(nil), policy.AllowedTools...)
	req.ToolInputAllowlist = cloneToolInputAllowlist(policy.ToolInputAllowlist)
}

func cloneToolInputAllowlist(in map[string]map[string][]string) map[string]map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string][]string, len(in))
	for tool, fields := range in {
		out[tool] = make(map[string][]string, len(fields))
		for field, values := range fields {
			out[tool][field] = append([]string(nil), values...)
		}
	}
	return out
}

func toolFailureGuardFromContext(ctx context.Context) *toolFailureGuard {
	guard, _ := ctx.Value(toolFailureGuardContextKey{}).(*toolFailureGuard)
	return guard
}

func toolTraceFromContext(ctx context.Context) toolTraceContext {
	trace, _ := ctx.Value(toolTraceContextKey{}).(toolTraceContext)
	return trace
}

func runtimeNativeToolsDisabledFromContext(ctx context.Context) bool {
	disabled, _ := ctx.Value(runtimeNativeToolsDisabledContextKey{}).(bool)
	return disabled
}

func runtimeToolAllowedFromContext(ctx context.Context, name string) bool {
	allowed, _ := ctx.Value(runtimeToolAllowlistContextKey{}).(map[string]struct{})
	if len(allowed) == 0 {
		return true
	}
	name = strings.TrimSpace(name)
	if _, ok := allowed[name]; ok {
		return true
	}
	_, ok := allowed[deepseek.DeepSeekToolName(name)]
	return ok
}

func validateRuntimeToolInputAllowlist(ctx context.Context, tool string, input map[string]any) error {
	allowlist, _ := ctx.Value(runtimeToolInputAllowlistContextKey{}).(map[string]map[string]map[string]struct{})
	fields := allowlist[strings.TrimSpace(tool)]
	if len(fields) == 0 {
		return nil
	}
	for field, allowed := range fields {
		value := strings.TrimSpace(fmt.Sprint(input[field]))
		if value == "" {
			return fmt.Errorf("tool input %s.%s is required by flow step", tool, field)
		}
		if _, ok := allowed[value]; !ok {
			return fmt.Errorf("tool input %s.%s=%q is not allowed by flow step", tool, field, value)
		}
	}
	return nil
}

func constrainedToolParameters(ctx context.Context, tool string, parameters map[string]any) map[string]any {
	allowlist, _ := ctx.Value(runtimeToolInputAllowlistContextKey{}).(map[string]map[string]map[string]struct{})
	fields := allowlist[strings.TrimSpace(tool)]
	if len(fields) == 0 {
		return parameters
	}
	out := cloneAnyMapDeep(parameters)
	props, _ := out["properties"].(map[string]any)
	for field, allowed := range fields {
		prop, _ := props[field].(map[string]any)
		if prop == nil {
			continue
		}
		values := make([]string, 0, len(allowed))
		for value := range allowed {
			values = append(values, value)
		}
		sort.Strings(values)
		prop["enum"] = values
	}
	return out
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
	instructionText string,
	req TurnRequest,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) (string, []ToolCallRecord, error) {
	return s.generateADK(ctx, profile, instructionText, req, recordEvent, recordAudit)
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
	instructionText string,
	req TurnRequest,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) (string, []ToolCallRecord, error) {
	modelID := profile.ModelPolicy.Model
	if !deepseek.SupportedModel(modelID) {
		return "", nil, fmt.Errorf("unsupported model %q", modelID)
	}
	messages := []deepseek.Message{
		{Role: "system", Content: instructionText},
		{Role: "user", Content: req.Message},
	}
	tools := s.toolDefinitions(ctx, profile)
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
		if isHumanRequestToolWireName(call.Function.Name) && !runtimeNativeToolsDisabledFromContext(ctx) && runtimeToolAllowedFromContext(ctx, "human.request") {
			args := map[string]any{}
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
			req, err := s.createAgentHumanRequest(ctx, call.ID, args)
			if err != nil {
				recordAudit("human.request", call.ID, false, err.Error(), args)
				return "", toolRecords, err
			}
			recordEvent("human.request.created", "runtime", "human request created", map[string]any{
				"human_request_id": req.ID,
				"kind":             req.Kind,
				"source":           req.Source,
				"tool_call_id":     req.ToolCallID,
				"question":         req.Question, // #109
			})
			recordAudit("human.request", req.ID, true, "agent requested human input", map[string]any{
				"kind":         req.Kind,
				"tool_call_id": req.ToolCallID,
			})
			if collector := runtimeSuspendCollectorFromContext(ctx); collector != nil && collector.HasInterrupt() {
				recordEvent("model.suspended", "deepseek", "model loop suspended by runtime interrupt", map[string]any{
					"model": modelID,
				})
				return "", toolRecords, nil
			}
		}
		rec := s.executeToolCall(ctx, profile, call, recordEvent, recordAudit)
		toolRecords = append(toolRecords, rec)
		if collector := runtimeSuspendCollectorFromContext(ctx); collector != nil && collector.HasInterrupt() {
			recordEvent("model.suspended", "deepseek", "model loop suspended by runtime interrupt", map[string]any{
				"model": modelID,
			})
			return "", toolRecords, nil
		}
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
	if !runtimeToolAllowedFromContext(ctx, rec.Name) {
		rec.Error = "tool is not allowed by flow step"
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
	if err := validateRuntimeToolInputAllowlist(ctx, rec.Name, rec.Input); err != nil {
		rec.Error = err.Error()
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
		// #81: audit the execution failure. The parallel failure paths (not
		// allowed / not registered, above) already recordAudit("tool.call",
		// ..., false, ...); this execution-failure path was the lone holdout —
		// it only recordEvent'd into Events, so the front-end's audit_events
		// field never saw tool execution failures (xiraClient.ts reads
		// audit_events for the run-inspector, not events).
		recordAudit("tool.call", rec.Name, false, rec.Error, rec.Input)
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

// sessionMessagesForRun builds the session message list for one agent turn. It
// records the user message, every tool call + result (including the one that
// triggered a HITL pause), any human requests the agent raised (the question/
// options), and the assistant reply. The reply may be empty when the run pauses
// for human input — that is intentional, the human-request messages are the
// audit record of why it paused.
func sessionMessagesForRun(userMessage, finalResponse, agentID, runID string, toolCalls []ToolCallRecord, humanRequests []humanrequest.HumanRequest, runStatus string) []fsession.Message {
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
	// Record every human request the agent raised — the question, options, and
	// (if already resolved) the human's response. This is the highest-value
	// audit evidence: it shows exactly what the agent asked and how a human
	// answered, even if the run never reaches "completed".
	for _, hr := range humanRequests {
		messages = append(messages, fsession.Message{
			Role:       "assistant",
			Kind:       fsession.MessageKindHumanRequest,
			Content:    strings.TrimSpace(hr.Question),
			ToolCallID: hr.ToolCallID,
			AgentID:    agentID,
			RunID:      runID,
			Metadata:   humanRequestMetadata(hr),
		})
		if hr.Response != nil {
			messages = append(messages, fsession.Message{
				Role:    "user",
				Kind:    fsession.MessageKindHumanResponse,
				Content: humanResponseText(hr.Response),
				AgentID: agentID,
				RunID:   runID,
				Metadata: map[string]any{
					"kind":             string(hr.Response.Kind),
					"actor":            hr.Response.Actor,
					"human_request_id": hr.ID,
				},
			})
		}
	}
	messages = append(messages, fsession.Message{
		Role:    "assistant",
		Kind:    fsession.MessageKindMessage,
		Content: strings.TrimSpace(finalResponse),
		AgentID: agentID,
		RunID:   runID,
	})
	// Stamp run_status onto every message so ADK session hydration can skip
	// messages from failed runs (audit keeps them all, but failed tool events
	// must not leak into the next run's model context).
	for i := range messages {
		if messages[i].Metadata == nil {
			messages[i].Metadata = map[string]any{}
		}
		messages[i].Metadata["run_status"] = runStatus
	}
	return messages
}

// humanRequestMetadata renders a human request's options/kind into message
// metadata for audit readability.
func humanRequestMetadata(hr humanrequest.HumanRequest) map[string]any {
	meta := map[string]any{
		"kind":             string(hr.Kind),
		"human_request_id": hr.ID,
		"request_kind":     string(hr.Kind),
	}
	if hr.Source != "" {
		meta["source"] = hr.Source
	}
	if len(hr.Options) > 0 {
		opts := make([]string, 0, len(hr.Options))
		for _, o := range hr.Options {
			opts = append(opts, o.Label)
		}
		meta["options"] = opts
	}
	return meta
}

// humanResponseText renders a human response (approve/deny/cancel/answer) into a
// readable content string for the session message.
func humanResponseText(resp *humanrequest.HumanResponse) string {
	if resp == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(string(resp.Kind))
	if resp.Actor != "" {
		b.WriteString(" by ")
		b.WriteString(resp.Actor)
	}
	if resp.Message != "" {
		b.WriteString(": ")
		b.WriteString(resp.Message)
	}
	return b.String()
}

// persistResumeSessionMessages appends the human response + resume turn's final
// response to the agent's session history. Called from HITL/delegation resume
// paths so the human's answer and the agent's post-approval reply are recorded
// for audit — not lost when the run is reloaded from the store.
func (s *Service) persistResumeSessionMessages(run TurnResponse, humanReq *humanrequest.HumanRequest, resumeMessage string) {
	if s == nil || s.sessions == nil || run.SessionScope == nil {
		return
	}
	ctx := inboundContextFromScope(run.SessionScope, nil)
	scope := run.SessionScope
	sessionTurn := fsession.AgentTurnInput{
		SessionID:      run.SessionID,
		AgentID:        run.AgentID,
		AgentSessionID: run.SessionID,
		RunID:          run.RunID,
		Context:        ctx,
		Scope:          scope,
		UserMessage:    resumeMessage,
	}
	var msgs []fsession.Message
	// Record the human's response as a session message.
	if humanReq != nil && humanReq.Response != nil {
		msgs = append(msgs, fsession.Message{
			Role:    "user",
			Kind:    fsession.MessageKindHumanResponse,
			Content: humanResponseText(humanReq.Response),
			AgentID: run.AgentID,
			RunID:   run.RunID,
			Metadata: map[string]any{
				"kind":             string(humanReq.Response.Kind),
				"actor":            humanReq.Response.Actor,
				"human_request_id": humanReq.ID,
			},
		})
	}
	// Record the resume turn's final response.
	if final := strings.TrimSpace(run.FinalResponse); final != "" {
		msgs = append(msgs, fsession.Message{
			Role:    "assistant",
			Kind:    fsession.MessageKindMessage,
			Content: final,
			AgentID: run.AgentID,
			RunID:   run.RunID,
		})
	}
	if len(msgs) == 0 {
		return
	}
	if err := s.sessions.AppendAgentMessages(sessionTurn, msgs); err != nil {
		slog.Warn("resume session history persistence failed",
			"run_id", run.RunID,
			"agent_id", run.AgentID,
			"session_id", run.SessionID,
			"error", err,
		)
	}
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

func (s *Service) toolDefinitions(ctx context.Context, profile agents.Profile) []deepseek.Tool {
	defs := s.toolRegistry(profile).Definitions()
	tools := make([]deepseek.Tool, 0, len(defs))
	if !runtimeNativeToolsDisabledFromContext(ctx) {
		for _, def := range runtimeNativeToolDefinitions(profile) {
			if !runtimeToolAllowedFromContext(ctx, def.Function.Name) {
				continue
			}
			tools = append(tools, def)
		}
	}
	for _, def := range defs {
		if !runtimeToolAllowedFromContext(ctx, def.Name) {
			continue
		}
		parameters := constrainedToolParameters(ctx, def.Name, def.Parameters)
		tools = append(tools, deepseek.Tool{
			Type: "function",
			Function: deepseek.ToolFunction{
				Name:        deepseek.DeepSeekToolName(def.Name),
				Description: def.Description,
				Parameters:  parameters,
			},
		})
	}
	return tools
}

func runtimeNativeToolDefinitions(agents.Profile) []deepseek.Tool {
	return []deepseek.Tool{{
		Type: "function",
		Function: deepseek.ToolFunction{
			Name:        deepseek.DeepSeekToolName("human.request"),
			Description: "Pause the current agent run and ask a human for freeform input or approval.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind": map[string]any{
						"type": "string",
						"enum": []string{string(humanrequest.RequestFreeform), string(humanrequest.RequestApproval)},
					},
					"question": map[string]any{"type": "string"},
					"options": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":    map[string]any{"type": "string"},
								"label": map[string]any{"type": "string"},
							},
							"required":             []string{"id", "label"},
							"additionalProperties": false,
						},
					},
				},
				"required":             []string{"question"},
				"additionalProperties": false,
			},
		},
	}}
}

func (s *Service) toolRegistry(profile agents.Profile) *rtools.Registry {
	return rtools.NewBuiltinRegistry(s.workspace, profile.Permissions.Tools, rtools.SandboxRoots{
		AllowRoots:    profile.Permissions.AllowRoots,
		ReadonlyRoots: profile.Permissions.ReadonlyRoots,
	})
}

func (s *Service) instructionText(profile agents.Profile) string {
	// Note: this path is used for InstructionHash (usage.go / modelPolicySnapshot).
	// It deliberately passes an empty InboundContext so the Conversation Context
	// block is omitted — the hash must be per-profile stable, not per-sender.
	// See TestInstructionHashStableAcrossSenders.
	if skillText := s.skillInstructionText(profile); skillText != "" {
		return s.composeInstructionText(profile, []string{skillText}, channel.InboundContext{})
	}
	return s.composeInstructionText(profile, nil, channel.InboundContext{})
}

func (s *Service) instructionTextForRun(profile agents.Profile, inbound channel.InboundContext) (string, []string, error) {
	activeSkills, activeSkillIDs, err := s.activateSkills(profile, profile.Skills)
	if err != nil {
		return "", nil, err
	}
	blocks := make([]string, 0, len(activeSkills))
	for _, skill := range activeSkills {
		blocks = append(blocks, skill.InstructionBlock())
	}
	return s.composeInstructionText(profile, blocks, inbound), activeSkillIDs, nil
}

func (s *Service) composeInstructionText(profile agents.Profile, skillBlocks []string, inbound channel.InboundContext) string {
	base := strings.TrimSpace(profile.InstructionText())
	if skillText := strings.TrimSpace(strings.Join(skillBlocks, "\n\n")); skillText != "" {
		if base == "" {
			base = skillText
		} else {
			base += "\n\n" + skillText
		}
	}
	tools := s.toolRegistry(profile).List()
	identity := fmt.Sprintf(
		"Current Xira agent: %s (%s).\nThis agent profile and runtime instruction are authoritative. If prior assistant messages or model defaults conflict with this agent identity, follow the current profile and correct the conflict. When asked who you are or which agent is active, answer as this Xira agent; do not identify as the underlying model provider unless the user explicitly asks about the model provider.\nCurrent date: %s (use this for `created`/`updated`/`review_at`/Decision Log entries when the user does not specify a date).",
		profile.ID,
		profile.Name,
		time.Now().Format("2006-01-02"),
	)
	conversation := formatConversationContext(inbound)
	var capability string
	if len(tools) == 0 {
		capability = "Available tools: none.\nOnly claim capabilities you can perform without tools."
	} else {
		capability = "Available tools: " + strings.Join(tools, ", ") + ".\nOnly claim capabilities you can perform with these tools.\nIf a needed tool is available, use it before claiming you cannot access the data. Only say a tool is unavailable or restricted when no appropriate tool exists or an attempted tool call returns an error; when that happens, mention the actual tool error."
	}
	// Order: Identity (who I am) → Conversation Context (who/where I'm talking to)
	// → Capabilities (what I can do). Conversation Context is omitted when the
	// inbound carries no identity (e.g. hash path in instructionText above).
	var sections []string
	sections = append(sections, "# Runtime Identity\n\n"+identity)
	if conversation != "" {
		sections = append(sections, "# Conversation Context\n\n"+conversation)
	}
	sections = append(sections, "# Runtime Capabilities\n\n"+capability)
	body := strings.Join(sections, "\n\n")
	if base == "" {
		return body
	}
	return base + "\n\n" + body
}

// formatConversationContext renders the "# Conversation Context" body (without
// the H1 heading) from the inbound identity. Returns "" when no identity is
// present (e.g. zero-value InboundContext on the InstructionHash path), so the
// whole section is omitted by the caller.
//
// Fields come from InboundContext: Channel/ChatID/ChatType/SenderID (IDs) and
// ChatName/SenderName (display names). NormalizeInboundContext guarantees
// ChatID and ChatType have fallback values for real inbound traffic; the empty
// checks here defend against zero-value contexts on the hash path and direct
// construction bypassing the normalizer. Name fields are optional — when no
// channel runner populates them they stay "" and the corresponding lines are
// omitted (first-version state; runner填充 is tracked in follow-up issues).
//
// Trust boundary: InboundContext fields are UNTRUSTED. HTTP API and websocket
// clients can carry arbitrary context, so a chat_id/sender_id containing
// "\n\n# Runtime Capabilities" could otherwise escape into a new prompt
// section and inject instructions (prompt-injection vector — see PR #130
// review). sanitizeInlineField collapses control chars (newlines, CR, tab,
// vertical tab, form feed, NUL) to single spaces and strips "# " heading
// markers, so each field renders as a single line that cannot start a new
// prompt section regardless of its content.
func formatConversationContext(inbound channel.InboundContext) string {
	channel := sanitizeInlineField(inbound.Channel)
	chatID := sanitizeInlineField(inbound.ChatID)
	chatType := sanitizeInlineField(inbound.ChatType)
	chatName := sanitizeInlineField(inbound.ChatName)
	senderID := sanitizeInlineField(inbound.SenderID)
	senderName := sanitizeInlineField(inbound.SenderName)
	if channel == "" && chatID == "" && senderID == "" {
		return ""
	}
	var lines []string
	if channel != "" {
		lines = append(lines, "Channel: "+channel)
	}
	if chatID != "" {
		if chatType != "" {
			lines = append(lines, fmt.Sprintf("Chat: %s (type: %s)", chatID, chatType))
		} else {
			lines = append(lines, "Chat: "+chatID)
		}
	}
	if chatName != "" {
		lines = append(lines, "ChatName: "+chatName)
	}
	if senderID != "" {
		lines = append(lines, "Sender: "+senderID)
	}
	if senderName != "" {
		lines = append(lines, "SenderName: "+senderName)
	}
	return strings.Join(lines, "\n")
}

// sanitizeInlineField cleans an untrusted InboundContext field so it is safe
// to embed in a prompt section. It:
//  1. Replaces all ASCII control chars (including \n \r \t \v \f and below
//     0x20, plus 0x7f DEL) with a single space — preventing newline escapes
//     that would start a new prompt line / section.
//  2. Collapses runs of whitespace to a single space.
//  3. Trims outer whitespace.
//  4. Strips leading "#" so a field starting with "# Runtime Identity" cannot
//     masquerade as a markdown heading (interior "#" is preserved — only the
//     leading-position heading-prefix form is dangerous).
//
// We deliberately do NOT strip interior substrings like "Available tools" or
// "Ignore previous" — those would be attacker-chosen text and we shouldn't
// pretend to enumerate them. The structural defenses (no newline, no leading
// "# ") are enough: the field ends up as a single line of opaque data, not
// instructions the model parses.
func sanitizeInlineField(s string) string {
	if s == "" {
		return ""
	}
	// Replace control chars (including \n \r \t \v \f and DEL) with space.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	cleaned := b.String()
	// Collapse runs of whitespace.
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	// Strip leading "#" so a field can't open with a heading marker. Interior
	// "#" is harmless (no newline to start a section with it).
	for strings.HasPrefix(cleaned, "#") {
		cleaned = strings.TrimSpace(cleaned[1:])
	}
	return cleaned
}

func (s *Service) activateSkills(profile agents.Profile, skillIDs []string) ([]skills.Skill, []string, error) {
	if len(skillIDs) == 0 {
		return nil, nil, nil
	}
	if s == nil || s.skills == nil {
		return nil, nil, fmt.Errorf("agent profile %q references skills but no skill registry is available", profile.ID)
	}
	knownTools := rtools.NewBuiltinRegistry(s.workspace, agents.BuiltinToolNames(), rtools.SandboxRoots{})
	seen := map[string]struct{}{}
	active := make([]skills.Skill, 0, len(skillIDs))
	activeIDs := make([]string, 0, len(skillIDs))
	for _, skillID := range skillIDs {
		skillID = strings.TrimSpace(skillID)
		if skillID == "" {
			continue
		}
		if _, ok := seen[skillID]; ok {
			continue
		}
		seen[skillID] = struct{}{}
		skill, ok := s.skills.Get(skillID)
		if !ok {
			return nil, nil, fmt.Errorf("agent profile %q references missing skill %q", profile.ID, skillID)
		}
		if err := validateSkillActivation(profile, skill, knownTools); err != nil {
			return nil, nil, err
		}
		active = append(active, skill)
		activeIDs = append(activeIDs, skill.ID)
	}
	return active, activeIDs, nil
}

func (s *Service) skillInstructionText(profile agents.Profile) string {
	if s == nil || s.skills == nil {
		return ""
	}
	var blocks []string
	seen := map[string]struct{}{}
	for _, id := range profile.Skills {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		skill, ok := s.skills.Get(id)
		if !ok {
			continue
		}
		blocks = append(blocks, skill.InstructionBlock())
	}
	return strings.Join(blocks, "\n\n")
}

func (s *Service) modelPolicySnapshot(profile agents.Profile) ModelPolicySnapshot {
	snapshot := modelPolicySnapshot(profile, s.profileSource)
	snapshot.Skills = compactProfileSkills(profile.Skills)
	snapshot.AllowRoots = profile.Permissions.AllowRoots
	snapshot.ReadonlyRoots = profile.Permissions.ReadonlyRoots
	snapshot.InstructionHash = instructionHash(s.instructionText(profile))
	return snapshot
}

func (s *Service) modelPolicySnapshotForRun(profile agents.Profile, instructionText string, activeSkillIDs []string) ModelPolicySnapshot {
	snapshot := modelPolicySnapshot(profile, s.profileSource)
	snapshot.Skills = append([]string{}, activeSkillIDs...)
	snapshot.AllowRoots = profile.Permissions.AllowRoots
	snapshot.ReadonlyRoots = profile.Permissions.ReadonlyRoots
	snapshot.InstructionHash = instructionHash(instructionText)
	return snapshot
}

func validateSkillActivation(profile agents.Profile, skill skills.Skill, knownTools *rtools.Registry) error {
	for _, tool := range skill.Requires.Tools {
		if !knownTools.Has(tool) {
			return fmt.Errorf("skill %q requires unknown tool %q", skill.ID, tool)
		}
		if !stringListContains(profile.Permissions.Tools, tool) {
			return fmt.Errorf("agent profile %q references skill %q but does not allow required tool %q", profile.ID, skill.ID, tool)
		}
	}
	for _, secret := range skill.Requires.Secrets {
		if !stringListContains(profile.Permissions.Secrets, secret) {
			return fmt.Errorf("agent profile %q references skill %q but does not allow required secret %q", profile.ID, skill.ID, secret)
		}
	}
	for _, server := range skill.Requires.MCPServers {
		if !stringListContains(profile.MCPServers, server) {
			return fmt.Errorf("agent profile %q references skill %q but does not allow required MCP server %q", profile.ID, skill.ID, server)
		}
	}
	return nil
}

func stringListContains(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func compactProfileSkills(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
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
