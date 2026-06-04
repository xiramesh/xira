package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-daming/flowdeck/internal/agents"
	"github.com/ai-daming/flowdeck/internal/runtime"
)

func TestAgentListUsesWorkspaceAgents(t *testing.T) {
	instance := writeCLIFixture(t, "flowdeck-assistant")
	out := executeCommand(t, "--config", filepath.Join(instance, "flowdeck.yaml"), "--mock-model", "agent", "list")

	var profiles []agents.Profile
	if err := json.Unmarshal([]byte(out), &profiles); err != nil {
		t.Fatalf("decode agent list: %v\n%s", err, out)
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles len = %d", len(profiles))
	}
	if profiles[0].ID != "flowdeck-assistant" || profiles[1].ID != "research-assistant" {
		t.Fatalf("profiles = %+v", profiles)
	}
}

func TestAgentRunUsesRuntimeDefaultAgent(t *testing.T) {
	instance := writeCLIFixture(t, "research-assistant")
	out := executeCommand(t, "--config", filepath.Join(instance, "flowdeck.yaml"), "--mock-model", "agent", "run", "--message", "hi")

	var resp runtime.TurnResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode agent run: %v\n%s", err, out)
	}
	if resp.AgentID != "research-assistant" {
		t.Fatalf("AgentID = %q", resp.AgentID)
	}
}

func TestAgentRunUsesExplicitWorkspaceAgent(t *testing.T) {
	instance := writeCLIFixture(t, "flowdeck-assistant")
	out := executeCommand(t, "--config", filepath.Join(instance, "flowdeck.yaml"), "--mock-model", "agent", "run", "--agent", "research-assistant", "--message", "please call exec")

	var resp runtime.TurnResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode agent run: %v\n%s", err, out)
	}
	if resp.AgentID != "research-assistant" {
		t.Fatalf("AgentID = %q", resp.AgentID)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "exec" {
		t.Fatalf("ToolCalls = %+v", resp.ToolCalls)
	}
}

func TestNoPerChannelFeishuCommand(t *testing.T) {
	cmd := newRootCommand()
	for _, sub := range cmd.Commands() {
		if sub.Name() == "feishu" {
			t.Fatal("flowdeck feishu command should not exist; channel runners are owned by flowdeck serve")
		}
	}
}

func executeCommand(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newRootCommand()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(%v) error = %v\n%s", args, err, stdout.String())
	}
	return stdout.String()
}

func writeCLIFixture(t *testing.T, defaultAgentID string) string {
	t.Helper()
	instance := t.TempDir()
	writeCLIFile(t, filepath.Join(instance, "flowdeck.yaml"), `workspace: workspace
default_agent: `+defaultAgentID+`
run_root: .flowdeck/runs
routes: workspace/routes.yaml
`)
	writeCLIFile(t, filepath.Join(instance, "workspace", "routes.yaml"), `default_agent: `+defaultAgentID+`
routes: []
`)
	writeCLIFile(t, filepath.Join(instance, "workspace", "agents", "flowdeck-assistant", "PROFILE.md"), `---
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
    - chat
    - sender
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Keep responses operational.
`)
	writeCLIFile(t, filepath.Join(instance, "workspace", "agents", "flowdeck-assistant", "SOUL.md"), `# Soul

Plain and practical.`)
	writeCLIFile(t, filepath.Join(instance, "workspace", "agents", "research-assistant", "PROFILE.md"), `---
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
