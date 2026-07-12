package runtime

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

// answer_child_test.go: tests executeAnswerChild — the #68 tool a parent LLM
// uses to answer a spawned child's HITL question on the child's behalf.
//
// Key contract under test:
//  1. answer_child MUST be non-blocking. Resolving a child's HumanRequest
//     triggers resumeDirectHumanRequest → a full s.generate (child LLM run),
//     which is heavy and synchronous. Running it in the parent's tool handler
//     would freeze the parent's event loop and disable the steering checkpoint
//     (same death as the retired wait_turn, PR #53 — see poll_turn.go:17-23).
//     So executeAnswerChild launches the resolve in a detached goroutine and
//     returns {status:"answering"} immediately.
//  2. The detached goroutine resolves the child's HumanRequest with
//     Kind=ResponseAnswer, Actor="parent_agent", Message=<the parent's answer>.
//     The child's resume then runs in the background and its final routes back
//     to the parent's chat key (断裂 A fix).

// newAnswerChildTestService builds a runtime whose resume model-call returns a
// plain final (good enough to complete the child turn), with a live HumanRequest
// store — enough to assert that answer_child RESOLVES the child's request.
func newAnswerChildTestService(t *testing.T) *Service {
	t.Helper()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return deepSeekHTTPResponse(deepSeekTextResponse("child resumed with the answer")), nil
	})}
	return newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
}

// TestSanitizeAnswerChildInput covers input cleaning incl. the unsupported-field
// branch (mirrors TestSanitizePollTurnInput).
func TestSanitizeAnswerChildInput(t *testing.T) {
	spec, clean, unsupported := sanitizeAnswerChildInput(map[string]any{
		"human_request_id": "  hr-1  ",
		"answer":           "  Tuesday  ",
	})
	if spec.HumanRequestID != "hr-1" {
		t.Errorf("HumanRequestID = %q, want 'hr-1' (trimmed)", spec.HumanRequestID)
	}
	if spec.Answer != "Tuesday" {
		t.Errorf("Answer = %q, want 'Tuesday' (trimmed)", spec.Answer)
	}
	if len(unsupported) != 0 {
		t.Errorf("unsupported = %v, want empty", unsupported)
	}
	if clean["human_request_id"] != "hr-1" || clean["answer"] != "Tuesday" {
		t.Errorf("clean map = %+v", clean)
	}

	// Unsupported fields reported, spec still extracted.
	_, _, unsupported = sanitizeAnswerChildInput(map[string]any{
		"human_request_id": "hr-2",
		"answer":           "x",
		"foo":              1,
	})
	if len(unsupported) != 1 || unsupported[0] != "foo" {
		t.Errorf("unsupported = %v, want [foo]", unsupported)
	}
}

// TestAnswerChildInputValidate covers field validation directly.
func TestAnswerChildInputValidate(t *testing.T) {
	if err := (answerChildInput{HumanRequestID: "hr", Answer: "a"}).Validate(); err != nil {
		t.Errorf("valid input errored: %v", err)
	}
	if err := (answerChildInput{}).Validate(); err == nil {
		t.Errorf("empty input should error")
	}
	if err := (answerChildInput{HumanRequestID: "hr"}).Validate(); err == nil {
		t.Errorf("missing answer should error")
	}
	if err := (answerChildInput{Answer: "a"}).Validate(); err == nil {
		t.Errorf("missing id should error")
	}
}

