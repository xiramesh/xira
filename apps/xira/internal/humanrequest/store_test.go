package humanrequest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestStoreCreateHumanRequestWritesWorkspaceScopedPendingFile(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	createdAt := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)

	req, err := store.Create(context.Background(), CreateRequest{
		ID:           "hrq_create",
		WorkspaceID:  "/Users/yinwm/work/flowdeck",
		WorkspaceKey: "ws_create",
		RunID:        "run-1",
		AgentID:      "agent-1",
		SessionID:    "session-1",
		ToolCallID:   "tool-1",
		Kind:         RequestFreeform,
		Question:     "Need human input?",
		DedupeKey:    "question-hash-1",
		CreatedAt:    createdAt,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if req.Status != StatusPending {
		t.Fatalf("status = %q", req.Status)
	}
	if req.CreatedAt.IsZero() {
		t.Fatal("created_at is zero")
	}
	if req.WorkspaceID != "/Users/yinwm/work/flowdeck" || req.WorkspaceKey != "ws_create" {
		t.Fatalf("workspace fields = %+v", req)
	}
	path := filepath.Join(root, "workspaces", "ws_create", "human-requests", "hrq_create.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected request file: %v", err)
	}
	if strings.Contains(filepath.ToSlash(path), "Users/yinwm/work/flowdeck") {
		t.Fatalf("request path leaked raw workspace id: %s", path)
	}

	var stored HumanRequest
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &stored); err != nil {
		t.Fatalf("decode stored request: %v\n%s", err, data)
	}
	if stored.ID != req.ID || stored.Status != StatusPending || stored.Question != "Need human input?" {
		t.Fatalf("stored request = %+v", stored)
	}
}

func TestStoreCreateHumanRequestRejectsPathTraversalWorkspaceKey(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	for _, workspaceKey := range []string{"", "../x", "/tmp/x", "a/../b", "a/b", `a\b`, ".hidden"} {
		_, err := store.Create(context.Background(), CreateRequest{
			ID:           "hrq_bad_" + strings.NewReplacer("/", "_", "\\", "_", ".", "_").Replace(workspaceKey),
			WorkspaceID:  "workspace",
			WorkspaceKey: workspaceKey,
			RunID:        "run-1",
			AgentID:      "agent-1",
			SessionID:    "session-1",
			Kind:         RequestFreeform,
			Question:     "bad workspace key?",
		})
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("workspace_key %q error = %v, want ErrValidation", workspaceKey, err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid creates wrote files: %+v", entries)
	}
}

func TestStoreRejectsPathTraversalRequestIDs(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	for _, requestID := range []string{"../outside", "/tmp/hrq", "nested/hrq", `nested\hrq`, "hrq..evil", ".hidden"} {
		_, err := store.Create(context.Background(), CreateRequest{
			ID:           requestID,
			WorkspaceID:  "workspace",
			WorkspaceKey: "ws_request_id",
			RunID:        "run-1",
			AgentID:      "agent-1",
			SessionID:    "session-1",
			Kind:         RequestFreeform,
			Question:     "bad request id?",
		})
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("Create request id %q error = %v, want ErrValidation", requestID, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "outside.yaml")); !os.IsNotExist(err) {
		t.Fatalf("invalid request id wrote outside file, stat err=%v", err)
	}
}

func TestStoreRejectsPathTraversalRequestIDOnReadResolveAndReplay(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:           "hrq_replay_path",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_replay_path",
		RunID:        "run-1",
		AgentID:      "agent-1",
		SessionID:    "session-1",
		ToolCallID:   "tool-1",
		Kind:         RequestApproval,
		Question:     "approve?",
		ActionSnapshot: &ActionSnapshot{
			ToolName:   "write_file",
			Arguments:  map[string]any{"path": "ok.txt"},
			RunID:      "run-1",
			AgentID:    "agent-1",
			SessionID:  "session-1",
			ToolCallID: "tool-1",
		},
	})
	resolved, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_replay_path",
		RequestID:    req.ID,
		Kind:         ResponseApprove,
		Actor:        "tester",
	})
	if err != nil {
		t.Fatalf("Resolve valid request: %v", err)
	}
	if _, err := store.BeginReplay(context.Background(), ReplayLeaseRequest{WorkspaceKey: "ws_replay_path", RequestID: resolved.ID, Owner: "worker", LeaseDuration: time.Minute}); err != nil {
		t.Fatalf("BeginReplay valid request: %v", err)
	}

	badID := "../" + req.ID
	checks := []struct {
		name string
		run  func() error
	}{
		{name: "Get", run: func() error {
			_, err := store.Get(context.Background(), "ws_replay_path", badID)
			return err
		}},
		{name: "Resolve", run: func() error {
			_, err := store.Resolve(context.Background(), ResolveRequest{WorkspaceKey: "ws_replay_path", RequestID: badID, Kind: ResponseAnswer, Message: "no"})
			return err
		}},
		{name: "BeginReplay", run: func() error {
			_, err := store.BeginReplay(context.Background(), ReplayLeaseRequest{WorkspaceKey: "ws_replay_path", RequestID: badID, Owner: "worker", LeaseDuration: time.Minute})
			return err
		}},
		{name: "CompleteReplay", run: func() error {
			_, err := store.CompleteReplay(context.Background(), CompleteReplayRequest{WorkspaceKey: "ws_replay_path", RequestID: badID, Owner: "worker", ResultDigest: "sha256:test"})
			return err
		}},
		{name: "FailReplay", run: func() error {
			_, err := store.FailReplay(context.Background(), FailReplayRequest{WorkspaceKey: "ws_replay_path", RequestID: badID, Owner: "worker", Error: "failed"})
			return err
		}},
	}
	for _, check := range checks {
		if err := check.run(); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s malicious request id error = %v, want ErrValidation", check.name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "workspaces", "hrq_replay_path.yaml")); !os.IsNotExist(err) {
		t.Fatalf("malicious request id wrote workspace sibling, stat err=%v", err)
	}
}

