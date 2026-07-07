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
	definition    entrypoints.Definition
	runtime       frt.Runtime
	hitlResolver  frt.HITLResolver
	ownerResolver frt.OwnerResolver
	router        *progress.Router
	dedupe        *dedupe.MessageDeduper

	// conns is the per-Runner connection registry (RFC chatkey-session Step 3b).
	// It maps a ChatKey to its single live connection, so outbound delivery
	// (Emit / resume final) can find the client that originated a turn. This is
	// websocket's analogue of ilink's accounts map (ilink/runner.go:83) and
	// feishu's stable chatID — websocket connections are short-lived, so the
	// registry tracks whichever is currently connected per chat key.
	//
	// Single-connection contract (round-7+): one ChatKey has at most one LIVE
	// owner. A new connection for a key held by a LIVE owner is REJECTED (the
	// old owner stays). A new connection may take over only when the prior owner
	// is STALE (lastSeen expired) AND no turn is active on it. The old owner is
	// NOT cancelled — cancelling would murder an active turn whose ctx derives
	// from the old connCtx (the cascade that drove rounds 2-7's reviews).
	connMu    sync.Mutex
	conns     map[frt.ChatKey]*wsConn
	connIDSeq uint64 // monotonic connection identity, for same-conn detection
}

// wsConn holds a live websocket connection's send capability. id is a stable
// identity (funcs can't be compared in Go). lastSeen is updated on every frame
// received from this connection, so registerConnKey can tell a STALE owner (no
// frames for > staleThreshold → presumed dead) from a live one.
//
// Single-connection contract (PR #97 round-7): one ChatKey has at most one
// live owner. A new connection registering a key held by a LIVE owner is
// REJECTED (the client should not open a second connection for the same chat).
// A new connection may take over only when the prior owner is STALE (its
// underlying socket died without the server noticing). This eliminates the
// multi-connection takeover that drove rounds 2-7's boundary cascade.
type wsConn struct {
	id       uint64
	send     func(outboundFrame) error
	cancel   context.CancelFunc
	lastSeen time.Time
	keys     map[frt.ChatKey]struct{}
}

// staleThreshold is how long without any frame from a connection before it is
// presumed dead (its socket died unnoticed). A new connection may take over a
// key from a stale owner. Clients ping every ~15-30s, so 60s allows a couple
// missed pings before takeover.
const staleThreshold = 60 * time.Second

// NewRunner constructs a websocket Runner. rt may be nil in tests that inject
// a fake runtime via the (unexported) field afterwards.
func NewRunner(def entrypoints.Definition, rt *frt.Service, stateRoot string) (*Runner, error) {
	return &Runner{
		definition: def,
		runtime:    rt,
		router:     progress.NewRouter(),
		dedupe:     dedupe.New("", dedupeTTL),
		conns:      map[frt.ChatKey]*wsConn{},
	}, nil
}

func (r *Runner) ID() string      { return r.definition.ID }
func (r *Runner) Channel() string { return "websocket" }

// SetHITLResolver injects the HITL resolve capability for IM direct-answer (#92).
func (r *Runner) SetHITLResolver(resolver frt.HITLResolver) {
	if r != nil {
		r.hitlResolver = resolver
	}
}

// SetOwnerResolver injects the owner-query capability (#122). nil = allowlist-only auth.
func (r *Runner) SetOwnerResolver(resolver frt.OwnerResolver) {
	if r != nil {
		r.ownerResolver = resolver
	}
}

// Start is a no-op: websocket is a passive server (connections arrive via the
// HTTP upgrade handler in api). There is nothing to connect out to up-front.
func (r *Runner) Start(ctx context.Context) error { return nil }

// Stop is a no-op for the same reason as Start; in-flight connections are
// cancelled by their own ctx (owned by the HTTP handler).
func (r *Runner) Stop(ctx context.Context) error { return nil }

// Capabilities advertises what this channel can do. websocket supports
// proactive outbound (resume delivery to a live connection). Interactive
// human response (in-IM approve/deny via human_response frames) is a future
// concern — the inbound human_response frame is still rejected (see
// HandleConnection) — so we do NOT advertise CapabilityInteractiveHumanResponse
// yet (advertising an unimplemented capability would be a lie).
func (r *Runner) Capabilities() channel.CapabilitySet {
	return channel.CapabilitySet{
		channel.CapabilityProactiveOutbound,
	}
}

// Compile-time: *Runner implements channel.OutboundEmitter.
var _ channel.OutboundEmitter = (*Runner)(nil)

