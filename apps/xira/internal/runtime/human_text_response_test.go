package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/humanrequest"
)

func TestResolveHumanTextResponseUsesExactCurrentSenderAndChat(t *testing.T) {
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})
	chatKey := ChatKey{Channel: "ilink", ChatID: "group-1", SenderID: "user-1"}.String()
	req, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		ID: "hrq_text_current", WorkspaceID: rt.workspace, RunID: "run-text-current",
		AgentID: "xira-assistant", SessionID: "session-text-current", Source: "text_test",
		Kind: humanrequest.RequestApproval, Question: "Approve?", ChatKey: chatKey,
		CorrelationToken: textProtocolRuntimeCorrelation,
		Options:          []humanrequest.HumanOption{{ID: "approve", Label: "批准"}, {ID: "deny", Label: "拒绝"}},
		Responder: humanrequest.ResponderPolicy{
			Type: humanrequest.ResponderCurrentSender, EntrypointID: "ilink-main",
			SenderID: "user-1", SenderIDType: "ilink_user_id",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := humanrequest.TextResponseEnvelope{
		CorrelationToken: req.CorrelationToken, EntrypointID: "ilink-main",
		SenderID: "user-1", SenderIDType: "ilink_user_id", ChatKey: chatKey,
		Answer: "批准", IdempotencyKey: "ilink-message-1",
	}
	for name, mutate := range map[string]func(*humanrequest.TextResponseEnvelope){
		"wrong chat": func(in *humanrequest.TextResponseEnvelope) {
			in.ChatKey = ChatKey{Channel: "ilink", ChatID: "other", SenderID: "user-1"}.String()
		},
		"wrong sender":      func(in *humanrequest.TextResponseEnvelope) { in.SenderID = "attacker" },
		"wrong sender type": func(in *humanrequest.TextResponseEnvelope) { in.SenderIDType = "open_id" },
		"wrong entrypoint":  func(in *humanrequest.TextResponseEnvelope) { in.EntrypointID = "ilink-other" },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			if _, err := rt.ResolveHumanTextResponse(context.Background(), input); !errors.Is(err, humanrequest.ErrConflict) {
				t.Fatalf("error = %v, want conflict", err)
			}
			pending, err := rt.GetHumanRequest(context.Background(), req.ID)
			if err != nil || pending.Status != humanrequest.StatusPending {
				t.Fatalf("request mutated after rejection: %+v, %v", pending, err)
			}
		})
	}

	resolved, err := rt.ResolveHumanTextResponse(context.Background(), base)
	if err != nil {
		t.Fatalf("ResolveHumanTextResponse() error = %v", err)
	}
	if resolved.Status != humanrequest.StatusResolved || resolved.Response == nil || resolved.Response.Message != "approve" || resolved.Resume.Status != humanrequest.ResumeCompleted {
		t.Fatalf("resolved request = %+v", resolved)
	}
	retried, err := rt.ResolveHumanTextResponse(context.Background(), base)
	if err != nil || retried.Response == nil || retried.Response.ID != resolved.Response.ID {
		t.Fatalf("idempotent retry = %+v, %v", retried, err)
	}
	conflict := base
	conflict.Answer = "拒绝"
	if _, err := rt.ResolveHumanTextResponse(context.Background(), conflict); !errors.Is(err, humanrequest.ErrConflict) {
		t.Fatalf("conflicting retry error = %v, want conflict", err)
	}
}

func TestResolveHumanTextResponseAllowsOwnerDMButRequiresCurrentOwner(t *testing.T) {
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})
	rt.ownerBindings.Set(ownerBinding{
		EntrypointID: "ilink-owner", OwnerSenderID: "owner-1", OwnerSenderIDType: "ilink_user_id",
	})
	req, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		ID: "hrq_text_owner", WorkspaceID: rt.workspace, RunID: "run-text-owner",
		AgentID: "xira-assistant", SessionID: "session-text-owner", Source: "text_test",
		Kind: humanrequest.RequestFreeform, Question: "Owner answer?",
		ChatKey:          ChatKey{Channel: "ilink", ChatID: "origin-group", SenderID: "coworker"}.String(),
		CorrelationToken: "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		Responder: humanrequest.ResponderPolicy{
			Type: humanrequest.ResponderOwner, EntrypointID: "ilink-owner",
			SenderID: "owner-1", SenderIDType: "ilink_user_id",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := humanrequest.TextResponseEnvelope{
		CorrelationToken: req.CorrelationToken, EntrypointID: "ilink-owner",
		SenderID: "owner-1", SenderIDType: "ilink_user_id",
		ChatKey:  ChatKey{Channel: "ilink", ChatID: "owner-1", SenderID: "owner-1"}.String(),
		ChatType: "direct", Answer: "Proceed Tuesday", IdempotencyKey: "owner-dm-1",
	}
	groupInput := input
	groupInput.ChatKey = ChatKey{Channel: "ilink", ChatID: "origin-group", SenderID: "owner-1"}.String()
	groupInput.ChatType = "group"
	if _, err := rt.ResolveHumanTextResponse(context.Background(), groupInput); !errors.Is(err, humanrequest.ErrConflict) {
		t.Fatalf("owner group response error = %v, want conflict", err)
	}
	resolved, err := rt.ResolveHumanTextResponse(context.Background(), input)
	if err != nil || resolved.Response == nil || resolved.Response.Actor != "owner-1" {
		t.Fatalf("owner DM resolve = %+v, %v", resolved, err)
	}
}

