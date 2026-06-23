package runtime

import (
	"fmt"

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
