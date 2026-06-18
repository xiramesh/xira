package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/entrypoints"
	"github.com/xiramesh/xira/internal/humanrequest"
	frt "github.com/xiramesh/xira/internal/runtime"
)

const (
	websocketDefaultEntrypoint = "websocket-default"
	websocketMessageDedupeTTL  = time.Hour
)

var websocketCapabilities = []string{
	"message",
	"event",
	"assistant_delta",
	"response",
	"interrupt",
	"human_response",
	"outbound_message",
}

var errWebSocketUnsupportedMessage = errors.New("only JSON text frames are supported")

type websocketBadJSONError struct {
	err error
}

func (e websocketBadJSONError) Error() string {
	return e.err.Error()
}

type websocketInboundFrame struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type websocketOutboundFrame struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Data      any    `json:"data,omitempty"`
}

type websocketHelloData struct {
	ClientID     string `json:"client_id,omitempty"`
	EntrypointID string `json:"entrypoint_id,omitempty"`
}

type websocketMessageData struct {
	Message      string                 `json:"message"`
	EntrypointID string                 `json:"entrypoint_id,omitempty"`
	AgentID      string                 `json:"agent_id,omitempty"`
	SessionID    string                 `json:"session_id,omitempty"`
	Context      channel.InboundContext `json:"context"`
}

type websocketHumanResponseData struct {
	HumanRequestID string                    `json:"human_request_id"`
	Kind           humanrequest.ResponseKind `json:"kind"`
	Actor          string                    `json:"actor"`
	Message        string                    `json:"message,omitempty"`
	IdempotencyKey string                    `json:"idempotency_key,omitempty"`
}

type websocketActiveRequest struct {
	requestID string
	context   channel.InboundContext
	runIDs    map[string]struct{}
	seen      map[string]struct{}
	mu        sync.Mutex
}

func (s *Server) websocketMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defaultEntrypointID := firstNonEmpty(r.URL.Query().Get("entrypoint_id"), websocketDefaultEntrypoint)
	var writeMu sync.Mutex
	active := map[string]*websocketActiveRequest{}
	var activeMu sync.Mutex

	writeFrame := func(frame websocketOutboundFrame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return wsjson.Write(ctx, conn, frame)
	}
	addActive := func(req *websocketActiveRequest) {
		activeMu.Lock()
		defer activeMu.Unlock()
		active[req.requestID] = req
	}
	removeActive := func(requestID string) {
		activeMu.Lock()
		defer activeMu.Unlock()
		delete(active, requestID)
	}
	snapshotActive := func() []*websocketActiveRequest {
		activeMu.Lock()
		defer activeMu.Unlock()
		out := make([]*websocketActiveRequest, 0, len(active))
		for _, req := range active {
			out = append(out, req)
		}
		return out
	}

	go s.pumpWebSocketEvents(ctx, writeFrame, snapshotActive)

	for {
		frame, err := readWebSocketInboundFrame(ctx, conn)
		if err != nil {
			requestID := ""
			var badJSON websocketBadJSONError
			switch {
			case errors.As(err, &badJSON):
				_ = writeFrame(websocketErrorFrame("", requestID, "bad_json", badJSON.Error(), false))
				continue
			case errors.Is(err, errWebSocketUnsupportedMessage):
				_ = writeFrame(websocketErrorFrame("", requestID, "unsupported_type", err.Error(), false))
				continue
			}
			return
		}
		requestID := websocketRequestID(frame)
		switch strings.TrimSpace(frame.Type) {
		case "hello":
			var data websocketHelloData
			if len(frame.Data) > 0 {
				if err := json.Unmarshal(frame.Data, &data); err != nil {
					_ = writeFrame(websocketErrorFrame(frame.ID, requestID, "bad_json", err.Error(), false))
					continue
				}
			}
			if strings.TrimSpace(data.EntrypointID) != "" {
				defaultEntrypointID = strings.TrimSpace(data.EntrypointID)
			}
			_ = writeFrame(websocketOutboundFrame{
				Type:      "ready",
				ID:        "srv_ready_" + requestID,
				RequestID: requestID,
				Data: map[string]any{
					"channel":       websocketChannel,
					"entrypoint_id": defaultEntrypointID,
					"server":        "xira",
					"capabilities":  websocketCapabilities,
				},
			})
		case "message":
			s.handleWebSocketMessage(ctx, frame, defaultEntrypointID, writeFrame, addActive, removeActive)
		case "human_response":
			s.handleWebSocketHumanResponse(ctx, frame, writeFrame)
		case "ping":
			_ = writeFrame(websocketOutboundFrame{
				Type:      "pong",
				ID:        "srv_pong_" + requestID,
				RequestID: requestID,
			})
		default:
			_ = writeFrame(websocketErrorFrame(frame.ID, requestID, "unsupported_type", fmt.Sprintf("unsupported frame type %q", frame.Type), false))
		}
	}
}

