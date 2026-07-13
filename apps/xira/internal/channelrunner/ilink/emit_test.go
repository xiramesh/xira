package ilink

import (
	"context"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
	frt "github.com/xiramesh/xira/internal/runtime"
)

// emit_test.go: tests ilink Runner.Emit (RFC #27 — unified outbound delivery
// for stateless HITL resume). Emit reconstructs the ilink addressing
// (account → client, senderID → recipient, context_token → reply token) from
// the OutboundEnvelope.Target and calls the existing send path.
//
// Reuses recordingClient + the runner wiring from progress_wiring_test.go.

// newEmitTestRunner wires a single account backed by a recordingClient.
func newEmitTestRunner(t *testing.T, accountID string) (*Runner, *recordingClient) {
	t.Helper()
	rc := &recordingClient{}
	r := &Runner{
		accounts: map[string]*accountPoller{
			accountID: {
				record: accountRecord{AccountID: accountID},
				client: rc,
			},
		},
	}
	return r, rc
}

func TestRunnerEmitDeliversFinal(t *testing.T) {
	r, rc := newEmitTestRunner(t, "acct-1")
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{
		Channel:  "ilink",
		Account:  "acct-1",
		SenderID: "user-9",
		Raw:      map[string]string{"context_token": "tok-xyz"},
	}
	env.Data = map[string]any{"content": "resume done"}

	if err := r.Emit(context.Background(), env); err != nil {
		t.Fatalf("Emit error: %v", err)
	}
	sent := rc.contents()
	if len(sent) != 1 {
		t.Fatalf("sent = %v, want exactly 1 message", sent)
	}
	if sent[0] != "resume done" {
		t.Errorf("sent = %q, want resume done", sent[0])
	}
}

func TestRunnerEmitUsesTypedRecipientWhenPresent(t *testing.T) {
	r, rc := newEmitTestRunner(t, "acct-1")
	env := channel.NewOutboundEnvelope(channel.OutboundProactiveMessage)
	env.Target = &channel.InboundContext{Channel: "ilink", Account: "acct-1"}
	env.Recipient = &channel.OutboundRecipient{ID: "wxid_owner", IDType: "ilink_user_id"}
	env.Data = map[string]any{"content": "private owner notice"}

	if err := r.Emit(context.Background(), env); err != nil {
		t.Fatalf("Emit error: %v", err)
	}
	if got := rc.contents(); len(got) != 1 || got[0] != "private owner notice" {
		t.Fatalf("sent = %v", got)
	}
}

func TestRunnerEmitRejectsUnsupportedTypedRecipient(t *testing.T) {
	r, _ := newEmitTestRunner(t, "acct-1")
	env := channel.NewOutboundEnvelope(channel.OutboundProactiveMessage)
	env.Target = &channel.InboundContext{Channel: "ilink", Account: "acct-1"}
	env.Recipient = &channel.OutboundRecipient{ID: "owner", IDType: "display_name"}
	env.Data = map[string]any{"content": "private owner notice"}
	if err := r.Emit(context.Background(), env); err == nil || !strings.Contains(err.Error(), "id_type") {
		t.Fatalf("unsupported recipient error = %v", err)
	}
}

func TestRunnerEmitRejectsMissingContent(t *testing.T) {
	r, _ := newEmitTestRunner(t, "acct-1")
	env := channel.NewOutboundEnvelope(channel.OutboundProactiveMessage)
	env.Target = &channel.InboundContext{Channel: "ilink", Account: "acct-1", SenderID: "user-9"}
	if err := r.Emit(context.Background(), env); err == nil || !strings.Contains(err.Error(), "content") {
		t.Fatalf("missing content error = %v", err)
	}
}

func TestRunnerEmitMissingAccount(t *testing.T) {
	r, _ := newEmitTestRunner(t, "acct-1")
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "ilink", SenderID: "user-9"} // no Account
	env.Data = map[string]any{"content": "x"}

	err := r.Emit(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "account") {
		t.Errorf("Emit without account: err = %v, want account error", err)
	}
}

func TestRunnerEmitUnknownAccount(t *testing.T) {
	r, _ := newEmitTestRunner(t, "acct-1")
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "ilink", Account: "acct-missing", SenderID: "u"}
	env.Data = map[string]any{"content": "x"}

	err := r.Emit(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "acct-missing") {
		t.Errorf("Emit unknown account: err = %v, want mention of acct-missing", err)
	}
}

func TestRunnerEmitMissingRecipient(t *testing.T) {
	r, _ := newEmitTestRunner(t, "acct-1")
	env := channel.NewOutboundEnvelope(channel.OutboundAssistantFinal)
	env.Target = &channel.InboundContext{Channel: "ilink", Account: "acct-1"} // no SenderID
	env.Data = map[string]any{"content": "x"}

	err := r.Emit(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "sender_id") {
		t.Errorf("Emit without recipient: err = %v, want sender_id error", err)
	}
}

