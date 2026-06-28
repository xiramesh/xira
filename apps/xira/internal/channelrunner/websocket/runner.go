// Package websocket implements the websocket channel runner.
//
// This is the relocated home for what used to live in internal/api/websocket_channel.go
// (RFC chatkey-session Step 3a). The websocket channel is a channel implementation
// like ilink/feishu — it translates websocket frames ↔ TurnRequest/Response — and
// belongs here under channelrunner/, registered with Manager alongside the others.
// The api package keeps only the HTTP upgrade entry (websocketMessages), delegating
// per-connection work to this Runner.
//
// Concurrency: like ilink/feishu, a single Runner instance manages all connections
// for one websocket entrypoint. Per-connection state (write mutex, active-request
// table) is encapsulated in wsConnection, passed into Runner.HandleConnection —
// mirroring ilink's accountPoller pattern.
package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"log/slog"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/channelrunner/dedupe"
	"github.com/xiramesh/xira/internal/channelrunner/progress"
	"github.com/xiramesh/xira/internal/entrypoints"
	frt "github.com/xiramesh/xira/internal/runtime"
)

const (
	defaultEntrypoint = "websocket-default"
	dedupeTTL         = time.Hour
	writeTimeout      = 10 * time.Second
	maxFrameBytes     = 1 << 20
)

var capabilities = []string{
	"message",
	"event",
	"response",
	"interrupt",
}

var errUnsupportedMessage = errors.New("only JSON text frames are supported")
var errMessageTooBig = fmt.Errorf("websocket frame exceeds %d bytes", maxFrameBytes)

// --- frame types (moved verbatim from api/websocket_channel.go) ---

type badJSONError struct{ err error }

func (e badJSONError) Error() string { return e.err.Error() }

type inboundFrame struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type outboundFrame struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Data      any    `json:"data,omitempty"`
}

type helloData struct {
	ClientID     string `json:"client_id,omitempty"`
	EntrypointID string `json:"entrypoint_id,omitempty"`
}

type messageData struct {
	Message      string                 `json:"message"`
	EntrypointID string                 `json:"entrypoint_id,omitempty"`
	AgentID      string                 `json:"agent_id,omitempty"`
	SessionID    string                 `json:"session_id,omitempty"`
	Context      channel.InboundContext `json:"context"`
}

type activeRequest struct {
	requestID string
	context   channel.InboundContext
	runIDs    map[string]struct{}
	seen      map[string]struct{}
	mu        sync.Mutex
}

// --- Runner ---

// Runner is the websocket channel runner. One instance per websocket
// entrypoint, registered in channelrunner.Manager alongside ilink/feishu.
type Runner struct {
	definition entrypoints.Definition
	runtime    frt.Runtime
	router     *progress.Router
	dedupe     *dedupe.MessageDeduper
}

// NewRunner constructs a websocket Runner. rt may be nil in tests that inject
// a fake runtime via the (unexported) field afterwards.
func NewRunner(def entrypoints.Definition, rt *frt.Service, stateRoot string) (*Runner, error) {
	return &Runner{
		definition: def,
		runtime:    rt,
		router:     progress.NewRouter(),
		dedupe:     dedupe.New("", dedupeTTL),
	}, nil
}

func (r *Runner) ID() string      { return r.definition.ID }
func (r *Runner) Channel() string { return "websocket" }

// Start is a no-op: websocket is a passive server (connections arrive via the
// HTTP upgrade handler in api). There is nothing to connect out to up-front.
func (r *Runner) Start(ctx context.Context) error { return nil }

// Stop is a no-op for the same reason as Start; in-flight connections are
// cancelled by their own ctx (owned by the HTTP handler).
func (r *Runner) Stop(ctx context.Context) error { return nil }

// Emit delivers an OutboundEnvelope to the websocket channel. This satisfies
// channel.OutboundEmitter so resume (via Manager.Emit) can route a final
// response back to websocket. NOTE: websocket connections are short-lived and
// per-request; resume typically fires after the originating connection has
// closed, so Emit cannot reliably push to the original client. It logs a
// warning and returns nil (non-fatal) — resume delivery to a live client is a
// future concern (Step 3b), not a regression (today's websocket has no resume
// delivery path either).
func (r *Runner) Emit(ctx context.Context, env channel.OutboundEnvelope) error {
	slog.Warn("websocket Emit called but resume-to-live-connection is not yet supported",
		"channel", "websocket",
		"chat_id", env.Target.ChatID,
	)
	return nil
}

