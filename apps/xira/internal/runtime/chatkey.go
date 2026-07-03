package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiramesh/xira/internal/channel"
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

// ChatKey uniquely identifies a conversation from one sender's perspective.
type ChatKey struct {
	Channel  string
	ChatID   string
	SenderID string
}

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

// String returns a stable, human-readable representation for logging/debugging.
func (k ChatKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.Channel, k.ChatID, k.SenderID)
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

// --- chatKey context propagation ---

// chatKeyContextKey carries the ChatKey for the current turn through ctx.
// Injected at RunAgent entry (service.go) so spawned children (spawn_turn.go)
// can read it and register themselves with the per-chat-key cancel registry
// (RFC #67). childToolConstraintCtx re-attaches it (like EventBus) since it
// starts from context.Background().
type chatKeyContextKey struct{}

// WithChatKey returns a ctx carrying the chatKey.
func WithChatKey(ctx context.Context, key ChatKey) context.Context {
	return context.WithValue(ctx, chatKeyContextKey{}, key)
}

// ChatKeyFromContext returns the chatKey carried in ctx, if any.
func ChatKeyFromContext(ctx context.Context) (ChatKey, bool) {
	k, ok := ctx.Value(chatKeyContextKey{}).(ChatKey)
	return k, ok
}