func TestRunnerEmitUnsupportedType(t *testing.T) {
	r, _ := newEmitTestRunner(t, "acct-1")
	env := channel.NewOutboundEnvelope(channel.OutboundAck) // not final/proactive
	env.Target = &channel.InboundContext{Channel: "ilink", Account: "acct-1", SenderID: "u"}
	env.Data = map[string]any{"content": "x"}

	err := r.Emit(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("Emit unsupported type: err = %v, want unsupported error", err)
	}
}

func TestRunnerCapabilities(t *testing.T) {
	r, _ := newEmitTestRunner(t, "acct-1")
	caps := r.Capabilities()
	if !caps.Supports(channel.CapabilityProactiveOutbound) {
		t.Errorf("ilink Capabilities missing proactive_outbound; got %v", caps)
	}
	if !caps.Supports(channel.CapabilityTypedRecipientOutbound) {
		t.Errorf("ilink Capabilities missing typed_recipient_outbound; got %v", caps)
	}
	if !caps.Supports(channel.CapabilityInteractiveHumanResponse) {
		t.Errorf("ilink Capabilities missing interactive_human_response; got %v", caps)
	}
}

func TestRunnerDeliversCurrentSenderHumanRequestWithReceipt(t *testing.T) {
	r, rc := newEmitTestRunner(t, "acct-1")
	target := frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{
		Channel: "ilink", EntrypointID: "ilink-main", Account: "acct-1",
		SenderID: "user-9", Raw: map[string]string{"context_token": "reply-token"},
	}}
	req := humanrequest.HumanRequest{
		ID: "hrq-current", Question: "哪一天？",
		CorrelationToken: "550e8400-e29b-41d4-a716-446655440000",
	}
	if err := r.ValidateHumanRequestDelivery(target); err != nil {
		t.Fatalf("ValidateHumanRequestDelivery() error = %v", err)
	}
	receipt, err := r.DeliverHumanRequest(context.Background(), req, target)
	if err != nil || receipt.MessageID != "client-id" {
		t.Fatalf("DeliverHumanRequest() = %+v, %v", receipt, err)
	}
	if got := rc.deliveryMethods(); len(got) != 1 || got[0] != "send_text" {
		t.Fatalf("delivery methods = %v, want send_text", got)
	}
	if got := rc.contents(); len(got) != 1 || !strings.Contains(got[0], "HR-550E8400E29B41D4A716446655440000") {
		t.Fatalf("delivered text = %v", got)
	}
}

func TestRunnerDeliversOwnerHumanRequestWithTypedPrivatePush(t *testing.T) {
	r, rc := newEmitTestRunner(t, "acct-1")
	target := frt.HumanRequestDeliveryTarget{
		Route:     channel.InboundContext{Channel: "ilink", EntrypointID: "ilink-owner", Account: "acct-1"},
		Recipient: &channel.OutboundRecipient{ID: "owner-1", IDType: "ilink_user_id"},
	}
	req := humanrequest.HumanRequest{
		ID: "hrq-owner", Question: "批准合同？",
		CorrelationToken: "550e8400-e29b-41d4-a716-446655440000",
		Options:          []humanrequest.HumanOption{{ID: "approve", Label: "批准"}},
	}
	receipt, err := r.DeliverHumanRequest(context.Background(), req, target)
	if err != nil || receipt.MessageID != "client-id" {
		t.Fatalf("DeliverHumanRequest() = %+v, %v", receipt, err)
	}
	if got := rc.deliveryMethods(); len(got) != 1 || got[0] != "push" {
		t.Fatalf("delivery methods = %v, want push", got)
	}

	bad := target
	bad.Recipient = &channel.OutboundRecipient{ID: "owner-1", IDType: "display_name"}
	if err := r.ValidateHumanRequestDelivery(bad); err == nil || !strings.Contains(err.Error(), "id_type") {
		t.Fatalf("unsupported recipient validation error = %v", err)
	}
}

func TestRunnerHumanRequestDeliveryRouteFailsClosed(t *testing.T) {
	r, _ := newEmitTestRunner(t, "acct-1")
	tests := []struct {
		name   string
		target frt.HumanRequestDeliveryTarget
		want   string
	}{
		{name: "missing account", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{SenderID: "user"}}, want: "account"},
		{name: "unknown account", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Account: "missing", SenderID: "user"}}, want: "not registered"},
		{name: "missing current recipient", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Account: "acct-1"}}, want: "recipient"},
		{name: "empty typed recipient", target: frt.HumanRequestDeliveryTarget{Route: channel.InboundContext{Account: "acct-1"}, Recipient: &channel.OutboundRecipient{IDType: "ilink_user_id"}}, want: "recipient"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := r.ValidateHumanRequestDelivery(tt.target); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
