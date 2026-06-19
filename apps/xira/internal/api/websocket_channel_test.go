package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	frt "github.com/xiramesh/xira/internal/runtime"
)

func TestWebSocketChannelMessageEmitsAckEventAndResponse(t *testing.T) {
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
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
	var response websocketOutboundFrame
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
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
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
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
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
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
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
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
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
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
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
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	conn := dialWebSocketChannel(t, server)
	defer conn.CloseNow()

	writeWebSocketFrame(t, conn, map[string]any{
		"type": "message",
		"id":   "msg_oversized",
		"data": map[string]any{
			"message": strings.Repeat("x", websocketMaxFrameBytes+1),
			"context": map[string]any{
				"chat_id":   "chat-1",
				"sender_id": "user-1",
			},
		},
	})
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	var frame websocketOutboundFrame
	if err := wsjson.Read(readCtx, conn, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "error" || frameDataString(frame, "code") != "validation_failed" {
		t.Fatalf("oversized response frame = %+v", frame)
	}
}

func TestWebSocketChannelDedupesMessageID(t *testing.T) {
	rt := newAPITestService(t, frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
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

func writeWebSocketFrame(t *testing.T, conn *websocket.Conn, frame any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, conn, frame); err != nil {
		t.Fatal(err)
	}
}

func readWebSocketFrame(t *testing.T, conn *websocket.Conn) websocketOutboundFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var frame websocketOutboundFrame
	if err := wsjson.Read(ctx, conn, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func frameDataString(frame websocketOutboundFrame, key string) string {
	value, _ := frameDataMap(nil, frame)[key].(string)
	return value
}

func frameDataMap(t *testing.T, frame websocketOutboundFrame) map[string]any {
	data, ok := frame.Data.(map[string]any)
	if !ok && t != nil {
		t.Fatalf("frame data = %+v", frame.Data)
	}
	return data
}

func assertWebSocketCapabilities(t *testing.T, frame websocketOutboundFrame, want []string) {
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
