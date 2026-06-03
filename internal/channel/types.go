package channel

import "strings"

type InboundContext struct {
	Channel          string            `json:"channel" yaml:"channel"`
	Account          string            `json:"account,omitempty" yaml:"account,omitempty"`
	ChatID           string            `json:"chat_id" yaml:"chat_id"`
	ChatType         string            `json:"chat_type,omitempty" yaml:"chat_type,omitempty"`
	TopicID          string            `json:"topic_id,omitempty" yaml:"topic_id,omitempty"`
	SpaceID          string            `json:"space_id,omitempty" yaml:"space_id,omitempty"`
	SpaceType        string            `json:"space_type,omitempty" yaml:"space_type,omitempty"`
	SenderID         string            `json:"sender_id" yaml:"sender_id"`
	MessageID        string            `json:"message_id,omitempty" yaml:"message_id,omitempty"`
	Mentioned        bool              `json:"mentioned,omitempty" yaml:"mentioned,omitempty"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty" yaml:"reply_to_message_id,omitempty"`
	ReplyToSenderID  string            `json:"reply_to_sender_id,omitempty" yaml:"reply_to_sender_id,omitempty"`
	Raw              map[string]string `json:"raw,omitempty" yaml:"raw,omitempty"`
}

type InboundEnvelope struct {
	Context            InboundContext    `json:"context" yaml:"context"`
	Content            string            `json:"content" yaml:"content"`
	RequestedAgentID   string            `json:"requested_agent_id,omitempty" yaml:"requested_agent_id,omitempty"`
	SessionIDOverride  string            `json:"session_id_override,omitempty" yaml:"session_id_override,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	OriginalEntrypoint string            `json:"original_entrypoint,omitempty" yaml:"original_entrypoint,omitempty"`
}

type OutboundMessage struct {
	Context          InboundContext    `json:"context" yaml:"context"`
	Channel          string            `json:"channel" yaml:"channel"`
	ChatID           string            `json:"chat_id" yaml:"chat_id"`
	Content          string            `json:"content" yaml:"content"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty" yaml:"reply_to_message_id,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

func NewInboundContext(channelName, userID string, metadata map[string]string) InboundContext {
	ctx := InboundContext{
		Channel:  normalizedOrDefault(channelName, "local"),
		SenderID: strings.TrimSpace(userID),
		Raw:      copyMetadata(metadata),
	}
	if ctx.SenderID == "" {
		ctx.SenderID = "local-user"
	}
	ctx.Account = firstMetadata(metadata, "account", "channel_account", "account_id")
	ctx.ChatID = firstMetadata(metadata, "chat_id", "room_id", "conversation_id")
	if ctx.ChatID == "" {
		ctx.ChatID = ctx.SenderID
	}
	ctx.ChatType = firstMetadata(metadata, "chat_type")
	if ctx.ChatType == "" {
		ctx.ChatType = "direct"
	}
	ctx.TopicID = firstMetadata(metadata, "topic_id", "thread_id")
	ctx.SpaceID = firstMetadata(metadata, "space_id", "tenant_id", "workspace_id")
	ctx.SpaceType = firstMetadata(metadata, "space_type")
	ctx.MessageID = firstMetadata(metadata, "message_id")
	ctx.ReplyToMessageID = firstMetadata(metadata, "reply_to_message_id")
	ctx.ReplyToSenderID = firstMetadata(metadata, "reply_to_sender_id")
	ctx.Mentioned = strings.EqualFold(firstMetadata(metadata, "mentioned"), "true")
	return NormalizeInboundContext(ctx)
}

func NormalizeInboundContext(ctx InboundContext) InboundContext {
	ctx.Channel = normalizedOrDefault(ctx.Channel, "local")
	ctx.Account = strings.TrimSpace(ctx.Account)
	ctx.ChatID = strings.TrimSpace(ctx.ChatID)
	ctx.ChatType = normalizedOrDefault(ctx.ChatType, "direct")
	ctx.TopicID = strings.TrimSpace(ctx.TopicID)
	ctx.SpaceID = strings.TrimSpace(ctx.SpaceID)
	ctx.SpaceType = strings.TrimSpace(ctx.SpaceType)
	ctx.SenderID = strings.TrimSpace(ctx.SenderID)
	ctx.MessageID = strings.TrimSpace(ctx.MessageID)
	ctx.ReplyToMessageID = strings.TrimSpace(ctx.ReplyToMessageID)
	ctx.ReplyToSenderID = strings.TrimSpace(ctx.ReplyToSenderID)
	if ctx.SenderID == "" {
		ctx.SenderID = "local-user"
	}
	if ctx.ChatID == "" {
		ctx.ChatID = ctx.SenderID
	}
	if len(ctx.Raw) == 0 {
		ctx.Raw = nil
	}
	return ctx
}

func NewOutboundMessage(in InboundEnvelope, content string) OutboundMessage {
	ctx := NormalizeInboundContext(in.Context)
	return OutboundMessage{
		Context:          ctx,
		Channel:          ctx.Channel,
		ChatID:           ctx.ChatID,
		Content:          content,
		ReplyToMessageID: ctx.MessageID,
		Metadata:         copyMetadata(in.Metadata),
	}
}

func copyMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}

func firstMetadata(metadata map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			return value
		}
	}
	return ""
}

func normalizedOrDefault(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	return value
}