// TestExecuteAnswerChildResolvesChildHumanRequest is the #68 core: a parent
// answering a child's HITL question resolves the child's pending HumanRequest
// with the parent's answer.
func TestExecuteAnswerChildResolvesChildHumanRequest(t *testing.T) {
	rt := newAnswerChildTestService(t)
	ctx := context.Background()

	// Seed a child run + pending HumanRequest (as if the child called
	// human.request). Use a real child run so resume has a target to Load.
	if err := rt.runs.SaveRun(TurnResponse{
		RunID:   "child-answer-1",
		AgentID: "xira-assistant",
		Status:  StatusWaitingHuman,
	}); err != nil {
		t.Fatal(err)
	}
	hr, err := rt.CreateHumanRequest(ctx, humanrequest.CreateRequest{
		WorkspaceID:  rt.workspace,
		WorkspaceKey: rt.WorkspaceKey(),
		RunID:        "child-answer-1",
		AgentID:      "xira-assistant",
		SessionID:    "session-child-1",
		Kind:         humanrequest.RequestFreeform,
		Question:     "Which deployment window?",
		Source:       "agent_request",
	})
	if err != nil {
		t.Fatalf("CreateHumanRequest: %v", err)
	}

	// Parent answers the child's question.
	out := executeAnswerChild(ctx, rt, answerChildInput{
		HumanRequestID: hr.ID,
		Answer:         "Use the Tuesday window.",
	})
	if out["status"] != "answering" {
		t.Errorf("status = %v, want 'answering' (resolve must be async, non-blocking)", out["status"])
	}

	// The detached goroutine resolves the child's HumanRequest, then runs the
	// child's resume (a model call). The test MUST wait for the resume to fully
	// finish before returning: the resume goroutine writes to the child run
	// directory, and t.TempDir() cleanup races with a still-running resume
	// ("directory not empty"). We poll until BOTH the HumanRequest is resolved
	// AND the child run has reached a terminal status (resume done).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := rt.GetHumanRequest(ctx, hr.ID)
		if err == nil && got.Status == humanrequest.StatusResolved {
			if got.Response == nil {
				t.Fatalf("HumanRequest resolved but Response is nil")
			}
			if got.Response.Kind != humanrequest.ResponseAnswer {
				t.Errorf("response Kind = %v, want %q", got.Response.Kind, humanrequest.ResponseAnswer)
			}
			if got.Response.Message != "Use the Tuesday window." {
				t.Errorf("response Message = %q, want the parent's answer", got.Response.Message)
			}
			if got.Response.Actor != "parent_agent" {
				t.Errorf("response Actor = %q, want 'parent_agent'", got.Response.Actor)
			}
			break // HumanRequest resolved — now wait for the resume to finish.
		}
		time.Sleep(10 * time.Millisecond)
		continue
	}

	// Wait for the child's resume to reach a terminal status so the detached
	// goroutine is done writing before TempDir cleanup. Without this the test
	// flakes ("directory not empty").
	resumeDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(resumeDeadline) {
		run, err := rt.RunStore().Load("child-answer-1")
		if err == nil && (run.Status == "completed" || run.Status == "failed") {
			break // resume finished (completed = model returned a final).
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run, err := rt.RunStore().Load("child-answer-1"); err == nil && run.Status == StatusWaitingHuman {
		t.Fatalf("child run still waiting_human — resume did not fire within timeout")
	}
}

// TestExecuteAnswerChildRejectsMissingInput: validation — both fields required.
func TestExecuteAnswerChildRejectsMissingInput(t *testing.T) {
	rt := newAnswerChildTestService(t)
	ctx := context.Background()

	out := executeAnswerChild(ctx, rt, answerChildInput{})
	if out["status"] != "rejected" {
		t.Errorf("status = %v, want 'rejected'", out["status"])
	}
	if out["error"] == nil {
		t.Errorf("expected an error message, got %+v", out)
	}

	out = executeAnswerChild(ctx, rt, answerChildInput{HumanRequestID: "hr-x"})
	if out["status"] != "rejected" {
		t.Errorf("missing answer: status = %v, want 'rejected'", out["status"])
	}

	out = executeAnswerChild(ctx, rt, answerChildInput{Answer: "something"})
	if out["status"] != "rejected" {
		t.Errorf("missing human_request_id: status = %v, want 'rejected'", out["status"])
	}
}

// TestExecuteAnswerChildRejectsUnknownRequest: resolving a non-existent
// HumanRequest reports a rejection, not a panic.
func TestExecuteAnswerChildRejectsUnknownRequest(t *testing.T) {
	rt := newAnswerChildTestService(t)
	ctx := context.Background()

	out := executeAnswerChild(ctx, rt, answerChildInput{
		HumanRequestID: "does-not-exist",
		Answer:         "anything",
	})
	if out["status"] != "rejected" {
		t.Errorf("status = %v, want 'rejected' for unknown HumanRequest", out["status"])
	}
}

// TestAnswerChildResumeKeepsChildWorking (断裂 B documentation, option A):
// the #68 child resume path (resumeDirectHumanRequest) is DELIBERATELY
// asymmetric with the approved-tool resume path. After the parent answers,
// the child must CONTINUE working — its tools/skills kept, native tools NOT
// disabled — because the semantic is "the question was answered, now finish
// the job." This test pins that contract: the resumed child reaches
// "completed" via a real model call (proving it could still generate), not a
// tool-less "I can't do anything" failure.
//
// #68 (the child could never finish its task after the parent answers).
func TestAnswerChildResumeKeepsChildWorking(t *testing.T) {
	rt := newAnswerChildTestService(t)
	ctx := context.Background()

	if err := rt.runs.SaveRun(TurnResponse{
		RunID:   "child-working-1",
		AgentID: "xira-assistant",
		Status:  StatusWaitingHuman,
		Message: "deploy",
	}); err != nil {
		t.Fatal(err)
	}
	hr, err := rt.CreateHumanRequest(ctx, humanrequest.CreateRequest{
		WorkspaceID:  rt.workspace,
		WorkspaceKey: rt.WorkspaceKey(),
		RunID:        "child-working-1",
		AgentID:      "xira-assistant",
		SessionID:    "session-child-working",
		Kind:         humanrequest.RequestFreeform,
		Question:     "Which window?",
		Source:       "agent_request",
	})
	if err != nil {
		t.Fatal(err)
	}

	out := executeAnswerChild(ctx, rt, answerChildInput{
		HumanRequestID: hr.ID,
		Answer:         "Tuesday",
	})
	if out["status"] != "answering" {
		t.Fatalf("status = %v, want 'answering'", out["status"])
	}

	// Wait for the child's resume to reach a terminal status (resume done).
	// The stub LLM returns a plain final, so "completed" proves the resume
	// ran generate with the model — tools were kept, not stripped.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := rt.RunStore().Load("child-working-1")
		if err == nil && (run.Status == "completed" || run.Status == "failed") {
			if run.Status != "completed" {
				t.Fatalf("child resume ended in %q with error metadata %+v — resume did not let the child finish its job (断裂 B: tools must be kept on the answer-resume path)", run.Status, run.Metadata)
			}
			request, err := rt.GetHumanRequest(ctx, hr.ID)
			if err == nil && request.Resume.Status == humanrequest.ResumeCompleted {
				return // pass: child and its durable resume bookkeeping both completed
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child run and durable resume bookkeeping did not complete within timeout")
}