// HandleConnection services one websocket connection end-to-end. The HTTP
// upgrade (websocket.Accept) is performed by the api package; this method
// receives the already-accepted conn and runs the read loop. Per-connection
// state (writeMu, active-request table) is created here, local to the
// connection — mirroring ilink's per-accountPoller pattern.
//
// Turn lifetime vs connection lifetime: each dispatched turn runs in a router
// goroutine, derived from connCtx (the per-connection cancelable context
// below). If the client disconnects, connCtx is cancelled, which propagates
// into RunAgent's ctx and ends the turn. There is no goroutine leak. The
// reverse — a turn outliving the connection — can only happen if RunAgent has
// already begun and does not promptly honor ctx cancellation; in that case
// the turn finishes but its frames can't be written (writeFrame fails →
// failFast cancels connCtx → the turn's ctx is cancelled too). This is the
// intended fail-fast contract, not a leak.
func (r *Runner) HandleConnection(ctx context.Context, conn *websocket.Conn, defaultEntrypointID string) {
	// Derive a per-connection cancelable ctx so a write failure can fail-fast
	// the entire connection (cancel → read loop returns → connection closes).
	// Without this, writeFrame errors are dropped by callers (`_ = writeFrame`)
	// and the client would keep talking to a half-dead connection whose replies
	// silently disappear — silent data loss (AGENTS.md §2). Mirrors the
	// pre-Step-3a api behavior (cancel() on write error).
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	conn.SetReadLimit(-1)

	var writeMu sync.Mutex
	active := map[string]*activeRequest{}
	var activeMu sync.Mutex

	writeFrame := func(frame outboundFrame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := writeJSON(connCtx, conn, frame); err != nil {
			// Fail-fast: a write failure means the connection is broken (peer
			// gone, network split). Cancel connCtx so the read loop exits and
			// this connection tears down, instead of silently swallowing all
			// subsequent replies (the caller discards this error via `_ =`).
			cancel()
			return err
		}
		return nil
	}
	addActive := func(req *activeRequest) {
		activeMu.Lock()
		defer activeMu.Unlock()
		active[req.requestID] = req
	}
	removeActive := func(requestID string) {
		activeMu.Lock()
		defer activeMu.Unlock()
		delete(active, requestID)
	}

	if strings.TrimSpace(defaultEntrypointID) == "" {
		defaultEntrypointID = defaultEntrypoint
	}

	for {
		frame, err := readInboundFrame(connCtx, conn)
		if err != nil {
			requestID := ""
			var badJSON badJSONError
			switch {
			case errors.As(err, &badJSON):
				_ = writeFrame(errorFrame("", requestID, "bad_json", badJSON.Error(), false))
				continue
			case errors.Is(err, errUnsupportedMessage):
				_ = writeFrame(errorFrame("", requestID, "unsupported_type", err.Error(), false))
				continue
			case errors.Is(err, errMessageTooBig):
				_ = writeFrame(errorFrame("", requestID, "validation_failed", err.Error(), false))
				return
			}
			return
		}
		requestID := requestIDOf(frame)
		switch strings.TrimSpace(frame.Type) {
		case "hello":
			var data helloData
			if len(frame.Data) > 0 {
				if err := json.Unmarshal(frame.Data, &data); err != nil {
					_ = writeFrame(errorFrame(frame.ID, requestID, "bad_json", err.Error(), false))
					continue
				}
			}
			if strings.TrimSpace(data.EntrypointID) != "" {
				defaultEntrypointID = strings.TrimSpace(data.EntrypointID)
			}
			_ = writeFrame(outboundFrame{
				Type:      "ready",
				ID:        "srv_ready_" + requestID,
				RequestID: requestID,
				Data: map[string]any{
					"channel":       "websocket",
					"entrypoint_id": defaultEntrypointID,
					"server":        "xira",
					"capabilities":  capabilities,
				},
			})
		case "message":
			r.handleMessage(connCtx, frame, defaultEntrypointID, writeFrame, addActive, removeActive)
		case "human_response":
			_ = writeFrame(errorFrame(frame.ID, requestID, "unsupported_type", "human_response is reserved for a later websocket resume slice; use the HTTP human-request API for now", false))
		case "ping":
			_ = writeFrame(outboundFrame{
				Type:      "pong",
				ID:        "srv_pong_" + requestID,
				RequestID: requestID,
			})
		default:
			_ = writeFrame(errorFrame(frame.ID, requestID, "unsupported_type", fmt.Sprintf("unsupported frame type %q", frame.Type), false))
		}
	}
}

