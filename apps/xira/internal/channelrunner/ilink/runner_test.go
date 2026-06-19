package ilink

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	openilink "github.com/openilink/openilink-sdk-go"

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
