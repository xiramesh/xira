package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

		"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
	rtools "github.com/xiramesh/xira/internal/tools"
)

func TestRequireConfirmationCreatesActionSnapshot(t *testing.T) {
	workspace := t.TempDir()
	targetPath := filepath.Join(workspace, "approved.txt")
	rt := newConfirmationTestService(t, workspace, map[string]any{
		"path":    "approved.txt",
		"content": "write after approval",
	})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "write a file", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman {
		t.Fatalf("status = %q", resp.Status)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("write_file executed before approval, stat err=%v", err)
	}
	if len(resp.HumanRequests) != 1 {
		t.Fatalf("human_requests = %+v", resp.HumanRequests)
	}
	req := resp.HumanRequests[0]
	if req.Kind != humanrequest.RequestApproval || req.Source != "runtime_tool_gate" {
		t.Fatalf("human request = %+v", req)
	}
	if req.ActionSnapshot == nil || req.ActionSnapshot.ToolName != "write_file" {
		t.Fatalf("action snapshot = %+v", req.ActionSnapshot)
	}
	if req.ActionSnapshot.Arguments["path"] != "approved.txt" || req.ActionSnapshot.Arguments["content"] != "write after approval" {
		t.Fatalf("action snapshot args = %+v", req.ActionSnapshot.Arguments)
	}
	if req.ActionSnapshot.RunID != resp.RunID || req.ActionSnapshot.AgentID != resp.AgentID || req.ActionSnapshot.SessionID != resp.SessionID || req.ActionSnapshot.ToolCallID != "write-confirm-call" {
		t.Fatalf("action snapshot identity = %+v, run=%s agent=%s session=%s", req.ActionSnapshot, resp.RunID, resp.AgentID, resp.SessionID)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "write_file" || resp.ToolCalls[0].Output["status"] != StatusWaitingHuman {
		t.Fatalf("waiting tool call = %+v", resp.ToolCalls)
	}
}

func TestRequireConfirmationReturnsWaitingHuman(t *testing.T) {
	rt := newConfirmationTestService(t, t.TempDir(), map[string]any{
		"path":    "waiting.txt",
		"content": "write after approval",
	})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "write a file", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman {
		t.Fatalf("status = %q, want %q", resp.Status, StatusWaitingHuman)
	}
	if resp.Interrupt == nil || resp.Interrupt.Status != StatusWaitingHuman {
		t.Fatalf("interrupt = %+v", resp.Interrupt)
	}
	if len(resp.HumanRequests) != 1 || len(resp.ToolCalls) != 1 {
		t.Fatalf("human_requests=%+v tool_calls=%+v", resp.HumanRequests, resp.ToolCalls)
	}
	if resp.ToolCalls[0].Output["status"] != StatusWaitingHuman {
		t.Fatalf("tool output = %+v", resp.ToolCalls[0].Output)
	}
}

func TestValidateActionSnapshotDigestRejectsUnmarshalableArguments(t *testing.T) {
	err := validateActionSnapshotDigest(&humanrequest.ActionSnapshot{
		Arguments:   map[string]any{"bad": func() {}},
		ContextHash: "sha256:expected",
	})
	if err == nil || !strings.Contains(err.Error(), "marshal snapshot arguments") {
		t.Fatalf("validateActionSnapshotDigest error = %v, want marshal failure", err)
	}
}

func TestNativeRequireConfirmationCreatesActionSnapshot(t *testing.T) {
	workspace := t.TempDir()
	targetPath := filepath.Join(workspace, "native-approved.txt")
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("native-write-confirm-call", "write_file", map[string]any{
				"path":    "native-approved.txt",
				"content": "native write after approval",
			}))),
		}, nil
	})}
	rt := newConfirmationRuntimeWithClient(t, workspace, client)
	runID := "native-confirm-run"
	if err := rt.RunStore().InitRun(runID); err != nil {
		t.Fatal(err)
	}
	profile := agents.BuiltinXiraAssistant()
	collector := newRuntimeSuspendCollector()
	ctx := contextWithRuntimeSuspendCollector(context.Background(), collector)
	ctx = contextWithToolTrace(ctx, runID)
	ctx = rtools.WithRunDir(ctx, rt.RunStore().RunDir(runID))
	ctx = contextWithRunExecution(ctx, runExecutionContext{
		Base: runtimeEventBase{
			RunID:                 runID,
			AgentID:               profile.ID,
			ConversationSessionID: "session-native-confirm",
			TraceID:               runID,
		},
		Profile: profile,
		Request: TurnRequest{Message: "write native", SessionID: "session-native-confirm"},
	})
	ctx = rt.withLLMInstrumentation(ctx, llmInstrumentationInput{
		RunID:   runID,
		AgentID: profile.ID,
		Pricing: rt.pricing,
	}, func(string, string, string, map[string]any) {}, func(LLMCallRecord) {})

	final, toolCalls, err := rt.generateNativeDeepSeek(ctx, profile, "native instruction", TurnRequest{
		Message:   "write native",
		SessionID: "session-native-confirm",
	}, func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {})
	if err != nil {
		t.Fatalf("generateNativeDeepSeek() error = %v", err)
	}
	if strings.TrimSpace(final) != "" {
		t.Fatalf("final = %q, want suspended empty final", final)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("native write_file executed before approval, stat err=%v", err)
	}
	if len(toolCalls) != 1 || toolCalls[0].Name != "write_file" || toolCalls[0].Output["status"] != StatusWaitingHuman {
		t.Fatalf("native waiting tool calls = %+v", toolCalls)
	}
	if !collector.HasInterrupt() || len(collector.Interrupt().HumanRequests) != 1 {
		t.Fatalf("collector interrupt = %+v", collector.Interrupt())
	}
	req := collector.Interrupt().HumanRequests[0]
	if req.ActionSnapshot == nil || req.ActionSnapshot.ToolName != "write_file" || req.ActionSnapshot.ToolCallID != "native-write-confirm-call" {
		t.Fatalf("native action snapshot = %+v", req.ActionSnapshot)
	}
}

