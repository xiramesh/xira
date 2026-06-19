package feishu

import (
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
