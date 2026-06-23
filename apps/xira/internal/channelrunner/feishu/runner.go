package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/channelrunner/dedupe"
	"github.com/xiramesh/xira/internal/channelrunner/progress"
	"github.com/xiramesh/xira/internal/entrypoints"
	frt "github.com/xiramesh/xira/internal/runtime"
)

var mentionPlaceholderRegex = regexp.MustCompile(`@_user_\d+`)

const messageDedupeTTL = time.Hour

type Runner struct {
	definition entrypoints.Definition
	runtime    *frt.Service
	appID      string
	appSecret  string
	verify     string
	encryptKey string
	client     *lark.Client

	mu       sync.Mutex
	cancel   context.CancelFunc
	wsClient *larkws.Client

	messages *dedupe.MessageDeduper
}

func NewRunner(definition entrypoints.Definition, rt *frt.Service, stateRoot string) (*Runner, error) {
	appID := resolveValue(definition.AppID, definition.AppIDEnv)
	appSecret := resolveValue(definition.AppSecret, definition.AppSecretEnv)
	if appID == "" {
		return nil, fmt.Errorf("feishu entrypoint %q missing app_id or app_id_env", definition.ID)
	}
	if appSecret == "" {
		return nil, fmt.Errorf("feishu entrypoint %q missing app_secret or app_secret_env", definition.ID)
	}
	opts := []lark.ClientOptionFunc{}
	if definition.IsLark {
		opts = append(opts, lark.WithOpenBaseUrl(lark.LarkBaseUrl))
	}
	stateDir, err := channelStateDir(definition, stateRoot, "feishu")
	if err != nil {
		return nil, err
	}
	slog.Info("feishu runner configured",
		"entrypoint_id", definition.ID,
		"app_id", appID,
		"app_id_env", definition.AppIDEnv,
		"state_dir", stateDir,
		"is_lark", definition.IsLark,
		"verification_token_configured", resolveValue(definition.VerifyToken, definition.VerifyTokenEnv) != "",
		"encrypt_key_configured", resolveValue(definition.EncryptKey, definition.EncryptKeyEnv) != "",
		"respond_to_unmentioned_group_messages", definition.RespondToUnmentionedGroupMessages,
	)
	return &Runner{
		definition: definition,
		runtime:    rt,
		appID:      appID,
		appSecret:  appSecret,
		verify:     resolveValue(definition.VerifyToken, definition.VerifyTokenEnv),
		encryptKey: resolveValue(definition.EncryptKey, definition.EncryptKeyEnv),
		client:     lark.NewClient(appID, appSecret, opts...),
		messages:   dedupe.New(filepath.Join(stateDir, "dedupe.json"), messageDedupeTTL),
	}, nil
}

func (r *Runner) ID() string {
	return r.definition.ID
}

func (r *Runner) Channel() string {
	return "feishu"
}

func (r *Runner) Start(ctx context.Context) error {
	dispatcher := larkdispatcher.NewEventDispatcher(r.verify, r.encryptKey).
		OnP2MessageReceiveV1(r.handleMessageReceive)

	runCtx, cancel := context.WithCancel(ctx)
	domain := lark.FeishuBaseUrl
	if r.definition.IsLark {
		domain = lark.LarkBaseUrl
	}
	slog.Info("feishu runner starting",
		"entrypoint_id", r.definition.ID,
		"app_id", r.appID,
		"domain", domain,
		"is_lark", r.definition.IsLark,
	)
	client := larkws.NewClient(
		r.appID,
		r.appSecret,
		larkws.WithEventHandler(dispatcher),
		larkws.WithDomain(domain),
	)

	r.mu.Lock()
	r.cancel = cancel
	r.wsClient = client
	r.mu.Unlock()

	go func() {
		if err := client.Start(runCtx); err != nil && runCtx.Err() == nil {
			slog.Error("feishu runner stopped with error", "entrypoint_id", r.definition.ID, "error", err)
			return
		}
		slog.Info("feishu runner stopped", "entrypoint_id", r.definition.ID, "reason", runCtx.Err())
	}()
	return nil
}

