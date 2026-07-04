package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
	"github.com/xiramesh/xira/internal/runtime"
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

func TestVersionCommandPrintsBuildMetadata(t *testing.T) {
	out := executeCommand(t, "version")
	if out != "xira 0.4.0 commit=unknown date=unknown\n" {
		t.Fatalf("version output = %q", out)
	}
}

func TestVersionFlagPrintsBuildMetadata(t *testing.T) {
	out := executeCommand(t, "--version")
	if out != "xira 0.4.0 commit=unknown date=unknown\n" {
		t.Fatalf("--version output = %q", out)
	}
}

func TestAgentRunPrintsFinalResponseByDefault(t *testing.T) {
	instance := writeCLIFixture(t, "research-assistant")
	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "agent", "run", "--message", "hi")

	if out != "fake cli response\n" {
		t.Fatalf("agent run output = %q", out)
	}
	if json.Valid([]byte(out)) {
		t.Fatalf("agent run default output should not be JSON: %s", out)
	}
}

func TestAgentRunJSONOutputUsesRuntimeDefaultAgent(t *testing.T) {
	instance := writeCLIFixture(t, "research-assistant")
	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "agent", "run", "--message", "hi", "--output", "json")

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
	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "agent", "run", "--agent", "research-assistant", "--message", "please call command", "--json")

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
			t.Fatal("xira tui command should not exist; runtime UI surfaces are external clients")
		}
	}
}

func TestHumanListCommandPrintsPendingRequests(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	rt := newCLITestRuntime(t, instance)
	pending := seedCLIHumanRequest(t, rt, "hrq_cli_list_pending", humanrequest.RequestFreeform)
	resolved := seedCLIHumanRequest(t, rt, "hrq_cli_list_resolved", humanrequest.RequestFreeform)
	if _, err := rt.ResolveHumanRequest(context.Background(), resolved.ID, humanrequest.ResolveRequest{Kind: humanrequest.ResponseAnswer, Actor: "tester", Message: "done"}); err != nil {
		t.Fatal(err)
	}

	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "human", "list", "--status", "pending")
	var list []humanrequest.HumanRequest
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("decode human list: %v\n%s", err, out)
	}
	if len(list) != 1 || list[0].ID != pending.ID {
		t.Fatalf("human list = %+v", list)
	}
}

func TestHumanShowCommandPrintsRequestDetail(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	rt := newCLITestRuntime(t, instance)
	req := seedCLIHumanRequest(t, rt, "hrq_cli_show", humanrequest.RequestFreeform)

	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "human", "show", req.ID)
	var shown humanrequest.HumanRequest
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("decode human show: %v\n%s", err, out)
	}
	if shown.ID != req.ID || shown.Question != "CLI question?" {
		t.Fatalf("shown = %+v", shown)
	}
}

func TestHumanAnswerCommandResolvesRequest(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	rt := newCLITestRuntime(t, instance)
	req := seedCLIHumanRequest(t, rt, "hrq_cli_answer", humanrequest.RequestFreeform)

	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "human", "answer", req.ID, "--message", "CLI answer")
	var resolved humanrequest.HumanRequest
	if err := json.Unmarshal([]byte(out), &resolved); err != nil {
		t.Fatalf("decode human answer: %v\n%s", err, out)
	}
	if resolved.Status != humanrequest.StatusResolved || resolved.Response == nil || resolved.Response.Kind != humanrequest.ResponseAnswer || resolved.Response.Message != "CLI answer" {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestHumanApproveDenyCancelCommandsResolveRequests(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	rt := newCLITestRuntime(t, instance)
	approve := seedCLIHumanRequest(t, rt, "hrq_cli_approve", humanrequest.RequestApproval)
	deny := seedCLIHumanRequest(t, rt, "hrq_cli_deny", humanrequest.RequestApproval)
	cancel := seedCLIHumanRequest(t, rt, "hrq_cli_cancel", humanrequest.RequestApproval)

	for _, tc := range []struct {
		cmd  string
		id   string
		kind humanrequest.ResponseKind
	}{
		{cmd: "approve", id: approve.ID, kind: humanrequest.ResponseApprove},
		{cmd: "deny", id: deny.ID, kind: humanrequest.ResponseDeny},
		{cmd: "cancel", id: cancel.ID, kind: humanrequest.ResponseCancel},
	} {
		out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "human", tc.cmd, tc.id, "--message", string(tc.kind))
		var resolved humanrequest.HumanRequest
		if err := json.Unmarshal([]byte(out), &resolved); err != nil {
			t.Fatalf("decode %s: %v\n%s", tc.cmd, err, out)
		}
		if resolved.Response == nil || resolved.Response.Kind != tc.kind {
			t.Fatalf("%s resolved = %+v", tc.cmd, resolved)
		}
	}
}

