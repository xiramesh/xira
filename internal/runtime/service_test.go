package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adksession "google.golang.org/adk/session"

	"github.com/ai-daming/xira/internal/agents"
	"github.com/ai-daming/xira/internal/model/deepseek"
	fsession "github.com/ai-daming/xira/internal/session"
)

func TestRunAgentWritesHarnessStore(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "hello",
		Channel: "test",
		UserID:  "user-1",
		Metadata: map[string]string{
			"chat_id":   "chat-1",
			"chat_type": "group",
		},
	})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if resp.Status != "completed" {
		t.Fatalf("status = %q", resp.Status)
	}
	if resp.AgentID != agents.DefaultAgentID {
		t.Fatalf("agent id = %q, want default agent %q", resp.AgentID, agents.DefaultAgentID)
	}
	if resp.SessionScope == nil {
		t.Fatal("session scope is nil")
	}
	if resp.SessionScope.Values["chat"] != "group:chat-1" {
		t.Fatalf("chat scope = %q, want group:chat-1", resp.SessionScope.Values["chat"])
	}
	if resp.RouteMatchedBy != "entrypoint.implicit" {
		t.Fatalf("route matched by = %q, want entrypoint.implicit", resp.RouteMatchedBy)
	}
	runDir := rt.RunStore().RunDir(resp.RunID)
	for _, name := range []string{"run.yaml", "events.jsonl", "audit.jsonl", "tool_calls.jsonl", "verification.json"} {
		if _, err := os.Stat(filepath.Join(runDir, name)); err != nil {
			t.Fatalf("expected %s: %v", name, err)
		}
	}
}

func TestDefaultAgentRespondsWithDeepSeekAdapter(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "hi", Channel: "test"})
	if err != nil {
		t.Fatalf("run default agent: %v", err)
	}
	if resp.Status != "completed" {
		t.Fatalf("status = %q", resp.Status)
	}
	if resp.AgentID != agents.DefaultAgentID {
		t.Fatalf("agent id = %q, want %q", resp.AgentID, agents.DefaultAgentID)
	}
	if resp.SessionID == "" {
		t.Fatal("session id is empty")
	}
	if resp.SessionScope == nil {
		t.Fatal("session scope is nil")
	}
	if resp.FinalResponse != "fake model response: hi" {
		t.Fatalf("final response = %q", resp.FinalResponse)
	}
	history := rt.SessionManager().History(resp.SessionID)
	if len(history) != 2 {
		t.Fatalf("session history len = %d, want 2", len(history))
	}
	if history[0].Role != "user" || history[0].Content != "hi" || history[1].Role != "assistant" {
		t.Fatalf("session history = %+v", history)
	}
}

