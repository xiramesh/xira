package session

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestManagerPersistsLayeredAgentSession(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManagerWithStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := channel.NewInboundContextWithEntrypoint("feishu", "feishu-yihao", "ou-1", map[string]string{
		"app_id":    "cli-1",
		"chat_id":   "oc-1",
		"chat_type": "direct",
	})
	scope := SessionScope{
		Version:      ScopeVersionV1,
		EntrypointID: "feishu-yihao",
		Channel:      "feishu",
		Dimensions:   []string{"chat", "sender"},
		Values: map[string]string{
			"chat":   "direct:oc-1",
			"sender": "feishu:ou-1",
		},
	}
	if err := manager.AppendAgentTurn(AgentTurnInput{
		SessionID:      "conversation:abc123",
		AgentID:        "yangsheng-yihao",
		AgentSessionID: "session:yangsheng-yihao:def456",
		RunID:          "run-1",
		Context:        ctx,
		Scope:          &scope,
		UserMessage:    "hi",
		AssistantReply: "hello",
	}); err != nil {
		t.Fatal(err)
	}

	conversationDir := filepath.Join(root, safePathID("conversation:abc123"))
	agentDir := filepath.Join(conversationDir, "agents", safePathID("yangsheng-yihao"))
	for _, path := range []string{
		filepath.Join(conversationDir, "meta.json"),
		filepath.Join(agentDir, "meta.json"),
		filepath.Join(agentDir, "messages.jsonl"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected %s: %v", path, err)
		}
	}

	var conversationMeta ConversationMeta
	content, err := os.ReadFile(filepath.Join(conversationDir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &conversationMeta); err != nil {
		t.Fatal(err)
	}
	if conversationMeta.SessionID != "conversation:abc123" || conversationMeta.LastAgentID != "yangsheng-yihao" {
		t.Fatalf("conversation meta = %+v", conversationMeta)
	}
	if conversationMeta.ChatID != "oc-1" || conversationMeta.SenderID != "ou-1" {
		t.Fatalf("conversation context meta = %+v", conversationMeta)
	}

	var agentMeta AgentMeta
	content, err = os.ReadFile(filepath.Join(agentDir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &agentMeta); err != nil {
		t.Fatal(err)
	}
	if agentMeta.AgentSessionID != "session:yangsheng-yihao:def456" || agentMeta.MessageCount != 2 {
		t.Fatalf("agent meta = %+v", agentMeta)
	}

	reloaded, err := NewManagerWithStore(root)
	if err != nil {
		t.Fatal(err)
	}
	conversationHistory := reloaded.History("conversation:abc123")
	if len(conversationHistory) != 2 {
		t.Fatalf("conversation history len = %d, want 2: %+v", len(conversationHistory), conversationHistory)
	}
	agentHistory := reloaded.AgentHistory("conversation:abc123", "yangsheng-yihao")
	if len(agentHistory) != 2 {
		t.Fatalf("agent history len = %d, want 2: %+v", len(agentHistory), agentHistory)
	}
	if agentHistory[0].Role != "user" || agentHistory[0].Content != "hi" || agentHistory[1].Role != "assistant" {
		t.Fatalf("agent history = %+v", agentHistory)
	}
}
