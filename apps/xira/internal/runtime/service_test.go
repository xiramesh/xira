package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adksession "google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/model/deepseek"
	fsession "github.com/xiramesh/xira/internal/session"
	rtools "github.com/xiramesh/xira/internal/tools"
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
	if _, _, err := reloaded.hydrateADKSession(context.Background(), "user-1", agentSessionID, resp.AgentID, resp.SessionID); err != nil {
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

func TestToolOutputForModelBoundsCommandStreams(t *testing.T) {
	output := toolOutputForModel(ToolCallRecord{
		Name:  "shell.run",
		Input: map[string]any{"max_stdout_bytes": 4, "max_stderr_bytes": 3},
		Output: map[string]any{
			"stdout":          "abcdef",
			"stderr":          "wxyz",
			"exit_code":       0,
			"duration_ms":     1,
			"raw_output_path": "artifacts/tool-outputs/call-1.json",
		},
	})
	if _, ok := output["stdout"]; ok {
		t.Fatalf("model output leaked stdout: %+v", output)
	}
	if _, ok := output["stderr"]; ok {
		t.Fatalf("model output leaked stderr: %+v", output)
	}
	if output["stdout_preview"] != "abcd" || output["stdout_bytes"] != 6 || output["stdout_truncated"] != true {
		t.Fatalf("stdout model output = %+v", output)
	}
	if output["stderr_preview"] != "wxy" || output["stderr_bytes"] != 4 || output["stderr_truncated"] != true {
		t.Fatalf("stderr model output = %+v", output)
	}
	if output["truncated"] != true || output["status"] != "ok" {
		t.Fatalf("model output = %+v", output)
	}
	if output["raw_output_path"] != "artifacts/tool-outputs/call-1.json" {
		t.Fatalf("model output missing raw output path: %+v", output)
	}
	if !strings.Contains(fmt.Sprint(output["raw_output_hint"]), "tool_output.read") {
		t.Fatalf("model output missing raw output hint: %+v", output)
	}
}

func TestRunAgentCanUseCommandRunTool(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		AgentID: agents.ResearchAssistantAgentID,
		Message: "please call command",
		Channel: "test",
	})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "command.run" {
		t.Fatalf("expected command.run tool call: %+v", resp.ToolCalls)
	}
}

