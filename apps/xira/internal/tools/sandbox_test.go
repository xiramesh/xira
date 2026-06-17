package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExpandRoots(t *testing.T) {
	t.Run("rejects relative roots", func(t *testing.T) {
		if _, err := ExpandRoots([]string{"some/relative/path"}); err == nil {
			t.Fatal("expected error for relative root")
		}
	})
	t.Run("skips blank entries", func(t *testing.T) {
		got, err := ExpandRoots([]string{"  "})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected no roots, got %v", got)
		}
	})
	t.Run("keeps absolute and dedupes", func(t *testing.T) {
		a := filepath.Clean(os.TempDir())
		got, err := ExpandRoots([]string{a, a})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		count := 0
		for _, r := range got {
			if r == a {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected %q deduped to one entry, got %v", a, got)
		}
	})
	t.Run("expands home tilde", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("home dir unavailable: %v", err)
		}
		got, err := ExpandRoots([]string{"~/Documents"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(home, "Documents")
		if len(got) != 1 || got[0] != want {
			t.Fatalf("expected %q, got %v", want, got)
		}
	})
}

func TestPathWithinRoots(t *testing.T) {
	root, err := filepath.Abs(filepath.Clean("/tmp/xira-sandbox-root"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"equal to root", root, true},
		{"nested under root", filepath.Join(root, "child", "file.md"), true},
		{"outside root", filepath.Join(root, "..", "sibling"), false},
		{"traversal escape", filepath.Join(root, "..", "..", "escape"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathWithinRoots(tc.path, []string{root}); got != tc.want {
				t.Fatalf("pathWithinRoots(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestSandboxReadWrite drives the full registry to verify that allow_roots are
// read/write, readonly_roots are read-only, and anything else stays locked.
func TestSandboxReadWrite(t *testing.T) {
	workspace := t.TempDir()
	allowDir := t.TempDir()    // read/write
	readonlyDir := t.TempDir() // read-only
	externalDir := t.TempDir() // not granted at all

	writeFile(t, filepath.Join(workspace, "ws.txt"), "ws")
	writeFile(t, filepath.Join(allowDir, "allow.txt"), "allow")
	writeFile(t, filepath.Join(readonlyDir, "ro.txt"), "ro")
	writeFile(t, filepath.Join(externalDir, "ext.txt"), "ext")

	reg := NewBuiltinRegistry(workspace, []string{
		"read_file", "write_file", "command.run",
	}, SandboxRoots{
		AllowRoots:    []string{allowDir},
		ReadonlyRoots: []string{readonlyDir},
	})
	readTool, _ := reg.Get("read_file")
	writeTool, _ := reg.Get("write_file")
	cmdTool, _ := reg.Get("command.run")

	// read_file: workspace, allow, readonly all readable; external denied.
	for _, p := range []string{
		filepath.Join(workspace, "ws.txt"),
		filepath.Join(allowDir, "allow.txt"),
		filepath.Join(readonlyDir, "ro.txt"),
	} {
		if _, err := readTool.Execute(context.Background(), map[string]any{"path": p}); err != nil {
			t.Fatalf("read_file %q should be allowed: %v", p, err)
		}
	}
	if _, err := readTool.Execute(context.Background(), map[string]any{"path": filepath.Join(externalDir, "ext.txt")}); err == nil {
		t.Fatal("read_file external path should be denied")
	}

	// write_file: workspace + allow writable; readonly + external denied.
	for _, p := range []string{
		filepath.Join(workspace, "new.txt"),
		filepath.Join(allowDir, "new.txt"),
	} {
		if _, err := writeTool.Execute(context.Background(), map[string]any{"path": p, "content": "x"}); err != nil {
			t.Fatalf("write_file %q should be allowed: %v", p, err)
		}
	}
	if _, err := writeTool.Execute(context.Background(), map[string]any{"path": filepath.Join(readonlyDir, "new.txt"), "content": "x"}); err == nil {
		t.Fatal("write_file into readonly root should be denied")
	}
	if _, err := writeTool.Execute(context.Background(), map[string]any{"path": filepath.Join(externalDir, "new.txt"), "content": "x"}); err == nil {
		t.Fatal("write_file external path should be denied")
	}

	// command.run cwd: workspace + allow allowed; readonly denied.
	for _, cwd := range []string{workspace, allowDir} {
		if _, err := cmdTool.Execute(context.Background(), map[string]any{"program": noopProgram(), "cwd": cwd}); err != nil {
			t.Fatalf("command cwd %q should be allowed: %v", cwd, err)
		}
	}
	if _, err := cmdTool.Execute(context.Background(), map[string]any{"program": noopProgram(), "cwd": readonlyDir}); err == nil {
		t.Fatal("command cwd into readonly root should be denied")
	}
}

// TestDefaultSandboxLocked ensures an agent with no extra roots keeps the
// historical workspace-only behavior (regression guard).
func TestDefaultSandboxLocked(t *testing.T) {
	workspace := t.TempDir()
	external := t.TempDir()
	writeFile(t, filepath.Join(workspace, "ws.txt"), "ws")
	writeFile(t, filepath.Join(external, "ext.txt"), "ext")

	reg := NewBuiltinRegistry(workspace, []string{"read_file", "write_file"}, SandboxRoots{})
	readTool, _ := reg.Get("read_file")
	writeTool, _ := reg.Get("write_file")

	if _, err := readTool.Execute(context.Background(), map[string]any{"path": filepath.Join(workspace, "ws.txt")}); err != nil {
		t.Fatalf("read workspace should be allowed: %v", err)
	}
	if _, err := readTool.Execute(context.Background(), map[string]any{"path": filepath.Join(external, "ext.txt")}); err == nil {
		t.Fatal("read external should be denied without allow_roots")
	}
	if _, err := writeTool.Execute(context.Background(), map[string]any{"path": filepath.Join(external, "x"), "content": "x"}); err == nil {
		t.Fatal("write external should be denied without allow_roots")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func noopProgram() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "true"
}
