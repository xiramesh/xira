package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

func TestReconcileHumanRequestsResumesAnswerPersistedBeforeCrash(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	newService := func(t *testing.T, transport http.RoundTripper) *Service {
		t.Helper()
		return newTestService(t, Config{
			StateDir: stateRoot,
			DeepSeekClient: deepseek.New(
				deepseek.WithBaseURLForTest("http://deepseek.test"),
				deepseek.WithAPIKey("test-key"),
				deepseek.WithHTTPClient(&http.Client{Transport: transport}),
			),
		})
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		if strings.Contains(lastUserMessage(request.Messages), "Use the durable window.") {
			return deepSeekHTTPResponse(deepSeekTextResponse("Durable resume completed.")), nil
		}
		return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("durable-human-call", "human_request", map[string]any{
			"kind": "freeform", "question": "Which durable window?",
		})), nil
	})

	rt := newService(t, transport)
	initial, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "ask before crash",
		Context: channel.NewInboundContext("test", "user-1", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID := initial.HumanRequests[0].ID
	resolved, err := rt.humanRequests.Resolve(context.Background(), humanrequest.ResolveRequest{
		WorkspaceKey:   rt.WorkspaceKey(),
		RequestID:      requestID,
		Kind:           humanrequest.ResponseAnswer,
		Actor:          "user-1",
		Message:        "Use the durable window.",
		IdempotencyKey: "message-before-crash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Resume.Status != humanrequest.ResumePending {
		t.Fatalf("persisted response resume = %+v, want pending", resolved.Resume)
	}

	restarted := newService(t, transport)
	if err := restarted.ReconcileHumanRequests(context.Background()); err != nil {
		t.Fatalf("ReconcileHumanRequests() error = %v", err)
	}
	run, err := restarted.RunStore().Load(initial.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" || !strings.Contains(run.FinalResponse, "Durable resume completed") {
		t.Fatalf("reconciled run = %+v", run)
	}
	got, err := restarted.GetHumanRequest(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Resume.Status != humanrequest.ResumeCompleted || got.Resume.Attempts != 1 {
		t.Fatalf("reconciled request resume = %+v", got.Resume)
	}
}

func TestReconcileHumanRequestsRetriesFailedResume(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	serviceWithTransport := func(t *testing.T, transport http.RoundTripper) *Service {
		t.Helper()
		return newTestService(t, Config{
			StateDir: stateRoot,
			DeepSeekClient: deepseek.New(
				deepseek.WithBaseURLForTest("http://deepseek.test"),
				deepseek.WithAPIKey("test-key"),
				deepseek.WithHTTPClient(&http.Client{Transport: transport}),
			),
		})
	}
	seedTransport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("retry-human-call", "human_request", map[string]any{
			"kind": "freeform", "question": "Retry resume?",
		})), nil
	})
	rt := serviceWithTransport(t, seedTransport)
	initial, err := rt.RunAgent(context.Background(), TurnRequest{Message: "seed retry", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	requestID := initial.HumanRequests[0].ID
	if _, err := rt.humanRequests.Resolve(context.Background(), humanrequest.ResolveRequest{
		WorkspaceKey: rt.WorkspaceKey(), RequestID: requestID,
		Kind: humanrequest.ResponseAnswer, Actor: "user-1", Message: "retry answer", IdempotencyKey: "retry-answer-1",
	}); err != nil {
		t.Fatal(err)
	}

	failing := serviceWithTransport(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("temporary model outage")
	}))
	if err := failing.ReconcileHumanRequests(context.Background()); err == nil {
		t.Fatal("first ReconcileHumanRequests() succeeded, want model failure")
	}
	failedRequest, err := failing.GetHumanRequest(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if failedRequest.Resume.Status != humanrequest.ResumeFailed || failedRequest.Resume.Attempts != 1 || failedRequest.Resume.LastError == "" {
		t.Fatalf("failed request resume = %+v", failedRequest.Resume)
	}

	succeeding := serviceWithTransport(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return deepSeekHTTPResponse(deepSeekTextResponse("Retry resume completed.")), nil
	}))
	if err := succeeding.ReconcileHumanRequests(context.Background()); err != nil {
		t.Fatalf("retry ReconcileHumanRequests() error = %v", err)
	}
	completedRequest, err := succeeding.GetHumanRequest(context.Background(), requestID)
	if err != nil {
		t.Fatal(err)
	}
	if completedRequest.Resume.Status != humanrequest.ResumeCompleted || completedRequest.Resume.Attempts != 2 {
		t.Fatalf("retried request resume = %+v", completedRequest.Resume)
	}
}

