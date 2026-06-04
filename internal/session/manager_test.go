package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	turn := AgentTurnInput{
		SessionID:      "conversation:abc123",
		AgentID:        "yangsheng-yihao",
		AgentSessionID: "session:yangsheng-yihao:def456",
		RunID:          "run-1",
		Context:        ctx,
		Scope:          &scope,
		UserMessage:    "hi",
		AssistantReply: "hello",
	}
	if err := manager.AppendAgentTurn(turn); err != nil {
		t.Fatal(err)
	}

	conversationDir := filepath.Join(root, "feishu", "feishu-yihao", conversationFolderName(turn))
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

func TestManagerPersistsReadableIlinkSessionPath(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManagerWithStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := channel.NewInboundContextWithEntrypoint("ilink", "ilink-wechat", "wxid-user", map[string]string{
		"chat_id":    "group-1",
		"chat_type":  "group",
		"space_id":   "group-1",
		"space_type": "group",
	})
	turn := AgentTurnInput{
		SessionID:      "conversation:ilink123",
		AgentID:        "xira-assistant",
		AgentSessionID: "session:xira-assistant:abc",
		Context:        ctx,
		UserMessage:    "hi",
		AssistantReply: "hello",
	}
	if err := manager.AppendAgentTurn(turn); err != nil {
		t.Fatal(err)
	}

	conversationDir := filepath.Join(root, "ilink", "ilink-wechat", conversationFolderName(turn))
	if _, err := os.Stat(filepath.Join(conversationDir, "agents", "xira-assistant", "messages.jsonl")); err != nil {
		t.Fatalf("expected readable ilink session path: %v", err)
	}
	if got := filepath.Base(conversationDir); got != "chat_group_group-1__sender_wxid-user__conversation_ilink123" {
		t.Fatalf("conversation dir = %q", got)
	}
}

func TestConversationFolderNameOnlyUsesSessionScopeDimensions(t *testing.T) {
	ctx := channel.NewInboundContextWithEntrypoint("feishu", "feishu-default", "sender-1", map[string]string{
		"chat_id":   "chat-1",
		"chat_type": "group",
	})
	got := conversationFolderName(AgentTurnInput{
		SessionID: "conversation:channel123",
		Context:   ctx,
		Scope: &SessionScope{
			Version:    ScopeVersionV1,
			Channel:    "feishu",
			Dimensions: []string{"channel"},
			Values:     map[string]string{"channel": "channel:feishu"},
		},
	})
	if got != "conversation_channel123" {
		t.Fatalf("conversation folder = %q, want only session id for channel-scoped session", got)
	}
}

func TestManagerLoadsLegacyFlatConversationSessionPath(t *testing.T) {
	root := t.TempDir()
	conversationDir := filepath.Join(root, safePathID("conversation:legacy123"))
	agentDir := filepath.Join(conversationDir, "agents", "xira-assistant")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	if err := writeJSONAtomic(filepath.Join(conversationDir, "meta.json"), ConversationMeta{
		Version:     fileStoreVersion,
		SessionID:   "conversation:legacy123",
		Channel:     "feishu",
		ChatID:      "chat-1",
		ChatType:    "direct",
		SenderID:    "sender-1",
		CreatedAt:   now,
		UpdatedAt:   now,
		LastAgentID: "xira-assistant",
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(agentDir, "meta.json"), AgentMeta{
		Version:        fileStoreVersion,
		SessionID:      "conversation:legacy123",
		AgentID:        "xira-assistant",
		AgentSessionID: "session:xira-assistant:legacy",
		MessageCount:   2,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendJSONLines(filepath.Join(agentDir, "messages.jsonl"), []Message{
		{Role: "user", Content: "old hi", CreatedAt: now, AgentID: "xira-assistant"},
		{Role: "assistant", Content: "old hello", CreatedAt: now.Add(time.Nanosecond), AgentID: "xira-assistant"},
	}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewManagerWithStore(root)
	if err != nil {
		t.Fatal(err)
	}
	history := reloaded.History("conversation:legacy123")
	if len(history) != 2 || history[0].Content != "old hi" || history[1].Content != "old hello" {
		t.Fatalf("legacy history = %+v", history)
	}
}
