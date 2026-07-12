package channel

import "strings"

const addressedToMetadataKey = "addressed_to"

type AddressTarget string

const (
	AddressTargetAgent AddressTarget = "agent"
	AddressTargetOwner AddressTarget = "owner"
)

type MentionTarget struct {
	ID     string `json:"id" yaml:"id"`
	IDType string `json:"id_type,omitempty" yaml:"id_type,omitempty"`
	Name   string `json:"name,omitempty" yaml:"name,omitempty"`
}

type InboundContext struct {
	Channel          string            `json:"channel" yaml:"channel"`
	EntrypointID     string            `json:"entrypoint_id,omitempty" yaml:"entrypoint_id,omitempty"`
	Account          string            `json:"account,omitempty" yaml:"account,omitempty"`
	ChannelAppID     string            `json:"channel_app_id,omitempty" yaml:"channel_app_id,omitempty"`
	BotID            string            `json:"bot_id,omitempty" yaml:"bot_id,omitempty"`
	ChatID           string            `json:"chat_id" yaml:"chat_id"`
	ChatType         string            `json:"chat_type,omitempty" yaml:"chat_type,omitempty"`
	ChatName         string            `json:"chat_name,omitempty" yaml:"chat_name,omitempty"`
	TopicID          string            `json:"topic_id,omitempty" yaml:"topic_id,omitempty"`
	SpaceID          string            `json:"space_id,omitempty" yaml:"space_id,omitempty"`
	SpaceType        string            `json:"space_type,omitempty" yaml:"space_type,omitempty"`
	SenderID         string            `json:"sender_id" yaml:"sender_id"`
	SenderName       string            `json:"sender_name,omitempty" yaml:"sender_name,omitempty"`
	MessageID        string            `json:"message_id,omitempty" yaml:"message_id,omitempty"`
	Mentioned        bool              `json:"mentioned,omitempty" yaml:"mentioned,omitempty"`
	MentionTargets   []MentionTarget   `json:"mention_targets,omitempty" yaml:"mention_targets,omitempty"`
	AddressedTo      []AddressTarget   `json:"addressed_to,omitempty" yaml:"addressed_to,omitempty"`
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
	return NewInboundContextWithEntrypoint(channelName, "", userID, metadata)
}

func NewInboundContextWithEntrypoint(channelName, entrypointID, userID string, metadata map[string]string) InboundContext {
	ctx := InboundContext{
		Channel:      normalizedOrDefault(channelName, "local"),
		EntrypointID: strings.TrimSpace(entrypointID),
		SenderID:     strings.TrimSpace(userID),
		Raw:          copyMetadata(metadata),
	}
	if ctx.SenderID == "" {
		ctx.SenderID = "local-user"
	}
	if ctx.EntrypointID == "" {
		ctx.EntrypointID = firstMetadata(metadata, "entrypoint_id", "entrypoint")
	}
	ctx.Account = firstMetadata(metadata, "account", "channel_account", "account_id")
	ctx.ChannelAppID = firstMetadata(metadata, "channel_app_id", "app_id", "application_id")
	ctx.BotID = firstMetadata(metadata, "bot_id", "robot_id")
	ctx.ChatID = firstMetadata(metadata, "chat_id", "room_id", "conversation_id")
	if ctx.ChatID == "" {
		ctx.ChatID = ctx.SenderID
	}
	ctx.ChatType = firstMetadata(metadata, "chat_type")
	if ctx.ChatType == "" {
		ctx.ChatType = "direct"
	}
	ctx.ChatName = firstMetadata(metadata, "chat_name")
	ctx.TopicID = firstMetadata(metadata, "topic_id", "thread_id")
	ctx.SpaceID = firstMetadata(metadata, "space_id", "tenant_id", "workspace_id")
	ctx.SpaceType = firstMetadata(metadata, "space_type")
	ctx.MessageID = firstMetadata(metadata, "message_id")
	ctx.ReplyToMessageID = firstMetadata(metadata, "reply_to_message_id")
	ctx.ReplyToSenderID = firstMetadata(metadata, "reply_to_sender_id")
	ctx.SenderName = firstMetadata(metadata, "sender_name")
	ctx.Mentioned = strings.EqualFold(firstMetadata(metadata, "mentioned"), "true")
	return NormalizeInboundContext(ctx)
}

