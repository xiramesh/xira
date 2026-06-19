package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

// TestRunEmitsAssistantFinalOnCompleted verifies the runtime gap fix: a
// completed run with a non-empty final response MUST publish a live
// `assistant.final` event (drain signal for the progress forwarder, see
// docs/architecture/xira-conversation-progress-feed-v0.zh.md §8.5). The event
// must be conversation-visible, carry only `final_chars` (never the full final
// text), and precede `run.finished`.
func TestRunEmitsAssistantFinalOnCompleted(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "hi",
		Context: channel.NewInboundContext("test", "user-1", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != "completed" {
		t.Fatalf("status = %q, want completed", resp.Status)
	}
	if strings.TrimSpace(resp.FinalResponse) == "" {
		t.Fatalf("expected non-empty final response")
	}
	evt, ok := findEvent(resp.Events, "assistant.final")
	if !ok {
		t.Fatalf("events missing assistant.final: %v", eventKinds(resp.Events))
	}
	if evt.Visibility == nil || !evt.Visibility.Conversation {
		t.Fatalf("assistant.final must be conversation-visible: %+v", evt.Visibility)
	}
	// Full final text must NOT leak into the event (avoid triple-store).
	if strings.Contains(evt.Message, resp.FinalResponse) {
		t.Fatalf("assistant.final message leaks final text: %q", evt.Message)
	}
	if finalChars, _ := evt.Payload["final_chars"]; finalChars == nil {
		t.Fatalf("assistant.final payload missing final_chars: %+v", evt.Payload)
	}
	// Must precede run.finished so the forwarder can drain before the runner
	// sends the final answer.
	finalIdx, finishedIdx := -1, -1
	for i, e := range resp.Events {
		switch e.Kind {
		case "assistant.final":
			finalIdx = i
		case "run.finished":
			finishedIdx = i
		}
	}
	if finalIdx < 0 || finishedIdx < 0 || finalIdx >= finishedIdx {
		t.Fatalf("assistant.final (%d) must precede run.finished (%d)", finalIdx, finishedIdx)
	}
}

// TestRunDoesNotEmitAssistantFinalOnWaitingHuman: a HITL run has no final
// answer ready, so assistant.final must NOT be published. Stop() is the
// forwarder's fallback stop signal in this case.
func TestRunDoesNotEmitAssistantFinalOnWaitingHuman(t *testing.T) {
	var calls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls > 1 {
			t.Fatalf("model called after human.request interrupt")
		}
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("hr-final-1", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Approve assistant.final drain smoke?",
			}))),
		}, nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "ask a human",
		Context: channel.NewInboundContext("test", "user-1", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman {
		t.Fatalf("status = %q, want waiting_human", resp.Status)
	}
	if _, ok := findEvent(resp.Events, "assistant.final"); ok {
		t.Fatalf("assistant.final must NOT be emitted on waiting_human: %v", eventKinds(resp.Events))
	}
}