// handleMessage processes one inbound "message" frame: validates, dedupes,
// acks, then dispatches a turn via ChatKeySession (per-chatKey single-active
// protection — fixes the pre-Step-3a race where each frame spawned a
// concurrent go-routine calling RunAgent).
func (r *Runner) handleMessage(
	ctx context.Context,
	frame inboundFrame,
	defaultEntrypointID string,
	writeFrame func(outboundFrame) error,
	addActive func(*activeRequest),
	removeActive func(string),
) {
	requestID := requestIDOf(frame)
	var data messageData
	if err := json.Unmarshal(frame.Data, &data); err != nil {
		_ = writeFrame(errorFrame(frame.ID, requestID, "bad_json", err.Error(), false))
		return
	}
	prepared, errFrame := r.prepareTurn(frame, data, defaultEntrypointID)
	if errFrame != nil {
		_ = writeFrame(*errFrame)
		return
	}
	if !prepared.handle {
		_ = writeFrame(outboundFrame{
			Type:      "ack",
			ID:        "srv_ack_" + requestID,
			RequestID: requestID,
			Data:      map[string]any{"status": "ignored", "reason": "unmentioned_group_message"},
		})
		return
	}
	if !r.dedupe.Begin(prepared.dedupeKey, time.Now()) {
		_ = writeFrame(outboundFrame{
			Type:      "ack",
			ID:        "srv_ack_" + requestID,
			RequestID: requestID,
			Data:      map[string]any{"status": "duplicate", "message_id": prepared.messageID},
		})
		return
	}
	activeReq := &activeRequest{
		requestID: requestID,
		context:   prepared.eventContext,
		runIDs:    map[string]struct{}{},
		seen:      map[string]struct{}{},
	}
	addActive(activeReq)
	if err := writeFrame(outboundFrame{
		Type:      "ack",
		ID:        "srv_ack_" + requestID,
		RequestID: requestID,
		Data: map[string]any{
			"status":        "accepted",
			"entrypoint_id": prepared.eventContext.EntrypointID,
			"channel":       "websocket",
			"message_id":    prepared.messageID,
		},
	}); err != nil {
		r.dedupe.Forget(prepared.dedupeKey)
		removeActive(requestID)
		return
	}

	// Dispatch the turn via ChatKeySession (structured-output path: OnTurnResult).
	// Router provides per-chatKey single-active protection + steering. The turn
	// runs in a router goroutine; OnTurnResult writes frames back via writeFrame.
	//
	// Inbound = prepared.turn.Context (NOT eventContext): turn.Context carries
	// runEntrypointID (empty when the entrypoint isn't registered, so RunAgent
	// uses its default agent), whereas eventContext carries the effective ID for
	// event-routing/filtering. Matching the pre-Step-3a behavior exactly.
	chatKey := frt.ChatKeyFromInbound(prepared.eventContext)
	frameID := frame.ID
	session := progress.NewChatKeySession(chatKey, r.router, progress.ChatKeySessionConfig{
		Runtime:      r.runtime,
		EntrypointID: prepared.turn.EntrypointID,
		Inbound:      prepared.turn.Context,
		OnTurnResult: func(resp frt.TurnResponse, runErr error) {
			defer removeActive(requestID)
			if runErr != nil {
				r.dedupe.Forget(prepared.dedupeKey)
				_ = writeFrame(errorFrame(frameID, requestID, "run_failed", runErr.Error(), true))
				return
			}
			for _, evt := range resp.Events {
				if !activeReq.acceptEvent(evt) {
					continue
				}
				if err := writeFrame(runtimeEventFrame(requestID, evt)); err != nil {
					r.dedupe.Forget(prepared.dedupeKey)
					return
				}
			}
			var out outboundFrame
			if resp.Interrupt != nil {
				out = interruptFrame(frameID, requestID, resp)
			} else {
				out = responseFrame(frameID, requestID, resp)
			}
			if err := writeFrame(out); err != nil {
				r.dedupe.Forget(prepared.dedupeKey)
				return
			}
			r.dedupe.Complete(prepared.dedupeKey, time.Now())
		},
		SpawnResetter: func() {
			if c := r.router.SpawnCollectorFor(chatKey); c != nil {
				c.Reset()
			}
		},
	})
	session.Handle(ctx, prepared.turn.Message)
}

