package ilink

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	openilink "github.com/openilink/openilink-sdk-go"

	"github.com/ai-daming/xira/internal/channelrunner/dedupe"
	"github.com/ai-daming/xira/internal/entrypoints"
	frt "github.com/ai-daming/xira/internal/runtime"
)

const messageDedupeTTL = time.Hour

type client interface {
	Monitor(context.Context, openilink.MessageHandler, *openilink.MonitorOptions) error
	SendText(context.Context, string, string, string) (string, error)
	Push(context.Context, string, string) (string, error)
	Token() string
	BaseURL() string
}

type Runner struct {
	definition entrypoints.Definition
	runtime    *frt.Service
	client     client
	token      string
	baseURL    string
	stateDir   string
	syncBuf    string

	mu       sync.Mutex
	runCtx   context.Context
	cancel   context.CancelFunc
	messages *dedupe.MessageDeduper
}

func NewRunner(definition entrypoints.Definition, rt *frt.Service, stateRoot string) (*Runner, error) {
	token := resolveValue(definition.Token, definition.TokenEnv)
	if token == "" {
		return nil, fmt.Errorf("ilink entrypoint %q missing token or token_env", definition.ID)
	}
	baseURL := resolveValue(definition.BaseURL, definition.BaseURLEnv)
	opts := []openilink.Option{}
	if baseURL != "" {
		opts = append(opts, openilink.WithBaseURL(baseURL))
	}
	stateDir := strings.TrimSpace(definition.StateDir)
	if stateDir == "" {
		if strings.TrimSpace(stateRoot) == "" {
			stateRoot = filepath.Join(".xira", "state")
		}
		stateDir = filepath.Join(stateRoot, "channels", "ilink", safePathSegment(definition.ID))
	}
	runner := &Runner{
		definition: definition,
		runtime:    rt,
		client:     openilink.NewClient(token, opts...),
		token:      token,
		baseURL:    baseURL,
		stateDir:   stateDir,
		messages:   dedupe.New(filepath.Join(stateDir, "dedupe.json"), messageDedupeTTL),
	}
	slog.Info("ilink runner configured",
		"entrypoint_id", definition.ID,
		"token_env", definition.TokenEnv,
		"token_configured", token != "",
		"base_url_configured", baseURL != "",
		"state_dir", stateDir,
		"respond_to_unmentioned_group_messages", definition.RespondToUnmentionedGroupMessages,
	)
	return runner, nil
}

func (r *Runner) ID() string {
	return r.definition.ID
}

func (r *Runner) Channel() string {
	return "ilink"
}

func (r *Runner) Start(ctx context.Context) error {
	initialBuf, err := r.loadSyncBuf()
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)

	r.mu.Lock()
	r.runCtx = runCtx
	r.cancel = cancel
	r.syncBuf = initialBuf
	r.mu.Unlock()

	slog.Info("ilink runner starting",
		"entrypoint_id", r.definition.ID,
		"base_url", r.client.BaseURL(),
		"state_dir", r.stateDir,
		"has_initial_buf", initialBuf != "",
	)
	go func() {
		err := r.client.Monitor(runCtx, r.handleMessage, &openilink.MonitorOptions{
			InitialBuf: initialBuf,
			OnBufUpdate: func(buf string) {
				if err := r.saveSyncBuf(buf); err != nil {
					slog.Warn("ilink sync cursor persist failed", "entrypoint_id", r.definition.ID, "error", err)
				}
			},
			OnError: func(err error) {
				slog.Warn("ilink monitor error", "entrypoint_id", r.definition.ID, "error", err)
			},
			OnSessionExpired: func() {
				slog.Error("ilink session expired", "entrypoint_id", r.definition.ID)
			},
		})
		if err != nil && runCtx.Err() == nil {
			slog.Error("ilink runner stopped with error", "entrypoint_id", r.definition.ID, "error", err)
			return
		}
		slog.Info("ilink runner stopped", "entrypoint_id", r.definition.ID, "reason", runCtx.Err())
	}()
	return nil
}

