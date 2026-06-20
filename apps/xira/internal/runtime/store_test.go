package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNewRunStoreDefaultRoot covers the empty-root default.
func TestNewRunStoreDefaultRoot(t *testing.T) {
	s := NewRunStore("")
	if s.Root() != ".xira/runs" {
		t.Fatalf("empty root should default to .xira/runs, got %q", s.Root())
	}
	if got := NewRunStore("  ").Root(); got != ".xira/runs" {
		t.Fatalf("blank root should default to .xira/runs, got %q", got)
	}
}

// TestRunStoreInitRun covers the empty-id error + directory creation.
func TestRunStoreInitRun(t *testing.T) {
	s := NewRunStore(t.TempDir())
	if err := s.InitRun(""); err == nil {
		t.Fatalf("empty run id should error")
	}
	if err := s.InitRun("   "); err == nil {
		t.Fatalf("blank run id should error")
	}
	if err := s.InitRun("run-1"); err != nil {
		t.Fatalf("InitRun failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.RunDir("run-1"), "artifacts")); err != nil {
		t.Fatalf("artifacts dir not created: %v", err)
	}
}

// TestRunStoreSaveLoadRoundtrip covers SaveRun (happy path, all jsonl/yaml
// writes) + Load roundtrip, including the EvolutionCandidate branch.
func TestRunStoreSaveLoadRoundtrip(t *testing.T) {
	s := NewRunStore(t.TempDir())
	now := time.Now().UTC()
	resp := TurnResponse{
		RunID:      "run-save-1",
		Status:     "completed",
		StartedAt:  now,
		FinalResponse: "done",
		Events:       []RuntimeEvent{{ID: "e1", Kind: "assistant.final", Scope: &RuntimeEventScope{RunID: "run-save-1"}}},
		AuditEvents:  []AuditEvent{{ID: "a1", Time: now, Action: "tool.call"}},
		LLMCalls:     []LLMCallRecord{{Model: "deepseek"}},
		ToolCalls:    []ToolCallRecord{{Name: "read_file"}},
	}
	if err := s.SaveRun(resp); err != nil {
		t.Fatalf("SaveRun failed: %v", err)
	}
	loaded, err := s.Load("run-save-1")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded.RunID != "run-save-1" || loaded.Status != "completed" || loaded.FinalResponse != "done" {
		t.Fatalf("roundtrip lost fields: %+v", loaded)
	}

	// EvolutionCandidate branch: SaveRun creates candidates dir + file.
	resp.RunID = "run-evo-1"
	resp.EvolutionCandidate = &EvolutionCandidate{ID: "cand-1", RunID: "run-evo-1", Trigger: "t", Status: "open"}
	if err := s.SaveRun(resp); err != nil {
		t.Fatalf("SaveRun with evolution candidate failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.RunDir("run-evo-1"), "evolution", "candidates", "cand-1.yaml")); err != nil {
		t.Fatalf("evolution candidate file not written: %v", err)
	}
}

// TestRunStoreList covers: missing root -> nil,nil; dir entries -> sorted desc;
// non-dir entries skipped; unreadable runs skipped.
func TestRunStoreList(t *testing.T) {
	// Missing root -> (nil, nil), not an error.
	s := NewRunStore(filepath.Join(t.TempDir(), "nope"))
	got, err := s.List()
	if err != nil || got != nil {
		t.Fatalf("missing root: got=%v err=%v, want nil/nil", got, err)
	}

	// A real store with two saved runs + a non-dir entry + a bogus dir.
	s = NewRunStore(t.TempDir())
	t0 := time.Now().UTC()
	saveAt := func(id string, started time.Time) {
		if err := s.SaveRun(TurnResponse{RunID: id, Status: "completed", StartedAt: started}); err != nil {
			t.Fatal(err)
		}
	}
	saveAt("run-a", t0)
	saveAt("run-b", t0.Add(time.Minute)) // later -> sorts first
	// A non-dir file in root must be skipped.
	if err := os.WriteFile(filepath.Join(s.Root(), "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dir with no run.yaml must be skipped (Load errors).
	if err := os.MkdirAll(filepath.Join(s.Root(), "bogus"), 0o755); err != nil {
		t.Fatal(err)
	}

	runs, err := s.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d: %+v", len(runs), runs)
	}
	if runs[0].RunID != "run-b" {
		t.Fatalf("expected newest (run-b) first, got %q", runs[0].RunID)
	}
}

// TestNewRunID covers agent-id sanitization + timestamp format.
func TestNewRunID(t *testing.T) {
	id := NewRunID("agent with/slash", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if id != "20260102-030405-agent-with-slash" {
		t.Fatalf("run id = %q", id)
	}
}