type preparedTurn struct {
	turn         frt.TurnRequest
	eventContext channel.InboundContext
	dedupeKey    string
	messageID    string
	handle       bool
}

func (r *Runner) prepareTurn(frame inboundFrame, data messageData, defaultEntrypointID string) (preparedTurn, *outboundFrame) {
	requestID := requestIDOf(frame)
	if strings.TrimSpace(data.Message) == "" {
		errFrame := errorFrame(frame.ID, requestID, "validation_failed", "data.message is required", false)
		return preparedTurn{}, &errFrame
	}
	if strings.TrimSpace(data.Context.ChatID) == "" {
		errFrame := errorFrame(frame.ID, requestID, "validation_failed", "context.chat_id is required", false)
		return preparedTurn{}, &errFrame
	}
	if strings.TrimSpace(data.Context.SenderID) == "" {
		errFrame := errorFrame(frame.ID, requestID, "validation_failed", "context.sender_id is required", false)
		return preparedTurn{}, &errFrame
	}
	if data.Context.Channel != "" && normalizeChannel(data.Context.Channel) != "websocket" {
		errFrame := errorFrame(frame.ID, requestID, "channel_conflict", `context.channel must be "websocket"`, false)
		return preparedTurn{}, &errFrame
	}
	dataEntrypoint := strings.TrimSpace(data.EntrypointID)
	contextEntrypoint := strings.TrimSpace(data.Context.EntrypointID)
	if dataEntrypoint != "" && contextEntrypoint != "" && dataEntrypoint != contextEntrypoint {
		errFrame := errorFrame(frame.ID, requestID, "validation_failed", "data.entrypoint_id and context.entrypoint_id must match", false)
		return preparedTurn{}, &errFrame
	}
	effectiveEntrypointID := firstNonEmpty(dataEntrypoint, contextEntrypoint, defaultEntrypointID, defaultEntrypoint)
	definition, found := r.findEntrypoint(effectiveEntrypointID)
	if found && normalizeChannel(definition.Channel) != "websocket" {
		errFrame := errorFrame(frame.ID, requestID, "channel_conflict", fmt.Sprintf("entrypoint %q uses channel %q", definition.ID, definition.Channel), false)
		return preparedTurn{}, &errFrame
	}
	if !found && effectiveEntrypointID != defaultEntrypoint {
		errFrame := errorFrame(frame.ID, requestID, "entrypoint_not_found", fmt.Sprintf("entrypoint %q not found", effectiveEntrypointID), false)
		return preparedTurn{}, &errFrame
	}
	if agentID := strings.TrimSpace(data.AgentID); agentID != "" && found && !definition.AllowsAgent(agentID) {
		errFrame := errorFrame(frame.ID, requestID, "agent_not_allowed", fmt.Sprintf("agent %q is not allowed by entrypoint %q", agentID, definition.ID), false)
		return preparedTurn{}, &errFrame
	}
	runEntrypointID := ""
	if found {
		runEntrypointID = effectiveEntrypointID
	}
	ctx := data.Context
	ctx.Channel = "websocket"
	ctx.EntrypointID = runEntrypointID
	ctx = channel.NormalizeInboundContext(ctx)
	eventCtx := ctx
	eventCtx.EntrypointID = effectiveEntrypointID
	messageID := strings.TrimSpace(ctx.MessageID)
	if messageID == "" {
		messageID = strings.TrimSpace(frame.ID)
	}
	if messageID == "" {
		errFrame := errorFrame(frame.ID, requestID, "validation_failed", "context.message_id or frame.id is required", false)
		return preparedTurn{}, &errFrame
	}
	ctx.MessageID = messageID
	eventCtx.MessageID = messageID
	handle := shouldHandle(ctx, definition)
	return preparedTurn{
		turn: frt.TurnRequest{
			EntrypointID: runEntrypointID,
			AgentID:      data.AgentID,
			Message:      data.Message,
			SessionID:    data.SessionID,
			Context:      ctx,
		},
		eventContext: eventCtx,
		dedupeKey:    dedupeKey(effectiveEntrypointID, messageID),
		messageID:    messageID,
		handle:       handle,
	}, nil
}

func (r *Runner) findEntrypoint(entrypointID string) (entrypoints.Definition, bool) {
	entrypointID = strings.TrimSpace(entrypointID)
	if entrypointID == "" || r.runtime == nil {
		return entrypoints.Definition{}, false
	}
	if ep, ok := r.runtime.(interface {
		Entrypoints() []entrypoints.Definition
	}); ok {
		for _, definition := range ep.Entrypoints() {
			if definition.ID == entrypointID {
				return definition, true
			}
		}
	}
	return entrypoints.Definition{}, false
}

