package progress

import (
	"context"
	"log/slog"

	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/runtime"
)

// hitl_preflight.go: shared HITL preflight check for all channel adapters (#92).
//
// All three channels (feishu/ilink/websocket) call TryResolveHITL before
// session.Handle. If it returns true, the adapter returns (skip session.Handle);
// if false, the adapter continues to session.Handle (start/steer a turn).
//
// This is the single place where "does this chatKey have a pending HITL that
// can be resolved via IM text?" is decided. No per-channel duplication.

// TryResolveHITL checks if chatKey has a pending HITL that can be resolved via
// the user's IM text reply. If yes, it resolves it and returns true (adapter
// should skip session.Handle). If no (no pending HITL, wrong source, or resolve
// error), returns false (adapter continues to session.Handle).
//
// Rules:
//   - Only agent_request HITLs are eligible (runtime_tool_gate needs precise
//     approve/deny — button card future or HTTP/CLI).
//   - The user's text is always treated as ResponseAnswer (no keyword matching;
//     intent understanding is left to the agent during resume).
//   - Multiple pending HITLs: resolves the most recent one (store sorts by
//     CreatedAt desc). The rest stay pending.
//   - resolve error → returns false (don't block the user; start a normal turn).
//
// resolver may be nil (HITL direct-answer disabled) → returns false.
func TryResolveHITL(ctx context.Context, resolver runtime.HITLResolver, chatKey runtime.ChatKey, content, senderID string) bool {
	if resolver == nil {
		return false
	}
	pending, err := resolver.ListPendingHumanRequestsByChatKey(ctx, chatKey.String())
	if err != nil || len(pending) == 0 {
		return false
	}
	// Find the most recent agent_request HITL (eligible for IM direct-answer).
	// runtime_tool_gate HITLs are skipped (need precise approve/deny).
	var hr *humanrequest.HumanRequest
	for i := range pending {
		if pending[i].Source == "agent_request" {
			hr = &pending[i]
			break // pending is sorted by CreatedAt desc, first match is most recent
		}
	}
	if hr == nil {
		return false // only tool-gate HITLs pending — not eligible for IM text resolve
	}
	kind, msg := ClassifyHITLResponse(content, hr.Kind)
	if _, err := resolver.ResolveHumanRequest(ctx, hr.ID, humanrequest.ResolveRequest{
		Kind:    kind,
		Actor:   senderID,
		Message: msg,
	}); err != nil {
		slog.Warn("HITL resolve via IM failed, falling through to normal turn",
			"chat_key", chatKey.String(),
			"human_request_id", hr.ID,
			"error", err,
		)
		return false
	}
	slog.Info("HITL resolved via IM direct answer",
		"chat_key", chatKey.String(),
		"human_request_id", hr.ID,
		"response_kind", kind,
		"sender_id", senderID,
	)
	return true
}
