package ilink

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	openilink "github.com/openilink/openilink-sdk-go"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/channelcontrol"
	"github.com/xiramesh/xira/internal/channelrunner/dedupe"
	"github.com/xiramesh/xira/internal/channelrunner/progress"
	"github.com/xiramesh/xira/internal/entrypoints"
	frt "github.com/xiramesh/xira/internal/runtime"
)

const (
	messageDedupeTTL           = time.Hour
	pairingPollIdleInterval    = 500 * time.Millisecond
	pairingPollWaitLogInterval = 10 * time.Second
)

type client interface {
	Monitor(context.Context, openilink.MessageHandler, *openilink.MonitorOptions) error
	SendText(context.Context, string, string, string) (string, error)
	Push(context.Context, string, string) (string, error)
	Token() string
	BaseURL() string
}

type qrClient interface {
	FetchQRCode(context.Context) (*openilink.QRCodeResponse, error)
	PollQRStatus(context.Context, string, ...string) (*openilink.QRStatusResponse, error)
}

type accountRecord struct {
	AccountID string    `json:"account_id"`
	Token     string    `json:"token"`
	BaseURL   string    `json:"base_url,omitempty"`
	UserID    string    `json:"user_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type accountPoller struct {
	record   accountRecord
	client   client
	stateDir string
	messages *dedupe.MessageDeduper
	cancel   context.CancelFunc
	runID    string
	running  bool
}

type pairingState struct {
	snapshot channelcontrol.PairingSnapshot
	qrcode   string
	baseURL  string
}

type Runner struct {
	definition      entrypoints.Definition
	runtime         *frt.Service
	stateDir        string
	baseURL         string
	allowPairing    bool
	clientFactory   func(accountRecord) client
	qrClientFactory func(string) qrClient

	mu       sync.Mutex
	runCtx   context.Context
	cancel   context.CancelFunc
	accounts map[string]*accountPoller
	pairings map[string]*pairingState
	router   *progress.Router
}

func NewRunner(definition entrypoints.Definition, rt *frt.Service, stateRoot string) (*Runner, error) {
	token := resolveValue(definition.Token, definition.TokenEnv)
	if token == "" && !definition.AllowRuntimePairing {
		return nil, fmt.Errorf("ilink entrypoint %q missing token or token_env", definition.ID)
	}
	baseURL := resolveValue(definition.BaseURL, definition.BaseURLEnv)
	stateDir := strings.TrimSpace(definition.StateDir)
	if stateDir == "" {
		if strings.TrimSpace(stateRoot) == "" {
			return nil, fmt.Errorf("ilink entrypoint %q requires runtime state_dir or entrypoint state_dir", definition.ID)
		}
		stateDir = filepath.Join(stateRoot, "channels", "ilink", safePathSegment(definition.ID))
	}
	runner := &Runner{
		definition:   definition,
		runtime:      rt,
		stateDir:     stateDir,
		baseURL:      baseURL,
		allowPairing: definition.AllowRuntimePairing,
		accounts:     map[string]*accountPoller{},
		pairings:     map[string]*pairingState{},
	}
	runner.clientFactory = func(account accountRecord) client {
		opts := []openilink.Option{}
		if strings.TrimSpace(account.BaseURL) != "" {
			opts = append(opts, openilink.WithBaseURL(account.BaseURL))
		}
		return openilink.NewClient(account.Token, opts...)
	}
	runner.qrClientFactory = func(baseURL string) qrClient {
		opts := []openilink.Option{}
		if strings.TrimSpace(baseURL) != "" {
			opts = append(opts, openilink.WithBaseURL(baseURL))
		}
		return openilink.NewClient("", opts...)
	}
	if token != "" {
		accountID := strings.TrimSpace(definition.BotID)
		if accountID == "" {
			accountID = "static"
		}
		now := time.Now()
		runner.accounts[accountID] = runner.newAccountPoller(accountRecord{
			AccountID: accountID,
			Token:     token,
			BaseURL:   baseURL,
			UserID:    strings.TrimSpace(definition.Account),
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	slog.Info("ilink runner configured",
		"entrypoint_id", definition.ID,
		"token_env", definition.TokenEnv,
		"token_configured", token != "",
		"base_url_configured", baseURL != "",
		"state_dir", stateDir,
		"allow_runtime_pairing", definition.AllowRuntimePairing,
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
	if err := r.loadPersistedAccounts(); err != nil {
		return err
	}
	// Per-chat-key Router (Phase 4 steering, RFC #48): routes messages to
	// new turns or steering queues. Replaces serial-blocking handleMessage.
	r.router = progress.NewRouter()
	runCtx, cancel := context.WithCancel(ctx)

	r.mu.Lock()
	r.runCtx = runCtx
	r.cancel = cancel
	accounts := make([]*accountPoller, 0, len(r.accounts))
	for _, account := range r.accounts {
		accounts = append(accounts, account)
	}
	r.mu.Unlock()

	slog.Info("ilink runner starting",
		"entrypoint_id", r.definition.ID,
		"state_dir", r.stateDir,
		"accounts", len(accounts),
		"allow_runtime_pairing", r.allowPairing,
	)
	for _, account := range accounts {
		if err := r.startAccount(runCtx, account); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) Stop(context.Context) error {
	slog.Info("ilink runner stopping", "entrypoint_id", r.definition.ID)
	r.mu.Lock()
	cancel := r.cancel
	r.runCtx = nil
	r.cancel = nil
	var accountCancels []context.CancelFunc
	for _, account := range r.accounts {
		if account.cancel != nil {
			accountCancels = append(accountCancels, account.cancel)
			account.cancel = nil
		}
		account.running = false
	}
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, accountCancel := range accountCancels {
		accountCancel()
	}
	return nil
}

func (r *Runner) CreatePairing(ctx context.Context) (channelcontrol.PairingSnapshot, error) {
	if !r.allowPairing {
		return channelcontrol.PairingSnapshot{}, fmt.Errorf("ilink entrypoint %q does not allow runtime pairing", r.definition.ID)
	}
	client := r.qrClientFactory(r.baseURL)
	qr, err := client.FetchQRCode(ctx)
	if err != nil {
		slog.Warn("ilink pairing qr fetch failed",
			"entrypoint_id", r.definition.ID,
			"base_url", r.baseURL,
			"error", err,
		)
		return channelcontrol.PairingSnapshot{}, err
	}
	now := time.Now()
	pairingID := "pair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	state := &pairingState{
		qrcode:  strings.TrimSpace(qr.QRCode),
		baseURL: r.baseURL,
		snapshot: channelcontrol.PairingSnapshot{
			PairingID:      pairingID,
			EntrypointID:   r.definition.ID,
			Status:         channelcontrol.PairingStatusWait,
			QRCode:         strings.TrimSpace(qr.QRCode),
			QRImageContent: strings.TrimSpace(qr.QRCodeImgContent),
			CreatedAt:      now,
			UpdatedAt:      now,
		},
	}
	r.mu.Lock()
	r.pairings[pairingID] = state
	runCtx := r.runCtx
	r.mu.Unlock()
	if runCtx == nil {
		runCtx = context.Background()
	}
	slog.Info("ilink pairing created",
		"entrypoint_id", r.definition.ID,
		"pairing_id", pairingID,
		"qrcode", state.snapshot.QRCode,
		"qr_image_content", state.snapshot.QRImageContent,
		"base_url", state.baseURL,
	)
	go r.pollPairing(runCtx, client, pairingID)
	return state.snapshot, nil
}

func (r *Runner) GetPairing(pairingID string) (channelcontrol.PairingSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.pairings[strings.TrimSpace(pairingID)]
	if !ok {
		return channelcontrol.PairingSnapshot{}, fmt.Errorf("pairing %q not found", pairingID)
	}
	return state.snapshot, nil
}

func (r *Runner) ListAccounts() ([]channelcontrol.AccountSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	accounts := make([]channelcontrol.AccountSnapshot, 0, len(r.accounts))
	for _, account := range r.accounts {
		accounts = append(accounts, r.accountSnapshot(account))
	}
	return accounts, nil
}

func (r *Runner) DeleteAccount(ctx context.Context, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return fmt.Errorf("account id is required")
	}
	r.mu.Lock()
	account, ok := r.accounts[accountID]
	if ok {
		if account.cancel != nil {
			account.cancel()
		}
		delete(r.accounts, accountID)
	}
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("account %q not found", accountID)
	}
	if err := os.Remove(accountRecordPath(r.accountsDir(), accountID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (r *Runner) pollPairing(ctx context.Context, client qrClient, pairingID string) {
	var lastWaitLog time.Time
	for {
		r.mu.Lock()
		state, ok := r.pairings[pairingID]
		if !ok {
			r.mu.Unlock()
			return
		}
		qrcode := state.qrcode
		baseURL := state.baseURL
		status := state.snapshot.Status
		r.mu.Unlock()
		if status == channelcontrol.PairingStatusConfirmed || status == channelcontrol.PairingStatusExpired || status == channelcontrol.PairingStatusFailed {
			return
		}
		resp, err := client.PollQRStatus(ctx, qrcode, baseURL)
		if err != nil {
			slog.Warn("ilink pairing poll failed",
				"entrypoint_id", r.definition.ID,
				"pairing_id", pairingID,
				"qrcode", qrcode,
				"base_url", baseURL,
				"previous_status", status,
				"error", err,
			)
			r.updatePairing(pairingID, func(snapshot *channelcontrol.PairingSnapshot) {
				snapshot.Status = channelcontrol.PairingStatusFailed
				snapshot.Error = err.Error()
			})
			return
		}
		nextStatus := strings.TrimSpace(resp.Status)
		if nextStatus != "" && nextStatus != status && nextStatus != channelcontrol.PairingStatusWait {
			slog.Info("ilink pairing status changed",
				"entrypoint_id", r.definition.ID,
				"pairing_id", pairingID,
				"qrcode", qrcode,
				"previous_status", status,
				"status", nextStatus,
			)
		}
		switch nextStatus {
		case channelcontrol.PairingStatusScanned:
			r.updatePairing(pairingID, func(snapshot *channelcontrol.PairingSnapshot) {
				snapshot.Status = channelcontrol.PairingStatusScanned
			})
		case channelcontrol.PairingStatusExpired:
			r.updatePairing(pairingID, func(snapshot *channelcontrol.PairingSnapshot) {
				snapshot.Status = channelcontrol.PairingStatusExpired
			})
			return
		case channelcontrol.PairingStatusConfirmed:
			accountID := strings.TrimSpace(resp.ILinkBotID)
			if accountID == "" {
				slog.Warn("ilink pairing confirmed without account id",
					"entrypoint_id", r.definition.ID,
					"pairing_id", pairingID,
					"qrcode", qrcode,
				)
				r.updatePairing(pairingID, func(snapshot *channelcontrol.PairingSnapshot) {
					snapshot.Status = channelcontrol.PairingStatusFailed
					snapshot.Error = "confirmed pairing did not return ilink_bot_id"
				})
				return
			}
			now := time.Now()
			account := accountRecord{
				AccountID: accountID,
				Token:     strings.TrimSpace(resp.BotToken),
				BaseURL:   firstNonEmpty(resp.BaseURL, r.baseURL),
				UserID:    strings.TrimSpace(resp.ILinkUserID),
				CreatedAt: now,
				UpdatedAt: now,
			}
			if account.Token == "" {
				slog.Warn("ilink pairing confirmed without bot token",
					"entrypoint_id", r.definition.ID,
					"pairing_id", pairingID,
					"qrcode", qrcode,
					"account_id", accountID,
					"user_id", account.UserID,
				)
				r.updatePairing(pairingID, func(snapshot *channelcontrol.PairingSnapshot) {
					snapshot.Status = channelcontrol.PairingStatusFailed
					snapshot.Error = "confirmed pairing did not return bot_token"
				})
				return
			}
			if err := r.addAccount(ctx, account, true); err != nil {
				slog.Warn("ilink pairing account add failed",
					"entrypoint_id", r.definition.ID,
					"pairing_id", pairingID,
					"qrcode", qrcode,
					"account_id", accountID,
					"user_id", account.UserID,
					"base_url", account.BaseURL,
					"error", err,
				)
				r.updatePairing(pairingID, func(snapshot *channelcontrol.PairingSnapshot) {
					snapshot.Status = channelcontrol.PairingStatusFailed
					snapshot.Error = err.Error()
				})
				return
			}
			r.updatePairing(pairingID, func(snapshot *channelcontrol.PairingSnapshot) {
				snapshot.Status = channelcontrol.PairingStatusConfirmed
				snapshot.AccountID = accountID
			})
			slog.Info("ilink pairing confirmed",
				"entrypoint_id", r.definition.ID,
				"pairing_id", pairingID,
				"qrcode", qrcode,
				"account_id", accountID,
				"user_id", account.UserID,
				"base_url", account.BaseURL,
			)
			return
		default:
			now := time.Now()
			if lastWaitLog.IsZero() || now.Sub(lastWaitLog) >= pairingPollWaitLogInterval {
				lastWaitLog = now
				slog.Info("ilink pairing still waiting",
					"entrypoint_id", r.definition.ID,
					"pairing_id", pairingID,
					"qrcode", qrcode,
					"status", firstNonEmpty(nextStatus, channelcontrol.PairingStatusWait),
					"base_url", baseURL,
				)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pairingPollIdleInterval):
		}
	}
}

func (r *Runner) updatePairing(pairingID string, mutate func(*channelcontrol.PairingSnapshot)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.pairings[pairingID]
	if !ok {
		return
	}
	mutate(&state.snapshot)
	state.snapshot.UpdatedAt = time.Now()
}

func (r *Runner) addAccount(ctx context.Context, record accountRecord, persist bool) error {
	record.AccountID = strings.TrimSpace(record.AccountID)
	record.Token = strings.TrimSpace(record.Token)
	if record.AccountID == "" {
		return fmt.Errorf("account id is required")
	}
	if record.Token == "" {
		return fmt.Errorf("account %q missing token", record.AccountID)
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	record.UpdatedAt = time.Now()
	if persist {
		if err := r.saveAccount(record); err != nil {
			return err
		}
	}
	poller := r.newAccountPoller(record)
	r.mu.Lock()
	if existing, ok := r.accounts[record.AccountID]; ok && existing.cancel != nil {
		existing.cancel()
	}
	r.accounts[record.AccountID] = poller
	runCtx := r.runCtx
	r.mu.Unlock()
	if runCtx != nil {
		return r.startAccount(runCtx, poller)
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (r *Runner) startAccount(ctx context.Context, account *accountPoller) error {
	if account == nil {
		return nil
	}
	initialBuf, err := loadSyncBuf(account.syncBufPath())
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	runID := uuid.NewString()
	r.mu.Lock()
	if account.running && account.cancel != nil {
		account.cancel()
	}
	account.cancel = cancel
	account.runID = runID
	account.running = true
	r.mu.Unlock()
	slog.Info("ilink account poller starting",
		"entrypoint_id", r.definition.ID,
		"account_id", account.record.AccountID,
		"base_url", account.client.BaseURL(),
		"state_dir", account.stateDir,
		"has_initial_buf", initialBuf != "",
	)
	go func() {
		err := account.client.Monitor(runCtx, func(msg openilink.WeixinMessage) {
			r.handleMessage(account, msg)
		}, &openilink.MonitorOptions{
			InitialBuf: initialBuf,
			OnBufUpdate: func(buf string) {
				if err := saveSyncBuf(account.syncBufPath(), buf); err != nil {
					slog.Warn("ilink sync cursor persist failed", "entrypoint_id", r.definition.ID, "account_id", account.record.AccountID, "error", err)
				}
			},
			OnError: func(err error) {
				slog.Warn("ilink monitor error", "entrypoint_id", r.definition.ID, "account_id", account.record.AccountID, "error", err)
			},
			OnSessionExpired: func() {
				slog.Error("ilink session expired", "entrypoint_id", r.definition.ID, "account_id", account.record.AccountID)
			},
		})
		if err != nil && runCtx.Err() == nil {
			slog.Error("ilink account poller stopped with error", "entrypoint_id", r.definition.ID, "account_id", account.record.AccountID, "error", err)
			return
		}
		r.mu.Lock()
		if account.runID == runID {
			account.running = false
			account.cancel = nil
			account.runID = ""
		}
		r.mu.Unlock()
		slog.Info("ilink account poller stopped", "entrypoint_id", r.definition.ID, "account_id", account.record.AccountID, "reason", runCtx.Err())
	}()
	return nil
}

func (r *Runner) handleMessage(account *accountPoller, msg openilink.WeixinMessage) {
	ctx := r.currentContext()
	if msg.MessageType == openilink.MsgTypeBot {
		slog.Info("ilink bot echo ignored", "entrypoint_id", r.definition.ID, "account_id", account.record.AccountID, "message_id", messageID(msg))
		return
	}
	messageID := messageID(msg)
	senderID := strings.TrimSpace(msg.FromUserID)
	if senderID == "" {
		slog.Warn("ilink message ignored", "entrypoint_id", r.definition.ID, "account_id", account.record.AccountID, "reason", "missing_sender", "message_id", messageID)
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
		"account_id", account.record.AccountID,
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
			"account_id", account.record.AccountID,
			"chat_id", chatID,
			"message_id", messageID,
			"sender_id", senderID,
			"reason", "unmentioned_group_message",
			"respond_to_unmentioned_group_messages", r.definition.RespondToUnmentionedGroupMessages,
		)
		return
	}
	dedupeKey := account.messageDedupeKey(messageID)
	if !account.messages.Begin(dedupeKey, time.Now()) {
		slog.Info("ilink duplicate message ignored",
			"entrypoint_id", r.definition.ID,
			"account_id", account.record.AccountID,
			"chat_id", chatID,
			"message_id", messageID,
			"sender_id", senderID,
			"dedupe_ttl", messageDedupeTTL,
		)
		return
	}
	// Dedupe: Begin already marked this message as "in progress". The turn
	// runs async (router goroutine), so Complete happens when the turn
	// finishes (in the onNewTurn closure). handleMessage returns immediately.
	defer func() {
		// If handleMessage returns before reaching router.Handle (e.g. error
		// in metadata), Forget so the message can be retried.
		// If router.Handle was called, the onNewTurn closure will Complete.
		// We can't know here which path was taken — so do nothing.
		// The dedupe entry stays "in progress" until the turn Completes it.
	}()

	metadata := r.buildMetadata(account, msg, chatID, chatType)
	slog.Info("ilink dispatching message to runtime",
		"entrypoint_id", r.definition.ID,
		"account_id", account.record.AccountID,
		"chat_id", chatID,
		"chat_type", chatType,
		"message_id", messageID,
		"sender_id", senderID,
	)
	inbound := channel.NewInboundContextWithEntrypoint("ilink", r.definition.ID, senderID, metadata)
	chatKey := frt.ChatKeyFromInbound(inbound)
	// Per-chat-key Router (Phase 4 steering, RFC #48): if no active turn for
	// this chatKey, starts a new turn. If active, steers (enqueues to
	// SteeringQueue). handleMessage returns immediately — Monitor can
	// receive the next message.
	//
	// If router is nil (Start not called — test scenario), run inline.
	runTurn := func(_ frt.ChatKey, turnMsg string, turnCtx context.Context) {
		// Complete dedupe when turn finishes (async — turn runs in router goroutine).
		defer account.messages.Complete(dedupeKey, time.Now())
		// Per-chat-key progress delivery: ChatContext replaces Forwarder.
		policy := progress.DefaultPolicy()
		chatCtx := progress.NewChatContext(turnCtx, progress.ChatContextConfig{
			Sender: progress.SenderFunc(func(ctx context.Context, m progress.Message) error {
				return r.send(ctx, account, msg, m.Text)
			}),
			MaxChars: policy.MaxChars,
			Policy:   policy,
		})
		chatCtx.Start()
		defer chatCtx.Stop()
		runCtx := frt.WithEventSink(turnCtx, chatCtx)
		resp, err := r.runtime.RunAgent(runCtx, frt.TurnRequest{
			EntrypointID: r.definition.ID,
			Message:      turnMsg,
			Context:      inbound,
		})
		if err != nil {
			slog.Error("ilink runtime run failed",
				"entrypoint_id", r.definition.ID,
				"account_id", account.record.AccountID,
				"chat_id", chatID,
				"message_id", messageID,
				"sender_id", senderID,
				"error", err,
			)
			return
		}
		slog.Info("ilink runtime run completed",
			"entrypoint_id", r.definition.ID,
			"account_id", account.record.AccountID,
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
				"account_id", account.record.AccountID,
				"run_id", resp.RunID,
				"chat_id", chatID,
				"message_id", messageID,
				"reason", "empty_final_response",
			)
			return
		}
		if err := r.send(turnCtx, account, msg, resp.FinalResponse); err != nil {
			slog.Error("ilink response send failed",
				"entrypoint_id", r.definition.ID,
				"account_id", account.record.AccountID,
				"run_id", resp.RunID,
				"chat_id", chatID,
				"message_id", messageID,
				"error", err,
			)
			return
		}
		slog.Info("ilink response sent",
			"entrypoint_id", r.definition.ID,
			"account_id", account.record.AccountID,
			"run_id", resp.RunID,
			"chat_id", chatID,
			"message_id", messageID,
			"final_response_chars", utf8.RuneCountInString(resp.FinalResponse),
		)
	}
	// Route through router (async, non-blocking) or run inline (test/no-Start).
	if r.router != nil {
		r.router.Handle(chatKey, content, ctx, runTurn)
	} else {
		runTurn(chatKey, content, ctx)
	}
}

func (r *Runner) currentContext() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runCtx != nil {
		return r.runCtx
	}
	return context.Background()
}

func (r *Runner) buildMetadata(account *accountPoller, msg openilink.WeixinMessage, chatID, chatType string) map[string]string {
	metadata := map[string]string{
		"entrypoint_id":  r.definition.ID,
		"account":        account.record.AccountID,
		"account_id":     account.record.AccountID,
		"chat_id":        chatID,
		"chat_type":      chatType,
		"message_id":     messageID(msg),
		"message_type":   strconv.Itoa(int(msg.MessageType)),
		"message_state":  strconv.Itoa(int(msg.MessageState)),
		"context_token":  strings.TrimSpace(msg.ContextToken),
		"session_id":     strings.TrimSpace(msg.SessionID),
		"seq":            strconv.FormatInt(msg.Seq, 10),
		"channel_app_id": account.record.AccountID,
		"bot_id":         account.record.AccountID,
	}
	if account.record.UserID != "" {
		metadata["account_user_id"] = account.record.UserID
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

func (r *Runner) send(ctx context.Context, account *accountPoller, msg openilink.WeixinMessage, content string) error {
	to := strings.TrimSpace(msg.FromUserID)
	if to == "" {
		return fmt.Errorf("missing ilink recipient")
	}
	if token := strings.TrimSpace(msg.ContextToken); token != "" {
		_, err := account.client.SendText(ctx, to, content, token)
		return err
	}
	_, err := account.client.Push(ctx, to, content)
	return err
}

func (a *accountPoller) messageDedupeKey(messageID string) string {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ""
	}
	return strings.TrimSpace(a.record.AccountID) + ":" + messageID
}

func (a *accountPoller) syncBufPath() string {
	return filepath.Join(a.stateDir, "get_updates_buf")
}

func loadSyncBuf(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read ilink sync cursor: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func saveSyncBuf(path, buf string) error {
	buf = strings.TrimSpace(buf)
	if buf == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create ilink state dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(buf), 0o600); err != nil {
		return fmt.Errorf("write ilink sync cursor: %w", err)
	}
	return nil
}

func (r *Runner) newAccountPoller(record accountRecord) *accountPoller {
	stateDir := filepath.Join(r.accountsDir(), safePathSegment(record.AccountID))
	return &accountPoller{
		record:   record,
		client:   r.clientFactory(record),
		stateDir: stateDir,
		messages: dedupe.New(filepath.Join(stateDir, "dedupe.json"), messageDedupeTTL),
	}
}

func (r *Runner) loadPersistedAccounts() error {
	entries, err := os.ReadDir(r.accountsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(r.accountsDir(), entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record accountRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("parse ilink account %s: %w", path, err)
		}
		record.AccountID = strings.TrimSpace(record.AccountID)
		if record.AccountID == "" {
			continue
		}
		r.mu.Lock()
		if _, exists := r.accounts[record.AccountID]; !exists {
			r.accounts[record.AccountID] = r.newAccountPoller(record)
		}
		r.mu.Unlock()
	}
	return nil
}

func (r *Runner) saveAccount(record accountRecord) error {
	if err := os.MkdirAll(r.accountsDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(accountRecordPath(r.accountsDir(), record.AccountID), append(data, '\n'), 0o600)
}

func (r *Runner) accountSnapshot(account *accountPoller) channelcontrol.AccountSnapshot {
	return channelcontrol.AccountSnapshot{
		AccountID:    account.record.AccountID,
		EntrypointID: r.definition.ID,
		UserID:       account.record.UserID,
		BaseURL:      account.record.BaseURL,
		StateDir:     account.stateDir,
		Running:      account.running,
		CreatedAt:    account.record.CreatedAt,
		UpdatedAt:    account.record.UpdatedAt,
	}
}

func (r *Runner) accountsDir() string {
	return filepath.Join(r.stateDir, "accounts")
}

func accountRecordPath(accountsDir, accountID string) string {
	return filepath.Join(accountsDir, safePathSegment(accountID)+".json")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
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