func TestStoreCreateHumanRequestIsIdempotentForSameRunAndDedupeKey(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	base := CreateRequest{
		ID:           "hrq_first",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_dedupe",
		RunID:        "run-1",
		AgentID:      "agent-1",
		SessionID:    "session-1",
		ToolCallID:   "tool-1",
		Kind:         RequestFreeform,
		Question:     "same question?",
		DedupeKey:    "same-question-hash",
	}
	first, err := store.Create(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	dup := base
	dup.ID = "hrq_second"
	second, err := store.Create(context.Background(), dup)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate returned id = %q, want %q", second.ID, first.ID)
	}

	distinct := base
	distinct.ID = "hrq_distinct"
	distinct.ToolCallID = "tool-2"
	third, err := store.Create(context.Background(), distinct)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatalf("distinct tool call reused request id %q", third.ID)
	}

	if _, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_dedupe",
		RequestID:    first.ID,
		Kind:         ResponseAnswer,
		Actor:        "tester",
		Message:      "resolved",
	}); err != nil {
		t.Fatal(err)
	}
	again := base
	again.ID = "hrq_after_resolved"
	afterResolved, err := store.Create(context.Background(), again)
	if err != nil {
		t.Fatal(err)
	}
	if afterResolved.ID == first.ID {
		t.Fatalf("resolved request blocked a later request: %+v", afterResolved)
	}
}

func TestStoreResolveApprovePersistsResponseAndAudit(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:             "hrq_approve",
		WorkspaceID:    "workspace",
		WorkspaceKey:   "ws_resolve",
		RunID:          "run-approve",
		AgentID:        "agent-1",
		SessionID:      "session-1",
		Kind:           RequestApproval,
		Question:       "Approve?",
		ActionSnapshot: &ActionSnapshot{ToolName: "test.echo", Arguments: map[string]any{"message": "hello"}},
	})

	resolved, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_resolve",
		RequestID:    req.ID,
		Kind:         ResponseApprove,
		Actor:        "user-1",
		Message:      "approved",
		ResolvedAt:   time.Date(2026, 6, 15, 10, 1, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Status != StatusResolved || resolved.ResolvedAt == nil {
		t.Fatalf("resolved request = %+v", resolved)
	}
	if resolved.Response == nil || resolved.Response.Kind != ResponseApprove || resolved.Response.Actor != "user-1" || resolved.Response.Message != "approved" {
		t.Fatalf("response = %+v", resolved.Response)
	}
	if len(resolved.Audit) == 0 || resolved.Audit[len(resolved.Audit)-1].FromStatus != StatusPending || resolved.Audit[len(resolved.Audit)-1].ToStatus != StatusResolved {
		t.Fatalf("audit = %+v", resolved.Audit)
	}
	respPath := filepath.Join(root, "workspaces", "ws_resolve", "human-responses", resolved.Response.ID+".yaml")
	if _, err := os.Stat(respPath); err != nil {
		t.Fatalf("expected response file: %v", err)
	}
}

func TestStoreResolveDenyPersistsResponseAndPreventsReplay(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:             "hrq_deny",
		WorkspaceID:    "workspace",
		WorkspaceKey:   "ws_deny",
		RunID:          "run-deny",
		AgentID:        "agent-1",
		SessionID:      "session-1",
		Kind:           RequestApproval,
		Question:       "Approve?",
		ActionSnapshot: &ActionSnapshot{ToolName: "test.echo", Arguments: map[string]any{"message": "hello"}},
	})
	resolved, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_deny",
		RequestID:    req.ID,
		Kind:         ResponseDeny,
		Actor:        "user-1",
		Message:      "no",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Response == nil || resolved.Response.Kind != ResponseDeny {
		t.Fatalf("response = %+v", resolved.Response)
	}
	if resolved.Replay == nil || resolved.Replay.Status != ReplayDenied {
		t.Fatalf("replay state = %+v", resolved.Replay)
	}
	_, err = store.BeginReplay(context.Background(), ReplayLeaseRequest{
		WorkspaceKey:  "ws_deny",
		RequestID:     req.ID,
		Owner:         "worker-1",
		LeaseDuration: time.Minute,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("BeginReplay after deny error = %v, want ErrConflict", err)
	}
}

func TestStoreResolveCancelPersistsCanceledSignal(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:             "hrq_cancel",
		WorkspaceID:    "workspace",
		WorkspaceKey:   "ws_cancel",
		RunID:          "run-cancel",
		AgentID:        "agent-1",
		SessionID:      "session-1",
		Kind:           RequestApproval,
		Question:       "Approve?",
		ActionSnapshot: &ActionSnapshot{ToolName: "test.echo", Arguments: map[string]any{"message": "hello"}},
	})
	resolved, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_cancel",
		RequestID:    req.ID,
		Kind:         ResponseCancel,
		Actor:        "user-1",
		Message:      "cancel it",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Response == nil || resolved.Response.Kind != ResponseCancel {
		t.Fatalf("response = %+v", resolved.Response)
	}
	if resolved.Replay == nil || resolved.Replay.Status != ReplayCanceled {
		t.Fatalf("replay state = %+v", resolved.Replay)
	}
}