func readWebSocketInboundFrame(ctx context.Context, conn *websocket.Conn) (websocketInboundFrame, error) {
	typ, reader, err := conn.Reader(ctx)
	if err != nil {
		return websocketInboundFrame{}, err
	}
	if typ != websocket.MessageText {
		return websocketInboundFrame{}, errWebSocketUnsupportedMessage
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return websocketInboundFrame{}, err
	}
	var frame websocketInboundFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return websocketInboundFrame{}, websocketBadJSONError{err: err}
	}
	return frame, nil
}

func (s *Server) pumpWebSocketEvents(ctx context.Context, writeFrame func(websocketOutboundFrame) error, snapshotActive func() []*websocketActiveRequest) {
	events := s.runtime.EventBus().Subscribe(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			for _, req := range snapshotActive() {
				if !req.acceptEvent(evt) {
					continue
				}
				if err := writeFrame(websocketRuntimeEventFrame(req.requestID, evt)); err != nil {
					return
				}
			}
		}
	}
}

func (s *Server) handleWebSocketMessage(
	ctx context.Context,
	frame websocketInboundFrame,
	defaultEntrypointID string,
	writeFrame func(websocketOutboundFrame) error,
	addActive func(*websocketActiveRequest),
	removeActive func(string),
) {
	requestID := websocketRequestID(frame)
	var data websocketMessageData
	if err := json.Unmarshal(frame.Data, &data); err != nil {
		_ = writeFrame(websocketErrorFrame(frame.ID, requestID, "bad_json", err.Error(), false))
		return
	}
	prepared, errFrame := s.prepareWebSocketTurn(frame, data, defaultEntrypointID)
	if errFrame != nil {
		_ = writeFrame(*errFrame)
		return
	}
	if !prepared.handle {
		_ = writeFrame(websocketOutboundFrame{
			Type:      "ack",
			ID:        "srv_ack_" + requestID,
			RequestID: requestID,
			Data: map[string]any{
				"status": "ignored",
				"reason": "unmentioned_group_message",
			},
		})
		return
	}
	if !s.websocketDedupe.Begin(prepared.dedupeKey, time.Now()) {
		_ = writeFrame(websocketOutboundFrame{
			Type:      "ack",
			ID:        "srv_ack_" + requestID,
			RequestID: requestID,
			Data: map[string]any{
				"status":     "duplicate",
				"message_id": prepared.messageID,
			},
		})
		return
	}
	activeReq := &websocketActiveRequest{
		requestID: requestID,
		context:   prepared.eventContext,
		runIDs:    map[string]struct{}{},
		seen:      map[string]struct{}{},
	}
	addActive(activeReq)
	if err := writeFrame(websocketOutboundFrame{
		Type:      "ack",
		ID:        "srv_ack_" + requestID,
		RequestID: requestID,
		Data: map[string]any{
			"status":        "accepted",
			"entrypoint_id": prepared.eventContext.EntrypointID,
			"channel":       websocketChannel,
			"message_id":    prepared.messageID,
		},
	}); err != nil {
		s.websocketDedupe.Forget(prepared.dedupeKey)
		removeActive(requestID)
		return
	}
	go func() {
		defer removeActive(requestID)
		resp, runErr := s.runtime.RunAgent(ctx, prepared.turn)
		if runErr != nil {
			s.websocketDedupe.Forget(prepared.dedupeKey)
			_ = writeFrame(websocketErrorFrame(frame.ID, requestID, "run_failed", runErr.Error(), true))
			return
		}
		for _, evt := range resp.Events {
			if !activeReq.acceptEvent(evt) {
				continue
			}
			if err := writeFrame(websocketRuntimeEventFrame(requestID, evt)); err != nil {
				s.websocketDedupe.Forget(prepared.dedupeKey)
				return
			}
		}
		var out websocketOutboundFrame
		if resp.Interrupt != nil {
			out = websocketInterruptFrame(frame.ID, requestID, resp)
		} else {
			out = websocketResponseFrame(frame.ID, requestID, resp)
		}
		if err := writeFrame(out); err != nil {
			s.websocketDedupe.Forget(prepared.dedupeKey)
			return
		}
		s.websocketDedupe.Complete(prepared.dedupeKey, time.Now())
	}()
}