func TestHumanApproveCommandPostsResponse(t *testing.T) {
	assertHumanCommandPostsResponse(t, "approve", humanrequest.ResponseApprove)
}

func TestHumanDenyCommandPostsResponse(t *testing.T) {
	assertHumanCommandPostsResponse(t, "deny", humanrequest.ResponseDeny)
}

func TestHumanCancelCommandPostsResponse(t *testing.T) {
	assertHumanCommandPostsResponse(t, "cancel", humanrequest.ResponseCancel)
}

func TestHumanCommandsReturnNonZeroOnAPIError(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	out, err := executeCommandError("--config", filepath.Join(instance, "xira.yaml"), "human", "approve", "missing-human-request")
	if err == nil {
		t.Fatalf("approve missing request succeeded:\n%s", out)
	}
	if !strings.Contains(out, "not found") && !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unexpected error = %v output=%s", err, out)
	}
}

func TestHumanAnswerCommandRequiresMessage(t *testing.T) {
	instance := writeCLIFixture(t, "xira-assistant")
	rt := newCLITestRuntime(t, instance)
	req := seedCLIHumanRequest(t, rt, "hrq_cli_no_message", humanrequest.RequestFreeform)

	out, err := executeCommandError("--config", filepath.Join(instance, "xira.yaml"), "human", "answer", req.ID)
	if err == nil {
		t.Fatalf("answer without message succeeded:\n%s", out)
	}
	if !strings.Contains(out, "required flag") && !strings.Contains(err.Error(), "required flag") {
		t.Fatalf("unexpected error = %v output=%s", err, out)
	}
}

func assertHumanCommandPostsResponse(t *testing.T, cmdName string, kind humanrequest.ResponseKind) {
	t.Helper()
	instance := writeCLIFixture(t, "xira-assistant")
	rt := newCLITestRuntime(t, instance)
	req := seedCLIHumanRequest(t, rt, "hrq_cli_"+cmdName+"_single", humanrequest.RequestApproval)

	out := executeCommand(t, "--config", filepath.Join(instance, "xira.yaml"), "human", cmdName, req.ID, "--message", string(kind))
	var resolved humanrequest.HumanRequest
	if err := json.Unmarshal([]byte(out), &resolved); err != nil {
		t.Fatalf("decode %s: %v\n%s", cmdName, err, out)
	}
	if resolved.Status != humanrequest.StatusResolved || resolved.Response == nil || resolved.Response.Kind != kind {
		t.Fatalf("%s resolved = %+v", cmdName, resolved)
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

func executeCommandError(args ...string) (string, error) {
	cmd := newRootCommandWithFactory(func(cfg runtime.Config) (*runtime.Service, error) {
		cfg.DeepSeekClient = fakeCLIDeepSeekClientForError()
		return runtime.NewService(cfg)
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), err
}

func newCLITestRuntime(t *testing.T, instance string) *runtime.Service {
	t.Helper()
	rt, err := runtime.NewService(runtime.Config{
		ConfigPath:     filepath.Join(instance, "xira.yaml"),
		DeepSeekClient: fakeCLIDeepSeekClient(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func seedCLIHumanRequest(t *testing.T, rt *runtime.Service, id string, kind humanrequest.RequestKind) *humanrequest.HumanRequest {
	t.Helper()
	req, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		ID:           id,
		WorkspaceID:  rt.Status()["workspace"].(string),
		WorkspaceKey: rt.WorkspaceKey(),
		RunID:        "run-cli",
		AgentID:      agents.DefaultAgentID,
		SessionID:    "session-cli",
		Kind:         kind,
		Question:     "CLI question?",
	})
	if err != nil {
		t.Fatal(err)
	}
	return req
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

func fakeCLIDeepSeekClientForError() *deepseek.Client {
	client := &http.Client{Transport: cliRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(cliDeepSeekTextResponse("unused"))),
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
state_dir: .xira
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
