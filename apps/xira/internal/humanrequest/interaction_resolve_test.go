package humanrequest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestStoreFindByCorrelationRequiresOneExactMatchAcrossStatuses(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	workspaceKey := "ws_find_correlation"
	first := mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_find_first", WorkspaceID: "workspace", WorkspaceKey: workspaceKey,
		RunID: "run-first", AgentID: "agent", SessionID: "session",
		Kind: RequestFreeform, Question: "First?", CorrelationToken: "550e8400-e29b-41d4-a716-446655440000",
	})
	mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_find_second", WorkspaceID: "workspace", WorkspaceKey: workspaceKey,
		RunID: "run-second", AgentID: "agent", SessionID: "session",
		Kind: RequestFreeform, Question: "Second?", CorrelationToken: "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
	})

	got, err := store.FindByCorrelation(context.Background(), workspaceKey, first.CorrelationToken)
	if err != nil || got.ID != first.ID {
		t.Fatalf("FindByCorrelation() = %+v, %v; want %s", got, err, first.ID)
	}
	if _, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: workspaceKey, RequestID: first.ID, Kind: ResponseAnswer,
		Actor: "user", Message: "done", IdempotencyKey: "find-first",
	}); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.FindByCorrelation(context.Background(), workspaceKey, first.CorrelationToken)
	if err != nil || resolved.Status != StatusResolved {
		t.Fatalf("resolved correlation lookup = %+v, %v", resolved, err)
	}
	if _, err := store.FindByCorrelation(context.Background(), workspaceKey, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing correlation error = %v, want not found", err)
	}
	if _, err := store.FindByCorrelation(context.Background(), "", first.CorrelationToken); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid workspace error = %v, want validation", err)
	}
	if _, err := store.FindByCorrelation(context.Background(), workspaceKey, " "); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty correlation error = %v, want validation", err)
	}

	mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_find_duplicate", WorkspaceID: "workspace", WorkspaceKey: workspaceKey,
		RunID: "run-duplicate", AgentID: "agent", SessionID: "session",
		Kind: RequestFreeform, Question: "Duplicate?", CorrelationToken: first.CorrelationToken,
	})
	if _, err := store.FindByCorrelation(context.Background(), workspaceKey, first.CorrelationToken); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate correlation error = %v, want conflict", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.FindByCorrelation(canceled, workspaceKey, first.CorrelationToken); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup error = %v", err)
	}

	if err := os.WriteFile(filepath.Join(store.requestsDir(workspaceKey), "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FindByCorrelation(context.Background(), workspaceKey, first.CorrelationToken); err == nil {
		t.Fatal("corrupt request store lookup succeeded, want read error")
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

func TestStoreLegacyResolveUsesIdempotencyKeyWithoutExactEnvelope(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_legacy_idempotent", WorkspaceID: "workspace", WorkspaceKey: "ws_legacy_idempotent",
		RunID: "run-legacy-idempotent", AgentID: "agent", SessionID: "session",
		Kind: RequestFreeform, Question: "Legacy retry?",
	})
	input := ResolveRequest{
		WorkspaceKey: req.WorkspaceKey, RequestID: req.ID, Kind: ResponseAnswer,
		Actor: "user", Message: "same answer", IdempotencyKey: "legacy-message-1",
	}
	first, err := store.Resolve(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Resolve(context.Background(), input)
	if err != nil {
		t.Fatalf("legacy identical retry error = %v", err)
	}
	if second.Response == nil || first.Response == nil || second.Response.ID != first.Response.ID {
		t.Fatalf("legacy retry responses = first=%+v second=%+v", first.Response, second.Response)
	}
	input.Message = "different answer"
	if _, err := store.Resolve(context.Background(), input); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy conflicting retry error = %v", err)
	}
}

func TestExactResponseContractNilAndRetryAuthority(t *testing.T) {
	if err := validateExactResponse(nil, HumanResponseEnvelope{}, time.Now()); !errors.Is(err, ErrValidation) {
		t.Fatalf("nil exact request error = %v", err)
	}
	existing := &HumanResponse{
		Kind: ResponseApprove, Actor: "ou_owner", ActorIDType: "open_id",
		EntrypointID: "feishu-owner", DeliveryMessageID: "om_1",
		Message: "approved", IdempotencyKey: "action-1",
	}
	legacy := ResolveRequest{Kind: ResponseApprove, Actor: "ou_owner", Message: "approved", IdempotencyKey: "action-1"}
	exact := &HumanResponseEnvelope{SenderIDType: "open_id", EntrypointID: "feishu-owner", DeliveryMessageID: "om_2"}
	if sameResponseRetry(existing, legacy, exact) {
		t.Fatal("retry with wrong delivery authority matched")
	}
}
