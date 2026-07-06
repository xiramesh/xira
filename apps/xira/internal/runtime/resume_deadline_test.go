package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

// resume_deadline_test.go: tests for issue #76 P1 — resume must be bounded by
// an overall deadline so a hanging generate (multi-round tool loop) cannot run
// unbounded, especially now that answer_child (#68) triggers resume in a
// detached, unattended goroutine.
//
// # Why the test drives the REAL resume path
//
// generateADK's checkpoint (suspend/steer/deadline) lives inside the ADK event
// loop, which is hard to exercise with a unit stub (the steering checkpoint —
// same shape — has no direct unit test either, by the same constraint). So the
// deadline contract is driven end-to-end through resumeDirectHumanRequest:
// seed a waiting_human run + pending HumanRequest, then resume with a ctx that
// expires BEFORE the (slow) LLM responds, and assert the run is marked failed
// rather than left waiting forever.
//
// The deadline is made controllable from the test by passing a parent ctx with
// a short WithTimeout: resume's internal WithTimeout(MaxDurationMS, ~120s) does
// NOT override a shorter parent deadline — Go's nested context picks the
// EARLIER deadline. So a test parent ctx of, say, 80ms forces the resume to
// time out regardless of the 120s policy default.

// slowDeepSeekClient returns a stub transport that blocks until the request's
// ctx is cancelled, mirroring how the REAL DeepSeek client behaves
// (http.NewRequestWithContext — the in-flight request aborts on ctx cancel).
// A naive time.Sleep stub would NOT honour ctx and would mask whether the
// deadline actually propagates; this one watches r.Context().Done().
func slowDeepSeekClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// Block until the request ctx is cancelled (as a real HTTP client
		// would abort the in-flight request on ctx cancel). Then surface a
		// transport error — generateADK's err branch / the resume's generate
		// error path takes over.
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
}

func newResumeDeadlineTestService(t *testing.T) *Service {
	t.Helper()
	return newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(slowDeepSeekClient())),
	})
}

// TestResumeDirectHumanRequestTimesOutAndMarksRunFailed is the #76 core test:
// a resume whose generate cannot finish before the deadline must mark the run
// failed (error_type=resume_timeout), NOT leave it stuck in waiting_human
// forever. Without the ctx-deadline checkpoint in generateADK, the resume
// would keep running the slow LLM to completion and never surface the timeout.
func TestResumeDirectHumanRequestTimesOutAndMarksRunFailed(t *testing.T) {
	rt := newResumeDeadlineTestService(t)

	// Seed a waiting_human run + pending HumanRequest (as if a child called
	// human.request, then the parent answered via answer_child).
	if err := rt.runs.SaveRun(TurnResponse{
		RunID:   "child-timeout-1",
		AgentID: "xira-assistant",
		Status:  StatusWaitingHuman,
		Message: "deploy",
	}); err != nil {
		t.Fatal(err)
	}
	hr, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		WorkspaceID:  rt.workspace,
		WorkspaceKey: rt.WorkspaceKey(),
		RunID:        "child-timeout-1",
		AgentID:      "xira-assistant",
		SessionID:    "session-timeout-1",
		Kind:         humanrequest.RequestFreeform,
		Question:     "Which window?",
		Source:       "agent_request",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Mark the HumanRequest resolved (as answer_child would), so resume fires.
	hr.Response = &humanrequest.HumanResponse{
		RequestID: hr.ID,
		Kind:      humanrequest.ResponseAnswer,
		Actor:     "parent_agent",
		Message:   "Tuesday",
	}

	// Parent ctx with a SHORT deadline — nested under resume's internal
	// WithTimeout(120s), so the effective deadline is this 80ms one.
	parentCtx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	// The resume HANDLES the timeout by marking the run failed (a deliberate
	// "successful handling" of the deadline) — it does NOT propagate the
	// timeout error upward. So we assert the run state, not a returned error.
	if err := rt.resumeDirectHumanRequest(parentCtx, hr); err != nil {
		t.Fatalf("resumeDirectHumanRequest returned error %v (timeout should be handled by marking the run failed, not propagated)", err)
	}

	run, err := rt.RunStore().Load("child-timeout-1")
	if err != nil {
		t.Fatal(err)
	}
	// #76 contract: the run must be marked failed, not stuck waiting_human.
	if run.Status != "failed" {
		t.Errorf("run Status = %q, want 'failed' (resume timeout must not leave the run waiting_human forever)", run.Status)
	}
	if run.Metadata["error_type"] != "resume_timeout" {
		t.Errorf("run Metadata[error_type] = %q, want 'resume_timeout'", run.Metadata["error_type"])
	}
}

