package ilink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openilink "github.com/openilink/openilink-sdk-go"

	"github.com/xiramesh/xira/internal/entrypoints"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
	frt "github.com/xiramesh/xira/internal/runtime"
)

// recordingClient records the contents delivered via SendText/Push in order.
type recordingClient struct {
	mu      sync.Mutex
	sent    []string
	methods []string
	token   string
	baseURL string
	sendErr error
}

type recordingTextHITLResolver struct {
	input humanrequest.TextResponseEnvelope
	err   error
	calls int
}

func (r *recordingTextHITLResolver) ResolveHumanTextResponse(_ context.Context, input humanrequest.TextResponseEnvelope) (*humanrequest.HumanRequest, error) {
	r.calls++
	r.input = input
	if r.err != nil {
		return nil, r.err
	}
	return &humanrequest.HumanRequest{ID: "hrq-resolved"}, nil
}

func (c *recordingClient) Monitor(context.Context, openilink.MessageHandler, *openilink.MonitorOptions) error {
	return nil
}
func (c *recordingClient) SendText(_ context.Context, _, content, _ string) (string, error) {
	c.mu.Lock()
	c.sent = append(c.sent, content)
	c.methods = append(c.methods, "send_text")
	err := c.sendErr
	c.mu.Unlock()
	if err != nil {
		return "", err
	}
	return "client-id", nil
}
func (c *recordingClient) Push(_ context.Context, _, content string) (string, error) {
	c.mu.Lock()
	c.sent = append(c.sent, content)
	c.methods = append(c.methods, "push")
	c.mu.Unlock()
	return "client-id", nil
}
func (c *recordingClient) deliveryMethods() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.methods...)
}
func (c *recordingClient) Token() string   { return c.token }
func (c *recordingClient) BaseURL() string { return c.baseURL }
func (c *recordingClient) contents() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

type ilinkRoundTripFunc func(*http.Request) (*http.Response, error)

func (f ilinkRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func dsText(text string) string {
	b, _ := json.Marshal(map[string]any{
		"model":   "deepseek-v4-flash",
		"choices": []map[string]any{{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}},
	})
	return string(b)
}

// newProgressTestRunner wires a real runtime (fake DeepSeek over HTTP) into an
// iLink runner with a recording channel client, plus a registered account.
func newProgressTestRunner(t *testing.T, respond func(*http.Request) string) (*Runner, *accountPoller, *recordingClient) {
	t.Helper()
	client := &http.Client{Transport: ilinkRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(respond(r))),
		}, nil
	})}
	ds := deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client))
	cfg := frt.Config{StateDir: filepath.Join(t.TempDir(), "state"), DeepSeekClient: ds}
	manager, err := frt.NewSessionManager(cfg)
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	cfg.SessionManager = manager
	rt, err := frt.NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	runner, err := NewRunner(entrypoints.Definition{ID: "", Channel: "ilink", AllowRuntimePairing: true}, rt, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	rec := &recordingClient{token: "t", baseURL: "http://ilink.test"}
	account := &accountPoller{
		record:   accountRecord{AccountID: "acct-1", UserID: "bot-1"},
		client:   rec,
		messages: newMessageDeduper(time.Minute),
		stateDir: t.TempDir(),
	}
	runner.accounts["acct-1"] = account
	return runner, account, rec
}

func userTextMsg(text string) openilink.WeixinMessage {
	return openilink.WeixinMessage{
		MessageID:    42,
		MessageType:  openilink.MsgTypeUser,
		FromUserID:   "wxid-user",
		SessionID:    "session-1",
		ContextToken: "ctx-token",
		ItemList:     []openilink.MessageItem{{Type: openilink.ItemText, TextItem: &openilink.TextItem{Text: text}}},
	}
}

// TestRunnerForwardsFinalAndDropsRawEvents: a completed run's final answer is
// delivered to the channel, and NO raw runtime events (adk.event, tool.*,
// model.*) leak into the chat despite the progress forwarder being wired
// around RunAgent (docs §13, §16.3).
func TestRunnerForwardsFinalAndDropsRawEvents(t *testing.T) {
	runner, account, rec := newProgressTestRunner(t, func(*http.Request) string {
		return dsText("iLink final answer")
	})
	runner.handleMessage(account, userTextMsg("hello"))

	contents := rec.contents()
	if len(contents) != 1 || contents[0] != "iLink final answer" {
		t.Fatalf("expected only the final answer delivered, got %v", contents)
	}
}

func TestRunnerReleasesDedupeAfterRuntimeFailure(t *testing.T) {
	runner, account, _ := newProgressTestRunner(t, func(*http.Request) string {
		return `not json`
	})
	msg := userTextMsg("hello")
	runner.handleMessage(account, msg)
	awaitDedupeReleased(t, account, account.messageDedupeKey(messageID(msg)))
}

