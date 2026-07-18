package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/model/deepseek"
	rtools "github.com/xiramesh/xira/internal/tools"
)

func TestIntentionalSilenceReasonContract(t *testing.T) {
	tests := []struct {
		name    string
		records []ToolCallRecord
		want    string
		ok      bool
	}{
		{name: "none"},
		{name: "explicit", records: []ToolCallRecord{{Name: finishSilentToolName, Output: map[string]any{"status": "accepted"}}}, want: finishSilentToolName, ok: true},
		{name: "explicit after successful work", records: []ToolCallRecord{
			{Name: "update_memory", Output: map[string]any{"updated": true}},
			{Name: finishSilentToolName, Output: map[string]any{"status": "accepted"}},
		}, want: finishSilentToolName, ok: true},
		{name: "explicit call error", records: []ToolCallRecord{
			{Name: finishSilentToolName, Error: "rejected", Output: map[string]any{"status": "rejected"}},
		}},
		{name: "failed tool cannot be hidden", records: []ToolCallRecord{
			{Name: "update_memory", Error: "write failed"},
			{Name: finishSilentToolName, Output: map[string]any{"status": "accepted"}},
		}},
		{name: "failed status cannot be hidden", records: []ToolCallRecord{
			{Name: "some_tool", Output: map[string]any{"status": "failed"}},
			{Name: finishSilentToolName, Output: map[string]any{"status": "accepted"}},
		}},
		{name: "rejected status cannot be hidden", records: []ToolCallRecord{
			{Name: "some_tool", Output: map[string]any{"status": "rejected"}},
			{Name: finishSilentToolName, Output: map[string]any{"status": "accepted"}},
		}},
		{name: "owner notification remains separate", records: []ToolCallRecord{
			{Name: notifyOwnerToolName, Output: map[string]any{"status": "sent"}},
		}, want: "notify_owner_sent", ok: true},
		{name: "explicit wins when both succeeded", records: []ToolCallRecord{
			{Name: notifyOwnerToolName, Output: map[string]any{"status": "sent"}},
			{Name: finishSilentToolName, Output: map[string]any{"status": "accepted"}},
		}, want: finishSilentToolName, ok: true},
		{name: "owner retry remains valid", records: []ToolCallRecord{
			{Name: notifyOwnerToolName, Error: "temporary", Output: map[string]any{"status": "failed"}},
			{Name: notifyOwnerToolName, Output: map[string]any{"status": "sent"}},
		}, want: "notify_owner_sent", ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := intentionalSilenceReason(tc.records)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("intentionalSilenceReason() = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestVerifyRunOutcomeContract(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	checks := []string{"final_response_non_empty"}
	if got := rt.verifyRunOutcome("public reply", nil, checks); got.Status != "passed" || len(got.Checks) != 1 || got.Checks[0] != "final_response_non_empty" {
		t.Fatalf("non-empty result = %+v", got)
	}
	silent := []ToolCallRecord{{Name: finishSilentToolName, Output: map[string]any{"status": "accepted"}}}
	if got := rt.verifyRunOutcome("", silent, checks); got.Status != "passed" || len(got.Checks) != 1 || got.Checks[0] != finishSilentToolName {
		t.Fatalf("silent result = %+v", got)
	}
	if got := rt.verifyRunOutcome("", nil, checks); got.Status != "failed" {
		t.Fatalf("accidental empty result = %+v", got)
	}
}

func TestFinishSilentToolIsSealedAndRegisteredForProductionADK(t *testing.T) {
	if strings.Contains(finishSilentToolDescription, notifyOwnerToolName) {
		t.Fatalf("finish_silent description depends on another tool: %q", finishSilentToolDescription)
	}
	if !strings.Contains(finishSilentToolGuidance, "all required work") || !strings.Contains(finishSilentToolGuidance, "failed or rejected action") {
		t.Fatalf("finish_silent Guidance misses its independent use boundary: %q", finishSilentToolGuidance)
	}
	schema := finishSilentInputSchema()
	if schema.Type != "object" || len(schema.Properties) != 0 || len(schema.Required) != 0 || schema.AdditionalProperties == nil {
		t.Fatalf("finish_silent schema must be a closed empty object: %+v", schema)
	}
	for _, def := range runtimeNativeToolDefinitions(agents.BuiltinXiraAssistant()) {
		if def.Function.Name == finishSilentToolName {
			t.Fatal("dead native definitions must not advertise finish_silent")
		}
	}
	svc := &Service{}
	tools, err := svc.runtimeADKTools(context.Background(), agents.BuiltinXiraAssistant(), func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {}, func(ToolCallRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	if !adkToolNames(tools)[finishSilentToolName] {
		t.Fatalf("ADK tools missing %s: %+v", finishSilentToolName, adkToolNames(tools))
	}
	disabled, err := svc.adkTools(contextWithRuntimeNativeToolsDisabled(context.Background()), agents.BuiltinXiraAssistant(), func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {}, func(ToolCallRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	if adkToolNames(disabled)[finishSilentToolName] {
		t.Fatal("finish_silent must be absent when runtime-native tools are disabled")
	}
	excluded, err := svc.adkTools(contextWithRuntimeToolAllowlist(context.Background(), []string{"write_file"}), agents.BuiltinXiraAssistant(), func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {}, func(ToolCallRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	if adkToolNames(excluded)[finishSilentToolName] {
		t.Fatal("finish_silent must respect an explicit runtime tool allowlist")
	}
	included, err := svc.adkTools(contextWithRuntimeToolAllowlist(context.Background(), []string{finishSilentToolName}), agents.BuiltinXiraAssistant(), func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {}, func(ToolCallRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	if names := adkToolNames(included); len(names) != 1 || !names[finishSilentToolName] {
		t.Fatalf("finish_silent explicit allowlist = %+v", names)
	}
}

func TestFinishSilentToolCallFailsClosedAndAuditsRejection(t *testing.T) {
	missingRuntime := finishSilentToolCall(context.Background(), "", nil)
	if missingRuntime.ID == "" || missingRuntime.Error == "" || missingRuntime.Output["status"] != "rejected" {
		t.Fatalf("missing runtime record = %+v", missingRuntime)
	}
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{})
	extraArgs := finishSilentToolCall(ctx, "silent-extra", map[string]any{"reason": "model-controlled"})
	if extraArgs.ID != "silent-extra" || extraArgs.Error == "" || extraArgs.Output["status"] != "rejected" {
		t.Fatalf("extra args record = %+v", extraArgs)
	}
	var eventPayload map[string]any
	var auditAllowed bool
	recordFinishSilentOutcome(extraArgs,
		func(_, _, _ string, payload map[string]any) { eventPayload = payload },
		func(_, _ string, allowed bool, _ string, _ map[string]any) { auditAllowed = allowed },
	)
	if eventPayload["error"] == "" || eventPayload["tool_call_id"] != "silent-extra" || auditAllowed {
		t.Fatalf("event=%+v auditAllowed=%v", eventPayload, auditAllowed)
	}
	if _, leaked := eventPayload["reason"]; leaked {
		t.Fatalf("model-controlled reason leaked into event: %+v", eventPayload)
	}
}

func TestADKFinishSilentCompletesWithoutFinalOrOutbound(t *testing.T) {
	rt := intentionalSilenceTestService(t, []string{
		deepSeekToolCallResponseWithArgs("silent-1", finishSilentToolName, map[string]any{}),
		emptyDeepSeekFinalResponse(),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "decide whether anything needs saying",
		Context: channel.NewInboundContext("test", "sender-a", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if resp.Status != "completed" || resp.FinalResponse != "" || resp.VerificationResult.Status != "passed" {
		t.Fatalf("response = %+v", resp)
	}
	if len(resp.VerificationResult.Checks) != 1 || resp.VerificationResult.Checks[0] != finishSilentToolName {
		t.Fatalf("verification = %+v", resp.VerificationResult)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != finishSilentToolName || resp.ToolCalls[0].Output["status"] != "accepted" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	evt, ok := findEvent(resp.Events, "adk.intentional_silence")
	if !ok || evt.Payload["reason"] != finishSilentToolName {
		t.Fatalf("intentional silence event = %+v, found=%v", evt, ok)
	}
	for _, evt := range resp.Events {
		if evt.Kind == "assistant.final" {
			t.Fatalf("silent run emitted assistant.final: %+v", evt)
		}
	}
	var foundAudit bool
	for _, audit := range resp.AuditEvents {
		if audit.Action == finishSilentToolName {
			foundAudit = true
			if !audit.Allowed || audit.Meta["tool_call_id"] != "silent-1" {
				t.Fatalf("finish_silent audit = %+v", audit)
			}
			if _, leaked := audit.Meta["reasoning"]; leaked {
				t.Fatalf("finish_silent audit leaked model reasoning: %+v", audit)
			}
		}
	}
	if !foundAudit {
		t.Fatalf("missing finish_silent audit: %+v", resp.AuditEvents)
	}
}

func TestADKJSONFormatKeepsIntentionalSilenceSuccess(t *testing.T) {
	instance := writeRuntimeFixture(t, agents.DefaultAgentID, []string{"chat", "sender"})
	writeFile(t, filepath.Join(instance, "workspace", "agents", agents.DefaultAgentID, "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: JSON intentional silence contract fixture.
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
  stream: false
  format: json
verification:
  default_checks:
    - final_response_non_empty
---
Use finish_silent when the user explicitly requests no public reply.
`)
	responses := []string{
		deepSeekToolCallResponseWithArgs("silent-json", finishSilentToolName, map[string]any{}),
		emptyDeepSeekFinalResponse(),
	}
	var requests []deepseek.ChatRequest
	index := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var request deepseek.ChatRequest
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			return nil, err
		}
		requests = append(requests, request)
		if index >= len(responses) {
			return nil, errors.New("unexpected model call")
		}
		body := responses[index]
		index++
		return deepSeekHTTPResponse(body), nil
	})}
	rt := newTestService(t, Config{
		ConfigPath: filepath.Join(instance, "xira.yaml"),
		DeepSeekClient: deepseek.New(
			deepseek.WithBaseURLForTest("http://deepseek.test"),
			deepseek.WithAPIKey("test-key"),
			deepseek.WithHTTPClient(client),
		),
	})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Complete silently with no public reply.",
		Context: channel.NewInboundContext("test", "sender-json-silent", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != "completed" || resp.FinalResponse != "" || resp.VerificationResult.Status != "passed" {
		t.Fatalf("response = %+v, want intentional silence success", resp)
	}
	if len(resp.VerificationResult.Checks) != 1 || resp.VerificationResult.Checks[0] != finishSilentToolName {
		t.Fatalf("verification = %+v, want finish_silent seal", resp.VerificationResult)
	}
	if len(requests) != 2 {
		t.Fatalf("model requests = %d, want tool call and empty-final follow-up", len(requests))
	}
	for i, request := range requests {
		if request.ResponseFormat == nil || request.ResponseFormat.Type != "json_object" {
			t.Fatalf("request %d response format = %+v, want json_object", i+1, request.ResponseFormat)
		}
	}
	for _, event := range resp.Events {
		if event.Kind == "assistant.final" || event.Kind == "adk.invalid_json_final" {
			t.Fatalf("intentional silence emitted incompatible event: %+v", event)
		}
	}
}

func TestADKMemoryThenFinishSilentUsesSelectedScope(t *testing.T) {
	for _, scope := range []string{"sender", "agent"} {
		t.Run(scope, func(t *testing.T) {
			rt := intentionalSilenceTestService(t, []string{
				deepSeekToolCallResponseWithArgs("memory-1", "update_memory", map[string]any{
					"scope": scope, "key": "silent-work", "content": scope + " memory persisted silently",
				}),
				deepSeekToolCallResponseWithArgs("silent-1", finishSilentToolName, map[string]any{}),
				emptyDeepSeekFinalResponse(),
			})
			resp, err := rt.RunAgent(context.Background(), TurnRequest{
				Message: "remember this without replying",
				Context: channel.NewInboundContext("test", "sender-memory", nil),
			})
			if err != nil || resp.Status != "completed" || resp.FinalResponse != "" {
				t.Fatalf("response=%+v err=%v", resp, err)
			}
			var path string
			if scope == "sender" {
				path = rtools.MemoryPath(rt.stateDir, "sender-memory")
			} else {
				path = rtools.AgentMemoryPath(rt.stateDir, agents.DefaultAgentID)
			}
			entries, loadErr := rtools.LoadMemories(path)
			if loadErr != nil || len(entries) != 1 || !strings.Contains(entries[0].Content, scope) {
				t.Fatalf("%s memory = %+v, err=%v", scope, entries, loadErr)
			}
		})
	}
}

func TestADKFinishSilentCannotHideFailedTool(t *testing.T) {
	rt := intentionalSilenceTestService(t, []string{
		deepSeekToolCallResponseWithArgs("command-bad", "command.run", map[string]any{"program": "false"}),
		deepSeekToolCallResponseWithArgs("silent-1", finishSilentToolName, map[string]any{}),
		emptyDeepSeekFinalResponse(),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "hide the failed tool",
		Context: channel.NewInboundContext("test", "sender-a", nil),
	})
	if err == nil || resp.Status != "failed" || resp.VerificationResult.Status != "failed" {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Error == "" {
		t.Fatalf("failed tool evidence lost: %+v", resp.ToolCalls)
	}
}

func TestADKEmptyFinalWithoutExplicitSilenceStillFails(t *testing.T) {
	rt := intentionalSilenceTestService(t, []string{emptyDeepSeekFinalResponse()})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "accidentally return nothing",
		Context: channel.NewInboundContext("test", "sender-a", nil),
	})
	if err == nil || resp.Status != "failed" || !strings.Contains(err.Error(), "empty final") {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
}

func intentionalSilenceTestService(t *testing.T, responses []string) *Service {
	t.Helper()
	index := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if index >= len(responses) {
			return nil, errors.New("unexpected model call")
		}
		body := responses[index]
		index++
		return deepSeekHTTPResponse(body), nil
	})}
	return newTestService(t, Config{
		StateDir: t.TempDir(),
		DeepSeekClient: deepseek.New(
			deepseek.WithBaseURLForTest("http://deepseek.test"),
			deepseek.WithAPIKey("test-key"),
			deepseek.WithHTTPClient(client),
		),
	})
}

func emptyDeepSeekFinalResponse() string {
	return `{"model":"deepseek-v4-flash","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null}}]}`
}
