package ilink

import (
	"path/filepath"
	"testing"
	"time"

	openilink "github.com/openilink/openilink-sdk-go"

	"github.com/ai-daming/xira/internal/entrypoints"
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
	if runner.token != "bot-token" {
		t.Fatalf("token = %q", runner.token)
	}
	if runner.stateDir != filepath.Join(stateRoot, "channels", "ilink", "ilink-default") {
		t.Fatalf("stateDir = %q", runner.stateDir)
	}
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
		ID:      "ilink-wechat",
		Account: "personal-wechat",
		AppID:   "ilink-app",
		BotID:   "bot-1",
	}}
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

	metadata := runner.buildMetadata(msg, chatID(msg), chatType(msg))
	if metadata["entrypoint_id"] != "ilink-wechat" {
		t.Fatalf("entrypoint_id = %q", metadata["entrypoint_id"])
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
	runner := &Runner{stateDir: t.TempDir()}
	if got, err := runner.loadSyncBuf(); err != nil || got != "" {
		t.Fatalf("initial sync buf = %q, err=%v", got, err)
	}
	if err := runner.saveSyncBuf("cursor-1"); err != nil {
		t.Fatal(err)
	}
	got, err := runner.loadSyncBuf()
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
