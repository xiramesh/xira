package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/entrypoints"
	"github.com/xiramesh/xira/internal/humanrequest"
	frt "github.com/xiramesh/xira/internal/runtime"
)

func TestBuildHumanRequestCardRendersNativeActions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		req         humanrequest.HumanRequest
		wantActions []string
		wantAnswers []string
		wantTextRef bool
	}{
		{
			name: "approval",
			req: humanrequest.HumanRequest{ID: "hrq-approve", Kind: humanrequest.RequestApproval,
				Question: "Deploy now?", CorrelationToken: "550e8400-e29b-41d4-a716-446655440000"},
			wantActions: []string{"approve", "deny"},
		},
		{
			name: "options",
			req: humanrequest.HumanRequest{ID: "hrq-options", Kind: humanrequest.RequestFreeform,
				Question: "Choose a window", CorrelationToken: "550e8400-e29b-41d4-a716-446655440001",
				Options: []humanrequest.HumanOption{{ID: "morning", Label: "Morning"}, {ID: "night", Label: "Night"}}},
			wantActions: []string{"answer", "answer"},
			wantAnswers: []string{"morning", "night"},
		},
		{
			name: "freeform uses exact text protocol",
			req: humanrequest.HumanRequest{ID: "hrq-freeform", Kind: humanrequest.RequestFreeform,
				Question: "What should I tell them?", CorrelationToken: "550e8400-e29b-41d4-a716-446655440002"},
			wantTextRef: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := buildHumanRequestCard(tt.req)
			if err != nil {
				t.Fatalf("buildHumanRequestCard() error = %v", err)
			}
			var card map[string]any
			if err := json.Unmarshal([]byte(content), &card); err != nil {
				t.Fatalf("card JSON: %v\n%s", err, content)
			}
			if card["schema"] != "2.0" {
				t.Fatalf("schema = %v, want 2.0", card["schema"])
			}
			config, _ := card["config"].(map[string]any)
			if config["update_multi"] != true {
				t.Fatalf("config = %#v, want update_multi=true", config)
			}
			values := cardCallbackValues(card)
			if len(values) != len(tt.wantActions) {
				t.Fatalf("callback values = %#v, want %d", values, len(tt.wantActions))
			}
			for i, value := range values {
				if value["request_id"] != tt.req.ID || value["correlation_token"] != tt.req.CorrelationToken || value["action"] != tt.wantActions[i] {
					t.Fatalf("callback[%d] = %#v", i, value)
				}
				if len(tt.wantAnswers) > 0 && value["answer"] != tt.wantAnswers[i] {
					t.Fatalf("callback[%d] answer = %v, want %q", i, value["answer"], tt.wantAnswers[i])
				}
			}
			if tt.wantTextRef && !strings.Contains(content, "/answer HR-550E8400E29B41D4A716446655440002") {
				t.Fatalf("freeform card does not expose exact text fallback: %s", content)
			}
		})
	}
}

func TestBuildResolvedHumanRequestCardRemovesActions(t *testing.T) {
	t.Parallel()
	req := humanrequest.HumanRequest{ID: "hrq-1", Kind: humanrequest.RequestApproval, Question: "Deploy?"}
	content, err := buildResolvedHumanRequestCard(req, humanrequest.ResponseApprove, "")
	if err != nil {
		t.Fatalf("buildResolvedHumanRequestCard() error = %v", err)
	}
	if strings.Contains(content, `"behaviors"`) || !strings.Contains(content, "已批准") {
		t.Fatalf("resolved card = %s", content)
	}
}

func TestHumanRequestCardRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	for name, req := range map[string]humanrequest.HumanRequest{
		"missing id":          {Kind: humanrequest.RequestApproval, Question: "Q", CorrelationToken: "550e8400-e29b-41d4-a716-446655440000"},
		"missing question":    {ID: "hrq", Kind: humanrequest.RequestApproval, CorrelationToken: "550e8400-e29b-41d4-a716-446655440000"},
		"missing correlation": {ID: "hrq", Kind: humanrequest.RequestApproval, Question: "Q"},
		"unknown kind":        {ID: "hrq", Kind: "other", Question: "Q", CorrelationToken: "550e8400-e29b-41d4-a716-446655440000"},
		"bad text reference":  {ID: "hrq", Kind: humanrequest.RequestFreeform, Question: "Q", CorrelationToken: "short"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildHumanRequestCard(req); err == nil {
				t.Fatal("buildHumanRequestCard() succeeded")
			}
		})
	}
	for _, kind := range []humanrequest.ResponseKind{humanrequest.ResponseApprove, humanrequest.ResponseDeny, humanrequest.ResponseCancel, humanrequest.ResponseAnswer} {
		if _, err := buildResolvedHumanRequestCard(humanrequest.HumanRequest{Question: "Q"}, kind, "detail"); err != nil {
			t.Fatalf("buildResolvedHumanRequestCard(%q): %v", kind, err)
		}
	}
	if _, err := buildResolvedHumanRequestCard(humanrequest.HumanRequest{}, "other", ""); err == nil {
		t.Fatal("unknown response kind succeeded")
	}
}

func TestHumanRequestDeliveryRouteIsExactAndTyped(t *testing.T) {
	t.Parallel()
	r := &Runner{definition: entrypoints.Definition{ID: "feishu-main"}}
	tests := []struct {
		name     string
		target   frt.HumanRequestDeliveryTarget
		wantType string
		wantID   string
		wantErr  bool
	}{
		{name: "current sender chat", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: "feishu-main", ChatID: "oc_chat"}}, wantType: "chat_id", wantID: "oc_chat"},
		{name: "owner open id", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: "feishu-main"}, Recipient: &channel.OutboundRecipient{ID: "ou_owner", IDType: "open_id"}}, wantType: "open_id", wantID: "ou_owner"},
		{name: "owner user id", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: "feishu-main"}, Recipient: &channel.OutboundRecipient{ID: "u_owner", IDType: "user_id"}}, wantType: "user_id", wantID: "u_owner"},
		{name: "owner union id", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: "feishu-main"}, Recipient: &channel.OutboundRecipient{ID: "on_owner", IDType: "union_id"}}, wantType: "union_id", wantID: "on_owner"},
		{name: "wrong entrypoint", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: "other", ChatID: "oc_chat"}}, wantErr: true},
		{name: "wrong channel", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "ilink", EntrypointID: "feishu-main", ChatID: "oc_chat"}}, wantErr: true},
		{name: "missing chat", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: "feishu-main"}}, wantErr: true},
		{name: "bad owner type", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: "feishu-main"}, Recipient: &channel.OutboundRecipient{ID: "ou_owner", IDType: "email"}}, wantErr: true},
		{name: "missing owner id", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: "feishu-main"}, Recipient: &channel.OutboundRecipient{IDType: "open_id"}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotID, err := r.humanRequestDeliveryRoute(tt.target)
			if (err != nil) != tt.wantErr {
				t.Fatalf("humanRequestDeliveryRoute() error = %v, wantErr=%v", err, tt.wantErr)
			}
			if gotType != tt.wantType || gotID != tt.wantID {
				t.Fatalf("route = (%q,%q), want (%q,%q)", gotType, gotID, tt.wantType, tt.wantID)
			}
		})
	}
	var nilRunner *Runner
	if _, _, err := nilRunner.humanRequestDeliveryRoute(frt.HumanRequestDeliveryTarget{}); err == nil {
		t.Fatal("nil runner route succeeded")
	}
}

func TestDeliverHumanRequestReturnsCardReceipt(t *testing.T) {
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "tenant_access_token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"token","expire":7200}`))
			return
		}
		body, _ := io.ReadAll(req.Body)
		requestBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_card_1"}}`))
	}))
	defer server.Close()
	r := newHumanRequestTestRunner(t, server.URL)
	receipt, err := r.DeliverHumanRequest(context.Background(), testFeishuHumanRequest(), frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: r.ID(), ChatID: "oc_chat"}})
	if err != nil || receipt.MessageID != "om_card_1" {
		t.Fatalf("DeliverHumanRequest() = %+v, %v", receipt, err)
	}
	var outer struct {
		MsgType string `json:"msg_type"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(requestBody), &outer); err != nil {
		t.Fatal(err)
	}
	if outer.MsgType != "interactive" || !strings.Contains(outer.Content, `"action":"approve"`) {
		t.Fatalf("request body = %s", requestBody)
	}
}

func TestDeliverHumanRequestFallsBackToExplicitText(t *testing.T) {
	var mu sync.Mutex
	var requestBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "tenant_access_token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"token","expire":7200}`))
			return
		}
		body, _ := io.ReadAll(req.Body)
		mu.Lock()
		requestBodies = append(requestBodies, string(body))
		call := len(requestBodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"code":230099,"msg":"card rejected"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_text_1"}}`))
	}))
	defer server.Close()
	r := newHumanRequestTestRunner(t, server.URL)
	receipt, err := r.DeliverHumanRequest(context.Background(), testFeishuHumanRequest(), frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: r.ID(), ChatID: "oc_chat"}})
	if err != nil || receipt.MessageID != "om_text_1" {
		t.Fatalf("DeliverHumanRequest() = %+v, %v", receipt, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requestBodies) != 2 || !strings.Contains(requestBodies[0], `"msg_type":"interactive"`) || !strings.Contains(requestBodies[1], `"msg_type":"text"`) || !strings.Contains(requestBodies[1], "/answer HR-") {
		t.Fatalf("request bodies = %#v", requestBodies)
	}
}

