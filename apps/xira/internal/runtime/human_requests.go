package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/xiramesh/xira/internal/flow"
	"github.com/xiramesh/xira/internal/humanrequest"
)

func mustHumanRequestStore(root string) *humanrequest.Store {
	store, err := humanrequest.NewStore(root)
	if err != nil {
		panic(err)
	}
	return store
}

// chatKeyStringFromContext returns the chatKey from ctx as its canonical string
// form (runtime.ChatKey.String()), or "" if ctx carries no chatKey OR a zero-
// value chatKey (which would otherwise serialize as "//" — meaningless and a
// future ListByChatKey footgun). Used when building CreateRequest.ChatKey.
func chatKeyStringFromContext(ctx context.Context) string {
	key, ok := ChatKeyFromContext(ctx)
	if !ok {
		return ""
	}
	// A zero-value ChatKey (Channel/ChatID/SenderID all empty) is not a real
	// chat identity — normalize to empty so it doesn't persist as "//" and get
	// treated as a distinct (wrong) chat key by ListByChatKey.
	if key.Channel == "" && key.ChatID == "" && key.SenderID == "" {
		return ""
	}
	return key.String()
}

func (s *Service) WorkspaceKey() string {
	if s == nil {
		return ""
	}
	return humanrequest.WorkspaceKeyFor(s.workspace)
}

func (s *Service) CreateHumanRequest(ctx context.Context, input humanrequest.CreateRequest) (*humanrequest.HumanRequest, error) {
	if s == nil || s.humanRequests == nil {
		return nil, fmt.Errorf("human request store is not available")
	}
	if strings.TrimSpace(input.WorkspaceID) == "" {
		input.WorkspaceID = s.workspace
	}
	if strings.TrimSpace(input.WorkspaceKey) == "" {
		input.WorkspaceKey = s.WorkspaceKey()
	}
	return s.humanRequests.Create(ctx, input)
}

func (s *Service) GetHumanRequest(ctx context.Context, requestID string) (*humanrequest.HumanRequest, error) {
	if s == nil || s.humanRequests == nil {
		return nil, fmt.Errorf("human request store is not available")
	}
	return s.humanRequests.Get(ctx, s.WorkspaceKey(), requestID)
}

func (s *Service) ListHumanRequests(ctx context.Context, status humanrequest.RequestStatus) ([]humanrequest.HumanRequest, error) {
	if s == nil || s.humanRequests == nil {
		return nil, fmt.Errorf("human request store is not available")
	}
	return s.humanRequests.List(ctx, humanrequest.ListQuery{WorkspaceKey: s.WorkspaceKey(), Status: status})
}

// ListPendingHumanRequestsByChatKey returns pending HITL requests for a chatKey
// (runtime.ChatKey.String()). Channel adapters use this to check "does this
// chat have a HITL waiting for an answer?" before starting a new turn (#92 —
// HITL IM direct answer). Returns empty (not error) if the store is absent.
func (s *Service) ListPendingHumanRequestsByChatKey(ctx context.Context, chatKey string) ([]humanrequest.HumanRequest, error) {
	if s == nil || s.humanRequests == nil {
		return nil, nil
	}
	return s.humanRequests.ListByChatKey(ctx, s.WorkspaceKey(), chatKey)
}

