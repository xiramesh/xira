package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

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
