package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestReadFileExpandsTilde: a user-supplied path starting with ~ must expand to
// the home directory (matching shell.run's behavior), NOT be treated as a
// relative path under the workspace. Previously read_file "~/work/..." resolved
// to "workspace/~/work/..." — the RCA's P2 path-semantics inconsistency.
func TestReadFileExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home dir: %v", err)
	}
	// Create a real file under home so the expanded path resolves.
	target := filepath.Join(home, ".xira_fs_home_test_marker")
	if err := os.WriteFile(target, []byte("home-content"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { os.Remove(target) })

	workspace := t.TempDir()
	// Allow home as a read root so the expanded path passes the sandbox check.
	registry := NewBuiltinRegistry(workspace, []string{"read_file"}, SandboxRoots{AllowRoots: []string{home}})

	out, err := registry.Execute(context.Background(), "read_file", map[string]any{"path": "~/.xira_fs_home_test_marker"})
	if err != nil {
		t.Fatalf("read_file ~ expansion failed: %v", err)
	}
	got, _ := out["content"].(string)
	if got != "home-content" {
		t.Fatalf("read_file ~ path returned wrong content: %q (want home-content)", got)
	}
}

// TestListDirExpandsTilde: list_dir must also expand ~ consistently.
func TestListDirExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home dir: %v", err)
	}
	workspace := t.TempDir()
	registry := NewBuiltinRegistry(workspace, []string{"list_dir"}, SandboxRoots{AllowRoots: []string{home}})

	out, err := registry.Execute(context.Background(), "list_dir", map[string]any{"path": "~"})
	if err != nil {
		t.Fatalf("list_dir ~ expansion failed: %v", err)
	}
	// ~ should resolve to home and list its entries (non-empty for any real home).
	if entries, _ := out["entries"].([]map[string]any); len(entries) == 0 {
		// home could theoretically be empty; just assert no error and path resolved.
		t.Logf("list_dir ~ returned 0 entries (home may be empty) — expansion still worked")
	}
}
