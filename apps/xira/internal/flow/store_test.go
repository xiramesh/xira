package flow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(t.TempDir())
}

func TestStoreDefaultRootDoesNotDoubleFlowRuns(t *testing.T) {
	wd := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})

	store := NewStore("")
	run, err := store.CreateRun(context.Background(), CreateRunRequest{
		FlowID:        "devrun",
		FlowVersion:   "0.1.0",
		EntrypointID:  "ad_hoc",
		CurrentStepID: "intake",
		Input:         map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	want := filepath.Join(".xira", "flow-runs", run.ID)
	if got := store.RunDir(run.ID); got != want {
		t.Fatalf("RunDir() = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(wd, want, "flow_run.yaml")); err != nil {
		t.Fatalf("default flow run file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wd, ".xira", "flow-runs", "flow-runs", run.ID)); !os.IsNotExist(err) {
		t.Fatalf("unexpected doubled flow-runs directory err=%v", err)
	}
}

func TestStoreCreateAndGetFlowRun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	created, err := store.CreateRun(ctx, CreateRunRequest{
		FlowID:        "devrun",
		FlowVersion:   "0.1.0",
		EntrypointID:  "ad_hoc",
		Input:         map[string]string{"repo": "/repo", "request": "fix bug"},
		CurrentStepID: "intake",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected non-empty flow_run_id")
	}
	if created.SchemaVersion != SchemaVersionRun {
		t.Errorf("schema_version = %q, want %q", created.SchemaVersion, SchemaVersionRun)
	}
	if created.Status != RunPending {
		t.Errorf("status = %q, want pending", created.Status)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Error("expected non-zero timestamps")
	}

	got, err := store.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("got id %q, want %q", got.ID, created.ID)
	}
	if got.Status != RunPending {
		t.Errorf("got status %q, want pending", got.Status)
	}
}

func TestStoreUpdateCurrentStep(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	run, err := store.CreateRun(ctx, CreateRunRequest{
		FlowID:        "devrun",
		FlowVersion:   "0.1.0",
		EntrypointID:  "ad_hoc",
		CurrentStepID: "intake",
		Input:         map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	updated, err := store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.Status = RunRunning
		r.CurrentStepID = "design"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	if updated.Status != RunRunning {
		t.Errorf("status = %q, want running", updated.Status)
	}
	if updated.CurrentStepID != "design" {
		t.Errorf("current_step_id = %q, want design", updated.CurrentStepID)
	}
	if !updated.UpdatedAt.After(run.UpdatedAt) && !updated.UpdatedAt.Equal(run.UpdatedAt) {
		t.Errorf("UpdatedAt did not advance")
	}

	// Persisted to disk.
	got, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after update: %v", err)
	}
	if got.Status != RunRunning {
		t.Errorf("persisted status = %q, want running", got.Status)
	}
	if got.CurrentStepID != "design" {
		t.Errorf("persisted current_step_id = %q, want design", got.CurrentStepID)
	}
}

func TestStoreRecordsOutputSlotsAndArtifacts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	run, err := store.CreateRun(ctx, CreateRunRequest{
		FlowID:        "devrun",
		FlowVersion:   "0.1.0",
		CurrentStepID: "intake",
		Input:         map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	started := time.Now().UTC()
	completed := started.Add(time.Second)
	_, err = store.UpdateRun(ctx, run.ID, func(r *Run) error {
		if r.Steps == nil {
			r.Steps = map[string]StepState{}
		}
		r.Steps["intake"] = StepState{
			Status:      StepCompleted,
			AgentRunID:  "20260613-100000-dev-intake",
			StartedAt:   &started,
			CompletedAt: &completed,
			Outputs: map[string]OutputRef{
				"task_spec": {Artifact: "artifacts/intake/task_spec.yaml"},
				"count":     {Value: 3},
			},
			Artifacts: []ArtifactRef{{Path: "artifacts/intake/task_spec.yaml"}},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	step, ok := got.Steps["intake"]
	if !ok {
		t.Fatalf("intake step missing")
	}
	if step.Status != StepCompleted {
		t.Errorf("status = %q, want completed", step.Status)
	}
	if step.Outputs["task_spec"].Artifact != "artifacts/intake/task_spec.yaml" {
		t.Errorf("task_spec artifact = %q", step.Outputs["task_spec"].Artifact)
	}
	if step.Outputs["count"].Value != 3 {
		t.Errorf("count value = %v, want 3", step.Outputs["count"].Value)
	}
	if len(step.Artifacts) != 1 || step.Artifacts[0].Path != "artifacts/intake/task_spec.yaml" {
		t.Errorf("artifacts = %+v", step.Artifacts)
	}
}

func TestStoreRejectsPathTraversalRunID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	badIDs := []string{"../escape", "..", "fr_/etc", "fr_/\\..", "fr_..", "fr_/secret"}
	for _, id := range badIDs {
		t.Run(id, func(t *testing.T) {
			_, err := store.CreateRun(ctx, CreateRunRequest{
				FlowID:      "devrun",
				FlowVersion: "0.1.0",
				ID:          id,
			})
			if err == nil {
				t.Fatalf("expected error for run id %q", id)
			}
		})
	}
	// GetRun with traversal should not read outside the root.
	if _, err := store.GetRun(ctx, "../escape"); err == nil {
		t.Fatal("expected error for GetRun with traversal id")
	}
}

func TestStoreAppendEvents(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	run, err := store.CreateRun(ctx, CreateRunRequest{
		FlowID:      "devrun",
		FlowVersion: "0.1.0",
		Input:       map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	now := time.Now().UTC()
	events := []Event{
		{ID: "e1", Time: now, Kind: "flow.run.started", FlowRunID: run.ID, Payload: map[string]any{"k": "v"}},
		{ID: "e2", Time: now, Kind: "flow.step.started", FlowRunID: run.ID, StepID: "intake"},
	}
	if err := store.AppendEvents(ctx, run.ID, events); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	got, err := store.ReadEvents(ctx, run.ID)
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Kind != "flow.run.started" || got[1].Kind != "flow.step.started" {
		t.Errorf("event kinds = %q, %q", got[0].Kind, got[1].Kind)
	}
	// Append again — should accumulate, not overwrite.
	if err := store.AppendEvents(ctx, run.ID, []Event{{ID: "e3", Time: now, Kind: "flow.step.completed", FlowRunID: run.ID}}); err != nil {
		t.Fatalf("AppendEvents second: %v", err)
	}
	got, err = store.ReadEvents(ctx, run.ID)
	if err != nil {
		t.Fatalf("ReadEvents second: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events after append, want 3", len(got))
	}

	// events_ref points to the jsonl file under the run dir.
	runDir := store.RunDir(run.ID)
	eventsPath := filepath.Join(runDir, "events.jsonl")
	if _, err := os.Stat(eventsPath); err != nil {
		t.Fatalf("events file missing: %v", err)
	}
}

func TestStoreCreateRunIdempotentForSameID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	first, err := store.CreateRun(ctx, CreateRunRequest{
		FlowID:      "devrun",
		FlowVersion: "0.1.0",
		ID:          "fr_idempotent_001",
		Input:       map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun first: %v", err)
	}
	second, err := store.CreateRun(ctx, CreateRunRequest{
		FlowID:      "devrun",
		FlowVersion: "0.1.0",
		ID:          "fr_idempotent_001",
		Input:       map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun second: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("ids differ: %q vs %q", first.ID, second.ID)
	}
	if !first.CreatedAt.Equal(second.CreatedAt) {
		t.Errorf("CreatedAt differs: %v vs %v", first.CreatedAt, second.CreatedAt)
	}
	// Different flow id with same run id is a conflict.
	_, err = store.CreateRun(ctx, CreateRunRequest{
		FlowID:      "other",
		FlowVersion: "0.1.0",
		ID:          "fr_idempotent_001",
	})
	if err == nil {
		t.Fatal("expected conflict for same run id with different flow id")
	}
}

func TestStoreConcurrentUpdateDoesNotCorruptRun(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	run, err := store.CreateRun(ctx, CreateRunRequest{
		FlowID:        "devrun",
		FlowVersion:   "0.1.0",
		CurrentStepID: "intake",
		Input:         map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _ = store.UpdateRun(ctx, run.ID, func(r *Run) error {
				if r.Steps == nil {
					r.Steps = map[string]StepState{}
				}
				key := "step_" + itoa(i)
				r.Steps[key] = StepState{Status: StepCompleted}
				return nil
			})
		}(i)
	}
	wg.Wait()
	got, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(got.Steps) != n {
		t.Errorf("got %d steps, want %d", len(got.Steps), n)
	}
}

func TestStoreRejectsCompletedStepOverwriteWithoutRetry(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	run, err := store.CreateRun(ctx, CreateRunRequest{
		FlowID:        "devrun",
		FlowVersion:   "0.1.0",
		CurrentStepID: "intake",
		Input:         map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	started := time.Now().UTC()
	completed := started.Add(time.Second)
	_, err = store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.Steps = map[string]StepState{
			"intake": {Status: StepCompleted, CompletedAt: &completed, Outputs: map[string]OutputRef{"task_spec": {Artifact: "a"}}},
		}
		return nil
	})
	if err != nil {
		t.Fatalf("initial UpdateRun: %v", err)
	}

	// Default guard rejects re-execution of a completed step.
	_, err = store.UpdateRun(ctx, run.ID, func(r *Run) error {
		return MarkStepRunning(r, "intake")
	})
	if err == nil {
		t.Fatal("expected error marking completed step running without retry")
	}
	if !errors.Is(err, ErrStepAlreadyCompleted) {
		t.Errorf("expected ErrStepAlreadyCompleted, got %v", err)
	}

	// Explicit retry allows it.
	_, err = store.UpdateRun(ctx, run.ID, func(r *Run) error {
		return MarkStepRunningWithRetry(r, "intake")
	})
	if err != nil {
		t.Fatalf("MarkStepRunningWithRetry: %v", err)
	}
	got, _ := store.GetRun(ctx, run.ID)
	if got.Steps["intake"].Status != StepRunning {
		t.Errorf("status = %q, want running after retry", got.Steps["intake"].Status)
	}
	if got.Steps["intake"].Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Steps["intake"].Attempts)
	}
	// Previous completed output cleared on retry.
	if got.Steps["intake"].CompletedAt != nil {
		t.Errorf("completed_at should be cleared on retry")
	}
}

func TestStoreRoundTripsThroughRestart(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	ctx := context.Background()
	run, err := store.CreateRun(ctx, CreateRunRequest{
		FlowID:        "devrun",
		FlowVersion:   "0.1.0",
		CurrentStepID: "intake",
		Input:         map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	_, _ = store.UpdateRun(ctx, run.ID, func(r *Run) error {
		r.Status = RunWaitingHuman
		r.Steps = map[string]StepState{"approve_design": {Status: StepWaitingHuman, HumanRequestIDs: []string{"hrq_1"}}}
		r.PendingHumanRequests = []string{"hrq_1"}
		return nil
	})

	// Simulate process restart by constructing a fresh store on same root.
	reopened := NewStore(root)
	got, err := reopened.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after reopen: %v", err)
	}
	if got.Status != RunWaitingHuman {
		t.Errorf("status = %q, want waiting_human", got.Status)
	}
	if len(got.Steps["approve_design"].HumanRequestIDs) != 1 || got.Steps["approve_design"].HumanRequestIDs[0] != "hrq_1" {
		t.Errorf("human_request_ids = %+v", got.Steps["approve_design"].HumanRequestIDs)
	}
	if len(got.PendingHumanRequests) != 1 || got.PendingHumanRequests[0] != "hrq_1" {
		t.Errorf("pending_human_requests = %+v", got.PendingHumanRequests)
	}
}

func TestStoreRejectsArtifactPathTraversal(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	run, err := store.CreateRun(ctx, CreateRunRequest{
		FlowID:      "devrun",
		FlowVersion: "0.1.0",
		Input:       map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	bad := []string{"../../secret", "/etc/passwd", "..\\windows"}
	for _, p := range bad {
		_, err := store.UpdateRun(ctx, run.ID, func(r *Run) error {
			if r.Steps == nil {
				r.Steps = map[string]StepState{}
			}
			r.Steps["x"] = StepState{Artifacts: []ArtifactRef{{Path: p}}}
			return nil
		})
		if err == nil {
			t.Errorf("expected error for artifact path %q", p)
		}
	}
}

// itoa is a tiny local int->string to avoid pulling strconv into a test helper.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
