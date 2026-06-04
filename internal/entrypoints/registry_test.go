package entrypoints

import (
	"testing"

	"github.com/ai-daming/xira/internal/channel"
	"github.com/ai-daming/xira/internal/routing"
)

func TestRegistryUsesRequestedAgentWhenAllowed(t *testing.T) {
	registry := NewRegistry("xira-assistant", []Definition{
		{
			ID:              "feishu-expense-bot",
			Channel:         "feishu",
			AppID:           "cli-expense",
			DefaultAgentID:  "expense-agent",
			AllowedAgentIDs: []string{"expense-agent", "research-assistant"},
			SessionPolicy:   routing.SessionPolicy{Dimensions: []string{"chat", "sender"}},
		},
	})

	decision, err := registry.Resolve(ResolveInput{
		Context:          channel.NewInboundContext("feishu", "ou-1", map[string]string{"app_id": "cli-expense", "chat_id": "oc-1"}),
		RequestedAgentID: "research-assistant",
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Definition.ID != "feishu-expense-bot" {
		t.Fatalf("entrypoint = %q", decision.Definition.ID)
	}
	if decision.AgentID != "research-assistant" {
		t.Fatalf("agent = %q", decision.AgentID)
	}
	if decision.MatchedBy != "request.agent_id" {
		t.Fatalf("matched by = %q", decision.MatchedBy)
	}
}

func TestRegistryRejectsDisallowedRequestedAgent(t *testing.T) {
	registry := NewRegistry("xira-assistant", []Definition{
		{
			ID:              "feishu-expense-bot",
			Channel:         "feishu",
			DefaultAgentID:  "expense-agent",
			AllowedAgentIDs: []string{"expense-agent"},
		},
	})

	_, err := registry.Resolve(ResolveInput{
		Context:          channel.NewInboundContext("feishu", "ou-1", nil),
		EntrypointID:     "feishu-expense-bot",
		RequestedAgentID: "research-assistant",
	})
	if err == nil {
		t.Fatal("expected disallowed agent error")
	}
}

func TestRegistryMatchesMultipleEntrypointsForSameChannelByAppAndBot(t *testing.T) {
	registry := NewRegistry("xira-assistant", []Definition{
		{
			ID:             "feishu-default",
			Channel:        "feishu",
			DefaultAgentID: "xira-assistant",
		},
		{
			ID:             "feishu-expense-bot",
			Channel:        "feishu",
			AppID:          "cli-expense",
			BotID:          "bot-expense",
			DefaultAgentID: "expense-agent",
		},
		{
			ID:             "feishu-leave-bot",
			Channel:        "feishu",
			AppID:          "cli-leave",
			BotID:          "bot-leave",
			DefaultAgentID: "leave-agent",
		},
	})

	ctx := channel.NewInboundContext("feishu", "ou-1", map[string]string{
		"app_id":  "cli-leave",
		"bot_id":  "bot-leave",
		"chat_id": "oc-1",
	})
	decision, err := registry.Resolve(ResolveInput{Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Definition.ID != "feishu-leave-bot" {
		t.Fatalf("entrypoint = %q", decision.Definition.ID)
	}
	if decision.AgentID != "leave-agent" {
		t.Fatalf("agent = %q", decision.AgentID)
	}
}

func TestRegistryUsesImplicitEntrypointWhenUnconfigured(t *testing.T) {
	registry := NewRegistry("xira-assistant", nil)

	decision, err := registry.Resolve(ResolveInput{
		Context: channel.NewInboundContext("ilink", "wx-1", map[string]string{"chat_id": "room-1"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Definition.ID != "ilink-default" {
		t.Fatalf("entrypoint = %q", decision.Definition.ID)
	}
	if decision.Definition.Channel != "ilink" {
		t.Fatalf("channel = %q", decision.Definition.Channel)
	}
	if decision.AgentID != "xira-assistant" {
		t.Fatalf("agent = %q", decision.AgentID)
	}
}