// Emit delivers an OutboundEnvelope to the websocket channel. It is the
// unified outbound surface used by the resume path (RFC #27 — stateless HITL
// resume): when a run resumed via HTTP/CLI produces a final, the runtime calls
// Manager.Emit, which routes here by Target.Channel == "websocket".
//
// Addressing: the envelope's Target.ChatID + Target.SenderID identify the chat
// key whose live connection should receive the frame. resume reconstructs
// Target from the run's persisted SessionScope, so the (chat, sender) here is
// the original inbound identity — matching the key the connection registered
// under in handleMessage.
//
// If no live connection is registered for the key (the client disconnected
// during the HITL pause — websocket connections are short-lived), Emit returns
// an error. This is best-effort: the resume path (human_request_resume.go:101)
// logs the error but does not fail the run, which is already persisted. We
// return an error rather than silently nil so the gap is observable, not
// swallowed (silent data loss, AGENTS.md §2).
//
// env.Type handling: OutboundAssistantFinal / OutboundProactiveMessage are
// delivered as a "response" frame (the same shape OnTurnResult sends, so the
// client's response handler covers both live turns and resumed finals).
func (r *Runner) Emit(_ context.Context, env channel.OutboundEnvelope) error {
	if env.Target == nil {
		return fmt.Errorf("websocket Emit: envelope has no target")
	}
	chatID := strings.TrimSpace(env.Target.ChatID)
	if chatID == "" {
		return fmt.Errorf("websocket Emit: target has no chat_id")
	}
	content := ""
	if env.Data != nil {
		if v, ok := env.Data["content"].(string); ok {
			content = v
		}
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("websocket Emit: envelope has no content")
	}
	switch env.Type {
	case channel.OutboundAssistantFinal, channel.OutboundProactiveMessage:
	default:
		return fmt.Errorf("websocket Emit: unsupported outbound type %q", env.Type)
	}
	conn := r.lookupConn(frt.ChatKeyFromInbound(*env.Target))
	if conn == nil {
		return fmt.Errorf("websocket Emit: no live connection for chat %q (connection may have closed)", chatID)
	}
	// Schema MUST match responseFrame (runner.go:responseFrame) so a WS client
	// reads resumed finals via the SAME handler as normal turn responses — same
	// type ("response"), same data fields. PR #97 review CRITICAL #1: a bespoke
	// "content" field broke this; clients reading "final_response" got empty.
	// Resume only knows run_id + final text (no agent/session/tool metadata —
	// those are TurnResponse fields the envelope doesn't carry), so they are
	// omitted; clients tolerant of absent fields read final_response fine.
	frame := outboundFrame{
		Type:      "response",
		ID:        "srv_resume_" + firstNonEmpty(env.RequestID, env.RunID),
		RequestID: env.RequestID,
		RunID:     env.RunID,
		Data: map[string]any{
			"run_id":         env.RunID,
			"status":         "completed",
			"final_response": content,
			"content_format": "markdown",
		},
	}
	return conn.send(frame)
}

// newConn constructs a wsConn handle for one physical websocket connection.
// The same *wsConn is reused across every ChatKey that connection registers
// (one connection may own multiple keys). id is assigned here so registerConnKey
// can detect "same connection re-registering a key" vs "a new connection took
// over" — comparing funcs is illegal in Go; the id is the safe identity token.
func (r *Runner) newConn(send func(outboundFrame) error, cancel context.CancelFunc) *wsConn {
	r.connMu.Lock()
	r.connIDSeq++
	id := r.connIDSeq
	r.connMu.Unlock()
	return &wsConn{id: id, send: send, cancel: cancel, lastSeen: time.Now(), keys: map[frt.ChatKey]struct{}{}}
}

