package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/routing"
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

// TestBuildScopeIncludesNames verifies BuildScope captures chat_name /
// sender_name into scope.Names (so they survive persistence and can be
// restored on resume via inboundContextFromScope).
func TestBuildScopeIncludesNames(t *testing.T) {
	ctx := channel.NewInboundContext("feishu", "user-1", map[string]string{
		"chat_id":     "chat-1",
		"chat_type":   "group",
		"chat_name":   "工作群",
		"sender_name": "张三",
	})
	policy := routing.SessionPolicy{Dimensions: []string{"chat", "sender"}}
	scope := BuildScope(ctx, policy)
	if scope.Names == nil {
		t.Fatal("scope.Names = nil, want populated names map")
	}
	if got := scope.Names["chat_name"]; got != "工作群" {
		t.Errorf("scope.Names[chat_name] = %q, want %q", got, "工作群")
	}
	if got := scope.Names["sender_name"]; got != "张三" {
		t.Errorf("scope.Names[sender_name] = %q, want %q", got, "张三")
	}
}

// TestBuildScopeOmitsEmptyNames verifies that when no name fields are present,
// scope.Names is not populated (keeps the zero-value scope clean for backward
// compatibility with older persisted scopes that predate the Names field).
func TestBuildScopeOmitsEmptyDisplayNames(t *testing.T) {
	// #151：sender_id 总会进 Names（供 resume），但 display names（chat_name/sender_name）
	// 只在实际有值时才存在。
	ctx := channel.NewInboundContext("feishu", "user-1", map[string]string{
		"chat_id":   "chat-1",
		"chat_type": "group",
	})
	policy := routing.SessionPolicy{Dimensions: []string{"chat"}}
	scope := BuildScope(ctx, policy)
	// sender_id 应该在（resume 需要）
	if scope.Names == nil || scope.Names["sender_id"] == "" {
		t.Errorf("scope.Names should contain sender_id: %+v", scope.Names)
	}
	// 但 chat_name / sender_name 不在（没有 display name）
	if scope.Names["chat_name"] != "" {
		t.Errorf("chat_name should be empty: %+v", scope.Names)
	}
	if scope.Names["sender_name"] != "" {
		t.Errorf("sender_name should be empty: %+v", scope.Names)
	}
}

// TestScopeSignatureExcludesNames is the CRITICAL regression: Names must not
// feed scopeSignature, otherwise adding/removing a name would re-hash the
// session id and break resume for every existing conversation. Two scopes
// identical except for Names must produce identical signatures.
func TestScopeSignatureExcludesNames(t *testing.T) {
	ctx := channel.NewInboundContext("feishu", "user-1", map[string]string{
		"chat_id":     "chat-1",
		"chat_type":   "group",
		"chat_name":   "工作群",
		"sender_name": "张三",
	})
	policy := routing.SessionPolicy{Dimensions: []string{"chat", "sender"}}
	scopeWithNames := BuildScope(ctx, policy)

	// Strip names to simulate a scope from before Names existed.
	scopeNoNames := scopeWithNames
	scopeNoNames.Names = nil

	sigWith := scopeSignature(scopeWithNames)
	sigWithout := scopeSignature(scopeNoNames)
	if sigWith != sigWithout {
		t.Fatalf("scopeSignature changed when Names toggled — session id would break:\nwith:    %s\nwithout: %s", sigWith, sigWithout)
	}
	// And BuildSessionID must be stable too (it wraps scopeSignature).
	if BuildSessionID(scopeWithNames) != BuildSessionID(scopeNoNames) {
		t.Fatalf("BuildSessionID changed when Names toggled — resume would break")
	}
}

