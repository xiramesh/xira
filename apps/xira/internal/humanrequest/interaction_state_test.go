package humanrequest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreCreateInitializesHumanInteractionContract(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	expiresAt := time.Date(2026, 7, 14, 9, 30, 0, 0, time.UTC)

	req, err := store.Create(context.Background(), CreateRequest{
		ID:           "hrq_owner_contract",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_owner_contract",
		RunID:        "run-owner",
		AgentID:      "agent-owner",
		SessionID:    "session-owner",
		Kind:         RequestApproval,
		Question:     "Approve the contract?",
		Responder: ResponderPolicy{
			Type:         ResponderOwner,
			EntrypointID: "feishu-owner",
			SenderID:     "ou_owner",
			SenderIDType: "open_id",
		},
		DeliveryRequired: true,
		ExpiresAt:        &expiresAt,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if req.Responder.Type != ResponderOwner || req.Responder.EntrypointID != "feishu-owner" || req.Responder.SenderID != "ou_owner" || req.Responder.SenderIDType != "open_id" {
		t.Fatalf("responder = %+v", req.Responder)
	}
	if req.CorrelationToken == "" {
		t.Fatal("correlation token is empty")
	}
	if req.Delivery.Status != DeliveryPending {
		t.Fatalf("delivery status = %q, want %q", req.Delivery.Status, DeliveryPending)
	}
	if req.Resume.Status != ResumeWaitingResponse {
		t.Fatalf("resume status = %q, want %q", req.Resume.Status, ResumeWaitingResponse)
	}
	if req.ExpiresAt == nil || !req.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expires_at = %v, want %v", req.ExpiresAt, expiresAt)
	}

	stored, err := store.Get(context.Background(), "ws_owner_contract", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CorrelationToken != req.CorrelationToken || stored.Responder != req.Responder || stored.Delivery.Status != DeliveryPending || stored.Resume.Status != ResumeWaitingResponse {
		t.Fatalf("stored interaction contract = %+v", stored)
	}
}

func TestStoreCreateDefaultsToCurrentSenderResponder(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:           "hrq_default_responder",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_default_responder",
		RunID:        "run-default",
		AgentID:      "agent-default",
		SessionID:    "session-default",
		Kind:         RequestFreeform,
		Question:     "Which window?",
	})
	if req.Responder.Type != ResponderCurrentSender {
		t.Fatalf("default responder = %q, want %q", req.Responder.Type, ResponderCurrentSender)
	}
	if req.Delivery.Status != DeliveryNone {
		t.Fatalf("default delivery = %q, want %q", req.Delivery.Status, DeliveryNone)
	}
	if req.Resume.Status != ResumeWaitingResponse {
		t.Fatalf("default resume = %q, want %q", req.Resume.Status, ResumeWaitingResponse)
	}
	if req.CorrelationToken == "" {
		t.Fatal("default request did not receive correlation token")
	}
}

func TestStoreCreateRejectsInvalidResponderPolicy(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	base := CreateRequest{
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_invalid_responder",
		RunID:        "run-invalid",
		AgentID:      "agent-invalid",
		SessionID:    "session-invalid",
		Kind:         RequestApproval,
		Question:     "Approve?",
	}
	tests := []struct {
		name      string
		responder ResponderPolicy
	}{
		{name: "unknown type", responder: ResponderPolicy{Type: "administrator"}},
		{name: "owner without entrypoint", responder: ResponderPolicy{Type: ResponderOwner, SenderID: "ou_owner", SenderIDType: "open_id"}},
		{name: "owner without sender", responder: ResponderPolicy{Type: ResponderOwner, EntrypointID: "feishu-owner", SenderIDType: "open_id"}},
		{name: "owner without sender type", responder: ResponderPolicy{Type: ResponderOwner, EntrypointID: "feishu-owner", SenderID: "ou_owner"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := base
			input.ID = "hrq_" + safeTestID(tt.name)
			input.Responder = tt.responder
			if _, err := store.Create(context.Background(), input); err == nil {
				t.Fatal("Create() succeeded, want responder validation error")
			}
		})
	}
}

func TestStoreLoadsLegacyHumanRequestWithoutInteractionState(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	legacy := HumanRequest{
		ID:           "hrq_legacy_interaction",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_legacy_interaction",
		RunID:        "run-legacy",
		AgentID:      "agent-legacy",
		SessionID:    "session-legacy",
		Kind:         RequestApproval,
		Status:       StatusResolved,
		Question:     "Legacy approval?",
		CreatedAt:    time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
	}
	path := filepath.Join(root, "workspaces", legacy.WorkspaceKey, "human-requests", legacy.ID+".json")
	if err := writeJSONAtomic(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), legacy.WorkspaceKey, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Responder.Type != "" || got.CorrelationToken != "" || got.Delivery.Status != "" || got.Resume.Status != "" {
		t.Fatalf("legacy record was synthesized during load: %+v", got)
	}
}

func safeTestID(value string) string {
	out := make([]rune, 0, len(value))
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			out = append(out, r)
			continue
		}
		out = append(out, '_')
	}
	return string(out)
}
