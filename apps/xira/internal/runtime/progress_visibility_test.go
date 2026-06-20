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

// TestV0FactEventsAreConversationVisible: the v0 progress forwarder filters
// on Visibility.Conversation == true. The three runtime-fact kinds it must
// deliver (run.waiting_human, agent.delegate.failed, agent.delegate.timeout)
// currently fall through to the default conversation=false rule and would be
// silently dropped. They MUST be explicitly conversation-visible. See
// docs/architecture/xira-conversation-progress-feed-v0.zh.md §7.
func TestV0FactEventsAreConversationVisible(t *testing.T) {
	for _, kind := range []string{
		"run.waiting_human",
		"agent.delegate.failed",
		"agent.delegate.timeout",
	} {
		v := eventVisibility(kind)
		if v == nil {
			t.Fatalf("eventVisibility(%q) = nil", kind)
		}
		if !v.Conversation {
			t.Fatalf("kind %q must be conversation-visible, got %+v", kind, v)
		}
		// Assistant-authored status stays conversation-visible too (unchanged).
	}
}

// Negative check: raw / high-frequency kinds must NOT flip to conversation.
func TestRawKindsStayNonConversation(t *testing.T) {
	for _, kind := range []string{
		"adk.event",
		"tool.started",
		"tool.completed",
		"model.policy_resolved",
		"context.item.included",
		"run.started",
		"run.finished",
	} {
		if v := eventVisibility(kind); v != nil && v.Conversation {
			t.Fatalf("kind %q must NOT be conversation-visible, got %+v", kind, v)
		}
	}
}

// TestRunWaitingHumanEventCarriesConversationSummary: the waiting_human event
// must be conversation-visible AND carry a human-facing `summary` (rendered
// into the IM chat by the forwarder). The summary is derived from the
// interrupt reason / first human request question.
func TestRunWaitingHumanEventCarriesConversationSummary(t *testing.T) {
	const question = "Approve waiting_human summary smoke?"
	rt := newWaitingHumanService(t, question)
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
	evt, ok := findEvent(resp.Events, "run.waiting_human")
	if !ok {
		t.Fatalf("events missing run.waiting_human: %v", eventKinds(resp.Events))
	}
	if evt.Visibility == nil || !evt.Visibility.Conversation {
		t.Fatalf("run.waiting_human must be conversation-visible: %+v", evt.Visibility)
	}
	summary, _ := evt.Payload["summary"].(string)
	if !strings.Contains(summary, question) {
		t.Fatalf("run.waiting_human summary = %q, want it to contain %q", summary, question)
	}
}

// TestWaitingHumanSummaryNilInterrupt: a nil interrupt (defensive) yields an
// empty summary rather than panicking.
func TestWaitingHumanSummaryNilInterrupt(t *testing.T) {
	if got := waitingHumanSummary(nil); got != "" {
		t.Fatalf("waitingHumanSummary(nil) = %q, want empty", got)
	}
}

// TestWaitingHumanSummaryFallsBackToReason: with no human requests, the
// interrupt reason is used as the summary.
func TestWaitingHumanSummaryFallsBackToReason(t *testing.T) {
	got := waitingHumanSummary(&RunInterrupt{Reason: "awaiting approval"})
	if got != "awaiting approval" {
		t.Fatalf("summary = %q, want reason fallback", got)
	}
}

// newWaitingHumanService builds a runtime backed by a fake DeepSeek client
// whose first model turn calls human.request with the given question, driving
// the run into the waiting_human state.
func newWaitingHumanService(t *testing.T, question string) *Service {
	t.Helper()
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
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("hr-vis-1", "human_request", map[string]any{
				"kind":     "freeform",
				"question": question,
			}))),
		}, nil
	})}
	return newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
}
