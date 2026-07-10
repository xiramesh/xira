package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/chatkey"
)

// MemoryPath 算出 sender 的 memory.jsonl 路径（#128）。

func TestMemoryPath(t *testing.T) {
	stateDir := "/data/state"
	cases := []struct {
		name     string
		senderID string
		want     string
	}{
		{"normal", "ou_大明", filepath.Join(stateDir, "memories", "sender_ou_大明", "memory.jsonl")},
		{"ascii", "wxid_abc", filepath.Join(stateDir, "memories", "sender_wxid_abc", "memory.jsonl")},
		{"empty sender", "", ""},
		{"whitespace", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MemoryPath(stateDir, tc.senderID)
			if got != tc.want {
				t.Errorf("MemoryPath(%q) = %q, want %q", tc.senderID, got, tc.want)
			}
		})
	}
}

func TestMemoryPath_OutsideWorkspace(t *testing.T) {
	workspace := "/data/workspace"
	stateDir := "/data/state"
	memPath := MemoryPath(stateDir, "ou_test")
	if strings.HasPrefix(memPath, workspace) {
		t.Errorf("memory path %q should NOT be under workspace", memPath)
	}
}

// --- jsonl store ---

func TestLoadMemories_NonexistentReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	entries, err := LoadMemories(path)
	if err != nil {
		t.Fatalf("load nonexistent: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("nonexistent should return 0 entries, got %d", len(entries))
	}
}

func TestUpsertMemory_CreateNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "memory.jsonl")
	err := upsertMemory(path, "出差", "用户下周三要出差", nil)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	entries, _ := LoadMemories(path)
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Key != "出差" || e.Content != "用户下周三要出差" {
		t.Errorf("entry = %+v", e)
	}
	if e.ID == "" || e.Status != "active" || e.Created.IsZero() || e.Updated.IsZero() {
		t.Errorf("entry missing metadata: %+v", e)
	}
}

func TestUpsertMemory_OverwriteSameKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	upsertMemory(path, "出差", "用户下周三要出差", nil)
	upsertMemory(path, "出差", "出差改到周五了", nil)

	entries, _ := LoadMemories(path)
	if len(entries) != 1 {
		t.Fatalf("upsert same key should not duplicate, got %d entries", len(entries))
	}
	if entries[0].Content != "出差改到周五了" {
		t.Errorf("content not overwritten: %q", entries[0].Content)
	}
	// created 不变，updated 刷新
	if entries[0].Updated.Before(entries[0].Created) {
		t.Error("updated should be >= created after overwrite")
	}
}

func TestUpsertMemory_DifferentKeysAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	upsertMemory(path, "出差", "下周出差", nil)
	upsertMemory(path, "猫", "猫叫橘子", nil)

	entries, _ := LoadMemories(path)
	if len(entries) != 2 {
		t.Fatalf("different keys should append, got %d", len(entries))
	}
}

func TestForgetMemory_SoftDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	upsertMemory(path, "出差", "下周出差", nil)
	upsertMemory(path, "猫", "猫叫橘子", nil)

	err := forgetMemory(path, "出差")
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	entries, _ := LoadMemories(path)
	if len(entries) != 2 {
		t.Fatalf("forget should not physically delete, got %d entries", len(entries))
	}
	// 出差 should be forgotten
	var foundForgotten bool
	for _, e := range entries {
		if e.Key == "出差" && e.Status == "forgotten" {
			foundForgotten = true
		}
	}
	if !foundForgotten {
		t.Error("forgotten entry should have status=forgotten")
	}
}

func TestForgetMemory_NonexistentKeyNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	upsertMemory(path, "猫", "猫叫橘子", nil)
	err := forgetMemory(path, "不存在的key")
	if err != nil {
		t.Fatalf("forget nonexistent: %v", err)
	}
	entries, _ := LoadMemories(path)
	if len(entries) != 1 || entries[0].Status != "active" {
		t.Errorf("forget nonexistent key should be no-op: %+v", entries)
	}
}

func TestActiveMemories_FiltersForgottenAndExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	upsertMemory(path, "active1", "正常记忆", nil)
	upsertMemory(path, "active2", "另一条", nil)

	// 过期的
	past := time.Now().Add(-1 * time.Hour)
	upsertMemory(path, "expired", "过期了", &past)

	// 忘记的
	forgetMemory(path, "active1")

	active, err := ActiveMemories(path)
	if err != nil {
		t.Fatalf("activeMemories: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("got %d active, want 1 (active2 only): %+v", len(active), active)
	}
	if active[0].Key != "active2" {
		t.Errorf("active entry = %+v, want active2", active[0])
	}
}

func TestUpsertMemory_ConcurrentNoLost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	var wg sync.WaitGroup
	keys := []string{"k1", "k2", "k3", "k4", "k5"}
	wg.Add(len(keys))
	for _, k := range keys {
		k := k
		go func() {
			defer wg.Done()
			upsertMemory(path, k, "content_"+k, nil)
		}()
	}
	wg.Wait()
	entries, _ := LoadMemories(path)
	if len(entries) != len(keys) {
		t.Errorf("concurrent upsert lost entries: got %d, want %d", len(entries), len(keys))
	}
}

func TestMemoryFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	upsertMemory(path, "test", "x", nil)
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("memory.jsonl perm = %o, want 0600", info.Mode().Perm())
	}
}

// --- Step 2+3: update_memory + forget_memory 工具 ---

func memCtxWithSender(sender string) context.Context {
	return chatkey.WithChatKey(context.Background(), chatkey.ChatKey{SenderID: sender})
}

func TestUpdateMemoryTool_WritesJsonl(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistry("", []string{"update_memory"}, SandboxRoots{}, stateDir)
	ctx := memCtxWithSender("ou_大明")

	out, err := reg.Execute(ctx, "update_memory", map[string]any{
		"key": "出差", "content": "用户下周三要出差",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out["updated"] != true {
		t.Errorf("updated = %v, want true", out["updated"])
	}
	// 不返回路径
	if _, hasPath := out["path"]; hasPath {
		t.Error("update_memory should NOT return path")
	}
	// 文件写入
	entries, _ := LoadMemories(MemoryPath(stateDir, "ou_大明"))
	if len(entries) != 1 || entries[0].Content != "用户下周三要出差" {
		t.Errorf("unexpected entries: %+v", entries)
	}
}

func TestUpdateMemoryTool_WithExpires(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistry("", []string{"update_memory"}, SandboxRoots{}, stateDir)
	ctx := memCtxWithSender("ou_a")

	_, err := reg.Execute(ctx, "update_memory", map[string]any{
		"key": "temp", "content": "临时记忆", "expires": "2999-01-01",
	})
	if err != nil {
		t.Fatalf("execute with expires: %v", err)
	}
	entries, _ := LoadMemories(MemoryPath(stateDir, "ou_a"))
	if len(entries) != 1 || entries[0].Expires == nil {
		t.Errorf("expires not set: %+v", entries)
	}
}

func TestUpdateMemoryTool_NoSenderRejected(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistry("", []string{"update_memory"}, SandboxRoots{}, stateDir)
	_, err := reg.Execute(context.Background(), "update_memory", map[string]any{
		"key": "x", "content": "y",
	})
	if err == nil {
		t.Error("update_memory without sender should error")
	}
}

func TestUpdateMemoryTool_MissingArgsRejected(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistry("", []string{"update_memory"}, SandboxRoots{}, stateDir)
	ctx := memCtxWithSender("ou_a")
	// 缺 content
	if _, err := reg.Execute(ctx, "update_memory", map[string]any{"key": "x"}); err == nil {
		t.Error("missing content should error")
	}
	// 缺 key
	if _, err := reg.Execute(ctx, "update_memory", map[string]any{"content": "y"}); err == nil {
		t.Error("missing key should error")
	}
}

func TestForgetMemoryTool_SoftDeletes(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistry("", []string{"update_memory", "forget_memory"}, SandboxRoots{}, stateDir)
	ctx := memCtxWithSender("ou_a")

	// 先写
	reg.Execute(ctx, "update_memory", map[string]any{"key": "出差", "content": "下周出差"})
	// 再忘
	out, err := reg.Execute(ctx, "forget_memory", map[string]any{"key": "出差"})
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if out["forgotten"] != true {
		t.Errorf("forgotten = %v, want true", out["forgotten"])
	}
	// 不返回路径
	if _, hasPath := out["path"]; hasPath {
		t.Error("forget_memory should NOT return path")
	}
	// 软删——文件里还在但 status=forgotten
	entries, _ := LoadMemories(MemoryPath(stateDir, "ou_a"))
	if len(entries) != 1 {
		t.Fatalf("soft delete should keep entry, got %d", len(entries))
	}
	if entries[0].Status != "forgotten" {
		t.Errorf("status = %q, want forgotten", entries[0].Status)
	}
}

func TestForgetMemoryTool_NoSenderRejected(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistry("", []string{"forget_memory"}, SandboxRoots{}, stateDir)
	_, err := reg.Execute(context.Background(), "forget_memory", map[string]any{"key": "x"})
	if err == nil {
		t.Error("forget_memory without sender should error")
	}
}

func TestUpdateMemoryTool_PerSenderIsolated(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistry("", []string{"update_memory"}, SandboxRoots{}, stateDir)

	reg.Execute(memCtxWithSender("ou_alice"), "update_memory", map[string]any{"key": "k", "content": "alice's"})
	reg.Execute(memCtxWithSender("ou_bob"), "update_memory", map[string]any{"key": "k", "content": "bob's"})

	aliceEntries, _ := LoadMemories(MemoryPath(stateDir, "ou_alice"))
	bobEntries, _ := LoadMemories(MemoryPath(stateDir, "ou_bob"))
	if len(aliceEntries) != 1 || !strings.Contains(aliceEntries[0].Content, "alice") {
		t.Errorf("alice's memory wrong: %+v", aliceEntries)
	}
	if len(bobEntries) != 1 || !strings.Contains(bobEntries[0].Content, "bob") {
		t.Errorf("bob's memory wrong: %+v", bobEntries)
	}
}