type preparedWebSocketTurn struct {
	turn         frt.TurnRequest
	eventContext channel.InboundContext
	dedupeKey    string
	messageID    string
	handle       bool
}

func (s *Server) prepareWebSocketTurn(frame websocketInboundFrame, data websocketMessageData, defaultEntrypointID string) (preparedWebSocketTurn, *websocketOutboundFrame) {
	requestID := websocketRequestID(frame)
	if strings.TrimSpace(data.Message) == "" {
		errFrame := websocketErrorFrame(frame.ID, requestID, "validation_failed", "data.message is required", false)
		return preparedWebSocketTurn{}, &errFrame
	}
	if strings.TrimSpace(data.Context.ChatID) == "" {
		errFrame := websocketErrorFrame(frame.ID, requestID, "validation_failed", "context.chat_id is required", false)
		return preparedWebSocketTurn{}, &errFrame
	}
	if strings.TrimSpace(data.Context.SenderID) == "" {
		errFrame := websocketErrorFrame(frame.ID, requestID, "validation_failed", "context.sender_id is required", false)
		return preparedWebSocketTurn{}, &errFrame
	}
	if data.Context.Channel != "" && normalizeChannel(data.Context.Channel) != websocketChannel {
		errFrame := websocketErrorFrame(frame.ID, requestID, "channel_conflict", `context.channel must be "websocket"`, false)
		return preparedWebSocketTurn{}, &errFrame
	}
	dataEntrypoint := strings.TrimSpace(data.EntrypointID)
	contextEntrypoint := strings.TrimSpace(data.Context.EntrypointID)
	if dataEntrypoint != "" && contextEntrypoint != "" && dataEntrypoint != contextEntrypoint {
		errFrame := websocketErrorFrame(frame.ID, requestID, "validation_failed", "data.entrypoint_id and context.entrypoint_id must match", false)
		return preparedWebSocketTurn{}, &errFrame
	}
	effectiveEntrypointID := firstNonEmpty(dataEntrypoint, contextEntrypoint, defaultEntrypointID, websocketDefaultEntrypoint)
	definition, found := s.findEntrypoint(effectiveEntrypointID)
	if found && normalizeChannel(definition.Channel) != websocketChannel {
		errFrame := websocketErrorFrame(frame.ID, requestID, "channel_conflict", fmt.Sprintf("entrypoint %q uses channel %q", definition.ID, definition.Channel), false)
		return preparedWebSocketTurn{}, &errFrame
	}
	if !found && effectiveEntrypointID != websocketDefaultEntrypoint {
		errFrame := websocketErrorFrame(frame.ID, requestID, "entrypoint_not_found", fmt.Sprintf("entrypoint %q not found", effectiveEntrypointID), false)
		return preparedWebSocketTurn{}, &errFrame
	}
	runEntrypointID := ""
	if found {
		runEntrypointID = effectiveEntrypointID
	}
	ctx := data.Context
	ctx.Channel = websocketChannel
	ctx.EntrypointID = runEntrypointID
	ctx = channel.NormalizeInboundContext(ctx)
	eventCtx := ctx
	eventCtx.EntrypointID = effectiveEntrypointID
	messageID := strings.TrimSpace(ctx.MessageID)
	if messageID == "" {
		messageID = strings.TrimSpace(frame.ID)
	}
	handle := shouldHandleWebSocketMessage(ctx, definition)
	return preparedWebSocketTurn{
		turn: frt.TurnRequest{
			EntrypointID: runEntrypointID,
			AgentID:      data.AgentID,
			Message:      data.Message,
			SessionID:    data.SessionID,
			Context:      ctx,
		},
		eventContext: eventCtx,
		dedupeKey:    websocketDedupeKey(effectiveEntrypointID, messageID),
		messageID:    messageID,
		handle:       handle,
	}, nil
}

