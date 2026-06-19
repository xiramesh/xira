package ilink

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	openilink "github.com/openilink/openilink-sdk-go"

	"github.com/xiramesh/xira/internal/entrypoints"
	"github.com/xiramesh/xira/internal/model/deepseek"
	frt "github.com/xiramesh/xira/internal/runtime"
)

// recordingClient records the contents delivered via SendText/Push in order.
type recordingClient struct {
	mu      sync.Mutex
	sent    []string
	token   string
	baseURL string
}

func (c *recordingClient) Monitor(context.Context, openilink.MessageHandler, *openilink.MonitorOptions) error {
	return nil
}
func (c *recordingClient) SendText(_ context.Context, _, content, _ string) (string, error) {
	c.mu.Lock()
	c.sent = append(c.sent, content)
	c.mu.Unlock()
	return "client-id", nil
}
func (c *recordingClient) Push(_ context.Context, _, content string) (string, error) {
	c.mu.Lock()
	c.sent = append(c.sent, content)
	c.mu.Unlock()
	return "client-id", nil
}
func (c *recordingClient) Token() string    { return c.token }
func (c *recordingClient) BaseURL() string  { return c.baseURL }
func (c *recordingClient) contents() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.sent...)
}

type ilinkRoundTripFunc func(*http.Request) (*http.Response, error)

func (f ilinkRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func dsText(text string) string {
	b, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4-flash",
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
	rt, err := frt.NewService(frt.Config{StateDir: filepath.Join(t.TempDir(), "state"), DeepSeekClient: ds})
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
		MessageID:  42,
		MessageType: openilink.MsgTypeUser,
		FromUserID: "wxid-user",
		SessionID:  "session-1",
		ContextToken: "ctx-token",
		ItemList:   []openilink.MessageItem{{Type: openilink.ItemText, TextItem: &openilink.TextItem{Text: text}}},
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
