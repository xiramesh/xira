package runtime

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/flow"
	"github.com/xiramesh/xira/internal/humanrequest"
)

// flowBridge adapts runtime.Service to the flow package's AgentRunner /
// HumanRequestCreator / HumanRequestResolver interfaces. It lives in the
// runtime package to keep the flow package free of a runtime import.
type flowBridge struct {
	service *Service
}

func newFlowBridge(s *Service) *flowBridge {
	return &flowBridge{service: s}
}

// RunAgent satisfies flow.AgentRunner.
func (b *flowBridge) RunAgent(ctx context.Context, req flow.AgentTurnRequest) (flow.AgentTurnResponse, error) {
	if b == nil || b.service == nil {
		return flow.AgentTurnResponse{}, fmt.Errorf("runtime service is not available")
	}
	// The flow executor populates Context with the run's trigger identity, so
	// the unified TurnRequest just carries it through. Merge flow-internal
	// traceability keys (flow_run_id/flow_id/flow_step_id) from Metadata into
	// Context.Raw so they survive into the session and run records.
	bridgeCtx := req.Context
	if len(req.Metadata) > 0 {
		raw := make(map[string]string, len(bridgeCtx.Raw)+len(req.Metadata))
		for k, v := range bridgeCtx.Raw {
			raw[k] = v
		}
		for k, v := range req.Metadata {
			raw[k] = v
		}
		bridgeCtx.Raw = raw
	}
	resp, err := b.service.RunAgent(ctx, TurnRequest{
		AgentID:            req.AgentID,
		EntrypointID:       req.EntrypointID,
		Message:            req.Message,
		AllowedToolsSet:    req.AllowedToolsSet,
		AllowedTools:       append([]string(nil), req.AllowedTools...),
		ToolInputAllowlist: cloneFlowToolInputAllowlist(req.ToolInputAllowlist),
		SessionID:          req.SessionID,
		Context:            bridgeCtx,
	})
	if err != nil {
		return flow.AgentTurnResponse{}, err
	}
	return mapTurnResponseToFlow(resp), nil
}

func cloneFlowToolInputAllowlist(in map[string]map[string][]string) map[string]map[string][]string {
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

func mapTurnResponseToFlow(resp TurnResponse) flow.AgentTurnResponse {
	out := flow.AgentTurnResponse{
		RunID:         resp.RunID,
		AgentID:       resp.AgentID,
		EntrypointID:  resp.EntrypointID,
		SessionID:     resp.SessionID,
		Status:        resp.Status,
		FinalResponse: resp.FinalResponse,
		StartedAt:     resp.StartedAt,
		EndedAt:       resp.EndedAt,
		Artifacts:     append([]string(nil), resp.Artifacts...),
	}
	for _, hr := range resp.HumanRequests {
		out.HumanRequests = append(out.HumanRequests, flow.AgentHumanRequestView{
			ID: hr.ID, Source: hr.Source, Kind: string(hr.Kind), Status: string(hr.Status),
		})
	}
	if resp.Interrupt != nil {
		iv := &flow.AgentInterruptView{
			Status: resp.Interrupt.Status,
			Reason: resp.Interrupt.Reason,
		}
		for _, b := range resp.Interrupt.BlockedBy {
			iv.BlockedBy = append(iv.BlockedBy, flow.AgentBlockedByView{
				Type: b.Type, HumanRequestID: b.HumanRequestID, RunID: b.RunID, Reason: b.Reason,
			})
		}
		out.Interrupt = iv
	}
	return out
}

// CreateHumanRequest satisfies flow.HumanRequestCreator.
func (b *flowBridge) CreateHumanRequest(ctx context.Context, input flow.CreateHumanRequestInput) (flow.HumanRequestView, error) {
	if b == nil || b.service == nil {
		return flow.HumanRequestView{}, fmt.Errorf("runtime service is not available")
	}
	kind := humanrequest.RequestApproval
	if input.Kind == "freeform" {
		kind = humanrequest.RequestFreeform
	}
	options := make([]humanrequest.HumanOption, 0, len(input.Options))
	for _, opt := range input.Options {
		options = append(options, humanrequest.HumanOption{ID: opt, Label: opt})
	}
	req, err := b.service.CreateHumanRequest(ctx, humanrequest.CreateRequest{
		WorkspaceID: input.WorkspaceID,
		RunID:       input.RunID,
		AgentID:     input.AgentID,
		SessionID:   input.SessionID,
		ToolCallID:  input.ToolCallID,
		Source:      input.Source,
		Kind:        kind,
		Question:    input.Question,
		Options:     options,
		DedupeKey:   input.DedupeKey,
		Metadata:    input.Metadata,
		ChatKey:     chatKeyStringFromInbound(input.Context),
		Responder:   currentSenderResponder(input.Context),
	})
	if err != nil {
		return flow.HumanRequestView{}, err
	}
	return flow.HumanRequestView{
		ID: req.ID, Source: req.Source, Kind: string(req.Kind), Status: string(req.Status), Question: req.Question,
	}, nil
}

// GetHumanRequest satisfies flow.HumanRequestResolver.
func (b *flowBridge) GetHumanRequest(ctx context.Context, requestID string) (flow.ResolvedHumanRequest, error) {
	if b == nil || b.service == nil {
		return flow.ResolvedHumanRequest{}, fmt.Errorf("runtime service is not available")
	}
	req, err := b.service.GetHumanRequest(ctx, requestID)
	if err != nil {
		return flow.ResolvedHumanRequest{}, err
	}
	out := flow.ResolvedHumanRequest{
		ID:     req.ID,
		Source: req.Source,
		Kind:   string(req.Kind),
		Status: string(req.Status),
	}
	if req.Response != nil {
		out.ResponseKind = string(req.Response.Kind)
		out.ResponseMessage = req.Response.Message
		if req.Source == flow.SourceFlowHumanApproval && req.Response.Kind == humanrequest.ResponseAnswer {
			if signal := strings.TrimSpace(req.Response.Message); signal != "" {
				out.ResponseKind = signal
			}
		}
	}
	return out, nil
}

func chatKeyStringFromInbound(ctx channel.InboundContext) string {
	key := ChatKeyFromInbound(ctx)
	if key.Channel == "" && key.ChatID == "" && key.SenderID == "" {
		return ""
	}
	return key.String()
}

// AgentStepStatus satisfies flow.AgentStatusResolver by reloading the backing
// agent run's status. Used when resuming agent-generated waiting_human.
func (b *flowBridge) AgentStepStatus(ctx context.Context, run *flow.Run, step flow.Step) (string, error) {
	if b == nil || b.service == nil || run == nil {
		return "", fmt.Errorf("runtime service is not available")
	}
	state, ok := run.Steps[step.ID]
	if !ok {
		return "", fmt.Errorf("step %q not found in flow run", step.ID)
	}
	if state.AgentRunID == "" {
		return "", fmt.Errorf("step %q has no agent run id", step.ID)
	}
	agentRun, err := b.service.RunStore().Load(state.AgentRunID)
	if err != nil {
		return "", err
	}
	return agentRun.Status, nil
}

// PolicyValue satisfies flow.PolicyResolver. v0 exposes simple boolean policy
// toggles from flow input so CLI/API callers can drive branches such as
// require_design_approval without a broad policy DSL.
func (b *flowBridge) PolicyValue(_ context.Context, run *flow.Run, key string) (any, bool) {
	if run == nil || run.Input == nil {
		return false, false
	}
	raw, ok := run.Input[strings.TrimSpace(key)]
	if !ok {
		return false, false
	}
	parsed, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return raw, true
	}
	return parsed, true
}