func TestRuntimeToolDefinitionsDoNotExposeExec(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	profile := agents.BuiltinResearchAssistant()
	for _, tool := range rt.toolDefinitions(profile) {
		if tool.Function.Name == "exec" {
			t.Fatalf("native tool definitions exposed exec: %+v", tool)
		}
	}
	adkTools, err := rt.adkTools(context.Background(), profile, func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {}, func(ToolCallRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range adkTools {
		if tool.Name() == "exec" {
			t.Fatalf("ADK tools exposed exec: %+v", tool)
		}
	}
}

func TestAgentRegistryExposesLoadedValidEnabledProfiles(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	entries := rt.AgentRegistry()
	if len(entries) != 2 {
		t.Fatalf("registry entries = %+v", entries)
	}
	for _, entry := range entries {
		if !entry.Installed || !entry.Valid || !entry.Enabled || !entry.Discoverable {
			t.Fatalf("registry entry should be loaded+valid+enabled: %+v", entry)
		}
		if entry.InputSchema != "delegate_task_v1" || entry.OutputSchema != "delegate_result_v1" {
			t.Fatalf("registry schemas = %+v", entry)
		}
	}
	if entries[0].ID != agents.DefaultAgentID {
		t.Fatalf("default agent should be listed first: %+v", entries)
	}
}

func TestRuntimeOwnedToolsAreInjectedByPolicyOnly(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	defaultProfile := agents.BuiltinXiraAssistant()
	researchProfile := agents.BuiltinResearchAssistant()
	for _, name := range []string{"delegate_agent", "emit_status"} {
		if rt.toolRegistry(defaultProfile).Has(name) {
			t.Fatalf("%s should not be exposed by ordinary tool registry", name)
		}
	}

	adkTools, err := rt.adkTools(context.Background(), defaultProfile, func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {}, func(ToolCallRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	if !adkToolNames(adkTools)["delegate_agent"] || !adkToolNames(adkTools)["emit_status"] {
		t.Fatalf("runtime-owned tools missing for delegated caller: %+v", adkToolNames(adkTools))
	}

	adkTools, err = rt.adkTools(context.Background(), researchProfile, func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {}, func(ToolCallRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	if adkToolNames(adkTools)["delegate_agent"] {
		t.Fatalf("delegate_agent should not be injected for non-delegating profile: %+v", adkToolNames(adkTools))
	}
	if !adkToolNames(adkTools)["emit_status"] {
		t.Fatalf("emit_status should be available as status producer: %+v", adkToolNames(adkTools))
	}
}

func TestRuntimeEventsUseV1EnvelopeWithLegacyFields(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "please call command",
		Channel: "xiragarden",
		UserID:  "user-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawCompleted, sawFinished bool
	for _, evt := range resp.Events {
		if evt.SchemaVersion != 1 {
			t.Fatalf("event missing schema_version=1: %+v", evt)
		}
		if evt.RunID == "" || evt.Source == "" || evt.Payload == nil {
			t.Fatalf("event missing legacy fields: %+v", evt)
		}
		if evt.SourceDetail == nil || evt.SourceDetail.Component == "" {
			t.Fatalf("event missing source_detail: %+v", evt)
		}
		if evt.Scope == nil || evt.Scope.RunID != resp.RunID || evt.Scope.AgentID != resp.AgentID || evt.Scope.Channel != "xiragarden" {
			t.Fatalf("event scope = %+v, want run/agent/channel", evt.Scope)
		}
		if got := evt.Payload["channel"]; got != "xiragarden" {
			t.Fatalf("event payload channel = %v", got)
		}
		if evt.Visibility == nil || !evt.Visibility.Inspector {
			t.Fatalf("event visibility = %+v", evt.Visibility)
		}
		sawCompleted = sawCompleted || evt.Kind == "tool.completed"
		sawFinished = sawFinished || evt.Kind == "tool.finished"
	}
	if !sawCompleted || !sawFinished {
		t.Fatalf("events missing tool.completed/tool.finished compatibility: %+v", eventKinds(resp.Events))
	}
}

func TestToolStartedEventInputCannotSpoofRuntimeIdentity(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	runID := "spoof-tool-event-run"
	base := runtimeEventBase{
		RunID:        runID,
		AgentID:      agents.DefaultAgentID,
		EntrypointID: "xiragarden-default",
		Channel:      "xiragarden",
		TraceID:      runID,
	}
	var events []RuntimeEvent
	recordEvent := func(kind, source, message string, payload map[string]any) {
		events = append(events, newRuntimeEvent(base, kind, source, message, payload, nil))
	}
	ctx := contextWithToolTrace(context.Background(), runID)
	rec := rt.executeToolCall(ctx, agents.BuiltinXiraAssistant(), deepseek.ToolCall{
		ID:   "spoof-tool-input",
		Type: "function",
		Function: deepseek.ToolCallFunction{
			Name: "command.run",
			Arguments: mustJSON(map[string]any{
				"program":  "printf",
				"args":     []string{"ok"},
				"channel":  "evil-channel",
				"run_id":   "evil-run",
				"agent_id": "evil-agent",
			}),
		},
	}, recordEvent, func(string, string, bool, string, map[string]any) {})
	if rec.Error != "" {
		t.Fatalf("tool call failed: %+v", rec)
	}
	evt, ok := findEvent(events, "tool.started")
	if !ok {
		t.Fatalf("events missing tool.started: %+v", eventKinds(events))
	}
	if evt.Payload["channel"] != "xiragarden" || evt.Payload["run_id"] != runID || evt.Payload["agent_id"] != agents.DefaultAgentID {
		t.Fatalf("runtime identity was not authoritative: %+v", evt.Payload)
	}
	if evt.Payload["input"] == nil {
		t.Fatalf("tool input should be nested under payload.input: %+v", evt.Payload)
	}
}

func TestToolOnlyTurnEmitsNoDelegationEvents(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "please call command",
		Channel: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, evt := range resp.Events {
		if strings.HasPrefix(evt.Kind, "agent.delegate.") || strings.HasPrefix(evt.Kind, "context.packet.") {
			t.Fatalf("tool-only turn emitted delegation event %q: %+v", evt.Kind, eventKinds(resp.Events))
		}
	}
}

func TestAssistantStatusToolEmitsStatusEventWithoutPersistingContent(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		body := deepSeekToolCallResponseWithArgs("status-1", "emit_status", map[string]any{
			"message": "I am checking local context.",
		})
		if lastRole(req.Messages) == "tool" {
			body = deepSeekTextResponse("status final")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	rt := newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "emit status", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("status tool should not persist as tool transcript: %+v", resp.ToolCalls)
	}
	statusEvent, ok := findEvent(resp.Events, "assistant.status")
	if !ok {
		t.Fatalf("events missing assistant.status: %+v", eventKinds(resp.Events))
	}
	if statusEvent.Message != "I am checking local context." || statusEvent.Payload["producer"] != "runtime.status_tool" {
		t.Fatalf("status event = %+v", statusEvent)
	}
	if statusEvent.Visibility == nil || !statusEvent.Visibility.Conversation {
		t.Fatalf("status visibility = %+v", statusEvent.Visibility)
	}
	history := rt.SessionManager().AgentHistory(resp.SessionID, resp.AgentID)
	if len(history) != 2 {
		t.Fatalf("session history len = %d, want user+final only: %+v", len(history), history)
	}
	for _, msg := range history {
		if strings.Contains(msg.Content, "I am checking local context.") {
			t.Fatalf("status text leaked into session history: %+v", history)
		}
	}
}

func TestAuthorizedDelegationEmitsProgressAndUsesEphemeralChildRun(t *testing.T) {
	var childRequestSeen bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		system := ""
		if len(req.Messages) > 0 {
			system = fmt.Sprint(req.Messages[0].Content)
		}
		var body string
		switch {
		case lastRole(req.Messages) == "tool":
			body = deepSeekTextResponse("parent synthesized final")
		case strings.Contains(system, "Current Xira agent: research-assistant"):
			childRequestSeen = true
			body = deepSeekTextResponse(`{"summary":"child evidence summary","limitations":["fixture only"],"confidence":"high","followup_needed":false}`)
		default:
			body = deepSeekToolCallResponseWithArgs("delegate-1", "delegate_agent", map[string]any{
				"agent_id":               agents.ResearchAssistantAgentID,
				"task":                   "Check local profile and tool registry evidence.",
				"context_refs":           []string{"conversation://current-turn/user-message"},
				"expected_output_schema": delegateResultSchemaV1,
			})
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	runRoot := filepath.Join(t.TempDir(), "runs")
	rt := newTestService(t, Config{
		RunRoot:        runRoot,
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "please delegate", Channel: "xiragarden", UserID: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !childRequestSeen {
		t.Fatal("child agent request was not sent")
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "delegate_agent" || resp.ToolCalls[0].Error != "" {
		t.Fatalf("delegate tool call = %+v", resp.ToolCalls)
	}
	childRunID, _ := resp.ToolCalls[0].Output["run_id"].(string)
	if childRunID == "" {
		t.Fatalf("delegate output missing child run id: %+v", resp.ToolCalls[0].Output)
	}
	childRun, err := rt.RunStore().Load(childRunID)
	if err != nil {
		t.Fatalf("load child run %q: %v", childRunID, err)
	}
	if childRun.AgentID != agents.ResearchAssistantAgentID || !strings.HasPrefix(childRun.SessionID, "ephemeral_worker:") {
		t.Fatalf("child run = %+v", childRun)
	}
	if history := rt.SessionManager().AgentHistory(resp.SessionID, agents.ResearchAssistantAgentID); len(history) != 0 {
		t.Fatalf("ephemeral child should not persist research assistant session history: %+v", history)
	}
	for _, kind := range []string{
		"agent.delegate.requested",
		"agent.delegate.allowed",
		"context.packet.started",
		"context.item.included",
		"context.packet.completed",
		"agent.delegate.started",
		"agent.delegate.completed",
		"agent.delegate.result_delivered",
	} {
		evt, ok := findEvent(resp.Events, kind)
		if !ok {
			t.Fatalf("events missing %s: %+v", kind, eventKinds(resp.Events))
		}
		if evt.Correlation == nil || evt.Correlation.ParentRunID != resp.RunID || evt.Correlation.ChildRunID != childRunID {
			t.Fatalf("%s correlation = %+v", kind, evt.Correlation)
		}
		if evt.Payload["channel"] != "xiragarden" || evt.Payload["entrypoint_id"] == "" {
			t.Fatalf("%s missing legacy channel payload keys: %+v", kind, evt.Payload)
		}
	}
	if refs, ok := resp.ToolCalls[0].Output["evidence_refs"].([]string); ok && len(refs) > 0 {
		if !strings.HasPrefix(refs[0], "context://"+childRunID+"/context/") {
			t.Fatalf("evidence refs not child-local: %+v", refs)
		}
	} else {
		t.Fatalf("delegate output missing runtime context evidence refs: %+v", resp.ToolCalls[0].Output)
	}
	if _, err := os.Stat(filepath.Join(runRoot, childRunID, "artifacts", "context", "ctxitem_current_user_message.json")); err != nil {
		t.Fatalf("child context item was not materialized: %v", err)
	}
	packet := readJSONMap(t, filepath.Join(runRoot, childRunID, "artifacts", "context", "context_packet.json"))
	target, _ := packet["target"].(map[string]any)
	for _, key := range []string{"profile_instruction_hash", "profile_instruction_ref", "allowed_tools_hash"} {
		if strings.TrimSpace(fmt.Sprint(target[key])) == "" {
			t.Fatalf("context packet target missing %s: %+v", key, target)
		}
	}
	workerTarget := delegateWorkerProfile(agents.BuiltinResearchAssistant())
	if target["profile_instruction_hash"] != instructionHash(rt.instructionText(workerTarget)) {
		t.Fatalf("context packet target instruction hash = %v, want effective child instruction hash", target["profile_instruction_hash"])
	}
	if target["profile_instruction_hash"] == instructionHash(agents.BuiltinResearchAssistant().InstructionText()) {
		t.Fatalf("context packet target instruction hash should not use raw profile instructions only")
	}
	if gotTools, wantTools := strings.Join(stringSliceFromAny(target["allowed_tools"]), "\n"), strings.Join(rt.toolRegistry(workerTarget).List(), "\n"); gotTools != wantTools {
		t.Fatalf("context packet allowed tools = %q, want actual registry tools %q", gotTools, wantTools)
	}
	items, _ := packet["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("context packet missing items: %+v", packet)
	}
	first, _ := items[0].(map[string]any)
	for _, key := range []string{"content_hash", "source_run_id", "source_ref"} {
		if strings.TrimSpace(fmt.Sprint(first[key])) == "" {
			t.Fatalf("context item missing %s: %+v", key, first)
		}
	}
}

func TestContextPacketTargetUsesEffectiveInstructionAndActualRegistryTools(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	childRunID := "child-run"
	if err := rt.RunStore().InitRun(childRunID); err != nil {
		t.Fatal(err)
	}
	target := agents.BuiltinResearchAssistant()
	target.Permissions.Tools = []string{"missing.tool", "command.run"}
	workerTarget := delegateWorkerProfile(target)
	packet, _, err := rt.buildDelegateContextPacket(runExecutionContext{
		Base: runtimeEventBase{
			RunID:                 "parent-run",
			AgentID:               agents.DefaultAgentID,
			ConversationSessionID: "conversation:test",
			AgentSessionID:        "session:test",
		},
		Profile:     agents.BuiltinXiraAssistant(),
		UserMessage: "parent message",
	}, workerTarget, childRunID, delegateAgentInput{
		Task:                 "check provenance",
		ExpectedOutputSchema: delegateResultSchemaV1,
	}, delegateCorrelationPayload("parent-run", childRunID, "delegate-call", agents.DefaultAgentID, target.ID), func(string, string, string, map[string]any) {})
	if err != nil {
		t.Fatal(err)
	}
	targetPacket := packet.Target
	if targetPacket["profile_instruction_hash"] != instructionHash(rt.instructionText(workerTarget)) {
		t.Fatalf("profile_instruction_hash = %v, want effective worker instruction hash", targetPacket["profile_instruction_hash"])
	}
	if targetPacket["profile_instruction_hash"] == instructionHash(target.InstructionText()) {
		t.Fatalf("profile_instruction_hash should include runtime worker instruction boundary")
	}
	if gotTools, wantTools := strings.Join(stringSliceFromAny(targetPacket["allowed_tools"]), "\n"), "command.run"; gotTools != wantTools {
		t.Fatalf("allowed_tools = %q, want actual registry tools %q", gotTools, wantTools)
	}
}

func TestContextPacketMaterializesParentToolOutputRef(t *testing.T) {
	runRoot := filepath.Join(t.TempDir(), "runs")
	rt := newTestService(t, Config{RunRoot: runRoot})
	parentRunID := "parent-run"
	childRunID := "child-run"
	if err := rt.RunStore().InitRun(parentRunID); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunStore().InitRun(childRunID); err != nil {
		t.Fatal(err)
	}
	rawPath := "artifacts/tool-outputs/call-1.json"
	longStdout := strings.Repeat("parent evidence ", 300)
	if err := writeJSONFile(filepath.Join(rt.RunStore().RunDir(parentRunID), filepath.FromSlash(rawPath)), map[string]any{
		"run_id":       parentRunID,
		"tool_call_id": "call-1",
		"tool":         "command.run",
		"stdout":       longStdout,
		"stderr":       "warning",
		"exit_code":    0,
	}); err != nil {
		t.Fatal(err)
	}
	var events []RuntimeEvent
	base := runtimeEventBase{RunID: parentRunID, AgentID: agents.DefaultAgentID}
	packet, refs, err := rt.buildDelegateContextPacket(runExecutionContext{
		Base:        base,
		Profile:     agents.BuiltinXiraAssistant(),
		UserMessage: "parent message",
	}, delegateWorkerProfile(agents.BuiltinResearchAssistant()), childRunID, delegateAgentInput{
		Task:                 "inspect parent evidence",
		ContextRefs:          []string{rawPath},
		ExpectedOutputSchema: delegateResultSchemaV1,
	}, delegateCorrelationPayload(parentRunID, childRunID, "delegate-materialize", agents.DefaultAgentID, agents.ResearchAssistantAgentID), func(kind, source, message string, payload map[string]any) {
		events = append(events, newRuntimeEvent(base, kind, source, message, payload, nil))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Items) != 1 {
		t.Fatalf("context items = %+v", packet.Items)
	}
	item := packet.Items[0]
	if item.Kind != "tool_result" || item.Source != "parent_tool_output" || item.SourceRunID != parentRunID || item.SourceToolCallID != "call-1" {
		t.Fatalf("materialized item = %+v", item)
	}
	wantContextRef := "context://" + childRunID + "/context/ctxitem_tool_output_call-1"
	wantArtifactRef := "artifact://" + childRunID + "/artifacts/tool-outputs/context_call-1.json"
	if item.Ref != wantContextRef || item.ArtifactRef != wantArtifactRef {
		t.Fatalf("item refs = %+v, want %s and %s", item, wantContextRef, wantArtifactRef)
	}
	if !containsString(refs, wantContextRef) || !containsString(refs, wantArtifactRef) {
		t.Fatalf("canonical refs = %+v", refs)
	}
	allowedRefs := rt.allowedDelegateEvidenceRefs(childRunID, delegateWorkerProfile(agents.BuiltinResearchAssistant()), refs, TurnResponse{})
	if _, ok := allowedRefs[wantArtifactRef]; !ok {
		t.Fatalf("materialized artifact ref not allowed for child result evidence: %+v", allowedRefs)
	}
	if _, err := validateDelegateAgentResult(
		fmt.Sprintf(`{"summary":"uses copied artifact","evidence_refs":[%q],"confidence":"high","followup_needed":false}`, wantArtifactRef),
		agents.ResearchAssistantAgentID,
		childRunID,
		refs,
		allowedRefs,
	); err != nil {
		t.Fatalf("materialized artifact evidence ref should validate: %v", err)
	}
	if strings.Contains(item.ContentPreview, longStdout) {
		t.Fatal("context preview includes unbounded parent stdout")
	}
	copied := readJSONMap(t, filepath.Join(rt.RunStore().RunDir(childRunID), "artifacts", "tool-outputs", "context_call-1.json"))
	if copied["run_id"] != childRunID || copied["source_run_id"] != parentRunID || copied["source_ref"] != rawPath || copied["stdout"] != longStdout {
		t.Fatalf("copied child-local artifact = %+v", copied)
	}
	if _, ok := findEvent(events, "context.item.included"); !ok {
		t.Fatalf("missing included event: %+v", eventKinds(events))
	}
	for _, event := range events {
		if event.Kind == "context.item.redacted" && event.Payload["source_ref"] == rawPath {
			t.Fatalf("materialized ref was also redacted: %+v", event)
		}
	}
}

func TestDelegateRejectsUnauthorizedDepthAndParallelBeforeChildRun(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	base := runtimeEventBase{
		RunID:        "parent-run",
		AgentID:      agents.DefaultAgentID,
		EntrypointID: "test-default",
		Channel:      "test",
		TraceID:      "parent-run",
	}
	recordEvent := func(string, string, string, map[string]any) {}
	recordAudit := func(string, string, bool, string, map[string]any) {}
	input := delegateAgentInput{
		AgentID:              agents.ResearchAssistantAgentID,
		Task:                 "child task",
		ExpectedOutputSchema: delegateResultSchemaV1,
	}

	unauthorized := agents.BuiltinXiraAssistant()
	unauthorized.Delegation.Allow = []string{"other-agent"}
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{Base: base, Profile: unauthorized, UserMessage: "parent"})
	output, err := rt.executeDelegateAgentTool(ctx, unauthorized, input, nil, nil, "delegate-unauthorized", recordEvent, recordAudit)
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("unauthorized error = %v output=%+v", err, output)
	}
	assertRejectedChildRunNotCreated(t, rt, output)

	depthLimited := agents.BuiltinXiraAssistant()
	depthLimited.Delegation.MaxDepth = 1
	depthBase := base
	depthBase.DelegationDepth = 1
	ctx = contextWithRunExecution(context.Background(), runExecutionContext{Base: depthBase, Profile: depthLimited, UserMessage: "parent"})
	output, err = rt.executeDelegateAgentTool(ctx, depthLimited, input, nil, nil, "delegate-depth", recordEvent, recordAudit)
	if err == nil || !strings.Contains(err.Error(), "exceeds max_depth") {
		t.Fatalf("depth error = %v output=%+v", err, output)
	}
	assertRejectedChildRunNotCreated(t, rt, output)

	parallelLimited := agents.BuiltinXiraAssistant()
	parallelLimited.Delegation.MaxParallel = 1
	if _, ok := rt.reserveChildSlot(base.RunID, 1); !ok {
		t.Fatal("failed to reserve initial child slot")
	}
	defer rt.releaseChildSlot(base.RunID)
	ctx = contextWithRunExecution(context.Background(), runExecutionContext{Base: base, Profile: parallelLimited, UserMessage: "parent"})
	output, err = rt.executeDelegateAgentTool(ctx, parallelLimited, input, nil, nil, "delegate-parallel", recordEvent, recordAudit)
	if err == nil || !strings.Contains(err.Error(), "max_parallel") {
		t.Fatalf("parallel error = %v output=%+v", err, output)
	}
	assertRejectedChildRunNotCreated(t, rt, output)
}

func TestDelegationMaxDepthZeroFromProfileRejectsChildBeforeRun(t *testing.T) {
	instance := writeRuntimeFixture(t, agents.DefaultAgentID, []string{"chat", "sender"})
	writeFile(t, filepath.Join(instance, "workspace", "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: Default Xira runtime assistant.
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
delegation:
  enabled: true
  allow:
    - research-assistant
  max_depth: 0
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Do not create child runs.
`)
	rt := newTestService(t, Config{ConfigPath: filepath.Join(instance, "xira.yaml")})
	caller, ok := rt.agents.Get(agents.DefaultAgentID)
	if !ok {
		t.Fatal("missing xira assistant")
	}
	if policy := caller.NormalizedDelegationPolicy(); policy.MaxDepth != 0 {
		t.Fatalf("caller max_depth = %d, want explicit 0", policy.MaxDepth)
	}
	base := runtimeEventBase{
		RunID:        "parent-run",
		AgentID:      agents.DefaultAgentID,
		EntrypointID: "test-default",
		Channel:      "test",
		TraceID:      "parent-run",
	}
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{Base: base, Profile: caller, UserMessage: "parent"})
	output, err := rt.executeDelegateAgentTool(ctx, caller, delegateAgentInput{
		AgentID:              agents.ResearchAssistantAgentID,
		Task:                 "should be depth blocked",
		ExpectedOutputSchema: delegateResultSchemaV1,
	}, nil, nil, "delegate-depth-zero", func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {})
	if err == nil || !strings.Contains(err.Error(), "exceeds max_depth 0") {
		t.Fatalf("depth-zero error = %v output=%+v", err, output)
	}
	assertRejectedChildRunNotCreated(t, rt, output)
}

func TestDelegationContextTruncationIsVisibleToParent(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		system := ""
		if len(req.Messages) > 0 {
			system = fmt.Sprint(req.Messages[0].Content)
		}
		var body string
		switch {
		case lastRole(req.Messages) == "tool":
			body = deepSeekTextResponse("parent final after truncation")
		case strings.Contains(system, "Current Xira agent: research-assistant"):
			body = deepSeekTextResponse(`{"summary":"truncated context handled","confidence":"medium","followup_needed":false}`)
		default:
			body = deepSeekToolCallResponseWithArgs("delegate-truncate", "delegate_agent", map[string]any{
				"agent_id":     agents.ResearchAssistantAgentID,
				"task":         "summarize bounded context",
				"context_refs": []string{"conversation://current-turn/user-message"},
			})
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	rt := newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: strings.Repeat("x", 2500),
		Channel: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findEvent(resp.Events, "context.packet.truncated"); !ok {
		t.Fatalf("events missing context.packet.truncated: %+v", eventKinds(resp.Events))
	}
	completed, ok := findEvent(resp.Events, "context.packet.completed")
	if !ok || completed.Payload["truncated"] != true {
		t.Fatalf("context.packet.completed = %+v", completed)
	}
}

func TestDelegateTimeoutPreventsLateSuccessEvents(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		system := ""
		if len(req.Messages) > 0 {
			system = fmt.Sprint(req.Messages[0].Content)
		}
		switch {
		case lastRole(req.Messages) == "tool":
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(deepSeekTextResponse("timeout handled")))}, nil
		case strings.Contains(system, "Current Xira agent: research-assistant"):
			<-r.Context().Done()
			return nil, r.Context().Err()
		default:
			body := deepSeekToolCallResponseWithArgs("delegate-timeout", "delegate_agent", map[string]any{
				"agent_id":        agents.ResearchAssistantAgentID,
				"task":            "slow child work",
				"max_duration_ms": 1,
			})
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
	})}
	rt := newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate slowly", Channel: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findEvent(resp.Events, "agent.delegate.timeout"); !ok {
		t.Fatalf("events missing timeout: %+v", eventKinds(resp.Events))
	}
	for _, forbidden := range []string{"agent.delegate.completed", "agent.delegate.result_delivered"} {
		if _, ok := findEvent(resp.Events, forbidden); ok {
			t.Fatalf("timeout should not emit %s: %+v", forbidden, eventKinds(resp.Events))
		}
	}
	if len(resp.ToolCalls) != 1 || !strings.Contains(resp.ToolCalls[0].Error, "context deadline exceeded") {
		t.Fatalf("timeout delegate tool call = %+v", resp.ToolCalls)
	}
}

func TestDelegateOversizedDurationClampsToPolicyMax(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		system := ""
		if len(req.Messages) > 0 {
			system = fmt.Sprint(req.Messages[0].Content)
		}
		var body string
		switch {
		case lastRole(req.Messages) == "tool":
			body = deepSeekTextResponse("parent synthesized final")
		case strings.Contains(system, "Current Xira agent: research-assistant"):
			body = deepSeekTextResponse(`{"summary":"bounded child completed","confidence":"high","followup_needed":false}`)
		default:
			body = deepSeekToolCallResponseWithArgs("delegate-clamped-duration", "delegate_agent", map[string]any{
				"agent_id":        agents.ResearchAssistantAgentID,
				"task":            "finish quickly despite oversized request",
				"max_duration_ms": 300000,
			})
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	rt := newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate oversized duration", Channel: "test"})
	if err != nil {
		t.Fatal(err)
	}
	allowed, ok := findEvent(resp.Events, "agent.delegate.allowed")
	if !ok {
		t.Fatalf("events missing agent.delegate.allowed: %+v", eventKinds(resp.Events))
	}
	if allowed.Payload["requested_max_duration_ms"] != 300000 || allowed.Payload["effective_max_duration_ms"] != 120000 {
		t.Fatalf("duration was not clamped: %+v", allowed.Payload)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Error != "" {
		t.Fatalf("oversized duration should not reject delegation: %+v", resp.ToolCalls)
	}
}

func TestDelegateAgentRejectsEmptyChildResultAsInvalidChildResult(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		system := ""
		if len(req.Messages) > 0 {
			system = fmt.Sprint(req.Messages[0].Content)
		}
		var body string
		switch {
		case lastRole(req.Messages) == "tool":
			body = deepSeekTextResponse("empty result rejected")
		case strings.Contains(system, "Current Xira agent: research-assistant"):
			body = deepSeekTextResponse(`{}`)
		default:
			body = deepSeekToolCallResponseWithArgs("delegate-empty-result", "delegate_agent", map[string]any{
				"agent_id": agents.ResearchAssistantAgentID,
				"task":     "return empty result",
			})
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	rt := newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate empty result", Channel: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "delegate_agent" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Output["error"] != "invalid_child_result" || resp.ToolCalls[0].Output["reason"] != "result_schema_failed" {
		t.Fatalf("empty child result output = %+v", resp.ToolCalls[0].Output)
	}
	if _, ok := findEvent(resp.Events, "agent.delegate.completed"); ok {
		t.Fatalf("invalid child result should not complete: %+v", eventKinds(resp.Events))
	}
	failed, ok := findEvent(resp.Events, "agent.delegate.failed")
	if !ok || failed.Payload["reason"] != "result_schema_failed" || failed.Payload["error"] != "invalid_child_result" || failed.Payload["raw_child_result_path"] == "" {
		t.Fatalf("invalid child result failure event = %+v", failed)
	}
	childRunID, _ := resp.ToolCalls[0].Output["run_id"].(string)
	if childRunID == "" {
		t.Fatalf("missing child run id: %+v", resp.ToolCalls[0].Output)
	}
	if _, err := os.Stat(filepath.Join(rt.RunStore().RunDir(childRunID), "artifacts", "delegate-result", "rejected.json")); err != nil {
		t.Fatalf("rejected child result artifact missing: %v", err)
	}
}

func TestDelegateAgentRejectsSpoofedRuntimeFieldsBeforeChildRun(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	base := runtimeEventBase{
		RunID:        "parent-run",
		AgentID:      agents.DefaultAgentID,
		EntrypointID: "test-default",
		Channel:      "test",
		TraceID:      "parent-run",
	}
	var events []RuntimeEvent
	recordEvent := func(kind, source, message string, payload map[string]any) {
		events = append(events, newRuntimeEvent(base, kind, source, message, payload, nil))
	}
	recordAudit := func(string, string, bool, string, map[string]any) {}
	caller := agents.BuiltinXiraAssistant()
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{Base: base, Profile: caller, UserMessage: "parent"})
	output, err := rt.executeDelegateAgentTool(ctx, caller, delegateAgentInput{
		AgentID:              agents.ResearchAssistantAgentID,
		Task:                 "try to spoof runtime fields",
		ExpectedOutputSchema: delegateResultSchemaV1,
	}, []string{"metadata", "scope"}, nil, "delegate-spoof", recordEvent, recordAudit)
	if err == nil || !strings.Contains(err.Error(), "runtime-owned field") {
		t.Fatalf("spoofed-field error = %v output=%+v", err, output)
	}
	assertRejectedChildRunNotCreated(t, rt, output)
	if _, ok := findEvent(events, "agent.delegate.started"); ok {
		t.Fatalf("spoofed delegate request should reject before child start: %+v", eventKinds(events))
	}
	rejected, ok := findEvent(events, "agent.delegate.rejected")
	if !ok || !strings.Contains(rejected.Message, "runtime-owned field") {
		t.Fatalf("missing rejection event: %+v", events)
	}
}

func TestDelegateAgentRejectsUnknownInputFieldsBeforeChildRun(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	base := runtimeEventBase{
		RunID:        "parent-run",
		AgentID:      agents.DefaultAgentID,
		EntrypointID: "test-default",
		Channel:      "test",
		TraceID:      "parent-run",
	}
	var events []RuntimeEvent
	recordEvent := func(kind, source, message string, payload map[string]any) {
		events = append(events, newRuntimeEvent(base, kind, source, message, payload, nil))
	}
	recordAudit := func(string, string, bool, string, map[string]any) {}
	caller := agents.BuiltinXiraAssistant()
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{Base: base, Profile: caller, UserMessage: "parent"})
	output, err := rt.executeDelegateAgentTool(ctx, caller, delegateAgentInput{
		AgentID:              agents.ResearchAssistantAgentID,
		Task:                 "try unknown field",
		ExpectedOutputSchema: delegateResultSchemaV1,
	}, nil, []string{"label"}, "delegate-unknown", recordEvent, recordAudit)
	if err == nil || !strings.Contains(err.Error(), "unsupported field") {
		t.Fatalf("unknown-field error = %v output=%+v", err, output)
	}
	assertRejectedChildRunNotCreated(t, rt, output)
	rejected, ok := findEvent(events, "agent.delegate.rejected")
	if !ok || rejected.Payload["unsupported_input_fields"] == nil {
		t.Fatalf("missing unsupported-field rejection event: %+v", events)
	}
}

func TestDelegateAgentRejectsUnsupportedExpectedOutputSchemaBeforeChildRun(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	base := runtimeEventBase{
		RunID:        "parent-run",
		AgentID:      agents.DefaultAgentID,
		EntrypointID: "test-default",
		Channel:      "test",
		TraceID:      "parent-run",
	}
	var events []RuntimeEvent
	recordEvent := func(kind, source, message string, payload map[string]any) {
		events = append(events, newRuntimeEvent(base, kind, source, message, payload, nil))
	}
	recordAudit := func(string, string, bool, string, map[string]any) {}
	caller := agents.BuiltinXiraAssistant()
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{Base: base, Profile: caller, UserMessage: "parent"})
	output, err := rt.executeDelegateAgentTool(ctx, caller, delegateAgentInput{
		AgentID:              agents.ResearchAssistantAgentID,
		Task:                 "try unsupported schema",
		ExpectedOutputSchema: "evidence_summary_v1",
	}, nil, nil, "delegate-schema", recordEvent, recordAudit)
	if err == nil || !strings.Contains(err.Error(), "unsupported expected_output_schema") {
		t.Fatalf("schema error = %v output=%+v", err, output)
	}
	assertRejectedChildRunNotCreated(t, rt, output)
	if _, ok := findEvent(events, "agent.delegate.started"); ok {
		t.Fatalf("unsupported schema should reject before child start: %+v", eventKinds(events))
	}
	rejected, ok := findEvent(events, "agent.delegate.rejected")
	if !ok || rejected.Payload["supported_schema"] != delegateResultSchemaV1 || rejected.Payload["expected_output_schema"] != "evidence_summary_v1" {
		t.Fatalf("missing schema rejection payload: %+v", events)
	}
}

func TestDelegateAgentRejectsForgedChildResultRefs(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		system := ""
		if len(req.Messages) > 0 {
			system = fmt.Sprint(req.Messages[0].Content)
		}
		var body string
		switch {
		case lastRole(req.Messages) == "tool":
			body = deepSeekTextResponse("forged result rejected")
		case strings.Contains(system, "Current Xira agent: research-assistant"):
			body = deepSeekTextResponse(`{"agent_id":"evil-agent","summary":"bad","evidence_refs":["workspace://secret"],"confidence":"high","followup_needed":false}`)
		default:
			body = deepSeekToolCallResponseWithArgs("delegate-forged-result", "delegate_agent", map[string]any{
				"agent_id": agents.ResearchAssistantAgentID,
				"task":     "return forged evidence refs",
			})
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	rt := newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate forged result", Channel: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "delegate_agent" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Error == "" || !strings.Contains(resp.ToolCalls[0].Error, "forged") {
		t.Fatalf("delegate tool should reject forged child output: %+v", resp.ToolCalls[0])
	}
	if failed, ok := findEvent(resp.Events, "agent.delegate.failed"); !ok || !strings.Contains(failed.Message, "forged") {
		t.Fatalf("missing forged-result failure event: %+v", resp.Events)
	}
}

func TestDelegateAgentRejectsUnregisteredChildArtifactEvidenceRef(t *testing.T) {
	childRunID := "child-run"
	contextRef := "context://" + childRunID + "/context/ctxitem_current_user_message"
	_, err := validateDelegateAgentResult(
		`{"summary":"bad artifact ref","evidence_refs":["artifact://child-run/artifacts/tool-outputs/fake.json"],"confidence":"high","followup_needed":false}`,
		agents.ResearchAssistantAgentID,
		childRunID,
		[]string{contextRef},
		map[string]struct{}{contextRef: {}},
	)
	if err == nil || !strings.Contains(err.Error(), "forged delegate result evidence ref") {
		t.Fatalf("validateDelegateAgentResult error = %v, want forged artifact ref rejection", err)
	}
}

func TestDelegateEvidenceRefsAllowExistingChildToolArtifact(t *testing.T) {
	runRoot := filepath.Join(t.TempDir(), "runs")
	rt := newTestService(t, Config{RunRoot: runRoot})
	childRunID := "child-run"
	if err := rt.RunStore().InitRun(childRunID); err != nil {
		t.Fatal(err)
	}
	rawPath := "artifacts/tool-outputs/call-1.json"
	if err := writeJSONFile(filepath.Join(rt.RunStore().RunDir(childRunID), filepath.FromSlash(rawPath)), map[string]any{
		"run_id":       childRunID,
		"tool_call_id": "call-1",
		"tool":         "command.run",
	}); err != nil {
		t.Fatal(err)
	}
	contextRef := "context://" + childRunID + "/context/ctxitem_current_user_message"
	artifactRef := "artifact://" + childRunID + "/" + rawPath
	allowed := rt.allowedDelegateEvidenceRefs(childRunID, agents.BuiltinResearchAssistant(), []string{contextRef}, TurnResponse{
		RunID:   childRunID,
		AgentID: agents.ResearchAssistantAgentID,
		ToolCalls: []ToolCallRecord{{
			RunID:  childRunID,
			Name:   "command.run",
			Output: map[string]any{"raw_output_path": rawPath},
		}},
	})
	if _, ok := allowed[artifactRef]; !ok {
		t.Fatalf("allowed evidence refs missing tool artifact %q: %+v", artifactRef, allowed)
	}
	result, err := validateDelegateAgentResult(
		`{"summary":"tool artifact is real","evidence_refs":["`+artifactRef+`"],"confidence":"high","followup_needed":false}`,
		agents.ResearchAssistantAgentID,
		childRunID,
		[]string{contextRef},
		allowed,
	)
	if err != nil {
		t.Fatalf("validateDelegateAgentResult returned error: %v", err)
	}
	if !containsString(result.EvidenceRefs, artifactRef) {
		t.Fatalf("result evidence refs missing artifact ref: %+v", result.EvidenceRefs)
	}
}

func TestUnauthorizedDelegationRecordsCapabilityGap(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	base := runtimeEventBase{
		RunID:        "parent-run",
		AgentID:      agents.DefaultAgentID,
		EntrypointID: "test-default",
		Channel:      "test",
		TraceID:      "parent-run",
	}
	var events []RuntimeEvent
	recordEvent := func(kind, source, message string, payload map[string]any) {
		events = append(events, newRuntimeEvent(base, kind, source, message, payload, nil))
	}
	recordAudit := func(string, string, bool, string, map[string]any) {}
	caller := agents.BuiltinXiraAssistant()
	caller.Delegation.Allow = []string{"other-agent"}
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{Base: base, Profile: caller, UserMessage: "parent"})
	_, err := rt.executeDelegateAgentTool(ctx, caller, delegateAgentInput{
		AgentID:              agents.ResearchAssistantAgentID,
		Task:                 "needs unavailable specialist",
		ExpectedOutputSchema: delegateResultSchemaV1,
	}, nil, nil, "delegate-capability-gap", recordEvent, recordAudit)
	if err == nil {
		t.Fatal("expected unauthorized delegation error")
	}
	gap, ok := findEvent(events, "capability_gap")
	if !ok {
		t.Fatalf("events missing capability_gap: %+v", eventKinds(events))
	}
	if gap.Visibility == nil || !gap.Visibility.Conversation || !gap.Visibility.Activity || !gap.Visibility.Inspector || !gap.Visibility.Audit {
		t.Fatalf("capability gap visibility = %+v", gap.Visibility)
	}
}

func TestRunAgentRejectsLegacyExecToolCall(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	rec := rt.executeToolCall(
		context.Background(),
		agents.BuiltinResearchAssistant(),
		deepseek.ToolCall{
			ID:   "legacy-exec",
			Type: "function",
			Function: deepseek.ToolCallFunction{
				Name:      "exec",
				Arguments: `{"action":"run","command":"printf should-not-run"}`,
			},
		},
		func(string, string, string, map[string]any) {},
		func(string, string, bool, string, map[string]any) {},
	)
	if rec.Name != "exec" || rec.Error != "tool is not allowed by agent profile" {
		t.Fatalf("legacy exec record = %+v", rec)
	}
}

func TestRunAgentReturnsBoundedShellFailureToADKModel(t *testing.T) {
	var requests []deepseek.ChatRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		requests = append(requests, req)
		body := deepSeekTextResponse("saw failure details")
		if len(requests) == 1 {
			body = deepSeekShellRunToolCallResponseWithArgs("shell-fail-1", map[string]any{
				"command":          `printf 'tool stdout'; printf 'tool stderr' >&2; exit 7`,
				"timeout_seconds":  5,
				"max_stdout_bytes": 4,
				"max_stderr_bytes": 4,
			})
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	rt := newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		AgentID: agents.ResearchAssistantAgentID,
		Message: "please call shell",
		Channel: "test",
	})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "shell.run" || resp.ToolCalls[0].Error != "exit status 7" {
		t.Fatalf("expected failed shell.run tool call: %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Output["exit_code"] != 7 || resp.ToolCalls[0].Output["stderr"] != "tool stderr" {
		t.Fatalf("tool output = %+v", resp.ToolCalls[0].Output)
	}
	if resp.ToolCalls[0].Input["timeout_seconds"] != float64(5) {
		t.Fatalf("tool input = %+v, want timeout_seconds", resp.ToolCalls[0].Input)
	}
	rawPath, _ := resp.ToolCalls[0].Output["raw_output_path"].(string)
	if rawPath == "" {
		t.Fatalf("tool output missing raw_output_path: %+v", resp.ToolCalls[0].Output)
	}
	rawData, err := os.ReadFile(filepath.Join(rt.RunStore().RunDir(resp.RunID), filepath.FromSlash(rawPath)))
	if err != nil {
		t.Fatalf("read raw output: %v", err)
	}
	var rawOutput map[string]any
	if err := json.Unmarshal(rawData, &rawOutput); err != nil {
		t.Fatalf("decode raw output: %v\n%s", err, rawData)
	}
	if rawOutput["tool"] != "shell.run" || rawOutput["stdout"] != "tool stdout" || rawOutput["stderr"] != "tool stderr" || rawOutput["exit_code"] != float64(7) || rawOutput["env_policy"] == "" {
		t.Fatalf("raw output file = %+v", rawOutput)
	}
	if len(requests) < 2 {
		t.Fatalf("requests len = %d, want follow-up request with tool result", len(requests))
	}
	var toolContent string
	for _, message := range requests[1].Messages {
		if message.Role == "tool" {
			toolContent = deepseek.ContentText(message.Content)
			break
		}
	}
	for _, want := range []string{`"stdout_preview":"tool"`, `"stderr_preview":"tool"`, `"stdout_bytes":11`, `"stderr_bytes":11`, `"raw_output_path":"`, `"status":"error"`, `"exit_code":7`, `"error":"exit status 7"`, `"error_message":"exit status 7"`} {
		if !strings.Contains(toolContent, want) {
			t.Fatalf("tool result sent to model missing %q:\n%s", want, toolContent)
		}
	}
	if strings.Contains(toolContent, `"stdout":"tool stdout"`) || strings.Contains(toolContent, `"stderr":"tool stderr"`) {
		t.Fatalf("tool result sent raw streams to model:\n%s", toolContent)
	}
}

func TestToolOutputReadCanReadRawOutputFromCurrentRun(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	runID := "tool-output-read-run"
	if err := rt.RunStore().InitRun(runID); err != nil {
		t.Fatal(err)
	}
	ctx := contextWithToolTrace(context.Background(), runID)
	ctx = rtools.WithRunDir(ctx, rt.RunStore().RunDir(runID))
	profile := agents.BuiltinResearchAssistant()
	shellRec := rt.executeToolCall(
		ctx,
		profile,
		deepseek.ToolCall{
			ID:   "shell-with-long-stderr",
			Type: "function",
			Function: deepseek.ToolCallFunction{
				Name:      "shell.run",
				Arguments: `{"command":"printf 'stdout head'; printf 'warning line\nreal failure\n' >&2; exit 9","max_stderr_bytes":4}`,
			},
		},
		func(string, string, string, map[string]any) {},
		func(string, string, bool, string, map[string]any) {},
	)
	rawPath, _ := shellRec.Output["raw_output_path"].(string)
	if rawPath == "" {
		t.Fatalf("shell output missing raw_output_path: %+v", shellRec)
	}
	readRec := rt.executeToolCall(
		ctx,
		profile,
		deepseek.ToolCall{
			ID:   "read-stderr-tail",
			Type: "function",
			Function: deepseek.ToolCallFunction{
				Name:      "tool_output.read",
				Arguments: mustJSON(map[string]any{"raw_output_path": rawPath, "stream": "stderr", "tail_lines": 1}),
			},
		},
		func(string, string, string, map[string]any) {},
		func(string, string, bool, string, map[string]any) {},
	)
	if readRec.Error != "" {
		t.Fatalf("tool_output.read error = %s output=%+v", readRec.Error, readRec.Output)
	}
	if readRec.Output["content"] != "real failure\n" || readRec.Output["mode"] != "tail" || readRec.Output["stream"] != "stderr" {
		t.Fatalf("tool_output.read output = %+v", readRec.Output)
	}
}

func TestRunAgentPersistsToolTranscriptMessages(t *testing.T) {
	rt := newTestService(t, Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		AgentID: agents.ResearchAssistantAgentID,
		Message: "please call command",
		Channel: "test",
	})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	history := rt.SessionManager().AgentHistory(resp.SessionID, resp.AgentID)
	if len(history) != 4 {
		t.Fatalf("agent history len = %d, want 4: %+v", len(history), history)
	}
	if history[0].Role != "user" || history[0].Kind != fsession.MessageKindMessage {
		t.Fatalf("user transcript message = %+v", history[0])
	}
	if history[1].Role != "assistant" || history[1].Kind != fsession.MessageKindToolCall || history[1].ToolName != "command.run" || history[1].ToolCallID == "" {
		t.Fatalf("tool call transcript message = %+v", history[1])
	}
	if !strings.Contains(history[1].Content, `"program":"printf"`) || !strings.Contains(history[1].Content, "hello from Xira command") {
		t.Fatalf("tool call content = %s", history[1].Content)
	}
	if history[2].Role != "tool" || history[2].Kind != fsession.MessageKindToolResult || history[2].ToolName != "command.run" || history[2].ToolCallID != history[1].ToolCallID {
		t.Fatalf("tool result transcript message = %+v", history[2])
	}
	for _, want := range []string{`"status":"ok"`, `"exit_code":0`, `"stdout_preview":"hello from Xira command"`} {
		if !strings.Contains(history[2].Content, want) {
			t.Fatalf("tool result content missing %q:\n%s", want, history[2].Content)
		}
	}
	if history[3].Role != "assistant" || history[3].Kind != fsession.MessageKindMessage || history[3].Content != "fake tool final" {
		t.Fatalf("assistant final transcript message = %+v", history[3])
	}
}

func TestHydrateADKSessionRestoresPersistedToolHistory(t *testing.T) {
	stateRoot := t.TempDir()
	rt := newTestService(t, Config{RunRoot: filepath.Join(stateRoot, "runs")})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		AgentID: agents.ResearchAssistantAgentID,
		Message: "please call command",
		Channel: "test",
		UserID:  "user-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	reloaded := newTestService(t, Config{RunRoot: filepath.Join(stateRoot, "runs")})
	agentSessionID := adkSessionID(fsession.BuildAgentSessionID(resp.SessionID, resp.AgentID), "rehydrate-test")
	if restored, _, err := reloaded.hydrateADKSession(context.Background(), "user-1", agentSessionID, resp.AgentID, resp.SessionID); err != nil {
		t.Fatal(err)
	} else if restored != 4 {
		t.Fatalf("restored = %d, want 4", restored)
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
	if events.Len() != 4 {
		t.Fatalf("restored event len = %d, want 4", events.Len())
	}
	if call := events.At(1).Content.Parts[0].FunctionCall; call == nil || call.Name != "command.run" || call.ID == "" {
		t.Fatalf("restored function call = %+v", events.At(1).Content.Parts[0])
	}
	response := events.At(2).Content.Parts[0].FunctionResponse
	if response == nil || response.Name != "command.run" || response.ID == "" || response.Response["status"] != "ok" {
		t.Fatalf("restored function response = %+v", events.At(2).Content.Parts[0])
	}
}

func TestADKSessionDoesNotReuseUnpersistedToolEventsAcrossRuns(t *testing.T) {
	var sawHello bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		userMessage := lastUserMessage(req.Messages)
		body := deepSeekTextResponse("ok")
		switch {
		case lastRole(req.Messages) == "tool":
			body = deepSeekTextResponse("")
		case userMessage == "bad shell":
			body = deepSeekShellRunToolCallResponseWithCommand("hidden-call", `printf 'hidden tool event'; exit 3`)
		case userMessage == "hello":
			sawHello = true
			for _, message := range req.Messages {
				if message.Role == "tool" && strings.Contains(deepseek.ContentText(message.Content), "hidden tool event") {
					t.Fatalf("second run reused unpersisted tool event: %+v", req.Messages)
				}
			}
			body = deepSeekTextResponse("clean hello")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	rt := newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	_, err := rt.RunAgent(context.Background(), TurnRequest{Message: "bad shell", Channel: "test", UserID: "user-1"})
	if err == nil {
		t.Fatal("expected first run to fail with empty final response")
	}
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "hello", Channel: "test", UserID: "user-1"})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !sawHello || resp.FinalResponse != "clean hello" {
		t.Fatalf("second run response = %q sawHello=%v", resp.FinalResponse, sawHello)
	}
}

func TestRepeatedFailedShellCommandIsBlockedOnThirdAttempt(t *testing.T) {
	counterPath := filepath.Join(t.TempDir(), "shell-counter.txt")
	command := fmt.Sprintf("printf run >> %q; printf err >&2; exit 9", counterPath)
	var requests int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		requests++
		body := deepSeekTextResponse("stopped after guard")
		if requests <= 3 {
			body = deepSeekShellRunToolCallResponseWithCommand(fmt.Sprintf("repeat-%d", requests), command)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	rt := newTestService(t, Config{
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		AgentID: agents.ResearchAssistantAgentID,
		Message: "repeat shell",
		Channel: "test",
	})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if len(resp.ToolCalls) != 3 {
		t.Fatalf("tool calls = %d, want 3: %+v", len(resp.ToolCalls), resp.ToolCalls)
	}
	third := resp.ToolCalls[2]
	if third.Error != "repeated identical failed tool command" || third.Output["retryable"] != false {
		t.Fatalf("third tool call = %+v", third)
	}
	content, err := os.ReadFile(counterPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "runrun" {
		t.Fatalf("counter content = %q, want two real executions", string(content))
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
		if event.Payload["final"] != true {
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

func TestRunAgentTracesLLMRequestWhenEnabled(t *testing.T) {
	t.Setenv(llmTraceEnv, "1")
	runRoot := filepath.Join(t.TempDir(), "runs")
	rt := newTestService(t, Config{RunRoot: runRoot})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "trace me", Channel: "test"})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	tracePath := filepath.Join(rt.RunStore().RunDir(resp.RunID), "llm_requests", "001.json")
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("expected llm request trace: %v", err)
	}
	var req deepseek.ChatRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	if req.Model != deepseek.ModelFlash {
		t.Fatalf("trace model = %q", req.Model)
	}
	if len(req.Messages) < 2 {
		t.Fatalf("trace messages = %+v", req.Messages)
	}
	if req.Messages[0].Role != "system" || !strings.Contains(fmt.Sprint(req.Messages[0].Content), "Current Xira agent: xira-assistant") {
		t.Fatalf("trace system message = %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "trace me" {
		t.Fatalf("trace user message = %+v", req.Messages[1])
	}
	if req.Thinking == nil || req.Thinking.Type != "disabled" {
		t.Fatalf("trace thinking = %+v", req.Thinking)
	}
	var tracedEvent bool
	for _, event := range resp.Events {
		if event.Kind == "llm.request_traced" && event.Payload["path"] == "llm_requests/001.json" {
			tracedEvent = true
		}
	}
	if !tracedEvent {
		t.Fatalf("events = %+v, want llm.request_traced", resp.Events)
	}
}

func TestRunAgentStoresRawLLMRequestAndResponseWhenTraceEnabled(t *testing.T) {
	t.Setenv(llmTraceEnv, "1")
	runRoot := filepath.Join(t.TempDir(), "runs")
	rt := newTestService(t, Config{RunRoot: runRoot})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "raw trace me", Channel: "test"})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}

	rawDir := filepath.Join(rt.RunStore().RunDir(resp.RunID), "llm_raw", "001")
	requestBody, err := os.ReadFile(filepath.Join(rawDir, "request.body"))
	if err != nil {
		t.Fatalf("expected raw llm request: %v", err)
	}
	if !strings.Contains(string(requestBody), `"raw trace me"`) || !strings.Contains(string(requestBody), `"messages"`) {
		t.Fatalf("raw request body = %s", requestBody)
	}
	responseBody, err := os.ReadFile(filepath.Join(rawDir, "response.body"))
	if err != nil {
		t.Fatalf("expected raw llm response: %v", err)
	}
	if !strings.Contains(string(responseBody), "fake model response: raw trace me") {
		t.Fatalf("raw response body = %s", responseBody)
	}
	metaData, err := os.ReadFile(filepath.Join(rawDir, "response.meta.json"))
	if err != nil {
		t.Fatalf("expected raw llm response metadata: %v", err)
	}
	var meta struct {
		StatusCode int `json:"status_code"`
	}
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("decode raw response metadata: %v", err)
	}
	if meta.StatusCode != http.StatusOK {
		t.Fatalf("raw response status = %d", meta.StatusCode)
	}
	if len(resp.LLMCalls) != 1 || resp.LLMCalls[0].RawTracePath != "llm_raw/001" {
		t.Fatalf("llm calls = %+v, want raw trace path", resp.LLMCalls)
	}
}

func TestRunAgentRecordsUsageWithoutLLMTrace(t *testing.T) {
	runRoot := filepath.Join(t.TempDir(), "runs")
	stateRoot := filepath.Join(t.TempDir(), "state")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"model":"deepseek-v4-flash",
				"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"usage ok"}}],
				"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
			}`)),
		}, nil
	})}
	rt := newTestService(t, Config{
		RunRoot:        runRoot,
		StateRoot:      stateRoot,
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "usage please", Channel: "test"})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if len(resp.LLMCalls) != 1 {
		t.Fatalf("llm calls = %+v", resp.LLMCalls)
	}
	call := resp.LLMCalls[0]
	if call.UsageSource != "provider" || call.PromptTokens != 11 || call.CompletionTokens != 7 || call.TotalTokens != 18 {
		t.Fatalf("llm call usage = %+v", call)
	}
	stableAgentSessionID := fsession.BuildAgentSessionID(resp.SessionID, resp.AgentID)
	if call.AgentSessionID != stableAgentSessionID {
		t.Fatalf("agent session id = %q, want stable %q", call.AgentSessionID, stableAgentSessionID)
	}
	if call.ADKSessionID == "" || call.ADKSessionID == call.AgentSessionID || !strings.HasPrefix(call.ADKSessionID, stableAgentSessionID+":run:"+resp.RunID+":") {
		t.Fatalf("adk session id = %q, stable agent session id = %q", call.ADKSessionID, call.AgentSessionID)
	}
	if resp.Usage.CallCount != 1 || resp.Usage.TotalTokens != 18 {
		t.Fatalf("usage summary = %+v", resp.Usage)
	}
	if _, err := os.Stat(filepath.Join(rt.RunStore().RunDir(resp.RunID), "llm_requests", "001.json")); !os.IsNotExist(err) {
		t.Fatalf("llm request trace should be absent when %s is disabled: %v", llmTraceEnv, err)
	}
	if _, err := os.Stat(filepath.Join(rt.RunStore().RunDir(resp.RunID), "llm_raw", "001", "request.body")); !os.IsNotExist(err) {
		t.Fatalf("raw llm trace should be absent when %s is disabled: %v", llmTraceEnv, err)
	}
	for _, path := range []string{
		filepath.Join(rt.RunStore().RunDir(resp.RunID), "llm_calls.jsonl"),
		filepath.Join(rt.RunStore().RunDir(resp.RunID), "usage.json"),
		filepath.Join(stateRoot, "usage-ledger.jsonl"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected usage file %s: %v", path, err)
		}
	}
	ledgerData, err := os.ReadFile(filepath.Join(stateRoot, "usage-ledger.jsonl"))
	if err != nil {
		t.Fatalf("read usage ledger: %v", err)
	}
	var ledgerCall LLMCallRecord
	if err := json.Unmarshal(bytes.TrimSpace(ledgerData), &ledgerCall); err != nil {
		t.Fatalf("decode usage ledger: %v\n%s", err, ledgerData)
	}
	if ledgerCall.AgentSessionID != stableAgentSessionID || ledgerCall.ADKSessionID != call.ADKSessionID {
		t.Fatalf("usage ledger call = %+v", ledgerCall)
	}
}

func TestDelegatedChildRunAppendsLLMCallsToUsageLedger(t *testing.T) {
	runRoot := filepath.Join(t.TempDir(), "runs")
	stateRoot := filepath.Join(t.TempDir(), "state")
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		system := ""
		if len(req.Messages) > 0 {
			system = fmt.Sprint(req.Messages[0].Content)
		}
		var body string
		switch {
		case lastRole(req.Messages) == "tool":
			body = deepSeekTextResponse("parent final with child usage")
		case strings.Contains(system, "Current Xira agent: research-assistant"):
			body = deepSeekTextResponse(`{"summary":"child usage recorded","confidence":"high","followup_needed":false}`)
		default:
			body = deepSeekToolCallResponseWithArgs("delegate-usage", "delegate_agent", map[string]any{
				"agent_id": agents.ResearchAssistantAgentID,
				"task":     "produce child usage",
			})
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
	rt := newTestService(t, Config{
		RunRoot:        runRoot,
		StateRoot:      stateRoot,
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "delegate and track usage", Channel: "test"})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "delegate_agent" || resp.ToolCalls[0].Error != "" {
		t.Fatalf("delegate tool call = %+v", resp.ToolCalls)
	}
	childRunID, _ := resp.ToolCalls[0].Output["run_id"].(string)
	if childRunID == "" {
		t.Fatalf("delegate output missing child run id: %+v", resp.ToolCalls[0].Output)
	}
	ledgerData, err := os.ReadFile(filepath.Join(stateRoot, "usage-ledger.jsonl"))
	if err != nil {
		t.Fatalf("read usage ledger: %v", err)
	}
	var childCallFound bool
	var parentCallCount int
	for _, line := range bytes.Split(bytes.TrimSpace(ledgerData), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var call LLMCallRecord
		if err := json.Unmarshal(line, &call); err != nil {
			t.Fatalf("decode usage ledger line: %v\n%s", err, ledgerData)
		}
		if call.RunID == childRunID && call.AgentID == agents.ResearchAssistantAgentID {
			childCallFound = true
		}
		if call.RunID == resp.RunID && call.AgentID == resp.AgentID {
			parentCallCount++
		}
	}
	if !childCallFound {
		t.Fatalf("usage ledger missing child run %q call:\n%s", childRunID, ledgerData)
	}
	if parentCallCount == 0 {
		t.Fatalf("usage ledger missing parent calls:\n%s", ledgerData)
	}
}

func TestWorkspaceModelPolicyControlsDeepSeekRequest(t *testing.T) {
	instance := writeRuntimeFixture(t, "xira-assistant", []string{"chat", "sender"})
	writeFile(t, filepath.Join(instance, "workspace", "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: Default Xira runtime assistant.
model_policy:
  provider: deepseek
  model: deepseek-v4-pro
  stream: true
  temperature: 0
  thinking:
    type: enabled
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Use Xira runtime context and keep responses operational.
`)
	var gotReq deepseek.ChatRequest
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"model":"deepseek-v4-pro","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"policy ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)),
		}, nil
	})}
	rt := newTestService(t, Config{
		ConfigPath:     filepath.Join(instance, "xira.yaml"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "policy", Channel: "test"})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if gotReq.Model != deepseek.ModelPro || !gotReq.Stream {
		t.Fatalf("request model/stream = %q/%v", gotReq.Model, gotReq.Stream)
	}
	if gotReq.Temperature == nil || *gotReq.Temperature != 0 {
		t.Fatalf("temperature = %+v, want explicit zero", gotReq.Temperature)
	}
	if gotReq.Thinking == nil || gotReq.Thinking.Type != "enabled" {
		t.Fatalf("thinking = %+v", gotReq.Thinking)
	}
	if resp.ModelPolicy.Model != deepseek.ModelPro || resp.ModelPolicy.Temperature == nil || *resp.ModelPolicy.Temperature != 0 || resp.ModelPolicy.ThinkingType != "enabled" {
		t.Fatalf("model policy snapshot = %+v", resp.ModelPolicy)
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

func TestConfigDefaultAgentHandlesImplicitEntrypoint(t *testing.T) {
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
		Message: "please call command",
		Channel: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.AgentID != "research-assistant" {
		t.Fatalf("AgentID = %q", resp.AgentID)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "command.run" {
		t.Fatalf("expected command.run tool call: %+v", resp.ToolCalls)
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
		Channel: "xiragarden",
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
	if got := resp.SessionScope.Values["channel"]; got != "channel:xiragarden" {
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
		if lastRole(req.Messages) == "tool" {
			body = deepSeekTextResponse("fake tool final")
		} else {
			userMessage := lastUserMessage(req.Messages)
			lower := strings.ToLower(userMessage)
			if strings.Contains(lower, "shell") {
				body = deepSeekShellRunToolCallResponseWithCommand("call-1", `printf 'hello from Xira shell'`)
			} else if strings.Contains(lower, "command") {
				body = deepSeekCommandRunToolCallResponse()
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

func adkToolNames(tools []adktool.Tool) map[string]bool {
	out := map[string]bool{}
	for _, tool := range tools {
		out[tool.Name()] = true
	}
	return out
}

func eventKinds(events []RuntimeEvent) []string {
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func findEvent(events []RuntimeEvent, kind string) (RuntimeEvent, bool) {
	for _, event := range events {
		if event.Kind == kind {
			return event, true
		}
	}
	return RuntimeEvent{}, false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertRejectedChildRunNotCreated(t *testing.T, rt *Service, output map[string]any) {
	t.Helper()
	childRunID, _ := output["run_id"].(string)
	if childRunID == "" {
		t.Fatalf("rejected output missing child run id: %+v", output)
	}
	if _, err := os.Stat(rt.RunStore().RunDir(childRunID)); err == nil {
		t.Fatalf("rejected delegation created child run dir %q", childRunID)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat child run dir: %v", err)
	}
}

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode %s: %v\n%s", path, err, data)
	}
	return out
}

func lastRole(messages []deepseek.Message) string {
	if len(messages) == 0 {
		return ""
	}
	return messages[len(messages)-1].Role
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

func deepSeekCommandRunToolCallResponse() string {
	return deepSeekToolCallResponseWithArgs("call-1", "command_run", map[string]any{
		"program": "printf",
		"args":    []string{"hello from Xira command"},
	})
}

func deepSeekShellRunToolCallResponseWithCommand(id, command string) string {
	return deepSeekShellRunToolCallResponseWithArgs(id, map[string]any{"command": command})
}

func deepSeekShellRunToolCallResponseWithArgs(id string, args map[string]any) string {
	return deepSeekToolCallResponseWithArgs(id, "shell_run", args)
}

func deepSeekToolCallResponseWithArgs(id, name string, args map[string]any) string {
	data, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4-flash",
		"choices": []map[string]any{{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": mustMarshalString(args),
					},
				}},
			},
		}},
	})
	return string(data)
}

func mustMarshalString(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func writeRuntimeFixture(t *testing.T, defaultAgentID string, xiraSessionDimensions []string) string {
	t.Helper()
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: `+defaultAgentID+`
run_root: .xira/runs
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
