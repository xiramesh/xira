package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// TestServiceAccessors covers the nil-safe + normal paths of the trivial
// Service accessors (Close, EventBus, StateDir/StateRoot, RunStore, Entrypoints).
func TestServiceAccessors(t *testing.T) {
	// Nil receivers must be safe (defensive guards).
	var nilSvc *Service
	if nilSvc.Close(); false { // must not panic
	}
	if nilSvc.StateDir() != "" {
		t.Fatalf("nil StateDir should be empty")
	}
	if nilSvc.Entrypoints() != nil {
		t.Fatalf("nil Entrypoints should be nil")
	}
	if nilSvc.EventBus() != nil {
		t.Fatalf("nil EventBus should be nil")
	}

	// Real service: accessors return the constructed fields.
	svc := newTestService(t, Config{})
	dir := svc.StateDir()
	if dir == "" {
		t.Fatalf("StateDir should be non-empty")
	}
	if svc.StateRoot() != dir {
		t.Fatalf("StateRoot should equal StateDir")
	}
	if svc.EventBus() == nil {
		t.Fatalf("EventBus should be set")
	}
	if svc.RunStore() == nil {
		t.Fatalf("RunStore should be set")
	}
	// Close is idempotent and safe to call twice.
	svc.Close()
	svc.Close()
}

// TestSamePath covers both branches of the path-equality helper.
func TestSamePath(t *testing.T) {
	if samePath("", "/x") {
		t.Fatalf("empty left should be false")
	}
	if samePath("/x", "  ") {
		t.Fatalf("blank right should be false")
	}
	// Same resolved path -> true.
	a, _ := filepath.Abs("/tmp")
	b, _ := filepath.Abs("/tmp")
	if !samePath(a, b) {
		t.Fatalf("same abs paths should be equal: %q vs %q", a, b)
	}
}

// TestDirExists covers the exists / not-exists / not-a-dir arms.
func TestDirExists(t *testing.T) {
	exists, err := dirExists(t.TempDir())
	if err != nil || !exists {
		t.Fatalf("temp dir should exist, got exists=%v err=%v", exists, err)
	}
	exists, err = dirExists(filepath.Join(t.TempDir(), "nope"))
	if err != nil || exists {
		t.Fatalf("nonexistent should be false/no-err, got exists=%v err=%v", exists, err)
	}
	// A file is not a directory.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exists, err = dirExists(f)
	if err != nil || exists {
		t.Fatalf("file should report exists=false, got exists=%v err=%v", exists, err)
	}
}