// flowStateRoot returns the directory used for flow run persistence. The
// flow.Store itself appends "flow-runs/<id>" under this root, so this returns
// the state root only (avoiding a doubled "flow-runs/flow-runs" segment).
func (s *Service) flowStateRoot() string {
	if s == nil || s.stateDir == "" {
		return ".xira"
	}
	return s.stateDir
}

// FlowKernel returns (lazily creating) the flow kernel wired to this runtime.
// The kernel is cached on the service for reuse across CLI/API calls.
func (s *Service) FlowKernel() *flow.Kernel {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flowKernel != nil {
		return s.flowKernel
	}
	bridge := newFlowBridge(s)
	store := flow.NewStore(s.flowStateRoot())
	executor := &flow.AgentExecutor{
		Agent:     bridge,
		Human:     bridge,
		Workspace: s.workspace,
	}
	s.flowKernel = &flow.Kernel{
		Store:       store,
		Definitions: s.flows,
		Executor:    executor,
		Policy:      bridge,
		Resolver:    bridge,
		AgentStatus: bridge,
	}
	return s.flowKernel
}

// StartFlow starts a new flow run.
func (s *Service) StartFlow(ctx context.Context, req flow.StartRequest) (*flow.Run, error) {
	return s.FlowKernel().Start(ctx, req)
}

// AdvanceFlow advances a flow run by one step.
func (s *Service) AdvanceFlow(ctx context.Context, flowRunID string) (*flow.Run, error) {
	return s.FlowKernel().Advance(ctx, flowRunID)
}

// ResumeFlow resumes a paused flow run after a HumanRequest is resolved.
func (s *Service) ResumeFlow(ctx context.Context, flowRunID, humanRequestID string) (*flow.Run, error) {
	return s.FlowKernel().Resume(ctx, flowRunID, humanRequestID)
}

// GetFlowRun loads a flow run by id.
func (s *Service) GetFlowRun(ctx context.Context, flowRunID string) (*flow.Run, error) {
	return s.FlowKernel().Store.GetRun(ctx, flowRunID)
}

// FlowRegistry returns the registry of flows discovered from the workspace, or
// nil. It powers flow_id-based starts (via the kernel) and flow list/inspect.
func (s *Service) FlowRegistry() *flow.FlowRegistry {
	if s == nil {
		return nil
	}
	return s.flows
}

// FlowRefs returns the list of discovered flow references for CLI/API listing.
func (s *Service) FlowRefs() []flow.FlowRef {
	if s == nil || s.flows == nil {
		return nil
	}
	return s.flows.List()
}

// FlowStartRequest is an alias for flow.StartRequest exposed on the runtime
// package so callers (CLI, API) can construct start requests without importing
// the flow package directly.
type FlowStartRequest = flow.StartRequest

// FlowRun is an alias for flow.Run exposed so CLI/API callers can decode run
// state without importing the flow package directly.
type FlowRun = flow.Run

// FlowRunView is a serializable projection of flow.Run for CLI/API consumers
// that want stable field names without depending on internal struct tags.
type FlowRunView = flow.Run

// FlowRef is an alias for flow.FlowRef exposed so CLI/API callers can decode
// registry listing output without importing the flow package directly.
type FlowRef = flow.FlowRef
