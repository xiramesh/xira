package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinRegistryFiltersAllowedTools(t *testing.T) {
	registry := NewBuiltinRegistry(t.TempDir(), []string{"read_file", "command.run", "shell.run", "tool_output.read", "exec", "missing"})

	if got := strings.Join(registry.List(), ","); got != "command.run,read_file,shell.run,tool_output.read" {
		t.Fatalf("List() = %q", got)
	}
	if len(registry.Definitions()) != 4 {
		t.Fatalf("Definitions len = %d", len(registry.Definitions()))
	}
	if registry.Has("exec") {
		t.Fatal("exec should not be registered")
	}
}

func TestToolOutputReadReadsCurrentRunArtifact(t *testing.T) {
	runDir := t.TempDir()
	rawRel := filepath.Join("artifacts", "tool-outputs", "call-1.json")
	rawAbs := filepath.Join(runDir, rawRel)
	if err := os.MkdirAll(filepath.Dir(rawAbs), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{
		"tool":"shell.run",
		"command":"go test ./...",
		"stdout":"hello world from stdout",
		"stderr":"warning line\nreal failure\n",
		"stdout_bytes":23,
		"stderr_bytes":26,
		"exit_code":1,
		"duration_ms":12
	}`
	if err := os.WriteFile(rawAbs, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewBuiltinRegistry(t.TempDir(), []string{"tool_output.read"})
	ctx := WithRunDir(context.Background(), runDir)

	tail, err := registry.Execute(ctx, "tool_output.read", map[string]any{
		"raw_output_path": filepath.ToSlash(rawRel),
		"stream":          "stderr",
		"tail_lines":      1,
	})
	if err != nil {
		t.Fatalf("tool_output.read tail error = %v", err)
	}
	if tail["content"] != "real failure\n" || tail["mode"] != "tail" || tail["stream"] != "stderr" || tail["truncated"] != true {
		t.Fatalf("tail output = %+v", tail)
	}
	if tail["stream_bytes"] != 26 || tail["returned_bytes"] != 13 {
		t.Fatalf("tail byte counts = %+v", tail)
	}
	if _, ok := tail["content_bytes"]; ok {
		t.Fatalf("tail output should not expose ambiguous content_bytes: %+v", tail)
	}
	if tail["raw_output_path"] != "artifacts/tool-outputs/call-1.json" {
		t.Fatalf("raw output path = %+v", tail["raw_output_path"])
	}

	slice, err := registry.Execute(ctx, "tool_output.read", map[string]any{
		"raw_output_path": filepath.ToSlash(rawRel),
		"stream":          "stdout",
		"offset_bytes":    6,
		"limit_bytes":     5,
	})
	if err != nil {
		t.Fatalf("tool_output.read slice error = %v", err)
	}
	if slice["content"] != "world" || slice["content_offset_bytes"] != 6 || slice["next_offset_bytes"] != 11 {
		t.Fatalf("slice output = %+v", slice)
	}
	if slice["stream_bytes"] != 23 || slice["returned_bytes"] != 5 {
		t.Fatalf("slice byte counts = %+v", slice)
	}
}

func TestToolOutputReadRejectsMissingRunContextAndOutsidePath(t *testing.T) {
	registry := NewBuiltinRegistry(t.TempDir(), []string{"tool_output.read"})
	_, err := registry.Execute(context.Background(), "tool_output.read", map[string]any{
		"raw_output_path": "artifacts/tool-outputs/call-1.json",
		"stream":          "stderr",
	})
	if err == nil || !strings.Contains(err.Error(), "active run context") {
		t.Fatalf("missing run context error = %v", err)
	}

	ctx := WithRunDir(context.Background(), t.TempDir())
	_, err = registry.Execute(ctx, "tool_output.read", map[string]any{
		"raw_output_path": "../outside.json",
		"stream":          "stderr",
	})
	if err == nil || !strings.Contains(err.Error(), "within the current run") {
		t.Fatalf("outside path error = %v", err)
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

func TestSearchFileFindsTextMatchesInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	writePath := filepath.Join(workspace, "kb", "index.md")
	if err := os.MkdirAll(filepath.Dir(writePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(writePath, []byte("第一行\n养生壹号是草本养生酒\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewBuiltinRegistry(workspace, []string{"search_file"})

	out, err := registry.Execute(context.Background(), "search_file", map[string]any{
		"query":       "养生壹号",
		"root":        "kb",
		"max_results": 5,
	})
	if err != nil {
		t.Fatalf("search_file error = %v", err)
	}
	if out["root"] != "kb" || out["match_count"] != 1 || out["total_matches"] != 1 {
		t.Fatalf("search output = %+v", out)
	}
	matches, ok := out["matches"].([]map[string]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("matches = %#v", out["matches"])
	}
	if matches[0]["path"] != "kb/index.md" || matches[0]["line"] != 2 {
		t.Fatalf("match = %+v", matches[0])
	}

	_, err = registry.Execute(context.Background(), "search_file", map[string]any{
		"query": "养生壹号",
		"root":  filepath.Dir(workspace),
	})
	if err == nil || !strings.Contains(err.Error(), "within workspace") {
		t.Fatalf("outside workspace error = %v", err)
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

func TestCommandRunRunsStructuredArgvWithoutShell(t *testing.T) {
	workspace := t.TempDir()
	registry := NewBuiltinRegistry(workspace, []string{"command.run"})

	out, err := registry.Execute(context.Background(), "command.run", map[string]any{
		"program": "printf",
		"args":    []any{"hello | cat"},
	})
	if err != nil {
		t.Fatalf("command.run error = %v output=%+v", err, out)
	}
	if out["stdout"] != "hello | cat" {
		t.Fatalf("stdout = %q", out["stdout"])
	}
}

func TestShellRunSupportsPipesAndRedirection(t *testing.T) {
	workspace := t.TempDir()
	registry := NewBuiltinRegistry(workspace, []string{"shell.run"})

	out, err := registry.Execute(context.Background(), "shell.run", map[string]any{
		"command": "printf hello > out.txt && cat out.txt | tr a-z A-Z",
	})
	if err != nil {
		t.Fatalf("shell.run error = %v output=%+v", err, out)
	}
	if out["stdout"] != "HELLO" {
		t.Fatalf("stdout = %q", out["stdout"])
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); err != nil {
		t.Fatalf("expected shell output file: %v", err)
	}
}