func (s *Service) ResolveHumanRequest(ctx context.Context, requestID string, input humanrequest.ResolveRequest) (*humanrequest.HumanRequest, error) {
	if s == nil || s.humanRequests == nil {
		return nil, fmt.Errorf("human request store is not available")
	}
	input.WorkspaceKey = s.WorkspaceKey()
	input.RequestID = requestID
	resolved, err := s.humanRequests.Resolve(ctx, input)
	if err != nil {
		return nil, err
	}
	if resolved.Response != nil && resolved.Response.Kind == humanrequest.ResponseApprove && resolved.ActionSnapshot != nil {
		resolved, err = s.replayApprovedActionSnapshot(ctx, resolved)
		if err != nil {
			return nil, err
		}
	}
	if resolved.Response != nil && resolved.ActionSnapshot != nil && (resolved.Response.Kind == humanrequest.ResponseDeny || resolved.Response.Kind == humanrequest.ResponseCancel) {
		if err := s.materializeRejectedActionSnapshot(resolved); err != nil {
			return nil, err
		}
	}
	// Resume the run after the human response. Only agent-request-sourced
	// interrupts trigger a direct resume (others no-op).
	if resolved.Source == "agent_request" {
		if err := s.resumeDirectHumanRequest(ctx, resolved); err != nil {
			return nil, err
		}
	}
	if resolved.Source == flow.SourceFlowHumanApproval {
		flowRunID := strings.TrimSpace(resolved.Metadata[flow.MetadataFlowRunID])
		if flowRunID == "" {
			return nil, fmt.Errorf("flow human approval %s missing %s metadata", resolved.ID, flow.MetadataFlowRunID)
		}
		if _, err := s.ResumeFlow(ctx, flowRunID, resolved.ID); err != nil {
			return nil, err
		}
	}
	return resolved, nil
}

func (s *Service) createAgentHumanRequest(ctx context.Context, callID string, args map[string]any) (*humanrequest.HumanRequest, error) {
	exec, ok := runExecutionFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("human.request requires runtime execution context")
	}
	collector := runtimeSuspendCollectorFromContext(ctx)
	if collector == nil {
		return nil, fmt.Errorf("human.request requires runtime suspend collector")
	}
	callID = strings.TrimSpace(callID)
	if callID == "" {
		callID = uuid.NewString()
	}
	kind := stringArg(args, "kind")
	if kind == "" {
		kind = string(humanrequest.RequestFreeform)
	}
	requestKind, err := requestKindFromString(kind)
	if err != nil {
		return nil, err
	}
	question := stringArg(args, "question")
	options := humanOptionsFromAny(args["options"])
	req, err := s.CreateHumanRequest(ctx, humanrequest.CreateRequest{
		WorkspaceID:  s.workspace,
		WorkspaceKey: s.WorkspaceKey(),
		RunID:        exec.Base.RunID,
		AgentID:      exec.Profile.ID,
		SessionID:    exec.Base.ConversationSessionID,
		ToolCallID:   callID,
		Source:       "agent_request",
		Kind:         requestKind,
		Question:     question,
		Options:      options,
		DedupeKey:    "agent_request:" + exec.Base.RunID + ":" + callID + ":" + question,
		ChatKey:      chatKeyStringFromContext(ctx),
	})
	if err != nil {
		return nil, err
	}
	collector.AddHumanRequest(*req, "agent_request")
	cancelRuntimeOnInterrupt(ctx)
	return req, nil
}

func (s *Service) createRuntimeToolGateHumanRequest(ctx context.Context, toolCallID, toolName string, args map[string]any) (*humanrequest.HumanRequest, error) {
	exec, ok := runExecutionFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("runtime tool gate requires execution context")
	}
	collector := runtimeSuspendCollectorFromContext(ctx)
	if collector == nil {
		return nil, fmt.Errorf("runtime tool gate requires suspend collector")
	}
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		toolCallID = uuid.NewString()
	}
	snapshotArgs, err := canonicalActionSnapshotArguments(args)
	if err != nil {
		return nil, fmt.Errorf("canonicalize action snapshot arguments: %w", err)
	}
	contextHash, err := digestAny(snapshotArgs)
	if err != nil {
		return nil, fmt.Errorf("marshal action snapshot arguments: %w", err)
	}
	req, err := s.CreateHumanRequest(ctx, humanrequest.CreateRequest{
		WorkspaceID:  s.workspace,
		WorkspaceKey: s.WorkspaceKey(),
		RunID:        exec.Base.RunID,
		AgentID:      exec.Profile.ID,
		SessionID:    exec.Base.ConversationSessionID,
		ToolCallID:   toolCallID,
		Source:       "runtime_tool_gate",
		Kind:         humanrequest.RequestApproval,
		Question:     "Approve tool call " + strings.TrimSpace(toolName) + "?",
		DedupeKey:    "runtime_tool_gate:" + exec.Base.RunID + ":" + toolCallID + ":" + strings.TrimSpace(toolName),
		ChatKey:      chatKeyStringFromContext(ctx),
		ActionSnapshot: &humanrequest.ActionSnapshot{
			ToolName:    strings.TrimSpace(toolName),
			Arguments:   snapshotArgs,
			RunID:       exec.Base.RunID,
			AgentID:     exec.Profile.ID,
			SessionID:   exec.Base.ConversationSessionID,
			ToolCallID:  toolCallID,
			ContextHash: contextHash,
		},
	})
	if err != nil {
		return nil, err
	}
	collector.AddHumanRequest(*req, "runtime_tool_gate")
	collector.SuspendToolCall(SuspendedToolCall{
		ID:     toolCallID,
		RunID:  exec.Base.RunID,
		Name:   strings.TrimSpace(toolName),
		Input:  cloneAnyMap(snapshotArgs),
		Status: StatusWaitingHuman,
	})
	cancelRuntimeOnInterrupt(ctx)
	return req, nil
}

