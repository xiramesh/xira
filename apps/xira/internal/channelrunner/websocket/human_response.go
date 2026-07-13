package websocket

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/xiramesh/xira/internal/humanrequest"
	frt "github.com/xiramesh/xira/internal/runtime"
)

var errWebSocketOwnerResponder = errors.New("websocket owner responder is unsupported")

// websocketHumanResponseAuthority binds one persisted current-sender request
// to its originating WebSocket ChatKey. WebSocket has no trusted typed user
// identity, so typed and owner responder policies fail closed.
// coverage: contract (100% required)
func websocketHumanResponseAuthority(req *humanrequest.HumanRequest) (frt.ChatKey, error) {
	if req == nil {
		return frt.ChatKey{}, fmt.Errorf("%w: human request is required", humanrequest.ErrConflict)
	}
	if req.Responder.Type == humanrequest.ResponderOwner {
		return frt.ChatKey{}, errWebSocketOwnerResponder
	}
	if req.Responder.Type != humanrequest.ResponderCurrentSender {
		return frt.ChatKey{}, fmt.Errorf("%w: unsupported responder policy", humanrequest.ErrConflict)
	}
	if strings.TrimSpace(req.Responder.SenderIDType) != "" {
		return frt.ChatKey{}, fmt.Errorf("%w: websocket cannot authenticate typed responders", humanrequest.ErrConflict)
	}
	key, ok := frt.ParseChatKey(req.ChatKey)
	if !ok || key.Channel != "websocket" || strings.TrimSpace(key.ChatID) == "" || strings.TrimSpace(key.SenderID) == "" {
		return frt.ChatKey{}, fmt.Errorf("%w: invalid websocket chat key", humanrequest.ErrConflict)
	}
	if strings.TrimSpace(req.Responder.SenderID) == "" || req.Responder.SenderID != key.SenderID {
		return frt.ChatKey{}, fmt.Errorf("%w: responder does not match websocket chat key", humanrequest.ErrConflict)
	}
	if strings.TrimSpace(req.Responder.EntrypointID) == "" {
		return frt.ChatKey{}, fmt.Errorf("%w: responder entrypoint is required", humanrequest.ErrConflict)
	}
	return key, nil
}

// normalizeWebSocketHumanResponse seals the supported action matrix. Approval
// requests accept approve/deny/cancel; freeform requests accept answer/cancel.
// coverage: contract (100% required)
func normalizeWebSocketHumanResponse(req *humanrequest.HumanRequest, action, answer string) (humanrequest.ResponseKind, string, error) {
	if req == nil {
		return "", "", fmt.Errorf("%w: human request is required", humanrequest.ErrValidation)
	}
	action = strings.ToLower(strings.TrimSpace(action))
	answer = strings.TrimSpace(answer)
	if action != string(humanrequest.ResponseAnswer) && answer != "" {
		return "", "", fmt.Errorf("%w: answer is only valid for answer actions", humanrequest.ErrValidation)
	}
	switch req.Kind {
	case humanrequest.RequestApproval:
		switch humanrequest.ResponseKind(action) {
		case humanrequest.ResponseApprove, humanrequest.ResponseDeny, humanrequest.ResponseCancel:
			return humanrequest.ResponseKind(action), "", nil
		default:
			return "", "", fmt.Errorf("%w: unsupported approval action", humanrequest.ErrValidation)
		}
	case humanrequest.RequestFreeform:
		switch humanrequest.ResponseKind(action) {
		case humanrequest.ResponseAnswer:
			normalized, err := humanrequest.NormalizeTextAnswer(*req, answer)
			if err != nil {
				return "", "", err
			}
			return humanrequest.ResponseAnswer, normalized, nil
		case humanrequest.ResponseCancel:
			return humanrequest.ResponseCancel, "", nil
		default:
			return "", "", fmt.Errorf("%w: unsupported freeform action", humanrequest.ErrValidation)
		}
	default:
		return "", "", fmt.Errorf("%w: unsupported request kind", humanrequest.ErrValidation)
	}
}

func webSocketHumanResponseIdempotencyKey(req *humanrequest.HumanRequest, token string, kind humanrequest.ResponseKind, message string) string {
	payload := strings.Join([]string{
		req.Responder.EntrypointID,
		req.ChatKey,
		req.ID,
		strings.TrimSpace(token),
		string(kind),
		message,
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("websocket:%x", sum[:])
}

func (r *Runner) handleHumanResponse(
	ctx context.Context,
	conn *wsConn,
	frame inboundFrame,
	writeFrame func(outboundFrame) error,
) {
	requestID := strings.TrimSpace(frame.RequestID)
	if r.structuredHITLResolver == nil {
		_ = writeFrame(errorFrame(frame.ID, requestIDOf(frame), "human_response_unavailable", "human response handling is temporarily unavailable", true))
		return
	}
	if requestID == "" || strings.TrimSpace(frame.CorrelationToken) == "" || strings.TrimSpace(frame.Action) == "" {
		_ = writeFrame(errorFrame(frame.ID, requestIDOf(frame), "human_response_rejected", "human response was rejected", false))
		return
	}
	req, err := r.structuredHITLResolver.GetHumanRequest(ctx, requestID)
	if err != nil {
		slog.Warn("websocket human response request lookup failed", "request_id", requestID, "error", err)
		_ = writeFrame(errorFrame(frame.ID, requestID, "human_response_rejected", "human response was rejected", false))
		return
	}
	key, err := websocketHumanResponseAuthority(req)
	if err != nil {
		code := "human_response_rejected"
		if errors.Is(err, errWebSocketOwnerResponder) {
			code = "unsupported_responder"
		}
		slog.Warn("websocket human response authority rejected", "request_id", requestID, "error", err)
		_ = writeFrame(errorFrame(frame.ID, requestID, code, "human response was rejected", false))
		return
	}
	if !r.connOwnsKey(conn, key) {
		slog.Warn("websocket human response connection rejected", "request_id", requestID, "chat_key", key.String())
		_ = writeFrame(errorFrame(frame.ID, requestID, "human_response_rejected", "human response was rejected", false))
		return
	}
	kind, message, err := normalizeWebSocketHumanResponse(req, frame.Action, frame.Answer)
	if err != nil {
		slog.Warn("websocket human response action rejected", "request_id", requestID, "error", err)
		_ = writeFrame(errorFrame(frame.ID, requestID, "human_response_rejected", "human response was rejected", false))
		return
	}
	input := humanrequest.HumanResponseEnvelope{
		RequestID:         req.ID,
		CorrelationToken:  strings.TrimSpace(frame.CorrelationToken),
		EntrypointID:      req.Responder.EntrypointID,
		SenderID:          req.Responder.SenderID,
		SenderIDType:      "",
		DeliveryMessageID: req.Delivery.MessageID,
		Kind:              kind,
		Message:           message,
		IdempotencyKey:    webSocketHumanResponseIdempotencyKey(req, frame.CorrelationToken, kind, message),
		ResolvedAt:        time.Now().UTC(),
	}
	if _, err := r.structuredHITLResolver.ResolveHumanResponseAsync(ctx, input); err != nil {
		slog.Warn("websocket human response resolve failed", "request_id", requestID, "error", err)
		_ = writeFrame(errorFrame(frame.ID, requestID, "human_response_rejected", "human response was rejected", false))
		return
	}
	_ = writeFrame(outboundFrame{
		Type:      "ack",
		ID:        "srv_ack_" + firstNonEmpty(frame.ID, requestID),
		RequestID: requestID,
		Data: map[string]any{
			"status":     "human_response_accepted",
			"request_id": requestID,
		},
	})
}
