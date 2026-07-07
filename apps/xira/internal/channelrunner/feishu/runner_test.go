package feishu

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/entrypoints"

	lark "github.com/larksuite/oapi-sdk-go/v3"
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
	// Backward-compat: empty AllowedSenderIDs → allowlist always passes, so
	// behavior is unchanged (mention gate only). Use a fixed sender for all calls.
	if !shouldHandleMessage("direct", false, "ou_1", defaultDefinition, nil) {
		t.Fatal("direct messages should be handled")
	}
	if !shouldHandleMessage("group", true, "ou_1", defaultDefinition, nil) {
		t.Fatal("mentioned group messages should be handled")
	}
	if shouldHandleMessage("group", false, "ou_1", defaultDefinition, nil) {
		t.Fatal("unmentioned group messages should be ignored by default")
	}

	respondAllGroups := entrypoints.Definition{RespondToUnmentionedGroupMessages: true}
	if !shouldHandleMessage("group", false, "ou_1", respondAllGroups, nil) {
		t.Fatal("unmentioned group messages should be handled when configured")
	}
}

// TestShouldHandleMessageSenderAllowlist covers the #121 sender authorization
// gate that's AND-combined with the mention gate. Cases:
//   - empty allowlist = allow all (backward compat)
//   - sender in allowlist → pass
//   - sender not in allowlist + no owner → reject
//   - sender not in allowlist + owner says yes → pass (owner bypass, #122)
//   - mention gate fails + sender in allowlist → still reject (AND)
func TestShouldHandleMessageSenderAllowlist(t *testing.T) {
	allowlist := entrypoints.Definition{
		RespondToUnmentionedGroupMessages: true, // ensure mention gate passes, isolate allowlist
		AllowedSenderIDs:                  []string{"ou_allowed"},
	}
	// empty allowlist → allow all.
	if !shouldHandleMessage("group", true, "ou_anyone", entrypoints.Definition{}, nil) {
		t.Error("empty allowlist should allow any mentioned sender")
	}
	// sender in allowlist → pass.
	if !shouldHandleMessage("group", true, "ou_allowed", allowlist, nil) {
		t.Error("sender in allowlist should pass")
	}
	// sender not in allowlist + no owner → reject.
	if shouldHandleMessage("group", true, "ou_blocked", allowlist, nil) {
		t.Error("sender not in allowlist + no owner should be rejected")
	}
	// sender not in allowlist + owner says yes → pass.
	if !shouldHandleMessage("group", true, "ou_owner", allowlist, &stubOwnerResolver{ownerSenderID: "ou_owner"}) {
		t.Error("owner should bypass allowlist")
	}
	// mention gate fails + sender in allowlist → still reject (AND).
	strictGroup := entrypoints.Definition{
		RespondToUnmentionedGroupMessages: false,
		AllowedSenderIDs:                  []string{"ou_allowed"},
	}
	if shouldHandleMessage("group", false, "ou_allowed", strictGroup, nil) {
		t.Error("mention gate fail should reject even if sender is in allowlist (AND)")
	}
}

// stubOwnerResolver implements frt.OwnerResolver for tests. It returns true
// only for the configured owner senderID. The third param (entrypointID) is
// recorded into LastEntrypointID so integration tests can assert runners pass
// definition.ID (not a channel name) — owner bypass is a privilege boundary.
// See PR #139 review: pin that runners pass entrypoint ID, not channel.
type stubOwnerResolver struct {
	ownerSenderID    string
	LastEntrypointID string
}