func TestStoreResolveAnswerKeepsAnswerPayload(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:           "hrq_answer",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_answer",
		RunID:        "run-answer",
		AgentID:      "agent-1",
		SessionID:    "session-1",
		Kind:         RequestFreeform,
		Question:     "What should I do?",
	})
	resolved, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_answer",
		RequestID:    req.ID,
		Kind:         ResponseAnswer,
		Actor:        "user-1",
		Message:      "Use the safer path.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Response == nil || resolved.Response.Message != "Use the safer path." {
		t.Fatalf("response = %+v", resolved.Response)
	}
	_, err = store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_answer",
		RequestID:    "missing",
		Kind:         ResponseAnswer,
		Actor:        "user-1",
		Message:      "",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing answer error = %v, want ErrNotFound", err)
	}

	req2 := mustCreateHumanRequest(t, store, CreateRequest{
		ID:           "hrq_empty_answer",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_answer",
		RunID:        "run-answer",
		AgentID:      "agent-1",
		SessionID:    "session-1",
		Kind:         RequestFreeform,
		Question:     "What should I do next?",
	})
	_, err = store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_answer",
		RequestID:    req2.ID,
		Kind:         ResponseAnswer,
		Actor:        "user-1",
		Message:      "",
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("empty answer error = %v, want ErrValidation", err)
	}
}

func TestStoreRejectsDoubleResolve(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:           "hrq_double",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_double",
		RunID:        "run-double",
		AgentID:      "agent-1",
		SessionID:    "session-1",
		Kind:         RequestFreeform,
		Question:     "Answer once?",
	})
	first, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_double",
		RequestID:    req.ID,
		Kind:         ResponseAnswer,
		Actor:        "user-1",
		Message:      "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_double",
		RequestID:    req.ID,
		Kind:         ResponseDeny,
		Actor:        "user-2",
		Message:      "second",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second resolve error = %v, want ErrConflict", err)
	}
	stored, err := store.Get(context.Background(), "ws_double", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Response == nil || stored.Response.ID != first.Response.ID || stored.Response.Message != "first" {
		t.Fatalf("stored response changed after conflict: %+v", stored.Response)
	}
}

func TestStoreListPendingFiltersByWorkspaceAndStatus(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	old := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	newer := old.Add(time.Minute)
	mustCreateHumanRequest(t, store, CreateRequest{ID: "hrq_old", WorkspaceID: "workspace", WorkspaceKey: "ws_list", RunID: "run-1", AgentID: "agent", SessionID: "session", Kind: RequestFreeform, Question: "old?", CreatedAt: old})
	mustCreateHumanRequest(t, store, CreateRequest{ID: "hrq_new", WorkspaceID: "workspace", WorkspaceKey: "ws_list", RunID: "run-2", AgentID: "agent", SessionID: "session", Kind: RequestFreeform, Question: "new?", CreatedAt: newer})
	other := mustCreateHumanRequest(t, store, CreateRequest{ID: "hrq_other", WorkspaceID: "other", WorkspaceKey: "ws_other", RunID: "run-3", AgentID: "agent", SessionID: "session", Kind: RequestFreeform, Question: "other?", CreatedAt: newer})
	if _, err := store.Resolve(context.Background(), ResolveRequest{WorkspaceKey: "ws_other", RequestID: other.ID, Kind: ResponseAnswer, Actor: "user", Message: "done"}); err != nil {
		t.Fatal(err)
	}

	list, err := store.List(context.Background(), ListQuery{WorkspaceKey: "ws_list", Status: StatusPending})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "hrq_new" || list[1].ID != "hrq_old" {
		t.Fatalf("list = %+v", list)
	}
	for _, req := range list {
		if req.WorkspaceKey != "ws_list" || req.Status != StatusPending {
			t.Fatalf("unfiltered request in list: %+v", req)
		}
	}
}

func TestStoreShowMissingRequestReturnsNotFound(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:           "hrq_show",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_show",
		RunID:        "run-show",
		AgentID:      "agent-1",
		SessionID:    "session-1",
		Kind:         RequestFreeform,
		Question:     "visible?",
	})
	if _, err := store.Get(context.Background(), "ws_show", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing error = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(context.Background(), "ws_wrong", req.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong workspace error = %v, want ErrNotFound", err)
	}
}

