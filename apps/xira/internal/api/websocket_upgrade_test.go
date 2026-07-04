package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/xiramesh/xira/internal/channelcontrol"
	wschannel "github.com/xiramesh/xira/internal/channelrunner/websocket"
	"github.com/xiramesh/xira/internal/entrypoints"
	frt "github.com/xiramesh/xira/internal/runtime"
)

// wsTestFrame is the api-side test-only mirror of the websocket channel's
// outbound frame (now living unexported in channelrunner/websocket). These
// integration tests read frames off the wire as JSON, so a local struct with
// matching json tags is all that's needed — api tests must not reach into the
// channelrunner package.
type wsTestFrame struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Data      any    `json:"data,omitempty"`
}

// wsTestMaxFrameBytes mirrors the channelrunner/websocket maxFrameBytes const
// (1<<20). Local copy so the oversized-frame test can stay in the api package.
const wsTestMaxFrameBytes = 1 << 20

func TestWebSocketChannelMessageEmitsAckEventAndResponse(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newWebSocketTestServer(t, rt)
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	conn := dialWebSocketChannel(t, server)
	defer conn.CloseNow()

	writeWebSocketFrame(t, conn, map[string]any{
		"type": "hello",
		"id":   "hello_001",
		"data": map[string]any{"client_id": "api-test"},
	})
	ready := readWebSocketFrame(t, conn)
	if ready.Type != "ready" || frameDataString(ready, "entrypoint_id") != websocketDefaultEntrypoint {
		t.Fatalf("ready = %+v", ready)
	}
	assertWebSocketCapabilities(t, ready, []string{"message", "event", "response", "interrupt"})

	writeWebSocketFrame(t, conn, map[string]any{
		"type": "message",
		"id":   "msg_001",
		"data": map[string]any{
			"message": "hello over websocket",
			"context": map[string]any{
				"chat_id":    "chat-1",
				"sender_id":  "user-1",
				"message_id": "client-msg-001",
			},
		},
	})
	ack := readWebSocketFrame(t, conn)
	if ack.Type != "ack" || frameDataString(ack, "status") != "accepted" || frameDataString(ack, "channel") != "websocket" {
		t.Fatalf("ack = %+v", ack)
	}

	var sawStarted bool
	var response wsTestFrame
	for i := 0; i < 20; i++ {
		frame := readWebSocketFrame(t, conn)
		switch frame.Type {
		case "event":
			event := frame.Data.(map[string]any)["event"].(map[string]any)
			sawStarted = sawStarted || event["kind"] == "run.started"
			if frame.RequestID != "msg_001" || frame.RunID == "" {
				t.Fatalf("event frame missing correlation: %+v", frame)
			}
		case "response":
			response = frame
			i = 20
		}
	}
	if !sawStarted {
		t.Fatal("did not receive run.started event")
	}
	if response.Type != "response" || response.RunID == "" {
		t.Fatalf("response = %+v", response)
	}
	if frameDataString(response, "final_response") == "" || frameDataString(response, "content_format") != "markdown" {
		t.Fatalf("response data = %+v", response.Data)
	}
	responseData := frameDataMap(t, response)
	if responseData["started_at"] == "" || responseData["ended_at"] == "" || responseData["route_matched_by"] == "" {
		t.Fatalf("response missing timing/route fields: %+v", response.Data)
	}
	if _, ok := responseData["tool_calls"].([]any); !ok {
		t.Fatalf("response tool_calls = %+v", responseData["tool_calls"])
	}
	if _, ok := responseData["artifacts"].([]any); !ok {
		t.Fatalf("response artifacts = %+v", responseData["artifacts"])
	}
	runs, err := rt.RunStore().List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].SessionScope == nil || runs[0].SessionScope.Channel != "websocket" {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestWebSocketChannelUsesFrameIDAsMessageIDFallback(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newWebSocketTestServer(t, rt)
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	conn := dialWebSocketChannel(t, server)
	defer conn.CloseNow()

	writeWebSocketFrame(t, conn, map[string]any{
		"type": "message",
		"id":   "msg_without_context_id",
		"data": map[string]any{
			"message": "use frame id as message id",
			"context": map[string]any{
				"chat_id":   "chat-1",
				"sender_id": "user-1",
			},
		},
	})
	ack := readWebSocketFrame(t, conn)
	if ack.Type != "ack" || frameDataString(ack, "message_id") != "msg_without_context_id" {
		t.Fatalf("ack = %+v", ack)
	}

	var sawScopedEvent bool
	for i := 0; i < 20; i++ {
		frame := readWebSocketFrame(t, conn)
		if frame.Type == "response" {
			break
		}
		if frame.Type != "event" {
			continue
		}
		event := frameDataMap(t, frame)["event"].(map[string]any)
		scope, ok := event["scope"].(map[string]any)
		if ok && scope["message_id"] == "msg_without_context_id" {
			sawScopedEvent = true
		}
	}
	if !sawScopedEvent {
		t.Fatal("did not receive event scoped by fallback frame id")
	}
}

func TestWebSocketChannelRejectsMismatchedChannel(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newWebSocketTestServer(t, rt)
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	conn := dialWebSocketChannel(t, server)
	defer conn.CloseNow()

	writeWebSocketFrame(t, conn, map[string]any{
		"type": "message",
		"id":   "msg_bad_channel",
		"data": map[string]any{
			"message": "bad channel",
			"context": map[string]any{
				"channel":   "feishu",
				"chat_id":   "chat-1",
				"sender_id": "user-1",
			},
		},
	})
	errFrame := readWebSocketFrame(t, conn)
	if errFrame.Type != "error" || frameDataString(errFrame, "code") != "channel_conflict" {
		t.Fatalf("error frame = %+v", errFrame)
	}
}

func TestWebSocketChannelRejectsEntrypointMismatch(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newWebSocketTestServer(t, rt)
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	conn := dialWebSocketChannel(t, server)
	defer conn.CloseNow()

	writeWebSocketFrame(t, conn, map[string]any{
		"type": "message",
		"id":   "msg_bad_entrypoint",
		"data": map[string]any{
			"entrypoint_id": "websocket-a",
			"message":       "bad entrypoint",
			"context": map[string]any{
				"entrypoint_id": "websocket-b",
				"chat_id":       "chat-1",
				"sender_id":     "user-1",
			},
		},
	})
	errFrame := readWebSocketFrame(t, conn)
	if errFrame.Type != "error" || frameDataString(errFrame, "code") != "validation_failed" {
		t.Fatalf("error frame = %+v", errFrame)
	}
}

func TestWebSocketChannelIgnoresUnmentionedGroupMessage(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newWebSocketTestServer(t, rt)
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	conn := dialWebSocketChannel(t, server)
	defer conn.CloseNow()

	writeWebSocketFrame(t, conn, map[string]any{
		"type": "message",
		"id":   "msg_group_ignored",
		"data": map[string]any{
			"message": "unmentioned group",
			"context": map[string]any{
				"chat_id":    "group-1",
				"chat_type":  "group",
				"sender_id":  "user-1",
				"message_id": "group-msg-1",
				"mentioned":  false,
			},
		},
	})
	ack := readWebSocketFrame(t, conn)
	if ack.Type != "ack" || frameDataString(ack, "status") != "ignored" || frameDataString(ack, "reason") != "unmentioned_group_message" {
		t.Fatalf("ack = %+v", ack)
	}
	runs, err := rt.RunStore().List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("ignored group message triggered runs: %+v", runs)
	}
}