func TestRunAgentPersistsSessionFilesAndReloadsHistory(t *testing.T) {
	stateRoot := t.TempDir()
	sessionRoot := filepath.Join(stateRoot, "sessions")
	rt := newTestService(t, Config{RunRoot: filepath.Join(stateRoot, "runs")})
	if got := rt.SessionManager().Root(); got != sessionRoot {
		t.Fatalf("session root = %q, want %q", got, sessionRoot)
	}
	if got := rt.Status()["session_root"]; got != sessionRoot {
		t.Fatalf("status session_root = %v, want %q", got, sessionRoot)
	}
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "persist me",
		Channel: "feishu",
		UserID:  "sender-1",
		Metadata: map[string]string{
			"chat_id":   "chat-1",
			"chat_type": "group",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if history := rt.SessionManager().AgentHistory(resp.SessionID, resp.AgentID); len(history) != 2 {
		t.Fatalf("agent history len = %d, want 2: %+v", len(history), history)
	}
	channelDir := filepath.Join(sessionRoot, "feishu")
	entrypointDir := filepath.Join(channelDir, resp.EntrypointID)
	entries, err := os.ReadDir(entrypointDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("session entries = %+v, want one conversation dir", entries)
	}
	if !strings.Contains(entries[0].Name(), "chat_group_chat-1") || !strings.Contains(entries[0].Name(), "sender_sender-1") {
		t.Fatalf("conversation dir = %q, want readable chat and sender labels", entries[0].Name())
	}
	messagesPath := filepath.Join(entrypointDir, entries[0].Name(), "agents", resp.AgentID, "messages.jsonl")
	if _, err := os.Stat(messagesPath); err != nil {
		t.Fatalf("expected persisted messages: %v", err)
	}

	reloaded := newTestService(t, Config{RunRoot: filepath.Join(stateRoot, "runs")})
	history := reloaded.SessionManager().History(resp.SessionID)
	if len(history) != 2 {
		t.Fatalf("reloaded history len = %d, want 2: %+v", len(history), history)
	}
	if history[0].Content != "persist me" || history[1].Content != "fake model response: persist me" {
		t.Fatalf("reloaded history = %+v", history)
	}
}

func TestHydrateADKSessionRestoresPersistedAgentHistory(t *testing.T) {
	stateRoot := t.TempDir()
	rt := newTestService(t, Config{RunRoot: filepath.Join(stateRoot, "runs")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "remember this",
		Channel: "test",
		UserID:  "user-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	reloaded := newTestService(t, Config{RunRoot: filepath.Join(stateRoot, "runs")})
	agentSessionID := fsession.BuildAgentSessionID(resp.SessionID, resp.AgentID)
	if err := reloaded.hydrateADKSession(context.Background(), "user-1", agentSessionID, resp.AgentID, resp.SessionID); err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.adkSessions.Get(context.Background(), &adksession.GetRequest{
		AppName:   "xira",
		UserID:    "user-1",
		SessionID: agentSessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := got.Session.Events()
	if events.Len() != 2 {
		t.Fatalf("restored event len = %d, want 2", events.Len())
	}
	if first := events.At(0); first.Author != "user" || contentText(first.Content) != "remember this" {
		t.Fatalf("first restored event = %+v", first)
	}
	if second := events.At(1); second.Author != resp.AgentID || contentText(second.Content) != "fake model response: remember this" {
		t.Fatalf("second restored event = %+v", second)
	}
}

func TestStatusDoesNotExposeToolDiscoveryInternals(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	status := rt.Status()
	for _, key := range []string{"known_tool_present", "known_tool_path"} {
		if _, ok := status[key]; ok {
			t.Fatalf("status exposes internal key %q: %+v", key, status)
		}
	}
}

func TestToolLogSummariesAvoidLargeContent(t *testing.T) {
	input := toolInputSummary(map[string]any{
		"path":    "kb/yangsheng-yihao/index.md",
		"content": "secret body",
	})
	if input["content"] != nil {
		t.Fatalf("input summary leaked content: %+v", input)
	}
	if input["content_chars"] != 11 {
		t.Fatalf("input content chars = %v", input["content_chars"])
	}

	output := toolOutputSummary(map[string]any{
		"path":    "/workspace/kb/yangsheng-yihao/index.md",
		"content": "knowledge body",
		"entries": []map[string]any{{"name": "a"}, {"name": "b"}},
	})
	if output["content"] != nil {
		t.Fatalf("output summary leaked content: %+v", output)
	}
	if output["content_chars"] != 14 || output["entries_count"] != 2 {
		t.Fatalf("output summary = %+v", output)
	}
}

func TestRunAgentCanUseExecTool(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		AgentID: agents.ResearchAssistantAgentID,
		Message: "please call exec",
		Channel: "test",
	})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "exec" {
		t.Fatalf("expected exec tool call: %+v", resp.ToolCalls)
	}
}

func TestRunAgentADKResponseRecordsContentStats(t *testing.T) {
	var gotReq deepseek.ChatRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"model":"deepseek-v4-flash","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"text","text":"adk runtime ok"}]}}]}`)),
		}, nil
	})}

	rt, err := NewService(Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "hi", Channel: "test"})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if resp.FinalResponse != "adk runtime ok" {
		t.Fatalf("final response = %q", resp.FinalResponse)
	}
	if gotReq.Thinking == nil || gotReq.Thinking.Type != "disabled" {
		t.Fatalf("thinking = %+v, want disabled", gotReq.Thinking)
	}
	if len(gotReq.Messages) < 2 {
		t.Fatalf("messages = %+v, want system and user messages", gotReq.Messages)
	}
	systemInstruction, ok := gotReq.Messages[0].Content.(string)
	if !ok || gotReq.Messages[0].Role != "system" || !strings.Contains(systemInstruction, "You are Xira's default runtime assistant.") {
		t.Fatalf("system instruction message = %+v", gotReq.Messages[0])
	}
	if !strings.Contains(systemInstruction, "Current Xira agent: xira-assistant (Xira Assistant).") {
		t.Fatalf("system instruction missing runtime identity: %q", systemInstruction)
	}
	if gotReq.Messages[1].Role != "user" || gotReq.Messages[1].Content != "hi" {
		t.Fatalf("user message = %+v", gotReq.Messages[1])
	}
	var found bool
	for _, event := range resp.Events {
		if event.Kind != "adk.event" {
			continue
		}
		found = true
		if event.Payload["content_chars"] == nil || event.Payload["parts"] == nil || event.Payload["finish_reason"] != "stop" {
			t.Fatalf("adk event payload = %+v, want content diagnostics", event.Payload)
		}
	}
	if !found {
		t.Fatalf("events = %+v, want adk.event", resp.Events)
	}
}

