package ilink

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	openilink "github.com/openilink/openilink-sdk-go"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/entrypoints"
)

func TestNewRunnerRequiresToken(t *testing.T) {
	_, err := NewRunner(entrypoints.Definition{ID: "ilink-default", Channel: "ilink"}, nil, t.TempDir())
	if err == nil {
		t.Fatal("expected missing token error")
	}
}

func TestNewRunnerUsesTokenEnv(t *testing.T) {
	t.Setenv("TEST_ILINK_TOKEN", "bot-token")
	stateRoot := t.TempDir()
	runner, err := NewRunner(entrypoints.Definition{
		ID:       "ilink-default",
		Channel:  "ilink",
		TokenEnv: "TEST_ILINK_TOKEN",
	}, nil, stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	account := runner.accounts["static"]
	if account == nil {
		t.Fatal("static account was not registered")
	}
	if account.record.Token != "bot-token" {
		t.Fatalf("token = %q", account.record.Token)
	}
	if runner.stateDir != filepath.Join(stateRoot, "channels", "ilink", "ilink-default") {
		t.Fatalf("stateDir = %q", runner.stateDir)
	}
}

func TestNewRunnerRequiresStateDir(t *testing.T) {
	t.Setenv("TEST_ILINK_TOKEN", "bot-token")
	_, err := NewRunner(entrypoints.Definition{
		ID:       "ilink-default",
		Channel:  "ilink",
		TokenEnv: "TEST_ILINK_TOKEN",
	}, nil, " ")
	if err == nil || !strings.Contains(err.Error(), "requires runtime state_dir") {
		t.Fatalf("NewRunner() error = %v, want state dir requirement", err)
	}
}

func TestNewRunnerAllowsRuntimePairingWithoutToken(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID:                  "ilink-default",
		Channel:             "ilink",
		AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !runner.allowPairing {
		t.Fatal("runtime pairing should be enabled")
	}
	if len(runner.accounts) != 0 {
		t.Fatalf("accounts = %d, want 0", len(runner.accounts))
	}
}

func TestCreatePairingConfirmsAndAddsAccount(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID:                  "ilink-default",
		Channel:             "ilink",
		AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeQR := &fakeQRClient{
		qr: &openilink.QRCodeResponse{
			QRCode:           "qr-key",
			QRCodeImgContent: "https://liteapp.weixin.qq.com/q/qr-key",
		},
		statuses: []*openilink.QRStatusResponse{
			{Status: "confirmed", BotToken: "bot-token", ILinkBotID: "bot-1", ILinkUserID: "user-1", BaseURL: "https://ilink.example"},
		},
	}
	runner.qrClientFactory = func(string) qrClient { return fakeQR }
	runner.clientFactory = func(record accountRecord) client { return &fakeClient{token: record.Token, baseURL: record.BaseURL} }
	snapshot, err := runner.CreatePairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "wait" || snapshot.QRCode != "qr-key" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		updated, err := runner.GetPairing(snapshot.PairingID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Status == "confirmed" {
			if updated.AccountID != "bot-1" {
				t.Fatalf("account id = %q", updated.AccountID)
			}
			if _, ok := runner.accounts["bot-1"]; !ok {
				t.Fatal("confirmed account was not registered")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pairing did not confirm")
}

func TestExtractContentUsesText(t *testing.T) {
	msg := openilink.WeixinMessage{ItemList: []openilink.MessageItem{
		{Type: openilink.ItemText, TextItem: &openilink.TextItem{Text: "hello"}},
	}}
	if got := extractContent(msg); got != "hello" {
		t.Fatalf("content = %q", got)
	}
}

func TestExtractContentUsesVoiceTranscript(t *testing.T) {
	msg := openilink.WeixinMessage{ItemList: []openilink.MessageItem{
		{Type: openilink.ItemVoice, VoiceItem: &openilink.VoiceItem{Text: "voice text"}},
	}}
	if got := extractContent(msg); got != "voice text" {
		t.Fatalf("content = %q", got)
	}
}

func TestExtractContentFallsBackToMediaPlaceholder(t *testing.T) {
	msg := openilink.WeixinMessage{ItemList: []openilink.MessageItem{
		{Type: openilink.ItemFile, FileItem: &openilink.FileItem{FileName: "brief.pdf"}},
	}}
	if got := extractContent(msg); got != "[file] brief.pdf" {
		t.Fatalf("content = %q", got)
	}
}

func TestChatIDAndType(t *testing.T) {
	group := openilink.WeixinMessage{FromUserID: "wxid-user", GroupID: "group-1", SessionID: "session-1"}
	if got := chatID(group); got != "group-1" {
		t.Fatalf("group chat id = %q", got)
	}
	if got := chatType(group); got != "group" {
		t.Fatalf("group chat type = %q", got)
	}

	direct := openilink.WeixinMessage{FromUserID: "wxid-user", SessionID: "session-1"}
	if got := chatID(direct); got != "wxid-user" {
		t.Fatalf("direct chat id = %q", got)
	}
	if got := chatType(direct); got != "direct" {
		t.Fatalf("direct chat type = %q", got)
	}
}

func TestBuildMetadataKeepsContextTokenAndGroupScope(t *testing.T) {
	runner := &Runner{definition: entrypoints.Definition{
		ID: "ilink-wechat",
	}}
	account := &accountPoller{record: accountRecord{AccountID: "bot-1", UserID: "owner-1"}}
	msg := openilink.WeixinMessage{
		Seq:          99,
		MessageID:    42,
		FromUserID:   "wxid-user",
		ToUserID:     "bot-1",
		SessionID:    "session-1",
		GroupID:      "group-1",
		MessageType:  openilink.MsgTypeUser,
		MessageState: openilink.StateFinish,
		ContextToken: "ctx-token",
	}

	metadata := runner.buildMetadata(account, msg, chatID(msg), chatType(msg))
	if metadata["entrypoint_id"] != "ilink-wechat" {
		t.Fatalf("entrypoint_id = %q", metadata["entrypoint_id"])
	}
	if metadata["account_id"] != "bot-1" {
		t.Fatalf("account_id = %q", metadata["account_id"])
	}
	if metadata["chat_id"] != "group-1" {
		t.Fatalf("chat_id = %q", metadata["chat_id"])
	}
	if metadata["chat_type"] != "group" {
		t.Fatalf("chat_type = %q", metadata["chat_type"])
	}
	if metadata["context_token"] != "ctx-token" {
		t.Fatalf("context_token = %q", metadata["context_token"])
	}
	if metadata["message_id"] != "42" {
		t.Fatalf("message_id = %q", metadata["message_id"])
	}
	if metadata["space_type"] != "group" {
		t.Fatalf("space_type = %q", metadata["space_type"])
	}
}

func TestSyncBufPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "get_updates_buf")
	if got, err := loadSyncBuf(path); err != nil || got != "" {
		t.Fatalf("initial sync buf = %q, err=%v", got, err)
	}
	if err := saveSyncBuf(path, "cursor-1"); err != nil {
		t.Fatal(err)
	}
	got, err := loadSyncBuf(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "cursor-1" {
		t.Fatalf("sync buf = %q", got)
	}
}

func TestMessageDeduperRejectsDuplicateUntilTTL(t *testing.T) {
	deduper := newMessageDeduper(time.Minute)
	now := time.Unix(1, 0)
	if !deduper.Begin("ilink-default:42", now) {
		t.Fatal("first message should be accepted")
	}
	deduper.Complete("ilink-default:42", now)
	if deduper.Begin("ilink-default:42", now.Add(30*time.Second)) {
		t.Fatal("duplicate should be rejected before ttl")
	}
	if !deduper.Begin("ilink-default:42", now.Add(2*time.Minute)) {
		t.Fatal("message should be accepted after ttl")
	}
}

type fakeQRClient struct {
	qr       *openilink.QRCodeResponse
	statuses []*openilink.QRStatusResponse
}

func (f *fakeQRClient) FetchQRCode(context.Context) (*openilink.QRCodeResponse, error) {
	return f.qr, nil
}

func (f *fakeQRClient) PollQRStatus(context.Context, string, ...string) (*openilink.QRStatusResponse, error) {
	if len(f.statuses) == 0 {
		return &openilink.QRStatusResponse{Status: "wait"}, nil
	}
	next := f.statuses[0]
	f.statuses = f.statuses[1:]
	return next, nil
}

type fakeClient struct {
	token   string
	baseURL string
}

func (f *fakeClient) Monitor(context.Context, openilink.MessageHandler, *openilink.MonitorOptions) error {
	return nil
}

func (f *fakeClient) SendText(context.Context, string, string, string) (string, error) {
	return "client-id", nil
}

func (f *fakeClient) Push(context.Context, string, string) (string, error) {
	return "client-id", nil
}

func (f *fakeClient) Token() string {
	return f.token
}

func (f *fakeClient) BaseURL() string {
	return f.baseURL
}

// TestIlinkShouldHandleMessageSenderAllowlist (#121): ilink's mention gate has
// no "mentioned" param (protocol has no mention concept), so only the group
// config + sender auth apply. Mirrors feishu's TestShouldHandleMessageSenderAllowlist.
func TestIlinkShouldHandleMessageSenderAllowlist(t *testing.T) {
	allowlist := entrypoints.Definition{
		RespondToUnmentionedGroupMessages: true,
		AllowedSenderIDs:                  []string{"wxid_allowed"},
	}
	// empty allowlist + respond-all → allow all (backward compat).
	respondAll := entrypoints.Definition{RespondToUnmentionedGroupMessages: true}
	if !shouldHandleMessage("group", "wxid_anyone", respondAll, nil) {
		t.Error("empty allowlist + respond-all should allow any sender")
	}
	// sender in allowlist → pass.
	if !shouldHandleMessage("group", "wxid_allowed", allowlist, nil) {
		t.Error("sender in allowlist should pass")
	}
	// sender not in allowlist + no owner → reject.
	if shouldHandleMessage("group", "wxid_blocked", allowlist, nil) {
		t.Error("sender not in allowlist + no owner should be rejected")
	}
	// sender not in allowlist + owner yes → pass. Pin entrypointID propagation (#139 review).
	ownerDef := entrypoints.Definition{
		ID:                                "ilink-owner-test",
		RespondToUnmentionedGroupMessages: true,
		AllowedSenderIDs:                  []string{"wxid_allowed"},
	}
	owner := &ilinkStubOwner{ownerSenderID: "wxid_owner"}
	if !shouldHandleMessage("group", "wxid_owner", ownerDef, owner) {
		t.Error("owner should bypass allowlist")
	}
	if owner.LastEntrypointID != "ilink-owner-test" {
		t.Errorf("owner resolver received entrypointID = %q, want %q (definition.ID, not channel)", owner.LastEntrypointID, ownerDef.ID)
	}
	// group with respond_to_unmentioned=false → reject regardless of allowlist.
	strict := entrypoints.Definition{AllowedSenderIDs: []string{"wxid_allowed"}}
	if shouldHandleMessage("group", "wxid_allowed", strict, nil) {
		t.Error("respond_to_unmentioned=false should reject even if sender in allowlist")
	}
	// direct (non-group) → mention gate passes, sender auth still applies.
	if !shouldHandleMessage("direct", "wxid_allowed", allowlist, nil) {
		t.Error("direct message from allowed sender should pass")
	}
	if shouldHandleMessage("direct", "wxid_blocked", allowlist, nil) {
		t.Error("direct message from blocked sender should be rejected")
	}
}

// ilinkStubOwner implements frt.OwnerResolver for ilink tests. Records the
// entrypointID param so integration tests can assert runners pass
// definition.ID, not channel. See PR #139 review.
type ilinkStubOwner struct {
	ownerSenderID    string
	LastEntrypointID string
}

func (s *ilinkStubOwner) IsOwner(_ context.Context, senderID, entrypointID string) bool {
	s.LastEntrypointID = entrypointID
	return senderID == s.ownerSenderID
}

// TestRunnerSetOwnerResolver covers the setter (nil-safe + value injection).
func TestRunnerSetOwnerResolver(t *testing.T) {
	r := &Runner{}
	r.SetOwnerResolver(nil) // must not panic on nil receiver guard
	if r.ownerResolver != nil {
		t.Error("SetOwnerResolver(nil) should leave field nil")
	}
	owner := &ilinkStubOwner{ownerSenderID: "wxid_x"}
	r.SetOwnerResolver(owner)
	if r.ownerResolver == nil {
		t.Error("SetOwnerResolver(stub) should set field non-nil")
	}
}

// TestIlinkRunnerIDChannelSetters covers ID()/Channel()/SetHITLResolver on
// a constructed runner (previously 0%).
func TestIlinkRunnerIDChannelSetters(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-id-test", Channel: "ilink", AllowRuntimePairing: true,
		StateDir: t.TempDir(),
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if runner.ID() != "ilink-id-test" {
		t.Errorf("ID() = %q, want ilink-id-test", runner.ID())
	}
	if runner.Channel() != "ilink" {
		t.Errorf("Channel() = %q, want ilink", runner.Channel())
	}
	runner.SetHITLResolver(nil)
	if runner.hitlResolver != nil {
		t.Error("SetHITLResolver(nil) should leave field nil")
	}
}

// TestMessageID covers the priority: MessageID > Seq > ClientID. Previously 28.6%.
func TestMessageID(t *testing.T) {
	if got := messageID(openilink.WeixinMessage{MessageID: 42}); got != "42" {
		t.Errorf("MessageID: got %q, want 42", got)
	}
	if got := messageID(openilink.WeixinMessage{Seq: 7}); got != "seq:7" {
		t.Errorf("Seq: got %q, want seq:7", got)
	}
	if got := messageID(openilink.WeixinMessage{ClientID: "c1"}); got != "c1" {
		t.Errorf("ClientID: got %q, want c1", got)
	}
	if got := messageID(openilink.WeixinMessage{}); got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}
}

// TestSyncBufPath covers accountPoller.syncBufPath (previously 0%).
func TestSyncBufPath(t *testing.T) {
	a := &accountPoller{stateDir: "/tmp/x"}
	if got := a.syncBufPath(); !strings.HasSuffix(got, "get_updates_buf") {
		t.Errorf("syncBufPath = %q, want suffix get_updates_buf", got)
	}
}

// TestAccountsDir covers Runner.accountsDir + accountRecordPath (previously uncovered).
func TestAccountsDir(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-ad", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	dir := runner.accountsDir()
	if dir == "" {
		t.Error("accountsDir should be non-empty")
	}
	path := accountRecordPath(dir, "acct-1")
	if !strings.HasSuffix(path, "acct-1.json") {
		t.Errorf("accountRecordPath = %q, want suffix acct-1.json", path)
	}
}

// TestAccountSnapshot covers accountSnapshot (previously 0%).
func TestAccountSnapshot(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-snap", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	account := &accountPoller{
		stateDir: "/tmp/x",
		running:  true,
		record: accountRecord{
			AccountID: "acct-1",
			UserID:    "u1",
			BaseURL:   "http://x",
		},
	}
	snap := runner.accountSnapshot(account)
	if snap.AccountID != "acct-1" || snap.UserID != "u1" || !snap.Running {
		t.Errorf("accountSnapshot = %+v, want acct-1/u1/running", snap)
	}
	if snap.EntrypointID != "ilink-snap" {
		t.Errorf("EntrypointID = %q, want ilink-snap", snap.EntrypointID)
	}
}

// TestIlinkListAccountsEmpty covers ListAccounts on a fresh runner (no accounts).
// Previously 0%.
func TestIlinkListAccountsEmpty(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-list", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	accounts, err := runner.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("fresh runner should have 0 accounts, got %d", len(accounts))
	}
}

