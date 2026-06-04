package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ai-daming/xira/internal/agents"
	"github.com/ai-daming/xira/internal/channelcontrol"
	"github.com/ai-daming/xira/internal/model/deepseek"
	frt "github.com/ai-daming/xira/internal/runtime"
)

func TestAgentRunAPI(t *testing.T) {
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(frt.TurnRequest{Message: "hello", Channel: "test"})
	resp, err := http.Post(server.URL()+"/api/v1/agent-runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var run frt.TurnResponse
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if run.RunID == "" || run.FinalResponse == "" {
		t.Fatalf("bad run response: %+v", run)
	}
}

func TestXiraGardenMessageChannelAPI(t *testing.T) {
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(frt.TurnRequest{Message: "hello garden"})
	resp, err := http.Post(server.URL()+"/api/v1/channels/xiragarden/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var run frt.TurnResponse
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if run.SessionScope == nil || run.SessionScope.Channel != "xiragarden" {
		t.Fatalf("session scope = %+v", run.SessionScope)
	}
	if run.EntrypointID != "xiragarden-default" {
		t.Fatalf("entrypoint = %q", run.EntrypointID)
	}
	if run.RouteMatchedBy != "entrypoint.implicit" {
		t.Fatalf("route matched by = %q", run.RouteMatchedBy)
	}
}

func TestXiraGardenMessageRejectsMismatchedChannel(t *testing.T) {
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(frt.TurnRequest{Message: "hello", Channel: "feishu"})
	resp, err := http.Post(server.URL()+"/api/v1/channels/xiragarden/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestShellTurnAPIIsNotAChannelEntrypoint(t *testing.T) {
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(frt.TurnRequest{Message: "hi", Channel: "test"})
	resp, err := http.Post(server.URL()+"/api/v1/shell-turns", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAgentsAPIUsesWorkspaceDiscoveredAgents(t *testing.T) {
	workspace := writeAPIWorkspace(t)
	rt, err := frt.NewService(frt.Config{
		WorkspaceRoot:  workspace,
		DefaultAgentID: "xira-assistant",
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: fakeAPIDeepSeekClient(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(server.URL() + "/api/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var profiles []agents.Profile
	if err := json.NewDecoder(resp.Body).Decode(&profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles len = %d", len(profiles))
	}
	if profiles[0].ID != "xira-assistant" || profiles[1].ID != "research-assistant" {
		t.Fatalf("profiles = %+v", profiles)
	}
}

func TestEventsWebSocketReceivesRunEvents(t *testing.T) {
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	wsURL := "ws" + server.URL()[4:] + "/api/v1/events"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	body, _ := json.Marshal(frt.TurnRequest{Message: "hello", Channel: "test"})
	resp, err := http.Post(server.URL()+"/api/v1/agent-runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	var evt frt.RuntimeEvent
	if err := conn.ReadJSON(&evt); err != nil {
		t.Fatal(err)
	}
	if evt.Kind != "run.started" {
		t.Fatalf("event kind = %q", evt.Kind)
	}
}

func TestXiraGardenEventsWebSocketReceivesChannelEvents(t *testing.T) {
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	wsURL := "ws" + server.URL()[4:] + "/api/v1/channels/xiragarden/events"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	body, _ := json.Marshal(frt.TurnRequest{Message: "hello garden"})
	resp, err := http.Post(server.URL()+"/api/v1/channels/xiragarden/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	var evt frt.RuntimeEvent
	if err := conn.ReadJSON(&evt); err != nil {
		t.Fatal(err)
	}
	if evt.Kind != "run.started" {
		t.Fatalf("event kind = %q", evt.Kind)
	}
	if got := evt.Payload["channel"]; got != "xiragarden" {
		t.Fatalf("event channel = %q", got)
	}
}

func TestEntrypointPairingAPIUsesChannelControls(t *testing.T) {
	controls := &fakeChannelControls{
		pairing: channelcontrol.PairingSnapshot{
			PairingID:      "pair-1",
			EntrypointID:   "ilink-wechat",
			Status:         channelcontrol.PairingStatusWait,
			QRCode:         "qr-key",
			QRImageContent: "https://liteapp.weixin.qq.com/q/qr-key",
		},
		accounts: []channelcontrol.AccountSnapshot{{
			AccountID:    "bot-1",
			EntrypointID: "ilink-wechat",
			UserID:       "owner-1",
			Running:      true,
		}},
	}
	server := NewServer(nil, "127.0.0.1:0", controls)

	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/entrypoints/ilink-wechat/pairings", nil)
	createResp := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", createResp.Code, createResp.Body.String())
	}
	var pairing channelcontrol.PairingSnapshot
	if err := json.NewDecoder(createResp.Body).Decode(&pairing); err != nil {
		t.Fatal(err)
	}
	if pairing.PairingID != "pair-1" || controls.createEntrypoint != "ilink-wechat" {
		t.Fatalf("pairing=%+v createEntrypoint=%q", pairing, controls.createEntrypoint)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/entrypoints/ilink-wechat/accounts", nil)
	listResp := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d body=%s", listResp.Code, listResp.Body.String())
	}
	var body struct {
		Accounts []channelcontrol.AccountSnapshot `json:"accounts"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].AccountID != "bot-1" {
		t.Fatalf("accounts = %+v", body.Accounts)
	}
}

func newAPITestService(t *testing.T, cfg frt.Config) *frt.Service {
	t.Helper()
	if cfg.DeepSeekClient == nil {
		cfg.DeepSeekClient = fakeAPIDeepSeekClient(t)
	}
	rt, err := frt.NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func fakeAPIDeepSeekClient(t *testing.T) *deepseek.Client {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4-flash",
		"choices": []map[string]any{{
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": "fake api response",
			},
		}},
	})
	client := &http.Client{Transport: apiRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(body))),
		}, nil
	})}
	return deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client))
}

type apiRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn apiRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type fakeChannelControls struct {
	pairing          channelcontrol.PairingSnapshot
	accounts         []channelcontrol.AccountSnapshot
	createEntrypoint string
}

func (f *fakeChannelControls) CreatePairing(_ context.Context, entrypointID string) (channelcontrol.PairingSnapshot, error) {
	f.createEntrypoint = entrypointID
	return f.pairing, nil
}

func (f *fakeChannelControls) GetPairing(entrypointID, pairingID string) (channelcontrol.PairingSnapshot, error) {
	f.pairing.EntrypointID = entrypointID
	f.pairing.PairingID = pairingID
	return f.pairing, nil
}

func (f *fakeChannelControls) ListAccounts(string) ([]channelcontrol.AccountSnapshot, error) {
	return f.accounts, nil
}

func (f *fakeChannelControls) DeleteAccount(context.Context, string, string) error {
	return nil
}

func writeAPIWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	writeAPIFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
---
Default contract.
`)
	writeAPIFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), `Default soul.`)
	writeAPIFile(t, filepath.Join(workspace, "agents", "research-assistant", "PROFILE.md"), `---
id: research-assistant
name: Research Assistant
version: 0.1.1
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
tools:
  - exec
  - read_file
  - write_file
  - list_dir
  - edit_file
---
Research contract.
`)
	writeAPIFile(t, filepath.Join(workspace, "agents", "research-assistant", "SOUL.md"), `Research soul.`)
	return workspace
}

func writeAPIFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}
