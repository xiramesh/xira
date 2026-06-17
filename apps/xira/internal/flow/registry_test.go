package flow

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestLoadFromWorkspaceDiscoversFlows asserts that flows placed under
// <workspace>/flows/<id>/flow.yaml are discovered, sorted, and resolvable by id.
func TestLoadFromWorkspaceDiscoversFlows(t *testing.T) {
	root := t.TempDir()
	writeRegistryFlowFile(t, filepath.Join(root, "flows", "hello", "flow.yaml"), "hello", "")
	writeRegistryFlowFile(t, filepath.Join(root, "flows", "world", "flow.yaml"), "world", "")

	reg, err := LoadFromWorkspace(root)
	if err != nil {
		t.Fatalf("LoadFromWorkspace: %v", err)
	}
	refs := reg.List()
	if len(refs) != 2 {
		t.Fatalf("list len = %d, want 2: %+v", len(refs), refs)
	}
	if refs[0].ID != "hello" || refs[1].ID != "world" {
		t.Fatalf("order = %q, %q; want hello, world", refs[0].ID, refs[1].ID)
	}
	def, err := reg.Definition("hello")
	if err != nil {
		t.Fatalf("Definition(hello): %v", err)
	}
	if def.ID != "hello" {
		t.Fatalf("def.ID = %q, want hello", def.ID)
	}
}

// TestLoadFromWorkspaceRejectsDirNameMismatch mirrors agents/loader.go:93:
// the directory name must equal the flow definition's id.
func TestLoadFromWorkspaceRejectsDirNameMismatch(t *testing.T) {
	root := t.TempDir()
	writeRegistryFlowFile(t, filepath.Join(root, "flows", "hello", "flow.yaml"), "other", "")
	if _, err := LoadFromWorkspace(root); err == nil {
		t.Fatal("expected dir-name/id mismatch error, got nil")
	}
}

// TestLoadFromWorkspaceCaseInsensitiveFileName mirrors PROFILE.md discovery:
// a differently-cased file name (Flow.YAML) must still be discovered.
func TestLoadFromWorkspaceCaseInsensitiveFileName(t *testing.T) {
	root := t.TempDir()
	writeRegistryFlowFile(t, filepath.Join(root, "flows", "hello", "Flow.YAML"), "hello", "")
	reg, err := LoadFromWorkspace(root)
	if err != nil {
		t.Fatalf("LoadFromWorkspace: %v", err)
	}
	def, err := reg.Definition("hello")
	if err != nil {
		t.Fatalf("Definition(hello): %v", err)
	}
	if def.ID != "hello" {
		t.Fatalf("def.ID = %q", def.ID)
	}
}

