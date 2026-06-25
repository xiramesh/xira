package ilink

import (
	"context"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
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
}