func TestResolveHumanResponseRequiresCurrentOwnerAndPersistedSnapshot(t *testing.T) {
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})
	rt.ownerBindings.Set(ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id",
	})
	req, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		ID: "hrq_owner_exact_runtime", WorkspaceID: rt.workspace,
		RunID: "run-owner-exact", AgentID: "xira-assistant", SessionID: "session-owner-exact",
		Kind: humanrequest.RequestApproval, Question: "Owner approve?", CorrelationToken: "corr-owner-exact",
		Responder: humanrequest.ResponderPolicy{
			Type: humanrequest.ResponderOwner, EntrypointID: "feishu-owner",
			SenderID: "ou_owner", SenderIDType: "open_id",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := humanrequest.HumanResponseEnvelope{
		RequestID: req.ID, CorrelationToken: req.CorrelationToken,
		EntrypointID: "feishu-owner", SenderID: "ou_owner", SenderIDType: "open_id",
		Kind: humanrequest.ResponseApprove, Message: "approved", IdempotencyKey: "owner-action-1",
	}
	wrong := base
	wrong.SenderID = "ou_attacker"
	if _, err := rt.ResolveHumanResponse(context.Background(), wrong); !errors.Is(err, humanrequest.ErrConflict) {
		t.Fatalf("wrong owner error = %v, want ErrConflict", err)
	}

	// A rebind invalidates both sides safely: the old owner no longer passes
	// current authority, while the new owner does not match the request snapshot.
	rt.ownerBindings.Set(ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_new_owner", OwnerSenderIDType: "open_id",
	})
	if _, err := rt.ResolveHumanResponse(context.Background(), base); !errors.Is(err, humanrequest.ErrConflict) {
		t.Fatalf("old owner after rebind error = %v, want ErrConflict", err)
	}
	newOwner := base
	newOwner.SenderID = "ou_new_owner"
	if _, err := rt.ResolveHumanResponse(context.Background(), newOwner); !errors.Is(err, humanrequest.ErrConflict) {
		t.Fatalf("new owner against old snapshot error = %v, want ErrConflict", err)
	}

	rt.ownerBindings.Set(ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id",
	})
	resolved, err := rt.ResolveHumanResponse(context.Background(), base)
	if err != nil {
		t.Fatalf("valid owner response error = %v", err)
	}
	if resolved.Resume.Status != humanrequest.ResumeCompleted || resolved.Response == nil || resolved.Response.Actor != "ou_owner" {
		t.Fatalf("resolved owner request = %+v", resolved)
	}
}

func TestNewServiceRecoversInterruptedHumanRequestResume(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	rt := newTestService(t, Config{StateDir: stateRoot})
	req, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		ID: "hrq_startup_running", WorkspaceID: rt.workspace,
		RunID: "run-startup-running", AgentID: "xira-assistant", SessionID: "session-startup-running",
		Kind: humanrequest.RequestApproval, Question: "Recover startup resume?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.humanRequests.Resolve(context.Background(), humanrequest.ResolveRequest{
		WorkspaceKey: rt.WorkspaceKey(), RequestID: req.ID,
		Kind: humanrequest.ResponseApprove, Actor: "tester", Message: "approved", IdempotencyKey: "startup-answer",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rt.humanRequests.ClaimResume(context.Background(), rt.WorkspaceKey(), req.ID, time.Now()); err != nil || !ok {
		t.Fatalf("ClaimResume() ok=%v err=%v", ok, err)
	}

	restarted := newTestService(t, Config{StateDir: stateRoot})
	got, err := restarted.GetHumanRequest(context.Background(), req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Resume.Status != humanrequest.ResumeFailed || !strings.Contains(got.Resume.LastError, "interrupted") {
		t.Fatalf("startup recovered resume = %+v", got.Resume)
	}
}
