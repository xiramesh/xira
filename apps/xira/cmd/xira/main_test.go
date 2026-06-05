package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-daming/xira/internal/agents"
	"github.com/ai-daming/xira/internal/model/deepseek"
	"github.com/ai-daming/xira/internal/runtime"
)

func TestAgentListUsesWorkspaceAgents(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "agent", "list")

	var profiles []agents.Profile
	if err := json.Unmarshal([]byte(out), &profiles); err != nil {
		t.Fatalf("decode agent list: %v\n%s", err, out)
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles len = %d", len(profiles))
	}
	if profiles[0].ID != "xira-assistant" || profiles[1].ID != "research-assistant" {
		t.Fatalf("profiles = %+v", profiles)
	}
}

func TestAgentRunUsesRuntimeDefaultAgent(t *testing.T) {
	instance := writeCLIFixture(t, "research-assistant")
	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "agent", "run", "--message", "hi")

	var resp runtime.TurnResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode agent run: %v\n%s", err, out)
	}
	if resp.AgentID != "research-assistant" {
		t.Fatalf("AgentID = %q", resp.AgentID)
	}
}

func TestAgentRunUsesExplicitWorkspaceAgent(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "agent", "run", "--agent", "research-assistant", "--message", "please call command")

	var resp runtime.TurnResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode agent run: %v\n%s", err, out)
	}
	if resp.AgentID != "research-assistant" {
		t.Fatalf("AgentID = %q", resp.AgentID)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "command.run" {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
}

func TestNoPerChannelFeishuCommand(t *testing.T) {
	cmd := newRootCommand()
	for _, sub := range cmd.Commands() {
		if sub.Name() == "feishu" {
			t.Fatal("xira feishu command should not exist; channel runners are owned by xira serve")
		}
	}
}

func TestNoEmbeddedTUICommand(t *testing.T) {
	cmd := newRootCommand()
	for _, sub := range cmd.Commands() {
		if sub.Name() == "tui" {
			t.Fatal("xira tui command should not exist; XiraGarden owns the GUI channel")
		}
	}
}

func executeCommand(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newRootCommandWithFactory(func(cfg runtime.Config) (*runtime.Service, error) {
		cfg.DeepSeekClient = fakeCLIDeepSeekClient(t)
		return runtime.NewService(cfg)
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(%v) error = %v\n%s", args, err, stdout.String())
	}
	return stdout.String()
}

func fakeCLIDeepSeekClient(t *testing.T) *deepseek.Client {
	t.Helper()
	client := &http.Client{Transport: cliRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		var body string
		if hasCLIToolResponse(req.Messages) {
			body = cliDeepSeekTextResponse("fake cli tool final")
		} else {
			userMessage := lastCLIUserMessage(req.Messages)
			if strings.Contains(strings.ToLower(userMessage), "command") {
				body = cliDeepSeekToolCallResponse()
			} else {
				body = cliDeepSeekTextResponse("fake cli response")
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

type cliRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn cliRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func hasCLIToolResponse(messages []deepseek.Message) bool {
	for _, message := range messages {
		if message.Role == "tool" {
			return true
		}
	}
	return false
}

func lastCLIUserMessage(messages []deepseek.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return deepseek.ContentText(messages[i].Content)
		}
	}
	return ""
}

func cliDeepSeekTextResponse(text string) string {
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

func cliDeepSeekToolCallResponse() string {
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
						"name":      "command_run",
						"arguments": `{"program":"printf","args":["hello from Xira command"]}`,
					},
				}},
			},
		}},
	})
	return string(data)
}

func writeCLIFixture(t *testing.T, defaultAgentID string) string {
	t.Helper()
	instance := t.TempDir()
	writeCLIFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: `+defaultAgentID+`
run_root: .xira/runs
`)
	writeCLIFile(t, filepath.Join(instance, "workspace", "agents", "xira-assistant", "PROFILE.md"), `---
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
    - chat
    - sender
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Keep responses operational.
`)
	writeCLIFile(t, filepath.Join(instance, "workspace", "agents", "xira-assistant", "SOUL.md"), `# Soul

Plain and practical.`)
	writeCLIFile(t, filepath.Join(instance, "workspace", "agents", "research-assistant", "PROFILE.md"), `---
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
  - command.run
  - shell.run
  - tool_output.read
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
---
# Working Contract

Use local evidence before summaries.
`)
	writeCLIFile(t, filepath.Join(instance, "workspace", "agents", "research-assistant", "SOUL.md"), `# Soul

Careful and source-backed.`)
	return instance
}

func writeCLIFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}
