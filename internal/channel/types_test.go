package channel

import "testing"

func TestNewInboundContextNormalizesChannelFacts(t *testing.T) {
	ctx := NewInboundContext("Feishu", "user-1", map[string]string{
		"account":   "tenant-a",
		"chat_id":   "chat-1",
		"chat_type": "group",
		"topic_id":  "topic-1",
	})

	if ctx.Channel != "feishu" {
		t.Fatalf("channel = %q, want feishu", ctx.Channel)
	}
	if ctx.Account != "tenant-a" || ctx.ChatID != "chat-1" || ctx.ChatType != "group" {
		t.Fatalf("context not populated from metadata: %+v", ctx)
	}
	if ctx.SenderID != "user-1" || ctx.TopicID != "topic-1" {
		t.Fatalf("sender/topic mismatch: %+v", ctx)
	}
}
