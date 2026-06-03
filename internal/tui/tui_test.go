package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	frt "github.com/ai-daming/flowdeck/internal/runtime"
)

func TestInitDoesNotSelectAgentByDefault(t *testing.T) {
	m := model{}
	updated, _ := m.Update(initMsg{
		status: map[string]any{},
		agents: []map[string]any{
			{"id": "flowdeck-assistant", "name": "FlowDeck Assistant"},
			{"id": "lead-research", "name": "Lead Research"},
			{"id": "research-assistant", "name": "Research Assistant"},
		},
	})
	got := updated.(model)

	if got.activeAgent != "" {
		t.Fatalf("activeAgent = %q, want empty default-agent mode", got.activeAgent)
	}
}

func TestPlainInputUsesDefaultAgentMode(t *testing.T) {
	m := model{
		agents: []map[string]any{
			{"id": "flowdeck-assistant", "name": "FlowDeck Assistant"},
			{"id": "research-assistant", "name": "Research Assistant"},
		},
	}
	m.input = textinput.New()
	m.input.SetValue("随便聊一句")

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(model)

	if cmd == nil {
		t.Fatal("plain input command is nil, want default agent command")
	}
	if got.err != nil {
		t.Fatalf("plain input err = %v, want nil", got.err)
	}
	if !got.loading {
		t.Fatal("loading = false, want true")
	}
	if got.loadingMode != "default" {
		t.Fatalf("loadingMode = %q, want default", got.loadingMode)
	}
	if got.output != "" {
		t.Fatalf("output = %q, want empty while default agent runs", got.output)
	}
}

func TestDefaultAgentResponseUpdatesTranscript(t *testing.T) {
	m := model{}
	updated, _ := m.Update(runMsg{resp: frt.TurnResponse{
		AgentID:       "flowdeck-assistant",
		FinalResponse: "你好",
		Status:        "completed",
	}})
	got := updated.(model)

	if got.output != "你好" {
		t.Fatalf("output = %q, want 你好", got.output)
	}
	if len(got.transcript) != 1 {
		t.Fatalf("transcript len = %d, want 1", len(got.transcript))
	}
	if got.transcript[0].Role != "flowdeck-assistant" || got.transcript[0].Content != "你好" {
		t.Fatalf("transcript = %+v", got.transcript)
	}
}

func TestApplyCommandSelectsAgent(t *testing.T) {
	m := model{
		agents: []map[string]any{
			{"id": "research-assistant", "name": "Research Assistant"},
			{"id": "lead-research", "name": "Lead Research"},
		},
		activeAgent: "research-assistant",
	}

	handled, cmd := m.applyCommand("/use lead-research")
	if !handled {
		t.Fatal("applyCommand did not handle /agent")
	}
	if cmd != nil {
		t.Fatal("applyCommand returned command, want nil")
	}
	if m.err != nil {
		t.Fatalf("applyCommand returned error: %v", m.err)
	}
	if m.activeAgent != "lead-research" {
		t.Fatalf("activeAgent = %q, want lead-research", m.activeAgent)
	}
	if !strings.Contains(m.output, "Mode: agent lead-research") {
		t.Fatalf("output = %q, want agent-mode confirmation", m.output)
	}
}

func TestApplyCommandRunsExplicitAgentWithMessage(t *testing.T) {
	m := model{
		agents: []map[string]any{
			{"id": "research-assistant", "name": "Research Assistant"},
		},
	}

	handled, cmd := m.applyCommand("/agent research-assistant search harness")
	if !handled {
		t.Fatal("applyCommand did not handle /agent")
	}
	if cmd == nil {
		t.Fatal("applyCommand command is nil, want agent run command")
	}
	if m.err != nil {
		t.Fatalf("applyCommand returned error: %v", m.err)
	}
	if !m.loading {
		t.Fatal("loading = false, want true")
	}
	if m.activeAgent != "" {
		t.Fatalf("activeAgent = %q, want one-shot agent call without mode switch", m.activeAgent)
	}
}

func TestApplyCommandRejectsUnknownAgent(t *testing.T) {
	m := model{
		agents: []map[string]any{
			{"id": "research-assistant", "name": "Research Assistant"},
		},
		activeAgent: "research-assistant",
	}

	handled, cmd := m.applyCommand("/agent missing-agent do work")
	if !handled {
		t.Fatal("applyCommand did not handle /agent")
	}
	if cmd != nil {
		t.Fatal("applyCommand returned command, want nil")
	}
	if m.err == nil {
		t.Fatal("applyCommand error is nil, want unknown-agent error")
	}
	if m.activeAgent != "research-assistant" {
		t.Fatalf("activeAgent = %q, want unchanged research-assistant", m.activeAgent)
	}
	if !strings.Contains(m.output, "Available agents:") {
		t.Fatalf("output = %q, want available agent list", m.output)
	}
}