// TestIlinkDeleteAccountErrors covers DeleteAccount's validation paths
// (empty id, unknown id). Previously 0%.
func TestIlinkDeleteAccountErrors(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-del", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx := context.Background()
	// empty id.
	if err := runner.DeleteAccount(ctx, "  "); err == nil {
		t.Error("DeleteAccount with empty id should error")
	}
	// unknown id.
	if err := runner.DeleteAccount(ctx, "nonexistent"); err == nil {
		t.Error("DeleteAccount with unknown id should error")
	}
}

// TestLoadPersistedAccountsEmpty covers loadPersistedAccounts on a fresh
// state dir (no accounts file). Previously 0%.
func TestLoadPersistedAccountsEmpty(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-load", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	// loadPersistedAccounts reads accounts dir; empty/nonexistent → no error.
	if err := runner.loadPersistedAccounts(); err != nil {
		t.Errorf("loadPersistedAccounts on fresh state: %v", err)
	}
}

// TestIlinkCapabilities covers Capabilities (previously uncovered).
func TestIlinkCapabilities(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-cap", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	caps := runner.Capabilities()
	if len(caps) == 0 {
		t.Error("Capabilities should be non-empty")
	}
}

// TestIlinkEmitErrorPaths covers Emit's validation branches.
func TestIlinkEmitErrorPaths(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-emit", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx := context.Background()
	// nil target.
	if err := runner.Emit(ctx, channel.OutboundEnvelope{}); err == nil {
		t.Error("Emit with nil target should error")
	}
	// empty chat_id.
	if err := runner.Emit(ctx, channel.OutboundEnvelope{Target: &channel.InboundContext{}}); err == nil {
		t.Error("Emit with empty chat_id should error")
	}
}

