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
	fsession "github.com/xiramesh/xira/internal/session"
)

// human_request_hydration_test.go: #106 — pending HITL hydration into RunAgent.
//
// These tests pin the contract that a fresh RunAgent turn sees a structured
// "Pending Human Requests" summary injected into the user message when the
// chatKey has pending HITL, AND that pending HITL session messages no longer
// leak as bare question text via history hydration (replaced by the summary).

// chatKeyForTestUser mirrors what RunAgent computes for a TurnRequest whose
// InboundContext is NewInboundContext("test", "user-1", nil): ChatID falls
// back to SenderID when metadata has no chat_id (channel/types.go:63-65), so
// ChatKey = "test/user-1/user-1".
func chatKeyForTestUser() string {
	return "test/user-1/user-1"
}

// seedPendingHumanRequest creates a waiting_human run + a pending HumanRequest
// owned by the test user's chatKey, so a subsequent RunAgent turn for that
// chatKey will see it. Returns the created request.
func seedPendingHumanRequest(t *testing.T, rt *Service, runID, question, source string, options []humanrequest.HumanOption) *humanrequest.HumanRequest {
	t.Helper()
	ctx := context.Background()
	if err := rt.runs.SaveRun(TurnResponse{
		RunID:   runID,
		AgentID: "xira-assistant",
		Status:  StatusWaitingHuman,
	}); err != nil {
		t.Fatalf("SaveRun waiting_human: %v", err)
	}
	kind := humanrequest.RequestFreeform
	if source == "flow_human_approval" {
		kind = humanrequest.RequestApproval
	}
	hr, err := rt.CreateHumanRequest(ctx, humanrequest.CreateRequest{
		WorkspaceID:  rt.workspace,
		WorkspaceKey: rt.WorkspaceKey(),
		RunID:        runID,
		AgentID:      "xira-assistant",
		SessionID:    "session-" + runID,
		Kind:         kind,
		Question:     question,
		Source:       source,
		Options:      options,
		ChatKey:      chatKeyForTestUser(),
	})
	if err != nil {
		t.Fatalf("CreateHumanRequest: %v", err)
	}
	return hr
}

// capturingClient builds a deepseek.Client whose transport captures every
// outbound ChatRequest into *captured and returns a plain text response.
func capturingClient(t *testing.T, captured *deepseek.ChatRequest) *deepseek.Client {
	t.Helper()
	return deepseek.New(
		deepseek.WithBaseURLForTest("http://deepseek.test"),
		deepseek.WithAPIKey("test-key"),
		deepseek.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(r.Body).Decode(captured); err != nil {
				return nil, err
			}
			body := deepSeekTextResponse("ok")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})}),
	)
}

// TestRunAgentInjectsPendingHITLSummary proves #106's core: a fresh RunAgent
// turn for a chatKey with a pending HITL sees a structured summary in the user
// message, so the agent knows what's awaiting an answer.
func TestRunAgentInjectsPendingHITLSummary(t *testing.T) {
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})
	seedPendingHumanRequest(t, rt, "run-pending-1", "Which deployment window?", "agent_request", nil)

	var captured deepseek.ChatRequest
	rt.deepseek = capturingClient(t, &captured)

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Use the Tuesday window.",
		Context: channel.NewInboundContext("test", "user-1", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if resp.Status != "completed" && resp.Status != "failed" {
		t.Fatalf("status = %q, want completed/failed", resp.Status)
	}
	msg := lastUserMessage(captured.Messages)
	if !strings.Contains(msg, "Pending Human Requests") {
		t.Errorf("user message missing pending HITL summary marker.\nmessage: %s", msg)
	}
	if !strings.Contains(msg, "Which deployment window?") {
		t.Errorf("user message missing the pending question text.\nmessage: %s", msg)
	}
	if !strings.Contains(msg, "Use the Tuesday window.") {
		t.Errorf("user message missing the original user text.\nmessage: %s", msg)
	}
}

// TestRunAgentNoPendingNoInjection is the regression guard: when the chatKey
// has no pending HITL, the user message is unchanged (no summary appended).
func TestRunAgentNoPendingNoInjection(t *testing.T) {
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})

	var captured deepseek.ChatRequest
	rt.deepseek = capturingClient(t, &captured)

	if _, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "hello there",
		Context: channel.NewInboundContext("test", "user-1", nil),
	}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	msg := lastUserMessage(captured.Messages)
	if strings.Contains(msg, "Pending Human Requests") {
		t.Errorf("user message should NOT contain pending summary when none pending.\nmessage: %s", msg)
	}
	if !strings.Contains(msg, "hello there") {
		t.Errorf("user message missing original text.\nmessage: %s", msg)
	}
}

// TestPendingHITLSummaryIncludesOptions proves the summary surfaces the
// approval options for a request that has them, so the agent can guide the
// user (and #108's option matching has the same options visible).
func TestPendingHITLSummaryIncludesOptions(t *testing.T) {
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})
	opts := []humanrequest.HumanOption{
		{ID: "approve", Label: "允许"},
		{ID: "deny", Label: "拒绝"},
	}
	seedPendingHumanRequest(t, rt, "run-opts-1", "Approve the merge?", "agent_request", opts)

	var captured deepseek.ChatRequest
	rt.deepseek = capturingClient(t, &captured)

	if _, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "ok",
		Context: channel.NewInboundContext("test", "user-1", nil),
	}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	msg := lastUserMessage(captured.Messages)
	if !strings.Contains(msg, "approve") || !strings.Contains(msg, "允许") {
		t.Errorf("pending summary missing option approve/允许.\nmessage: %s", msg)
	}
}

