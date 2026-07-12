package humanrequest

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RecordDelivery persists one platform delivery attempt. Sent is terminal;
// pending/failed may retry. Exactly one of MessageID or Error is required.
func (s *Store) RecordDelivery(ctx context.Context, workspaceKey, requestID string, attempt DeliveryAttempt) (*HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceKey(workspaceKey); err != nil {
		return nil, err
	}
	if err := validatePathID(requestID, "request id"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, err := s.loadRequest(workspaceKey, requestID)
	if err != nil {
		return nil, err
	}
	now := attempt.AttemptedAt
	if now.IsZero() {
		now = time.Now()
	}
	if err := applyDeliveryAttempt(req, attempt, now); err != nil {
		return nil, err
	}
	if err := s.writeRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// applyDeliveryAttempt is the sealed delivery transition table.
// coverage: contract (100% required)
func applyDeliveryAttempt(req *HumanRequest, attempt DeliveryAttempt, now time.Time) error {
	messageID := strings.TrimSpace(attempt.MessageID)
	attemptError := strings.TrimSpace(attempt.Error)
	if (messageID == "") == (attemptError == "") {
		return fmt.Errorf("%w: delivery attempt requires exactly one of message id or error", ErrValidation)
	}
	if req == nil {
		return fmt.Errorf("%w: human request is required", ErrValidation)
	}
	if req.Delivery.Status != DeliveryPending && req.Delivery.Status != DeliveryFailed {
		return fmt.Errorf("%w: delivery for human request %s is %s", ErrConflict, req.ID, req.Delivery.Status)
	}
	req.Delivery.Attempts++
	req.Delivery.LastAttempt = copyTime(&now)
	if attemptError != "" {
		req.Delivery.Status = DeliveryFailed
		req.Delivery.LastError = attemptError
		req.Audit = append(req.Audit, AuditRecord{Time: now, Action: "human_request.delivery_failed", Message: attemptError})
		return nil
	}
	req.Delivery.Status = DeliverySent
	req.Delivery.MessageID = messageID
	req.Delivery.LastError = ""
	req.Delivery.DeliveredAt = copyTime(&now)
	req.Audit = append(req.Audit, AuditRecord{Time: now, Action: "human_request.delivery_sent"})
	return nil
}

// ClaimResume atomically selects one process/goroutine to resume a resolved
// request. Running/completed means another claimant already owns or finished
// the work and is returned as a non-error no-op.
func (s *Store) ClaimResume(ctx context.Context, workspaceKey, requestID string, startedAt time.Time) (*HumanRequest, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := validateWorkspaceKey(workspaceKey); err != nil {
		return nil, false, err
	}
	if err := validatePathID(requestID, "request id"); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, err := s.loadRequest(workspaceKey, requestID)
	if err != nil {
		return nil, false, err
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	claimed, err := applyResumeClaim(req, startedAt)
	if err != nil || !claimed {
		return req, claimed, err
	}
	if err := s.writeRequest(req); err != nil {
		return nil, false, err
	}
	return req, true, nil
}

// applyResumeClaim is the sealed resume-claim transition table.
// coverage: contract (100% required)
func applyResumeClaim(req *HumanRequest, startedAt time.Time) (bool, error) {
	if req == nil {
		return false, fmt.Errorf("%w: human request is required", ErrValidation)
	}
	if req.Status != StatusResolved || req.Response == nil {
		return false, fmt.Errorf("%w: human request %s has no resolved response", ErrConflict, req.ID)
	}
	switch req.Resume.Status {
	case ResumePending, ResumeFailed:
		// claim below
	case ResumeRunning, ResumeCompleted:
		return false, nil
	default:
		return false, fmt.Errorf("%w: human request %s resume is %s", ErrConflict, req.ID, req.Resume.Status)
	}
	req.Resume.Status = ResumeRunning
	req.Resume.Attempts++
	req.Resume.StartedAt = copyTime(&startedAt)
	req.Resume.CompletedAt = nil
	req.Resume.LastError = ""
	req.Audit = append(req.Audit, AuditRecord{Time: startedAt, Action: "human_request.resume_started"})
	return true, nil
}

// CompleteResume marks a claimed resume terminally successful.
func (s *Store) CompleteResume(ctx context.Context, workspaceKey, requestID string, completedAt time.Time) (*HumanRequest, error) {
	return s.finishResume(ctx, workspaceKey, requestID, "", completedAt)
}

// FailResume persists a retryable resume failure.
func (s *Store) FailResume(ctx context.Context, workspaceKey, requestID, reason string, failedAt time.Time) (*HumanRequest, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: resume failure reason is required", ErrValidation)
	}
	return s.finishResume(ctx, workspaceKey, requestID, reason, failedAt)
}

func (s *Store) finishResume(ctx context.Context, workspaceKey, requestID, failure string, finishedAt time.Time) (*HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceKey(workspaceKey); err != nil {
		return nil, err
	}
	if err := validatePathID(requestID, "request id"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, err := s.loadRequest(workspaceKey, requestID)
	if err != nil {
		return nil, err
	}
	if finishedAt.IsZero() {
		finishedAt = time.Now()
	}
	changed, err := applyResumeFinish(req, failure, finishedAt)
	if err != nil || !changed {
		return req, err
	}
	if err := s.writeRequest(req); err != nil {
		return nil, err
	}
	return req, nil
}

// applyResumeFinish is the sealed resume completion/failure transition table.
// coverage: contract (100% required)
func applyResumeFinish(req *HumanRequest, failure string, finishedAt time.Time) (bool, error) {
	if req == nil {
		return false, fmt.Errorf("%w: human request is required", ErrValidation)
	}
	if failure == "" && req.Resume.Status == ResumeCompleted {
		return false, nil
	}
	if req.Resume.Status != ResumeRunning {
		return false, fmt.Errorf("%w: human request %s resume is %s", ErrConflict, req.ID, req.Resume.Status)
	}
	if failure != "" {
		req.Resume.Status = ResumeFailed
		req.Resume.LastError = failure
		req.Resume.CompletedAt = nil
		req.Audit = append(req.Audit, AuditRecord{Time: finishedAt, Action: "human_request.resume_failed", Message: failure})
		return true, nil
	}
	req.Resume.Status = ResumeCompleted
	req.Resume.LastError = ""
	req.Resume.CompletedAt = copyTime(&finishedAt)
	req.Audit = append(req.Audit, AuditRecord{Time: finishedAt, Action: "human_request.resume_completed"})
	return true, nil
}

// ListResumable returns only new-contract resolved requests whose resume work
// is pending or retryable. Legacy records with empty resume state are excluded.
func (s *Store) ListResumable(ctx context.Context, workspaceKey string) ([]HumanRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateWorkspaceKey(workspaceKey); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved, err := s.listLocked(ListQuery{WorkspaceKey: workspaceKey, Status: StatusResolved})
	if err != nil {
		return nil, err
	}
	out := make([]HumanRequest, 0, len(resolved))
	for _, req := range resolved {
		if resumeNeedsRetry(req.Resume.Status) {
			out = append(out, req)
		}
	}
	return out, nil
}

// resumeNeedsRetry is the sealed reconciliation filter.
// coverage: contract (100% required)
func resumeNeedsRetry(status ResumeStatus) bool {
	return status == ResumePending || status == ResumeFailed
}

// RecoverInterruptedResumes converts stale running claims from a previous
// process into retryable failures. Call only during single-threaded startup.
func (s *Store) RecoverInterruptedResumes(ctx context.Context, workspaceKey string, recoveredAt time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := validateWorkspaceKey(workspaceKey); err != nil {
		return 0, err
	}
	if recoveredAt.IsZero() {
		recoveredAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved, err := s.listLocked(ListQuery{WorkspaceKey: workspaceKey, Status: StatusResolved})
	if err != nil {
		return 0, err
	}
	count := 0
	for i := range resolved {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		req := &resolved[i]
		if !recoverInterruptedResume(req, recoveredAt) {
			continue
		}
		if err := s.writeRequest(req); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// recoverInterruptedResume is the sealed startup recovery transition.
// coverage: contract (100% required)
func recoverInterruptedResume(req *HumanRequest, recoveredAt time.Time) bool {
	if req == nil || req.Resume.Status != ResumeRunning {
		return false
	}
	req.Resume.Status = ResumeFailed
	req.Resume.LastError = "resume interrupted before completion"
	req.Resume.CompletedAt = nil
	req.Audit = append(req.Audit, AuditRecord{Time: recoveredAt, Action: "human_request.resume_recovered", Message: req.Resume.LastError})
	return true
}