func TestApproveReplaysSnapshotExactlyOnce(t *testing.T) {
	workspace := t.TempDir()
	targetPath := filepath.Join(workspace, "approved.txt")
	rt := newConfirmationTestService(t, workspace, map[string]any{
		"path":    "approved.txt",
		"content": "write once",
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "write a file", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	requestID := resp.HumanRequests[0].ID

	resolved, err := rt.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseApprove,
		Actor:   "tester",
		Message: "approved",
	})
	if err != nil {
		t.Fatalf("ResolveHumanRequest approve: %v", err)
	}
	if resolved.Replay == nil || resolved.Replay.Status != humanrequest.ReplayCompleted {
		t.Fatalf("replay = %+v", resolved.Replay)
	}
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read approved file: %v", err)
	}
	if string(data) != "write once" {
		t.Fatalf("approved file = %q", data)
	}

	_, err = rt.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseApprove,
		Actor:   "tester",
		Message: "approved again",
	})
	if err == nil {
		t.Fatal("second approval succeeded; expected conflict")
	}
	data, err = os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "write once" {
		t.Fatalf("second approval changed file: %q", data)
	}
}

func TestApproveReplaysSnapshotWithLeadingNewlineContent(t *testing.T) {
	workspace := t.TempDir()
	rt := newConfirmationTestService(t, workspace, map[string]any{
		"path":    "leading-newline.txt",
		"content": "\nstarts after blank line",
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "write a file", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	requestID := resp.HumanRequests[0].ID
	if _, err := rt.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseApprove,
		Actor:   "tester",
		Message: "approved",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest approve leading newline: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "leading-newline.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "starts after blank line" {
		t.Fatalf("canonical approved file = %q", data)
	}
}

func TestApprovedToolReplayDisablesRuntimeNativeTools(t *testing.T) {
	workspace := t.TempDir()
	var sawApprovedReplay bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		if strings.Contains(lastUserMessage(req.Messages), "approved tool output") {
			sawApprovedReplay = true
			if len(req.Tools) != 0 {
				t.Fatalf("approved-tool replay exposed tools: %+v", req.Tools)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(deepSeekTextResponse("approved tool output final"))),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("write-confirm-call", "write_file", map[string]any{
				"path":    "approved.txt",
				"content": "write once",
			}))),
		}, nil
	})}
	rt := newConfirmationRuntimeWithClient(t, workspace, client)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "write a file", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	requestID := resp.HumanRequests[0].ID

	_, err = rt.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseApprove,
		Actor:   "tester",
		Message: "approved",
	})
	if err != nil {
		t.Fatalf("ResolveHumanRequest approve: %v", err)
	}
	if !sawApprovedReplay {
		t.Fatal("fake model did not receive approved-tool replay turn")
	}
	pending, err := rt.ListHumanRequests(context.Background(), humanrequest.StatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("approved-tool replay created new pending human requests: %+v", pending)
	}
}

