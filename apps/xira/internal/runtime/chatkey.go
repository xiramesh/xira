package runtime

import (
	"context"
	"strings"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/chatkey"
)

// chatkey.go defines the per-chat-key routing identity (RFC
// xira-per-chat-key-architecture-rfc-v0.zh.md §2.1).
//
// ChatKey = (Channel, ChatID, SenderID). It identifies one conversation
// from one sender's perspective. Same chat key = same turn scope. Events
// are routed per-chat-key, eliminating the need for a global EventBus +
// scopeMatcher.
//
// Design (verified, PR #48):
//   - SenderID makes group-chat senders independent turns (A and B in the
//     same group are different keys → different turns → no cross-steering).
//   - @me filtering is orthogonal to ChatKey (it's an inbound gate, not a
//     turn-ownership dimension). See RFC §2.2 routing diagram.
//
// ChatKey struct + ctx 传播下沉到 internal/chatkey 包（#126：tools 包要从
// ctx 取 SenderID 做 per-sender 数据隔离，但不能 import runtime）。本包通过
// type alias + re-export 保持向后兼容（现有 runtime.ChatKey / WithChatKey /
// ChatKeyFromContext 调用点不破），ctx key 是同一个（internal/chatkey 拥有）。

// ChatKey 是 chatkey.ChatKey 的别名——runtime 包内可直接用 ChatKey，
// 与 internal/chatkey.ChatKey 完全等价（同一类型）。
type ChatKey = chatkey.ChatKey

// ChatKeyFromInbound extracts a ChatKey from an InboundContext. If
// Channel/ChatID/SenderID are all empty, returns the zero ChatKey (routing
// layer treats this as "disabled turn", like the old Forwarder's disabled
// reason).
func ChatKeyFromInbound(ic channel.InboundContext) ChatKey {
	return ChatKey{
		Channel:  ic.Channel,
		ChatID:   ic.ChatID,
		SenderID: ic.SenderID,
	}
}

// ParseChatKey is the inverse of (ChatKey).String(): it splits "channel/chat/sender"
// back into a ChatKey. It uses SplitN with limit 3 so a SenderID containing "/"
// (rare but possible) is preserved in the third field. Returns ok=false if the
// string has fewer than 3 segments (not a valid chatKey string form).
//
// Used by the resume paths (#114) to recover the ChatKey from a persisted
// HumanRequest.ChatKey string — the authoritative source, since that is the
// exact value the store compares against (ListByChatKey). Recovering from
// SessionScope instead would be lossy: SessionScope lowercases its values and
// applies canonicalSenderID rewriting, both of which can diverge from the
// original ChatKey string and silently break the equality check.
func ParseChatKey(s string) (ChatKey, bool) {
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 3 {
		return ChatKey{}, false
	}
	return ChatKey{Channel: parts[0], ChatID: parts[1], SenderID: parts[2]}, true
}

// --- chatKey context propagation（re-export internal/chatkey，保持调用点不破）---

// WithChatKey 返回携带 chatKey 的 ctx。委托给 internal/chatkey（ctx key 的唯一真相源）。
func WithChatKey(ctx context.Context, key ChatKey) context.Context {
	return chatkey.WithChatKey(ctx, key)
}

// ChatKeyFromContext 返回 ctx 里的 chatKey。委托给 internal/chatkey。
func ChatKeyFromContext(ctx context.Context) (ChatKey, bool) {
	return chatkey.FromContext(ctx)
}
