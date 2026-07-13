package websocket

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
	frt "github.com/xiramesh/xira/internal/runtime"
)

type recordingStructuredHITLResolver struct {
	mu         sync.Mutex
	requests   map[string]*humanrequest.HumanRequest
	inputs     []humanrequest.HumanResponseEnvelope
	getErr     error
	resolveErr error
}

func (r *recordingStructuredHITLResolver) GetHumanRequest(_ context.Context, requestID string) (*humanrequest.HumanRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	req := r.requests[requestID]
	if req == nil {
		return nil, humanrequest.ErrNotFound
	}
	clone := *req
	return &clone, nil
}

func (r *recordingStructuredHITLResolver) ResolveHumanResponseAsync(_ context.Context, input humanrequest.HumanResponseEnvelope) (*humanrequest.HumanRequest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inputs = append(r.inputs, input)
	if r.resolveErr != nil {
		return nil, r.resolveErr
	}
	resolved := *r.requests[input.RequestID]
	return &resolved, nil
}

func (r *recordingStructuredHITLResolver) recordedInputs() []humanrequest.HumanResponseEnvelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]humanrequest.HumanResponseEnvelope(nil), r.inputs...)
}

func websocketHumanRequest(kind humanrequest.RequestKind) *humanrequest.HumanRequest {
	return &humanrequest.HumanRequest{
		ID:               "hrq_ws_1",
		Kind:             kind,
		Status:           humanrequest.StatusPending,
		CorrelationToken: "0123456789abcdef0123456789abcdef",
		ChatKey:          keyOf("chat-hitl", "sender-hitl").String(),
		Responder: humanrequest.ResponderPolicy{
			Type:         humanrequest.ResponderCurrentSender,
			EntrypointID: "websocket-default",
			SenderID:     "sender-hitl",
		},
	}
}