func TestAgentListTextIncludesRuntimeProfileDetails(t *testing.T) {
	text := agentListText([]map[string]any{
		{
			"id":          "research-assistant",
			"name":        "Research Assistant",
			"description": "Local-first research assistant",
			"tools":       []string{"exec", "read_file"},
		},
	})

	for _, want := range []string{"Available agents:", "research-assistant", "Research Assistant", "Local-first research assistant", "tools: exec, read_file"} {
		if !strings.Contains(text, want) {
			t.Fatalf("agentListText() = %q, want substring %q", text, want)
		}
	}
}

func TestLoadUsesRuntimeDiscoveredAgents(t *testing.T) {
	workspace := writeTUIWorkspace(t)
	rt, err := frt.NewService(frt.Config{
		WorkspaceRoot:  workspace,
		DefaultAgentID: "custom-agent",
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		UseMockModel:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	msg := model{runtime: rt}.load()
	init, ok := msg.(initMsg)
	if !ok {
		t.Fatalf("load() = %T, want initMsg", msg)
	}
	if init.err != nil {
		t.Fatalf("init err = %v", init.err)
	}
	if len(init.agents) != 1 || init.agents[0]["id"] != "custom-agent" {
		t.Fatalf("agents = %+v", init.agents)
	}
}

func TestViewStartsAsDefaultAgent(t *testing.T) {
	input := textinput.New()
	input.Placeholder = "Talk to FlowDeck, or use /agent <id> <message>"
	m := model{
		input: input,
		status: map[string]any{
			"mock_model":    false,
			"default_agent": "flowdeck-assistant",
		},
		agents: []map[string]any{
			{"id": "flowdeck-assistant", "name": "FlowDeck Assistant"},
			{"id": "lead-research", "name": "Lead Research"},
		},
	}

	view := m.View()
	for _, want := range []string{"FlowDeck TUI", "Model: DeepSeek", "Mode: default agent flowdeck-assistant"} {
		if !strings.Contains(view, want) {
			t.Fatalf("View() = %q, want substring %q", view, want)
		}
	}
	for _, forbidden := range []string{"Mode: shell", "Ask research-assistant", "Entrypoint: agent run", "Selected agent:", "mock_model=", "known_tool=", "known_tool:"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("View() = %q, should not contain %q", view, forbidden)
		}
	}
}

func TestRunTraceShowsToolCallsAuditAndEvents(t *testing.T) {
	trace := renderRunTrace(&frt.TurnResponse{
		RunID:          "run-1",
		AgentID:        "flowdeck-assistant",
		RouteMatchedBy: "default",
		ToolCalls: []frt.ToolCallRecord{
			{
				Name:  "read_file",
				Input: map[string]any{"path": "kb/开门时间.txt"},
				Output: map[string]any{
					"path":    "/workspace/kb/开门时间.txt",
					"content": "工作日早上9点开门",
					"bytes":   25,
				},
			},
		},
		AuditEvents: []frt.AuditEvent{
			{Action: "tool.call", Target: "read_file", Allowed: true, Reason: "tool allowed by profile"},
		},
		Events: []frt.RuntimeEvent{
			{Kind: "run.started", Source: "runtime", Message: "agent run started"},
			{Kind: "tool.started", Source: "read_file", Message: "tool call started"},
			{Kind: "tool.finished", Source: "read_file", Message: "tool call finished"},
		},
	}, 120)

	for _, want := range []string{"Trace", "tools", "read_file", "path=\"kb/开门时间.txt\"", "content=<", "audit", "allow tool.call -> read_file", "events", "tool.finished read_file"} {
		if !strings.Contains(trace, want) {
			t.Fatalf("trace = %q, want substring %q", trace, want)
		}
	}
	if strings.Contains(trace, "工作日早上9点开门") {
		t.Fatalf("trace leaked full file content: %q", trace)
	}
}

func TestModelStatusLabel(t *testing.T) {
	if got := modelStatusLabel(map[string]any{"mock_model": true}); got != "mock" {
		t.Fatalf("modelStatusLabel(mock) = %q", got)
	}
	if got := modelStatusLabel(map[string]any{"mock_model": false}); got != "DeepSeek" {
		t.Fatalf("modelStatusLabel(deepseek) = %q", got)
	}
}

func writeTUIWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	path := filepath.Join(workspace, "agents", "custom-agent", "PROFILE.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`---
id: custom-agent
name: Custom Agent
version: 0.1.0
description: Runtime-discovered test agent.
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
tools:
  - exec
  - read_file
---
Custom contract.
`), 0o644); err != nil {
		t.Fatalf("WriteFile(PROFILE.md) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "agents", "custom-agent", "SOUL.md"), []byte("Custom soul."), 0o644); err != nil {
		t.Fatalf("WriteFile(SOUL.md) error = %v", err)
	}
	return workspace
}
