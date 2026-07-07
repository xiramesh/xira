package entrypoints

import (
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/routing"
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

// TestDefinitionAllowsSender covers the AllowsSender contract: glob matching
// via path.Match, empty = allow-all, empty sender rejected, bad pattern
// skipped (not fail-open). See #121.
func TestDefinitionAllowsSender(t *testing.T) {
	cases := []struct {
		name     string
		senders  []string
		senderID string
		want     bool
	}{
		{"empty allowlist allows any non-empty sender", nil, "ou_anyone", true},
		{"star matches any non-empty sender", []string{"*"}, "ou_anyone", true},
		{"exact match passes", []string{"ou_abc"}, "ou_abc", true},
		{"exact mismatch rejects", []string{"ou_abc"}, "ou_def", false},
		{"glob prefix matches (future expansion)", []string{"ou_*"}, "ou_abc", true},
		{"glob prefix non-match rejects", []string{"ou_*"}, "wxid_abc", false},
		{"empty senderID rejected even with star", []string{"*"}, "", false},
		{"empty senderID rejected with empty allowlist too", nil, "", false},
		{"malformed pattern skipped not fail-open", []string{"[bad"}, "ou_abc", false},
		{"first match wins (star + exact)", []string{"*", "ou_abc"}, "anyone", true},
		{"senderID trimmed before match", []string{"ou_abc"}, "  ou_abc  ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Definition{AllowedSenderIDs: c.senders}
			if got := d.AllowsSender(c.senderID); got != c.want {
				t.Errorf("AllowsSender(%q) with senders=%v = %v, want %v", c.senderID, c.senders, got, c.want)
			}
		})
	}
}

// TestNormalizeDefinitionDedupesAllowedSenderIDs verifies normalizeDefinition
// trims, dedupes, and drops empty entries — mirroring AllowedAgentIDs handling.
func TestNormalizeDefinitionDedupesAllowedSenderIDs(t *testing.T) {
	raw := Definition{
		AllowedSenderIDs: []string{
			"ou_abc",
			"  ou_abc  ", // dup after trim
			"",
			"   ",
			"ou_def",
			"*",
		},
	}
	normalized := normalizeDefinition(raw, "xira-assistant")
	want := []string{"ou_abc", "ou_def", "*"}
	if len(normalized.AllowedSenderIDs) != len(want) {
		t.Fatalf("got %v, want %v", normalized.AllowedSenderIDs, want)
	}
	for i, v := range want {
		if normalized.AllowedSenderIDs[i] != v {
			t.Errorf("AllowedSenderIDs[%d] = %q, want %q (full: %v)", i, normalized.AllowedSenderIDs[i], v, normalized.AllowedSenderIDs)
		}
	}
}

// TestNormalizeDefinitionEmptyAllowedSenderIDs verifies empty stays empty (not nil-ish junk).
func TestNormalizeDefinitionEmptyAllowedSenderIDs(t *testing.T) {
	normalized := normalizeDefinition(Definition{}, "xira-assistant")
	if len(normalized.AllowedSenderIDs) != 0 {
		t.Errorf("empty input should stay empty, got %v", normalized.AllowedSenderIDs)
	}
}