func registerWebSocketChat(t *testing.T, c *websocket.Conn) {
	t.Helper()
	writeFrameClient(t, c, inboundFrame{
		Type: "message",
		ID:   "register-hitl-chat",
		Data: mustJSON(t, messageData{
			Message: "register this connection",
			Context: channel.InboundContext{
				Channel:   "websocket",
				ChatID:    "chat-hitl",
				SenderID:  "sender-hitl",
				MessageID: "register-hitl-chat",
			},
		}),
	})
	if got := readFrameClient(t, c); got.Type != "ack" {
		t.Fatalf("registration frame = %q, want ack", got.Type)
	}
	if got := readFrameClient(t, c); got.Type != "response" {
		t.Fatalf("registration result = %q, want response", got.Type)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestHandleConnectionHumanResponseAcceptsExactCurrentSender(t *testing.T) {
	req := websocketHumanRequest(humanrequest.RequestApproval)
	resolver := &recordingStructuredHITLResolver{requests: map[string]*humanrequest.HumanRequest{req.ID: req}}
	srv := newWSLoopbackServer(t, newFakeRuntime())
	srv.runner.SetStructuredHITLResolver(resolver)
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")
	registerWebSocketChat(t, c)

	frame := inboundFrame{
		Type:             "human_response",
		ID:               "human-action-1",
		RequestID:        req.ID,
		CorrelationToken: req.CorrelationToken,
		Action:           "approve",
	}
	writeFrameClient(t, c, map[string]any{
		"type": frame.Type, "id": frame.ID, "request_id": frame.RequestID,
		"correlation_token": frame.CorrelationToken, "action": frame.Action,
		"sender_id": "forged-sender", "sender_id_type": "open_id",
		"chat_id": "forged-chat", "entrypoint_id": "forged-entrypoint",
		"idempotency_key": "forged-idempotency",
	})
	ack := readFrameClient(t, c)
	if ack.Type != "ack" {
		t.Fatalf("frame type = %q, want ack", ack.Type)
	}
	data, _ := ack.Data.(map[string]any)
	if got := data["status"]; got != "human_response_accepted" {
		t.Fatalf("ack status = %v, want human_response_accepted", got)
	}

	writeFrameClient(t, c, frame)
	_ = readFrameClient(t, c)
	inputs := resolver.recordedInputs()
	if len(inputs) != 2 {
		t.Fatalf("resolve calls = %d, want 2", len(inputs))
	}
	for _, input := range inputs {
		if input.RequestID != req.ID || input.CorrelationToken != req.CorrelationToken {
			t.Fatalf("correlation changed: %+v", input)
		}
		if input.SenderID != req.Responder.SenderID || input.SenderIDType != "" {
			t.Fatalf("identity = (%q,%q), want persisted untyped sender", input.SenderID, input.SenderIDType)
		}
		if input.EntrypointID != req.Responder.EntrypointID || input.Kind != humanrequest.ResponseApprove {
			t.Fatalf("response authority/action changed: %+v", input)
		}
	}
	if inputs[0].IdempotencyKey == "" || inputs[0].IdempotencyKey != inputs[1].IdempotencyKey {
		t.Fatalf("idempotency keys = %q / %q, want stable non-empty", inputs[0].IdempotencyKey, inputs[1].IdempotencyKey)
	}
}

func TestHandleConnectionHumanResponseFailsClosed(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*humanrequest.HumanRequest)
		getErr     error
		resolveErr error
		action     string
		token      string
		wantCode   string
	}{
		{name: "request missing", getErr: humanrequest.ErrNotFound, wantCode: "human_response_rejected"},
		{name: "owner unsupported", mutate: func(req *humanrequest.HumanRequest) { req.Responder.Type = humanrequest.ResponderOwner }, wantCode: "unsupported_responder"},
		{name: "typed sender untrusted", mutate: func(req *humanrequest.HumanRequest) { req.Responder.SenderIDType = "open_id" }, wantCode: "human_response_rejected"},
		{name: "wrong correlation", token: "wrong-token", resolveErr: humanrequest.ErrConflict, wantCode: "human_response_rejected"},
		{name: "expired", resolveErr: humanrequest.ErrExpired, wantCode: "human_response_rejected"},
		{name: "invalid action", action: "later", wantCode: "human_response_rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := websocketHumanRequest(humanrequest.RequestApproval)
			if tc.mutate != nil {
				tc.mutate(req)
			}
			resolver := &recordingStructuredHITLResolver{
				requests:   map[string]*humanrequest.HumanRequest{req.ID: req},
				getErr:     tc.getErr,
				resolveErr: tc.resolveErr,
			}
			srv := newWSLoopbackServer(t, newFakeRuntime())
			srv.runner.SetStructuredHITLResolver(resolver)
			defer srv.close()
			c := srv.dial(t)
			defer c.Close(websocket.StatusNormalClosure, "")
			registerWebSocketChat(t, c)

			action := tc.action
			if action == "" {
				action = "approve"
			}
			token := tc.token
			if token == "" {
				token = req.CorrelationToken
			}
			writeFrameClient(t, c, inboundFrame{
				Type:             "human_response",
				ID:               "human-action-rejected",
				RequestID:        req.ID,
				CorrelationToken: token,
				Action:           action,
			})
			got := readFrameClient(t, c)
			if got.Type != "error" {
				t.Fatalf("frame type = %q, want error", got.Type)
			}
			data, _ := got.Data.(map[string]any)
			if code := data["code"]; code != tc.wantCode {
				t.Fatalf("error code = %v, want %s", code, tc.wantCode)
			}
		})
	}
}