func TestResolveHumanTextResponseRejectsExpiredAndUnknownReferences(t *testing.T) {
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})
	expiredAt := time.Now().Add(-time.Minute)
	_, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		ID: "hrq_text_expired", WorkspaceID: rt.workspace, RunID: "run-text-expired",
		AgentID: "xira-assistant", SessionID: "session-text-expired", Source: "text_test",
		Kind: humanrequest.RequestFreeform, Question: "Too late?", ExpiresAt: &expiredAt,
		CorrelationToken: textProtocolRuntimeCorrelation,
		Responder:        humanrequest.ResponderPolicy{Type: humanrequest.ResponderCurrentSender, EntrypointID: "ilink-main", SenderID: "user-1", SenderIDType: "ilink_user_id"},
		ChatKey:          ChatKey{Channel: "ilink", ChatID: "user-1", SenderID: "user-1"}.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := humanrequest.TextResponseEnvelope{
		CorrelationToken: textProtocolRuntimeCorrelation, EntrypointID: "ilink-main",
		SenderID: "user-1", SenderIDType: "ilink_user_id",
		ChatKey: ChatKey{Channel: "ilink", ChatID: "user-1", SenderID: "user-1"}.String(),
		Answer:  "late", IdempotencyKey: "late-1",
	}
	if _, err := rt.ResolveHumanTextResponse(context.Background(), base); !errors.Is(err, humanrequest.ErrExpired) {
		t.Fatalf("expired response error = %v", err)
	}
	base.CorrelationToken = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := rt.ResolveHumanTextResponse(context.Background(), base); !errors.Is(err, humanrequest.ErrNotFound) {
		t.Fatalf("unknown response error = %v", err)
	}
}

const textProtocolRuntimeCorrelation = "550e8400-e29b-41d4-a716-446655440000"

func TestTextResponseChatAuthorized(t *testing.T) {
	tests := []struct {
		name     string
		req      *humanrequest.HumanRequest
		chatKey  string
		chatType string
		want     bool
	}{
		{name: "nil request", want: false},
		{name: "current match", req: &humanrequest.HumanRequest{ChatKey: "ilink/chat/user", Responder: humanrequest.ResponderPolicy{Type: humanrequest.ResponderCurrentSender}}, chatKey: "ilink/chat/user", want: true},
		{name: "current mismatch", req: &humanrequest.HumanRequest{ChatKey: "ilink/chat/user", Responder: humanrequest.ResponderPolicy{Type: humanrequest.ResponderCurrentSender}}, chatKey: "ilink/other/user", want: false},
		{name: "current missing persisted chat", req: &humanrequest.HumanRequest{Responder: humanrequest.ResponderPolicy{Type: humanrequest.ResponderCurrentSender}}, chatKey: "ilink/chat/user", want: false},
		{name: "owner dm", req: &humanrequest.HumanRequest{ChatKey: "ilink/origin/coworker", Responder: humanrequest.ResponderPolicy{Type: humanrequest.ResponderOwner, SenderID: "owner"}}, chatKey: "ilink/owner/owner", chatType: "direct", want: true},
		{name: "owner group rejected", req: &humanrequest.HumanRequest{ChatKey: "ilink/origin/coworker", Responder: humanrequest.ResponderPolicy{Type: humanrequest.ResponderOwner, SenderID: "owner"}}, chatKey: "ilink/origin/owner", chatType: "group", want: false},
		{name: "owner mismatched dm rejected", req: &humanrequest.HumanRequest{Responder: humanrequest.ResponderPolicy{Type: humanrequest.ResponderOwner, SenderID: "owner"}}, chatKey: "ilink/someone/owner", chatType: "direct", want: false},
		{name: "unknown responder", req: &humanrequest.HumanRequest{Responder: humanrequest.ResponderPolicy{Type: "other"}}, chatKey: "ilink/chat/user", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := textResponseChatAuthorized(tt.req, tt.chatKey, tt.chatType); got != tt.want {
				t.Fatalf("textResponseChatAuthorized() = %v, want %v", got, tt.want)
			}
		})
	}
}