func TestDeliverHumanRequestRejectsInvalidRouteAndFallbackFailure(t *testing.T) {
	r := newHumanRequestTestRunner(t, "http://127.0.0.1:9")
	validTarget := frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "feishu", EntrypointID: r.ID(), ChatID: "oc_chat"}}
	if err := r.ValidateHumanRequestDelivery(validTarget); err != nil {
		t.Fatalf("ValidateHumanRequestDelivery(valid): %v", err)
	}
	invalidTarget := frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Channel: "ilink", EntrypointID: r.ID(), ChatID: "oc_chat"}}
	if err := r.ValidateHumanRequestDelivery(invalidTarget); err == nil {
		t.Fatal("ValidateHumanRequestDelivery(invalid) succeeded")
	}
	if _, err := r.DeliverHumanRequest(context.Background(), testFeishuHumanRequest(), invalidTarget); err == nil {
		t.Fatal("DeliverHumanRequest(invalid route) succeeded")
	}
	badRequest := testFeishuHumanRequest()
	badRequest.CorrelationToken = "short"
	if _, err := r.DeliverHumanRequest(context.Background(), badRequest, validTarget); err == nil {
		t.Fatal("DeliverHumanRequest(invalid request) succeeded")
	}
	if _, err := r.DeliverHumanRequest(context.Background(), testFeishuHumanRequest(), validTarget); err == nil {
		t.Fatal("DeliverHumanRequest(transport failure) succeeded")
	}
}

type recordingAsyncExactResolver struct {
	inputs []humanrequest.HumanResponseEnvelope
	err    error
	nilReq bool
}

func (r *recordingAsyncExactResolver) ResolveHumanResponseAsync(_ context.Context, input humanrequest.HumanResponseEnvelope) (*humanrequest.HumanRequest, error) {
	r.inputs = append(r.inputs, input)
	if r.err != nil {
		return nil, r.err
	}
	if r.nilReq {
		return nil, nil
	}
	return &humanrequest.HumanRequest{ID: input.RequestID, Question: "Deploy?", Kind: humanrequest.RequestApproval}, nil
}

