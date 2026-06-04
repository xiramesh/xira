package routing

import (
	"testing"

	"github.com/ai-daming/xira/internal/channel"
)

func TestRouterReturnsDefaultAgentAndSessionPolicy(t *testing.T) {
	router := NewRouter("research-assistant")
	decision := router.Resolve(channel.NewInboundContext("xiragarden", "local-user", nil), "")

	if decision.AgentID != "research-assistant" {
		t.Fatalf("agent id = %q, want research-assistant", decision.AgentID)
	}
	if decision.MatchedBy != "default" {
		t.Fatalf("matched by = %q, want default", decision.MatchedBy)
	}
	if len(decision.SessionPolicy.Dimensions) != 2 {
		t.Fatalf("dimensions = %+v, want default chat/sender dimensions", decision.SessionPolicy.Dimensions)
	}
}

func TestRouterPreservesExplicitAgentRequest(t *testing.T) {
	router := NewRouter("research-assistant")
	decision := router.Resolve(channel.NewInboundContext("xiragarden", "local-user", nil), "lead-research")

	if decision.AgentID != "lead-research" {
		t.Fatalf("agent id = %q, want lead-research", decision.AgentID)
	}
	if decision.MatchedBy != "request.agent_id" {
		t.Fatalf("matched by = %q, want request.agent_id", decision.MatchedBy)
	}
}

func TestRouterMatchesChannelRule(t *testing.T) {
	router := NewRouterWithRules("xira-assistant", []Rule{
		{
			Channel: "research",
			AgentID: "research-assistant",
			SessionPolicy: SessionPolicy{
				Dimensions: []string{"channel"},
			},
		},
	})
	decision := router.Resolve(channel.NewInboundContext("research", "local-user", nil), "")

	if decision.AgentID != "research-assistant" {
		t.Fatalf("agent id = %q, want research-assistant", decision.AgentID)
	}
	if decision.MatchedBy != "route.channel" {
		t.Fatalf("matched by = %q, want route.channel", decision.MatchedBy)
	}
	if len(decision.SessionPolicy.Dimensions) != 1 || decision.SessionPolicy.Dimensions[0] != "channel" {
		t.Fatalf("dimensions = %+v", decision.SessionPolicy.Dimensions)
	}
}
