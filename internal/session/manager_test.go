package session

import (
	"testing"

	"github.com/ai-daming/xira/internal/channel"
	"github.com/ai-daming/xira/internal/routing"
)

func TestManagerAllocatesStableScopedSession(t *testing.T) {
	manager := NewManager()
	ctx := channel.NewInboundContext("Feishu", "sender-1", map[string]string{
		"account":   "tenant-a",
		"chat_id":   "chat-1",
		"chat_type": "group",
	})
	input := AllocationInput{
		Context:       ctx,
		SessionPolicy: routing.SessionPolicy{Dimensions: []string{"chat", "sender"}},
	}

	first := manager.Allocate(input)
	second := manager.Allocate(input)

	if first.SessionID == "" {
		t.Fatal("session id is empty")
	}
	if first.SessionID != second.SessionID {
		t.Fatalf("session id changed: %q != %q", first.SessionID, second.SessionID)
	}
	if first.Scope.Channel != "feishu" || first.Scope.Account != "tenant-a" {
		t.Fatalf("scope channel/account mismatch: %+v", first.Scope)
	}
	if first.Scope.Values["chat"] != "group:chat-1" {
		t.Fatalf("chat scope = %q, want group:chat-1", first.Scope.Values["chat"])
	}
	if first.Scope.Values["sender"] != "feishu:sender-1" {
		t.Fatalf("sender scope = %q, want feishu:sender-1", first.Scope.Values["sender"])
	}
}

func TestManagerConversationSessionIncludesEntrypoint(t *testing.T) {
	manager := NewManager()
	base := map[string]string{
		"account":   "tenant-a",
		"chat_id":   "chat-1",
		"chat_type": "group",
	}
	expense := channel.NewInboundContextWithEntrypoint("feishu", "feishu-expense-bot", "sender-1", base)
	leave := channel.NewInboundContextWithEntrypoint("feishu", "feishu-leave-bot", "sender-1", base)
	policy := routing.SessionPolicy{Dimensions: []string{"chat", "sender"}}

	first := manager.Allocate(AllocationInput{Context: expense, SessionPolicy: policy})
	second := manager.Allocate(AllocationInput{Context: leave, SessionPolicy: policy})

	if first.SessionID == second.SessionID {
		t.Fatalf("conversation session should differ across entrypoints: %q", first.SessionID)
	}
	if first.Scope.EntrypointID != "feishu-expense-bot" {
		t.Fatalf("entrypoint = %q", first.Scope.EntrypointID)
	}
}

func TestAgentSessionIDIsDerivedFromConversationAndAgent(t *testing.T) {
	manager := NewManager()
	ctx := channel.NewInboundContext("feishu", "sender-1", map[string]string{
		"account":   "tenant-a",
		"chat_id":   "chat-1",
		"chat_type": "group",
	})
	allocation := manager.Allocate(AllocationInput{
		Context:       ctx,
		SessionPolicy: routing.SessionPolicy{Dimensions: []string{"chat", "sender"}},
	})

	first := BuildAgentSessionID(allocation.SessionID, "xira-assistant")
	second := BuildAgentSessionID(allocation.SessionID, "research-assistant")
	again := BuildAgentSessionID(allocation.SessionID, "xira-assistant")

	if first == "" || second == "" {
		t.Fatal("agent session id is empty")
	}
	if first == second {
		t.Fatalf("agent sessions should differ for different agents: %q", first)
	}
	if first != again {
		t.Fatalf("agent session id changed: %q != %q", first, again)
	}
}

func TestManagerConversationHistoryReturnsCopy(t *testing.T) {
	manager := NewManager()
	manager.AppendTurn("session-1", "hi", "hello")

	history := manager.History("session-1")
	history[0].Content = "mutated"
	got := manager.History("session-1")

	if got[0].Content != "hi" {
		t.Fatalf("history content = %q, want hi", got[0].Content)
	}
}