func TestNewServiceLoadsWorkspaceAgentsFromConfig(t *testing.T) {
	instance := writeRuntimeFixture(t, "xira-assistant", []string{"chat", "sender"})
	rt := newTestService(t, Config{ConfigPath: filepath.Join(instance, "xira.yaml")})
	status := rt.Status()
	if status["profile_source"] != "workspace" {
		t.Fatalf("profile_source = %v", status["profile_source"])
	}
	if status["default_agent"] != "xira-assistant" {
		t.Fatalf("default_agent = %v", status["default_agent"])
	}
	if status["agents"] != 2 {
		t.Fatalf("agents = %v", status["agents"])
	}
	if status["config_path"] != filepath.Join(instance, "xira.yaml") {
		t.Fatalf("config_path = %v", status["config_path"])
	}
	if status["workspace"] != filepath.Join(instance, "workspace") {
		t.Fatalf("workspace = %v", status["workspace"])
	}
}

func TestConfigDefaultAgentRoutesDefaultRequest(t *testing.T) {
	instance := writeRuntimeFixture(t, "research-assistant", []string{"chat", "sender"})
	rt := newTestService(t, Config{ConfigPath: filepath.Join(instance, "xira.yaml")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "hi", Channel: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AgentID != "research-assistant" {
		t.Fatalf("AgentID = %q", resp.AgentID)
	}
	if resp.RouteMatchedBy != "entrypoint.implicit" {
		t.Fatalf("RouteMatchedBy = %q", resp.RouteMatchedBy)
	}
}

func TestExplicitAgentCanRunWorkspaceResearchAssistant(t *testing.T) {
	instance := writeRuntimeFixture(t, "xira-assistant", []string{"chat", "sender"})
	rt := newTestService(t, Config{ConfigPath: filepath.Join(instance, "xira.yaml")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		AgentID: "research-assistant",
		Message: "please call exec",
		Channel: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AgentID != "research-assistant" {
		t.Fatalf("AgentID = %q", resp.AgentID)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "exec" {
		t.Fatalf("expected exec tool call: %+v", resp.ToolCalls)
	}
}

func TestExplicitAgentSharesConversationSessionWithDefaultAgent(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	metadata := map[string]string{
		"account":   "tenant-a",
		"chat_id":   "chat-1",
		"chat_type": "group",
	}
	first, err := rt.RunAgent(context.Background(), TurnRequest{
		Message:  "hello",
		Channel:  "feishu",
		UserID:   "sender-1",
		Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := rt.RunAgent(context.Background(), TurnRequest{
		AgentID:  agents.ResearchAssistantAgentID,
		Message:  "research this",
		Channel:  "feishu",
		UserID:   "sender-1",
		Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}

	if first.AgentID != agents.DefaultAgentID {
		t.Fatalf("default AgentID = %q", first.AgentID)
	}
	if second.AgentID != agents.ResearchAssistantAgentID {
		t.Fatalf("explicit AgentID = %q", second.AgentID)
	}
	if second.RouteMatchedBy != "request.agent_id" {
		t.Fatalf("RouteMatchedBy = %q, want request.agent_id", second.RouteMatchedBy)
	}
	if first.SessionID != second.SessionID {
		t.Fatalf("conversation session changed across agents: %q != %q", first.SessionID, second.SessionID)
	}
	history := rt.SessionManager().History(first.SessionID)
	if len(history) != 4 {
		t.Fatalf("conversation history len = %d, want 4", len(history))
	}
}

func TestFeishuEntrypointsSplitConversationByBotInstance(t *testing.T) {
	instance := writeRuntimeFixtureWithEntrypoints(t)
	rt := newTestService(t, Config{ConfigPath: filepath.Join(instance, "xira.yaml")})
	base := map[string]string{
		"account":   "tenant-a",
		"chat_id":   "chat-1",
		"chat_type": "group",
	}
	expenseMetadata := cloneStringMap(base)
	expenseMetadata["app_id"] = "cli-expense"
	expenseMetadata["bot_id"] = "bot-expense"
	leaveMetadata := cloneStringMap(base)
	leaveMetadata["app_id"] = "cli-leave"
	leaveMetadata["bot_id"] = "bot-leave"

	expense, err := rt.RunAgent(context.Background(), TurnRequest{
		Message:  "expense",
		Channel:  "feishu",
		UserID:   "sender-1",
		Metadata: expenseMetadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	leave, err := rt.RunAgent(context.Background(), TurnRequest{
		Message:  "leave",
		Channel:  "feishu",
		UserID:   "sender-1",
		Metadata: leaveMetadata,
	})
	if err != nil {
		t.Fatal(err)
	}

	if expense.EntrypointID != "feishu-expense-bot" {
		t.Fatalf("expense entrypoint = %q", expense.EntrypointID)
	}
	if leave.EntrypointID != "feishu-leave-bot" {
		t.Fatalf("leave entrypoint = %q", leave.EntrypointID)
	}
	if expense.SessionID == leave.SessionID {
		t.Fatalf("conversation session should differ across Feishu bots: %q", expense.SessionID)
	}
	if expense.SessionScope == nil || expense.SessionScope.EntrypointID != "feishu-expense-bot" {
		t.Fatalf("expense scope = %+v", expense.SessionScope)
	}
}

func TestAgentProfileSessionDimensionsOverrideDefaultScope(t *testing.T) {
	instance := writeRuntimeFixture(t, "xira-assistant", []string{"channel"})
	rt := newTestService(t, Config{ConfigPath: filepath.Join(instance, "xira.yaml")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "hello",
		Channel: "tui",
		UserID:  "user-1",
		Metadata: map[string]string{
			"chat_id":   "chat-1",
			"chat_type": "group",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.SessionScope == nil {
		t.Fatal("session scope is nil")
	}
	if got := resp.SessionScope.Values["channel"]; got != "channel:tui" {
		t.Fatalf("channel scope = %q", got)
	}
	if _, ok := resp.SessionScope.Values["sender"]; ok {
		t.Fatalf("sender scope should not be present: %+v", resp.SessionScope.Values)
	}
	if _, ok := resp.SessionScope.Values["chat"]; ok {
		t.Fatalf("chat scope should not be present: %+v", resp.SessionScope.Values)
	}
}

func TestVerificationFailureCreatesEvolutionCandidate(t *testing.T) {
	engine := NewEvolutionEngine()
	candidate := engine.CandidateForFailure("run-1", "test", VerificationResult{Status: "failed", Errors: []string{"x"}}, nil, testTime())
	if candidate == nil {
		t.Fatal("expected candidate")
	}
	if candidate.FailureLayer != "Verification" {
		t.Fatalf("failure layer = %q", candidate.FailureLayer)
	}
}

func TestNewServiceRequiresDeepSeekAPIKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	if _, err := NewService(Config{RunRoot: filepath.Join(t.TempDir(), "runs")}); err == nil || !strings.Contains(err.Error(), "DEEPSEEK_API_KEY is required") {
		t.Fatalf("NewService() error = %v, want DEEPSEEK_API_KEY requirement", err)
	}
}

func newTestService(t *testing.T, cfg Config) *Service {
	t.Helper()
	if cfg.DeepSeekClient == nil {
		cfg.DeepSeekClient = fakeDeepSeekClient(t)
	}
	rt, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func fakeDeepSeekClient(t *testing.T) *deepseek.Client {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		var body string
		if hasToolResponse(req.Messages) {
			body = deepSeekTextResponse("fake tool final")
		} else {
			userMessage := lastUserMessage(req.Messages)
			if strings.Contains(strings.ToLower(userMessage), "exec") {
				body = deepSeekToolCallResponse()
			} else {
				body = deepSeekTextResponse("fake model response: " + userMessage)
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	return deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func hasToolResponse(messages []deepseek.Message) bool {
	for _, message := range messages {
		if message.Role == "tool" {
			return true
		}
	}
	return false
}

func lastUserMessage(messages []deepseek.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return deepseek.ContentText(messages[i].Content)
		}
	}
	return ""
}

func deepSeekTextResponse(text string) string {
	data, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4-flash",
		"choices": []map[string]any{{
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
		}},
	})
	return string(data)
}

func deepSeekToolCallResponse() string {
	data, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4-flash",
		"choices": []map[string]any{{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":   "call-1",
					"type": "function",
					"function": map[string]any{
						"name":      "exec",
						"arguments": `{"action":"run","command":"printf 'hello from Xira exec'"}`,
					},
				}},
			},
		}},
	})
	return string(data)
}

func writeRuntimeFixture(t *testing.T, defaultAgentID string, xiraSessionDimensions []string) string {
	t.Helper()
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: `+defaultAgentID+`
run_root: .xira/runs
routes: workspace/routes.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "routes.yaml"), `default_agent: `+defaultAgentID+`
routes: []
`)
	writeFile(t, filepath.Join(instance, "workspace", "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: Default Xira runtime assistant.
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
  stream: true
  temperature: 0.2
session:
  dimensions:
`+yamlStringList(xiraSessionDimensions, "    ")+`verification:
  default_checks:
    - final_response_non_empty
artifacts:
  output_dir: artifacts
  retention: local
evolution:
  enabled: true
  candidate_only: true
---
# Working Contract

Use Xira runtime context and keep responses operational.
`)
	writeFile(t, filepath.Join(instance, "workspace", "agents", "xira-assistant", "SOUL.md"), `# Soul

Plain, direct, and practical.`)
	writeFile(t, filepath.Join(instance, "workspace", "agents", "research-assistant", "PROFILE.md"), `---
id: research-assistant
name: Research Assistant
version: 0.1.1
description: Evidence-first research assistant.
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
  stream: true
  temperature: 0.2
tools:
  - exec
  - read_file
  - write_file
  - list_dir
  - edit_file
session:
  dimensions:
    - chat
    - sender
verification:
  default_checks:
    - final_response_non_empty
artifacts:
  output_dir: artifacts
  retention: local
evolution:
  enabled: true
  candidate_only: true
---
# Working Contract

Use local evidence before summaries.
`)
	writeFile(t, filepath.Join(instance, "workspace", "agents", "research-assistant", "SOUL.md"), `# Soul

Careful and source-backed.`)
	return instance
}

func writeRuntimeFixtureWithEntrypoints(t *testing.T) string {
	t.Helper()
	instance := writeRuntimeFixture(t, "xira-assistant", []string{"chat", "sender"})
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
run_root: .xira/runs
routes: workspace/routes.yaml
entrypoints: workspace/entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: feishu-expense-bot
    channel: feishu
    app_id: cli-expense
    bot_id: bot-expense
    default_agent: xira-assistant
    allowed_agents:
      - xira-assistant
      - research-assistant
    session:
      dimensions:
        - chat
        - sender
  - id: feishu-leave-bot
    channel: feishu
    app_id: cli-leave
    bot_id: bot-leave
    default_agent: xira-assistant
    allowed_agents:
      - xira-assistant
      - research-assistant
    session:
      dimensions:
        - chat
        - sender
  - id: ilink-wechat
    channel: ilink
    default_agent: xira-assistant
    allowed_agents:
      - xira-assistant
      - research-assistant
    session:
      dimensions:
        - chat
        - sender
`)
	return instance
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

func yamlStringList(values []string, indent string) string {
	var b strings.Builder
	for _, value := range values {
		b.WriteString(indent)
		b.WriteString("- ")
		b.WriteString(value)
		b.WriteString("\n")
	}
	return b.String()
}