// registerConnKey registers conn as the owner of key under the single-connection
// contract. Returns (displaced, rejected):
//   - same connection already owns key → (nil, false) no-op
//   - a different LIVE owner holds key → (existing, true) REJECTED — the caller
//     should tell the client "chat already has a connection"
//   - a different STALE owner holds key (no frame for > staleThreshold) →
//     (existing, false) take over; the prior owner is presumed dead (its socket
//     died without notice). The caller does NOT cancel the stale owner — it's
//     already dead; cancel would be moot.
//   - no prior owner → (nil, false) registered.
//
// The stale check is what makes "reject new" survivable for reconnects: if the
// old socket truly died, lastSeen stops updating and a new connection can take
// over after staleThreshold. This avoids the multi-connection takeover cascade
// (rounds 2-7) while not trapping users behind a dead connection.
func (r *Runner) registerConnKey(conn *wsConn, key frt.ChatKey) (displaced *wsConn, rejected bool) {
	if conn == nil {
		return nil, false
	}
	key = canonicalKey(key)
	r.connMu.Lock()
	defer r.connMu.Unlock()
	existing := r.conns[key]
	if existing != nil && existing.id == conn.id {
		return nil, false // same connection, no-op
	}
	if existing != nil && time.Since(existing.lastSeen) < staleThreshold {
		// Live owner — reject the new connection (single-connection contract).
		return existing, true
	}
	// Stale owner — only take over if NO turn is active for this key. If a turn
	// is still running on the stale owner, taking over would re-introduce the
	// "old turn alive, registry moved" boundary (round-8 WARNING). The stale
	// owner's turn keeps running on its own ctx; a fresh takeover happens only
	// after it completes (entry goes idle).
	if existing != nil && r.router.IsActive(key) {
		return existing, true // reject: turn still active on the stale owner
	}
	// No owner, or stale owner with no active turn — take over.
	r.conns[key] = conn
	conn.keys[key] = struct{}{}
	return existing, false
}

// releaseConn removes every key this connection still owns from the registry.
// Called from HandleConnection's defer on disconnect. Keys already taken over
// by a newer connection (registerConnKey returned them as displaced) are no
// longer mapped to conn, so they are skipped — the new owner must not be
// evicted. This is the multi-key generalization of the per-key ownership guard.
func (r *Runner) releaseConn(conn *wsConn) {
	if conn == nil {
		return
	}
	r.connMu.Lock()
	defer r.connMu.Unlock()
	for key := range conn.keys {
		if current, ok := r.conns[key]; ok && current.id == conn.id {
			delete(r.conns, key)
		}
	}
}

// lookupConn returns the live connection for key, or nil if none is registered.
func (r *Runner) lookupConn(key frt.ChatKey) *wsConn {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	return r.conns[canonicalKey(key)]
}

// canonicalKey lowercases ChatID and SenderID so the registry key is stable
// across the two sources of ChatKeys: inbound (original client case, e.g.
// "RoomA") and resume (reconstructed from SessionScope, where BuildScope
// lowercases — session/manager.go). Without this, a mixed-case client id
// registers under "RoomA" but resume looks up "rooma" → miss → "no live
// connection". Channel is already lowercase ("websocket") and left as-is.
// (PR #97 re-review WARNING #1.)
func canonicalKey(key frt.ChatKey) frt.ChatKey {
	return frt.ChatKey{
		Channel:  strings.ToLower(key.Channel),
		ChatID:   strings.ToLower(key.ChatID),
		SenderID: strings.ToLower(key.SenderID),
	}
}