func (s *stubOwnerResolver) IsOwner(_ context.Context, senderID, entrypointID string) bool {
	s.LastEntrypointID = entrypointID
	return senderID == s.ownerSenderID
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

// TestRunnerSetOwnerResolver covers the setter (nil-safe + value injection).
func TestRunnerSetOwnerResolver(t *testing.T) {
	r := &Runner{}
	r.SetOwnerResolver(nil) // nil-safe
	if r.ownerResolver != nil {
		t.Error("SetOwnerResolver(nil) should leave field nil")
	}
	owner := &stubOwnerResolver{ownerSenderID: "ou_x"}
	r.SetOwnerResolver(owner)
	if r.ownerResolver == nil {
		t.Error("SetOwnerResolver(stub) should set field non-nil")
	}
}

// TestExtractSenderID covers the priority order: user_id > open_id > union_id
// + nil guards. Previously 33.3%.
func TestExtractSenderID(t *testing.T) {
	if got := extractSenderID(nil); got != "" {
		t.Errorf("extractSenderID(nil) = %q, want empty", got)
	}
	if got := extractSenderID(&larkim.EventSender{}); got != "" {
		t.Errorf("extractSenderID(empty) = %q, want empty", got)
	}
	// user_id wins over open_id.
	sender := &larkim.EventSender{SenderId: &larkim.UserId{
		UserId: strPtr("u1"),
		OpenId: strPtr("o1"),
	}}
	if got := extractSenderID(sender); got != "u1" {
		t.Errorf("user_id priority: got %q, want u1", got)
	}
	// open_id when no user_id.
	sender.SenderId.UserId = nil
	if got := extractSenderID(sender); got != "o1" {
		t.Errorf("open_id fallback: got %q, want o1", got)
	}
	// union_id when no user_id/open_id.
	sender.SenderId.OpenId = nil
	sender.SenderId.UnionId = strPtr("un1")
	if got := extractSenderID(sender); got != "un1" {
		t.Errorf("union_id fallback: got %q, want un1", got)
	}
}

// TestFirstJSONStringField covers JSON content field extraction.
// Previously 0%.
func TestFirstJSONStringField(t *testing.T) {
	if got := firstJSONStringField("not json", "text"); got != "" {
		t.Errorf("invalid json: got %q, want empty", got)
	}
	if got := firstJSONStringField(`{"text":"hello"}`, "text"); got != "hello" {
		t.Errorf("basic field: got %q, want hello", got)
	}
	// First matching field wins.
	if got := firstJSONStringField(`{"a":"1","b":"2"}`, "a", "b"); got != "1" {
		t.Errorf("first-match: got %q, want 1", got)
	}
	// Fallback to second field.
	if got := firstJSONStringField(`{"b":"2"}`, "a", "b"); got != "2" {
		t.Errorf("fallback: got %q, want 2", got)
	}
	// Empty value skipped.
	if got := firstJSONStringField(`{"a":""}`, "a", "b"); got != "" {
		t.Errorf("empty value: got %q, want empty", got)
	}
}

// TestNormalizeChatType covers the p2p/direct → direct normalization.
func TestNormalizeChatType(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"p2p", "direct"}, {"direct", "direct"}, {"P2P", "direct"},
		{" group ", "group"}, {"", "group"}, {"unknown", "group"},
	} {
		if got := normalizeChatType(c.in); got != c.want {
			t.Errorf("normalizeChatType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStringValue covers the nil-safe string dereference.
func TestStringValue(t *testing.T) {
	if got := stringValue(nil); got != "" {
		t.Errorf("stringValue(nil) = %q, want empty", got)
	}
	if got := stringValue(strPtr("  hi  ")); got != "hi" {
		t.Errorf("stringValue trims: got %q, want hi", got)
	}
}

// TestFeishuRunnerIDChannelSetters covers ID()/Channel()/SetHITLResolver/
// SetOwnerResolver on a constructed runner. Previously 0%.
func TestFeishuRunnerIDChannelSetters(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID:        "feishu-id-test",
		Channel:   "feishu",
		AppID:     "cli_x",
		AppSecret: "secret",
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if runner.ID() != "feishu-id-test" {
		t.Errorf("ID() = %q, want feishu-id-test", runner.ID())
	}
	if runner.Channel() != "feishu" {
		t.Errorf("Channel() = %q, want feishu", runner.Channel())
	}
	// SetHITLResolver nil-safe.
	runner.SetHITLResolver(nil)
	if runner.hitlResolver != nil {
		t.Error("SetHITLResolver(nil) should leave field nil")
	}
}

// TestFeishuCapabilities covers Capabilities() (previously 0%, single statement).
func TestFeishuCapabilities(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "feishu-cap", Channel: "feishu", AppID: "cli_x", AppSecret: "s",
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	caps := runner.Capabilities()
	hasCap := func(c channel.Capability) bool {
		for _, x := range caps {
			if x == c {
				return true
			}
		}
		return false
	}
	if !hasCap(channel.CapabilityProactiveOutbound) {
		t.Error("missing CapabilityProactiveOutbound")
	}
	if !hasCap(channel.CapabilityInteractiveHumanResponse) {
		t.Error("missing CapabilityInteractiveHumanResponse")
	}
}

// TestFeishuEmitErrorPaths covers Emit's validation branches (nil target,
// empty chat_id, empty content, unknown type). All should return errors.
// The happy path requires a real lark API call (covered by integration tests).
func TestFeishuEmitErrorPaths(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "feishu-emit", Channel: "feishu", AppID: "cli_x", AppSecret: "s",
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
	// empty content.
	if err := runner.Emit(ctx, channel.OutboundEnvelope{
		Target: &channel.InboundContext{ChatID: "c1"},
		Data:   map[string]any{"content": "  "},
	}); err == nil {
		t.Error("Emit with empty content should error")
	}
	// unknown type (with content).
	if err := runner.Emit(ctx, channel.OutboundEnvelope{
		Type:   channel.OutboundType("unknown"),
		Target: &channel.InboundContext{ChatID: "c1"},
		Data:   map[string]any{"content": "hi"},
	}); err == nil {
		t.Error("Emit with unknown type should error")
	}
}

// TestFeishuStartStop covers Start + Stop lifecycle. Start spawns a lark ws
// client goroutine (connection fails in background — fake app_id, logged not
// fatal). Stop cancels the context and clears state. Both functions were 0%.
func TestFeishuStartStop(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID:        "feishu-start",
		Channel:   "feishu",
		AppID:     "cli_test",
		AppSecret: "secret",
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runner.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Verify wsClient + cancel were set.
	runner.mu.Lock()
	hasClient := runner.wsClient != nil
	hasCancel := runner.cancel != nil
	runner.mu.Unlock()
	if !hasClient || !hasCancel {
		t.Fatal("Start should set wsClient and cancel")
	}
	// Stop must clear both + call cancel.
	if err := runner.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	runner.mu.Lock()
	clearedClient := runner.wsClient == nil
	clearedCancel := runner.cancel == nil
	runner.mu.Unlock()
	if !clearedClient || !clearedCancel {
		t.Error("Stop should clear wsClient and cancel")
	}
	// Stop without Start (cancel==nil) must not panic.
	runner2, err := NewRunner(entrypoints.Definition{
		ID: "feishu-nostart", Channel: "feishu", AppID: "cli_x", AppSecret: "s",
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if err := runner2.Stop(ctx); err != nil {
		t.Errorf("Stop without Start should be no-op, got: %v", err)
	}
}

// TestExtractContent covers all message type branches. Previously 41.7%.
func TestExtractContent(t *testing.T) {
	// empty content.
	if got := extractContent(larkim.MsgTypeText, ""); got != "" {
		t.Errorf("empty content: got %q, want empty", got)
	}
	// text type with valid JSON.
	if got := extractContent(larkim.MsgTypeText, `{"text":"hello"}`); got != "hello" {
		t.Errorf("text JSON: got %q, want hello", got)
	}
	// text type with invalid JSON → fallback to raw.
	if got := extractContent(larkim.MsgTypeText, "raw text"); got != "raw text" {
		t.Errorf("text raw fallback: got %q, want 'raw text'", got)
	}
	// image type.
	if got := extractContent(larkim.MsgTypeImage, `{}`); got != "[image]" {
		t.Errorf("image: got %q, want [image]", got)
	}
	// file type → firstJSONStringField.
	if got := extractContent(larkim.MsgTypeFile, `{"file_name":"doc.pdf"}`); got != "doc.pdf" {
		t.Errorf("file: got %q, want doc.pdf", got)
	}
	// audio type.
	if got := extractContent(larkim.MsgTypeAudio, `{}`); got != "[audio]" {
		t.Errorf("audio: got %q, want [audio]", got)
	}
	// media type.
	if got := extractContent(larkim.MsgTypeMedia, `{}`); got != "[video]" {
		t.Errorf("media: got %q, want [video]", got)
	}
	// unknown type → raw content.
	if got := extractContent("custom", "payload"); got != "payload" {
		t.Errorf("unknown: got %q, want payload", got)
	}
}

// TestFeishuSendError covers send/sendText/sendCard error paths by redirecting
// the lark client to a closed port (connection refused). Previously these
// functions were ~55% — the success branch needs real API; error branch is
// covered here.
func TestFeishuSendError(t *testing.T) {
	runner, err := NewRunner(entrypoints.Definition{
		ID: "feishu-send", Channel: "feishu", AppID: "cli_test", AppSecret: "s",
	}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	// Redirect to closed port → Create returns connection error.
	runner.client = lark.NewClient("cli_test", "s", lark.WithOpenBaseUrl("http://127.0.0.1:9"))
	ctx := context.Background()
	// sendText path (via Emit happy path: type + content present, but API fails).
	err = runner.Emit(ctx, channel.OutboundEnvelope{
		Type:   channel.OutboundAssistantFinal,
		Target: &channel.InboundContext{ChatID: "c1"},
		Data:   map[string]any{"content": "hello"},
	})
	if err == nil {
		t.Error("Emit to closed port should return error")
	}
	// sendCard path (unknown envelope type routes to... actually unknown errors
	// before send. Test sendCard directly via proactive_message with markdown).
	err = runner.Emit(ctx, channel.OutboundEnvelope{
		Type:   channel.OutboundProactiveMessage,
		Target: &channel.InboundContext{ChatID: "c1"},
		Data:   map[string]any{"content": "**markdown**"},
	})
	if err == nil {
		t.Error("Emit proactive to closed port should return error")
	}
}