// TestLoadFromWorkspaceAmbiguousCaseIsError mirrors fileutil/case.go:33:
// two case variants of the same canonical name must be a hard error, not a
// silent pick. This requires a case-sensitive filesystem to construct, so we
// skip on case-insensitive hosts (e.g. default macOS APFS) where the two
// variants collapse into one directory entry.
func TestLoadFromWorkspaceAmbiguousCaseIsError(t *testing.T) {
	root := t.TempDir()
	flowDir := filepath.Join(root, "flows", "hello")
	if err := os.MkdirAll(flowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRegistryFlowFile(t, filepath.Join(flowDir, "flow.yaml"), "hello", "")
	writeRegistryFlowFile(t, filepath.Join(flowDir, "Flow.YAML"), "hello", "")

	// Detect whether the host FS actually preserved two distinct entries. On a
	// case-insensitive FS the second write overwrites the first, so there is no
	// ambiguity to assert here.
	entries, err := os.ReadDir(flowDir)
	if err != nil {
		t.Fatal(err)
	}
	var matches int
	for _, e := range entries {
		if equalFoldASCII(e.Name(), flowDefinitionFileName) {
			matches++
		}
	}
	if matches < 2 {
		t.Skipf("host filesystem is case-insensitive: only %d flow.yaml entry, cannot reproduce ambiguity", matches)
	}
	if _, err := LoadFromWorkspace(root); err == nil {
		t.Fatal("expected ambiguous case error, got nil")
	}
}

// equalFoldASCII is a minimal ASCII case-insensitive compare used only by the
// ambiguity probe above (we avoid importing fileutil's private internals).
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// TestLoadFromWorkspaceEmptyReturnsEmptyRegistry differs from agents: a missing
// or empty flows directory is allowed (not every workspace has flows) and
// returns an empty registry instead of erroring.
func TestLoadFromWorkspaceEmptyReturnsEmptyRegistry(t *testing.T) {
	root := t.TempDir()
	reg, err := LoadFromWorkspace(root)
	if err != nil {
		t.Fatalf("LoadFromWorkspace on missing flows dir: %v", err)
	}
	if len(reg.List()) != 0 {
		t.Fatalf("expected empty list, got %+v", reg.List())
	}
	if _, err := reg.Definition("anything"); err == nil {
		t.Fatal("expected not-found error for empty registry")
	}
}

// TestLoadFromWorkspaceCarriesDescription asserts that the FlowRef surfaces the
// description from the flow definition file (single source of truth).
func TestLoadFromWorkspaceCarriesDescription(t *testing.T) {
	root := t.TempDir()
	writeRegistryFlowFile(t, filepath.Join(root, "flows", "hello", "flow.yaml"), "hello", "A hello flow")
	reg, err := LoadFromWorkspace(root)
	if err != nil {
		t.Fatalf("LoadFromWorkspace: %v", err)
	}
	refs := reg.List()
	if len(refs) != 1 {
		t.Fatalf("list len = %d, want 1", len(refs))
	}
	if refs[0].Description != "A hello flow" {
		t.Fatalf("description = %q, want %q", refs[0].Description, "A hello flow")
	}
	if refs[0].Name != "hello" {
		t.Fatalf("name = %q, want hello", refs[0].Name)
	}
}

// TestLoadFromWorkspaceSkipsDirsWithoutFlowFile asserts that sibling
// directories without a flow.yaml are ignored (agents loader does the same).
func TestLoadFromWorkspaceSkipsDirsWithoutFlowFile(t *testing.T) {
	root := t.TempDir()
	writeRegistryFlowFile(t, filepath.Join(root, "flows", "hello", "flow.yaml"), "hello", "")
	// a directory without flow.yaml
	if err := os.MkdirAll(filepath.Join(root, "flows", "just-a-folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a non-directory entry (regular file) at the flows root must be ignored
	if err := os.WriteFile(filepath.Join(root, "flows", "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := LoadFromWorkspace(root)
	if err != nil {
		t.Fatalf("LoadFromWorkspace: %v", err)
	}
	refs := reg.List()
	if len(refs) != 1 || refs[0].ID != "hello" {
		t.Fatalf("refs = %+v, want [hello]", refs)
	}
}

// TestFlowRegistryFind asserts Find returns ok only for known ids.
func TestFlowRegistryFind(t *testing.T) {
	root := t.TempDir()
	writeRegistryFlowFile(t, filepath.Join(root, "flows", "hello", "flow.yaml"), "hello", "")
	reg, err := LoadFromWorkspace(root)
	if err != nil {
		t.Fatalf("LoadFromWorkspace: %v", err)
	}
	if ref, ok := reg.Find("hello"); !ok || ref.ID != "hello" {
		t.Fatalf("Find(hello) = %+v ok=%v", ref, ok)
	}
	if _, ok := reg.Find("missing"); ok {
		t.Fatal("Find(missing) should be false")
	}
}

// TestLoadFromWorkspaceListIsSorted asserts List returns refs in a stable,
// sorted order regardless of filesystem ordering.
func TestLoadFromWorkspaceListIsSorted(t *testing.T) {
	root := t.TempDir()
	ids := []string{"zeta", "alpha", "mike"}
	for _, id := range ids {
		writeRegistryFlowFile(t, filepath.Join(root, "flows", id, "flow.yaml"), id, "")
	}
	reg, err := LoadFromWorkspace(root)
	if err != nil {
		t.Fatalf("LoadFromWorkspace: %v", err)
	}
	got := reg.List()
	if len(got) != len(ids) {
		t.Fatalf("len = %d, want %d", len(got), len(ids))
	}
	gotIDs := make([]string, len(got))
	for i, r := range got {
		gotIDs[i] = r.ID
	}
	want := append([]string(nil), ids...)
	sort.Strings(want)
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("order = %v, want %v", gotIDs, want)
		}
	}
}

// writeRegistryFlowFile writes a minimal valid flow.yaml with the given id at
// path, optionally with a description. It mirrors the structure agents expect:
// <workspace>/flows/<id>/flow.yaml.
func writeRegistryFlowFile(t *testing.T, path, id, description string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	content := "schema_version: xira.flow.v0\n" +
		"id: " + id + "\n" +
		"name: " + id + "\n" +
		"version: 0.1.0\n"
	if description != "" {
		content += "description: " + description + "\n"
	}
	content += "entrypoints:\n" +
		"  - id: ad_hoc\n" +
		"    start_step: answer\n" +
		"steps:\n" +
		"  - id: answer\n" +
		"    objective: Answer.\n" +
		"    executor:\n" +
		"      agent: xira-assistant\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