func TestHandleCardActionCommitsExactResponseBeforeUpdatingCard(t *testing.T) {
	t.Parallel()
	resolver := &recordingAsyncExactResolver{}
	r := &Runner{definition: entrypoints.Definition{ID: "feishu-main"}, asyncExactResolver: resolver}
	event := testCardActionEvent("approve")
	resp, err := r.handleCardAction(context.Background(), event)
	if err != nil {
		t.Fatalf("handleCardAction() error = %v", err)
	}
	if len(resolver.inputs) != 1 {
		t.Fatalf("resolver inputs = %#v", resolver.inputs)
	}
	input := resolver.inputs[0]
	if input.RequestID != "hrq-1" || input.CorrelationToken != "550e8400-e29b-41d4-a716-446655440000" || input.EntrypointID != "feishu-main" || input.DeliveryMessageID != "om_card_1" || input.Kind != humanrequest.ResponseApprove {
		t.Fatalf("resolver input = %+v", input)
	}
	if input.SenderIdentities["user_id"] != "u_operator" || input.SenderIdentities["open_id"] != "ou_operator" {
		t.Fatalf("sender identities = %#v", input.SenderIdentities)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "success" || resp.Card == nil {
		t.Fatalf("callback response = %+v", resp)
	}
	cardBytes, _ := json.Marshal(resp.Card.Data)
	if strings.Contains(string(cardBytes), `"behaviors"`) || !strings.Contains(string(cardBytes), "已批准") {
		t.Fatalf("resolved callback card = %s", cardBytes)
	}
	firstKey := input.IdempotencyKey
	if _, err := r.handleCardAction(context.Background(), testCardActionEvent("approve")); err != nil {
		t.Fatalf("duplicate callback error = %v", err)
	}
	if resolver.inputs[1].IdempotencyKey != firstKey {
		t.Fatalf("duplicate idempotency keys differ: %q != %q", resolver.inputs[1].IdempotencyKey, firstKey)
	}
}

func TestHandleCardActionRejectsWithoutReplacingCard(t *testing.T) {
	t.Parallel()
	t.Run("malformed", func(t *testing.T) {
		resolver := &recordingAsyncExactResolver{}
		r := &Runner{definition: entrypoints.Definition{ID: "feishu-main"}, asyncExactResolver: resolver}
		event := testCardActionEvent("approve")
		delete(event.Event.Action.Value, "correlation_token")
		resp, err := r.handleCardAction(context.Background(), event)
		if err != nil || len(resolver.inputs) != 0 || resp == nil || resp.Toast == nil || resp.Card != nil {
			t.Fatalf("malformed response=%+v err=%v inputs=%#v", resp, err, resolver.inputs)
		}
	})
	t.Run("unauthorized", func(t *testing.T) {
		resolver := &recordingAsyncExactResolver{err: errors.New("wrong operator")}
		r := &Runner{definition: entrypoints.Definition{ID: "feishu-main"}, asyncExactResolver: resolver}
		resp, err := r.handleCardAction(context.Background(), testCardActionEvent("deny"))
		if err != nil || resp == nil || resp.Toast == nil || resp.Toast.Type != "error" || resp.Card != nil {
			t.Fatalf("rejected response=%+v err=%v", resp, err)
		}
	})
	t.Run("missing resolver", func(t *testing.T) {
		r := &Runner{definition: entrypoints.Definition{ID: "feishu-main"}}
		resp, err := r.handleCardAction(context.Background(), testCardActionEvent("approve"))
		if err != nil || resp == nil || resp.Card != nil || resp.Toast == nil || resp.Toast.Type != "error" {
			t.Fatalf("missing resolver response=%+v err=%v", resp, err)
		}
	})
	t.Run("resolver returns nil", func(t *testing.T) {
		r := &Runner{definition: entrypoints.Definition{ID: "feishu-main"}, asyncExactResolver: &recordingAsyncExactResolver{nilReq: true}}
		resp, err := r.handleCardAction(context.Background(), testCardActionEvent("approve"))
		if err != nil || resp == nil || resp.Card != nil || resp.Toast == nil || resp.Toast.Type != "error" {
			t.Fatalf("nil request response=%+v err=%v", resp, err)
		}
	})
	t.Run("nil runner", func(t *testing.T) {
		var r *Runner
		resp, err := r.handleCardAction(context.Background(), testCardActionEvent("approve"))
		if err != nil || resp == nil || resp.Card != nil {
			t.Fatalf("nil runner response=%+v err=%v", resp, err)
		}
	})
}

func TestParseHumanCardActionCoversSealedProtocol(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"approve", "deny", "cancel"} {
		command, identities, messageID, err := parseHumanCardAction(testCardActionEvent(action))
		if err != nil || string(command.Kind) != action || identities["open_id"] == "" || messageID != "om_card_1" {
			t.Fatalf("parse action %q = %+v %#v %q %v", action, command, identities, messageID, err)
		}
	}
	answerEvent := testCardActionEvent("answer")
	answerEvent.Event.Action.Value["answer"] = "night"
	command, _, _, err := parseHumanCardAction(answerEvent)
	if err != nil || command.Kind != humanrequest.ResponseAnswer || command.Answer != "night" {
		t.Fatalf("parse answer = %+v, %v", command, err)
	}

	tests := map[string]func() *callback.CardActionTriggerEvent{
		"nil event":   func() *callback.CardActionTriggerEvent { return nil },
		"nil payload": func() *callback.CardActionTriggerEvent { return &callback.CardActionTriggerEvent{} },
		"missing operator": func() *callback.CardActionTriggerEvent {
			e := testCardActionEvent("approve")
			e.Event.Operator = nil
			return e
		},
		"missing action": func() *callback.CardActionTriggerEvent {
			e := testCardActionEvent("approve")
			e.Event.Action = nil
			return e
		},
		"missing context": func() *callback.CardActionTriggerEvent {
			e := testCardActionEvent("approve")
			e.Event.Context = nil
			return e
		},
		"wrong tag": func() *callback.CardActionTriggerEvent {
			e := testCardActionEvent("approve")
			e.Event.Action.Tag = "select"
			return e
		},
		"no identity": func() *callback.CardActionTriggerEvent {
			e := testCardActionEvent("approve")
			e.Event.Operator.UserID = nil
			e.Event.Operator.OpenID = ""
			return e
		},
		"no message": func() *callback.CardActionTriggerEvent {
			e := testCardActionEvent("approve")
			e.Event.Context.OpenMessageID = ""
			return e
		},
		"no request": func() *callback.CardActionTriggerEvent {
			e := testCardActionEvent("approve")
			delete(e.Event.Action.Value, "request_id")
			return e
		},
		"no action value": func() *callback.CardActionTriggerEvent {
			e := testCardActionEvent("approve")
			delete(e.Event.Action.Value, "action")
			return e
		},
		"non-string value": func() *callback.CardActionTriggerEvent {
			e := testCardActionEvent("approve")
			e.Event.Action.Value["request_id"] = 1
			return e
		},
		"empty answer":   func() *callback.CardActionTriggerEvent { return testCardActionEvent("answer") },
		"unknown action": func() *callback.CardActionTriggerEvent { return testCardActionEvent("other") },
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := parseHumanCardAction(build()); err == nil {
				t.Fatal("parseHumanCardAction() succeeded")
			}
		})
	}
}

