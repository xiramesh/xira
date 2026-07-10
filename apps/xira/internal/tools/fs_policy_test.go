package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fs_policy_test.go: #110 — write_file/edit_file 移除 RequireConfirmation。
//
// 设计:allow_roots 是硬边界(越界=拒绝),gate 对 write/edit 是冗余二次防护。
// 这些测试 pin:(1) Policy.Risk="high"(审计分级保留);(2) 越界写仍被 sandbox 拒
// (边界防护不降级);(3) 边界内写直接执行(不触发任何 gate/waiting)。
//
// 注:RequireConfirmation 字段已从 ToolPolicy 删除,故不断言它(只断言 Risk)。

// TestWriteFilePolicyHighRisk 是 #110 的契约 pin:write_file/edit_file 的
// Policy.Risk 必须为 "high"(保留审计分级)。回归门:防止有人误把 Risk 降级。
func TestWriteFilePolicyHighRisk(t *testing.T) {
	ws := t.TempDir()
	roots := SandboxRoots{AllowRoots: []string{ws}}
	registry := NewBuiltinRegistry(ws, []string{"write_file", "edit_file", "read_file"}, roots, "")

	for _, name := range []string{"write_file", "edit_file"} {
		def, ok := registry.GetDefinition(name)
		if !ok {
			t.Fatalf("%s not in registry", name)
		}
		if def.Policy.Risk != "high" {
			t.Errorf("%s Policy.Risk = %q, want high (audit grading preserved)", name, def.Policy.Risk)
		}
	}
}

// TestWriteFileSandboxStillRejectsOutOfBoundPath 是 #110 的安全回归门:
// 移除 gate 后,越界写必须仍被 sandbox 拒绝(path must stay within allowed roots)。
// 这证明 "边界 = 唯一防护" 没有降级 —— gate 移除不等于放行任意路径。
func TestWriteFileSandboxStillRejectsOutOfBoundPath(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt") // 不同 TempDir,不在 allow_roots
	registry := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{AllowRoots: []string{ws}}, "")

	_, err := registry.Execute(context.Background(), "write_file", map[string]any{
		"path":    outside,
		"content": "should be rejected",
	})
	if err == nil {
		// 写成功了?检查文件是否真的被创建 —— 不该被创建。
		if _, statErr := os.Stat(outside); statErr == nil {
			t.Fatalf("write_file wrote OUTSIDE allow_roots to %s — sandbox boundary broken", outside)
		}
		t.Fatalf("write_file to out-of-bound path succeeded without error (file not created either — unexpected)")
	}
	if !strings.Contains(err.Error(), "allowed roots") && !strings.Contains(err.Error(), "within") {
		t.Errorf("out-of-bound write error = %v, want it to mention allowed roots / within", err)
	}
}

// TestWriteFileInBoundExecutesWithoutGate 是 #110 的正向验证:allow_roots 内的写
// 直接执行(不触发任何 gate/waiting)。这是 outcome 场景的核心:agent 写 vault/work
// 内文件,不该被拦。
func TestWriteFileInBoundExecutesWithoutGate(t *testing.T) {
	ws := t.TempDir()
	registry := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{AllowRoots: []string{ws}}, "")

	inBound := filepath.Join(ws, "task-done.md")
	out, err := registry.Execute(context.Background(), "write_file", map[string]any{
		"path":    inBound,
		"content": "task completed",
	})
	if err != nil {
		t.Fatalf("in-bound write_file error = %v, want success (no gate)", err)
	}
	// 文件确实写了
	got, readErr := os.ReadFile(inBound)
	if readErr != nil {
		t.Fatalf("read back written file: %v", readErr)
	}
	if string(got) != "task completed" {
		t.Errorf("written content = %q, want 'task completed'", string(got))
	}
	// 输出状态是完成,不是 waiting_human(gate 不再介入)
	status, _ := out["status"].(string)
	if status == "waiting_human" {
		t.Errorf("in-bound write returned status=waiting_human — gate should not trigger (#110)")
	}
}
