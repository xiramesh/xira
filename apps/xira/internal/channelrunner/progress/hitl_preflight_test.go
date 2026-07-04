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
		{ID: "hrq_tool", Source: "unsupported_source", Kind: humanrequest.RequestApproval},
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

// TestTryResolveHITLAgentRequestOptionMatch: agent_request 带 options 时,用户
// 文本命中 option → 结构化 resolve(信号=option id)。对齐 flow_human_approval。
func TestTryResolveHITLAgentRequestOptionMatch(t *testing.T) {
	resolver := &fakeHITLResolver{pending: []humanrequest.HumanRequest{{
		ID:      "hrq_agent_choice",
		Source:  "agent_request",
		Kind:    humanrequest.RequestFreeform,
		Options: []humanrequest.HumanOption{{ID: "minimal_fix", Label: "最小修复"}, {ID: "refactor", Label: "重构"}},
	}}}
	ok := TryResolveHITL(context.Background(), resolver, runtime.ChatKey{Channel: "feishu", ChatID: "oc", SenderID: "u"}, "最小修复", "u")
	if !ok {
		t.Fatal("TryResolveHITL returned false for agent_request option label match")
	}
	if resolver.resolvedID != "hrq_agent_choice" {
		t.Fatalf("resolved id = %q, want hrq_agent_choice", resolver.resolvedID)
	}
	// 命中 label → 归一化成 option id
	if resolver.resolvedReq.Message != "minimal_fix" {
		t.Fatalf("resolved message = %q, want normalized option id minimal_fix", resolver.resolvedReq.Message)
	}
	if resolver.resolvedReq.Kind != humanrequest.ResponseAnswer {
		t.Fatalf("resolved kind = %v, want answer", resolver.resolvedReq.Kind)
	}
}

// TestTryResolveHITLAgentRequestOptionMatchByID: agent_request option 按 id 命中。
func TestTryResolveHITLAgentRequestOptionMatchByID(t *testing.T) {
	resolver := &fakeHITLResolver{pending: []humanrequest.HumanRequest{{
		ID:      "hrq_agent_id",
		Source:  "agent_request",
		Kind:    humanrequest.RequestFreeform,
		Options: []humanrequest.HumanOption{{ID: "opt_a", Label: "Option A"}},
	}}}
	ok := TryResolveHITL(context.Background(), resolver, runtime.ChatKey{Channel: "feishu", ChatID: "oc", SenderID: "u"}, "opt_a", "u")
	if !ok {
		t.Fatal("TryResolveHITL returned false for agent_request option id match")
	}
	if resolver.resolvedReq.Message != "opt_a" {
		t.Fatalf("resolved message = %q, want opt_a", resolver.resolvedReq.Message)
	}
}

// TestTryResolveHITLAgentRequestOptionMissFallsThrough: agent_request 带 options,
// 用户文本没命中任何 option(如"行")→ 回 false,进 agent turn(走 #106/#107)。
// 这是 #108 的核心:不再把任何文本当 answer resolve。
func TestTryResolveHITLAgentRequestOptionMissFallsThrough(t *testing.T) {
	resolver := &fakeHITLResolver{pending: []humanrequest.HumanRequest{{
		ID:      "hrq_agent_miss",
		Source:  "agent_request",
		Kind:    humanrequest.RequestFreeform,
		Options: []humanrequest.HumanOption{{ID: "approve", Label: "允许"}, {ID: "deny", Label: "拒绝"}},
	}}}
	ok := TryResolveHITL(context.Background(), resolver, runtime.ChatKey{Channel: "feishu", ChatID: "oc", SenderID: "u"}, "行", "u")
	if ok {
		t.Fatal("TryResolveHITL returned true for agent_request option miss — should fall through to agent turn")
	}
	if resolver.resolvedID != "" {
		t.Fatalf("should not resolve on option miss, got resolvedID=%q", resolver.resolvedID)
	}
}

// TestTryResolveHITLAgentRequestNoOptionsStillResolves: agent_request 不带 options
// (纯 freeform 问答)→ 维持旧行为,文本当 answer resolve(#92 行为不变)。
// 这类没有"结构化选项"可匹配,必须进 agent 理解或直接 answer。
func TestTryResolveHITLAgentRequestNoOptionsStillResolves(t *testing.T) {
	resolver := &fakeHITLResolver{pending: []humanrequest.HumanRequest{{
		ID:     "hrq_agent_freeform",
		Source: "agent_request",
		Kind:   humanrequest.RequestFreeform,
	}}}
	ok := TryResolveHITL(context.Background(), resolver, runtime.ChatKey{Channel: "feishu", ChatID: "oc", SenderID: "u"}, "Tuesday window please", "u")
	if !ok {
		t.Fatal("TryResolveHITL returned false for agent_request without options — should resolve as freeform answer")
	}
	if resolver.resolvedReq.Message != "Tuesday window please" {
		t.Fatalf("resolved message = %q, want the freeform text", resolver.resolvedReq.Message)
	}
}
