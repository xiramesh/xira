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
