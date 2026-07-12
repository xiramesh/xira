package runtime

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// startup_pending_scan_test.go: tests the startup scan for pending HumanRequests
// (#72 item 3 minimal). On restart, NewService logs how many HITL requests are
// still awaiting resolution — operational visibility only (no notify/timeout
// cleanup, those are bigger decisions). Best-effort: scan failure doesn't block
// startup.

// TestNewServiceLogsPendingHumanRequestsAtStartup verifies that NewService, when
// a state dir already contains pending HumanRequests (simulating a restart),
// logs the pending count so the operator can see unresolved HITL.
func TestNewServiceLogsPendingHumanRequestsAtStartup(t *testing.T) {
	stateDir := t.TempDir()

	// First boot: create a Service and seed a pending HumanRequest (persisted).
	rt1 := newTestService(t, Config{StateDir: stateDir})
	if _, err := rt1.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		ID:           "hrq_startup_1",
		RunID:        "run-1",
		AgentID:      "xira-assistant",
		SessionID:    "session-1",
		WorkspaceKey: rt1.WorkspaceKey(),
		Kind:         humanrequest.RequestFreeform,
		Question:     "confirm delete?",
	}); err != nil {
		t.Fatalf("seed CreateHumanRequest: %v", err)
	}
	rt1.Close()

	// Capture logs during the "restart" (second NewService on same stateDir).
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prevLogger)

	rt2 := newTestService(t, Config{StateDir: stateDir})
	defer rt2.Close()

	logs := logBuf.String()
	if !strings.Contains(logs, "pending at startup") {
		t.Errorf("startup log missing pending scan; logs:\n%s", logs)
	}
	if !strings.Contains(logs, "count") {
		t.Errorf("startup log missing count; logs:\n%s", logs)
	}
}

// TestNewServiceNoPendingLogWhenEmpty verifies that when there are no pending
// HumanRequests, NewService does NOT log a pending-scan message (no noise on a
// clean start).
func TestNewServiceNoPendingLogWhenEmpty(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prevLogger)

	rt := newTestService(t, Config{StateDir: t.TempDir()})
	defer rt.Close()

	logs := logBuf.String()
	if strings.Contains(logs, "pending at startup") {
		t.Errorf("clean startup should not log pending scan; logs:\n%s", logs)
	}
}

func TestLogHumanRequestResumeRecoveryReportsPartialSuccessBeforeFailure(t *testing.T) {
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prevLogger)

	logHumanRequestResumeRecovery(2, errors.New("one request could not be persisted"))

	logs := logBuf.String()
	successAt := strings.Index(logs, "interrupted human request resumes recovered for retry")
	failureAt := strings.Index(logs, "startup human request resume recovery partially failed")
	if successAt < 0 || failureAt < 0 || successAt >= failureAt {
		t.Fatalf("partial recovery logs must report successes before failure; logs:\n%s", logs)
	}
	if !strings.Contains(logs, "count=2") || !strings.Contains(logs, "recovered_count=2") {
		t.Fatalf("partial recovery logs missing recovered count; logs:\n%s", logs)
	}

	logBuf.Reset()
	logHumanRequestResumeRecovery(0, nil)
	if logBuf.Len() != 0 {
		t.Fatalf("empty successful recovery should not log; logs:\n%s", logBuf.String())
	}

	logHumanRequestResumeRecovery(0, errors.New("store unavailable"))
	logs = logBuf.String()
	if strings.Contains(logs, "recovered for retry") || !strings.Contains(logs, "recovery failed") {
		t.Fatalf("total recovery failure logs = %q", logs)
	}
}
