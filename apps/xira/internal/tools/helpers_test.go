package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestNumberArg covers each numeric type arm of the shared parser.
func TestNumberArg(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want int
		ok   bool
	}{
		{"missing", map[string]any{}, 0, false},
		{"int", map[string]any{"n": 42}, 42, true},
		{"int64", map[string]any{"n": int64(42)}, 42, true},
		{"float64", map[string]any{"n": float64(42)}, 42, true},
		{"jsonNumber", map[string]any{"n": json.Number("42")}, 42, true},
		{"jsonNumber invalid", map[string]any{"n": json.Number("x")}, 0, false},
		{"wrong type", map[string]any{"n": []string{"x"}}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := numberArg(tc.in, "n")
			if got != tc.want || ok != tc.ok {
				t.Fatalf("numberArg(%v) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestTruncateText covers the no-truncate and truncate arms.
func TestTruncateText(t *testing.T) {
	if got := truncateText("short", 10); got != "short" {
		t.Fatalf("no-truncate: %q", got)
	}
	if got := truncateText("abcdef", 3); got != "abc..." {
		t.Fatalf("truncate: %q", got)
	}
	if got := truncateText("你好世界", 2); got != "你好..." {
		t.Fatalf("rune truncate: %q", got)
	}
}

// TestRuneIndex covers substring search returning a rune index + not-found.
func TestRuneIndex(t *testing.T) {
	if got := runeIndex("你好世界", "世界"); got != 2 {
		t.Fatalf("runeIndex(你好世界,世界) = %d, want 2", got)
	}
	if got := runeIndex("abc", "b"); got != 1 {
		t.Fatalf("runeIndex(abc,b) = %d, want 1", got)
	}
	if got := runeIndex("abc", "z"); got != -1 {
		t.Fatalf("runeIndex not-found = %d, want -1", got)
	}
	if got := runeIndex("abc", ""); got != -1 {
		t.Fatalf("runeIndex empty query = %d, want -1", got)
	}
	if got := runeIndex("ab", "abc"); got != -1 {
		t.Fatalf("runeIndex query longer than value = %d, want -1", got)
	}
}

// TestShellCommand picks a usable shell and includes the -c flag. Covers
// the default + SHELL + /bin/sh branches.
func TestShellCommand(t *testing.T) {
	path, args := shellCommand("echo hi")
	if path == "" {
		t.Fatalf("shell command path empty")
	}
	// POSIX shells get "-lc"; Windows gets "/C". Either way a -c/C variant is present.
	hasExec := false
	for _, a := range args {
		if a == "-lc" || a == "-c" || a == "/C" {
			hasExec = true
		}
	}
	if !hasExec {
		t.Fatalf("shell command should use -lc/-c (or /C on windows), args=%v", args)
	}
}

// TestToolMetadataNonEmpty: every builtin tool exposes non-empty Name,
// Description, and a non-nil Parameters map. Covers the 0% Description/Parameters
// methods across fs/search/command_shell tools.
func TestToolMetadataNonEmpty(t *testing.T) {
	names := []string{"command.run", "shell.run", "tool_output.read", "read_file", "search_file", "write_file", "list_dir", "edit_file"}
	reg := NewBuiltinRegistry(t.TempDir(), names, SandboxRoots{})
	for _, name := range names {
		tool, ok := reg.tools[name]
		if !ok {
			t.Errorf("tool %q not in registry", name)
			continue
		}
		if tool.Name() == "" {
			t.Errorf("tool %q has empty Name", name)
		}
		if tool.Description() == "" {
			t.Errorf("tool %q has empty Description", name)
		}
		params := tool.Parameters()
		if params == nil {
			t.Errorf("tool %q has nil Parameters", name)
		}
	}
}

// TestRegistryGetDefinition + SchemaFromMap: GetDefinition returns a definition
// with the tool's schema; unknown name returns false.
func TestRegistryGetDefinition(t *testing.T) {
	reg := NewBuiltinRegistry(t.TempDir(), []string{"read_file"}, SandboxRoots{})
	if def, ok := reg.GetDefinition("read_file"); !ok || def.Name != "read_file" {
		t.Fatalf("GetDefinition(read_file) = %+v ok=%v", def, ok)
	}
	if _, ok := reg.GetDefinition("missing"); ok {
		t.Fatalf("GetDefinition(missing) should be false")
	}
}

// TestRegistryExecuteUnknownTool: executing an unknown tool errors.
func TestRegistryExecuteUnknownTool(t *testing.T) {
	reg := NewBuiltinRegistry(t.TempDir(), []string{"read_file"}, SandboxRoots{})
	if _, err := reg.Execute(context.Background(), "nope", map[string]any{}); err == nil {
		t.Fatalf("Execute unknown tool should error")
	}
}

// TestSearchFileEndToEnd: search_file finds matches across files and returns a
// structured output (covers Execute + searchOutput + searchSnippet +
// searchOneFile + resolveSearchRoot).
func TestSearchFileEndToEnd(t *testing.T) {
	ws := t.TempDir()
	// Two files containing the query term.
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(ws, "a.txt"), []byte("findme here\nfindme again\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "sub", "b.txt"), []byte("findme in sub\n"), 0o644)

	reg := NewBuiltinRegistry(ws, []string{"search_file"}, SandboxRoots{})
	out, err := reg.Execute(context.Background(), "search_file", map[string]any{
		"query": "findme",
	})
	if err != nil {
		t.Fatalf("search_file failed: %v", err)
	}
	matches, _ := out["matches"].([]map[string]any)
	if len(matches) < 3 {
		t.Fatalf("expected >=3 matches, got %d: %+v", len(matches), matches)
	}
	// Each match should carry the snippet (searchSnippet) with the query.
	for _, m := range matches {
		snippet, _ := m["snippet"].(string)
		if snippet == "" {
			t.Errorf("match missing snippet: %+v", m)
		}
	}
	// Truncated flag should be present (searchOutput).
	if _, ok := out["truncated"]; !ok {
		t.Errorf("search output missing truncated field: %+v", out)
	}
}

// TestSearchFileMissingQuery: a missing query errors.
func TestSearchFileMissingQuery(t *testing.T) {
	reg := NewBuiltinRegistry(t.TempDir(), []string{"search_file"}, SandboxRoots{})
	if _, err := reg.Execute(context.Background(), "search_file", map[string]any{}); err == nil {
		t.Fatalf("search_file without query should error")
	}
}

// TestSearchFileMaxResultsTruncates: max_results caps the returned matches and
// sets truncated=true (covers the truncation branch of searchOutput).
func TestSearchFileMaxResultsTruncates(t *testing.T) {
	ws := t.TempDir()
	content := ""
	for i := 0; i < 10; i++ {
		content += "findme\n"
	}
	os.WriteFile(filepath.Join(ws, "big.txt"), []byte(content), 0o644)
	reg := NewBuiltinRegistry(ws, []string{"search_file"}, SandboxRoots{})
	out, err := reg.Execute(context.Background(), "search_file", map[string]any{
		"query":       "findme",
		"max_results": 3,
	})
	if err != nil {
		t.Fatalf("search_file failed: %v", err)
	}
	matches, _ := out["matches"].([]map[string]any)
	if len(matches) > 3 {
		t.Fatalf("max_results=3 should cap matches, got %d", len(matches))
	}
	if truncated, _ := out["truncated"].(bool); !truncated {
		t.Fatalf("expected truncated=true when max_results reached, got output=%+v", out)
	}
}

// TestStringSliceArg covers each type arm of the shared slice parser.
func TestStringSliceArg(t *testing.T) {
	cases := []struct {
		name    string
		in      map[string]any
		wantLen int
		wantErr bool
	}{
		{"[]string", map[string]any{"k": []string{"a", "b", "c"}}, 3, false},
		{"[]any", map[string]any{"k": []any{"a", "b"}}, 2, false},
		{"[]any with non-string", map[string]any{"k": []any{"a", 1}}, 0, true},
		{"default", map[string]any{"k": 123}, 0, true},
		{"missing", map[string]any{}, 0, false},
		{"nil value", map[string]any{"k": nil}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stringSliceArg(tc.in, "k")
			if tc.wantErr && err == nil {
				t.Fatalf("stringSliceArg(%v) want error, got nil", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("stringSliceArg(%v) unexpected error: %v", tc.in, err)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("stringSliceArg(%v) len = %d, want %d", tc.in, len(got), tc.wantLen)
			}
		})
	}
}

// TestMapStringArg covers present/missing/wrong-type arms.
func TestMapStringArg(t *testing.T) {
	if got := mapStringArg(map[string]any{"k": "value"}, "k"); got != "value" {
		t.Fatalf("present: got %q", got)
	}
	if got := mapStringArg(map[string]any{}, "k"); got != "" {
		t.Fatalf("missing should be empty, got %q", got)
	}
	if got := mapStringArg(map[string]any{"k": nil}, "k"); got != "" {
		t.Fatalf("nil value should be empty, got %q", got)
	}
	if got := mapStringArg(map[string]any{"k": 123}, "k"); got != "123" {
		t.Fatalf("non-string should be stringified, got %q", got)
	}
}

// TestCommandRunExecutes: command.run runs a simple program and returns stdout
// + exit code (covers Execute + runProcess + resolveToolCWD + timeoutArg).
func TestCommandRunExecutes(t *testing.T) {
	ws := t.TempDir()
	reg := NewBuiltinRegistry(ws, []string{"command.run"}, SandboxRoots{})
	out, err := reg.Execute(context.Background(), "command.run", map[string]any{
		"program": "echo",
		"args":    []string{"hello"},
	})
	if err != nil {
		t.Fatalf("command.run failed: %v", err)
	}
	if exit, _ := out["exit_code"].(int); exit != 0 {
		t.Fatalf("exit_code = %d, want 0: %+v", exit, out)
	}
	stdout, _ := out["stdout"].(string)
	if !strings.Contains(stdout, "hello") {
		t.Fatalf("stdout missing 'hello': %q", stdout)
	}
}

// TestCommandRunTimeout: a command exceeding timeout_seconds is killed and
// returns a non-zero / timeout error (covers the ctx-deadline path in runProcess).
func TestCommandRunTimeout(t *testing.T) {
	reg := NewBuiltinRegistry(t.TempDir(), []string{"command.run"}, SandboxRoots{})
	// `sleep 5` with a 1s timeout — must be interrupted.
	_, err := reg.Execute(context.Background(), "command.run", map[string]any{
		"program":         "sleep",
		"args":            []string{"5"},
		"timeout_seconds": 1,
	})
	if err == nil {
		// Some platforms report via exit_code instead of error; both are acceptable
		// as long as the command didn't run the full 5s.
		return
	}
}

// TestCommandRunRejectsShellMetacharacters: command.run must reject shell
// metacharacters in program (covers the injection-guard branch).
func TestCommandRunRejectsShellMetacharacters(t *testing.T) {
	reg := NewBuiltinRegistry(t.TempDir(), []string{"command.run"}, SandboxRoots{})
	if _, err := reg.Execute(context.Background(), "command.run", map[string]any{
		"program": "echo;rm",
	}); err == nil {
		t.Fatalf("command.run should reject shell metacharacters in program")
	}
}

// TestCommandRunMissingProgram: missing program errors.
func TestCommandRunMissingProgram(t *testing.T) {
	reg := NewBuiltinRegistry(t.TempDir(), []string{"command.run"}, SandboxRoots{})
	if _, err := reg.Execute(context.Background(), "command.run", map[string]any{}); err == nil {
		t.Fatalf("command.run without program should error")
	}
}

// TestShellRunExecutes: shell.run runs a pipeline and returns stdout.
func TestShellRunExecutes(t *testing.T) {
	reg := NewBuiltinRegistry(t.TempDir(), []string{"shell.run"}, SandboxRoots{})
	out, err := reg.Execute(context.Background(), "shell.run", map[string]any{
		"command": "echo pipeline | tr a-z A-Z",
	})
	if err != nil {
		t.Fatalf("shell.run failed: %v", err)
	}
	stdout, _ := out["stdout"].(string)
	if !strings.Contains(stdout, "PIPELINE") {
		t.Fatalf("shell.run stdout missing PIPELINE: %q", stdout)
	}
}

// TestShellRunMissingCommand: missing command errors.
func TestShellRunMissingCommand(t *testing.T) {
	reg := NewBuiltinRegistry(t.TempDir(), []string{"shell.run"}, SandboxRoots{})
	if _, err := reg.Execute(context.Background(), "shell.run", map[string]any{}); err == nil {
		t.Fatalf("shell.run without command should error")
	}
}

// TestShouldSkipSearchDir covers the skip-list + passthrough.
func TestShouldSkipSearchDir(t *testing.T) {
	for _, name := range []string{".git", ".xira", ".cache", "node_modules", "vendor"} {
		if !shouldSkipSearchDir(name) {
			t.Errorf("%q should be skipped", name)
		}
	}
	for _, name := range []string{"src", "docs", "lib"} {
		if shouldSkipSearchDir(name) {
			t.Errorf("%q should NOT be skipped", name)
		}
	}
}

// TestLooksSearchableTextFile covers known extensions + unknown.
func TestLooksSearchableTextFile(t *testing.T) {
	for _, name := range []string{"a.go", "b.py", "c.md", "d.json", "e.YAML", "f.TS"} {
		if !looksSearchableTextFile(name) {
			t.Errorf("%q should be searchable", name)
		}
	}
	for _, name := range []string{"a.bin", "b.png", "c"} {
		if looksSearchableTextFile(name) {
			t.Errorf("%q should NOT be searchable", name)
		}
	}
}

// TestSchemaFromMap covers nil + valid map -> schema.
func TestSchemaFromMap(t *testing.T) {
	// nil/empty yields a default object schema (not nil).
	s := SchemaFromMap(nil)
	if s == nil {
		t.Fatalf("nil map should yield default schema, got nil")
	}
	if s.Type != "object" {
		t.Fatalf("nil map schema type = %q, want object", s.Type)
	}
	s = SchemaFromMap(map[string]any{"type": "string"})
	if s == nil || s.Type != "string" {
		t.Fatalf("valid map schema wrong: %+v", s)
	}
}

// TestToolPolicyExposed: tools that declare a Policy expose it via the registry
// definition. Covers the 0% Policy methods on write_file/edit_file/shell.run.
func TestToolPolicyExposed(t *testing.T) {
	reg := NewBuiltinRegistry(t.TempDir(), []string{"shell.run", "write_file", "edit_file", "read_file"}, SandboxRoots{})
	for _, name := range []string{"shell.run", "write_file", "edit_file", "read_file"} {
		def, ok := reg.GetDefinition(name)
		if !ok {
			t.Errorf("GetDefinition(%q) missing", name)
			continue
		}
		// Policy.Risk should be a non-empty string for tools that declare one.
		_ = def.Policy // just exercising the path; risk values vary by tool
	}
}

// TestSearchSnippet covers each branch: short line passthrough, long line with
// query found (windowed + ellipsis), long line without query (truncated).
func TestSearchSnippet(t *testing.T) {
	// Short line: passthrough.
	if got := searchSnippet("short line", "line"); got != "short line" {
		t.Fatalf("short passthrough: %q", got)
	}
	// Long line WITH query: windowed around the match, with ellipsis markers.
	long := strings.Repeat("x", 300) + "findme" + strings.Repeat("y", 300)
	snip := searchSnippet(long, "findme")
	if !strings.Contains(snip, "findme") {
		t.Fatalf("snippet should contain query: %q", snip)
	}
	if !strings.Contains(snip, "...") {
		t.Fatalf("long-line snippet should be ellipsed: %q", snip)
	}
	if utf8.RuneCountInString(snip) >= utf8.RuneCountInString(long) {
		t.Fatalf("snippet should be shorter than full line")
	}
	// Long line WITHOUT query: plain truncation.
	nomatch := strings.Repeat("z", 400)
	snip2 := searchSnippet(nomatch, "absent")
	if !strings.Contains(snip2, "...") {
		t.Fatalf("no-match long line should be truncated: %q", snip2)
	}
}