// TestIlinkStartStop covers Start + Stop lifecycle on a fresh runner (no
// accounts → no startAccount calls, no SDK connections). Previously 0%.
func TestIlinkStartStop(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-start", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Verify state was set.
	runner.mu.Lock()
	hasRunCtx := runner.runCtx != nil
	hasCancel := runner.cancel != nil
	hasRouter := runner.router != nil
	runner.mu.Unlock()
	if !hasRunCtx || !hasCancel || !hasRouter {
		t.Fatal("Start should set runCtx, cancel, router")
	}
	// Stop must clear + cancel.
	if err := runner.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	runner.mu.Lock()
	clearedRunCtx := runner.runCtx == nil
	clearedCancel := runner.cancel == nil
	runner.mu.Unlock()
	if !clearedRunCtx || !clearedCancel {
		t.Error("Stop should clear runCtx and cancel")
	}
	// Stop without Start must not panic.
	runner2, err := NewRunner(entrypoints.Definition{
		ID: "ilink-nostart", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if err := runner2.Stop(ctx); err != nil {
		t.Errorf("Stop without Start should be no-op, got: %v", err)
	}
}

// TestCreatePairingExpires covers pollPairing's Expired branch (previously
// uncovered in pollPairing's 44.3%).
func TestCreatePairingExpires(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-exp", Channel: "ilink", AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeQR := &fakeQRClient{
		qr:       &openilink.QRCodeResponse{QRCode: "qr-exp"},
		statuses: []*openilink.QRStatusResponse{{Status: "expired"}},
	}
	runner.qrClientFactory = func(string) qrClient { return fakeQR }
	snapshot, err := runner.CreatePairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := runner.GetPairing(snapshot.PairingID)
		if err == nil && got.Status == "expired" {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := runner.GetPairing(snapshot.PairingID)
	t.Fatalf("pairing did not expire, status = %q", got.Status)
}

// TestCreatePairingPollError covers pollPairing's error branch (PollQRStatus
// returns error → Failed status).
func TestCreatePairingPollError(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-err", Channel: "ilink", AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeQR := &errQRClient{}
	runner.qrClientFactory = func(string) qrClient { return fakeQR }
	snapshot, err := runner.CreatePairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := runner.GetPairing(snapshot.PairingID)
		if err == nil && got.Status == "failed" {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := runner.GetPairing(snapshot.PairingID)
	t.Fatalf("pairing did not fail on poll error, status = %q", got.Status)
}

// errQRClient is a qrClient whose PollQRStatus always errors.
type errQRClient struct{}

func (errQRClient) FetchQRCode(context.Context) (*openilink.QRCodeResponse, error) {
	return &openilink.QRCodeResponse{QRCode: "qr-err"}, nil
}

func (errQRClient) PollQRStatus(context.Context, string, ...string) (*openilink.QRStatusResponse, error) {
	return nil, fmt.Errorf("poll failed")
}

// TestExtractContentIlink covers extractContent's branches (text / voice / fallback).
// Previously 54.5%.
func TestExtractContentIlink(t *testing.T) {
	// text message.
	msg := openilink.WeixinMessage{
		ItemList: []openilink.MessageItem{{
			Type:     openilink.ItemText,
			TextItem: &openilink.TextItem{Text: "hello"},
		}},
	}
	if got := extractContent(msg); got != "hello" {
		t.Errorf("text: got %q, want hello", got)
	}
	// empty.
	if got := extractContent(openilink.WeixinMessage{}); got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}
}

// TestLoadPersistedAccounts covers loadPersistedAccounts with real files:
// valid account, invalid JSON, empty accountID, non-json file. Previously 17.4%.
func TestLoadPersistedAccounts(t *testing.T) {
	stateDir := t.TempDir()
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-load2", Channel: "ilink", StateDir: stateDir, AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	accountsDir := runner.accountsDir()
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(accountsDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// valid account.
	write("acct-1.json", `{"account_id":"acct-1","token":"tok-1"}`)
	// non-json file (skipped).
	write("readme.txt", `ignore me`)
	// empty accountID (skipped).
	write("empty.json", `{"account_id":"","token":"x"}`)
	// invalid JSON → error.
	write("bad.json", `{not json`)

	err = runner.loadPersistedAccounts()
	if err == nil {
		t.Fatal("loadPersistedAccounts with invalid JSON should error")
	}
	// Remove bad file, retry → should succeed and load acct-1.
	os.Remove(filepath.Join(accountsDir, "bad.json"))
	runner.accounts = map[string]*accountPoller{} // reset
	if err := runner.loadPersistedAccounts(); err != nil {
		t.Fatalf("loadPersistedAccounts after removing bad file: %v", err)
	}
	if _, ok := runner.accounts["acct-1"]; !ok {
		t.Error("valid account acct-1 should be loaded")
	}
}

// TestIlinkSendError covers send's error path (no account loaded → error).
// Previously 62.5%.
func TestIlinkSendError(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-send-err", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	// Emit with content but no account to send through → error.
	err = runner.Emit(context.Background(), channel.OutboundEnvelope{
		Type:   channel.OutboundAssistantFinal,
		Target: &channel.InboundContext{ChatID: "c1", Account: "missing-acct"},
		Data:   map[string]any{"content": "hi"},
	})
	if err == nil {
		t.Error("Emit with unknown account should error")
	}
}

// TestCreatePairingConfirmedMissingAccountID covers pollPairing's confirmed
// branch when ILinkBotID is empty → Failed. Previously uncovered.
func TestCreatePairingConfirmedMissingAccountID(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-no-botid", Channel: "ilink", AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeQR := &fakeQRClient{
		qr:       &openilink.QRCodeResponse{QRCode: "qr-x"},
		statuses: []*openilink.QRStatusResponse{{Status: "confirmed", BotToken: "tok"}}, // no ILinkBotID
	}
	runner.qrClientFactory = func(string) qrClient { return fakeQR }
	snapshot, err := runner.CreatePairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := runner.GetPairing(snapshot.PairingID)
		if err == nil && got.Status == "failed" && strings.Contains(got.Error, "ilink_bot_id") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := runner.GetPairing(snapshot.PairingID)
	t.Fatalf("expected failed (no bot_id), status=%q error=%q", got.Status, got.Error)
}

// TestCreatePairingConfirmedMissingToken covers pollPairing's confirmed branch
// when BotToken is empty → Failed. Previously uncovered.
func TestCreatePairingConfirmedMissingToken(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-no-token", Channel: "ilink", AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeQR := &fakeQRClient{
		qr:       &openilink.QRCodeResponse{QRCode: "qr-y"},
		statuses: []*openilink.QRStatusResponse{{Status: "confirmed", ILinkBotID: "bot-1"}}, // no BotToken
	}
	runner.qrClientFactory = func(string) qrClient { return fakeQR }
	snapshot, err := runner.CreatePairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := runner.GetPairing(snapshot.PairingID)
		if err == nil && got.Status == "failed" && strings.Contains(got.Error, "bot_token") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := runner.GetPairing(snapshot.PairingID)
	t.Fatalf("expected failed (no token), status=%q error=%q", got.Status, got.Error)
}

// TestCreatePairingScanedStatus covers pollPairing's scanned branch (status
// changes to scanned, then expires). Previously the scanned branch was
// only covered indirectly.
func TestCreatePairingScanedStatus(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-scan", Channel: "ilink", AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeQR := &fakeQRClient{
		qr: &openilink.QRCodeResponse{QRCode: "qr-scan"},
		statuses: []*openilink.QRStatusResponse{
			{Status: "scaned"},
			{Status: "expired"},
		},
	}
	runner.qrClientFactory = func(string) qrClient { return fakeQR }
	snapshot, err := runner.CreatePairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := runner.GetPairing(snapshot.PairingID)
		if err == nil && got.Status == "expired" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := runner.GetPairing(snapshot.PairingID)
	t.Fatalf("expected expired after scan, status=%q", got.Status)
}

