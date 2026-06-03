package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-daming/flowdeck/internal/agents"
	"github.com/ai-daming/flowdeck/internal/model/deepseek"
)

func TestRunAgentWritesHarnessStore(t *testing.T) {
	rt, err := NewService(Config{RunRoot: filepath.Join(t.TempDir(), "runs"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
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
	if resp.RouteMatchedBy != "default" {
		t.Fatalf("route matched by = %q, want default", resp.RouteMatchedBy)
	}
	runDir := rt.RunStore().RunDir(resp.RunID)
	for _, name := range []string{"run.yaml", "events.jsonl", "audit.jsonl", "tool_calls.jsonl", "verification.json"} {
		if _, err := os.Stat(filepath.Join(runDir, name)); err != nil {
			t.Fatalf("expected %s: %v", name, err)
		}
	}
}

func TestDefaultAgentRespondsInMockMode(t *testing.T) {
	rt, err := NewService(Config{RunRoot: filepath.Join(t.TempDir(), "runs"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
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
	if resp.FinalResponse != "Mock model response: hi" {
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

func TestStatusDoesNotExposeToolDiscovery(t *testing.T) {
	rt, err := NewService(Config{RunRoot: filepath.Join(t.TempDir(), "runs"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
	status := rt.Status()
	for _, key := range []string{"known_tool_present", "known_tool_path"} {
		if _, ok := status[key]; ok {
			t.Fatalf("status exposes tool discovery key %q: %+v", key, status)
		}
	}
	if _, ok := status["mock_model"]; !ok {
		t.Fatalf("status missing model mode: %+v", status)
	}
}

func TestRunAgentCanUseExecTool(t *testing.T) {
	rt, err := NewService(Config{RunRoot: filepath.Join(t.TempDir(), "runs"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
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
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	var gotReq deepseek.ChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":[{"type":"text","text":"adk runtime ok"}]}}]}`))
	}))
	defer server.Close()

	rt, err := NewService(Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest(server.URL), deepseek.WithAPIKey("test-key")),
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
	instance := writeRuntimeFixture(t, "flowdeck-assistant", []string{"chat", "sender"})
	rt, err := NewService(Config{ConfigPath: filepath.Join(instance, "flowdeck.yaml"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
	status := rt.Status()
	if status["profile_source"] != "workspace" {
		t.Fatalf("profile_source = %v", status["profile_source"])
	}
	if status["default_agent"] != "flowdeck-assistant" {
		t.Fatalf("default_agent = %v", status["default_agent"])
	}
	if status["agents"] != 2 {
		t.Fatalf("agents = %v", status["agents"])
	}
	if status["config_path"] != filepath.Join(instance, "flowdeck.yaml") {
		t.Fatalf("config_path = %v", status["config_path"])
	}
	if status["workspace"] != filepath.Join(instance, "workspace") {
		t.Fatalf("workspace = %v", status["workspace"])
	}
}

func TestConfigDefaultAgentRoutesDefaultRequest(t *testing.T) {
	instance := writeRuntimeFixture(t, "research-assistant", []string{"chat", "sender"})
	rt, err := NewService(Config{ConfigPath: filepath.Join(instance, "flowdeck.yaml"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "hi", Channel: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AgentID != "research-assistant" {
		t.Fatalf("AgentID = %q", resp.AgentID)
	}
	if resp.RouteMatchedBy != "default" {
		t.Fatalf("RouteMatchedBy = %q", resp.RouteMatchedBy)
	}
}

func TestExplicitAgentCanRunWorkspaceResearchAssistant(t *testing.T) {
	instance := writeRuntimeFixture(t, "flowdeck-assistant", []string{"chat", "sender"})
	rt, err := NewService(Config{ConfigPath: filepath.Join(instance, "flowdeck.yaml"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
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

func TestAgentProfileSessionDimensionsOverrideDefaultScope(t *testing.T) {
	instance := writeRuntimeFixture(t, "flowdeck-assistant", []string{"channel"})
	rt, err := NewService(Config{ConfigPath: filepath.Join(instance, "flowdeck.yaml"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
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

func writeRuntimeFixture(t *testing.T, defaultAgentID string, flowdeckSessionDimensions []string) string {
	t.Helper()
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "flowdeck.yaml"), `workspace: workspace
default_agent: `+defaultAgentID+`
run_root: .flowdeck/runs
routes: workspace/routes.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "routes.yaml"), `default_agent: `+defaultAgentID+`
routes:
  - match:
      channel: research
    agent: research-assistant
`)
	writeFile(t, filepath.Join(instance, "workspace", "agents", "flowdeck-assistant", "PROFILE.md"), `---
id: flowdeck-assistant
name: FlowDeck Assistant
version: 0.1.0
description: Default FlowDeck runtime assistant.
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
  stream: true
  temperature: 0.2
session:
  dimensions:
`+yamlStringList(flowdeckSessionDimensions, "    ")+`verification:
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

Use FlowDeck runtime context and keep responses operational.
`)
	writeFile(t, filepath.Join(instance, "workspace", "agents", "flowdeck-assistant", "SOUL.md"), `# Soul

Plain, direct, and practical.`)
	writeFile(t, filepath.Join(instance, "workspace", "agents", "research-assistant", "PROFILE.md"), `---
id: research-assistant
name: Research Assistant
version: 0.1.0
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
