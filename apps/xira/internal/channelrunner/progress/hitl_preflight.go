package progress

import (
	"context"
	"log/slog"
	"strings"

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
//   - agent_request and flow_human_approval HITLs are eligible for pure-text IM
//     reply.
//   - agent_request stores the user's text as ResponseAnswer + message and
//     leaves intent understanding to the agent during resume.
//   - flow_human_approval first matches the text against the request options,
//     then stores the normalized option id as ResponseAnswer + message. Unknown
//     text does not resolve the request, so a typo cannot consume the gate.
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
	// Find the most recent IM-resolvable HITL.
	var hr *humanrequest.HumanRequest
	for i := range pending {
		if imResolvableHITLSource(pending[i].Source) {
			hr = &pending[i]
			break // pending is sorted by CreatedAt desc, first match is most recent
		}
	}
	if hr == nil {
		return false // no IM-resolvable HITL pending
	}
	kind, msg := ClassifyHITLResponse(content, hr.Kind)
	// #108: any HITL with Options (agent_request OR flow_human_approval) goes
	// through structured option matching — the user is choosing from a known
	// set. Match → resolve with the option id; no match → return false so the
	// message enters the agent turn (#106 hydration + #107 interpret) for
	// intent understanding. HITLs without Options (freeform questions) skip
	// this and resolve the text directly as an answer (legacy #92 behavior).
	//
	// This is source-neutral: an option is an option regardless of whether the
	// request originated from a flow human_approval step or an agent's
	// human.request call. The earlier `source == flow_human_approval` special
	// case was an asymmetry bug (#105 缺口 A) — flow HITLs matched options
	// but agent HITLs resolved any text, which was the "答不上/对不齐" root cause.
	if len(hr.Options) > 0 {
		signal, ok := matchHumanOption(content, hr.Options)
		if !ok {
			return false
		}
		msg = signal
	}
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

func imResolvableHITLSource(source string) bool {
	switch source {
	case "agent_request", "flow_human_approval":
		return true
	default:
		return false
	}
}

// matchHumanOption checks whether the user's text selects one of the request's
// options (by id or label, case-insensitive). Returns the normalized option id
// (or label when no id) and true on match; ("", false) when the text doesn't
// match any option — caller treats that as "no structured resolve, fall through
// to agent turn".
//
// Renamed from flowApprovalSignalFromText (#108): the logic is source-neutral,
// applicable to any HITL that carries Options (flow_human_approval AND
// agent_request). The old name implied flow-only.
func matchHumanOption(text string, options []humanrequest.HumanOption) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	for _, opt := range options {
		id := strings.TrimSpace(opt.ID)
		if id != "" && strings.EqualFold(text, id) {
			return id, true
		}
		label := strings.TrimSpace(opt.Label)
		if label != "" && strings.EqualFold(text, label) {
			if id != "" {
				return id, true
			}
			return label, true
		}
	}
	return "", false
}