func TestNativeApprovedToolReplayRejectsHumanRequestToolCall(t *testing.T) {
	workspace := t.TempDir()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		if len(req.Tools) != 0 {
			t.Fatalf("disabled native replay exposed tools: %+v", req.Tools)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("native-replay-human-call", "human_request", map[string]any{
				"kind":     "approval",
				"question": "should not create a new pending request",
			}))),
		}, nil
	})}
	rt := newConfirmationRuntimeWithClient(t, workspace, client)
	runID := "native-replay-disabled"
	if err := rt.RunStore().InitRun(runID); err != nil {
		t.Fatal(err)
	}
	profile := agents.BuiltinXiraAssistant()
	profile.Permissions.Tools = nil
	profile.Skills = nil
	collector := newRuntimeSuspendCollector()
	ctx := contextWithRuntimeNativeToolsDisabled(context.Background())
	ctx = contextWithRuntimeSuspendCollector(ctx, collector)
	ctx = contextWithToolTrace(ctx, runID)
	ctx = rtools.WithRunDir(ctx, rt.RunStore().RunDir(runID))
	ctx = contextWithRunExecution(ctx, runExecutionContext{
		Base: runtimeEventBase{
			RunID:                 runID,
			AgentID:               profile.ID,
			ConversationSessionID: "session-native-replay-disabled",
			TraceID:               runID,
		},
		Profile: profile,
		Request: TurnRequest{Message: "approved tool output", SessionID: "session-native-replay-disabled"},
	})
	ctx = rt.withLLMInstrumentation(ctx, llmInstrumentationInput{
		RunID:   runID,
		AgentID: profile.ID,
		Pricing: rt.pricing,
	}, func(string, string, string, map[string]any) {}, func(LLMCallRecord) {})

	final, toolCalls, err := rt.generateNativeDeepSeek(ctx, profile, "native instruction", TurnRequest{
		Message:   "approved tool output",
		SessionID: "session-native-replay-disabled",
	}, func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {})
	if err != nil {
		t.Fatalf("generateNativeDeepSeek() error = %v", err)
	}
	if strings.TrimSpace(final) != "" {
		t.Fatalf("final = %q, want empty after rejected undeclared tool", final)
	}
	if len(toolCalls) != 1 || toolCalls[0].Name != "human_request" || !strings.Contains(toolCalls[0].Error, "not allowed") {
		t.Fatalf("native replay human.request should be rejected tool call: %+v", toolCalls)
	}
	if collector.HasInterrupt() {
		t.Fatalf("disabled native replay created interrupt: %+v", collector.Interrupt())
	}
	pending, err := rt.ListHumanRequests(context.Background(), humanrequest.StatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("disabled native replay created pending human requests: %+v", pending)
	}
}

