package session

import (
	"testing"

	"github.com/ai-daming/flowdeck/internal/channel"
	"github.com/ai-daming/flowdeck/internal/routing"
)

func TestManagerAllocatesStableScopedSession(t *testing.T) {
	manager := NewManager()
	ctx := channel.NewInboundContext("Feishu", "sender-1", map[string]string{
		"account":   "tenant-a",
		"chat_id":   "chat-1",
		"chat_type": "group",
	})
	input := AllocationInput{
		AgentID:       "research-assistant",
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