// TestHydrateSkipsPendingHumanRequestMessage proves #106's hydrate fix at the
// unit level: adkEventFromSessionMessage skips a human_request message whose
// run_status is waiting_human, so the pending question does NOT leak as a bare
// assistant text into the next turn's model context (it's replaced by the
// injected summary from injectPendingHITLSummary).
func TestHydrateSkipsPendingHumanRequestMessage(t *testing.T) {
	msg := fsession.Message{
		Role:    "assistant",
		Kind:    fsession.MessageKindHumanRequest,
		Content: "Which deployment window?",
		Metadata: map[string]any{
			"run_status": StatusWaitingHuman,
		},
	}
	_, _, ok := adkEventFromSessionMessage(msg, "xira-assistant")
	if ok {
		t.Fatal("pending human_request (run_status=waiting_human) should be skipped by adkEventFromSessionMessage, got ok=true")
	}
}

// TestHydrateKeepsResolvedHumanResponseMessage proves the counterpart: a
// resolved human_response is NOT skipped — it stays in history so later turns
// in the same session can see "the user already answered X". Only pending
// (waiting_human) is skipped.
//
// NOTE on data shape (§5.4 — test data must match production): the resume
// path persists via persistResumeSessionMessages (service.go:1441), which does
// NOT stamp run_status. So a real resolved HITL message has run_status == ""
// (absent), NOT "completed". The earlier version of this test wrongly set
// run_status="completed" — a shape that never occurs in production. Fixed to
// use the real shape (empty/absent run_status) so the test exercises the
// actual skip predicate.
func TestHydrateKeepsResolvedHumanResponseMessage(t *testing.T) {
	msg := fsession.Message{
		Role:    "user",
		Kind:    fsession.MessageKindHumanResponse,
		Content: "approve by alice: yes",
		// Resolved HITL messages from the resume path have NO run_status
		// (persistResumeSessionMessages doesn't stamp it). nil Metadata mirrors
		// the most realistic shape; the skip predicate must read it as "".
		Metadata: nil,
	}
	_, _, ok := adkEventFromSessionMessage(msg, "xira-assistant")
	if !ok {
		t.Fatal("resolved human_response (no run_status) should be kept, got ok=false")
	}
}

// TestHydrateKeepsResolvedHumanRequestMessage proves a resolved human_request
// (its run resumed and finished) is also kept — it's the question half of a
// resolved pair and belongs in history. Same data-shape note as above: real
// resolved messages have empty/absent run_status.
func TestHydrateKeepsResolvedHumanRequestMessage(t *testing.T) {
	msg := fsession.Message{
		Role:    "assistant",
		Kind:    fsession.MessageKindHumanRequest,
		Content: "Approve the merge?",
		Metadata: map[string]any{
			// Resolved via the normal RunAgent path CAN carry run_status=completed
			// (sessionMessagesForRun stamps it). Cover that shape too — both the
			// empty (resume path) and completed (normal path) forms must be kept.
			"run_status": "completed",
		},
	}
	_, _, ok := adkEventFromSessionMessage(msg, "xira-assistant")
	if !ok {
		t.Fatal("resolved human_request (run_status=completed) should be kept, got ok=false")
	}
}

// TestHydrateKeepsResolvedHumanRequestNoRunStatus pins the resume-path shape
// explicitly: a human_request with absent run_status (the real resume-path
// shape, per persistResumeSessionMessages not stamping) must NOT be skipped.
func TestHydrateKeepsResolvedHumanRequestNoRunStatus(t *testing.T) {
	msg := fsession.Message{
		Role:    "assistant",
		Kind:    fsession.MessageKindHumanRequest,
		Content: "Approve the merge?",
		Metadata: map[string]any{
			"human_request_id": "hr_x", // resume path DOES stamp this, just not run_status
		},
	}
	_, _, ok := adkEventFromSessionMessage(msg, "xira-assistant")
	if !ok {
		t.Fatal("resolved human_request (absent run_status, resume-path shape) should be kept, got ok=false")
	}
}

// TestFormatHumanOptionVariants pins formatHumanOption's three branches so the
// summary's option rendering stays stable (id/label differs, label-only,
// id-only).
func TestFormatHumanOptionVariants(t *testing.T) {
	cases := []struct {
		name string
		opt  humanrequest.HumanOption
		want string
	}{
		{"id and label differ", humanrequest.HumanOption{ID: "approve", Label: "允许"}, "approve/允许"},
		{"label only", humanrequest.HumanOption{ID: "", Label: "允许"}, "允许"},
		{"id only", humanrequest.HumanOption{ID: "approve", Label: ""}, "approve"},
		{"id equals label", humanrequest.HumanOption{ID: "ok", Label: "ok"}, "ok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatHumanOption(c.opt); got != c.want {
				t.Errorf("formatHumanOption(%+v) = %q, want %q", c.opt, got, c.want)
			}
		})
	}
}

// TestListPendingHumanRequestsByChatKeyNilStore pins the nil-store guard
// (returns empty, not error) — covering the early-return branch that keeps
// callers (channel preflight, RunAgent injection) safe when HITL is disabled.
func TestListPendingHumanRequestsByChatKeyNilStore(t *testing.T) {
	s := &Service{} // humanRequests == nil
	got, err := s.ListPendingHumanRequestsByChatKey(context.Background(), "feishu/c/u")
	if err != nil {
		t.Fatalf("nil store should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("nil store should return empty, got %d", len(got))
	}
}
