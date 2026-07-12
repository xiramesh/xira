package humanrequest

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStoreResolveExactValidatesCorrelationAndResponder(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:               "hrq_exact_owner",
		WorkspaceID:      "workspace",
		WorkspaceKey:     "ws_exact_owner",
		RunID:            "run-owner",
		AgentID:          "agent-owner",
		SessionID:        "session-owner",
		Kind:             RequestApproval,
		Question:         "Approve?",
		CorrelationToken: "corr-owner-123",
		DeliveryRequired: true,
		Responder: ResponderPolicy{
			Type:         ResponderOwner,
			EntrypointID: "feishu-owner",
			SenderID:     "ou_owner",
			SenderIDType: "open_id",
		},
	})
	req.Delivery.Status = DeliverySent
	req.Delivery.MessageID = "om_owner_prompt"
	if err := store.writeRequest(req); err != nil {
		t.Fatal(err)
	}

	base := HumanResponseEnvelope{
		WorkspaceKey:      req.WorkspaceKey,
		RequestID:         req.ID,
		CorrelationToken:  req.CorrelationToken,
		EntrypointID:      "feishu-owner",
		SenderID:          "ou_owner",
		SenderIDType:      "open_id",
		DeliveryMessageID: "om_owner_prompt",
		Kind:              ResponseApprove,
		Message:           "approved",
		IdempotencyKey:    "card-action-1",
		ResolvedAt:        time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name   string
		mutate func(*HumanResponseEnvelope)
	}{
		{name: "wrong correlation", mutate: func(v *HumanResponseEnvelope) { v.CorrelationToken = "wrong" }},
		{name: "wrong entrypoint", mutate: func(v *HumanResponseEnvelope) { v.EntrypointID = "feishu-other" }},
		{name: "wrong sender", mutate: func(v *HumanResponseEnvelope) { v.SenderID = "ou_attacker" }},
		{name: "wrong sender type", mutate: func(v *HumanResponseEnvelope) { v.SenderIDType = "user_id" }},
		{name: "wrong delivery message", mutate: func(v *HumanResponseEnvelope) { v.DeliveryMessageID = "om_other" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := base
			tt.mutate(&input)
			if _, err := store.ResolveExact(context.Background(), input); !errors.Is(err, ErrConflict) {
				t.Fatalf("ResolveExact() error = %v, want ErrConflict", err)
			}
			stored, err := store.Get(context.Background(), req.WorkspaceKey, req.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != StatusPending || stored.Response != nil || stored.Resume.Status != ResumeWaitingResponse {
				t.Fatalf("rejected response mutated request: %+v", stored)
			}
		})
	}

	resolved, err := store.ResolveExact(context.Background(), base)
	if err != nil {
		t.Fatalf("ResolveExact(valid) error = %v", err)
	}
	if resolved.Status != StatusResolved || resolved.Response == nil {
		t.Fatalf("resolved request = %+v", resolved)
	}
	if resolved.Response.Actor != "ou_owner" || resolved.Response.ActorIDType != "open_id" || resolved.Response.EntrypointID != "feishu-owner" || resolved.Response.DeliveryMessageID != "om_owner_prompt" {
		t.Fatalf("response authority = %+v", resolved.Response)
	}
	if resolved.Resume.Status != ResumePending {
		t.Fatalf("resume status = %q, want %q", resolved.Resume.Status, ResumePending)
	}
}

func TestStoreResolveExactRejectsExpiredRequest(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	expiresAt := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:               "hrq_expired",
		WorkspaceID:      "workspace",
		WorkspaceKey:     "ws_expired",
		RunID:            "run-expired",
		AgentID:          "agent-expired",
		SessionID:        "session-expired",
		Kind:             RequestFreeform,
		Question:         "Too late?",
		CorrelationToken: "corr-expired",
		ExpiresAt:        &expiresAt,
		Responder: ResponderPolicy{
			Type:         ResponderCurrentSender,
			EntrypointID: "feishu-default",
			SenderID:     "ou_sender",
			SenderIDType: "open_id",
		},
	})
	_, err := store.ResolveExact(context.Background(), HumanResponseEnvelope{
		WorkspaceKey:     req.WorkspaceKey,
		RequestID:        req.ID,
		CorrelationToken: req.CorrelationToken,
		EntrypointID:     "feishu-default",
		SenderID:         "ou_sender",
		SenderIDType:     "open_id",
		Kind:             ResponseAnswer,
		Message:          "late answer",
		IdempotencyKey:   "late-1",
		ResolvedAt:       expiresAt.Add(time.Second),
	})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("ResolveExact() error = %v, want ErrExpired", err)
	}
}

func TestStoreResolveExactIsIdempotentOnlyForSameKeyAndPayload(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:               "hrq_idempotent_response",
		WorkspaceID:      "workspace",
		WorkspaceKey:     "ws_idempotent_response",
		RunID:            "run-idempotent",
		AgentID:          "agent-idempotent",
		SessionID:        "session-idempotent",
		Kind:             RequestFreeform,
		Question:         "Which window?",
		CorrelationToken: "corr-idempotent",
		Responder: ResponderPolicy{
			Type:         ResponderCurrentSender,
			EntrypointID: "ilink-default",
			SenderID:     "wxid_owner",
			SenderIDType: "ilink_user_id",
		},
	})
	input := HumanResponseEnvelope{
		WorkspaceKey:     req.WorkspaceKey,
		RequestID:        req.ID,
		CorrelationToken: req.CorrelationToken,
		EntrypointID:     "ilink-default",
		SenderID:         "wxid_owner",
		SenderIDType:     "ilink_user_id",
		Kind:             ResponseAnswer,
		Message:          "Tuesday",
		IdempotencyKey:   "ilink-message-1",
	}
	first, err := store.ResolveExact(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ResolveExact(context.Background(), input)
	if err != nil {
		t.Fatalf("identical retry error = %v", err)
	}
	if second.Response == nil || first.Response == nil || second.Response.ID != first.Response.ID {
		t.Fatalf("identical retry created another response: first=%+v second=%+v", first.Response, second.Response)
	}

	conflict := input
	conflict.Message = "Wednesday"
	if _, err := store.ResolveExact(context.Background(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key/different payload error = %v, want ErrConflict", err)
	}
	conflict = input
	conflict.IdempotencyKey = "ilink-message-2"
	if _, err := store.ResolveExact(context.Background(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("different key error = %v, want ErrConflict", err)
	}
}
