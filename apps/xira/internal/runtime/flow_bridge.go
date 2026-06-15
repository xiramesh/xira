package runtime

import (
	"context"
	"fmt"
	"path/filepath"

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
	resp, err := b.service.RunAgent(ctx, TurnRequest{
		AgentID:      req.AgentID,
		EntrypointID: req.EntrypointID,
		Message:      req.Message,
		UserID:       req.UserID,
		SessionID:    req.SessionID,
		Channel:      req.Channel,
		Metadata:     req.Metadata,
	})
	if err != nil {
		return flow.AgentTurnResponse{}, err
	}
	return mapTurnResponseToFlow(resp), nil
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
		Source:      input.Source,
		Kind:        kind,
		Question:    input.Question,
		Options:     options,
		DedupeKey:   input.DedupeKey,
		Metadata:    input.Metadata,
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
	}
	return out, nil
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

// flowStateRoot returns the directory used for flow run persistence.
func (s *Service) flowStateRoot() string {
	if s == nil || s.stateRoot == "" {
		return filepath.Join(".xira", "flow-runs")
	}
	return filepath.Join(s.stateRoot, "flow-runs")
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
		Executor:    executor,
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