// TestGenerateAbortsWhenContextAlreadyCancelled drives the ctx-deadline
// checkpoint in generateADK directly: with a stub LLM that IGNORES ctx (returns
// a final event regardless), an already-cancelled ctx must make generate return
// ctx.Err() via the checkpoint — proving the checkpoint fires between
// iterations rather than letting the loop run to its natural end.
//
// (The resume-path test above proves cancellation propagates via the real
// HTTP client's ctx-awareness; this test proves the CHECKPOINT itself fires
// even when the downstream call is ctx-blind — the gap the Explore agent
// flagged: without the checkpoint, a ctx-blind tool/MCP call would let the
// loop continue.)
func TestGenerateAbortsWhenContextAlreadyCancelled(t *testing.T) {
	// Stub ignores the request ctx entirely and returns a final response.
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return deepSeekHTTPResponse(deepSeekTextResponse("would-have-succeeded")), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	profile, ok := rt.agents.Get("xira-assistant")
	if !ok {
		t.Fatal("default agent not found")
	}
	instruction, _, err := rt.instructionTextForRun(profile, channel.NewInboundContext("test", "user-1", nil))
	if err != nil {
		t.Fatal(err)
	}

	// Cancel BEFORE calling generate. The stub ignores ctx, so the iterator
	// yields a final event; the deadline checkpoint then sees ctx.Err() != nil
	// and returns it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err = rt.generate(ctx, profile, instruction, TurnRequest{
		AgentID: profile.ID,
		Message: "x",
		Context: channel.NewInboundContext("test", "user-1", nil),
	}, func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {})
	if err == nil {
		t.Fatal("generate returned nil error with a cancelled ctx — the deadline checkpoint did NOT fire (loop ran to natural end)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("generate error = %v, want errors.Is context.Canceled (the deadline checkpoint returns ctx.Err())", err)
	}
}

// TestResumeNormalCompletionUnaffectedByDeadline is the regression guard: a
// resume that finishes quickly (well within the 120s policy deadline) must
// complete normally — the deadline must NOT误伤 normal resumes.
func TestResumeNormalCompletionUnaffectedByDeadline(t *testing.T) {
	// Fast stub: returns immediately.
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return deepSeekHTTPResponse(deepSeekTextResponse("resumed fine")), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	if err := rt.runs.SaveRun(TurnResponse{
		RunID:   "child-normal-1",
		AgentID: "xira-assistant",
		Status:  StatusWaitingHuman,
		Message: "task",
	}); err != nil {
		t.Fatal(err)
	}
	hr, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		WorkspaceID:  rt.workspace,
		WorkspaceKey: rt.WorkspaceKey(),
		RunID:        "child-normal-1",
		AgentID:      "xira-assistant",
		SessionID:    "session-normal-1",
		Kind:         humanrequest.RequestFreeform,
		Question:     "ok?",
		Source:       "agent_request",
	})
	if err != nil {
		t.Fatal(err)
	}
	hr.Response = &humanrequest.HumanResponse{
		RequestID: hr.ID,
		Kind:      humanrequest.ResponseAnswer,
		Actor:     "parent_agent",
		Message:   "yes",
	}

	// No short parent deadline — resume uses its internal 120s budget; the fast
	// stub finishes in milliseconds.
	if err := rt.resumeDirectHumanRequest(context.Background(), hr); err != nil {
		t.Fatalf("resumeDirectHumanRequest normal completion errored: %v (deadline must not误伤 fast resumes)", err)
	}
	run, err := rt.RunStore().Load("child-normal-1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" {
		t.Errorf("run Status = %q, want 'completed' (normal resume must finish, not be cut by the 120s deadline)", run.Status)
	}
}

func TestResumeDirectHumanRequestRestoresPersistedExecutionPolicy(t *testing.T) {
	var sawResume bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		sawResume = true
		if len(req.Tools) != 0 {
			t.Fatalf("resume exposed tools despite persisted explicit empty policy: %+v", req.Tools)
		}
		return deepSeekHTTPResponse(deepSeekTextResponse("resumed without tools")), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	if err := rt.runs.SaveRun(TurnResponse{
		RunID:   "child-policy-1",
		AgentID: "xira-assistant",
		Status:  StatusWaitingHuman,
		Message: "continue without tools",
		ExecutionPolicy: ExecutionPolicySnapshot{
			AllowedToolsSet: true,
			AllowedTools:    []string{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	hr, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		WorkspaceID:  rt.workspace,
		WorkspaceKey: rt.WorkspaceKey(),
		RunID:        "child-policy-1",
		AgentID:      "xira-assistant",
		SessionID:    "session-policy-1",
		Kind:         humanrequest.RequestFreeform,
		Question:     "continue?",
		Source:       "agent_request",
	})
	if err != nil {
		t.Fatal(err)
	}
	hr.Response = &humanrequest.HumanResponse{
		RequestID: hr.ID,
		Kind:      humanrequest.ResponseAnswer,
		Actor:     "parent_agent",
		Message:   "yes",
	}

	if err := rt.resumeDirectHumanRequest(context.Background(), hr); err != nil {
		t.Fatalf("resumeDirectHumanRequest: %v", err)
	}
	if !sawResume {
		t.Fatal("fake model did not receive resume request")
	}
}
