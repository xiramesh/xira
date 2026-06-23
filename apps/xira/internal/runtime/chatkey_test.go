package runtime

import (
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
)

// ChatKey tests define the per-chat-key routing identity contract (RFC
// xira-per-chat-key-architecture-rfc-v0.zh.md §2.1).
//
// ChatKey = (Channel, ChatID, SenderID). It identifies one conversation
// from one sender's perspective. Same chat key = same turn scope.

func TestChatKeyEquality(t *testing.T) {
	cases := []struct {
		name string
		a, b ChatKey
		want bool
	}{
		{
			name: "identical",
			a:    ChatKey{Channel: "feishu", ChatID: "chat_1", SenderID: "user_A"},
			b:    ChatKey{Channel: "feishu", ChatID: "chat_1", SenderID: "user_A"},
			want: true,
		},
		{
			name: "different sender same chat",
			a:    ChatKey{Channel: "feishu", ChatID: "group_1", SenderID: "user_A"},
			b:    ChatKey{Channel: "feishu", ChatID: "group_1", SenderID: "user_B"},
			want: false, // A and B are independent turns in a group
		},
		{
			name: "different chat same sender",
			a:    ChatKey{Channel: "feishu", ChatID: "chat_1", SenderID: "user_A"},
			b:    ChatKey{Channel: "feishu", ChatID: "chat_2", SenderID: "user_A"},
			want: false,
		},
		{
			name: "different channel same chat+sender",
			a:    ChatKey{Channel: "feishu", ChatID: "chat_1", SenderID: "user_A"},
			b:    ChatKey{Channel: "ilink", ChatID: "chat_1", SenderID: "user_A"},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a == c.b; got != c.want {
				t.Errorf("%+v == %+v = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestChatKeyFromInboundContext(t *testing.T) {
	ic := channel.InboundContext{
		Channel:  "feishu",
		ChatID:   "oc_test_chat",
		SenderID: "user_42",
	}
	key := ChatKeyFromInbound(ic)
	if key.Channel != "feishu" || key.ChatID != "oc_test_chat" || key.SenderID != "user_42" {
		t.Errorf("ChatKeyFromInbound = %+v, want {feishu oc_test_chat user_42}", key)
	}
}

func TestChatKeyFromInboundContextEmptyFields(t *testing.T) {
	// Empty fields should produce a valid (but empty-dimension) key — the
	// routing layer handles "no chat_id" as a disabled turn (like the old
	// Forwarder disabled reason).
	ic := channel.InboundContext{}
	key := ChatKeyFromInbound(ic)
	if key != (ChatKey{}) {
		t.Errorf("empty InboundContext should produce zero ChatKey, got %+v", key)
	}
}

func TestChatKeyString(t *testing.T) {
	key := ChatKey{Channel: "feishu", ChatID: "oc_test", SenderID: "u1"}
	// String() is for logging/debugging — verify it's stable and contains
	// all three dimensions.
	s := key.String()
	if s == "" {
		t.Error("String() should not be empty")
	}
	// Verify all three dimensions appear in the string representation.
	for _, dim := range []string{"feishu", "oc_test", "u1"} {
		if !strings.Contains(s, dim) {
			t.Errorf("String() %q should contain %q", s, dim)
		}
	}
}