// TestCreatePairingConfirmedWithStartedRunner covers startAccount via the
// confirmed pairing path when the runner is Start()ed (runCtx != nil).
// addAccount then calls startAccount → fakeClient.Monitor returns nil
// immediately (goroutine exits). Previously startAccount was 0%.
func TestCreatePairingConfirmedWithStartedRunner(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-started", Channel: "ilink", AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner.qrClientFactory = func(string) qrClient {
		return &fakeQRClient{
			qr:       &openilink.QRCodeResponse{QRCode: "qr-z"},
			statuses: []*openilink.QRStatusResponse{{Status: "confirmed", BotToken: "tok", ILinkBotID: "bot-started", ILinkUserID: "u1", BaseURL: "http://x"}},
		}
	}
	runner.clientFactory = func(record accountRecord) client { return &fakeClient{token: record.Token, baseURL: record.BaseURL} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer runner.Stop(ctx)
	snapshot, err := runner.CreatePairing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := runner.GetPairing(snapshot.PairingID)
		if err == nil && got.Status == "confirmed" {
			return // startAccount was called, account added
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := runner.GetPairing(snapshot.PairingID)
	t.Fatalf("expected confirmed, status=%q", got.Status)
}

// TestIlinkDeleteAccountSuccess covers DeleteAccount's happy path (account
// exists → cancel + delete from map + remove file). Previously 60%.
func TestIlinkDeleteAccountSuccess(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-del-ok", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Add an account (persist=true writes file). runner not Started so runCtx=nil,
	// addAccount won't call startAccount.
	record := accountRecord{AccountID: "acct-del", Token: "tok", BaseURL: "http://x"}
	if err := runner.addAccount(ctx, record, true); err != nil {
		t.Fatalf("addAccount: %v", err)
	}
	// Verify it's there.
	accounts, _ := runner.ListAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	// Delete it.
	if err := runner.DeleteAccount(ctx, "acct-del"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	accounts, _ = runner.ListAccounts()
	if len(accounts) != 0 {
		t.Errorf("after delete, expected 0 accounts, got %d", len(accounts))
	}
	// Delete again (file already gone) → not found error.
	if err := runner.DeleteAccount(ctx, "acct-del"); err == nil {
		t.Error("delete again should error (not found)")
	}
}

// TestExtractContentIlinkAllTypes covers all item type branches in extractContent.
// Previously 63.6%.
func TestExtractContentIlinkAllTypes(t *testing.T) {
	// image.
	if got := extractContent(openilink.WeixinMessage{
		ItemList: []openilink.MessageItem{{Type: openilink.ItemImage}},
	}); got != "[image]" {
		t.Errorf("image: got %q, want [image]", got)
	}
	// voice.
	if got := extractContent(openilink.WeixinMessage{
		ItemList: []openilink.MessageItem{{Type: openilink.ItemVoice}},
	}); got != "[voice]" {
		t.Errorf("voice: got %q, want [voice]", got)
	}
	// file with name.
	if got := extractContent(openilink.WeixinMessage{
		ItemList: []openilink.MessageItem{{Type: openilink.ItemFile, FileItem: &openilink.FileItem{FileName: "doc.pdf"}}},
	}); got != "[file] doc.pdf" {
		t.Errorf("file with name: got %q, want [file] doc.pdf", got)
	}
	// file without name.
	if got := extractContent(openilink.WeixinMessage{
		ItemList: []openilink.MessageItem{{Type: openilink.ItemFile}},
	}); got != "[file]" {
		t.Errorf("file without name: got %q, want [file]", got)
	}
	// video.
	if got := extractContent(openilink.WeixinMessage{
		ItemList: []openilink.MessageItem{{Type: openilink.ItemVideo}},
	}); got != "[video]" {
		t.Errorf("video: got %q, want [video]", got)
	}
}

// TestIlinkSendPaths covers send's branches: missing recipient, context-token
// path (SendText), push path (Push). Previously 62.5%.
func TestIlinkSendPaths(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-send", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{token: "tok", baseURL: "http://x"}
	account := &accountPoller{record: accountRecord{AccountID: "a1"}, client: fc}
	ctx := context.Background()
	// missing recipient.
	if err := runner.send(ctx, account, openilink.WeixinMessage{}, "hi"); err == nil {
		t.Error("send with empty recipient should error")
	}
	// context-token path (SendText).
	if err := runner.send(ctx, account, openilink.WeixinMessage{
		FromUserID: "u1", ContextToken: "ct",
	}, "hi"); err != nil {
		t.Errorf("send with context token: %v", err)
	}
	// push path (no context token).
	if err := runner.send(ctx, account, openilink.WeixinMessage{
		FromUserID: "u1",
	}, "hi"); err != nil {
		t.Errorf("send push: %v", err)
	}
}

// TestPreviewText covers previewText's truncation + unicode handling. 71.4%.
func TestPreviewText(t *testing.T) {
	if got := previewText("short", 10); got != "short" {
		t.Errorf("short: got %q, want short", got)
	}
	if got := previewText("", 10); got != "" {
		t.Errorf("empty: got %q", got)
	}
	long := strings.Repeat("a", 200)
	got := previewText(long, 50)
	if len(got) > 53 { // 50 + "..." = 53 max
		t.Errorf("truncated length = %d, want <= 53", len(got))
	}
}

// TestSaveSyncBuf covers saveSyncBuf + loadSyncBuf round-trip. 62.5%.
func TestSaveSyncBuf(t *testing.T) {
	account := &accountPoller{stateDir: t.TempDir()}
	if err := saveSyncBuf(account.syncBufPath(), "cursor-data"); err != nil {
		t.Fatalf("saveSyncBuf: %v", err)
	}
	loaded, err := loadSyncBuf(account.syncBufPath())
	if err != nil {
		t.Fatalf("loadSyncBuf: %v", err)
	}
	if loaded != "cursor-data" {
		t.Errorf("round-trip = %q, want cursor-data", loaded)
	}
}

// TestIlinkHandleMessageBotEcho covers handleMessage's bot-echo early return
// (MsgTypeBot → ignored). Uses a fully constructed accountPoller to avoid nil
// fields. Previously uncovered branch.
func TestIlinkHandleMessageBotEcho(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-bot", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	account := runner.newAccountPoller(accountRecord{AccountID: "a1", Token: "tok", BaseURL: "http://x"})
	// bot echo → early return, no panic, no downstream processing.
	runner.handleMessage(account, openilink.WeixinMessage{MessageType: openilink.MsgTypeBot})
	// missing sender → early return.
	runner.handleMessage(account, openilink.WeixinMessage{MessageType: openilink.MsgTypeUser})
}

// TestMessageDedupeKey covers the empty-messageID branch. 75%.
func TestMessageDedupeKey(t *testing.T) {
	account := &accountPoller{record: accountRecord{AccountID: "a1"}}
	if got := account.messageDedupeKey(""); got != "" {
		t.Errorf("empty messageID: got %q, want empty", got)
	}
	if got := account.messageDedupeKey("m1"); got == "" {
		t.Error("non-empty messageID should produce non-empty key")
	}
}

// TestFirstNonEmpty covers the all-empty branch. 75%.
func TestFirstNonEmptyEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", ""); got != "" {
		t.Errorf("all empty: got %q, want empty", got)
	}
	if got := firstNonEmpty("", "x"); got != "x" {
		t.Errorf("second non-empty: got %q, want x", got)
	}
}

