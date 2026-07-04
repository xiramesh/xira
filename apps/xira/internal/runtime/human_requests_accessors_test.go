package runtime

import (
	"context"
	"testing"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// TestHumanRequestAccessorsNilSafe covers the nil-receiver guards on the
// human-request accessors (WorkspaceKey, Get/List/Resolve).
func TestHumanRequestAccessorsNilSafe(t *testing.T) {
	var nilSvc *Service
	if nilSvc.WorkspaceKey() != "" {
		t.Fatalf("nil WorkspaceKey should be empty")
	}
	ctx := context.Background()
	if _, err := nilSvc.GetHumanRequest(ctx, "r"); err == nil {
		t.Fatalf("nil GetHumanRequest should error")
	}
	if _, err := nilSvc.ListHumanRequests(ctx, humanrequest.StatusPending); err == nil {
		t.Fatalf("nil ListHumanRequests should error")
	}
	if _, err := nilSvc.ResolveHumanRequest(ctx, "r", humanrequest.ResolveRequest{}); err == nil {
		t.Fatalf("nil ResolveHumanRequest should error")
	}
}

// TestHumanRequestCreateGetList covers the happy path of Create/Get/List +
// WorkspaceKey + the workspace-id/key auto-fill branches.
func TestHumanRequestCreateGetList(t *testing.T) {
	svc := newTestService(t, Config{})
	ctx := context.Background()

	// WorkspaceKey is derived from the (empty-default) workspace.
	if wk := svc.WorkspaceKey(); wk == "" {
		t.Fatalf("WorkspaceKey should be non-empty")
	}

	// The human-request store validates RunID, and Create persists a reference
	// to the run dir, so initialize the run first.
	const runID = "run-hr-1"
	if err := svc.runs.InitRun(runID); err != nil {
		t.Fatalf("InitRun failed: %v", err)
	}

	created, err := svc.CreateHumanRequest(ctx, humanrequest.CreateRequest{
		RunID:       runID,
		AgentID:     "agent-1",
		SessionID:   "session-1",
		WorkspaceID: "ws-1",
		Question:    "confirm?",
		Kind:        humanrequest.RequestFreeform,
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("created request should have an id")
	}

	// Get round-trips.
	got, err := svc.GetHumanRequest(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.ID != created.ID || got.Question != "confirm?" {
		t.Fatalf("Get roundtrip mismatch: %+v", got)
	}

	// List by status: the pending request appears.
	list, err := svc.ListHumanRequests(ctx, humanrequest.StatusPending)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	found := false
	for _, r := range list {
		if r.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created request not in pending list (%d items)", len(list))
	}
}