func (s *Server) handleWebSocketHumanResponse(ctx context.Context, frame websocketInboundFrame, writeFrame func(websocketOutboundFrame) error) {
	requestID := websocketRequestID(frame)
	var data websocketHumanResponseData
	if err := json.Unmarshal(frame.Data, &data); err != nil {
		_ = writeFrame(websocketErrorFrame(frame.ID, requestID, "bad_json", err.Error(), false))
		return
	}
	if strings.TrimSpace(data.HumanRequestID) == "" {
		_ = writeFrame(websocketErrorFrame(frame.ID, requestID, "validation_failed", "data.human_request_id is required", false))
		return
	}
	resolved, err := s.runtime.ResolveHumanRequest(ctx, data.HumanRequestID, humanrequest.ResolveRequest{
		Kind:           data.Kind,
		Actor:          data.Actor,
		Message:        data.Message,
		IdempotencyKey: data.IdempotencyKey,
	})
	if err != nil {
		code := "internal_error"
		switch {
		case errors.Is(err, humanrequest.ErrNotFound):
			code = "validation_failed"
		case errors.Is(err, humanrequest.ErrConflict):
			code = "validation_failed"
		case errors.Is(err, humanrequest.ErrValidation):
			code = "validation_failed"
		}
		_ = writeFrame(websocketErrorFrame(frame.ID, requestID, code, err.Error(), false))
		return
	}
	_ = writeFrame(websocketOutboundFrame{
		Type:      "ack",
		ID:        "srv_ack_" + requestID,
		RequestID: requestID,
		Data: map[string]any{
			"status":           "accepted",
			"human_request_id": resolved.ID,
		},
	})
}

func (s *Server) findEntrypoint(entrypointID string) (entrypoints.Definition, bool) {
	entrypointID = strings.TrimSpace(entrypointID)
	if entrypointID == "" || s == nil || s.runtime == nil {
		return entrypoints.Definition{}, false
	}
	for _, definition := range s.runtime.Entrypoints() {
		if definition.ID == entrypointID {
			return definition, true
		}
	}
	return entrypoints.Definition{}, false
}

func shouldHandleWebSocketMessage(ctx channel.InboundContext, definition entrypoints.Definition) bool {
	if normalizeChannel(ctx.ChatType) != "group" {
		return true
	}
	return ctx.Mentioned || definition.RespondToUnmentionedGroupMessages
}

func (req *websocketActiveRequest) acceptEvent(evt frt.RuntimeEvent) bool {
	req.mu.Lock()
	defer req.mu.Unlock()
	if evt.ID != "" {
		if _, ok := req.seen[evt.ID]; ok {
			return false
		}
	}
	if evt.RunID != "" {
		if _, ok := req.runIDs[evt.RunID]; ok {
			req.markSeenLocked(evt)
			return true
		}
	}
	if evt.Correlation != nil {
		if _, ok := req.runIDs[evt.Correlation.ParentRunID]; ok {
			req.rememberEventRunsLocked(evt)
			req.markSeenLocked(evt)
			return true
		}
		if _, ok := req.runIDs[evt.Correlation.ChildRunID]; ok {
			req.rememberEventRunsLocked(evt)
			req.markSeenLocked(evt)
			return true
		}
	}
	if !websocketEventContextMatches(evt, req.context) {
		return false
	}
	req.rememberEventRunsLocked(evt)
	req.markSeenLocked(evt)
	return true
}