// resolveSend returns the write function for the CURRENT live connection under
// chatKey. This is the dynamic counterpart to a captured writeFrame closure:
// instead of always writing to the connection that started the turn (which may
// have been superseded by a reconnect), it looks up the registry at write time
// so the frame reaches whoever is connected now.
//
// Why this exists (PR #97 round-3 review): steering runs the retried turn in
// the ORIGINAL turn's ChatKeySession, whose OnTurnResult closure captured the
// original connection's writeFrame. If connection B took over the ChatKey
// mid-turn, the retry's terminal frame was written to A — B got only an ack.
// resolveSend makes OnRawEvent/OnTurnResult write to the current registry
// connection instead of the captured one.
//
// Returns a function that reports a descriptive error when no live connection
// is registered (so the caller's `_ = send(...)` path stays observable rather
// than silently swallowing). ack/error frames written synchronously in
// handleMessage still use the captured writeFrame directly — at that point the
// connection is provably live (it just delivered the inbound frame).
func (r *Runner) resolveSend(chatKey frt.ChatKey) func(outboundFrame) error {
	return func(frame outboundFrame) error {
		conn := r.lookupConn(chatKey)
		if conn == nil || conn.send == nil {
			return fmt.Errorf("websocket: no live connection for %s (frame %q dropped)", chatKey, frame.Type)
		}
		return conn.send(frame)
	}
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

	// wsHandle is this connection's identity in the registry. One handle per
	// physical connection, reused across every ChatKey it registers (the
	// protocol allows multiple message frames with different chat/sender on one
	// connection). On disconnect, releaseConn removes ALL keys this handle
	// still owns. NewConn assigns a unique id so registerConnKey can tell
	// "same connection, another key" (no-op) from "a new connection took over
	// this key" (cancel the displaced one).
	wsHandle := r.newConn(nil, cancel)
	wsHandle.send = nil // set after writeFrame is defined below
	defer r.releaseConn(wsHandle)

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
	wsHandle.send = writeFrame
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
		// Mark this connection as live (seen a frame). registerConnKey uses
		// lastSeen to detect stale owners (single-connection takeover, round-7).
		r.connMu.Lock()
		wsHandle.lastSeen = time.Now()
		r.connMu.Unlock()
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
			r.handleMessage(connCtx, frame, defaultEntrypointID, writeFrame, addActive, removeActive, func(key frt.ChatKey) (*wsConn, bool) {
				// Register this connection under the message's chat key (single-
				// connection contract, PR #97 round-7). Returns (displaced, rejected):
				// rejected=true if a LIVE owner already holds the key.
				return r.registerConnKey(wsHandle, key)
			})
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
	onRegister func(key frt.ChatKey) (displaced *wsConn, rejected bool), // single-connection register
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
		reason := prepared.ignoreReason
		if reason == "" {
			reason = "unmentioned_group_message"
		}
		_ = writeFrame(outboundFrame{
			Type:      "ack",
			ID:        "srv_ack_" + requestID,
			RequestID: requestID,
			Data:      map[string]any{"status": "ignored", "reason": reason},
		})
		return
	}
	// Register the connection under the chat key (single-connection contract).
	// onRegister returns rejected=true if a LIVE owner already holds this key —
	// the client opened a second connection for the same chat. Reject it.
	chatKey := frt.ChatKeyFromInbound(prepared.eventContext)
	if _, rejected := onRegister(chatKey); rejected {
		_ = writeFrame(outboundFrame{
			Type:      "ack",
			ID:        "srv_ack_" + requestID,
			RequestID: requestID,
			Data:      map[string]any{"status": "rejected", "reason": "chat_already_has_connection"},
		})
		return
	}
	if !r.dedupe.Begin(prepared.dedupeKey, time.Now()) {
		// Duplicate: a retry/reconnect with the same message_id. Cite the active
		// turn's request_id if one is running (scheme P: reply follows active turn).
		activeID := r.router.ActiveRequestID(chatKey)
		ackData := map[string]any{"status": "duplicate", "message_id": prepared.messageID}
		if activeID != "" {
			ackData["reply_request_id"] = activeID
		}
		_ = writeFrame(outboundFrame{
			Type:      "ack",
			ID:        "srv_ack_" + requestID,
			RequestID: requestID,
			Data:      ackData,
		})
		return
	}
	// HITL direct-answer (#92): shared preflight check (same logic as feishu/ilink).
	// If this chatKey has a pending HITL (agent_request only), resolve it from
	// the user's message text and return. Resume runs async via Emit.
	if progress.TryResolveHITL(ctx, r.hitlResolver, chatKey, prepared.turn.Message, prepared.eventContext.SenderID) {
		_ = writeFrame(outboundFrame{
			Type:      "ack",
			ID:        "srv_ack_" + requestID,
			RequestID: requestID,
			Data:      map[string]any{"status": "hitl_resolved"},
		})
		return
	}
	// Build the turn's per-message state up front. activeReq is referenced by the
	// session's OnRawEvent/OnTurnResult closures, so it must exist before the
	// session — but it is only addActive'd for a STARTED turn (a steered message
	// gets no independent turn, so its activeReq is never tracked and OnTurnResult
	// never runs for it → no leak). send resolves to the live connection at write
	// time (so a stale-owner takeover's frames reach the new owner).
	activeReq := &activeRequest{
		requestID: requestID,
		context:   prepared.eventContext,
		runIDs:    map[string]struct{}{},
		seen:      map[string]struct{}{},
	}
	send := r.resolveSend(chatKey)
	frameID := frame.ID
	session := progress.NewChatKeySession(chatKey, r.router, progress.ChatKeySessionConfig{
		Runtime:      r.runtime,
		EntrypointID: prepared.turn.EntrypointID,
		Inbound:      prepared.turn.Context,
		// OnRawEvent: websocket already does its own event rendering (structured
		// event frame, not text). The closure captures activeReq (event routing
		// filter), requestID (frame correlation), and send (dynamic connection
		// resolution). evt.Scope carries Channel/ChatID/SenderID/MessageID —
		// websocket sends the full RuntimeEvent as a JSON frame so the client
		// has all context. No IMEventRenderer; ws is the "channel decides
		// rendering" model in action. prepared.turn.Context (= InboundConfig)
		// is available as Config.Inbound above for future extensions.
		OnRawEvent: func(evt frt.RuntimeEvent) {
			if !activeReq.acceptEvent(evt) {
				return
			}
			_ = send(runtimeEventFrame(requestID, evt))
		},
		OnTurnResult: func(resp frt.TurnResponse, runErr error) {
			defer removeActive(requestID)
			if runErr != nil {
				r.dedupe.Forget(prepared.dedupeKey)
				_ = send(errorFrame(frameID, requestID, "run_failed", runErr.Error(), true))
				return
			}
			var out outboundFrame
			if resp.Interrupt != nil {
				out = interruptFrame(frameID, requestID, resp)
			} else {
				out = responseFrame(frameID, requestID, resp)
			}
			if err := send(out); err != nil {
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
	// Route atomically (under Router's entry lock): decide started vs steered
	// WITHOUT starting the turn goroutine yet. The outcome drives the ack +
	// activeRequest lifecycle — steered messages must NOT be accepted/tracked
	// (they get no independent OnTurnResult/terminal), started ones must be
	// tracked + acked before the goroutine runs (PR #97 round-8: round-7's
	// over-contraction wrongly ignored the outcome and leaked activeRequest on
	// same-connection steering).
	outcome := session.Route(ctx, requestID, prepared.turn.Message)
	if outcome.Steered {
		// Steered: this is an interjection on the active turn. It will be drained
		// on the active turn's steering checkpoint; its reply comes via that turn's
		// OnTurnResult. Do NOT addActive (its OnTurnResult would never run → leak).
		// The steered ack cites reply_request_id so the client knows which
		// request_id the terminal carries (scheme P).
		_ = writeFrame(outboundFrame{
			Type:      "ack",
			ID:        "srv_ack_" + requestID,
			RequestID: requestID,
			Data: map[string]any{
				"status":           "steered",
				"message_id":       prepared.messageID,
				"reply_request_id": outcome.ActiveRequestID,
			},
		})
		return
	}
	// Started: this message began a turn (entry active=true, goroutine not yet
	// running). addActive + accepted ack, THEN Start — so a fast RunAgent cannot
	// emit a terminal frame or removeActive ahead of registration (round-5
	// ordering, retained under single-connection).
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
		// Ack write failed (connection dropped): forget dedupe, drop the
		// activeRequest, abort the Router entry (turn never runs).
		r.dedupe.Forget(prepared.dedupeKey)
		removeActive(requestID)
		outcome.Abort()
		return
	}
	outcome.Start()
}

type preparedTurn struct {
	turn         frt.TurnRequest
	eventContext channel.InboundContext
	dedupeKey    string
	messageID    string
	handle       bool
	ignoreReason string // populated when handle==false (unmentioned_group_message | sender_not_authorized)
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
	handle := shouldHandle(ctx, definition, r.ownerResolver)
	ignoreReason := ""
	if !handle {
		// Distinguish mention gate vs sender auth for ack clarity.
		if !definition.AllowsSender(ctx.SenderID) && (r.ownerResolver == nil || !r.ownerResolver.IsOwner(context.Background(), ctx.SenderID, ctx.Channel)) {
			ignoreReason = "sender_not_authorized"
		} else {
			ignoreReason = "unmentioned_group_message"
		}
	}
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
		ignoreReason: ignoreReason,
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

// shouldHandle decides whether websocket should process an inbound message.
// Two gates (AND): mention gate + sender authorization (#121).
func shouldHandle(ctx channel.InboundContext, definition entrypoints.Definition, owner frt.OwnerResolver) bool {
	if normalizeChannel(ctx.ChatType) != "group" {
		return isAuthorizedSender(ctx, definition, owner)
	}
	if !ctx.Mentioned && !definition.RespondToUnmentionedGroupMessages {
		return false
	}
	return isAuthorizedSender(ctx, definition, owner)
}

// isAuthorizedSender checks the sender allowlist (#121) with optional owner
// bypass (#122). Channel is read from ctx.Channel (websocket clients set it).
func isAuthorizedSender(ctx channel.InboundContext, definition entrypoints.Definition, owner frt.OwnerResolver) bool {
	if definition.AllowsSender(ctx.SenderID) {
		return true
	}
	if owner == nil {
		return false
	}
	return owner.IsOwner(context.Background(), ctx.SenderID, ctx.Channel)
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