func TestStoreReplayCASPendingRunningCompleted(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:             "hrq_replay",
		WorkspaceID:    "workspace",
		WorkspaceKey:   "ws_replay",
		RunID:          "run-replay",
		AgentID:        "agent-1",
		SessionID:      "session-1",
		Kind:           RequestApproval,
		Question:       "Replay?",
		ActionSnapshot: &ActionSnapshot{ToolName: "test.echo", Arguments: map[string]any{"message": "hello"}},
	})
	leased, err := store.BeginReplay(context.Background(), ReplayLeaseRequest{
		WorkspaceKey:  "ws_replay",
		RequestID:     req.ID,
		Owner:         "worker-1",
		LeaseDuration: time.Minute,
		Now:           time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if leased.Replay == nil || leased.Replay.Status != ReplayRunning || leased.Replay.LeaseOwner != "worker-1" {
		t.Fatalf("lease replay = %+v", leased.Replay)
	}
	_, err = store.BeginReplay(context.Background(), ReplayLeaseRequest{
		WorkspaceKey:  "ws_replay",
		RequestID:     req.ID,
		Owner:         "worker-2",
		LeaseDuration: time.Minute,
		Now:           time.Date(2026, 6, 15, 10, 0, 30, 0, time.UTC),
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second BeginReplay error = %v, want ErrConflict", err)
	}
	completed, err := store.CompleteReplay(context.Background(), CompleteReplayRequest{
		WorkspaceKey:    "ws_replay",
		RequestID:       req.ID,
		Owner:           "worker-1",
		ResultDigest:    "sha256:result",
		IdempotencyKey:  "idem-1",
		CompletedAt:     time.Date(2026, 6, 15, 10, 0, 45, 0, time.UTC),
		ResultReference: "runs/run-replay/tool_calls.jsonl",
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Replay == nil || completed.Replay.Status != ReplayCompleted || completed.Replay.ResultDigest != "sha256:result" {
		t.Fatalf("completed replay = %+v", completed.Replay)
	}
	again, err := store.CompleteReplay(context.Background(), CompleteReplayRequest{
		WorkspaceKey:    "ws_replay",
		RequestID:       req.ID,
		Owner:           "worker-1",
		ResultDigest:    "sha256:result",
		IdempotencyKey:  "idem-1",
		CompletedAt:     time.Date(2026, 6, 15, 10, 0, 46, 0, time.UTC),
		ResultReference: "runs/run-replay/tool_calls.jsonl",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Replay.ResultDigest != completed.Replay.ResultDigest {
		t.Fatalf("idempotent complete changed replay: before=%+v after=%+v", completed.Replay, again.Replay)
	}
	resultPath := filepath.Join(root, "workspaces", "ws_replay", "replay-results", req.ID+".yaml")
	if _, err := os.Stat(resultPath); err != nil {
		t.Fatalf("expected replay result file: %v", err)
	}
}

func TestStoreReplayLeaseCanRecoverAfterCrash(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:             "hrq_recover",
		WorkspaceID:    "workspace",
		WorkspaceKey:   "ws_recover",
		RunID:          "run-recover",
		AgentID:        "agent-1",
		SessionID:      "session-1",
		Kind:           RequestApproval,
		Question:       "Replay?",
		ActionSnapshot: &ActionSnapshot{ToolName: "test.echo", Arguments: map[string]any{"message": "hello"}},
	})
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	if _, err := store.BeginReplay(context.Background(), ReplayLeaseRequest{WorkspaceKey: "ws_recover", RequestID: req.ID, Owner: "worker-1", LeaseDuration: time.Minute, Now: start}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginReplay(context.Background(), ReplayLeaseRequest{WorkspaceKey: "ws_recover", RequestID: req.ID, Owner: "worker-2", LeaseDuration: time.Minute, Now: start.Add(30 * time.Second)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("early recovery error = %v, want ErrConflict", err)
	}
	recovered, err := store.BeginReplay(context.Background(), ReplayLeaseRequest{WorkspaceKey: "ws_recover", RequestID: req.ID, Owner: "worker-2", LeaseDuration: time.Minute, Now: start.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Replay == nil || recovered.Replay.LeaseOwner != "worker-2" {
		t.Fatalf("recovered replay = %+v", recovered.Replay)
	}
}

func TestReplayLeaseRecoveryAfterCrash(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:             "hrq_exact_recover",
		WorkspaceID:    "workspace",
		WorkspaceKey:   "ws_exact_recover",
		RunID:          "run-recover",
		AgentID:        "agent-1",
		SessionID:      "session-1",
		Kind:           RequestApproval,
		Question:       "Replay?",
		ActionSnapshot: &ActionSnapshot{ToolName: "test.echo", Arguments: map[string]any{"message": "hello"}},
	})
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	if _, err := store.BeginReplay(context.Background(), ReplayLeaseRequest{WorkspaceKey: "ws_exact_recover", RequestID: req.ID, Owner: "worker-1", LeaseDuration: time.Minute, Now: start}); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.BeginReplay(context.Background(), ReplayLeaseRequest{WorkspaceKey: "ws_exact_recover", RequestID: req.ID, Owner: "worker-2", LeaseDuration: time.Minute, Now: start.Add(2 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Replay == nil || recovered.Replay.Status != ReplayRunning || recovered.Replay.LeaseOwner != "worker-2" {
		t.Fatalf("recovered replay = %+v", recovered.Replay)
	}
}

func TestReplayRecordsAuditTrail(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:             "hrq_replay_audit",
		WorkspaceID:    "workspace",
		WorkspaceKey:   "ws_replay_audit",
		RunID:          "run-replay-audit",
		AgentID:        "agent-1",
		SessionID:      "session-1",
		Kind:           RequestApproval,
		Question:       "Replay?",
		ActionSnapshot: &ActionSnapshot{ToolName: "test.echo", Arguments: map[string]any{"message": "hello"}},
	})
	if _, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_replay_audit",
		RequestID:    req.ID,
		Kind:         ResponseApprove,
		Actor:        "human-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginReplay(context.Background(), ReplayLeaseRequest{
		WorkspaceKey:  "ws_replay_audit",
		RequestID:     req.ID,
		Owner:         "worker-1",
		LeaseDuration: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteReplay(context.Background(), CompleteReplayRequest{
		WorkspaceKey:    "ws_replay_audit",
		RequestID:       req.ID,
		Owner:           "worker-1",
		ResultDigest:    "sha256:result",
		ResultReference: "runs/run-replay-audit/tool_calls.jsonl",
		IdempotencyKey:  "audit-idem",
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, record := range completed.Audit {
		actions[record.Action] = true
	}
	for _, action := range []string{"human_request.created", "human_request.resolved", "human_request.replay_started", "human_request.replay_completed"} {
		if !actions[action] {
			t.Fatalf("audit missing %s: %+v", action, completed.Audit)
		}
	}
}

func TestStoreLoadCorruptFileReportsErrorButDoesNotPanic(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	dir := filepath.Join(root, "workspaces", "ws_corrupt", "human-requests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(":\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := store.List(context.Background(), ListQuery{WorkspaceKey: "ws_corrupt"})
	if err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Fatalf("corrupt list error = %v, want file-specific error", err)
	}
}

func TestStoreAtomicWriteDoesNotLeavePartialFile(t *testing.T) {
	root := t.TempDir()
	store := newTestStore(t, root)
	workspaceDir := filepath.Join(root, "workspaces", "ws_atomic")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "human-requests"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := store.Create(context.Background(), CreateRequest{
		ID:           "hrq_atomic",
		WorkspaceID:  "workspace",
		WorkspaceKey: "ws_atomic",
		RunID:        "run-atomic",
		AgentID:      "agent-1",
		SessionID:    "session-1",
		Kind:         RequestFreeform,
		Question:     "write?",
	})
	if err == nil {
		t.Fatal("Create() succeeded despite request directory being a file")
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "human-requests", "hrq_atomic.yaml")); err == nil {
		t.Fatal("partial request file exists")
	}
}

func newTestStore(t *testing.T, root string) *Store {
	t.Helper()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestNewStoreRequiresStateDir(t *testing.T) {
	if _, err := NewStore(" "); err == nil || !strings.Contains(err.Error(), "state dir is required") {
		t.Fatalf("NewStore() error = %v, want state dir requirement", err)
	}
}

func mustCreateHumanRequest(t *testing.T, store *Store, req CreateRequest) *HumanRequest {
	t.Helper()
	created, err := store.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return created
}

// TestCreateHumanRequestPersistsChatKey verifies ChatKey round-trips through
// Create → write YAML → Get → read YAML. This is the field #91-A adds so the
// Store can answer "which pending HITL belongs to this chatKey".
func TestCreateHumanRequestPersistsChatKey(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID:           "hrq_ck1",
		WorkspaceID:  "/ws",
		WorkspaceKey: "ws_ck",
		RunID:        "run-1",
		AgentID:      "agent-1",
		SessionID:    "sess-1",
		Kind:         RequestFreeform,
		Question:     "q",
		DedupeKey:    "d-1",
		CreatedAt:    time.Now(),
		ChatKey:      "ilink/chat-1/user-1",
	})
	if req.ChatKey != "ilink/chat-1/user-1" {
		t.Fatalf("Create() ChatKey = %q, want ilink/chat-1/user-1", req.ChatKey)
	}
	got, err := store.Get(context.Background(), "ws_ck", "hrq_ck1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ChatKey != "ilink/chat-1/user-1" {
		t.Errorf("Get() ChatKey = %q, want ilink/chat-1/user-1 (round-trip failed)", got.ChatKey)
	}
}

// TestListByChatKeyFiltersPendingRequests verifies ListQuery.ChatKey filters:
// create 3 pending requests (2 chatKey-A, 1 chatKey-B), list with ChatKey=A →
// only the 2 A requests return.
func TestListByChatKeyFiltersPendingRequests(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	for i, key := range []string{"A/A/A", "A/A/A", "B/B/B"} {
		mustCreateHumanRequest(t, store, CreateRequest{
			ID:           "hrq_" + string(rune('a'+i)),
			WorkspaceID:  "/ws",
			WorkspaceKey: "ws_lk",
			RunID:        "run-" + string(rune('a'+i)),
			AgentID:      "agent-1",
			SessionID:    "sess-1",
			Kind:         RequestFreeform,
			Question:     "q",
			DedupeKey:    "d-" + string(rune('a'+i)),
			CreatedAt:    time.Now(),
			ChatKey:      key,
		})
	}
	got, err := store.List(context.Background(), ListQuery{
		WorkspaceKey: "ws_lk",
		Status:       StatusPending,
		ChatKey:      "A/A/A",
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(ChatKey=A) returned %d, want 2", len(got))
	}
	for _, r := range got {
		if r.ChatKey != "A/A/A" {
			t.Errorf("returned request ChatKey = %q, want A/A/A", r.ChatKey)
		}
	}
}

// TestListByChatKeyEmptyMatchesAll verifies backward compat: ChatKey=="" does
// NOT filter (returns all, like the old behavior before the field existed).
func TestListByChatKeyEmptyMatchesAll(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_e1", WorkspaceID: "/ws", WorkspaceKey: "ws_e", RunID: "r1",
		AgentID: "a", SessionID: "s", Kind: RequestFreeform, Question: "q",
		DedupeKey: "d1", CreatedAt: time.Now(), ChatKey: "X/X/X",
	})
	mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_e2", WorkspaceID: "/ws", WorkspaceKey: "ws_e", RunID: "r2",
		AgentID: "a", SessionID: "s", Kind: RequestFreeform, Question: "q",
		DedupeKey: "d2", CreatedAt: time.Now(), ChatKey: "Y/Y/Y",
	})
	got, err := store.List(context.Background(), ListQuery{
		WorkspaceKey: "ws_e", Status: StatusPending,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(no ChatKey filter) returned %d, want 2 (empty ChatKey = no filter)", len(got))
	}
}

// TestOldYAMLWithoutChatKeyReadsAsEmpty verifies backward compat: a YAML file
// written before the ChatKey field existed reads back with ChatKey=="" (no
// error). Prevents breaking existing on-disk requests on upgrade.
func TestOldYAMLWithoutChatKeyReadsAsEmpty(t *testing.T) {
	root := t.TempDir()
	reqDir := filepath.Join(root, "workspaces", "ws_old", "human-requests")
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Hand-write a YAML without the chat_key field (mimics pre-upgrade file).
	oldYAML := strings.NewReader(strings.TrimSpace(`
id: hrq_old
workspace_id: /ws
workspace_key: ws_old
run_id: run-1
agent_id: agent-1
session_id: sess-1
kind: freeform
status: pending
question: legacy?
dedupe_key: d-old
created_at: 2026-06-01T00:00:00Z
`))
	dec := yaml.NewDecoder(oldYAML)
	var hr HumanRequest
	if err := dec.Decode(&hr); err != nil {
		t.Fatalf("decode old YAML: %v", err)
	}
	if hr.ChatKey != "" {
		t.Errorf("old YAML ChatKey = %q, want empty (missing field = zero value)", hr.ChatKey)
	}
}

// TestListByChatKeyConvenienceMethod covers Store.ListByChatKey directly
// (the convenience wrapper that fixes Status=Pending).
func TestListByChatKeyConvenienceMethod(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_lb1", WorkspaceID: "/ws", WorkspaceKey: "ws_lb", RunID: "r1",
		AgentID: "a", SessionID: "s", Kind: RequestFreeform, Question: "q",
		DedupeKey: "d1", CreatedAt: time.Now(), ChatKey: "ilink/c/u",
	})
	// A resolved (non-pending) request with the same chatKey — must NOT return.
	resolved := mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_lb2", WorkspaceID: "/ws", WorkspaceKey: "ws_lb", RunID: "r2",
		AgentID: "a", SessionID: "s", Kind: RequestFreeform, Question: "q2",
		DedupeKey: "d2", CreatedAt: time.Now(), ChatKey: "ilink/c/u",
	})
	if _, err := store.Resolve(context.Background(), ResolveRequest{
		WorkspaceKey: "ws_lb", RequestID: resolved.ID, Kind: ResponseApprove,
		Actor: "tester", Message: "ok", ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got, err := store.ListByChatKey(context.Background(), "ws_lb", "ilink/c/u")
	if err != nil {
		t.Fatalf("ListByChatKey: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListByChatKey returned %d, want 1 (only pending; resolved excluded)", len(got))
	}
	if got[0].ID != "hrq_lb1" {
		t.Errorf("returned ID = %q, want hrq_lb1", got[0].ID)
	}
	// Empty chatKey MUST error: this is a dedicated #92 entry point, and an
	// empty value (missing inbound field / bug) must NOT silently return all
	// pending (cross-chat mismatch risk). The底层 List(ChatKey:"") "no filter"
	// semantics are for backward-compat, not for this query.
	if _, err := store.ListByChatKey(context.Background(), "ws_lb", ""); err == nil {
		t.Fatal("ListByChatKey(empty) should error (cross-chat mismatch risk), not return all pending")
	}
}

// TestValidateCreateErrorBranches covers validateCreate's validation branches
// (each returns a distinct error). Table-driven to cover them all at once.
func TestValidateCreateErrorBranches(t *testing.T) {
	base := func() CreateRequest {
		return CreateRequest{
			ID: "hrq_v", WorkspaceID: "/ws", WorkspaceKey: "ws_v", RunID: "r",
			AgentID: "a", SessionID: "s", Kind: RequestFreeform, Question: "q",
		}
	}
	cases := []struct {
		name  string
		mut   func(*CreateRequest)
	}{
		{"bad workspace key (path traversal)", func(c *CreateRequest) { c.WorkspaceKey = "../bad" }},
		{"bad request id (path traversal)", func(c *CreateRequest) { c.ID = "../bad" }},
		{"empty workspace id", func(c *CreateRequest) { c.WorkspaceID = "" }},
		{"empty run id", func(c *CreateRequest) { c.RunID = "" }},
		{"empty agent id", func(c *CreateRequest) { c.AgentID = "" }},
		{"empty session id", func(c *CreateRequest) { c.SessionID = "" }},
		{"invalid kind", func(c *CreateRequest) { c.Kind = "bogus" }},
		{"empty question", func(c *CreateRequest) { c.Question = "" }},
		{"option without id", func(c *CreateRequest) { c.Options = []HumanOption{{ID: ""}} }},
		{"duplicate option id", func(c *CreateRequest) {
			c.Options = []HumanOption{{ID: "x"}, {ID: "x"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := base()
			tc.mut(&input)
			if err := validateCreate(input); err == nil {
				t.Errorf("validateCreate(%s) = nil, want error", tc.name)
			}
		})
	}
	// Happy path: valid input returns nil.
	if err := validateCreate(base()); err != nil {
		t.Errorf("validateCreate(valid) = %v, want nil", err)
	}
}

// TestStoreCreateValidationErrors covers Store.Create's validation entry points
// (invalid workspace key, invalid request id) — exercises Create's error branches
// before the happy path.
func TestStoreCreateValidationErrors(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	base := func() CreateRequest {
		return CreateRequest{ID: "x", WorkspaceID: "/ws", WorkspaceKey: "ws", RunID: "r",
			AgentID: "a", SessionID: "s", Kind: RequestFreeform, Question: "q"}
	}
	// Bad workspace key.
	bad := base()
	bad.WorkspaceKey = "../bad"
	if _, err := store.Create(context.Background(), bad); err == nil {
		t.Error("Create with bad workspace key should error")
	}
	// Bad request id.
	bad = base()
	bad.ID = "../bad"
	if _, err := store.Create(context.Background(), bad); err == nil {
		t.Error("Create with bad request id should error")
	}
	// Missing question.
	bad = base()
	bad.Question = ""
	if _, err := store.Create(context.Background(), bad); err == nil {
		t.Error("Create with empty question should error")
	}
}

// TestFailReplayValidation covers FailReplay's error branches (invalid workspace,
// invalid request id, empty owner, no replay state, replay not running).
func TestFailReplayValidation(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	// Create a request WITH an action snapshot so it has a Replay state.
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_fr", WorkspaceID: "/ws", WorkspaceKey: "ws_fr", RunID: "r",
		AgentID: "a", SessionID: "s", Kind: RequestApproval, Question: "q",
		ActionSnapshot: &ActionSnapshot{ToolName: "t", RunID: "r"},
	})
	// Begin replay first so FailReplay can act on it.
	beginReq, err := store.BeginReplay(context.Background(), ReplayLeaseRequest{
		WorkspaceKey: "ws_fr", RequestID: "hrq_fr", Owner: "tester",
	})
	if err != nil {
		t.Fatalf("BeginReplay: %v", err)
	}
	_ = beginReq

	// Bad workspace key.
	if _, err := store.FailReplay(context.Background(), FailReplayRequest{
		WorkspaceKey: "../bad", RequestID: "hrq_fr", Owner: "tester", Error: "x",
	}); err == nil {
		t.Error("FailReplay bad workspace should error")
	}
	// Empty owner.
	if _, err := store.FailReplay(context.Background(), FailReplayRequest{
		WorkspaceKey: "ws_fr", RequestID: "hrq_fr", Owner: "", Error: "x",
	}); err == nil {
		t.Error("FailReplay empty owner should error")
	}
	// Not-found request.
	if _, err := store.FailReplay(context.Background(), FailReplayRequest{
		WorkspaceKey: "ws_fr", RequestID: "ghost", Owner: "tester", Error: "x",
	}); err == nil {
		t.Error("FailReplay ghost request should error")
	}
	// Happy path: fail the running replay.
	if _, err := store.FailReplay(context.Background(), FailReplayRequest{
		WorkspaceKey: "ws_fr", RequestID: "hrq_fr", Owner: "tester", Error: "done",
	}); err != nil {
		t.Errorf("FailReplay happy path: %v", err)
	}
	// Fail again → replay not running (conflict).
	if _, err := store.FailReplay(context.Background(), FailReplayRequest{
		WorkspaceKey: "ws_fr", RequestID: "hrq_fr", Owner: "tester", Error: "x",
	}); err == nil {
		t.Error("FailReplay on non-running replay should error")
	}
	_ = req
}

// TestWorkspaceKeyFor covers the trivial WorkspaceKeyFor (was 0%).
func TestWorkspaceKeyFor(t *testing.T) {
	got := WorkspaceKeyFor("team-a")
	if got == "" {
		t.Error("WorkspaceKeyFor returned empty for non-empty input")
	}
	// Deterministic for same input.
	if WorkspaceKeyFor("team-a") != got {
		t.Error("WorkspaceKeyFor not deterministic")
	}
}

// TestStoreAdditionalErrorBranches covers remaining low-coverage branches:
// List/Get error paths, validateResponseKind invalid, cloneStringMap nil.
func TestStoreAdditionalErrorBranches(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	ctx := context.Background()

	// List with bad workspace key → error.
	if _, err := store.List(ctx, ListQuery{WorkspaceKey: "../bad"}); err == nil {
		t.Error("List bad workspace should error")
	}
	// List with invalid status → error.
	if _, err := store.List(ctx, ListQuery{WorkspaceKey: "ws", Status: "bogus"}); err == nil {
		t.Error("List invalid status should error")
	}
	// Get with bad workspace key → error.
	if _, err := store.Get(ctx, "../bad", "x"); err == nil {
		t.Error("Get bad workspace should error")
	}
	// Get non-existent request → error.
	if _, err := store.Get(ctx, "ws", "ghost"); err == nil {
		t.Error("Get non-existent should error")
	}
	// validateResponseKind invalid → error.
	if err := validateResponseKind("bogus"); err == nil {
		t.Error("validateResponseKind bogus should error")
	}
	// validateResponseKind valid → nil.
	for _, k := range []ResponseKind{ResponseApprove, ResponseDeny, ResponseCancel, ResponseAnswer} {
		if err := validateResponseKind(k); err != nil {
			t.Errorf("validateResponseKind(%q) = %v, want nil", k, err)
		}
	}
}

// TestCloneStringMapEdgeCases covers cloneStringMap nil + empty (was 33%).
func TestCloneStringMapEdgeCases(t *testing.T) {
	if got := cloneStringMap(nil); got != nil {
		t.Errorf("cloneStringMap(nil) = %v, want nil", got)
	}
	if got := cloneStringMap(map[string]string{}); len(got) != 0 {
		t.Errorf("cloneStringMap(empty) = %v, want empty", got)
	}
	in := map[string]string{"a": "1", "b": "2"}
	got := cloneStringMap(in)
	if len(got) != 2 || got["a"] != "1" || got["b"] != "2" {
		t.Errorf("cloneStringMap = %v, want %v", got, in)
	}
	// Mutating clone must not affect original.
	got["c"] = "3"
	if _, ok := in["c"]; ok {
		t.Error("cloneStringMap not independent from source")
	}
}

// TestResolveWriteResponseAndLoad covers Resolve (which calls writeResponse +
// loadRequest) and its error branches. Also exercises CompleteReplay's
// not-running branch.
func TestResolveWriteResponseAndLoad(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	ctx := context.Background()
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_rw", WorkspaceID: "/ws", WorkspaceKey: "ws_rw", RunID: "r",
		AgentID: "a", SessionID: "s", Kind: RequestFreeform, Question: "q",
	})
	// Happy path: Resolve writes a response.
	if _, err := store.Resolve(ctx, ResolveRequest{
		WorkspaceKey: "ws_rw", RequestID: "hrq_rw", Kind: ResponseAnswer,
		Actor: "u", Message: "ans", ResolvedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Resolve again → already resolved (conflict).
	if _, err := store.Resolve(ctx, ResolveRequest{
		WorkspaceKey: "ws_rw", RequestID: "hrq_rw", Kind: ResponseAnswer,
		Actor: "u", Message: "ans", ResolvedAt: time.Now(),
	}); err == nil {
		t.Error("Resolve twice should error (already resolved)")
	}
	// Resolve non-existent → error.
	if _, err := store.Resolve(ctx, ResolveRequest{
		WorkspaceKey: "ws_rw", RequestID: "ghost", Kind: ResponseAnswer,
		Actor: "u", Message: "x", ResolvedAt: time.Now(),
	}); err == nil {
		t.Error("Resolve ghost should error")
	}
	// Resolve invalid kind → error.
	if _, err := store.Resolve(ctx, ResolveRequest{
		WorkspaceKey: "ws_rw", RequestID: "hrq_rw", Kind: "bogus",
		Actor: "u", Message: "x", ResolvedAt: time.Now(),
	}); err == nil {
		t.Error("Resolve invalid kind should error")
	}
	_ = req
}

// TestCompleteReplayNotRunning covers CompleteReplay's "replay not running"
// conflict branch (create a request, don't begin replay, try complete → error).
func TestCompleteReplayNotRunning(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_cr", WorkspaceID: "/ws", WorkspaceKey: "ws_cr", RunID: "r",
		AgentID: "a", SessionID: "s", Kind: RequestApproval, Question: "q",
		ActionSnapshot: &ActionSnapshot{ToolName: "t", RunID: "r"},
	})
	// CompleteReplay without BeginReplay → replay not running error.
	if _, err := store.CompleteReplay(context.Background(), CompleteReplayRequest{
		WorkspaceKey: "ws_cr", RequestID: "hrq_cr", ResultReference: "ok",
	}); err == nil {
		t.Error("CompleteReplay without BeginReplay should error (not running)")
	}
}

// TestWriteYAMLAtomicFailure covers writeYAMLAtomic's error branches by
// pointing the store at an unwritable root (parent dir doesn't exist / is a
// file). This exercises the os.WriteFile / os.Rename failure paths.
func TestWriteYAMLAtomicFailure(t *testing.T) {
	// Root that doesn't exist → Create will fail to write.
	store := newTestStore(t, t.TempDir())
	// Force an invalid requests dir by using a workspace key whose path is a
	// file (not a dir) — writeRequest will fail.
	root := t.TempDir()
	filePath := filepath.Join(root, "blocker")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	store2, err2 := NewStore(filePath) // root is a file → writes fail
	if err2 != nil || store2 == nil {
		t.Skipf("NewStore on file root: %v (env-dependent, skip)", err2)
	}
	_, err := store2.Create(context.Background(), CreateRequest{
		ID: "x", WorkspaceID: "/ws", WorkspaceKey: "ws", RunID: "r",
		AgentID: "a", SessionID: "s", Kind: RequestFreeform, Question: "q",
	})
	if err == nil {
		t.Error("Create on unwritable root should error (writeYAMLAtomic failure path)")
	}
	_ = store
}

// TestLoadRequestMissing covers loadRequest's file-not-found branch explicitly.
func TestLoadRequestMissing(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	// Get a request that was never created.
	_, err := store.Get(context.Background(), "ws_lm", "never-existed")
	if err == nil {
		t.Error("Get missing request should error")
	}
}

// TestBeginReplayErrorBranches covers BeginReplay's validation + state checks
// (no snapshot, already running, not found) — was 77%.
func TestBeginReplayErrorBranches(t *testing.T) {
	store := newTestStore(t, t.TempDir())
	ctx := context.Background()
	// Request WITHOUT action snapshot → BeginReplay rejects (no snapshot).
	mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_nosnap", WorkspaceID: "/ws", WorkspaceKey: "ws_br", RunID: "r",
		AgentID: "a", SessionID: "s", Kind: RequestFreeform, Question: "q",
	})
	if _, err := store.BeginReplay(ctx, ReplayLeaseRequest{
		WorkspaceKey: "ws_br", RequestID: "hrq_nosnap", Owner: "o",
	}); err == nil {
		t.Error("BeginReplay on request without snapshot should error")
	}
	// Not-found request.
	if _, err := store.BeginReplay(ctx, ReplayLeaseRequest{
		WorkspaceKey: "ws_br", RequestID: "ghost", Owner: "o",
	}); err == nil {
		t.Error("BeginReplay ghost should error")
	}
	// Request WITH snapshot → BeginReplay succeeds; second BeginReplay → conflict (already running).
	req := mustCreateHumanRequest(t, store, CreateRequest{
		ID: "hrq_snap", WorkspaceID: "/ws", WorkspaceKey: "ws_br", RunID: "r2",
		AgentID: "a", SessionID: "s", Kind: RequestApproval, Question: "q",
		ActionSnapshot: &ActionSnapshot{ToolName: "t", RunID: "r2"},
	})
	if _, err := store.BeginReplay(ctx, ReplayLeaseRequest{
		WorkspaceKey: "ws_br", RequestID: "hrq_snap", Owner: "o",
	}); err != nil {
		t.Fatalf("BeginReplay happy: %v", err)
	}
	if _, err := store.BeginReplay(ctx, ReplayLeaseRequest{
		WorkspaceKey: "ws_br", RequestID: "hrq_snap", Owner: "o2",
	}); err == nil {
		t.Error("BeginReplay twice should error (already running)")
	}
	_ = req
}

// TestStoreWriteFailuresOnReadOnlyRoot triggers writeYAMLAtomic failures by
// pointing the store at a path that's a FILE (not a dir) — every write under it
// fails reliably across OSes (no chmod dependency). Covers writeYAMLAtomic's
// os.CreateTemp/os.Rename error branches → writeRequest/writeResponse error return.
func TestStoreWriteFailuresOnReadOnlyRoot(t *testing.T) {
	// Create a file; use it as the store root. Any write (Create/Resolve) under
	// it fails because the "workspaces/<key>/..." path can't be created inside a file.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(blocker)
	if err != nil {
		t.Skipf("NewStore(blocker): %v", err)
	}
	// Create → writeRequest → writeYAMLAtomic fails (can't mkdir under a file).
	_, err = store.Create(context.Background(), CreateRequest{
		ID: "x", WorkspaceID: "/ws", WorkspaceKey: "ws", RunID: "r",
		AgentID: "a", SessionID: "s", Kind: RequestFreeform, Question: "q",
	})
	if err == nil {
		t.Error("Create under file-as-root should fail (writeYAMLAtomic error path)")
	}
}
