package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/channelrunner/dedupe"
	"github.com/xiramesh/xira/internal/entrypoints"
	"github.com/xiramesh/xira/internal/routing"
	"github.com/xiramesh/xira/internal/runtime"
	fsession "github.com/xiramesh/xira/internal/session"
)

// stubOwnerResolver implements runtime.OwnerResolver for tests.
type stubOwnerResolver struct {
	ownerSenderID string
}

func (s *stubOwnerResolver) IsOwner(_ context.Context, senderID, entrypointID string) bool {
	return senderID == s.ownerSenderID
}

func TestAuthorizeSender(t *testing.T) {
	def := entrypoints.Definition{AllowedSenderIDs: []string{"ou_allowed"}}
	owner := &stubOwnerResolver{ownerSenderID: "ou_owner"}

	cases := []struct {
		name     string
		senderID string
		content  string
		def      entrypoints.Definition
		owner    runtime.OwnerResolver
		want     bool
	}{
		{"allowlist hit", "ou_allowed", "hello", def, nil, true},
		{"allowlist miss no owner", "ou_blocked", "hello", def, nil, false},
		{"owner bypass", "ou_owner", "hello", def, owner, true},
		{"not owner not allowlisted", "ou_stranger", "hello", def, owner, false},
		{"bind pre-auth", "ou_stranger", "/bind ABCD-EFGH", def, nil, true},
		{"empty allowlist allows all", "anyone", "hello", entrypoints.Definition{}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AuthorizeSender(tc.senderID, tc.content, tc.def, tc.owner)
			if got != tc.want {
				t.Errorf("AuthorizeSender(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestGateDispatch(t *testing.T) {
	ing := New(nil, nil)
	def := entrypoints.Definition{AllowedSenderIDs: []string{"ou_ok"}}

	// 私聊 + 授权 → dispatch
	got := ing.Gate(MessageInput{ChatType: "direct", SenderID: "ou_ok", Content: "hi"}, def)
	if got != DecisionDispatch {
		t.Errorf("private + authorized = %v, want dispatch", got)
	}

	// 群聊 @ bot + 授权 → dispatch
	got = ing.Gate(MessageInput{ChatType: "group", Mentioned: true, SenderID: "ou_ok", Content: "hi"}, def)
	if got != DecisionDispatch {
		t.Errorf("group @bot + authorized = %v, want dispatch", got)
	}
}

func TestGateObserve(t *testing.T) {
	ing := New(nil, nil)
	def := entrypoints.Definition{AllowedSenderIDs: []string{"ou_ok"}}

	// 群聊没 @ bot + 授权 → observe
	got := ing.Gate(MessageInput{ChatType: "group", Mentioned: false, SenderID: "ou_ok", Content: "hi"}, def)
	if got != DecisionObserve {
		t.Errorf("group unmentioned + authorized = %v, want observe", got)
	}
}

func TestGateRejectUnauthorized(t *testing.T) {
	ing := New(nil, nil)
	def := entrypoints.Definition{AllowedSenderIDs: []string{"ou_ok"}}

	// 未授权 → reject（不管 mention/chatType）
	cases := []MessageInput{
		{ChatType: "group", Mentioned: true, SenderID: "ou_stranger", Content: "hi"},
		{ChatType: "group", Mentioned: false, SenderID: "ou_stranger", Content: "hi"},
		{ChatType: "direct", SenderID: "ou_stranger", Content: "hi"},
	}
	for i, input := range cases {
		got := ing.Gate(input, def)
		if got != DecisionReject {
			t.Errorf("case %d: unauthorized = %v, want reject", i, got)
		}
	}
}

func TestGateRejectDoesNotObserveUnauthorized(t *testing.T) {
	// 安全核心测试（reviewer blocker 2）：未授权 sender 不能通过 observe 注入内容。
	// 即使群聊没 @ bot，未授权 → reject（不 observe）。
	mgr, _ := fsession.NewManagerWithStore(t.TempDir())
	ing := New(mgr, nil)
	def := entrypoints.Definition{
		ID:             "ep-test",
		DefaultAgentID: "agent-1",
		AllowedSenderIDs: []string{"ou_authorized"},
	}

	// 未授权 sender 发群消息没 @ bot
	input := MessageInput{
		ChatType: "group", Mentioned: false,
		SenderID: "ou_evil", Content: "inject me",
		ChatID: "chat-1", Channel: "test", MessageID: "msg-1",
	}
	decision := ing.Gate(input, def)
	if decision != DecisionReject {
		t.Fatal("unauthorized should be rejected, not observed")
	}
	// Observe 不该被调用（decision 是 reject）
	// 但如果有人误调 Observe，验证未授权内容不进 session
	// （Gate 返回 reject，调用方不该再调 Observe）

	// 授权 sender observe → 内容进 session
	authInput := MessageInput{
		ChatType: "group", Mentioned: false,
		SenderID: "ou_authorized", Content: "normal message",
		ChatID: "chat-1", Channel: "test", MessageID: "msg-2",
	}
	ing.Observe(authInput, def, "", nil)

	// session 里只有授权 sender 的消息
	entries := readHistory(t, mgr, "test", "chat-1", "group", "ou_authorized")
	found := false
	for _, m := range entries {
		if m.Content == "inject me" {
			t.Error("BLOCKER: unauthorized content leaked into session")
		}
		if m.Content == "normal message" {
			found = true
		}
	}
	if !found {
		t.Error("authorized message should be in session")
	}
}

func TestObserveStoresWithSender(t *testing.T) {
	mgr, _ := fsession.NewManagerWithStore(t.TempDir())
	ing := New(mgr, nil)
	def := entrypoints.Definition{ID: "ep-test", DefaultAgentID: "agent-1"}

	ing.Observe(MessageInput{
		ChatType: "group", SenderID: "ou_alice", SenderName: "Alice",
		Content: "hello from alice", ChatID: "chat-1",
		Channel: "test", MessageID: "msg-1",
	}, def, "", nil)

	entries := readHistory(t, mgr, "test", "chat-1", "group", "ou_alice")
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].SenderID != "ou_alice" {
		t.Errorf("SenderID = %q, want ou_alice", entries[0].SenderID)
	}
	if entries[0].SenderName != "Alice" {
		t.Errorf("SenderName = %q, want Alice", entries[0].SenderName)
	}
}

func TestObserveDedupe(t *testing.T) {
	mgr, _ := fsession.NewManagerWithStore(t.TempDir())
	ing := New(mgr, nil)
	def := entrypoints.Definition{ID: "ep-test", DefaultAgentID: "agent-1"}

	deduper := dedupe.New("", time.Hour)
	input := MessageInput{
		ChatType: "group", SenderID: "ou_a", Content: "hello",
		ChatID: "chat-1", Channel: "test", MessageID: "msg-1",
	}
	// observe 两次同一消息ID
	ing.Observe(input, def, "ep-test:msg-1", deduper)
	ing.Observe(input, def, "ep-test:msg-1", deduper)

	entries := readHistory(t, mgr, "test", "chat-1", "group", "ou_a")
	if len(entries) != 1 {
		t.Errorf("dedupe should prevent double observe, got %d entries", len(entries))
	}
}

func TestObserveNilSessionManager(t *testing.T) {
	// sessionManager=nil → Observe 是 no-op（不 panic）
	ing := New(nil, nil)
	ing.Observe(MessageInput{Content: "x"}, entrypoints.Definition{}, "", nil)
}

// readHistory helper：读 session 历史。
func readHistory(t *testing.T, mgr *fsession.Manager, channelName, chatID, chatType, senderID string) []fsession.Message {
	t.Helper()
	scope := fsession.BuildScope(
		channel.InboundContext{
			Channel: channelName, ChatID: chatID, ChatType: chatType, SenderID: senderID,
		},
		routing.SessionPolicy{}, // 用默认 [chat] 维度
	)
	sessionID := fsession.BuildSessionID(scope)
	return mgr.History(sessionID)
}