type recordingFeishuTextResolver struct {
	input humanrequest.TextResponseEnvelope
	calls int
	err   error
}

func (r *recordingFeishuTextResolver) ResolveHumanTextResponse(_ context.Context, input humanrequest.TextResponseEnvelope) (*humanrequest.HumanRequest, error) {
	r.calls++
	r.input = input
	if r.err != nil {
		return nil, r.err
	}
	return &humanrequest.HumanRequest{ID: "hrq-1"}, nil
}

func TestTryResolveTextHumanResponseConsumesFallbackProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.URL.Path, "tenant_access_token") {
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"token","expire":7200}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_ack"}}`))
	}))
	defer server.Close()
	r := newHumanRequestTestRunner(t, server.URL)
	resolver := &recordingFeishuTextResolver{}
	r.SetTextHITLResolver(resolver)
	inbound := channel.InboundContext{Channel: "feishu", EntrypointID: r.ID(), ChatID: "oc_chat", ChatType: "direct", SenderID: "u_sender", SenderIDType: "user_id", MessageID: "om_answer"}
	key := r.messageDedupeKey(inbound.MessageID)
	if !r.messages.Begin(key, time.Now()) {
		t.Fatal("dedupe begin failed")
	}
	chatKey := frt.ChatKeyFromInbound(inbound)
	if !r.tryResolveTextHumanResponse(context.Background(), inbound, chatKey, "/answer HR-550E8400E29B41D4A716446655440000 yes", key) {
		t.Fatal("explicit fallback was not consumed")
	}
	if resolver.calls != 1 || resolver.input.SenderID != "u_sender" || resolver.input.SenderIDType != "user_id" || resolver.input.ChatKey != chatKey.String() || resolver.input.IdempotencyKey == "" {
		t.Fatalf("resolver = calls=%d input=%+v", resolver.calls, resolver.input)
	}
	if r.messages.Begin(key, time.Now()) {
		t.Fatal("committed fallback response released dedupe")
	}
	if r.tryResolveTextHumanResponse(context.Background(), inbound, chatKey, "ordinary chat", "other") {
		t.Fatal("ordinary chat was consumed")
	}
}

