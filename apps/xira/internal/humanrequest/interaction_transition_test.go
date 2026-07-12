package humanrequest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStoreRecordDeliveryTracksRetryAndReceipt(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:               "hrq_delivery_transition",
		WorkspaceID:      "workspace",
		WorkspaceKey:     "ws_delivery_transition",
		RunID:            "run-delivery",
		AgentID:          "agent-delivery",
		SessionID:        "session-delivery",
		Kind:             RequestApproval,
		Question:         "Deliver this?",
		DeliveryRequired: true,
	})
	firstAt := time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC)
	failed, err := store.RecordDelivery(context.Background(), req.WorkspaceKey, req.ID, DeliveryAttempt{
		Error:       "temporary network failure",
		AttemptedAt: firstAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Delivery.Status != DeliveryFailed || failed.Delivery.Attempts != 1 || failed.Delivery.LastError == "" || failed.Delivery.LastAttempt == nil || !failed.Delivery.LastAttempt.Equal(firstAt) {
		t.Fatalf("failed delivery = %+v", failed.Delivery)
	}

	secondAt := firstAt.Add(time.Minute)
	sent, err := store.RecordDelivery(context.Background(), req.WorkspaceKey, req.ID, DeliveryAttempt{
		MessageID:   "om_delivery_1",
		AttemptedAt: secondAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent.Delivery.Status != DeliverySent || sent.Delivery.MessageID != "om_delivery_1" || sent.Delivery.Attempts != 2 || sent.Delivery.LastError != "" || sent.Delivery.DeliveredAt == nil || !sent.Delivery.DeliveredAt.Equal(secondAt) {
		t.Fatalf("sent delivery = %+v", sent.Delivery)
	}
	if _, err := store.RecordDelivery(context.Background(), req.WorkspaceKey, req.ID, DeliveryAttempt{Error: "late failure"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("delivery after sent error = %v, want ErrConflict", err)
	}
}

func TestStoreRecordDeliveryRejectsInvalidTransitions(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:           "hrq_delivery_none",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_delivery_none",
		RunID:        "run-delivery-none",
		AgentID:      "agent-delivery-none",
		SessionID:    "session-delivery-none",
		Kind:         RequestApproval,
		Question:     "No delivery tracking?",
	})
	if _, err := store.RecordDelivery(context.Background(), req.WorkspaceKey, req.ID, DeliveryAttempt{MessageID: "om_x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("untracked delivery error = %v, want ErrConflict", err)
	}
	tracked := mustCreateHumanRequest(t, store, CreateRequest{
		ID:               "hrq_delivery_invalid_attempt",
		WorkspaceID:      "workspace",
		WorkspaceKey:     "ws_delivery_none",
		RunID:            "run-delivery-invalid",
		AgentID:          "agent-delivery-invalid",
		SessionID:        "session-delivery-invalid",
		Kind:             RequestApproval,
		Question:         "Invalid attempt?",
		DeliveryRequired: true,
	})
	if _, err := store.RecordDelivery(context.Background(), tracked.WorkspaceKey, tracked.ID, DeliveryAttempt{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty delivery attempt error = %v, want ErrValidation", err)
	}
	if _, err := store.RecordDelivery(context.Background(), tracked.WorkspaceKey, tracked.ID, DeliveryAttempt{MessageID: "om_x", Error: "both"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("ambiguous delivery attempt error = %v, want ErrValidation", err)
	}
}

func TestStoreResumeLifecycleSupportsFailureRetryAndCompletion(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := resolvedTrackedRequest(t, store, "ws_resume_lifecycle", "hrq_resume_lifecycle")
	firstAt := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	claimed, ok, err := store.ClaimResume(context.Background(), req.WorkspaceKey, req.ID, firstAt)
	if err != nil || !ok {
		t.Fatalf("ClaimResume() = (%+v, %v, %v), want claimed", claimed, ok, err)
	}
	if claimed.Resume.Status != ResumeRunning || claimed.Resume.Attempts != 1 || claimed.Resume.StartedAt == nil || !claimed.Resume.StartedAt.Equal(firstAt) {
		t.Fatalf("claimed resume = %+v", claimed.Resume)
	}
	if _, ok, err := store.ClaimResume(context.Background(), req.WorkspaceKey, req.ID, firstAt); err != nil || ok {
		t.Fatalf("second ClaimResume() = (ok=%v, err=%v), want no-op", ok, err)
	}

	failedAt := firstAt.Add(time.Minute)
	failed, err := store.FailResume(context.Background(), req.WorkspaceKey, req.ID, "model unavailable", failedAt)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Resume.Status != ResumeFailed || failed.Resume.LastError != "model unavailable" || failed.Resume.CompletedAt != nil {
		t.Fatalf("failed resume = %+v", failed.Resume)
	}

	retryAt := failedAt.Add(time.Minute)
	retried, ok, err := store.ClaimResume(context.Background(), req.WorkspaceKey, req.ID, retryAt)
	if err != nil || !ok || retried.Resume.Attempts != 2 || retried.Resume.Status != ResumeRunning {
		t.Fatalf("retry claim = (%+v, %v, %v)", retried, ok, err)
	}
	completedAt := retryAt.Add(time.Minute)
	completed, err := store.CompleteResume(context.Background(), req.WorkspaceKey, req.ID, completedAt)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Resume.Status != ResumeCompleted || completed.Resume.LastError != "" || completed.Resume.CompletedAt == nil || !completed.Resume.CompletedAt.Equal(completedAt) {
		t.Fatalf("completed resume = %+v", completed.Resume)
	}
	if _, err := store.FailResume(context.Background(), req.WorkspaceKey, req.ID, "late", completedAt); !errors.Is(err, ErrConflict) {
		t.Fatalf("fail completed resume error = %v, want ErrConflict", err)
	}
}

func TestStoreClaimResumeIsConcurrentSingleWinner(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := resolvedTrackedRequest(t, store, "ws_resume_concurrent", "hrq_resume_concurrent")
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := store.ClaimResume(context.Background(), req.WorkspaceKey, req.ID, time.Now())
			if err != nil {
				t.Errorf("ClaimResume() error = %v", err)
				return
			}
			if ok {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("resume claim winners = %d, want 1", winners.Load())
	}
}

func TestStoreRecoversInterruptedAndListsOnlyResumableRequests(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	workspaceKey := "ws_resume_reconcile"
	pending := resolvedTrackedRequest(t, store, workspaceKey, "hrq_resume_pending")
	running := resolvedTrackedRequest(t, store, workspaceKey, "hrq_resume_running")
	if _, ok, err := store.ClaimResume(context.Background(), workspaceKey, running.ID, time.Now()); err != nil || !ok {
		t.Fatalf("claim running: ok=%v err=%v", ok, err)
	}
	completed := resolvedTrackedRequest(t, store, workspaceKey, "hrq_resume_completed")
	if _, ok, err := store.ClaimResume(context.Background(), workspaceKey, completed.ID, time.Now()); err != nil || !ok {
		t.Fatalf("claim completed seed: ok=%v err=%v", ok, err)
	}
	if _, err := store.CompleteResume(context.Background(), workspaceKey, completed.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_waiting_response", WorkspaceID: "workspace", WorkspaceKey: workspaceKey,
		RunID: "run-waiting", AgentID: "agent", SessionID: "session", Kind: RequestApproval, Question: "Waiting?",
	})
	legacy := HumanRequest{
		ID: "hrq_legacy_resolved", WorkspaceID: "workspace", WorkspaceKey: workspaceKey,
		RunID: "run-legacy", AgentID: "agent", SessionID: "session", Kind: RequestApproval,
		Status: StatusResolved, Question: "Legacy?", CreatedAt: time.Now(),
	}
	if err := store.writeRequest(&legacy); err != nil {
		t.Fatal(err)
	}

	recoveredAt := time.Date(2026, 7, 13, 13, 0, 0, 0, time.UTC)
	count, err := store.RecoverInterruptedResumes(context.Background(), workspaceKey, recoveredAt)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recovered count = %d, want 1", count)
	}
	resumable, err := store.ListResumable(context.Background(), workspaceKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumable) != 2 {
		t.Fatalf("resumable = %+v, want pending + recovered running", resumable)
	}
	ids := map[string]bool{}
	for _, req := range resumable {
		ids[req.ID] = true
	}
	if !ids[pending.ID] || !ids[running.ID] || ids[completed.ID] || ids[legacy.ID] {
		t.Fatalf("resumable ids = %+v", ids)
	}
	recovered, err := store.Get(context.Background(), workspaceKey, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Resume.Status != ResumeFailed || recovered.Resume.LastError == "" {
		t.Fatalf("recovered running resume = %+v", recovered.Resume)
	}
}

func TestRecoverInterruptedRequestsContinuesAfterSinglePersistFailure(t *testing.T) {
	persistFailure := errors.New("first request cannot be persisted")
	recoveredAt := time.Date(2026, 7, 13, 13, 30, 0, 0, time.UTC)
	requests := []HumanRequest{
		{ID: "hrq_broken", Resume: ResumeState{Status: ResumeRunning}},
		{ID: "hrq_healthy", Resume: ResumeState{Status: ResumeRunning}},
		{ID: "hrq_completed", Resume: ResumeState{Status: ResumeCompleted}},
	}
	persisted := []string{}

	count, err := recoverInterruptedRequests(context.Background(), requests, recoveredAt, func(req *HumanRequest) error {
		if req.ID == "hrq_broken" {
			return persistFailure
		}
		persisted = append(persisted, req.ID)
		return nil
	})

	if !errors.Is(err, persistFailure) {
		t.Fatalf("recovery error = %v, want persisted failure", err)
	}
	if count != 1 {
		t.Fatalf("recovered count = %d, want 1 successful persistence", count)
	}
	if len(persisted) != 1 || persisted[0] != "hrq_healthy" {
		t.Fatalf("persisted requests = %v, want healthy request after failed request", persisted)
	}
	if requests[1].Resume.Status != ResumeFailed || requests[1].Resume.LastError == "" {
		t.Fatalf("healthy request recovery = %+v", requests[1].Resume)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	writeCalled := false
	count, err = recoverInterruptedRequests(canceled, requests, recoveredAt, func(*HumanRequest) error {
		writeCalled = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || count != 0 || writeCalled {
		t.Fatalf("canceled recovery = count %d, error %v, writeCalled %v", count, err, writeCalled)
	}
}

func resolvedTrackedRequest(t *testing.T, store *Store, workspaceKey, requestID string) *HumanRequest {
	t.Helper()
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID: requestID, WorkspaceID: "workspace", WorkspaceKey: workspaceKey,
		RunID: "run-" + requestID, AgentID: "agent", SessionID: "session",
		Kind: RequestApproval, Question: "Approve?",
	})
	resolved, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey:   workspaceKey,
		RequestID:      req.ID,
		Kind:           ResponseApprove,
		Actor:          "tester",
		Message:        "approved",
		IdempotencyKey: "resolve-" + requestID,
	})
	if err != nil {
		t.Fatalf("Resolve(%s): %v", requestID, err)
	}
	return resolved
}

func TestInteractionTransitionTablesFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC)
	if err := applyDeliveryAttempt(nil, DeliveryAttempt{MessageID: "om_x"}, now); !errors.Is(err, ErrValidation) {
		t.Fatalf("nil delivery request error = %v", err)
	}
	if err := applyDeliveryAttempt(&HumanRequest{Delivery: DeliveryState{Status: DeliverySent}}, DeliveryAttempt{MessageID: "om_x"}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal delivery error = %v", err)
	}

	if _, err := applyResumeClaim(nil, now); !errors.Is(err, ErrValidation) {
		t.Fatalf("nil resume claim error = %v", err)
	}
	if _, err := applyResumeClaim(&HumanRequest{ID: "pending", Status: StatusPending}, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("unresolved resume claim error = %v", err)
	}
	legacyResolved := &HumanRequest{ID: "legacy", Status: StatusResolved, Response: &HumanResponse{ID: "hrs_legacy"}}
	if _, err := applyResumeClaim(legacyResolved, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("legacy resume claim error = %v", err)
	}
	for _, status := range []ResumeStatus{ResumeRunning, ResumeCompleted} {
		req := &HumanRequest{ID: "noop", Status: StatusResolved, Response: &HumanResponse{ID: "hrs_noop"}, Resume: ResumeState{Status: status}}
		if claimed, err := applyResumeClaim(req, now); err != nil || claimed {
			t.Fatalf("claim %s = (%v, %v), want no-op", status, claimed, err)
		}
	}

	if _, err := applyResumeFinish(nil, "", now); !errors.Is(err, ErrValidation) {
		t.Fatalf("nil resume finish error = %v", err)
	}
	completed := &HumanRequest{ID: "completed", Resume: ResumeState{Status: ResumeCompleted}}
	if changed, err := applyResumeFinish(completed, "", now); err != nil || changed {
		t.Fatalf("complete completed = (%v, %v), want idempotent no-op", changed, err)
	}
	if _, err := applyResumeFinish(&HumanRequest{ID: "pending", Resume: ResumeState{Status: ResumePending}}, "", now); !errors.Is(err, ErrConflict) {
		t.Fatalf("finish pending error = %v", err)
	}

	wants := map[ResumeStatus]bool{
		ResumeWaitingResponse: false,
		ResumePending:         true,
		ResumeRunning:         false,
		ResumeCompleted:       false,
		ResumeFailed:          true,
		"":                    false,
	}
	for status, want := range wants {
		if got := resumeNeedsRetry(status); got != want {
			t.Fatalf("resumeNeedsRetry(%q) = %v, want %v", status, got, want)
		}
	}
	if recoverInterruptedResume(nil, now) {
		t.Fatal("nil interrupted resume recovered")
	}
	if recoverInterruptedResume(&HumanRequest{Resume: ResumeState{Status: ResumePending}}, now) {
		t.Fatal("pending resume recovered as interrupted")
	}
	running := &HumanRequest{Resume: ResumeState{Status: ResumeRunning}}
	if !recoverInterruptedResume(running, now) || running.Resume.Status != ResumeFailed {
		t.Fatalf("running resume recovery = %+v", running.Resume)
	}
}