func (r *Runner) Stop(context.Context) error {
	slog.Info("feishu runner stopping", "entrypoint_id", r.definition.ID)
	r.mu.Lock()
	cancel := r.cancel
	r.cancel = nil
	r.wsClient = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (r *Runner) handleMessageReceive(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}
	message := event.Event.Message
	chatID := stringValue(message.ChatId)
	if chatID == "" {
		slog.Warn("feishu message ignored", "entrypoint_id", r.definition.ID, "reason", "missing_chat_id")
		return nil
	}
	chatType := normalizeChatType(stringValue(message.ChatType))
	senderID := extractSenderID(event.Event.Sender)
	if senderID == "" {
		senderID = "unknown"
	}
	mentioned := len(message.Mentions) > 0
	messageType := stringValue(message.MessageType)
	content := extractContent(messageType, stringValue(message.Content))
	content = stripMentionPlaceholders(content, message.Mentions)
	if strings.TrimSpace(content) == "" {
		content = "[empty message]"
	}
	messageID := stringValue(message.MessageId)
	slog.Info("feishu message received",
		"entrypoint_id", r.definition.ID,
		"app_id", r.appID,
		"chat_id", chatID,
		"chat_type", chatType,
		"message_id", messageID,
		"message_type", messageType,
		"sender_id", senderID,
		"mentioned", mentioned,
		"mentions", len(message.Mentions),
		"content_chars", utf8.RuneCountInString(content),
		"content_preview", previewText(content, 120),
	)
	if !shouldHandleMessage(chatType, mentioned, r.definition) {
		slog.Info("feishu message ignored",
			"entrypoint_id", r.definition.ID,
			"chat_id", chatID,
			"chat_type", chatType,
			"message_id", messageID,
			"sender_id", senderID,
			"reason", "unmentioned_group_message",
			"respond_to_unmentioned_group_messages", r.definition.RespondToUnmentionedGroupMessages,
		)
		return nil
	}
	dedupeKey := r.messageDedupeKey(messageID)
	if !r.messages.Begin(dedupeKey, time.Now()) {
		slog.Info("feishu duplicate message ignored",
			"entrypoint_id", r.definition.ID,
			"chat_id", chatID,
			"chat_type", chatType,
			"message_id", messageID,
			"sender_id", senderID,
			"dedupe_ttl", messageDedupeTTL,
		)
		return nil
	}
	messageProcessed := false
	defer func() {
		if messageProcessed {
			r.messages.Complete(dedupeKey, time.Now())
			return
		}
		r.messages.Forget(dedupeKey)
	}()

	metadata := r.buildMetadata(message, event.Event.Sender, chatType, messageType)
	slog.Info("feishu dispatching message to runtime",
		"entrypoint_id", r.definition.ID,
		"chat_id", chatID,
		"chat_type", chatType,
		"message_id", messageID,
		"sender_id", senderID,
	)
	inbound := channel.NewInboundContextWithEntrypoint("feishu", r.definition.ID, senderID, metadata)
	// Conversation progress forwarder: projects allowlisted runtime facts
	// (delegate failed/timeout, waiting_human) into this IM chat during the
	// Per-chat-key progress delivery (RFC #48): ChatContext replaces Forwarder.
	policy := progress.DefaultPolicy()
	chatCtx := progress.NewChatContext(ctx, progress.ChatContextConfig{
		Sender: progress.SenderFunc(func(ctx context.Context, m progress.Message) error {
			return r.send(ctx, chatID, m.Text)
		}),
		MaxChars: policy.MaxChars,
		Policy:   policy,
	})
	chatCtx.Start()
	runCtx := frt.WithEventSink(ctx, chatCtx)
	resp, err := r.runtime.RunAgent(runCtx, frt.TurnRequest{
		EntrypointID: r.definition.ID,
		Message:      content,
		// Trigger identity travels as a first-class InboundContext: channel +
		// chat/sender/space are extracted from the metadata map so the session
		// lands under sessions/feishu/<entrypoint>/chat_<id>__sender_<id>/.
		Context: inbound,
	})
	chatCtx.Stop()
	if err != nil {
		slog.Error("feishu runtime run failed",
			"entrypoint_id", r.definition.ID,
			"chat_id", chatID,
			"message_id", messageID,
			"sender_id", senderID,
			"error", err,
		)
		return fmt.Errorf("feishu entrypoint %s run agent: %w", r.definition.ID, err)
	}
	slog.Info("feishu runtime run completed",
		"entrypoint_id", r.definition.ID,
		"run_id", resp.RunID,
		"agent_id", resp.AgentID,
		"status", resp.Status,
		"session_id", resp.SessionID,
		"chat_id", chatID,
		"message_id", messageID,
		"tool_calls", len(resp.ToolCalls),
		"events", len(resp.Events),
		"final_response_chars", utf8.RuneCountInString(resp.FinalResponse),
	)
	if strings.TrimSpace(resp.FinalResponse) == "" {
		slog.Warn("feishu response skipped",
			"entrypoint_id", r.definition.ID,
			"run_id", resp.RunID,
			"chat_id", chatID,
			"message_id", messageID,
			"reason", "empty_final_response",
		)
		messageProcessed = true
		return nil
	}
	if err := r.send(ctx, chatID, resp.FinalResponse); err != nil {
		slog.Error("feishu response send failed",
			"entrypoint_id", r.definition.ID,
			"run_id", resp.RunID,
			"chat_id", chatID,
			"message_id", messageID,
			"error", err,
		)
		return fmt.Errorf("feishu entrypoint %s send response: %w", r.definition.ID, err)
	}
	messageProcessed = true
	slog.Info("feishu response sent",
		"entrypoint_id", r.definition.ID,
		"run_id", resp.RunID,
		"chat_id", chatID,
		"message_id", messageID,
		"final_response_chars", utf8.RuneCountInString(resp.FinalResponse),
	)
	return nil
}

