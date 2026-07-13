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
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

// progress_visibility_test.go: tests the run.waiting_human summary contract.
//
// The eventVisibility switch + Visibility field were removed (issue #43): the
// per-chat-key architecture routes events by Event TYPE (renderEventText's
// type switch in progress/render_event.go), not by a kind→visibility map. The
// old forwarder that filtered on Visibility.Conversation was itself removed in
// Phase 6b (a32dae7). The two tests that asserted on eventVisibility() directly
// (TestV0FactEventsAreConversationVisible / TestRawKindsStayNonConversation)
// guarded that deleted forwarder's filter and are gone with it — the rendering
// contract they indirectly checked now lives in render_event_test.go's
// type-switch assertions. What remains here is the waiting_human *summary*
// contract (the human-facing question text spliced into the event payload).

// TestRunWaitingHumanEventCarriesConversationSummary: the waiting_human event
// must carry a human-facing `summary` (rendered into IM chat). The summary is
// derived from the interrupt reason / first human request question.
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

func TestWaitingHumanEventPayloadDoesNotExposeOwnerQuestion(t *testing.T) {
	request := humanrequest.HumanRequest{
		ID: "hrq-owner", Question: "Confidential owner decision",
		Responder: humanrequest.ResponderPolicy{Type: humanrequest.ResponderOwner},
		Delivery:  humanrequest.DeliveryState{Status: humanrequest.DeliverySent},
	}
	payload := waitingHumanEventPayload(&RunInterrupt{Reason: "human input requested", HumanRequests: []humanrequest.HumanRequest{request}})
	if payload["summary"] != "" || payload["question"] != nil {
		t.Fatalf("owner question leaked into waiting payload: %+v", payload)
	}
	if payload["human_request_id"] != "hrq-owner" || payload["responder_type"] != humanrequest.ResponderOwner || payload["delivery_status"] != humanrequest.DeliverySent {
		t.Fatalf("owner routing state missing from waiting payload: %+v", payload)
	}
	created := humanRequestEventPayload(request)
	if created["question"] != nil {
		t.Fatalf("owner question leaked into created payload: %+v", created)
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