func shouldHandle(ctx channel.InboundContext, definition entrypoints.Definition) bool {
	if normalizeChannel(ctx.ChatType) != "group" {
		return true
	}
	return ctx.Mentioned || definition.RespondToUnmentionedGroupMessages
}

func (req *activeRequest) acceptEvent(evt frt.RuntimeEvent) bool {
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
	if !eventContextMatches(evt, req.context) {
		return false
	}
	req.rememberEventRunsLocked(evt)
	req.markSeenLocked(evt)
	return true
}

func (req *activeRequest) rememberEventRunsLocked(evt frt.RuntimeEvent) {
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

func (req *activeRequest) markSeenLocked(evt frt.RuntimeEvent) {
	if evt.ID != "" {
		req.seen[evt.ID] = struct{}{}
	}
}

func eventContextMatches(evt frt.RuntimeEvent, ctx channel.InboundContext) bool {
	if normalizeChannel(eventField(evt, "channel")) != "websocket" {
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

func runtimeEventFrame(requestID string, evt frt.RuntimeEvent) outboundFrame {
	return outboundFrame{
		Type:      "event",
		ID:        "srv_evt_" + strings.TrimSpace(evt.ID),
		RequestID: requestID,
		RunID:     evt.RunID,
		Data:      map[string]any{"event": evt},
	}
}

func responseFrame(frameID, requestID string, resp frt.TurnResponse) outboundFrame {
	toolCalls := resp.ToolCalls
	if toolCalls == nil {
		toolCalls = []frt.ToolCallRecord{}
	}
	artifacts := resp.Artifacts
	if artifacts == nil {
		artifacts = []string{}
	}
	return outboundFrame{
		Type:      "response",
		ID:        "srv_resp_" + firstNonEmpty(frameID, requestID),
		RequestID: requestID,
		RunID:     resp.RunID,
		Data: map[string]any{
			"run_id":           resp.RunID,
			"agent_id":         resp.AgentID,
			"entrypoint_id":    resp.EntrypointID,
			"session_id":       resp.SessionID,
			"route_matched_by": resp.RouteMatchedBy,
			"status":           resp.Status,
			"final_response":   resp.FinalResponse,
			"content_format":   "markdown",
			"started_at":       resp.StartedAt,
			"ended_at":         resp.EndedAt,
			"tool_calls":       toolCalls,
			"artifacts":        artifacts,
			"usage":            resp.Usage,
			"verification":     resp.VerificationResult,
		},
	}
}

func interruptFrame(frameID, requestID string, resp frt.TurnResponse) outboundFrame {
	return outboundFrame{
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

func errorFrame(frameID, requestID, code, message string, retryable bool) outboundFrame {
	return outboundFrame{
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

func requestIDOf(frame inboundFrame) string {
	return firstNonEmpty(frame.RequestID, frame.ID)
}

func dedupeKey(entrypointID, messageID string) string {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return ""
	}
	return firstNonEmpty(entrypointID, defaultEntrypoint) + ":" + messageID
}

func readInboundFrame(ctx context.Context, conn *websocket.Conn) (inboundFrame, error) {
	typ, reader, err := conn.Reader(ctx)
	if err != nil {
		if errors.Is(err, websocket.ErrMessageTooBig) {
			return inboundFrame{}, errMessageTooBig
		}
		return inboundFrame{}, err
	}
	if typ != websocket.MessageText {
		return inboundFrame{}, errUnsupportedMessage
	}
	limited := io.LimitReader(reader, maxFrameBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return inboundFrame{}, err
	}
	if len(raw) > maxFrameBytes {
		return inboundFrame{}, errMessageTooBig
	}
	var frame inboundFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return inboundFrame{}, badJSONError{err}
	}
	return frame, nil
}

func writeJSON(ctx context.Context, conn *websocket.Conn, value any) error {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(writeCtx, conn, value)
}

// --- local helpers (not shared with api pkg — each pkg keeps its own copy) ---

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// normalizeChannel lowercases and trims a channel name. Own copy (api/server.go
// has one too) — kept local to avoid a channelrunner → api dependency.
func normalizeChannel(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// payloadString reads a string field from a RuntimeEvent payload map.
func payloadString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	v, ok := payload[key]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	default:
		return fmt.Sprint(v)
	}
}