func (r *Runner) messageDedupeKey(messageID string) string {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ""
	}
	return strings.TrimSpace(r.definition.ID) + ":" + messageID
}

func shouldHandleMessage(chatType string, mentioned bool, definition entrypoints.Definition) bool {
	if chatType != "group" {
		return true
	}
	if mentioned {
		return true
	}
	return definition.RespondToUnmentionedGroupMessages
}

func (r *Runner) buildMetadata(message *larkim.EventMessage, sender *larkim.EventSender, chatType, messageType string) map[string]string {
	metadata := map[string]string{
		"entrypoint_id":  r.definition.ID,
		"app_id":         r.appID,
		"channel_app_id": r.appID,
		"chat_type":      chatType,
	}
	if r.definition.Account != "" {
		metadata["account"] = r.definition.Account
	}
	if r.definition.BotID != "" {
		metadata["bot_id"] = r.definition.BotID
	}
	if message != nil {
		setMetadata(metadata, "chat_id", stringValue(message.ChatId))
		setMetadata(metadata, "message_id", stringValue(message.MessageId))
		setMetadata(metadata, "message_type", messageType)
		setMetadata(metadata, "thread_id", stringValue(message.ThreadId))
		setMetadata(metadata, "reply_to_message_id", firstNonEmpty(stringValue(message.ParentId), stringValue(message.RootId)))
	}
	if sender != nil {
		setMetadata(metadata, "tenant_key", stringValue(sender.TenantKey))
		setMetadata(metadata, "space_id", stringValue(sender.TenantKey))
		if stringValue(sender.TenantKey) != "" {
			metadata["space_type"] = "tenant"
		}
	}
	return metadata
}

func (r *Runner) send(ctx context.Context, chatID, content string) error {
	cardContent, err := buildMarkdownCard(content)
	if err == nil {
		if err := r.sendCard(ctx, chatID, cardContent); err == nil {
			slog.Info("feishu card response sent", "entrypoint_id", r.definition.ID, "chat_id", chatID, "content_chars", utf8.RuneCountInString(content))
			return nil
		} else {
			slog.Warn("feishu card response failed; falling back to text", "entrypoint_id", r.definition.ID, "chat_id", chatID, "error", err)
		}
	} else {
		slog.Warn("feishu card response build failed; falling back to text", "entrypoint_id", r.definition.ID, "chat_id", chatID, "error", err)
	}
	if err := r.sendText(ctx, chatID, content); err != nil {
		return err
	}
	slog.Info("feishu text response sent", "entrypoint_id", r.definition.ID, "chat_id", chatID, "content_chars", utf8.RuneCountInString(content))
	return nil
}

