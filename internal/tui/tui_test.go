package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	frt "github.com/ai-daming/xira/internal/runtime"
)

func TestInitDoesNotSelectAgentByDefault(t *testing.T) {
	m := model{}
	updated, _ := m.Update(initMsg{
		status: map[string]any{},
		agents: []map[string]any{
			{"id": "xira-assistant", "name": "Xira Assistant"},
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
			{"id": "xira-assistant", "name": "Xira Assistant"},
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
	if got.input.Value() != "" {
		t.Fatalf("input value = %q, want cleared after submit", got.input.Value())
	}
}

func TestDefaultAgentResponseUpdatesTranscript(t *testing.T) {
	m := model{}
	updated, _ := m.Update(runMsg{resp: frt.TurnResponse{
		AgentID:       "xira-assistant",
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
	if got.transcript[0].Role != "xira-assistant" || got.transcript[0].Content != "你好" {
		t.Fatalf("transcript = %+v", got.transcript)
	}
}

func TestFailedAgentResponseStillAddsRunForTraceReview(t *testing.T) {
	m := model{loading: true, loadingMode: "default"}
	updated, _ := m.Update(runMsg{
		resp: frt.TurnResponse{
			RunID:   "run-failed",
			AgentID: "xira-assistant",
			Status:  "failed",
			Events: []frt.RuntimeEvent{
				{Kind: "run.started", Source: "runtime"},
				{Kind: "adk.empty_final", Source: "adk.runner", Message: "final ADK event contained no response text"},
			},
		},
		err: errors.New("ADK runner produced empty final response"),
	})
	got := updated.(model)

	if len(got.runs) != 1 || got.runs[0].RunID != "run-failed" {
		t.Fatalf("runs = %+v, want failed run retained", got.runs)
	}
	if !strings.Contains(renderRunTrace(lastRun(got), 120), "adk.empty_final") {
		t.Fatalf("trace = %q, want adk.empty_final", renderRunTrace(lastRun(got), 120))
	}
	if len(got.transcript) != 1 || got.transcript[0].Role != "Error" {
		t.Fatalf("transcript = %+v, want error transcript", got.transcript)
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
	if m.runningAgent != "research-assistant" {
		t.Fatalf("runningAgent = %q, want research-assistant", m.runningAgent)
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
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	rt, err := frt.NewService(frt.Config{
		WorkspaceRoot:  workspace,
		DefaultAgentID: "custom-agent",
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
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
	input.Placeholder = "Talk to Xira, or use /agent <id> <message>"
	m := model{
		input: input,
		status: map[string]any{
			"default_agent": "xira-assistant",
		},
		agents: []map[string]any{
			{"id": "xira-assistant", "name": "Xira Assistant"},
			{"id": "lead-research", "name": "Lead Research"},
		},
	}

	view := m.View()
	for _, want := range []string{"Xira TUI", "Model: DeepSeek", "Mode: default agent xira-assistant", "Current run", "idle"} {
		if !strings.Contains(view, want) {
			t.Fatalf("View() = %q, want substring %q", view, want)
		}
	}
	for _, forbidden := range []string{"Mode: shell", "Ask research-assistant", "Entrypoint: agent run", "Selected agent:", "known_tool=", "known_tool:", "No agents loaded", "No runs yet"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("View() = %q, should not contain %q", view, forbidden)
		}
	}
}

func TestConversationDoesNotShowTraceByDefaultAfterRun(t *testing.T) {
	input := textinput.New()
	m := model{
		input:  input,
		width:  120,
		height: 32,
		transcript: []transcriptEntry{
			{Role: "You", Content: "你好"},
			{Role: "xira-assistant", Content: "你好，有什么可以帮你？"},
		},
		runs: []frt.TurnResponse{{
			RunID:   "run-1",
			AgentID: "xira-assistant",
			Status:  "completed",
			Events: []frt.RuntimeEvent{
				{Kind: "run.started", Source: "runtime"},
				{Kind: "tool.started", Source: "read_file"},
				{Kind: "run.finished", Source: "runtime"},
			},
		}},
	}

	view := m.View()
	for _, want := range []string{"Conversation", "You", "xira-assistant", "Activity summary", "1 agent", "1 step", "read_file", "audit ref"} {
		if !strings.Contains(view, want) {
			t.Fatalf("View() = %q, want substring %q", view, want)
		}
	}
	for _, forbidden := range []string{"Trace", "tools: none", "tools 0  events 3", "run run-1", "No runs yet"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("View() = %q, should not contain default trace substring %q", view, forbidden)
		}
	}
}

func TestViewRendersConversationWithoutOuterFrame(t *testing.T) {
	input := textinput.New()
	m := model{
		input:  input,
		width:  100,
		height: 24,
		transcript: []transcriptEntry{
			{Role: "You", Content: "你好"},
			{Role: "xira-assistant", Content: "你好，有什么可以帮你？"},
		},
	}

	view := m.View()
	for _, forbidden := range []string{"│", "┌", "└"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("View() = %q, should not contain outer frame glyph %q", view, forbidden)
		}
	}
}

func TestViewRendersFullConversationHistory(t *testing.T) {
	input := textinput.New()
	m := model{
		input:  input,
		width:  90,
		height: 20,
		transcript: []transcriptEntry{
			{Role: "You", Content: "turn-01"},
			{Role: "xira-assistant", Content: "turn middle"},
			{Role: "You", Content: "turn latest"},
		},
	}

	view := m.View()
	for _, want := range []string{"turn-01", "turn middle", "turn latest"} {
		if !strings.Contains(view, want) {
			t.Fatalf("View() = %q, want full history substring %q", view, want)
		}
	}
}

func TestTraceCommandTogglesInspector(t *testing.T) {
	m := model{}
	handled, cmd := m.applyCommand("/trace")
	if !handled {
		t.Fatal("applyCommand did not handle /trace")
	}
	if cmd != nil {
		t.Fatal("applyCommand returned command, want nil")
	}
	if !m.showTrace {
		t.Fatal("showTrace = false, want true")
	}
	if m.output != "Trace inspector: on" {
		t.Fatalf("output = %q", m.output)
	}
}

func TestTraceInspectorShowsRunOnlyWhenEnabled(t *testing.T) {
	input := textinput.New()
	m := model{
		input:     input,
		width:     120,
		height:    32,
		showTrace: true,
		transcript: []transcriptEntry{
			{Role: "You", Content: "你好"},
			{Role: "xira-assistant", Content: "你好"},
		},
		runs: []frt.TurnResponse{{
			RunID:          "run-1",
			AgentID:        "xira-assistant",
			Status:         "completed",
			RouteMatchedBy: "default",
			Events: []frt.RuntimeEvent{
				{Kind: "run.started", Source: "runtime"},
				{Kind: "run.finished", Source: "runtime"},
			},
		}},
	}

	view := m.View()
	for _, want := range []string{"Trace", "run run-1", "events", "run.finished runtime"} {
		if !strings.Contains(view, want) {
			t.Fatalf("View() = %q, want trace substring %q", view, want)
		}
	}
}

func TestRunTraceShowsToolCallsAuditAndEvents(t *testing.T) {
	trace := renderRunTrace(&frt.TurnResponse{
		RunID:          "run-1",
		AgentID:        "xira-assistant",
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

func TestMarkdownRenderingHighlightsStructure(t *testing.T) {
	rendered := renderMarkdown(strings.Join([]string{
		"## Result",
		"",
		"- **first** item",
		"1. `second` item",
		"> important note",
		"",
		"```txt",
		"code line",
		"```",
	}, "\n"), 80, assistantTextStyle)

	for _, want := range []string{"Result", "- first item", "1. second item", "> important note", "txt", "code line"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("renderMarkdown() = %q, want substring %q", rendered, want)
		}
	}
}

func TestUpdateStreamsTraceEventsWhileLoading(t *testing.T) {
	events := make(chan frt.RuntimeEvent, 1)
	events <- frt.RuntimeEvent{
		Kind:    "tool.started",
		Source:  "read_file",
		Message: "tool call started",
		Payload: map[string]any{"path": "kb/开门时间.txt"},
	}
	m := model{loading: true, traceSub: events}

	msg := m.watchTrace()()
	updated, cmd := m.Update(msg)
	got := updated.(model)

	if len(got.traceEvents) != 1 {
		t.Fatalf("traceEvents len = %d, want 1", len(got.traceEvents))
	}
	if cmd == nil {
		t.Fatal("trace update command is nil, want next trace watch command")
	}
	trace := renderLiveTrace(got.traceEvents, 120)
	for _, want := range []string{"Trace", "live", "tool.started", "read_file", "path=\"kb/开门时间.txt\""} {
		if !strings.Contains(trace, want) {
			t.Fatalf("live trace = %q, want substring %q", trace, want)
		}
	}
}

func TestViewShowsVisibleRunningStatusNearInput(t *testing.T) {
	input := textinput.New()
	input.Placeholder = "Talk to Xira, or use /agent <id> <message>"
	m := model{
		input:       input,
		width:       110,
		height:      30,
		loading:     true,
		loadingMode: "default",
		transcript: []transcriptEntry{
			{Role: "You", Content: "明天几点开门"},
		},
	}

	view := m.View()
	for _, want := range []string{"RUNNING", "thinking - activity is streaming", "Activity live", "waiting for activity", "You"} {
		if !strings.Contains(view, want) {
			t.Fatalf("View() = %q, want substring %q", view, want)
		}
	}
	for _, forbidden := range []string{"Trace", "waiting for run.started", "Message"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("View() = %q, should not contain %q", view, forbidden)
		}
	}
}

func TestInlineLiveActivitySummarizesSteps(t *testing.T) {
	m := model{
		loading:      true,
		loadingMode:  "default",
		runningAgent: "xira-assistant",
		traceEvents: []frt.RuntimeEvent{
			{Kind: "run.started", Source: "runtime", Payload: map[string]any{"agent_id": "xira-assistant"}},
			{Kind: "tool.started", Source: "exec", Payload: map[string]any{"command": "pwd && ls -la"}},
			{Kind: "tool.finished", Source: "exec", Payload: map[string]any{"tool": "exec"}},
			{Kind: "adk.event", Source: "adk.runner", Payload: map[string]any{"content_chars": "0", "event_id": "event-1"}},
			{Kind: "adk.event", Source: "adk.runner", Payload: map[string]any{"content_chars": "20", "event_id": "event-2", "finish_reason": "tool_calls"}},
			{Kind: "adk.event", Source: "adk.runner", Payload: map[string]any{"content_chars": "0", "invocation_id": "invoke-1"}},
		},
	}

	activity := renderInlineLiveActivity(m, 120)
	for _, want := range []string{"Activity live", "2 steps", "xira-assistant", "exec", "pwd && ls -la", "model", "finish reason: tool_calls"} {
		if !strings.Contains(activity, want) {
			t.Fatalf("activity = %q, want substring %q", activity, want)
		}
	}
	for _, forbidden := range []string{"run.started", "tool.started", "tool.finished", "adk.event", "event_id", "invocation_id"} {
		if strings.Contains(activity, forbidden) {
			t.Fatalf("activity = %q, should not contain raw event substring %q", activity, forbidden)
		}
	}
}

func TestComposerLabelUsesUserPerspective(t *testing.T) {
	if got := composerLabel(model{}); got != "You" {
		t.Fatalf("composerLabel(default) = %q, want You", got)
	}
	if got := composerLabel(model{activeAgent: "research-assistant"}); got != "You -> research-assistant" {
		t.Fatalf("composerLabel(agent) = %q, want You -> research-assistant", got)
	}
}

func TestModelStatusLabel(t *testing.T) {
	if got := modelStatusLabel(map[string]any{}); got != "DeepSeek" {
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
version: 0.1.1
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