func TestTryResolveTextHumanResponseFailureAndAckSemantics(t *testing.T) {
	const answer = "/answer HR-550E8400E29B41D4A716446655440000 yes"
	newCase := func(t *testing.T, resolver frt.TextHITLResolver, baseURL string) (*Runner, channel.InboundContext, frt.ChatKey, string) {
		t.Helper()
		r := newHumanRequestTestRunner(t, baseURL)
		r.SetTextHITLResolver(resolver)
		inbound := channel.InboundContext{Channel: "feishu", EntrypointID: r.ID(), ChatID: "oc_chat", ChatType: "direct", SenderID: "u_sender", SenderIDType: "user_id", MessageID: "om_answer"}
		key := r.messageDedupeKey(inbound.MessageID)
		if !r.messages.Begin(key, time.Now()) {
			t.Fatal("dedupe begin failed")
		}
		return r, inbound, frt.ChatKeyFromInbound(inbound), key
	}
	ackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.URL.Path, "tenant_access_token") {
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"token","expire":7200}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"message_id":"om_ack"}}`))
	}))
	defer ackServer.Close()

	t.Run("malformed and missing resolver stay protocol traffic", func(t *testing.T) {
		r, inbound, chatKey, _ := newCase(t, nil, ackServer.URL)
		if !r.tryResolveTextHumanResponse(context.Background(), inbound, chatKey, "/answer short yes", r.messageDedupeKey(inbound.MessageID)) {
			t.Fatal("malformed command not consumed")
		}
	})
	t.Run("rejected response is acknowledged safely", func(t *testing.T) {
		resolver := &recordingFeishuTextResolver{err: errors.New("unauthorized")}
		r, inbound, chatKey, _ := newCase(t, resolver, ackServer.URL)
		if !r.tryResolveTextHumanResponse(context.Background(), inbound, chatKey, answer, r.messageDedupeKey(inbound.MessageID)) || resolver.calls != 1 {
			t.Fatal("rejected command was not consumed")
		}
	})
	t.Run("committed answer remains deduped when ack fails", func(t *testing.T) {
		r, inbound, chatKey, key := newCase(t, &recordingFeishuTextResolver{}, "http://127.0.0.1:9")
		r.tryResolveTextHumanResponse(context.Background(), inbound, chatKey, answer, key)
		if r.messages.Begin(key, time.Now()) {
			t.Fatal("committed answer was released after ack failure")
		}
	})
	t.Run("uncommitted answer is retryable when ack fails", func(t *testing.T) {
		r, inbound, chatKey, key := newCase(t, &recordingFeishuTextResolver{err: errors.New("unauthorized")}, "http://127.0.0.1:9")
		r.tryResolveTextHumanResponse(context.Background(), inbound, chatKey, answer, key)
		if !r.messages.Begin(key, time.Now()) {
			t.Fatal("uncommitted answer stayed deduped after ack failure")
		}
	})
}

func testCardActionEvent(action string) *callback.CardActionTriggerEvent {
	userID := "u_operator"
	return &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Operator: &callback.Operator{UserID: &userID, OpenID: "ou_operator"},
		Action: &callback.CallBackAction{Tag: "button", Value: map[string]any{
			"request_id": "hrq-1", "correlation_token": "550e8400-e29b-41d4-a716-446655440000", "action": action,
		}},
		Context: &callback.Context{OpenMessageID: "om_card_1", OpenChatID: "oc_chat"},
	}}
}

func newHumanRequestTestRunner(t *testing.T, baseURL string) *Runner {
	t.Helper()
	r, err := NewRunner(entrypoints.Definition{ID: "feishu-main", Channel: "feishu", AppID: "cli_test", AppSecret: "secret"}, nil, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.client = lark.NewClient("cli_test", "secret", lark.WithOpenBaseUrl(baseURL))
	return r
}

func testFeishuHumanRequest() humanrequest.HumanRequest {
	return humanrequest.HumanRequest{ID: "hrq-1", Kind: humanrequest.RequestApproval, Question: "Deploy?", CorrelationToken: "550e8400-e29b-41d4-a716-446655440000"}
}

func cardCallbackValues(card map[string]any) []map[string]any {
	body, _ := card["body"].(map[string]any)
	elements, _ := body["elements"].([]any)
	var out []map[string]any
	for _, raw := range elements {
		element, _ := raw.(map[string]any)
		behaviors, _ := element["behaviors"].([]any)
		for _, rawBehavior := range behaviors {
			behavior, _ := rawBehavior.(map[string]any)
			if behavior["type"] != "callback" {
				continue
			}
			value, _ := behavior["value"].(map[string]any)
			out = append(out, value)
		}
	}
	return out
}
