package chatkey

import (
	"context"
	"testing"
)

// ChatKey 是 per-turn 的路由身份（Channel/ChatID/SenderID）。
// 本包定义 ChatKey struct + ctx 传播，供 runtime 和 tools 共用（避免 tools→runtime 循环依赖）。

func TestChatKeyContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	// 未设置 → ok=false
	if _, ok := FromContext(ctx); ok {
		t.Error("FromContext on plain ctx should return ok=false")
	}
	// 设置后取回
	key := ChatKey{Channel: "feishu", ChatID: "chat-1", SenderID: "ou_owner"}
	ctx = WithChatKey(ctx, key)
	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext after WithChatKey should return ok=true")
	}
	if got != key {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, key)
	}
}

func TestSenderIDFromContext(t *testing.T) {
	// tools 包主要用这个：从 ctx 取 senderID 做 per-sender 隔离。
	ctx := context.Background()
	if _, ok := SenderIDFromContext(ctx); ok {
		t.Error("SenderIDFromContext on plain ctx should return ok=false")
	}
	ctx = WithChatKey(ctx, ChatKey{Channel: "feishu", SenderID: "ou_大明"})
	got, ok := SenderIDFromContext(ctx)
	if !ok || got != "ou_大明" {
		t.Errorf("SenderIDFromContext = (%q, %v), want (ou_大明, true)", got, ok)
	}
}

func TestChatKeyString(t *testing.T) {
	k := ChatKey{Channel: "feishu", ChatID: "chat-1", SenderID: "u1"}
	if got := k.String(); got != "feishu/chat-1/u1" {
		t.Errorf("String() = %q, want feishu/chat-1/u1", got)
	}
}

func TestDataIsolationEnabledFromContext(t *testing.T) {
	// plain ctx → false
	if DataIsolationEnabledFromContext(context.Background()) {
		t.Error("plain ctx should return false")
	}
	// DataIsolation=false → false（entrypoint 没配）
	ctx := WithChatKey(context.Background(), ChatKey{SenderID: "u1"})
	if DataIsolationEnabledFromContext(ctx) {
		t.Error("DataIsolation=false should return false")
	}
	// DataIsolation=true → true（entrypoint 配了）
	ctx = WithChatKey(context.Background(), ChatKey{SenderID: "u1", DataIsolation: true})
	if !DataIsolationEnabledFromContext(ctx) {
		t.Error("DataIsolation=true should return true")
	}
}
