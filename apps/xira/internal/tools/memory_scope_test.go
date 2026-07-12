package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentMemoryPath(t *testing.T) {
	stateDir := "/data/state"
	cases := []struct {
		name    string
		agentID string
		want    string
	}{
		{"normal", "xira-assistant", filepath.Join(stateDir, "memories", "agent_xira-assistant", "memory.jsonl")},
		{"unicode", "助理-A", filepath.Join(stateDir, "memories", "agent_助理-A", "memory.jsonl")},
		{"empty", "", ""},
		{"whitespace", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AgentMemoryPath(stateDir, tc.agentID); got != tc.want {
				t.Fatalf("AgentMemoryPath(%q) = %q, want %q", tc.agentID, got, tc.want)
			}
		})
	}
}

func TestMemoryScopeFromArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		want    memoryScope
		wantErr bool
	}{
		{name: "missing defaults sender", args: map[string]any{}, want: memoryScopeSender},
		{name: "sender", args: map[string]any{"scope": "sender"}, want: memoryScopeSender},
		{name: "agent", args: map[string]any{"scope": "agent"}, want: memoryScopeAgent},
		{name: "trimmed", args: map[string]any{"scope": " agent "}, want: memoryScopeAgent},
		{name: "unknown", args: map[string]any{"scope": "owner"}, wantErr: true},
		{name: "empty explicit", args: map[string]any{"scope": ""}, wantErr: true},
		{name: "non string", args: map[string]any{"scope": 1}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := memoryScopeFromArgs(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("memoryScopeFromArgs(%v) error = %v, wantErr %v", tc.args, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("memoryScopeFromArgs(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestMemoryPathForScopeContract(t *testing.T) {
	stateDir := t.TempDir()
	ctx := memCtxWithSender("ou_sender")
	cases := []struct {
		name    string
		ctx     context.Context
		agentID string
		scope   memoryScope
		want    string
		wantErr bool
	}{
		{name: "sender", ctx: ctx, agentID: "agent-a", scope: memoryScopeSender, want: MemoryPath(stateDir, "ou_sender")},
		{name: "sender missing", ctx: context.Background(), agentID: "agent-a", scope: memoryScopeSender, wantErr: true},
		{name: "agent", ctx: context.Background(), agentID: "agent-a", scope: memoryScopeAgent, want: AgentMemoryPath(stateDir, "agent-a")},
		{name: "agent missing", ctx: ctx, scope: memoryScopeAgent, wantErr: true},
		{name: "invalid", ctx: ctx, agentID: "agent-a", scope: memoryScope("owner"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := memoryPathForScope(tc.ctx, stateDir, tc.agentID, tc.scope)
			if (err != nil) != tc.wantErr {
				t.Fatalf("memoryPathForScope() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("memoryPathForScope() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMemoryToolSchemasExposeOptionalSealedScope(t *testing.T) {
	for _, tool := range []Tool{NewUpdateMemoryTool("/tmp"), NewForgetMemoryTool("/tmp")} {
		params := tool.Parameters()
		properties, ok := params["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s properties = %#v", tool.Name(), params["properties"])
		}
		scope, ok := properties["scope"].(map[string]any)
		if !ok {
			t.Fatalf("%s missing scope schema", tool.Name())
		}
		enum, ok := scope["enum"].([]any)
		if !ok || len(enum) != 2 || enum[0] != "sender" || enum[1] != "agent" {
			t.Fatalf("%s scope enum = %#v", tool.Name(), scope["enum"])
		}
		required, _ := params["required"].([]string)
		for _, field := range required {
			if field == "scope" {
				t.Fatalf("%s scope must remain optional for backward compatibility", tool.Name())
			}
		}
		// The ADK path converts the map schema before exposing it to the model.
		// Pin that conversion so the sealed scope enum cannot disappear between
		// the tool and the production model request.
		converted := SchemaFromMap(params)
		scopeSchema := converted.Properties["scope"]
		if scopeSchema == nil || len(scopeSchema.Enum) != 2 || scopeSchema.Enum[0] != "sender" || scopeSchema.Enum[1] != "agent" {
			t.Fatalf("%s converted scope schema = %#v", tool.Name(), scopeSchema)
		}
	}
}

func TestUpdateMemoryTool_AgentScopeSharedAcrossSenders(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistryForAgent("", []string{"update_memory"}, SandboxRoots{}, stateDir, "agent-a")

	if _, err := reg.Execute(memCtxWithSender("ou_alice"), "update_memory", map[string]any{
		"scope": "agent", "key": "release", "content": "alice asked me to watch release",
	}); err != nil {
		t.Fatalf("alice agent memory: %v", err)
	}
	if _, err := reg.Execute(memCtxWithSender("ou_bob"), "update_memory", map[string]any{
		"scope": "agent", "key": "release", "content": "bob added CI context",
	}); err != nil {
		t.Fatalf("bob agent memory: %v", err)
	}

	agentEntries, err := LoadMemories(AgentMemoryPath(stateDir, "agent-a"))
	if err != nil || len(agentEntries) != 1 || agentEntries[0].Content != "bob added CI context" {
		t.Fatalf("shared agent memory = %+v, err=%v", agentEntries, err)
	}
	for _, sender := range []string{"ou_alice", "ou_bob"} {
		entries, loadErr := LoadMemories(MemoryPath(stateDir, sender))
		if loadErr != nil || len(entries) != 0 {
			t.Fatalf("agent scope polluted sender %s: %+v, err=%v", sender, entries, loadErr)
		}
	}
}

func TestUpdateMemoryTool_AgentScopeIsolatedAcrossAgents(t *testing.T) {
	stateDir := t.TempDir()
	for _, tc := range []struct {
		agentID string
		content string
	}{{"agent-a", "A memory"}, {"agent-b", "B memory"}} {
		reg := NewBuiltinRegistryForAgent("", []string{"update_memory"}, SandboxRoots{}, stateDir, tc.agentID)
		if _, err := reg.Execute(context.Background(), "update_memory", map[string]any{
			"scope": "agent", "key": "same", "content": tc.content,
		}); err != nil {
			t.Fatalf("write %s: %v", tc.agentID, err)
		}
	}
	for _, tc := range []struct {
		agentID string
		content string
	}{{"agent-a", "A memory"}, {"agent-b", "B memory"}} {
		entries, err := LoadMemories(AgentMemoryPath(stateDir, tc.agentID))
		if err != nil || len(entries) != 1 || entries[0].Content != tc.content {
			t.Fatalf("%s entries = %+v, err=%v", tc.agentID, entries, err)
		}
	}
}

func TestMemoryTools_InvalidOrUnavailableScopeFailsClosed(t *testing.T) {
	stateDir := t.TempDir()
	legacy := NewBuiltinRegistry("", []string{"update_memory", "forget_memory"}, SandboxRoots{}, stateDir)
	ctx := memCtxWithSender("ou_a")
	for _, tool := range []string{"update_memory", "forget_memory"} {
		args := map[string]any{"scope": "owner", "key": "x", "content": "y"}
		if _, err := legacy.Execute(ctx, tool, args); err == nil {
			t.Fatalf("%s accepted unknown scope", tool)
		}
		args["scope"] = "agent"
		if _, err := legacy.Execute(ctx, tool, args); err == nil {
			t.Fatalf("%s accepted agent scope without bound agent identity", tool)
		}
	}
	if entries, err := LoadMemories(MemoryPath(stateDir, "ou_a")); err != nil || len(entries) != 0 {
		t.Fatalf("failed scoped calls wrote sender memory: %+v, err=%v", entries, err)
	}
}

func TestForgetMemoryTool_OnlyForgetsSelectedScope(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistryForAgent("", []string{"update_memory", "forget_memory"}, SandboxRoots{}, stateDir, "agent-a")
	ctx := memCtxWithSender("ou_a")
	for _, scope := range []string{"sender", "agent"} {
		if _, err := reg.Execute(ctx, "update_memory", map[string]any{
			"scope": scope, "key": "release", "content": scope + " memory",
		}); err != nil {
			t.Fatalf("write %s: %v", scope, err)
		}
	}
	if _, err := reg.Execute(ctx, "forget_memory", map[string]any{"scope": "agent", "key": "release"}); err != nil {
		t.Fatalf("forget agent: %v", err)
	}
	senderEntries, _ := LoadMemories(MemoryPath(stateDir, "ou_a"))
	agentEntries, _ := LoadMemories(AgentMemoryPath(stateDir, "agent-a"))
	if len(senderEntries) != 1 || senderEntries[0].Status != memoryStatusActive {
		t.Fatalf("sender memory changed: %+v", senderEntries)
	}
	if len(agentEntries) != 1 || agentEntries[0].Status != memoryStatusForgotten {
		t.Fatalf("agent memory not forgotten: %+v", agentEntries)
	}
}

func TestMemoryStorePropagatesFilesystemAndScannerFailures(t *testing.T) {
	if _, err := LoadMemories("\x00"); err == nil {
		t.Fatal("LoadMemories must propagate invalid-path open errors")
	}
	oversized := filepath.Join(t.TempDir(), "oversized.jsonl")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", 1024*1024+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMemories(oversized); err == nil {
		t.Fatal("LoadMemories must return scanner error for an oversized jsonl line")
	}
	blankAndValid := filepath.Join(t.TempDir(), "blank.jsonl")
	if err := os.WriteFile(blankAndValid, []byte("\n  \n"+`{"id":"m1","key":"k","content":"v","created":"2026-01-01T00:00:00Z","updated":"2026-01-01T00:00:00Z","status":"active"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := LoadMemories(blankAndValid)
	if err != nil || len(entries) != 1 {
		t.Fatalf("blank lines should be skipped without losing valid entries: %+v err=%v", entries, err)
	}

	for name, operation := range map[string]func() error{
		"upsert": func() error { return upsertMemory("\x00", "k", "v", nil) },
		"forget": func() error { return forgetMemory("\x00", "k") },
		"active": func() error { _, err := ActiveMemories("\x00"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); err == nil {
				t.Fatalf("%s must propagate load error", name)
			}
		})
	}
}

func TestSaveMemoriesRejectsDirectoryAsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveMemories(path, []MemoryEntry{{ID: "m1", Key: "k", Content: "v"}}); err == nil {
		t.Fatal("saveMemories must reject a directory at the target file path")
	}
}

func TestScopedMemoryToolsPropagateStoreFailures(t *testing.T) {
	reg := NewBuiltinRegistryForAgent("", []string{"update_memory", "forget_memory"}, SandboxRoots{}, "\x00", "agent-a")
	if _, err := reg.Execute(context.Background(), "update_memory", map[string]any{
		"scope": "agent", "key": "k", "content": "v",
	}); err == nil {
		t.Fatal("update_memory must propagate Agent memory store failure")
	}
	if _, err := reg.Execute(context.Background(), "forget_memory", map[string]any{
		"scope": "agent", "key": "k",
	}); err == nil {
		t.Fatal("forget_memory must propagate Agent memory store failure")
	}
}
