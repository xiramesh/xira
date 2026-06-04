package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/ai-daming/xira/internal/entrypoints"
	frt "github.com/ai-daming/xira/internal/runtime"
)

var mentionPlaceholderRegex = regexp.MustCompile(`@_user_\d+`)

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
}

func NewRunner(definition entrypoints.Definition, rt *frt.Service) (*Runner, error) {
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
	return &Runner{
		definition: definition,
		runtime:    rt,
		appID:      appID,
		appSecret:  appSecret,
		verify:     resolveValue(definition.VerifyToken, definition.VerifyTokenEnv),
		encryptKey: resolveValue(definition.EncryptKey, definition.EncryptKeyEnv),
		client:     lark.NewClient(appID, appSecret, opts...),
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
			log.Printf("xira feishu runner %s stopped: %v", r.definition.ID, err)
		}
	}()
	return nil
}

func (r *Runner) Stop(context.Context) error {
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
		return nil
	}
	chatType := normalizeChatType(stringValue(message.ChatType))
	senderID := extractSenderID(event.Event.Sender)
	if senderID == "" {
		senderID = "unknown"
	}
	mentioned := len(message.Mentions) > 0
	if !shouldHandleMessage(chatType, mentioned, r.definition) {
		return nil
	}

	messageType := stringValue(message.MessageType)
	content := extractContent(messageType, stringValue(message.Content))
	content = stripMentionPlaceholders(content, message.Mentions)
	if strings.TrimSpace(content) == "" {
		content = "[empty message]"
	}

	metadata := r.buildMetadata(message, event.Event.Sender, chatType, messageType)
	resp, err := r.runtime.RunAgent(ctx, frt.TurnRequest{
		EntrypointID: r.definition.ID,
		Channel:      "feishu",
		UserID:       senderID,
		Message:      content,
		Metadata:     metadata,
	})
	if err != nil {
		return fmt.Errorf("feishu entrypoint %s run agent: %w", r.definition.ID, err)
	}
	if strings.TrimSpace(resp.FinalResponse) == "" {
		return nil
	}
	if err := r.send(ctx, chatID, resp.FinalResponse); err != nil {
		return fmt.Errorf("feishu entrypoint %s send response: %w", r.definition.ID, err)
	}
	return nil
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
			return nil
		}
	}
	return r.sendText(ctx, chatID, content)
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