// TestChatIDAndTypeFallbacks covers chatID's FromUserID/SessionID fallbacks
// + chatType's direct branch. Both 80%.
func TestChatIDAndTypeFallbacks(t *testing.T) {
	// group → group id + "group".
	if got := chatID(openilink.WeixinMessage{GroupID: "g1"}); got != "g1" {
		t.Errorf("group: got %q, want g1", got)
	}
	if got := chatType(openilink.WeixinMessage{GroupID: "g1"}); got != "group" {
		t.Errorf("group type: got %q, want group", got)
	}
	// no group → FromUserID fallback + "direct".
	if got := chatID(openilink.WeixinMessage{FromUserID: "u1"}); got != "u1" {
		t.Errorf("user fallback: got %q, want u1", got)
	}
	if got := chatType(openilink.WeixinMessage{FromUserID: "u1"}); got != "direct" {
		t.Errorf("direct type: got %q, want direct", got)
	}
	// no group/user → SessionID fallback.
	if got := chatID(openilink.WeixinMessage{SessionID: "s1"}); got != "s1" {
		t.Errorf("session fallback: got %q, want s1", got)
	}
	// all empty.
	if got := chatID(openilink.WeixinMessage{}); got != "" {
		t.Errorf("empty: got %q", got)
	}
}

