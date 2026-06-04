package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinRegistryFiltersAllowedTools(t *testing.T) {
	registry := NewBuiltinRegistry(t.TempDir(), []string{"read_file", "exec", "missing"})

	if got := strings.Join(registry.List(), ","); got != "exec,read_file" {
		t.Fatalf("List() = %q", got)
	}
	if len(registry.Definitions()) != 2 {
		t.Fatalf("Definitions len = %d", len(registry.Definitions()))
	}
}

func TestBuiltinRegistryRequiresExplicitAllowedTools(t *testing.T) {
	registry := NewBuiltinRegistry(t.TempDir(), nil)

	if got := strings.Join(registry.List(), ","); got != "" {
		t.Fatalf("List() = %q, want no tools", got)
	}
}

func TestFileToolsReadWriteListAndEdit(t *testing.T) {
	workspace := t.TempDir()
	registry := NewBuiltinRegistry(workspace, []string{"read_file", "write_file", "list_dir", "edit_file"})

	if _, err := registry.Execute(context.Background(), "write_file", map[string]any{
		"path":    "notes/one.md",
		"content": "hello Xira\n",
	}); err != nil {
		t.Fatalf("write_file error = %v", err)
	}

	read, err := registry.Execute(context.Background(), "read_file", map[string]any{"path": "notes/one.md"})
	if err != nil {
		t.Fatalf("read_file error = %v", err)
	}
	if read["content"] != "hello Xira\n" {
		t.Fatalf("read content = %q", read["content"])
	}

	list, err := registry.Execute(context.Background(), "list_dir", map[string]any{"path": "notes"})
	if err != nil {
		t.Fatalf("list_dir error = %v", err)
	}
	entries, ok := list["entries"].([]map[string]any)
	if !ok || len(entries) != 1 || entries[0]["name"] != "one.md" {
		t.Fatalf("entries = %#v", list["entries"])
	}

	if _, err := registry.Execute(context.Background(), "edit_file", map[string]any{
		"path":     "notes/one.md",
		"old_text": "Xira",
		"new_text": "kernel",
	}); err != nil {
		t.Fatalf("edit_file error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "notes", "one.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello kernel\n" {
		t.Fatalf("updated file = %q", string(data))
	}
}

func TestEditFileRejectsAmbiguousReplacement(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "dupe.txt")
	if err := os.WriteFile(path, []byte("x x"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewBuiltinRegistry(workspace, []string{"edit_file"})

	_, err := registry.Execute(context.Background(), "edit_file", map[string]any{
		"path":     "dupe.txt",
		"old_text": "x",
		"new_text": "y",
	})
	if err == nil {
		t.Fatal("expected ambiguous edit error")
	}
	if !strings.Contains(err.Error(), "occurs 2 times") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecRunsShellCommand(t *testing.T) {
	workspace := t.TempDir()
	registry := NewBuiltinRegistry(workspace, []string{"exec"})

	out, err := registry.Execute(context.Background(), "exec", map[string]any{
		"action":  "run",
		"command": "printf hello > out.txt && cat out.txt",
	})
	if err != nil {
		t.Fatalf("exec error = %v output=%+v", err, out)
	}
	if out["stdout"] != "hello" {
		t.Fatalf("stdout = %q", out["stdout"])
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); err != nil {
		t.Fatalf("expected shell output file: %v", err)
	}
}