func (r *Runner) sendCard(ctx context.Context, chatID, cardContent string) error {
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(larkim.MsgTypeInteractive).
			Content(cardContent).
			Build()).
		Build()
	resp, err := r.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("feishu api error code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (r *Runner) sendText(ctx context.Context, chatID, text string) error {
	content, _ := json.Marshal(map[string]string{"text": text})
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(larkim.ReceiveIdTypeChatId).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).
			MsgType(larkim.MsgTypeText).
			Content(string(content)).
			Build()).
		Build()
	resp, err := r.client.Im.V1.Message.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("feishu api error code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func buildMarkdownCard(content string) (string, error) {
	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": []map[string]any{
				{"tag": "markdown", "content": content},
			},
		},
	}
	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

type messageDeduper = dedupe.MessageDeduper

func newMessageDeduper(ttl time.Duration) *messageDeduper {
	return dedupe.New("", ttl)
}

func channelStateDir(definition entrypoints.Definition, stateRoot, channel string) (string, error) {
	if strings.TrimSpace(definition.StateDir) != "" {
		return strings.TrimSpace(definition.StateDir), nil
	}
	if strings.TrimSpace(stateRoot) == "" {
		return "", fmt.Errorf("%s entrypoint %q requires runtime state_dir or entrypoint state_dir", channel, definition.ID)
	}
	return filepath.Join(stateRoot, "channels", channel, safePathSegment(definition.ID)), nil
}

func safePathSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func extractContent(messageType, rawContent string) string {
	if rawContent == "" {
		return ""
	}
	switch messageType {
	case larkim.MsgTypeText:
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(rawContent), &payload); err == nil {
			return payload.Text
		}
		return rawContent
	case larkim.MsgTypeImage:
		return "[image]"
	case larkim.MsgTypeFile:
		return firstJSONStringField(rawContent, "file_name", "file_key")
	case larkim.MsgTypeAudio:
		return "[audio]"
	case larkim.MsgTypeMedia:
		return "[video]"
	default:
		return rawContent
	}
}

func firstJSONStringField(content string, fields ...string) string {
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &values); err != nil {
		return ""
	}
	for _, field := range fields {
		raw, ok := values[field]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err == nil && value != "" {
			return value
		}
	}
	return ""
}

func stripMentionPlaceholders(content string, mentions []*larkim.MentionEvent) string {
	for _, mention := range mentions {
		if mention != nil && mention.Key != nil && *mention.Key != "" {
			content = strings.ReplaceAll(content, *mention.Key, "")
		}
	}
	content = mentionPlaceholderRegex.ReplaceAllString(content, "")
	return strings.TrimSpace(content)
}

func extractSenderID(sender *larkim.EventSender) string {
	if sender == nil || sender.SenderId == nil {
		return ""
	}
	if sender.SenderId.UserId != nil && *sender.SenderId.UserId != "" {
		return *sender.SenderId.UserId
	}
	if sender.SenderId.OpenId != nil && *sender.SenderId.OpenId != "" {
		return *sender.SenderId.OpenId
	}
	if sender.SenderId.UnionId != nil && *sender.SenderId.UnionId != "" {
		return *sender.SenderId.UnionId
	}
	return ""
}

func normalizeChatType(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "p2p") || strings.EqualFold(strings.TrimSpace(value), "direct") {
		return "direct"
	}
	return "group"
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func setMetadata(metadata map[string]string, key, value string) {
	if strings.TrimSpace(value) != "" {
		metadata[key] = strings.TrimSpace(value)
	}
}

func previewText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if limit <= 0 || text == "" {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func resolveValue(value, envName string) string {
	value = strings.TrimSpace(value)
	if value != "" {
		return value
	}
	envName = strings.TrimSpace(envName)
	if envName == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(envName))
}
