package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/humanrequest"
)

func TestBindHumanResponseIdentityUsesPersistedResponderType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		req     *humanrequest.HumanRequest
		input   humanrequest.HumanResponseEnvelope
		wantID  string
		wantTyp string
		wantErr bool
	}{
		{
			name:   "select user id from trusted callback identities",
			req:    &humanrequest.HumanRequest{Responder: humanrequest.ResponderPolicy{SenderIDType: "user_id"}},
			input:  humanrequest.HumanResponseEnvelope{SenderIdentities: map[string]string{"user_id": "u_1", "open_id": "ou_1"}},
			wantID: "u_1", wantTyp: "user_id",
		},
		{
			name:   "select open id from same callback",
			req:    &humanrequest.HumanRequest{Responder: humanrequest.ResponderPolicy{SenderIDType: "open_id"}},
			input:  humanrequest.HumanResponseEnvelope{SenderIdentities: map[string]string{"user_id": "u_1", "open_id": "ou_1"}},
			wantID: "ou_1", wantTyp: "open_id",
		},
		{
			name:   "legacy single identity remains supported",
			req:    &humanrequest.HumanRequest{Responder: humanrequest.ResponderPolicy{SenderIDType: "open_id"}},
			input:  humanrequest.HumanResponseEnvelope{SenderID: "ou_legacy", SenderIDType: "open_id"},
			wantID: "ou_legacy", wantTyp: "open_id",
		},
		{
			name:    "missing persisted type fails closed",
			req:     &humanrequest.HumanRequest{Responder: humanrequest.ResponderPolicy{SenderIDType: "union_id"}},
			input:   humanrequest.HumanResponseEnvelope{SenderIdentities: map[string]string{"open_id": "ou_1"}},
			wantErr: true,
		},
		{name: "nil request fails closed", wantErr: true},
		{
			name: "persisted type absent fails closed for identity set",
			req:  &humanrequest.HumanRequest{}, input: humanrequest.HumanResponseEnvelope{SenderIdentities: map[string]string{"open_id": "ou_1"}}, wantErr: true,
		},
		{
			name:  "blank trusted identities fail closed",
			req:   &humanrequest.HumanRequest{Responder: humanrequest.ResponderPolicy{SenderIDType: "open_id"}},
			input: humanrequest.HumanResponseEnvelope{SenderIdentities: map[string]string{" OPEN_ID ": " "}}, wantErr: true,
		},
		{
			name:  "single and set sender disagree",
			req:   &humanrequest.HumanRequest{Responder: humanrequest.ResponderPolicy{SenderIDType: "open_id"}},
			input: humanrequest.HumanResponseEnvelope{SenderID: "ou_other", SenderIdentities: map[string]string{"open_id": "ou_1"}}, wantErr: true,
		},
		{
			name:  "single and set type disagree",
			req:   &humanrequest.HumanRequest{Responder: humanrequest.ResponderPolicy{SenderIDType: "open_id"}},
			input: humanrequest.HumanResponseEnvelope{SenderIDType: "user_id", SenderIdentities: map[string]string{"open_id": "ou_1"}}, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bindHumanResponseIdentity(tt.req, tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("bindHumanResponseIdentity() error = %v, wantErr=%v", err, tt.wantErr)
			}
			if !tt.wantErr && (got.SenderID != tt.wantID || got.SenderIDType != tt.wantTyp) {
				t.Fatalf("identity = (%q,%q), want (%q,%q)", got.SenderID, got.SenderIDType, tt.wantID, tt.wantTyp)
			}
		})
	}
}

func TestResolveHumanResponseAsyncCommitsThenResumesDurably(t *testing.T) {
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})
	req, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		ID: "hrq_async", WorkspaceID: rt.workspace, RunID: "run-async", AgentID: "xira-assistant",
		SessionID: "session-async", Source: "async_test", Kind: humanrequest.RequestApproval,
		Question: "Approve?", CorrelationToken: "550e8400-e29b-41d4-a716-446655440000",
		Responder: humanrequest.ResponderPolicy{Type: humanrequest.ResponderCurrentSender,
			EntrypointID: "feishu-main", SenderID: "u_operator", SenderIDType: "user_id"},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := humanrequest.HumanResponseEnvelope{
		RequestID: req.ID, CorrelationToken: req.CorrelationToken, EntrypointID: "feishu-main",
		SenderIdentities: map[string]string{"user_id": "u_operator", "open_id": "ou_operator"},
		Kind:             humanrequest.ResponseApprove, IdempotencyKey: "feishu-card-async",
	}
	accepted, err := rt.ResolveHumanResponseAsync(context.Background(), input)
	if err != nil {
		t.Fatalf("ResolveHumanResponseAsync() error = %v", err)
	}
	if accepted.Status != humanrequest.StatusResolved || accepted.Response == nil || accepted.Response.Actor != "u_operator" {
		t.Fatalf("accepted response = %+v", accepted)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, getErr := rt.GetHumanRequest(context.Background(), req.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Resume.Status == humanrequest.ResumeCompleted {
			break
		}
		if got.Resume.Status == humanrequest.ResumeFailed {
			t.Fatalf("async resume failed: %+v", got.Resume)
		}
		if time.Now().After(deadline) {
			t.Fatalf("async resume did not complete: %+v", got.Resume)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := rt.ResolveHumanResponseAsync(context.Background(), input); err != nil {
		t.Fatalf("idempotent async retry = %v", err)
	}
	conflict := input
	conflict.Kind = humanrequest.ResponseDeny
	if _, err := rt.ResolveHumanResponseAsync(context.Background(), conflict); !errors.Is(err, humanrequest.ErrConflict) {
		t.Fatalf("conflicting async retry error = %v", err)
	}
}

func TestResolveHumanResponseAsyncRejectsUnavailableStore(t *testing.T) {
	var nilService *Service
	if _, err := nilService.ResolveHumanResponseAsync(context.Background(), humanrequest.HumanResponseEnvelope{}); err == nil {
		t.Fatal("nil service async resolve succeeded")
	}
}
