package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

// TestWriteJSONFile covers dir creation + write + marshal error arm.
func TestWriteJSONFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "out.json")
	if err := writeJSONFile(p, map[string]any{"a": 1}); err != nil {
		t.Fatalf("writeJSONFile failed: %v", err)
	}
	// Marshal error: a channel cannot be JSON-marshalled.
	if err := writeJSONFile(filepath.Join(dir, "bad.json"), func() {}); err == nil {
		t.Fatalf("expected marshal error for un-marshallable value")
	}
}

// TestStreamBytes covers the present-key arm and the fallback-to-len arm.
func TestStreamBytes(t *testing.T) {
	if got := streamBytes(map[string]any{"stdout_bytes": 42}, "stdout", "hello"); got != 42 {
		t.Fatalf("explicit bytes should win, got %d", got)
	}
	if got := streamBytes(map[string]any{}, "stdout", "hello"); got != 5 {
		t.Fatalf("fallback should be len(value), got %d", got)
	}
}

// TestRepeatedToolFailureOutput covers both the command.run branch and the else
// branch.
func TestRepeatedToolFailureOutput(t *testing.T) {
	out := repeatedToolFailureOutput("command.run", map[string]any{"program": "ls", "args": []string{"-l"}})
	if out["status"] != "error" || out["program"] != "ls" {
		t.Fatalf("command.run output wrong: %v", out)
	}
	if _, ok := out["command"]; ok {
		t.Fatalf("command.run should not set command field")
	}
	out2 := repeatedToolFailureOutput("read_file", map[string]any{"command": "cat"})
	if out2["command"] != "cat" {
		t.Fatalf("non-command tool should set command field: %v", out2)
	}
	if _, ok := out2["program"]; ok {
		t.Fatalf("non-command tool should not set program field")
	}
}

// TestValidateRuntimeToolInputAllowlist covers: no allowlist (nil), tool not in
// allowlist, required field missing, value not allowed, valid value.
func TestValidateRuntimeToolInputAllowlist(t *testing.T) {
	// No allowlist in ctx -> always valid.
	if err := validateRuntimeToolInputAllowlist(context.Background(), "any", map[string]any{}); err != nil {
		t.Fatalf("no allowlist should pass: %v", err)
	}
	allowlist := map[string]map[string][]string{
		"command.run": {"program": {"ls", "cat"}},
	}
	ctx := contextWithRuntimeToolInputAllowlist(context.Background(), allowlist)
	// Missing required field.
	if err := validateRuntimeToolInputAllowlist(ctx, "command.run", map[string]any{}); err == nil {
		t.Fatalf("missing required field should error")
	}
	// Disallowed value.
	if err := validateRuntimeToolInputAllowlist(ctx, "command.run", map[string]any{"program": "rm"}); err == nil {
		t.Fatalf("disallowed value should error")
	}
	// Allowed value.
	if err := validateRuntimeToolInputAllowlist(ctx, "command.run", map[string]any{"program": "ls"}); err != nil {
		t.Fatalf("allowed value should pass: %v", err)
	}
	// Tool not in allowlist -> pass.
	if err := validateRuntimeToolInputAllowlist(ctx, "other.tool", map[string]any{}); err != nil {
		t.Fatalf("tool not in allowlist should pass: %v", err)
	}
}

// TestConstrainedToolParameters covers: no allowlist (passthrough), enum
// injection on properties.
func TestConstrainedToolParameters(t *testing.T) {
	params := map[string]any{"properties": map[string]any{"program": map[string]any{"type": "string"}}}
	// No allowlist -> returned as-is (deep-cloned).
	out := constrainedToolParameters(context.Background(), "command.run", params)
	props, _ := out["properties"].(map[string]any)
	if prog, _ := props["program"].(map[string]any); prog["enum"] != nil {
		t.Fatalf("no allowlist should not inject enum")
	}
	allowlist := map[string]map[string][]string{
		"command.run": {"program": {"cat", "ls"}},
	}
	ctx := contextWithRuntimeToolInputAllowlist(context.Background(), allowlist)
	out = constrainedToolParameters(ctx, "command.run", params)
	props, _ = out["properties"].(map[string]any)
	prog, _ := props["program"].(map[string]any)
	enum, ok := prog["enum"].([]string)
	if !ok || len(enum) != 2 {
		t.Fatalf("enum not injected correctly: %v", prog["enum"])
	}
}