func (r *Runner) Stop(context.Context) error {
	slog.Info("ilink runner stopping", "entrypoint_id", r.definition.ID)
	r.mu.Lock()
	cancel := r.cancel
	r.runCtx = nil
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (r *Runner) handleMessage(msg openilink.WeixinMessage) {
	ctx := r.currentContext()
	if msg.MessageType == openilink.MsgTypeBot {
		slog.Info("ilink bot echo ignored", "entrypoint_id", r.definition.ID, "message_id", messageID(msg))
		return
	}
	messageID := messageID(msg)
	senderID := strings.TrimSpace(msg.FromUserID)
	if senderID == "" {
		slog.Warn("ilink message ignored", "entrypoint_id", r.definition.ID, "reason", "missing_sender", "message_id", messageID)
		return
	}
	chatID := chatID(msg)
	chatType := chatType(msg)
	content := extractContent(msg)
	if strings.TrimSpace(content) == "" {
		content = "[empty message]"
	}
	slog.Info("ilink message received",
		"entrypoint_id", r.definition.ID,
		"chat_id", chatID,
		"chat_type", chatType,
		"message_id", messageID,
		"sender_id", senderID,
		"items", len(msg.ItemList),
		"content_chars", utf8.RuneCountInString(content),
		"content_preview", previewText(content, 120),
	)
	if !shouldHandleMessage(chatType, r.definition) {
		slog.Info("ilink group message ignored",
			"entrypoint_id", r.definition.ID,
			"chat_id", chatID,
			"message_id", messageID,
			"sender_id", senderID,
			"reason", "unmentioned_group_message",
			"respond_to_unmentioned_group_messages", r.definition.RespondToUnmentionedGroupMessages,
		)
		return
	}
	dedupeKey := r.messageDedupeKey(messageID)
	if !r.messages.Begin(dedupeKey, time.Now()) {
		slog.Info("ilink duplicate message ignored",
			"entrypoint_id", r.definition.ID,
			"chat_id", chatID,
			"message_id", messageID,
			"sender_id", senderID,
			"dedupe_ttl", messageDedupeTTL,
		)
		return
	}
	messageProcessed := false
	defer func() {
		if messageProcessed {
			r.messages.Complete(dedupeKey, time.Now())
			return
		}
		r.messages.Forget(dedupeKey)
	}()

	metadata := r.buildMetadata(msg, chatID, chatType)
	slog.Info("ilink dispatching message to runtime",
		"entrypoint_id", r.definition.ID,
		"chat_id", chatID,
		"chat_type", chatType,
		"message_id", messageID,
		"sender_id", senderID,
	)
	resp, err := r.runtime.RunAgent(ctx, frt.TurnRequest{
		EntrypointID: r.definition.ID,
		Channel:      "ilink",
		UserID:       senderID,
		Message:      content,
		Metadata:     metadata,
	})
	if err != nil {
		slog.Error("ilink runtime run failed",
			"entrypoint_id", r.definition.ID,
			"chat_id", chatID,
			"message_id", messageID,
			"sender_id", senderID,
			"error", err,
		)
		return
	}
	slog.Info("ilink runtime run completed",
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
		slog.Warn("ilink response skipped",
			"entrypoint_id", r.definition.ID,
			"run_id", resp.RunID,
			"chat_id", chatID,
			"message_id", messageID,
			"reason", "empty_final_response",
		)
		messageProcessed = true
		return
	}
	if err := r.send(ctx, msg, resp.FinalResponse); err != nil {
		slog.Error("ilink response send failed",
			"entrypoint_id", r.definition.ID,
			"run_id", resp.RunID,
			"chat_id", chatID,
			"message_id", messageID,
			"error", err,
		)
		return
	}
	messageProcessed = true
	slog.Info("ilink response sent",
		"entrypoint_id", r.definition.ID,
		"run_id", resp.RunID,
		"chat_id", chatID,
		"message_id", messageID,
		"final_response_chars", utf8.RuneCountInString(resp.FinalResponse),
	)
}

func (r *Runner) currentContext() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runCtx != nil {
		return r.runCtx
	}
	return context.Background()
}

