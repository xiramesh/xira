package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/xiramesh/xira/internal/channel"
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
	return s.resumeResolvedHumanRequest(ctx, resolved)
}

// ResolveHumanResponse validates an exact, channel-neutral response envelope.
// Owner authority is checked against the current binding as well as the
// persisted responder snapshot before the Store mutates the request.
func (s *Service) ResolveHumanResponse(ctx context.Context, input humanrequest.HumanResponseEnvelope) (*humanrequest.HumanRequest, error) {
	if s == nil || s.humanRequests == nil {
		return nil, fmt.Errorf("human request store is not available")
	}
	input.WorkspaceKey = s.WorkspaceKey()
	req, err := s.humanRequests.Get(ctx, input.WorkspaceKey, input.RequestID)
	if err != nil {
		return nil, err
	}
	if req.Responder.Type == humanrequest.ResponderOwner && !s.IsOwner(ctx, input.SenderID, input.EntrypointID) {
		return nil, fmt.Errorf("%w: response actor is not the current owner for entrypoint", humanrequest.ErrConflict)
	}
	resolved, err := s.humanRequests.ResolveExact(ctx, input)
	if err != nil {
		return nil, err
	}
	return s.resumeResolvedHumanRequest(ctx, resolved)
}

// ReconcileHumanRequests retries every response whose durable resume work is
// pending or previously failed. Individual failures do not prevent unrelated
// requests from being attempted.
func (s *Service) ReconcileHumanRequests(ctx context.Context) error {
	if s == nil || s.humanRequests == nil {
		return fmt.Errorf("human request store is not available")
	}
	requests, err := s.humanRequests.ListResumable(ctx, s.WorkspaceKey())
	if err != nil {
		return err
	}
	var failures []error
	for i := range requests {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if _, err := s.resumeResolvedHumanRequest(ctx, &requests[i]); err != nil {
			failures = append(failures, fmt.Errorf("resume human request %s: %w", requests[i].ID, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Service) resumeResolvedHumanRequest(ctx context.Context, resolved *humanrequest.HumanRequest) (*humanrequest.HumanRequest, error) {
	if resolved == nil {
		return nil, fmt.Errorf("human request is required")
	}
	// Backward compatibility: pre-#163 records have no durable resume state.
	// Preserve the former synchronous behavior but never auto-reconcile them.
	if resolved.Resume.Status == "" {
		if err := s.dispatchHumanRequestResume(ctx, resolved); err != nil {
			return nil, err
		}
		return resolved, nil
	}
	claimed, ok, err := s.humanRequests.ClaimResume(ctx, s.WorkspaceKey(), resolved.ID, time.Now())
	if err != nil {
		return nil, err
	}
	if !ok {
		return claimed, nil
	}
	if err := s.dispatchHumanRequestResume(ctx, claimed); err != nil {
		failed, markErr := s.humanRequests.FailResume(context.WithoutCancel(ctx), s.WorkspaceKey(), claimed.ID, err.Error(), time.Now())
		if markErr != nil {
			return failed, errors.Join(err, fmt.Errorf("persist resume failure: %w", markErr))
		}
		return failed, err
	}
	completed, err := s.humanRequests.CompleteResume(context.WithoutCancel(ctx), s.WorkspaceKey(), claimed.ID, time.Now())
	if err != nil {
		return nil, err
	}
	return completed, nil
}

func (s *Service) dispatchHumanRequestResume(ctx context.Context, resolved *humanrequest.HumanRequest) error {
	switch resolved.Source {
	case "agent_request":
		return s.resumeDirectHumanRequest(ctx, resolved)
	case flow.SourceFlowHumanApproval:
		flowRunID := strings.TrimSpace(resolved.Metadata[flow.MetadataFlowRunID])
		if flowRunID == "" {
			return fmt.Errorf("flow human approval %s missing %s metadata", resolved.ID, flow.MetadataFlowRunID)
		}
		_, err := s.ResumeFlow(ctx, flowRunID, resolved.ID)
		return err
	default:
		return nil
	}
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
		Responder:    currentSenderResponder(exec.Request.Context),
	})
	if err != nil {
		return nil, err
	}
	collector.AddHumanRequest(*req, "agent_request")
	cancelRuntimeOnInterrupt(ctx)
	return req, nil
}

// currentSenderResponder binds the generic responder policy to authoritative
// inbound identity. It never accepts model-provided IDs.
func currentSenderResponder(ctx channel.InboundContext) humanrequest.ResponderPolicy {
	ctx = channel.NormalizeInboundContext(ctx)
	return humanrequest.ResponderPolicy{
		Type:         humanrequest.ResponderCurrentSender,
		EntrypointID: strings.TrimSpace(ctx.EntrypointID),
		SenderID:     strings.TrimSpace(ctx.SenderID),
		SenderIDType: strings.ToLower(strings.TrimSpace(ctx.SenderIDType)),
	}
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