// TestIlinkStopWithAccountCancel covers Stop's account-cancel loop (line 219-231):
// when accounts have cancel set (via startAccount), Stop must cancel them all.
func TestIlinkStopWithAccountCancel(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-stop-acct", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner.qrClientFactory = func(string) qrClient {
		return &fakeQRClient{qr: &openilink.QRCodeResponse{QRCode: "qr"},
			statuses: []*openilink.QRStatusResponse{{Status: "confirmed", BotToken: "t", ILinkBotID: "b1", ILinkUserID: "u1", BaseURL: "http://x"}}}
	}
	runner.clientFactory = func(r accountRecord) client { return &fakeClient{token: r.Token, baseURL: r.BaseURL} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Pair to add an account (startAccount sets account.cancel).
	snap, err := runner.CreatePairing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for confirmed so account is added + startAccount ran.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := runner.GetPairing(snap.PairingID)
		if err == nil && got.Status == "confirmed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Stop must cancel all account contexts without panic.
	if err := runner.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestAddAccountValidation covers addAccount's validation branches
// (empty accountID, missing token). Previously 79.2%.
func TestAddAccountValidation(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-add-val", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// empty accountID.
	if err := runner.addAccount(ctx, accountRecord{Token: "tok"}, false); err == nil {
		t.Error("empty accountID should error")
	}
	// missing token.
	if err := runner.addAccount(ctx, accountRecord{AccountID: "a1"}, false); err == nil {
		t.Error("missing token should error")
	}
	// valid (no persist, runCtx nil) → success, account in map.
	if err := runner.addAccount(ctx, accountRecord{AccountID: "a2", Token: "tok"}, false); err != nil {
		t.Fatalf("valid addAccount: %v", err)
	}
	if _, ok := runner.accounts["a2"]; !ok {
		t.Error("a2 should be in accounts map")
	}
}

// TestDefaultClientFactories covers the default client/qrClient factories set
// by NewRunner (lines 130-142). These closures are assigned but their bodies
// only run when invoked — tests usually override them, leaving the default
// bodies uncovered.
func TestDefaultClientFactories(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "ilink-factories", Channel: "ilink", StateDir: t.TempDir(), AllowRuntimePairing: true,
	}, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Default clientFactory: returns openilink.NewClient with baseURL.
	c := runner.clientFactory(accountRecord{Token: "tok", BaseURL: "http://x"})
	if c == nil {
		t.Error("default clientFactory returned nil")
	}
	// Default qrClientFactory: empty baseURL branch + non-empty branch.
	qrEmpty := runner.qrClientFactory("")
	if qrEmpty == nil {
		t.Error("default qrClientFactory (empty) returned nil")
	}
	qrNonEmpty := runner.qrClientFactory("http://y")
	if qrNonEmpty == nil {
		t.Error("default qrClientFactory (non-empty) returned nil")
	}
}