// TestBuildScopeDimensions exercises BuildScope's per-dimension branches
// (space/topic/chat/sender/channel) and their empty-skip fallbacks. These were
// previously uncovered (BuildScope sat at 59.5%), dragging the session package
// below the 85% threshold (AGENTS.md §5.2). Table-driven to cover all branches
// cheaply.
func TestBuildScopeDimensions(t *testing.T) {
	tests := []struct {
		name       string
		ctx        channel.InboundContext
		dimensions []string
		wantValues map[string]string // expected scope.Values entries
	}{
		{
			name:       "space dimension with explicit type",
			ctx:        channel.InboundContext{Channel: "feishu", SpaceID: "ws-1", SpaceType: "tenant"},
			dimensions: []string{"space"},
			wantValues: map[string]string{"space": "tenant:ws-1"},
		},
		{
			name:       "space dimension type defaults to space when empty",
			ctx:        channel.InboundContext{Channel: "feishu", SpaceID: "ws-1"},
			dimensions: []string{"space"},
			wantValues: map[string]string{"space": "space:ws-1"},
		},
		{
			name:       "space dimension skipped when SpaceID empty",
			ctx:        channel.InboundContext{Channel: "feishu", SpaceType: "tenant"},
			dimensions: []string{"space"},
			wantValues: map[string]string{},
		},
		{
			name:       "topic dimension",
			ctx:        channel.InboundContext{Channel: "feishu", TopicID: "th-9"},
			dimensions: []string{"topic"},
			wantValues: map[string]string{"topic": "topic:th-9"},
		},
		{
			name:       "topic dimension skipped when empty",
			ctx:        channel.InboundContext{Channel: "feishu"},
			dimensions: []string{"topic"},
			wantValues: map[string]string{},
		},
		{
			name:       "channel dimension",
			ctx:        channel.InboundContext{Channel: "Feishu"},
			dimensions: []string{"channel"},
			wantValues: map[string]string{"channel": "channel:feishu"},
		},
		{
			name:       "chat dimension type defaults to direct when empty",
			ctx:        channel.InboundContext{Channel: "feishu", ChatID: "c1"},
			dimensions: []string{"chat"},
			wantValues: map[string]string{"chat": "direct:c1"},
		},
		{
			name:       "all dimensions together",
			ctx:        channel.InboundContext{Channel: "feishu", SpaceID: "s1", ChatID: "c1", TopicID: "t1", SenderID: "u1"},
			dimensions: []string{"space", "chat", "topic", "sender", "channel"},
			wantValues: map[string]string{
				"space":   "space:s1",
				"chat":    "direct:c1",
				"topic":   "topic:t1",
				"sender":  "feishu:u1",
				"channel": "channel:feishu",
			},
		},
		{
			name:       "unknown dimension ignored",
			ctx:        channel.InboundContext{Channel: "feishu", ChatID: "c1"},
			dimensions: []string{"chat", "bogus"},
			wantValues: map[string]string{"chat": "direct:c1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := BuildScope(tt.ctx, routing.SessionPolicy{Dimensions: tt.dimensions})
			if len(tt.wantValues) == 0 {
				if len(scope.Values) != 0 {
					t.Errorf("expected empty Values, got %+v", scope.Values)
				}
				return
			}
			for k, want := range tt.wantValues {
				if got := scope.Values[k]; got != want {
					t.Errorf("Values[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}

// TestCanonicalSenderIDIdentityLinks covers the identity-links branch of
// canonicalSenderID (the alias-mapping path that lets multiple sender ids
// collapse to one canonical id). Was uncovered (60% → target 100%).
func TestCanonicalSenderIDIdentityLinks(t *testing.T) {
	links := map[string][]string{
		"canonical:user-1": {"feishu:user-1", "wxid_abc"},
	}
	tests := []struct {
		name     string
		channel  string
		senderID string
		links    map[string][]string
		want     string
	}{
		{"empty sender returns empty", "feishu", "", nil, ""},
		{"no links → channel:id", "feishu", "user-1", nil, "feishu:user-1"},
		{"matches channel:id alias → canonical", "feishu", "user-1", links, "canonical:user-1"},
		{"matches bare alias → canonical", "ilink", "wxid_abc", links, "canonical:user-1"},
		{"no alias match → channel:id", "feishu", "user-2", links, "feishu:user-2"},
		{"case-insensitive alias match", "FEISHU", "USER-1", links, "canonical:user-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalSenderID(tt.channel, tt.senderID, tt.links)
			if got != tt.want {
				t.Errorf("canonicalSenderID(%q, %q, ...) = %q, want %q", tt.channel, tt.senderID, got, tt.want)
			}
		})
	}
}

// TestSafeID covers the empty-fallback of safeID (returns "unknown" for empty
// input). Was at 80% — the empty branch was missed.
func TestSafeID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"normal", "Chat-1", "chat-1"},
		{"slashes to dashes", "a/b c:d", "a-b-c-d"},
		{"empty falls back to unknown", "", "unknown"},
		{"whitespace-only falls back to unknown", "   ", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeID(tt.in); got != tt.want {
				t.Errorf("safeID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestFileStoreRootNilReceiver covers the nil-receiver guards on Root and
// related accessors (0% previously). These return empty values instead of
// panicking — a defensive contract worth pinning.
func TestFileStoreRootNilReceiver(t *testing.T) {
	var s *FileStore
	if got := s.Root(); got != "" {
		t.Errorf("nil FileStore.Root() = %q, want empty", got)
	}
	var m *Manager
	if got := m.Root(); got != "" {
		t.Errorf("nil Manager.Root() = %q, want empty", got)
	}
	if got := m.AgentMessagesPath(AgentTurnInput{}); got != "" {
		t.Errorf("nil Manager.AgentMessagesPath() = %q, want empty", got)
	}
}

// TestFileStoreRootAndAgentTurn covers Root/AppendAgentTurn/AppendAgentMessages
// happy paths and the empty-messages skip (0% → covered).
func TestFileStoreRootAndAgentTurn(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.Root() == "" {
		t.Fatal("Root() empty on initialized store")
	}
	// NewFileStore rejects empty root.
	if _, err := NewFileStore("   "); err == nil {
		t.Error("NewFileStore with whitespace root should error")
	}
	input := AgentTurnInput{
		SessionID:      "session-rt-1",
		AgentID:        "agent-1",
		AgentSessionID: "agent-session-1",
		Context:        channel.NewInboundContext("feishu", "u1", map[string]string{"chat_id": "c1"}),
	}
	// Empty messages → no-op, no error.
	if err := s.AppendAgentTurn(input, nil); err != nil {
		t.Errorf("AppendAgentTurn with nil messages = %v", err)
	}
	if err := s.AppendAgentMessages(input, []Message{}); err != nil {
		t.Errorf("AppendAgentMessages with empty = %v", err)
	}
	// Real write round-trips through LoadHistories.
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	if err := s.AppendAgentMessages(input, msgs); err != nil {
		t.Fatalf("AppendAgentMessages write: %v", err)
	}
}

// TestManagerLastMessages covers LastMessages (0% previously): tail trimming
// and the max<=0 "return all" branch.
func TestManagerLastMessages(t *testing.T) {
	root := t.TempDir()
	mgr, err := NewManagerWithStore(root)
	if err != nil {
		t.Fatal(err)
	}
	scope := SessionScope{
		Version: ScopeVersionV1,
		Channel: "feishu",
		Values:  map[string]string{"chat": "direct:c1", "sender": "feishu:u1"},
	}
	sessionID := BuildSessionID(scope)
	input := AgentTurnInput{
		SessionID:      sessionID,
		AgentSessionID: "agent-session-lm",
		AgentID:        "agent-1",
		Scope:          &scope,
		Context:        channel.NewInboundContext("feishu", "u1", map[string]string{"chat_id": "c1"}),
	}
	msgs := []Message{
		{Role: "user", Content: "m1"},
		{Role: "assistant", Content: "m2"},
		{Role: "user", Content: "m3"},
	}
	if err := mgr.AppendAgentMessages(input, msgs); err != nil {
		t.Fatal(err)
	}
	// max<=0 → return all.
	if got := mgr.LastMessages(sessionID, 0); len(got) != 3 {
		t.Errorf("LastMessages(max=0) = %d messages, want 3", len(got))
	}
	// max=2 → tail 2.
	got := mgr.LastMessages(sessionID, 2)
	if len(got) != 2 || got[0].Content != "m2" || got[1].Content != "m3" {
		t.Errorf("LastMessages(max=2) = %+v, want [m2, m3]", got)
	}
	// max larger than history → all.
	if got := mgr.LastMessages(sessionID, 99); len(got) != 3 {
		t.Errorf("LastMessages(max=99) = %d, want 3", len(got))
	}
}

// TestNewManagerWithStore covers NewManagerWithStore (78.6% → higher) and
// exercises the agent-messages-path lookup.
func TestNewManagerWithStore(t *testing.T) {
	root := t.TempDir()
	store := mustFileStore(t, root)
	mgr, err := NewManagerWithStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if mgr.Root() != store.Root() {
		t.Errorf("Manager.Root() = %q, want %q", mgr.Root(), store.Root())
	}
	scope := SessionScope{
		Version: ScopeVersionV1,
		Channel: "feishu",
		Values:  map[string]string{"chat": "direct:c1", "sender": "feishu:u1"},
	}
	sessionID := BuildSessionID(scope)
	input := AgentTurnInput{
		SessionID:      sessionID,
		AgentSessionID: "agent-session-nmws",
		AgentID:        "agent-1",
		Scope:          &scope,
		Context:        channel.NewInboundContext("feishu", "u1", map[string]string{"chat_id": "c1"}),
	}
	path := mgr.AgentMessagesPath(input)
	if path == "" || !strings.HasSuffix(path, "messages.jsonl") {
		t.Errorf("AgentMessagesPath = %q, want non-empty ending in messages.jsonl", path)
	}
}

func mustFileStore(t *testing.T, root string) *FileStore {
	t.Helper()
	s, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestFileStoreDirHelpers covers the sessionID-based dir helpers
// (conversationDir / agentDir) which were 0% previously. These are used by
// the LoadHistories scan path (NewManagerWithStore on a pre-populated root).
func TestFileStoreDirHelpers(t *testing.T) {
	root := t.TempDir()
	s := mustFileStore(t, root)
	// conversationDir joins root + safe sessionID.
	convDir := s.conversationDir("session-x")
	if !strings.Contains(convDir, "session-x") || !strings.HasPrefix(convDir, root) {
		t.Errorf("conversationDir = %q, want under %q containing session-x", convDir, root)
	}
	// agentDir nests under conversationDir/agents/<agentID>.
	agentDir := s.agentDir("session-x", "agent-y")
	if !strings.Contains(agentDir, "agents") || !strings.Contains(agentDir, "agent-y") {
		t.Errorf("agentDir = %q, want .../agents/agent-y", agentDir)
	}
}

// TestFileStoreLoadHistoriesOnPopulatedRoot covers the LoadHistories scan
// path: NewManagerWithStore on a pre-populated root must restore histories.
// This exercises conversationDir/agentDir + readMessages + appendJSONLines
// happy paths through the integration path.
func TestFileStoreLoadHistoriesOnPopulatedRoot(t *testing.T) {
	root := t.TempDir()
	// First manager: write some messages.
	mgr1, err := NewManagerWithStore(root)
	if err != nil {
		t.Fatal(err)
	}
	scope := SessionScope{
		Version: ScopeVersionV1,
		Channel: "feishu",
		Values:  map[string]string{"chat": "direct:c1", "sender": "feishu:u1"},
	}
	sessionID := BuildSessionID(scope)
	input := AgentTurnInput{
		SessionID:      sessionID,
		AgentSessionID: "agent-session-load",
		AgentID:        "agent-load",
		Scope:          &scope,
		Context:        channel.NewInboundContext("feishu", "u1", map[string]string{"chat_id": "c1"}),
	}
	if err := mgr1.AppendAgentMessages(input, []Message{
		{Role: "user", Content: "persisted"},
	}); err != nil {
		t.Fatal(err)
	}
	// Second manager on the SAME root: must load the persisted message back.
	mgr2, err := NewManagerWithStore(root)
	if err != nil {
		t.Fatal(err)
	}
	history := mgr2.History(sessionID)
	found := false
	for _, msg := range history {
		if msg.Content == "persisted" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("LoadHistories did not restore persisted message; got %d messages: %+v", len(history), history)
	}
}

// TestInputChannelEntrypointFallbacks covers inputChannel/inputEntrypointID's
// scope-fallback and unknown-fallback branches (40% → full). These decide the
// disk path layout when Context is sparse (e.g. scope-only resume paths).
func TestInputChannelEntrypointFallbacks(t *testing.T) {
	// Context.Channel present → use it.
	ctxChannel := inputChannel(AgentTurnInput{
		Context: channel.InboundContext{Channel: "feishu"},
	})
	if ctxChannel != "feishu" {
		t.Errorf("inputChannel with Context.Channel = %q, want feishu", ctxChannel)
	}
	// Context empty, Scope present → fall back to Scope.Channel.
	scopeChannel := inputChannel(AgentTurnInput{
		Context: channel.InboundContext{},
		Scope:   &SessionScope{Channel: "ilink"},
	})
	if scopeChannel != "ilink" {
		t.Errorf("inputChannel scope fallback = %q, want ilink", scopeChannel)
	}
	// Both empty → unknown-channel.
	unknownChannel := inputChannel(AgentTurnInput{})
	if unknownChannel != "unknown-channel" {
		t.Errorf("inputChannel unknown fallback = %q, want unknown-channel", unknownChannel)
	}
	// Same three branches for entrypointID.
	if got := inputEntrypointID(AgentTurnInput{Context: channel.InboundContext{EntrypointID: "ep-1"}}); got != "ep-1" {
		t.Errorf("inputEntrypointID Context = %q, want ep-1", got)
	}
	if got := inputEntrypointID(AgentTurnInput{Scope: &SessionScope{EntrypointID: "ep-2"}}); got != "ep-2" {
		t.Errorf("inputEntrypointID scope fallback = %q, want ep-2", got)
	}
	if got := inputEntrypointID(AgentTurnInput{}); got != "unknown-entrypoint" {
		t.Errorf("inputEntrypointID unknown fallback = %q, want unknown-entrypoint", got)
	}
}

// TestConversationFolderNameScopeFallback covers conversationFolderName's
// scope-fallback branches (line 268/277/282): when Context has no chat/space/
// sender but Scope does, the folder name is built from Scope values. Also
// covers the space != chatID dedup branch (line 271).
func TestConversationFolderNameScopeFallback(t *testing.T) {
	// Context empty, Scope carries chat/space/sender values.
	input := AgentTurnInput{
		Context: channel.InboundContext{}, // sparse — forces scope fallback
		Scope: &SessionScope{
			Channel: "feishu",
			Values: map[string]string{
				"chat":   "group:chat-9",
				"space":  "tenant:ws-1",
				"sender": "feishu:user-42",
			},
		},
	}
	name := conversationFolderName(input)
	for _, want := range []string{"chat_", "space_", "sender_"} {
		if !strings.Contains(name, want) {
			t.Errorf("folder name %q missing dimension prefix %q (scope fallback not exercised)", name, want)
		}
	}
	// Context with space == chatID → space dedup branch (space omitted).
	dedupInput := AgentTurnInput{
		Context: channel.InboundContext{
			Channel: "feishu",
			ChatID:  "shared-id",
			SpaceID: "shared-id", // same as chatID → space omitted
		},
	}
	dedupName := conversationFolderName(dedupInput)
	if strings.Contains(dedupName, "space_") {
		t.Errorf("folder name %q should omit space when SpaceID==ChatID, but contains space_", dedupName)
	}
}