func TestWebSocketHumanResponseAuthority(t *testing.T) {
	valid := websocketHumanRequest(humanrequest.RequestApproval)
	cases := []struct {
		name   string
		req    *humanrequest.HumanRequest
		wantOK bool
	}{
		{name: "valid", req: valid, wantOK: true},
		{name: "nil"},
		{name: "owner", req: func() *humanrequest.HumanRequest {
			r := *valid
			r.Responder.Type = humanrequest.ResponderOwner
			return &r
		}()},
		{name: "unknown responder", req: func() *humanrequest.HumanRequest { r := *valid; r.Responder.Type = "someone"; return &r }()},
		{name: "typed sender", req: func() *humanrequest.HumanRequest { r := *valid; r.Responder.SenderIDType = "open_id"; return &r }()},
		{name: "malformed chat key", req: func() *humanrequest.HumanRequest { r := *valid; r.ChatKey = "bad"; return &r }()},
		{name: "empty chat", req: func() *humanrequest.HumanRequest { r := *valid; r.ChatKey = "websocket//sender-hitl"; return &r }()},
		{name: "empty chat sender", req: func() *humanrequest.HumanRequest { r := *valid; r.ChatKey = "websocket/chat-hitl/"; return &r }()},
		{name: "wrong channel", req: func() *humanrequest.HumanRequest { r := *valid; r.ChatKey = "feishu/chat-hitl/sender-hitl"; return &r }()},
		{name: "empty responder sender", req: func() *humanrequest.HumanRequest { r := *valid; r.Responder.SenderID = ""; return &r }()},
		{name: "sender mismatch", req: func() *humanrequest.HumanRequest { r := *valid; r.Responder.SenderID = "other"; return &r }()},
		{name: "empty entrypoint", req: func() *humanrequest.HumanRequest { r := *valid; r.Responder.EntrypointID = ""; return &r }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := websocketHumanResponseAuthority(tc.req)
			if (err == nil) != tc.wantOK {
				t.Fatalf("error = %v, wantOK=%v", err, tc.wantOK)
			}
		})
	}
}

func TestNormalizeWebSocketHumanResponse(t *testing.T) {
	approval := websocketHumanRequest(humanrequest.RequestApproval)
	freeform := websocketHumanRequest(humanrequest.RequestFreeform)
	freeform.Options = []humanrequest.HumanOption{{ID: "opt-a", Label: "Option A"}}
	cases := []struct {
		name        string
		req         *humanrequest.HumanRequest
		action      string
		answer      string
		wantKind    humanrequest.ResponseKind
		wantMessage string
		wantErr     bool
	}{
		{name: "approve", req: approval, action: " APPROVE ", wantKind: humanrequest.ResponseApprove},
		{name: "deny", req: approval, action: "deny", wantKind: humanrequest.ResponseDeny},
		{name: "cancel approval", req: approval, action: "cancel", wantKind: humanrequest.ResponseCancel},
		{name: "answer option", req: freeform, action: "answer", answer: " opt-a ", wantKind: humanrequest.ResponseAnswer, wantMessage: "opt-a"},
		{name: "answer option miss", req: freeform, action: "answer", answer: "missing", wantErr: true},
		{name: "cancel freeform", req: freeform, action: "cancel", wantKind: humanrequest.ResponseCancel},
		{name: "approval cannot answer", req: approval, action: "answer", answer: "yes", wantErr: true},
		{name: "freeform cannot approve", req: freeform, action: "approve", wantErr: true},
		{name: "non-answer payload", req: approval, action: "approve", answer: "smuggled", wantErr: true},
		{name: "unknown action", req: approval, action: "later", wantErr: true},
		{name: "unknown request kind", req: func() *humanrequest.HumanRequest { r := *approval; r.Kind = "other"; return &r }(), action: "approve", wantErr: true},
		{name: "missing request", action: "approve", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, message, err := normalizeWebSocketHumanResponse(tc.req, tc.action, tc.answer)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil || kind != tc.wantKind || message != tc.wantMessage {
				t.Fatalf("got (%q,%q,%v), want (%q,%q,nil)", kind, message, err, tc.wantKind, tc.wantMessage)
			}
		})
	}
}