func TestDenyDoesNotReplaySnapshot(t *testing.T) {
	workspace := t.TempDir()
	targetPath := filepath.Join(workspace, "denied.txt")
	rt := newConfirmationTestService(t, workspace, map[string]any{
		"path":    "denied.txt",
		"content": "should not write",
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "write a file", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := rt.ResolveHumanRequest(context.Background(), resp.HumanRequests[0].ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseDeny,
		Actor:   "tester",
		Message: "denied",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Replay == nil || resolved.Replay.Status != humanrequest.ReplayDenied {
		t.Fatalf("replay = %+v", resolved.Replay)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("denied snapshot executed, stat err=%v", err)
	}
	run, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if status := confirmedToolCallStatus(run, "write-confirm-call"); status != "denied" {
		t.Fatalf("denied tool call status = %q tool_calls=%+v", status, run.ToolCalls)
	}
}

func TestCancelDoesNotReplayAndMaterializesCanceledOutput(t *testing.T) {
	workspace := t.TempDir()
	targetPath := filepath.Join(workspace, "canceled.txt")
	rt := newConfirmationTestService(t, workspace, map[string]any{
		"path":    "canceled.txt",
		"content": "should not write",
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "write a file", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := rt.ResolveHumanRequest(context.Background(), resp.HumanRequests[0].ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseCancel,
		Actor:   "tester",
		Message: "canceled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Replay == nil || resolved.Replay.Status != humanrequest.ReplayCanceled {
		t.Fatalf("replay = %+v", resolved.Replay)
	}
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Fatalf("canceled snapshot executed, stat err=%v", err)
	}
	run, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if status := confirmedToolCallStatus(run, "write-confirm-call"); status != "canceled" {
		t.Fatalf("canceled tool call status = %q tool_calls=%+v", status, run.ToolCalls)
	}
	if run.Status != "failed" || run.Metadata["error_type"] != "canceled" {
		t.Fatalf("run after cancel = %+v", run)
	}
}

func confirmedToolCallStatus(run TurnResponse, toolCallID string) string {
	for _, rec := range run.ToolCalls {
		if rec.ID == toolCallID {
			return anyString(rec.Output["status"])
		}
	}
	return ""
}

func TestReplayBypassesOnlyConfirmationGate(t *testing.T) {
	workspace := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "outside.txt")
	rt := newConfirmationTestService(t, workspace, map[string]any{
		"path":    outsidePath,
		"content": "must not escape workspace",
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "write outside", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	requestID := resp.HumanRequests[0].ID

	_, err = rt.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseApprove,
		Actor:   "tester",
		Message: "approved",
	})
	if err == nil || !strings.Contains(err.Error(), "within workspace") {
		t.Fatalf("ResolveHumanRequest outside replay error = %v, want workspace policy failure", err)
	}
	if _, err := os.Stat(outsidePath); !os.IsNotExist(err) {
		t.Fatalf("outside replay wrote file, stat err=%v", err)
	}
	stored, err := rt.GetHumanRequest(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Replay == nil || stored.Replay.Status != humanrequest.ReplayFailed {
		t.Fatalf("stored replay = %+v, want failed", stored.Replay)
	}
	if stored.Replay.Error == "" || !strings.Contains(stored.Replay.Error, "within workspace") {
		t.Fatalf("stored replay error = %q", stored.Replay.Error)
	}
}

func TestReplayRejectsChangedToolArgs(t *testing.T) {
	workspace := t.TempDir()
	rt := newConfirmationTestService(t, workspace, map[string]any{
		"path":    "original.txt",
		"content": "original content",
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "write original", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	requestID := resp.HumanRequests[0].ID
	requestPath := filepath.Join(rt.StateRoot(), "workspaces", rt.WorkspaceKey(), "human-requests", requestID+".yaml")
	data, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), "content: original content", "content: tampered content", 1)
	if tampered == string(data) {
		t.Fatalf("test fixture did not find snapshot content in %s:\n%s", requestPath, string(data))
	}
	if err := os.WriteFile(requestPath, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = rt.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseApprove,
		Actor:   "tester",
		Message: "approved",
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot arguments changed") {
		t.Fatalf("ResolveHumanRequest tampered snapshot error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "original.txt")); !os.IsNotExist(err) {
		t.Fatalf("original file written despite tamper, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "tampered.txt")); !os.IsNotExist(err) {
		t.Fatalf("tampered file written, stat err=%v", err)
	}
	stored, err := rt.GetHumanRequest(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Replay == nil || stored.Replay.Status != humanrequest.ReplayFailed || !strings.Contains(stored.Replay.Error, "snapshot arguments changed") {
		t.Fatalf("stored replay after tamper = %+v", stored.Replay)
	}
}

func TestReplayRunningLeasePreventsConcurrentExecution(t *testing.T) {
	workspace := t.TempDir()
	targetPath := filepath.Join(workspace, "approved.txt")
	rt := newConfirmationTestService(t, workspace, map[string]any{
		"path":    "approved.txt",
		"content": "write once under concurrent replay",
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "write concurrently", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	requestID := resp.HumanRequests[0].ID
	resolved, err := rt.humanRequests.Resolve(context.Background(), humanrequest.ResolveRequest{
		WorkspaceKey: rt.WorkspaceKey(),
		RequestID:    requestID,
		Kind:         humanrequest.ResponseApprove,
		Actor:        "tester",
		Message:      "approved",
	})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := rt.replayApprovedActionSnapshot(context.Background(), resolved)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	var successCount, conflictCount int
	for err := range errs {
		switch {
		case err == nil:
			successCount++
		case errors.Is(err, humanrequest.ErrConflict):
			conflictCount++
		default:
			t.Fatalf("unexpected replay error = %v", err)
		}
	}
	if successCount != 1 || conflictCount != 1 {
		t.Fatalf("success=%d conflict=%d, want one each", successCount, conflictCount)
	}
	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("read replay target: %v", err)
	}
	if string(data) != "write once under concurrent replay" {
		t.Fatalf("target content = %q", data)
	}
	stored, err := rt.GetHumanRequest(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	var started, completed int
	for _, audit := range stored.Audit {
		switch audit.Action {
		case "human_request.replay_started":
			started++
		case "human_request.replay_completed":
			completed++
		}
	}
	if started != 1 || completed != 1 {
		t.Fatalf("audit started=%d completed=%d audit=%+v", started, completed, stored.Audit)
	}
}

func newConfirmationTestService(t *testing.T, workspace string, args map[string]any) *Service {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		if lastRole(req.Messages) == "tool" {
			t.Fatalf("model called after confirmation interrupt")
		}
		if strings.Contains(lastUserMessage(req.Messages), "approved tool output") {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(deepSeekTextResponse("approved tool output final"))),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("write-confirm-call", "write_file", args))),
		}, nil
	})}
	return newConfirmationRuntimeWithClient(t, workspace, client)
}

func newConfirmationRuntimeWithClient(t *testing.T, workspace string, client *http.Client) *Service {
	t.Helper()
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: Default Xira runtime assistant.
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
tools:
  - write_file
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Use runtime tools carefully.
`)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), `# Soul

Direct.
`)
	return newTestService(t, Config{
		WorkspaceRoot:  workspace,
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		StateRoot:      filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
}