func TestWebSocketChannelRejectsHumanResponseUntilResumeBindingExists(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newWebSocketTestServer(t, rt)
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	conn := dialWebSocketChannel(t, server)
	defer conn.CloseNow()

	writeWebSocketFrame(t, conn, map[string]any{
		"type": "human_response",
		"id":   "hrsp_001",
		"data": map[string]any{
			"human_request_id": "hrq_001",
			"kind":             "approve",
			"actor":            "user-1",
		},
	})
	errFrame := readWebSocketFrame(t, conn)
	if errFrame.Type != "error" || frameDataString(errFrame, "code") != "unsupported_type" {
		t.Fatalf("error frame = %+v", errFrame)
	}
}

func TestWebSocketChannelRejectsOversizedInboundFrame(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newWebSocketTestServer(t, rt)
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	conn := dialWebSocketChannel(t, server)
	defer conn.CloseNow()

	writeWebSocketFrame(t, conn, map[string]any{
		"type": "message",
		"id":   "msg_oversized",
		"data": map[string]any{
			"message": strings.Repeat("x", wsTestMaxFrameBytes+1),
			"context": map[string]any{
				"chat_id":   "chat-1",
				"sender_id": "user-1",
			},
		},
	})
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	var frame wsTestFrame
	if err := wsjson.Read(readCtx, conn, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "error" || frameDataString(frame, "code") != "validation_failed" {
		t.Fatalf("oversized response frame = %+v", frame)
	}
}

func TestWebSocketChannelDedupesMessageID(t *testing.T) {
	rt := newAPITestService(t, frt.Config{StateDir: t.TempDir()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newWebSocketTestServer(t, rt)
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	conn := dialWebSocketChannel(t, server)
	defer conn.CloseNow()

	message := map[string]any{
		"type": "message",
		"id":   "msg_dup_1",
		"data": map[string]any{
			"message": "dedupe me",
			"context": map[string]any{
				"chat_id":    "chat-1",
				"sender_id":  "user-1",
				"message_id": "client-msg-dup",
			},
		},
	}
	writeWebSocketFrame(t, conn, message)
	ack := readWebSocketFrame(t, conn)
	if ack.Type != "ack" || frameDataString(ack, "status") != "accepted" {
		t.Fatalf("first ack = %+v", ack)
	}
	for {
		frame := readWebSocketFrame(t, conn)
		if frame.Type == "response" {
			break
		}
	}

	message["id"] = "msg_dup_2"
	writeWebSocketFrame(t, conn, message)
	duplicate := readWebSocketFrame(t, conn)
	if duplicate.Type != "ack" || frameDataString(duplicate, "status") != "duplicate" {
		t.Fatalf("duplicate ack = %+v", duplicate)
	}
	runs, err := rt.RunStore().List()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("duplicate message triggered runs: %+v", runs)
	}
}

func dialWebSocketChannel(t *testing.T, server *Server) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+server.URL()[4:]+"/api/v1/channels/websocket/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// newWebSocketTestServer builds a Server whose ChannelControls expose a real
// websocket Runner built from rt. This wires the post-Step-3a path: the HTTP
// upgrade handler delegates to the Runner via wsRunnerProvider.
func newWebSocketTestServer(t *testing.T, rt *frt.Service) *Server {
	t.Helper()
	runner, err := wschannel.NewRunner(entrypoints.Definition{ID: "websocket-default", Channel: "websocket"}, rt, t.TempDir())
	if err != nil {
		t.Fatalf("wschannel.NewRunner: %v", err)
	}
	controls := &testWSControls{runner: runner}
	return NewServer(rt, "127.0.0.1:0", controls)
}

// testWSControls implements ChannelControls (no-ops for pairing methods, since
// these WS tests don't exercise pairing) and wsRunnerProvider (returns the
// websocket Runner).
type testWSControls struct{ runner *wschannel.Runner }

func (c *testWSControls) CreatePairing(context.Context, string) (channelcontrol.PairingSnapshot, error) {
	return channelcontrol.PairingSnapshot{}, nil
}
func (c *testWSControls) GetPairing(string, string) (channelcontrol.PairingSnapshot, error) {
	return channelcontrol.PairingSnapshot{}, nil
}
func (c *testWSControls) ListAccounts(string) ([]channelcontrol.AccountSnapshot, error) {
	return nil, nil
}
func (c *testWSControls) DeleteAccount(context.Context, string, string) error { return nil }
func (c *testWSControls) WSRunner() *wschannel.Runner                         { return c.runner }

func writeWebSocketFrame(t *testing.T, conn *websocket.Conn, frame any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, frame); err != nil {
		t.Fatal(err)
	}
}

func readWebSocketFrame(t *testing.T, conn *websocket.Conn) wsTestFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var frame wsTestFrame
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func frameDataString(frame wsTestFrame, key string) string {
	value, _ := frameDataMap(nil, frame)[key].(string)
	return value
}

func frameDataMap(t *testing.T, frame wsTestFrame) map[string]any {
	data, ok := frame.Data.(map[string]any)
	if !ok && t != nil {
		t.Fatalf("frame data = %+v", frame.Data)
	}
	return data
}

func assertWebSocketCapabilities(t *testing.T, frame wsTestFrame, want []string) {
	t.Helper()
	data, ok := frame.Data.(map[string]any)
	if !ok {
		t.Fatalf("frame data = %+v", frame.Data)
	}
	raw, ok := data["capabilities"].([]any)
	if !ok {
		t.Fatalf("capabilities = %+v", data["capabilities"])
	}
	if len(raw) != len(want) {
		t.Fatalf("capabilities len = %d, want %d: %+v", len(raw), len(want), raw)
	}
	for i, value := range raw {
		if value != want[i] {
			t.Fatalf("capabilities[%d] = %v, want %q; all=%+v", i, value, want[i], raw)
		}
	}
}