func TestHandleConnectionHumanResponseRequiresCurrentConnection(t *testing.T) {
	req := websocketHumanRequest(humanrequest.RequestApproval)
	resolver := &recordingStructuredHITLResolver{requests: map[string]*humanrequest.HumanRequest{req.ID: req}}
	srv := newWSLoopbackServer(t, newFakeRuntime())
	srv.runner.SetStructuredHITLResolver(resolver)
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	writeFrameClient(t, c, inboundFrame{
		Type: "human_response", ID: "unowned", RequestID: req.ID,
		CorrelationToken: req.CorrelationToken, Action: "approve",
	})
	got := readFrameClient(t, c)
	data, _ := got.Data.(map[string]any)
	if got.Type != "error" || data["code"] != "human_response_rejected" {
		t.Fatalf("unowned response = %+v, want generic rejection", got)
	}
	if len(resolver.recordedInputs()) != 0 {
		t.Fatal("unowned connection reached resolver")
	}
}

func TestPlainApproveRemainsANormalAgentTurn(t *testing.T) {
	rt := newFakeRuntime()
	seen := make(chan frt.TurnRequest, 1)
	rt.respond = func(req frt.TurnRequest) (frt.TurnResponse, error) {
		seen <- req
		return frt.TurnResponse{RunID: "run-approve", Status: "completed", FinalResponse: "ok"}, nil
	}
	srv := newWSLoopbackServer(t, rt)
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	writeFrameClient(t, c, inboundFrame{
		Type: "message", ID: "plain-approve",
		Data: mustJSON(t, messageData{
			Message: "approve",
			Context: channel.InboundContext{
				Channel: "websocket", ChatID: "chat-plain", SenderID: "sender-plain", MessageID: "plain-approve",
			},
		}),
	})
	if got := readFrameClient(t, c); got.Type != "ack" {
		t.Fatalf("first frame = %q, want ack", got.Type)
	}
	if got := readFrameClient(t, c); got.Type != "response" {
		t.Fatalf("second frame = %q, want response", got.Type)
	}
	select {
	case req := <-seen:
		if req.Message != "approve" {
			t.Fatalf("agent turn message = %q, want approve", req.Message)
		}
	default:
		t.Fatal("plain approve never reached RunAgent")
	}
}

func TestHandleConnectionHumanResponseUnavailableAndMalformed(t *testing.T) {
	srv := newWSLoopbackServer(t, newFakeRuntime())
	defer srv.close()
	c := srv.dial(t)
	defer c.Close(websocket.StatusNormalClosure, "")

	writeFrameClient(t, c, inboundFrame{Type: "human_response", ID: "unavailable", RequestID: "hrq"})
	got := readFrameClient(t, c)
	data, _ := got.Data.(map[string]any)
	if got.Type != "error" || data["code"] != "human_response_unavailable" || data["retryable"] != true {
		t.Fatalf("unavailable response = %+v", got)
	}

	srv.runner.SetStructuredHITLResolver(&recordingStructuredHITLResolver{})
	writeFrameClient(t, c, inboundFrame{Type: "human_response", ID: "malformed"})
	got = readFrameClient(t, c)
	data, _ = got.Data.(map[string]any)
	if got.Type != "error" || data["code"] != "human_response_rejected" {
		t.Fatalf("malformed response = %+v", got)
	}
}

func TestConnOwnsKeyUsesCurrentRegistryOwner(t *testing.T) {
	runner := newTestRunner(t, newFakeRuntime())
	key := keyOf("chat-owner", "sender-owner")
	first := runner.newConn(func(outboundFrame) error { return nil }, func() {})
	second := runner.newConn(func(outboundFrame) error { return nil }, func() {})
	if runner.connOwnsKey(nil, key) || runner.connOwnsKey(first, key) {
		t.Fatal("nil or unregistered connection reported ownership")
	}
	if _, rejected := runner.registerConnKey(first, key); rejected || !runner.connOwnsKey(first, key) {
		t.Fatal("registered current connection does not own key")
	}
	runner.connMu.Lock()
	runner.conns[canonicalKey(key)] = second
	runner.connMu.Unlock()
	if runner.connOwnsKey(first, key) || !runner.connOwnsKey(second, key) {
		t.Fatal("ownership followed historical conn.keys instead of current registry")
	}
}
