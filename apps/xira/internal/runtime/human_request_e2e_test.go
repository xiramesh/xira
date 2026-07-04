package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

func TestE2EDirectHumanRequestAnswerResumesRun(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		if strings.Contains(lastUserMessage(req.Messages), "Use the Tuesday window.") {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(deepSeekTextResponse("Final plan uses the Tuesday window."))),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("direct-human-call", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Which deployment window should I use?",
			}))),
		}, nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "ask a human directly", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || len(resp.HumanRequests) != 1 {
		t.Fatalf("waiting response = %+v", resp)
	}
	if _, err := rt.ResolveHumanRequest(context.Background(), resp.HumanRequests[0].ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseAnswer,
		Actor:   "tester",
		Message: "Use the Tuesday window.",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest answer: %v", err)
	}
	resumed, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "completed" || !strings.Contains(resumed.FinalResponse, "Tuesday window") {
		t.Fatalf("resumed run = %+v", resumed)
	}
}

func TestE2EDirectHumanRequestApproveAndResume(t *testing.T) {
	var modelCalls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		modelCalls++
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		if strings.Contains(lastUserMessage(req.Messages), "Human approved the request.") {
			return deepSeekHTTPResponse(deepSeekTextResponse("Approved direct HITL resume complete.")), nil
		}
		return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("direct-approve-call", "human_request", map[string]any{
			"kind":     "approval",
			"question": "Approve direct resume?",
		})), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "ask direct approval", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || len(resp.HumanRequests) != 1 {
		t.Fatalf("waiting response = %+v", resp)
	}
	if _, err := rt.ResolveHumanRequest(context.Background(), resp.HumanRequests[0].ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseApprove,
		Actor:   "tester",
		Message: "approved",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest approve: %v", err)
	}
	resumed, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "completed" || !strings.Contains(resumed.FinalResponse, "Approved direct HITL") {
		t.Fatalf("resumed run = %+v", resumed)
	}
	if modelCalls != 2 {
		t.Fatalf("model calls = %d, want initial wait + resume", modelCalls)
	}
}

func TestE2EDirectHumanRequestDeny(t *testing.T) {
	var modelCalls int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		modelCalls++
		return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("direct-deny-call", "human_request", map[string]any{
			"kind":     "approval",
			"question": "Approve direct deny?",
		})), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "ask direct deny", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if _, err := rt.ResolveHumanRequest(context.Background(), resp.HumanRequests[0].ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseDeny,
		Actor:   "tester",
		Message: "denied",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest deny: %v", err)
	}
	resumed, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "failed" || resumed.Metadata["error_type"] == "canceled" {
		t.Fatalf("denied run = %+v", resumed)
	}
	if modelCalls != 1 {
		t.Fatalf("model calls after deny = %d, want no resume model call", modelCalls)
	}
}

func TestE2EProcessRestartBeforeHumanResponse(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	newService := func(t *testing.T) *Service {
		t.Helper()
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var req deepseek.ChatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				return nil, err
			}
			if strings.Contains(lastUserMessage(req.Messages), "Use the restart window.") {
				return deepSeekHTTPResponse(deepSeekTextResponse("Restart resume complete.")), nil
			}
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("restart-human-call", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Which restart window?",
			})), nil
		})}
		return newTestService(t, Config{
			StateDir:       stateRoot,
			DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
		})
	}
	rt := newService(t)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "ask then restart", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	restarted := newService(t)
	if _, err := restarted.ResolveHumanRequest(context.Background(), resp.HumanRequests[0].ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseAnswer,
		Actor:   "tester",
		Message: "Use the restart window.",
	}); err != nil {
		t.Fatalf("ResolveHumanRequest after restart: %v", err)
	}
	run, err := restarted.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" || !strings.Contains(run.FinalResponse, "Restart resume complete") {
		t.Fatalf("run after restart response = %+v", run)
	}
}

func TestE2EModelRetryDoesNotDuplicateHumanRequest(t *testing.T) {
	rt, ctx := newHumanRequestToolTestRuntime(t, "run-e2e-retry", "session-e2e-retry")
	first, err := rt.createAgentHumanRequest(ctx, "retry-human-call", map[string]any{
		"kind":     "freeform",
		"question": "Retry dedupe?",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := rt.createAgentHumanRequest(ctx, "retry-human-call", map[string]any{
		"kind":     "freeform",
		"question": "Retry dedupe?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("retry request id = %q, want %q", second.ID, first.ID)
	}
	list, err := rt.ListHumanRequests(context.Background(), humanrequest.StatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("pending after retry = %+v", list)
	}
}

func TestE2EWorkspaceIsolation(t *testing.T) {
	instanceA := writeRuntimeFixture(t, "xira-assistant", []string{"chat", "sender"})
	instanceB := writeRuntimeFixture(t, "xira-assistant", []string{"chat", "sender"})
	sharedState := filepath.Join(t.TempDir(), "state")
	rtA := newTestService(t, Config{
		WorkspaceRoot: filepath.Join(instanceA, "workspace"),
		StateDir:      sharedState,
	})
	rtB := newTestService(t, Config{
		WorkspaceRoot: filepath.Join(instanceB, "workspace"),
		StateDir:      sharedState,
	})
	req, err := rtA.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		WorkspaceID:  rtA.workspace,
		WorkspaceKey: rtA.WorkspaceKey(),
		RunID:        "run-a",
		AgentID:      "agent-a",
		SessionID:    "session-a",
		Kind:         humanrequest.RequestFreeform,
		Question:     "Only workspace A?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rtB.GetHumanRequest(context.Background(), req.ID); !errors.Is(err, humanrequest.ErrNotFound) {
		t.Fatalf("workspace B get error = %v, want not found", err)
	}
	if _, err := rtB.ResolveHumanRequest(context.Background(), req.ID, humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseAnswer,
		Actor:   "tester",
		Message: "cross workspace",
	}); !errors.Is(err, humanrequest.ErrNotFound) {
		t.Fatalf("workspace B resolve error = %v, want not found", err)
	}
	stored, err := rtA.GetHumanRequest(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != humanrequest.StatusPending {
		t.Fatalf("workspace A request changed after B attempt: %+v", stored)
	}
}
