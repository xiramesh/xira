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

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/channelcontrol"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
	frt "github.com/xiramesh/xira/internal/runtime"
)

func TestAgentRunAPI(t *testing.T) {
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(frt.TurnRequest{Message: "hello", Context: channel.NewInboundContext("test", "", nil)})
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
	body, _ := json.Marshal(frt.TurnRequest{Message: "hello", Context: channel.NewInboundContext("feishu", "", nil)})
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
	body, _ := json.Marshal(frt.TurnRequest{Message: "hi", Context: channel.NewInboundContext("test", "", nil)})
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
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	conn, _, err := websocket.Dial(readCtx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	body, _ := json.Marshal(frt.TurnRequest{Message: "hello", Context: channel.NewInboundContext("test", "", nil)})
	resp, err := http.Post(server.URL()+"/api/v1/agent-runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	var evt frt.RuntimeEvent
	if err := wsjson.Read(readCtx, conn, &evt); err != nil {
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
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	conn, _, err := websocket.Dial(readCtx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()

	body, _ := json.Marshal(frt.TurnRequest{Message: "hello garden"})
	resp, err := http.Post(server.URL()+"/api/v1/channels/xiragarden/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	var evt frt.RuntimeEvent
	if err := wsjson.Read(readCtx, conn, &evt); err != nil {
		t.Fatal(err)
	}
	if evt.Kind != "run.started" {
		t.Fatalf("event kind = %q", evt.Kind)
	}
	if got := evt.Payload["channel"]; got != "xiragarden" {
		t.Fatalf("event channel = %q", got)
	}
}

func TestChannelEventFilterKeepsChildRunCompatibility(t *testing.T) {
	runIDs := map[string]struct{}{"parent-run": {}}
	child := frt.RuntimeEvent{
		RunID:  "child-run",
		Kind:   "agent.delegate.started",
		Source: "runtime",
		Payload: map[string]any{
			"channel":       "xiragarden",
			"entrypoint_id": "xiragarden-default",
			"parent_run_id": "parent-run",
			"child_run_id":  "child-run",
		},
		Correlation: &frt.RuntimeEventCorrelation{
			ParentRunID: "parent-run",
			ChildRunID:  "child-run",
		},
	}
	if !eventBelongsToChannel(child, "xiragarden", runIDs) {
		t.Fatalf("child event should belong to channel")
	}
	if _, ok := runIDs["child-run"]; !ok {
		t.Fatalf("child run id was not remembered: %+v", runIDs)
	}

	scoped := frt.RuntimeEvent{
		RunID:  "scoped-child-run",
		Kind:   "agent.delegate.completed",
		Source: "runtime",
		Scope:  &frt.RuntimeEventScope{Channel: "xiragarden"},
		Payload: map[string]any{
			"channel": "evil-channel",
		},
	}
	if !eventBelongsToChannel(scoped, "xiragarden", runIDs) {
		t.Fatalf("scoped child event should belong to channel")
	}
	if eventBelongsToChannel(scoped, "evil-channel", map[string]struct{}{}) {
		t.Fatalf("payload channel should not override scoped channel")
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

func TestPostHumanRequestResponseApprove(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	req := seedAPIHumanRequest(t, rt, humanrequest.CreateRequest{
		ID:       "hrq_api_approve",
		Kind:     humanrequest.RequestApproval,
		Question: "Approve API test?",
	})
	server := NewServer(rt, "127.0.0.1:0")

	resp := serveJSON(t, server, http.MethodPost, "/api/v1/human-requests/"+req.ID+"/responses", map[string]any{
		"kind":  "approve",
		"actor": "test-user",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	var body humanrequest.HumanRequest
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ID != req.ID || body.Status != humanrequest.StatusResolved || body.Response == nil || body.Response.Kind != humanrequest.ResponseApprove {
		t.Fatalf("response body = %+v", body)
	}
}

func TestPostHumanRequestResponseAnswer(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	req := seedAPIHumanRequest(t, rt, humanrequest.CreateRequest{
		ID:       "hrq_api_answer",
		Kind:     humanrequest.RequestFreeform,
		Question: "What next?",
	})
	server := NewServer(rt, "127.0.0.1:0")

	resp := serveJSON(t, server, http.MethodPost, "/api/v1/human-requests/"+req.ID+"/responses", map[string]any{
		"kind":    "answer",
		"actor":   "test-user",
		"message": "Use the conservative option.",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	var body humanrequest.HumanRequest
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Response == nil || body.Response.Message != "Use the conservative option." {
		t.Fatalf("response body = %+v", body)
	}
}

func TestPostHumanRequestResponseRejectsInvalidKind(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	req := seedAPIHumanRequest(t, rt, humanrequest.CreateRequest{
		ID:       "hrq_api_invalid",
		Kind:     humanrequest.RequestFreeform,
		Question: "What next?",
	})
	server := NewServer(rt, "127.0.0.1:0")

	resp := serveJSON(t, server, http.MethodPost, "/api/v1/human-requests/"+req.ID+"/responses", map[string]any{
		"kind":  "maybe",
		"actor": "test-user",
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	stored, err := rt.GetHumanRequest(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != humanrequest.StatusPending {
		t.Fatalf("stored request changed after invalid kind: %+v", stored)
	}
}

func TestPostHumanRequestResponseConflictOnResolved(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	req := seedAPIHumanRequest(t, rt, humanrequest.CreateRequest{
		ID:       "hrq_api_conflict",
		Kind:     humanrequest.RequestFreeform,
		Question: "Answer once?",
	})
	server := NewServer(rt, "127.0.0.1:0")
	first := serveJSON(t, server, http.MethodPost, "/api/v1/human-requests/"+req.ID+"/responses", map[string]any{
		"kind":    "answer",
		"actor":   "test-user",
		"message": "first",
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d body=%s", first.Code, first.Body.String())
	}

	second := serveJSON(t, server, http.MethodPost, "/api/v1/human-requests/"+req.ID+"/responses", map[string]any{
		"kind":    "deny",
		"actor":   "test-user",
		"message": "second",
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("second status = %d body=%s", second.Code, second.Body.String())
	}
}

func TestPostHumanRequestResponseMissingRequest(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	server := NewServer(rt, "127.0.0.1:0")

	resp := serveJSON(t, server, http.MethodPost, "/api/v1/human-requests/missing/responses", map[string]any{
		"kind":    "answer",
		"actor":   "test-user",
		"message": "hello",
	})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestPostHumanRequestResponseWrongWorkspaceDoesNotLeak(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	req := seedAPIHumanRequest(t, rt, humanrequest.CreateRequest{
		ID:       "hrq_api_workspace",
		Kind:     humanrequest.RequestFreeform,
		Question: "Answer in current workspace?",
	})
	server := NewServer(rt, "127.0.0.1:0")

	resp := serveJSON(t, server, http.MethodPost, "/api/v1/human-requests/"+req.ID+"/responses", map[string]any{
		"kind":          "answer",
		"actor":         "test-user",
		"message":       "hello",
		"workspace_key": "ws_attacker",
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	stored, err := rt.GetHumanRequest(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != humanrequest.StatusPending {
		t.Fatalf("workspace override resolved request: %+v", stored)
	}
}

func TestPostHumanRequestResponseTriggersResumeHookButDoesNotRequireReplayYet(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	req := seedAPIHumanRequest(t, rt, humanrequest.CreateRequest{
		ID:       "hrq_api_hook",
		Kind:     humanrequest.RequestFreeform,
		Question: "Trigger hook?",
	})
	var resumed []string
	rt.SetHumanRequestResumeHook(func(_ context.Context, resolved humanrequest.HumanRequest) error {
		resumed = append(resumed, resolved.ID)
		return nil
	})
	server := NewServer(rt, "127.0.0.1:0")

	resp := serveJSON(t, server, http.MethodPost, "/api/v1/human-requests/"+req.ID+"/responses", map[string]any{
		"kind":    "answer",
		"actor":   "test-user",
		"message": "resume",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	if len(resumed) != 1 || resumed[0] != req.ID {
		t.Fatalf("resume hook calls = %+v", resumed)
	}
}

func TestListHumanRequests(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	old := seedAPIHumanRequest(t, rt, humanrequest.CreateRequest{ID: "hrq_api_list_old", Kind: humanrequest.RequestFreeform, Question: "old?"})
	newer := seedAPIHumanRequest(t, rt, humanrequest.CreateRequest{ID: "hrq_api_list_new", Kind: humanrequest.RequestFreeform, Question: "new?"})
	if _, err := rt.ResolveHumanRequest(context.Background(), old.ID, humanrequest.ResolveRequest{Kind: humanrequest.ResponseAnswer, Actor: "tester", Message: "done"}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(rt, "127.0.0.1:0")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/human-requests?status=pending", nil)
	resp := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	var list []humanrequest.HumanRequest
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != newer.ID || list[0].Status != humanrequest.StatusPending {
		t.Fatalf("list = %+v", list)
	}
}

func TestShowHumanRequest(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	created := seedAPIHumanRequest(t, rt, humanrequest.CreateRequest{ID: "hrq_api_show", Kind: humanrequest.RequestFreeform, Question: "show?"})
	server := NewServer(rt, "127.0.0.1:0")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/human-requests/"+created.ID, nil)
	resp := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	var shown humanrequest.HumanRequest
	if err := json.NewDecoder(resp.Body).Decode(&shown); err != nil {
		t.Fatal(err)
	}
	if shown.ID != created.ID || shown.Question != "show?" {
		t.Fatalf("shown = %+v", shown)
	}
}

func TestShowHumanRequestMissing(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	server := NewServer(rt, "127.0.0.1:0")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/human-requests/missing", nil)
	resp := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
}

func TestListHumanRequestsRejectsInvalidStatus(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateRoot: filepath.Join(t.TempDir(), "state")})
	server := NewServer(rt, "127.0.0.1:0")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/human-requests?status=maybe", nil)
	resp := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
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

func seedAPIHumanRequest(t *testing.T, rt *frt.Service, input humanrequest.CreateRequest) *humanrequest.HumanRequest {
	t.Helper()
	if input.RunID == "" {
		input.RunID = "run-api"
	}
	if input.AgentID == "" {
		input.AgentID = agents.DefaultAgentID
	}
	if input.SessionID == "" {
		input.SessionID = "session-api"
	}
	if input.WorkspaceID == "" {
		input.WorkspaceID = rt.Status()["workspace"].(string)
	}
	if input.WorkspaceKey == "" {
		input.WorkspaceKey = rt.WorkspaceKey()
	}
	req, err := rt.CreateHumanRequest(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func serveJSON(t *testing.T, server *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(resp, req)
	return resp
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
  - command.run
  - shell.run
  - tool_output.read
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