func (req *websocketActiveRequest) rememberEventRunsLocked(evt frt.RuntimeEvent) {
	if evt.RunID != "" {
		req.runIDs[evt.RunID] = struct{}{}
	}
	if evt.Correlation != nil {
		if evt.Correlation.ParentRunID != "" {
			req.runIDs[evt.Correlation.ParentRunID] = struct{}{}
		}
		if evt.Correlation.ChildRunID != "" {
			req.runIDs[evt.Correlation.ChildRunID] = struct{}{}
		}
	}
}

func (req *websocketActiveRequest) markSeenLocked(evt frt.RuntimeEvent) {
	if evt.ID != "" {
		req.seen[evt.ID] = struct{}{}
	}
}

func websocketEventContextMatches(evt frt.RuntimeEvent, ctx channel.InboundContext) bool {
	if normalizeChannel(eventField(evt, "channel")) != websocketChannel {
		return false
	}
	if ctx.EntrypointID != "" && eventField(evt, "entrypoint_id") != ctx.EntrypointID {
		return false
	}
	if ctx.ChatID != "" && eventField(evt, "chat_id") != ctx.ChatID {
		return false
	}
	if ctx.SenderID != "" && eventField(evt, "sender_id") != ctx.SenderID {
		return false
	}
	if ctx.MessageID != "" && eventField(evt, "message_id") != ctx.MessageID {
		return false
	}
	return true
}

func eventField(evt frt.RuntimeEvent, field string) string {
	if evt.Scope != nil {
		var value string
		switch field {
		case "channel":
			value = evt.Scope.Channel
		case "entrypoint_id":
			value = evt.Scope.EntrypointID
		case "chat_id":
			value = evt.Scope.ChatID
		case "sender_id":
			value = evt.Scope.SenderID
		case "message_id":
			value = evt.Scope.MessageID
		}
		if value != "" {
			return value
		}
	}
	return payloadString(evt.Payload, field)
}

func websocketRuntimeEventFrame(requestID string, evt frt.RuntimeEvent) websocketOutboundFrame {
	return websocketOutboundFrame{
		Type:      "event",
		ID:        "srv_evt_" + strings.TrimSpace(evt.ID),
		RequestID: requestID,
		RunID:     evt.RunID,
		Data: map[string]any{
			"event": evt,
		},
	}
}

func websocketResponseFrame(frameID, requestID string, resp frt.TurnResponse) websocketOutboundFrame {
	return websocketOutboundFrame{
		Type:      "response",
		ID:        "srv_resp_" + firstNonEmpty(frameID, requestID),
		RequestID: requestID,
		RunID:     resp.RunID,
		Data: map[string]any{
			"run_id":         resp.RunID,
			"agent_id":       resp.AgentID,
			"entrypoint_id":  resp.EntrypointID,
			"session_id":     resp.SessionID,
			"status":         resp.Status,
			"final_response": resp.FinalResponse,
			"content_format": "markdown",
			"usage":          resp.Usage,
			"verification":   resp.VerificationResult,
		},
	}
}

func websocketInterruptFrame(frameID, requestID string, resp frt.TurnResponse) websocketOutboundFrame {
	return websocketOutboundFrame{
		Type:      "interrupt",
		ID:        "srv_interrupt_" + firstNonEmpty(frameID, requestID),
		RequestID: requestID,
		RunID:     resp.RunID,
		Data: map[string]any{
			"run_id":         resp.RunID,
			"status":         resp.Status,
			"reason":         resp.Interrupt.Reason,
			"human_requests": resp.Interrupt.HumanRequests,
			"blocked_by":     resp.Interrupt.BlockedBy,
		},
	}
}

func websocketErrorFrame(frameID, requestID, code, message string, retryable bool) websocketOutboundFrame {
	return websocketOutboundFrame{
		Type:      "error",
		ID:        "srv_err_" + firstNonEmpty(frameID, requestID),
		RequestID: requestID,
		Data: map[string]any{
			"code":      code,
			"message":   message,
			"retryable": retryable,
		},
	}
}

func websocketRequestID(frame websocketInboundFrame) string {
	return firstNonEmpty(frame.RequestID, frame.ID)
}

func websocketDedupeKey(entrypointID, messageID string) string {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ""
	}
	return firstNonEmpty(entrypointID, websocketDefaultEntrypoint) + ":" + messageID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
