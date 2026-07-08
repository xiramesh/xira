package websocket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/entrypoints"
	frt "github.com/xiramesh/xira/internal/runtime"
)

// handle_connection_test.go: loopback tests for HandleConnection + the read/
// write IO helpers (readInboundFrame / writeJSON / payloadString), which were
// previously 0% covered because they need a real *websocket.Conn. We spin up
// an httptest server that Accept's and hands the conn to HandleConnection, then
// Dial from the test client. This is real loopback (not a fake conn), so it
// exercises the actual coder/websocket framing — the coverage it produces is
// legitimate, not mocked.

// wsTestServer runs HandleConnection on each accepted WS connection. Returns
// (server, runner). Close the server to end the test.
type wsTestServer struct {
	server *httptest.Server
	runner *Runner
}

func newWSLoopbackServer(t *testing.T, rt frt.Runtime) *wsTestServer {
	t.Helper()
	runner := newTestRunner(t, rt)
	srv := &wsTestServer{runner: runner}
	srv.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		runner.HandleConnection(r.Context(), c, "websocket-default")
	}))
	return srv
}

func (s *wsTestServer) wsURL() string {
	return "ws" + s.server.URL[4:] + "/"
}

func (s *wsTestServer) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, s.wsURL(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

func (s *wsTestServer) close() { s.server.Close() }

// writeFrameClient writes an inbound frame (as the test client).
func writeFrameClient(t *testing.T, c *websocket.Conn, frame any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// readFrameClient reads one outbound frame from the server.
func readFrameClient(t *testing.T, c *websocket.Conn) outboundFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var f outboundFrame
	if err := wsjson.Read(ctx, c, &f); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

// TestHandleConnectionPingPong covers the ping → pong branch + the read loop +
// writeJSON (the server writes a pong back over the real conn).
func TestHandleConnectionPingPong(t *testing.T) {
	srv := newWSLoopbackServer(t, newFakeRuntime())
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	writeFrameClient(t, c, inboundFrame{Type: "ping", ID: "ping-1"})
	pong := readFrameClient(t, c)
	if pong.Type != "pong" {
		t.Errorf("got %q, want pong", pong.Type)
	}
}

// TestHandleConnectionHelloReady covers the hello → ready branch.
func TestHandleConnectionHelloReady(t *testing.T) {
	srv := newWSLoopbackServer(t, newFakeRuntime())
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	helloData, _ := json.Marshal(helloData{EntrypointID: "ws-custom"})
	writeFrameClient(t, c, inboundFrame{Type: "hello", ID: "h1", Data: helloData})
	ready := readFrameClient(t, c)
	if ready.Type != "ready" {
		t.Errorf("got %q, want ready", ready.Type)
	}
	if d, _ := ready.Data.(map[string]any); d != nil {
		if eid, _ := d["entrypoint_id"].(string); eid != "ws-custom" {
			t.Errorf("ready entrypoint_id = %q, want ws-custom", eid)
		}
	}
}

// TestHandleConnectionHumanResponseRejected covers the human_response →
// unsupported_type error branch.
func TestHandleConnectionHumanResponseRejected(t *testing.T) {
	srv := newWSLoopbackServer(t, newFakeRuntime())
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	writeFrameClient(t, c, inboundFrame{Type: "human_response", ID: "hr1"})
	err := readFrameClient(t, c)
	if err.Type != "error" {
		t.Errorf("got %q, want error", err.Type)
	}
	if d, _ := err.Data.(map[string]any); d != nil {
		if code, _ := d["code"].(string); code != "unsupported_type" {
			t.Errorf("error code = %q, want unsupported_type", code)
		}
	}
}

// TestHandleConnectionUnsupportedType covers the default branch.
func TestHandleConnectionUnsupportedType(t *testing.T) {
	srv := newWSLoopbackServer(t, newFakeRuntime())
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	writeFrameClient(t, c, inboundFrame{Type: "bogus", ID: "b1"})
	err := readFrameClient(t, c)
	if err.Type != "error" {
		t.Errorf("got %q, want error", err.Type)
	}
}

// TestHandleConnectionMessageTurn covers the message branch end-to-end through
// the real loopback: client sends a message frame, server routes it, the turn
// runs, client reads ack + response.
func TestHandleConnectionMessageTurn(t *testing.T) {
	rt := newFakeRuntime()
	srv := newWSLoopbackServer(t, rt)
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	msgData, _ := json.Marshal(messageData{
		Message: "hi",
		Context: channel.InboundContext{Channel: "websocket", ChatID: "chat-lb", SenderID: "u", MessageID: "om_lb"},
	})
	writeFrameClient(t, c, inboundFrame{Type: "message", ID: "om_lb", Data: msgData})

	ack := readFrameClient(t, c)
	if ack.Type != "ack" {
		t.Fatalf("got %q, want ack", ack.Type)
	}
	resp := readFrameClient(t, c)
	if resp.Type != "response" {
		t.Errorf("got %q, want response", resp.Type)
	}
}

// TestHandleConnectionDisconnectRelease covers the disconnect path: when the
// client closes, the read loop exits, releaseConn runs, and the registry drops
// the connection's keys. This exercises the defer r.releaseConn(wsHandle) path.
func TestHandleConnectionDisconnectRelease(t *testing.T) {
	rt := newFakeRuntime()
	srv := newWSLoopbackServer(t, rt)
	defer srv.close()
	c := srv.dial(t)

	msgData, _ := json.Marshal(messageData{
		Message: "hi",
		Context: channel.InboundContext{Channel: "websocket", ChatID: "chat-disc", SenderID: "u", MessageID: "om_disc"},
	})
	writeFrameClient(t, c, inboundFrame{Type: "message", ID: "om_disc", Data: msgData})
	_ = readFrameClient(t, c) // ack
	_ = readFrameClient(t, c) // response

	// Close client → server read loop returns → releaseConn.
	c.Close(websocket.StatusNormalClosure, "")
	time.Sleep(100 * time.Millisecond)

	key := keyOf("chat-disc", "u")
	if got := srv.runner.lookupConn(key); got != nil {
		t.Errorf("registry still has conn after disconnect (releaseConn did not run)")
	}
}

// TestPrepareTurnValidation covers prepareTurn's validation error branches,
// which return error frames without starting a turn. These were uncovered
// because happy-path tests always used valid messages.
func TestPrepareTurnValidation(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	mkFrame := func(payload messageData) inboundFrame {
		raw, _ := json.Marshal(payload)
		return inboundFrame{Type: "message", ID: "om_v", Data: raw}
	}
	cases := []struct {
		name string
		data messageData
		want string
	}{
		{"empty message", messageData{Context: channel.InboundContext{ChatID: "c", SenderID: "s"}}, "validation_failed"},
		{"empty chat_id", messageData{Message: "hi", Context: channel.InboundContext{SenderID: "s"}}, "validation_failed"},
		{"empty sender_id", messageData{Message: "hi", Context: channel.InboundContext{ChatID: "c"}}, "validation_failed"},
		{"channel conflict", messageData{Message: "hi", Context: channel.InboundContext{ChatID: "c", SenderID: "s", Channel: "feishu"}}, "channel_conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errFrame := runner.prepareTurn(mkFrame(tc.data), tc.data, "websocket-default")
			if errFrame == nil {
				t.Fatalf("expected error frame")
			}
			d, _ := errFrame.Data.(map[string]any)
			if code, _ := d["code"].(string); code != tc.want {
				t.Errorf("code = %q, want %q", code, tc.want)
			}
		})
	}
}

// TestHandleConnectionBadJSON covers readInboundFrame's bad_json branch +
// badJSONError.Error (loopback: client sends invalid JSON, server replies with
// a bad_json error frame and continues).
func TestHandleConnectionBadJSON(t *testing.T) {
	srv := newWSLoopbackServer(t, newFakeRuntime())
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Write raw invalid JSON as a text message (bypass wsjson framing).
	if err := c.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	f := readFrameClient(t, c)
	if f.Type != "error" {
		t.Fatalf("got %q, want error", f.Type)
	}
	d, _ := f.Data.(map[string]any)
	if code, _ := d["code"].(string); code != "bad_json" {
		t.Errorf("code = %q, want bad_json", code)
	}
}

// TestPrepareTurnSenderAuthorization (#121): when AllowedSenderIDs is set,
// prepareTurn must mark the turn as not-handled for senders outside the list.
// The internal ignoreReason field is "sender_not_authorized" (for slog only).
// The client-visible ack does NOT carry this reason — it's generic
// "unmentioned_group_message" (see TestHandleConnectionUnauthorizedSenderGenericAck).
// This test pins the internal field; the ack-reason contract is pinned separately.
func TestPrepareTurnSenderAuthorization(t *testing.T) {
	rt := newFakeRuntime()
	rt.entrypoints = []entrypoints.Definition{{
		ID:               "ws-allowlist",
		Channel:          "websocket",
		AllowedSenderIDs: []string{"ou_allowed"},
	}}
	runner, err := NewRunner(entrypoints.Definition{ID: "ws-allowlist", Channel: "websocket"}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	runner.runtime = rt
	mkFrame := func(senderID string) (inboundFrame, messageData) {
		data := messageData{
			Message: "hi",
			Context: channel.InboundContext{ChatID: "c1", SenderID: senderID, Channel: "websocket", ChatType: "p2p"},
		}
		raw, _ := json.Marshal(data)
		return inboundFrame{Type: "message", ID: "om_sa", Data: raw}, data
	}
	// Unauthorized sender → handle=false + reason.
	frame, data := mkFrame("ou_blocked")
	prepared, errFrame := runner.prepareTurn(frame, data, "ws-allowlist")
	if errFrame != nil {
		t.Fatalf("unexpected errFrame for unauthorized sender: %+v", errFrame)
	}
	if prepared.handle {
		t.Error("unauthorized sender should have handle=false")
	}
	if prepared.ignoreReason != "sender_not_authorized" {
		t.Errorf("ignoreReason = %q, want sender_not_authorized", prepared.ignoreReason)
	}
	// Authorized sender → handle=true, no ignore reason.
	frame, data = mkFrame("ou_allowed")
	prepared, errFrame = runner.prepareTurn(frame, data, "ws-allowlist")
	if errFrame != nil {
		t.Fatalf("unexpected errFrame for authorized sender: %+v", errFrame)
	}
	if !prepared.handle {
		t.Error("authorized sender should have handle=true")
	}
	if prepared.ignoreReason != "" {
		t.Errorf("authorized sender ignoreReason = %q, want empty", prepared.ignoreReason)
	}
	// Owner bypass: unauthorized sender + owner resolver → handle=true.
	// Pin entrypointID propagation (#139 review): resolver must receive
	// definition.ID, not channel "websocket".
	owner := &wsStubOwner{}
	runner.ownerResolver = owner
	frame, data = mkFrame("ou_owner")
	prepared, errFrame = runner.prepareTurn(frame, data, "ws-allowlist")
	if errFrame != nil {
		t.Fatalf("unexpected errFrame for owner bypass: %+v", errFrame)
	}
	if !prepared.handle {
		t.Error("owner should bypass allowlist (handle=true)")
	}
	if owner.LastEntrypointID != "ws-allowlist" {
		t.Errorf("owner resolver received entrypointID = %q, want 'ws-allowlist' (definition.ID, not channel)", owner.LastEntrypointID)
	}
}

// TestRunnerSetOwnerResolver covers the setter (nil-safe + value injection).
// Previously 0% — directly relevant to #121.
func TestRunnerSetOwnerResolver(t *testing.T) {
	r := &Runner{}
	r.SetOwnerResolver(nil)
	if r.ownerResolver != nil {
		t.Error("SetOwnerResolver(nil) should leave field nil")
	}
	owner := &wsStubOwner{}
	r.SetOwnerResolver(owner)
	if r.ownerResolver == nil {
		t.Error("SetOwnerResolver(stub) should set field non-nil")
	}
}

// TestRunnerSetHITLResolver covers the HITL setter (nil-safe). Previously 0%.
func TestRunnerSetHITLResolver(t *testing.T) {
	r := &Runner{}
	r.SetHITLResolver(nil)
	if r.hitlResolver != nil {
		t.Error("SetHITLResolver(nil) should leave field nil")
	}
}

// wsStubOwner implements frt.OwnerResolver for websocket tests. Records the
// entrypointID so integration tests can assert runners pass definition.ID,
// not channel. See PR #139 review.
type wsStubOwner struct {
	LastEntrypointID string
}

func (s *wsStubOwner) IsOwner(_ context.Context, _, entrypointID string) bool {
	s.LastEntrypointID = entrypointID
	return true
}

// TestShouldHandleGroupMentionedAuthorized covers shouldHandle's group branch
// (mentioned=true + authorized sender → handle). Previously only p2p was
// tested via TestPrepareTurnSenderAuthorization.
func TestShouldHandleGroupMentionedAuthorized(t *testing.T) {
	ctx := channel.InboundContext{
		ChatType:  "group",
		Mentioned: true,
		SenderID:  "ou_ok",
		Channel:   "websocket",
	}
	def := entrypoints.Definition{AllowedSenderIDs: []string{"ou_ok"}}
	if !shouldHandle(ctx, "", def, nil) {
		t.Error("mentioned + authorized group message should be handled")
	}
	// group + not mentioned + respond-to-unmentioned=true → handled (if authed).
	def2 := entrypoints.Definition{RespondToUnmentionedGroupMessages: true, AllowedSenderIDs: []string{"ou_ok"}}
	if !shouldHandle(ctx, "", def2, nil) {
		t.Error("respond-all + authorized should be handled")
	}
	// group + not mentioned + respond-to-unmentioned=false → not handled.
	ctxNotMentioned := ctx
	ctxNotMentioned.Mentioned = false
	if shouldHandle(ctxNotMentioned, "", entrypoints.Definition{}, nil) {
		t.Error("unmentioned group + no respond-all should be ignored")
	}
}

// TestShouldHandleBindPreAuth covers #123 /bind pre-auth for websocket:
// unauthorized sender sending "/bind <code>" passes auth; plain msg rejected.
func TestShouldHandleBindPreAuth(t *testing.T) {
	ctx := channel.InboundContext{
		ChatType:  "group",
		Mentioned: true,
		SenderID:  "ou_stranger",
		Channel:   "websocket",
	}
	allowlist := entrypoints.Definition{
		ID:                                "ws-protected",
		RespondToUnmentionedGroupMessages: true,
		AllowedSenderIDs:                  []string{"ou_ok"},
	}
	// /bind from unauthorized sender → passes (pre-auth bypass).
	if !shouldHandle(ctx, "/bind WDJM-LHKD", allowlist, nil) {
		t.Error("/bind from unauthorized sender should pass pre-auth")
	}
	// plain message from unauthorized sender → rejected.
	if shouldHandle(ctx, "hello", allowlist, nil) {
		t.Error("plain message from unauthorized sender should be rejected")
	}
	// bare /bind (no code) → not a bind command, rejected.
	if shouldHandle(ctx, "/bind", allowlist, nil) {
		t.Error("bare /bind (no code) should not bypass auth")
	}
}

// TestHandleConnectionUnauthorizedSenderGenericAck (PR #134 review): when an
// unauthorized sender messages the bot via websocket, the ack must NOT reveal
// auth-failure — it returns the same generic "unmentioned_group_message"
// reason as the mention-gate path. The real reason stays in server logs only.
// This prevents leaking bot/auth existence to unauthorized clients.
func TestHandleConnectionUnauthorizedSenderGenericAck(t *testing.T) {
	rt := newFakeRuntime()
	rt.entrypoints = []entrypoints.Definition{{
		ID:               "websocket-default",
		Channel:          "websocket",
		AllowedSenderIDs: []string{"ou_allowed"},
	}}
	srv := newWSLoopbackServer(t, rt)
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	// Unauthorized sender sends a message.
	data := messageData{
		Message: "hi",
		Context: channel.InboundContext{ChatID: "c1", SenderID: "ou_blocked", Channel: "websocket"},
	}
	raw, _ := json.Marshal(data)
	writeFrameClient(t, c, inboundFrame{Type: "message", ID: "om_unauth", Data: raw})
	f := readFrameClient(t, c)
	d, _ := f.Data.(map[string]any)
	status, _ := d["status"].(string)
	reason, _ := d["reason"].(string)
	if status != "ignored" {
		t.Fatalf("ack status = %q, want ignored", status)
	}
	// CRITICAL: reason must NOT reveal auth failure. It must be the generic
	// mention-gate reason, indistinguishable from a normal ignore.
	if reason == "sender_not_authorized" {
		t.Errorf("ack reason leaked auth failure: %q — must be generic", reason)
	}
	if reason != "unmentioned_group_message" {
		t.Errorf("ack reason = %q, want generic 'unmentioned_group_message'", reason)
	}
}
