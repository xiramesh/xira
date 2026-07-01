package progress

import (
	"context"
	"testing"

	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/runtime"
)

type fakeHITLResolver struct {
	pending     []humanrequest.HumanRequest
	resolvedID  string
	resolvedReq humanrequest.ResolveRequest
}

func (f *fakeHITLResolver) ListPendingHumanRequestsByChatKey(ctx context.Context, chatKey string) ([]humanrequest.HumanRequest, error) {
	return append([]humanrequest.HumanRequest(nil), f.pending...), nil
}

func (f *fakeHITLResolver) ResolveHumanRequest(ctx context.Context, requestID string, input humanrequest.ResolveRequest) (*humanrequest.HumanRequest, error) {
	f.resolvedID = requestID
	f.resolvedReq = input
	return &humanrequest.HumanRequest{ID: requestID, Status: humanrequest.StatusResolved, Response: &humanrequest.HumanResponse{Kind: input.Kind, Message: input.Message}}, nil
}

func TestTryResolveHITLResolvesFlowHumanApprovalFromIMText(t *testing.T) {
	resolver := &fakeHITLResolver{pending: []humanrequest.HumanRequest{{
		ID:      "hrq_flow",
		Source:  "flow_human_approval",
		Kind:    humanrequest.RequestApproval,
		Options: []humanrequest.HumanOption{{ID: "approve", Label: "approve"}, {ID: "revise", Label: "revise"}, {ID: "cancel", Label: "cancel"}},
	}}}
	ok := TryResolveHITL(context.Background(), resolver, runtime.ChatKey{Channel: "feishu", ChatID: "oc", SenderID: "u"}, " revise ", "u")
	if !ok {
		t.Fatal("TryResolveHITL returned false, want true for flow_human_approval")
	}
	if resolver.resolvedID != "hrq_flow" {
		t.Fatalf("resolved id = %q, want hrq_flow", resolver.resolvedID)
	}
	if resolver.resolvedReq.Kind != humanrequest.ResponseAnswer || resolver.resolvedReq.Message != "revise" {
		t.Fatalf("resolved request = %+v, want answer/revise", resolver.resolvedReq)
	}
}

func TestTryResolveHITLPrefersMostRecentIMResolvableRequest(t *testing.T) {
	resolver := &fakeHITLResolver{pending: []humanrequest.HumanRequest{
		{ID: "hrq_tool", Source: "runtime_tool_gate", Kind: humanrequest.RequestApproval},
		{ID: "hrq_flow", Source: "flow_human_approval", Kind: humanrequest.RequestApproval, Options: []humanrequest.HumanOption{{ID: "approve", Label: "approve"}}},
		{ID: "hrq_agent", Source: "agent_request", Kind: humanrequest.RequestFreeform},
	}}
	ok := TryResolveHITL(context.Background(), resolver, runtime.ChatKey{Channel: "ilink", ChatID: "c", SenderID: "u"}, "approve", "u")
	if !ok {
		t.Fatal("TryResolveHITL returned false, want true")
	}
	if resolver.resolvedID != "hrq_flow" {
		t.Fatalf("resolved id = %q, want first IM-resolvable pending request hrq_flow", resolver.resolvedID)
	}
}

func TestTryResolveHITLDoesNotResolveFlowHumanApprovalOnEmptyText(t *testing.T) {
	resolver := &fakeHITLResolver{pending: []humanrequest.HumanRequest{{
		ID:      "hrq_flow",
		Source:  "flow_human_approval",
		Kind:    humanrequest.RequestApproval,
		Options: []humanrequest.HumanOption{{ID: "approve", Label: "approve"}},
	}}}
	ok := TryResolveHITL(context.Background(), resolver, runtime.ChatKey{Channel: "feishu", ChatID: "oc", SenderID: "u"}, "  ", "u")
	if ok {
		t.Fatal("TryResolveHITL returned true for empty flow approval text")
	}
	if resolver.resolvedID != "" {
		t.Fatalf("resolved id = %q, want no resolve", resolver.resolvedID)
	}
}

func TestTryResolveHITLDoesNotResolveFlowHumanApprovalOnUnknownOption(t *testing.T) {
	resolver := &fakeHITLResolver{pending: []humanrequest.HumanRequest{{
		ID:      "hrq_flow",
		Source:  "flow_human_approval",
		Kind:    humanrequest.RequestApproval,
		Options: []humanrequest.HumanOption{{ID: "approve", Label: "approve"}, {ID: "revise", Label: "revise"}, {ID: "cancel", Label: "cancel"}},
	}}}
	ok := TryResolveHITL(context.Background(), resolver, runtime.ChatKey{Channel: "feishu", ChatID: "oc", SenderID: "u"}, "approved", "u")
	if ok {
		t.Fatal("TryResolveHITL returned true for unknown flow approval option")
	}
	if resolver.resolvedID != "" {
		t.Fatalf("resolved id = %q, want no resolve", resolver.resolvedID)
	}
}

func TestTryResolveHITLNormalizesFlowHumanApprovalOption(t *testing.T) {
	resolver := &fakeHITLResolver{pending: []humanrequest.HumanRequest{{
		ID:      "hrq_flow",
		Source:  "flow_human_approval",
		Kind:    humanrequest.RequestApproval,
		Options: []humanrequest.HumanOption{{ID: "approve", Label: "Approve"}},
	}}}
	ok := TryResolveHITL(context.Background(), resolver, runtime.ChatKey{Channel: "feishu", ChatID: "oc", SenderID: "u"}, " APPROVE ", "u")
	if !ok {
		t.Fatal("TryResolveHITL returned false, want true")
	}
	if resolver.resolvedReq.Message != "approve" {
		t.Fatalf("resolved message = %q, want normalized option id approve", resolver.resolvedReq.Message)
	}
}