func (r *Runner) buildMetadata(msg openilink.WeixinMessage, chatID, chatType string) map[string]string {
	metadata := map[string]string{
		"entrypoint_id":  r.definition.ID,
		"chat_id":        chatID,
		"chat_type":      chatType,
		"message_id":     messageID(msg),
		"message_type":   strconv.Itoa(int(msg.MessageType)),
		"message_state":  strconv.Itoa(int(msg.MessageState)),
		"context_token":  strings.TrimSpace(msg.ContextToken),
		"session_id":     strings.TrimSpace(msg.SessionID),
		"seq":            strconv.FormatInt(msg.Seq, 10),
		"channel_app_id": r.definition.AppID,
	}
	if r.definition.Account != "" {
		metadata["account"] = r.definition.Account
	}
	if r.definition.BotID != "" {
		metadata["bot_id"] = r.definition.BotID
	}
	if msg.GroupID != "" {
		metadata["group_id"] = strings.TrimSpace(msg.GroupID)
		metadata["space_id"] = strings.TrimSpace(msg.GroupID)
		metadata["space_type"] = "group"
	}
	if msg.ToUserID != "" {
		metadata["to_user_id"] = strings.TrimSpace(msg.ToUserID)
	}
	return compactMetadata(metadata)
}

func (r *Runner) send(ctx context.Context, msg openilink.WeixinMessage, content string) error {
	to := strings.TrimSpace(msg.FromUserID)
	if to == "" {
		return fmt.Errorf("missing ilink recipient")
	}
	if token := strings.TrimSpace(msg.ContextToken); token != "" {
		_, err := r.client.SendText(ctx, to, content, token)
		return err
	}
	_, err := r.client.Push(ctx, to, content)
	return err
}

func (r *Runner) messageDedupeKey(messageID string) string {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ""
	}
	return strings.TrimSpace(r.definition.ID) + ":" + messageID
}

func (r *Runner) syncBufPath() string {
	return filepath.Join(r.stateDir, "get_updates_buf")
}

func (r *Runner) loadSyncBuf() (string, error) {
	data, err := os.ReadFile(r.syncBufPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read ilink sync cursor: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func (r *Runner) saveSyncBuf(buf string) error {
	buf = strings.TrimSpace(buf)
	if buf == "" {
		return nil
	}
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		return fmt.Errorf("create ilink state dir: %w", err)
	}
	if err := os.WriteFile(r.syncBufPath(), []byte(buf), 0o600); err != nil {
		return fmt.Errorf("write ilink sync cursor: %w", err)
	}
	r.mu.Lock()
	r.syncBuf = buf
	r.mu.Unlock()
	return nil
}

func shouldHandleMessage(chatType string, definition entrypoints.Definition) bool {
	if chatType != "group" {
		return true
	}
	return definition.RespondToUnmentionedGroupMessages
}

func chatID(msg openilink.WeixinMessage) string {
	if msg.GroupID != "" {
		return strings.TrimSpace(msg.GroupID)
	}
	if msg.FromUserID != "" {
		return strings.TrimSpace(msg.FromUserID)
	}
	return strings.TrimSpace(msg.SessionID)
}

func chatType(msg openilink.WeixinMessage) string {
	if strings.TrimSpace(msg.GroupID) != "" {
		return "group"
	}
	return "direct"
}

func messageID(msg openilink.WeixinMessage) string {
	if msg.MessageID != 0 {
		return strconv.FormatInt(msg.MessageID, 10)
	}
	if msg.Seq != 0 {
		return "seq:" + strconv.FormatInt(msg.Seq, 10)
	}
	if msg.ClientID != "" {
		return strings.TrimSpace(msg.ClientID)
	}
	return ""
}

func extractContent(msg openilink.WeixinMessage) string {
	if text := strings.TrimSpace(openilink.ExtractText(&msg)); text != "" {
		return text
	}
	for _, item := range msg.ItemList {
		switch item.Type {
		case openilink.ItemImage:
			return "[image]"
		case openilink.ItemVoice:
			return "[voice]"
		case openilink.ItemFile:
			if item.FileItem != nil && strings.TrimSpace(item.FileItem.FileName) != "" {
				return "[file] " + strings.TrimSpace(item.FileItem.FileName)
			}
			return "[file]"
		case openilink.ItemVideo:
			return "[video]"
		}
	}
	return ""
}

func compactMetadata(metadata map[string]string) map[string]string {
	for key, value := range metadata {
		if strings.TrimSpace(value) == "" {
			delete(metadata, key)
			continue
		}
		metadata[key] = strings.TrimSpace(value)
	}
	if len(metadata) == 0 {
		return nil
	}
	return metadata
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

func safePathSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-", " ", "-")
	return replacer.Replace(value)
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

type messageDeduper = dedupe.MessageDeduper

func newMessageDeduper(ttl time.Duration) *messageDeduper {
	return dedupe.New("", ttl)
}