func NormalizeInboundContext(ctx InboundContext) InboundContext {
	ctx.Raw = copyMetadata(ctx.Raw)
	ctx.Channel = normalizedOrDefault(ctx.Channel, "local")
	ctx.EntrypointID = strings.TrimSpace(ctx.EntrypointID)
	ctx.Account = strings.TrimSpace(ctx.Account)
	ctx.ChannelAppID = strings.TrimSpace(ctx.ChannelAppID)
	ctx.BotID = strings.TrimSpace(ctx.BotID)
	ctx.ChatID = strings.TrimSpace(ctx.ChatID)
	ctx.ChatType = normalizedOrDefault(ctx.ChatType, "direct")
	ctx.ChatName = strings.TrimSpace(ctx.ChatName)
	ctx.TopicID = strings.TrimSpace(ctx.TopicID)
	ctx.SpaceID = strings.TrimSpace(ctx.SpaceID)
	ctx.SpaceType = strings.TrimSpace(ctx.SpaceType)
	ctx.SenderID = strings.TrimSpace(ctx.SenderID)
	ctx.SenderName = strings.TrimSpace(ctx.SenderName)
	ctx.MessageID = strings.TrimSpace(ctx.MessageID)
	ctx.ReplyToMessageID = strings.TrimSpace(ctx.ReplyToMessageID)
	ctx.ReplyToSenderID = strings.TrimSpace(ctx.ReplyToSenderID)
	ctx.MentionTargets = normalizeMentionTargets(ctx.MentionTargets)
	if len(ctx.AddressedTo) == 0 {
		ctx.AddressedTo = parseAddressTargets(firstMetadata(ctx.Raw, addressedToMetadataKey))
	}
	ctx.AddressedTo = normalizeAddressTargets(ctx.AddressedTo)
	if len(ctx.AddressedTo) > 0 {
		if ctx.Raw == nil {
			ctx.Raw = map[string]string{}
		}
		values := make([]string, 0, len(ctx.AddressedTo))
		for _, target := range ctx.AddressedTo {
			values = append(values, string(target))
		}
		ctx.Raw[addressedToMetadataKey] = strings.Join(values, ",")
	} else {
		delete(ctx.Raw, addressedToMetadataKey)
	}
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

// normalizeAddressTargets filters external values to the sealed addressing contract.
// coverage: contract (100% required)
func normalizeAddressTargets(targets []AddressTarget) []AddressTarget {
	present := map[AddressTarget]bool{}
	for _, target := range targets {
		switch AddressTarget(strings.ToLower(strings.TrimSpace(string(target)))) {
		case AddressTargetAgent:
			present[AddressTargetAgent] = true
		case AddressTargetOwner:
			present[AddressTargetOwner] = true
		}
	}
	result := make([]AddressTarget, 0, len(present))
	for _, target := range []AddressTarget{AddressTargetAgent, AddressTargetOwner} {
		if present[target] {
			result = append(result, target)
		}
	}
	return result
}

// parseAddressTargets restores the sealed addressing contract from run metadata.
// coverage: contract (100% required)
func parseAddressTargets(value string) []AddressTarget {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	targets := make([]AddressTarget, 0, len(parts))
	for _, part := range parts {
		targets = append(targets, AddressTarget(part))
	}
	return targets
}

// normalizeMentionTargets makes platform mention identities safe and deterministic.
// coverage: contract (100% required)
func normalizeMentionTargets(targets []MentionTarget) []MentionTarget {
	if len(targets) == 0 {
		return nil
	}
	seen := map[string]bool{}
	result := make([]MentionTarget, 0, len(targets))
	for _, target := range targets {
		target.ID = strings.TrimSpace(target.ID)
		target.IDType = strings.ToLower(strings.TrimSpace(target.IDType))
		target.Name = strings.TrimSpace(target.Name)
		if target.ID == "" {
			continue
		}
		key := target.IDType + "\x00" + target.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, target)
	}
	return result
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