func TestRunnerReleasesDedupeAfterFinalSendFailure(t *testing.T) {
	runner, account, rec := newProgressTestRunner(t, func(*http.Request) string {
		return dsText("iLink final answer")
	})
	rec.sendErr = errors.New("ilink unavailable")
	msg := userTextMsg("hello")
	runner.handleMessage(account, msg)
	awaitDedupeReleased(t, account, account.messageDedupeKey(messageID(msg)))
}

func TestRunnerConsumesExplicitHumanResponseWithoutAgentTurn(t *testing.T) {
	var modelCalls atomic.Int32
	runner, account, rec := newProgressTestRunner(t, func(*http.Request) string {
		modelCalls.Add(1)
		return dsText("must not run")
	})
	resolver := &recordingTextHITLResolver{}
	runner.SetTextHITLResolver(resolver)
	msg := userTextMsg("/answer HR-550E8400E29B41D4A716446655440000 approve")
	runner.handleMessage(account, msg)

	if modelCalls.Load() != 0 {
		t.Fatalf("explicit response started %d agent turns", modelCalls.Load())
	}
	if resolver.calls != 1 || resolver.input.CorrelationToken != "550e8400-e29b-41d4-a716-446655440000" || resolver.input.Answer != "approve" {
		t.Fatalf("resolver input = %+v, calls = %d", resolver.input, resolver.calls)
	}
	if resolver.input.EntrypointID != runner.definition.ID || resolver.input.SenderID != "wxid-user" || resolver.input.SenderIDType != "ilink_user_id" || resolver.input.IdempotencyKey == "" {
		t.Fatalf("authoritative response envelope = %+v", resolver.input)
	}
	if got := rec.contents(); len(got) != 1 || got[0] != "已收到回答。" {
		t.Fatalf("response acknowledgement = %v", got)
	}
}

func TestRunnerConsumesMalformedAndRejectedHumanResponsesWithoutAgentTurn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    string
		resolveErr error
		wantText   string
		wantCalls  int
	}{
		{name: "malformed", content: "/answer short yes", wantText: "回答格式无效", wantCalls: 0},
		{name: "rejected", content: "/answer HR-550E8400E29B41D4A716446655440000 yes", resolveErr: errors.New("wrong sender"), wantText: "无法接受该回答", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var modelCalls atomic.Int32
			runner, account, rec := newProgressTestRunner(t, func(*http.Request) string {
				modelCalls.Add(1)
				return dsText("must not run")
			})
			resolver := &recordingTextHITLResolver{err: tc.resolveErr}
			runner.SetTextHITLResolver(resolver)
			runner.handleMessage(account, userTextMsg(tc.content))
			if modelCalls.Load() != 0 || resolver.calls != tc.wantCalls {
				t.Fatalf("model calls = %d, resolver calls = %d", modelCalls.Load(), resolver.calls)
			}
			got := rec.contents()
			if len(got) != 1 || !strings.Contains(got[0], tc.wantText) {
				t.Fatalf("safe response = %v", got)
			}
		})
	}
}

func TestRunnerExplicitHumanResponseHandlesMissingResolverAndAckFailure(t *testing.T) {
	const answer = "/answer HR-550E8400E29B41D4A716446655440000 yes"

	t.Run("missing resolver stays protocol traffic", func(t *testing.T) {
		runner, account, rec := newProgressTestRunner(t, func(*http.Request) string { return dsText("must not run") })
		runner.handleMessage(account, userTextMsg(answer))
		got := rec.contents()
		if len(got) != 1 || !strings.Contains(got[0], "无法接受该回答") {
			t.Fatalf("missing resolver response = %v", got)
		}
	})

	t.Run("committed answer completes dedupe before ack", func(t *testing.T) {
		runner, account, rec := newProgressTestRunner(t, func(*http.Request) string { return dsText("must not run") })
		runner.SetTextHITLResolver(&recordingTextHITLResolver{})
		rec.sendErr = errors.New("ilink unavailable")
		msg := userTextMsg(answer)
		runner.handleMessage(account, msg)
		key := account.messageDedupeKey(messageID(msg))
		if account.messages.Begin(key, time.Now()) {
			t.Fatalf("committed answer dedupe key %q was released after acknowledgement failure", key)
		}
	})

	t.Run("rejected answer releases dedupe when error ack fails", func(t *testing.T) {
		runner, account, rec := newProgressTestRunner(t, func(*http.Request) string { return dsText("must not run") })
		runner.SetTextHITLResolver(&recordingTextHITLResolver{err: errors.New("unauthorized")})
		rec.sendErr = errors.New("ilink unavailable")
		msg := userTextMsg(answer)
		runner.handleMessage(account, msg)
		key := account.messageDedupeKey(messageID(msg))
		if !account.messages.Begin(key, time.Now()) {
			t.Fatalf("uncommitted answer dedupe key %q was not released", key)
		}
	})
}

func awaitDedupeReleased(t *testing.T, account *accountPoller, key string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if account.messages.Begin(key, time.Now()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("dedupe key %q was not released after failed turn", key)
}
