package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
)

// Live (real DeepSeek API) coverage for the conversation progress feed
// contract changes. Skipped unless XIRA_DEEPSEEK_LIVE=1 and DEEPSEEK_API_KEY
// are set. These exercise the full runtime path (ADK, tools, event bus) end to
// end against the real provider — complementing the fast fake-HTTP unit tests.

// TestLiveProgressFeedAssistantFinal: a real completed run publishes a live
// assistant.final event (drain signal for the forwarder, §8.5).
func TestLiveProgressFeedAssistantFinal(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, false)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Reply with a single short greeting sentence. Do not call any tools.",
		Context: channel.NewInboundContext("test", "live-progress-user", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != "completed" {
		t.Fatalf("status = %q, want completed (final=%q)", resp.Status, resp.FinalResponse)
	}
	evt, ok := findEvent(resp.Events, "assistant.final")
	if !ok {
		t.Fatalf("live run missing assistant.final: %v", eventKinds(resp.Events))
	}
	if strings.Contains(evt.Message, resp.FinalResponse) {
		t.Fatalf("assistant.final must not leak the full final text: %q", evt.Message)
	}
}

// TestLiveProgressFeedWaitingHumanSummary: a real HITL run publishes a
// conversation-visible run.waiting_human event carrying a human-facing summary
// rendered by the forwarder (§7, §14).
func TestLiveProgressFeedWaitingHumanSummary(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, false)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Live progress smoke: call human.request exactly once and ask `Approve progress feed live smoke?`. Do not answer normally.",
		Context: channel.NewInboundContext("test", "live-progress-user", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman {
		t.Fatalf("status = %q, want waiting_human", resp.Status)
	}
	evt, ok := findEvent(resp.Events, "run.waiting_human")
	if !ok {
		t.Fatalf("live run missing run.waiting_human: %v", eventKinds(resp.Events))
	}
	summary, _ := evt.Payload["summary"].(string)
	if strings.TrimSpace(summary) == "" {
		t.Fatalf("run.waiting_human missing summary payload: %+v", evt.Payload)
	}
	if !strings.Contains(summary, "Approve progress feed live smoke") {
		t.Fatalf("run.waiting_human summary = %q, want it to carry the question", summary)
	}
}