func (s *Service) replayApprovedActionSnapshot(ctx context.Context, req *humanrequest.HumanRequest) (*humanrequest.HumanRequest, error) {
	if req == nil || req.ActionSnapshot == nil {
		return req, nil
	}
	owner := "runtime-replay"
	leased, err := s.humanRequests.BeginReplay(ctx, humanrequest.ReplayLeaseRequest{
		WorkspaceKey:  s.WorkspaceKey(),
		RequestID:     req.ID,
		Owner:         owner,
		LeaseDuration: 5 * time.Minute,
	})
	if err != nil {
		return nil, err
	}
	profile, ok := s.agents.Get(leased.ActionSnapshot.AgentID)
	if !ok {
		return nil, s.failApprovedActionReplay(ctx, req.ID, owner, fmt.Errorf("replay agent profile %q not found", leased.ActionSnapshot.AgentID))
	}
	if err := validateActionSnapshotDigest(leased.ActionSnapshot); err != nil {
		return nil, s.failApprovedActionReplay(ctx, req.ID, owner, err)
	}
	registry := s.toolRegistry(profile)
	output, execErr := registry.Execute(ctx, leased.ActionSnapshot.ToolName, cloneAnyMap(leased.ActionSnapshot.Arguments))
	if execErr != nil {
		return nil, s.failApprovedActionReplay(ctx, req.ID, owner, execErr)
	}
	if err := s.materializeApprovedActionSnapshotOutput(leased, output); err != nil {
		return nil, s.failApprovedActionReplay(ctx, req.ID, owner, err)
	}
	digest, err := digestAny(output)
	if err != nil {
		return nil, s.failApprovedActionReplay(ctx, req.ID, owner, fmt.Errorf("marshal replay output: %w", err))
	}
	completed, err := s.humanRequests.CompleteReplay(ctx, humanrequest.CompleteReplayRequest{
		WorkspaceKey:    s.WorkspaceKey(),
		RequestID:       req.ID,
		Owner:           owner,
		ResultDigest:    digest,
		ResultReference: "tool:" + leased.ActionSnapshot.ToolName,
		IdempotencyKey:  req.Response.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	if err := s.resumeRunAfterApprovedToolOutput(ctx, completed, output); err != nil {
		return nil, err
	}
	return completed, nil
}

func (s *Service) failApprovedActionReplay(ctx context.Context, requestID, owner string, replayErr error) error {
	if replayErr == nil {
		return nil
	}
	if s == nil || s.humanRequests == nil {
		return replayErr
	}
	_, failErr := s.humanRequests.FailReplay(ctx, humanrequest.FailReplayRequest{
		WorkspaceKey: s.WorkspaceKey(),
		RequestID:    requestID,
		Owner:        owner,
		Error:        replayErr.Error(),
	})
	if failErr != nil {
		return fmt.Errorf("%w; additionally failed to persist replay failure: %v", replayErr, failErr)
	}
	return replayErr
}

func (s *Service) materializeRejectedActionSnapshot(req *humanrequest.HumanRequest) error {
	if s == nil || s.runs == nil || req == nil || req.Response == nil || req.ActionSnapshot == nil {
		return nil
	}
	run, err := s.runs.Load(req.ActionSnapshot.RunID)
	if err != nil {
		return err
	}
	status := "denied"
	if req.Response.Kind == humanrequest.ResponseCancel {
		status = "canceled"
	}
	now := time.Now()
	replaced := false
	for i := range run.ToolCalls {
		if run.ToolCalls[i].ID != req.ActionSnapshot.ToolCallID {
			continue
		}
		if run.ToolCalls[i].Output == nil {
			run.ToolCalls[i].Output = map[string]any{}
		}
		run.ToolCalls[i].Output["status"] = status
		run.ToolCalls[i].Output["human_request_id"] = req.ID
		run.ToolCalls[i].Output["message"] = strings.TrimSpace(req.Response.Message)
		run.ToolCalls[i].Error = strings.TrimSpace(req.Response.Message)
		run.ToolCalls[i].EndedAt = now
		replaced = true
		break
	}
	if !replaced {
		run.ToolCalls = append(run.ToolCalls, ToolCallRecord{
			ID:    req.ActionSnapshot.ToolCallID,
			RunID: req.ActionSnapshot.RunID,
			Name:  req.ActionSnapshot.ToolName,
			Input: cloneAnyMap(req.ActionSnapshot.Arguments),
			Output: map[string]any{
				"status":           status,
				"human_request_id": req.ID,
				"message":          strings.TrimSpace(req.Response.Message),
			},
			Error:     strings.TrimSpace(req.Response.Message),
			StartedAt: now,
			EndedAt:   now,
		})
	}
	run.Status = "failed"
	run.Interrupt = nil
	replaceRunHumanRequest(&run, *req)
	if run.Metadata == nil {
		run.Metadata = map[string]string{}
	}
	if req.Response.Kind == humanrequest.ResponseCancel {
		run.Metadata["error_type"] = "canceled"
	}
	run.VerificationResult = VerificationResult{Status: "failed", Checks: []string{"human_response_" + string(req.Response.Kind)}}
	run.EndedAt = now
	return s.runs.SaveRun(run)
}

func isHumanRequestToolWireName(name string) bool {
	name = strings.TrimSpace(name)
	return name == "human.request" || name == "human_request"
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneAnyMapDeep(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return cloneAnyMap(in)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return cloneAnyMap(in)
	}
	return out
}

func digestAny(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func canonicalActionSnapshotArguments(args map[string]any) (map[string]any, error) {
	if args == nil {
		return nil, nil
	}
	data, err := yaml.Marshal(args)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateActionSnapshotDigest(snapshot *humanrequest.ActionSnapshot) error {
	if snapshot == nil || strings.TrimSpace(snapshot.ContextHash) == "" {
		return nil
	}
	got, err := digestAny(snapshot.Arguments)
	if err != nil {
		return fmt.Errorf("marshal snapshot arguments: %w", err)
	}
	if got != snapshot.ContextHash {
		return fmt.Errorf("snapshot arguments changed: got %s want %s", got, snapshot.ContextHash)
	}
	return nil
}

func humanOptionsFromAny(value any) []humanrequest.HumanOption {
	switch v := value.(type) {
	case []humanrequest.HumanOption:
		return append([]humanrequest.HumanOption(nil), v...)
	case []map[string]any:
		out := make([]humanrequest.HumanOption, 0, len(v))
		for _, item := range v {
			out = append(out, humanrequest.HumanOption{
				ID:    strings.TrimSpace(fmt.Sprint(item["id"])),
				Label: strings.TrimSpace(fmt.Sprint(item["label"])),
			})
		}
		return out
	case []any:
		out := make([]humanrequest.HumanOption, 0, len(v))
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, humanrequest.HumanOption{
				ID:    strings.TrimSpace(fmt.Sprint(item["id"])),
				Label: strings.TrimSpace(fmt.Sprint(item["label"])),
			})
		}
		return out
	default:
		return nil
	}
}
