package feishu

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/entrypoints"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestExtractContentText(t *testing.T) {
	got := extractContent(larkim.MsgTypeText, `{"text":"hello"}`)
	if got != "hello" {
		t.Fatalf("content = %q", got)
	}
}

func TestStripMentionPlaceholders(t *testing.T) {
	key := "@_user_1"
	got := stripMentionPlaceholders("hello @_user_1 world", []*larkim.MentionEvent{{Key: &key}})
	if got != "hello  world" {
		t.Fatalf("content = %q", got)
	}
}

func TestBuildMarkdownCard(t *testing.T) {
	card, err := buildMarkdownCard("hello")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(card), &parsed); err != nil {
		t.Fatalf("card is not json: %v", err)
	}
	if parsed["schema"] != "2.0" {
		t.Fatalf("schema = %v", parsed["schema"])
	}
}

func TestShouldHandleMessageRespectsGroupMentionPolicy(t *testing.T) {
	defaultDefinition := entrypoints.Definition{}
	if !shouldHandleMessage("direct", false, defaultDefinition) {
		t.Fatal("direct messages should be handled")
	}
	if !shouldHandleMessage("group", true, defaultDefinition) {
		t.Fatal("mentioned group messages should be handled")
	}
	if shouldHandleMessage("group", false, defaultDefinition) {
		t.Fatal("unmentioned group messages should be ignored by default")
	}

	respondAllGroups := entrypoints.Definition{RespondToUnmentionedGroupMessages: true}
	if !shouldHandleMessage("group", false, respondAllGroups) {
		t.Fatal("unmentioned group messages should be handled when configured")
	}
}

func TestMessageDeduperRejectsInFlightDuplicate(t *testing.T) {
	deduper := newMessageDeduper(time.Minute)
	now := time.Unix(1, 0)

	if !deduper.Begin("feishu-default:om-1", now) {
		t.Fatal("first message should be accepted")
	}
	if deduper.Begin("feishu-default:om-1", now.Add(time.Second)) {
		t.Fatal("duplicate in-flight message should be rejected")
	}
}

func TestChannelStateDirRequiresRuntimeOrEntrypointStateDir(t *testing.T) {
	stateRoot := t.TempDir()
	got, err := channelStateDir(entrypoints.Definition{ID: "feishu/default"}, stateRoot, "feishu")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(stateRoot, "channels", "feishu", "feishu_default") {
		t.Fatalf("state dir = %q", got)
	}

	got, err = channelStateDir(entrypoints.Definition{ID: "feishu-default", StateDir: "custom-state"}, "", "feishu")
	if err != nil {
		t.Fatal(err)
	}
	if got != "custom-state" {
		t.Fatalf("explicit state dir = %q", got)
	}

	if _, err := channelStateDir(entrypoints.Definition{ID: "feishu-default"}, " ", "feishu"); err == nil || !strings.Contains(err.Error(), "requires runtime state_dir") {
		t.Fatalf("channelStateDir() error = %v, want state dir requirement", err)
	}
}

func TestMessageDeduperKeepsCompletedMessageUntilTTL(t *testing.T) {
	deduper := newMessageDeduper(time.Minute)
	now := time.Unix(1, 0)

	if !deduper.Begin("feishu-default:om-1", now) {
		t.Fatal("first message should be accepted")
	}
	deduper.Complete("feishu-default:om-1", now.Add(10*time.Second))
	if deduper.Begin("feishu-default:om-1", now.Add(30*time.Second)) {
		t.Fatal("completed message should be rejected before ttl")
	}
	if !deduper.Begin("feishu-default:om-1", now.Add(2*time.Minute)) {
		t.Fatal("completed message should be accepted after ttl")
	}
}

func TestMessageDeduperForgetAllowsRetry(t *testing.T) {
	deduper := newMessageDeduper(time.Minute)
	now := time.Unix(1, 0)

	if !deduper.Begin("feishu-default:om-1", now) {
		t.Fatal("first message should be accepted")
	}
	deduper.Forget("feishu-default:om-1")
	if !deduper.Begin("feishu-default:om-1", now.Add(time.Second)) {
		t.Fatal("forgotten message should be accepted for retry")
	}
}

func TestRunnerMessageDedupeKeyIncludesEntrypoint(t *testing.T) {
	runner := &Runner{definition: entrypoints.Definition{ID: "feishu-default"}}
	if got := runner.messageDedupeKey("om-1"); got != "feishu-default:om-1" {
		t.Fatalf("dedupe key = %q", got)
	}
	if got := runner.messageDedupeKey(""); got != "" {
		t.Fatalf("empty message id dedupe key = %q", got)
	}
}

// handleMessageRead is the sink for im.message.message_read_v1 (message-read
// receipts). xira does not yet consume read receipts; for now it just logs and
// returns nil so the SDK stops emitting "not found handler" errors on every
// read event. When we want to use read state, extend this method.
func TestRunnerHandlesMessageReadEvent(t *testing.T) {
	runner := &Runner{definition: entrypoints.Definition{ID: "feishu-default"}}

	// nil / partial payloads must be a safe no-op (mirrors handleMessageReceive
	// nil-guard discipline), never a panic.
	if err := runner.handleMessageRead(context.Background(), nil); err != nil {
		t.Fatalf("nil event returned err = %v", err)
	}
	if err := runner.handleMessageRead(context.Background(), &larkim.P2MessageReadV1{}); err != nil {
		t.Fatalf("empty event returned err = %v", err)
	}

	// A populated event must return nil (handled = swallowed) rather than an
	// error; the contract is "we accept the receipt silently for now".
	readerID := "ou_reader"
	readTime := "1609484183000"
	msgID := "om_read_1"
	evt := &larkim.P2MessageReadV1{
		Event: &larkim.P2MessageReadV1Data{
			Reader: &larkim.EventMessageReader{
				ReaderId: &larkim.UserId{OpenId: &readerID},
				ReadTime: &readTime,
			},
			MessageIdList: []string{msgID},
		},
	}
	if err := runner.handleMessageRead(context.Background(), evt); err != nil {
		t.Fatalf("populated event returned err = %v", err)
	}

	// Reader id resolution: open_id is preferred, then user_id, then union_id.
	// Each form must resolve without error.
	for name, uid := range map[string]*larkim.UserId{
		"user_id":  {UserId: ptr("u_user")},
		"union_id": {UnionId: ptr("on_union")},
	} {
		e := &larkim.P2MessageReadV1{
			Event: &larkim.P2MessageReadV1Data{
				Reader: &larkim.EventMessageReader{ReaderId: uid},
			},
		}
		if err := runner.handleMessageRead(context.Background(), e); err != nil {
			t.Fatalf("%s reader id returned err = %v", name, err)
		}
	}

	// Reader present but no readable id at all (all nil) must still be safe.
	noIDEvt := &larkim.P2MessageReadV1{
		Event: &larkim.P2MessageReadV1Data{
			Reader: &larkim.EventMessageReader{ReaderId: &larkim.UserId{}},
		},
	}
	if err := runner.handleMessageRead(context.Background(), noIDEvt); err != nil {
		t.Fatalf("nil reader id returned err = %v", err)
	}
}

func ptr(s string) *string { return &s }
