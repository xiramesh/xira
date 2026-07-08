package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/chatkey"
)

// resolvePrivateRoot 算出 sender 的私有层根目录：
// workspaceRoot/users/sender_{safePathID(senderID)}。
// senderID 为空 → 返回 ""（上层据此判断「未启用隔离」，走单层逻辑）。

func TestResolvePrivateRoot(t *testing.T) {
	ws := "/data/workspace"
	cases := []struct {
		name     string
		senderID string
		want     string // 期望的私有层根（"" 表示不隔离）
	}{
		{"normal", "ou_大明", filepath.Join(ws, "users", "sender_ou_大明")},
		{"ascii id", "wxid_abc123", filepath.Join(ws, "users", "sender_wxid_abc123")},
		{"empty -> no isolation", "", ""},
		{"whitespace only -> no isolation", "   ", ""},
		{"dangerous slash", "ou_evil/../../../etc", filepath.Join(ws, "users", "sender_ou_evil_.._.._.._etc")},
		{"chinese", "张三", filepath.Join(ws, "users", "sender_张三")},
		{"spaces in id", "ou_a b", filepath.Join(ws, "users", "sender_ou_a_b")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePrivateRoot(ws, tc.senderID)
			if got != tc.want {
				t.Errorf("resolvePrivateRoot(%q) = %q, want %q", tc.senderID, got, tc.want)
			}
		})
	}
}

// 关键安全属性：私有层根永远在 workspaceRoot/users/ 下，senderID 逃逸不了。
// 注：SafePathID 保留 '.' 字符（session 包既有行为），所以 ".." 可能出现在
// 目录名里作为字面字符（如 "sender_.._.._etc"），但 filepath.Join 不会把它解析成
// 路径穿越——Clean 后仍在 users/ 下。这里断言「不逃出 users 目录」。
func TestResolvePrivateRoot_NoEscape(t *testing.T) {
	ws := "/data/workspace"
	evil := resolvePrivateRoot(ws, "../../../../etc/passwd")
	if !strings.HasPrefix(evil, filepath.Join(ws, "users")+string(filepath.Separator)) {
		t.Errorf("private root escapes workspace/users: %q", evil)
	}
	// 再 Clean 一遍，确认不会因为残留的 .. 语义逃逸。
	cleaned := filepath.Clean(evil)
	if !strings.HasPrefix(cleaned, filepath.Join(ws, "users")+string(filepath.Separator)) {
		t.Errorf("private root escapes after Clean: %q", cleaned)
	}
}

// resolveWrite 隔离开启时：相对路径永远落私有层，在私有 root 内。
// resolveRead 隔离开启时：先查私有层，没有 fallback 通用层。
// 隔离未开启（senderID 空）：退化为单层（现有 resolveWithin 行为）。

func setupOverlayWorkspace(t *testing.T) (ws string) {
	t.Helper()
	ws = t.TempDir()
	// 通用层预置：kb/index.md
	mustWrite(t, filepath.Join(ws, "kb", "index.md"), "shared kb content")
	// 通用层预置：fibonacci.py（用户产物遗留）
	mustWrite(t, filepath.Join(ws, "fibonacci.py"), "shared legacy file")
	return ws
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mustMkdir 建一个目录（含父目录）。
func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestResolveWrite_Overlay(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	sender := "ou_大明"
	privateRoot := resolvePrivateRoot(ws, sender)
	// 私有层 root 算进 writeRoots，让边界检查通过
	writeRoots := []string{ws, privateRoot}

	got, err := resolveWrite("decision.md", ws, sender, writeRoots)
	if err != nil {
		t.Fatalf("resolveWrite error: %v", err)
	}
	want := filepath.Join(privateRoot, "decision.md")
	if got != want {
		t.Errorf("resolveWrite = %q, want %q", got, want)
	}
}

func TestResolveWrite_OverlayEscapesPrivateRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	sender := "ou_a"
	privateRoot := resolvePrivateRoot(ws, sender)
	writeRoots := []string{ws, privateRoot} // ws 在 roots 里，但隔离语义要求写只能落私有层

	// 用 ../ 逃逸私有层 → 应被拒（即使 ws 在 roots 里，也不能写通用层）
	_, err := resolveWrite("../kb/index.md", ws, sender, writeRoots)
	if err == nil {
		t.Error("resolveWrite with ../ escape should be rejected")
	}
}

func TestResolveRead_OverlayPrivateFirst(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	sender := "ou_a"
	privateRoot := resolvePrivateRoot(ws, sender)
	// 私有层有 decision.md
	mustWrite(t, filepath.Join(privateRoot, "decision.md"), "private decision")
	readRoots := []string{ws, privateRoot}

	got, err := resolveRead("decision.md", ws, sender, readRoots)
	if err != nil {
		t.Fatalf("resolveRead error: %v", err)
	}
	want := filepath.Join(privateRoot, "decision.md")
	if got != want {
		t.Errorf("resolveRead (private exists) = %q, want private %q", got, want)
	}
}

func TestResolveRead_OverlayFallbackToCommon(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	sender := "ou_a"
	privateRoot := resolvePrivateRoot(ws, sender)
	readRoots := []string{ws, privateRoot}

	// kb/index.md 私有层没有 → fallback 通用层
	got, err := resolveRead("kb/index.md", ws, sender, readRoots)
	if err != nil {
		t.Fatalf("resolveRead fallback error: %v", err)
	}
	want := filepath.Join(ws, "kb", "index.md")
	if got != want {
		t.Errorf("resolveRead fallback = %q, want common %q", got, want)
	}
}

func TestResolveRead_AbsolutePathNotRewritten(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	sender := "ou_a"
	privateRoot := resolvePrivateRoot(ws, sender)
	readRoots := []string{ws, privateRoot}

	// 绝对路径（在 readRoots 内）→ 保持原样，不 rewrite
	abs := filepath.Join(ws, "kb", "index.md")
	got, err := resolveRead(abs, ws, sender, readRoots)
	if err != nil {
		t.Fatalf("resolveRead abs error: %v", err)
	}
	if got != abs {
		t.Errorf("resolveRead abs = %q, want %q (not rewritten)", got, abs)
	}
}

func TestResolveRead_NoIsolationDegradesToSingleRoot(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	// senderID 空 → 不隔离，退化为单层（现有 resolveWithin 行为）
	got, err := resolveRead("fibonacci.py", ws, "", []string{ws})
	if err != nil {
		t.Fatalf("resolveRead no-isolation error: %v", err)
	}
	want := filepath.Join(ws, "fibonacci.py")
	if got != want {
		t.Errorf("resolveRead no-isolation = %q, want %q", got, want)
	}
}

func TestResolveWrite_NoIsolationDegradesToSingleRoot(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	// senderID 空 → 写落 workspace 根（现状）
	got, err := resolveWrite("fibonacci.py", ws, "", []string{ws})
	if err != nil {
		t.Fatalf("resolveWrite no-isolation error: %v", err)
	}
	want := filepath.Join(ws, "fibonacci.py")
	if got != want {
		t.Errorf("resolveWrite no-isolation = %q, want %q", got, want)
	}
}

func TestResolveRead_NotInAnyRootRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	sender := "ou_a"
	privateRoot := resolvePrivateRoot(ws, sender)
	readRoots := []string{ws, privateRoot}

	// 路径在两个 root 外 → 拒绝
	_, err := resolveRead("/etc/passwd", ws, sender, readRoots)
	if err == nil {
		t.Error("resolveRead outside roots should be rejected")
	}
}

// --- Step 4: fs 工具 Execute 接 overlay（集成测）---

func ctxWithSender(sender string) context.Context {
	// 测试用：同时设 DataIsolation=true，模拟配了 data_isolation 的 entrypoint。
	return chatkey.WithChatKey(context.Background(), chatkey.ChatKey{SenderID: sender, DataIsolation: true})
}

func ctxWithSenderNoIsolation(sender string) context.Context {
	// DataIsolation=false（entrypoint 没配）→ 即使有 sender 也走单层。
	return chatkey.WithChatKey(context.Background(), chatkey.ChatKey{SenderID: sender})
}

func TestWriteFile_IsolatesPerSender(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"write_file", "read_file"}, SandboxRoots{})
	alice := ctxWithSender("ou_alice")
	bob := ctxWithSender("ou_bob")

	// Alice 写 decision.md
	if _, err := execute(reg, alice, "write_file", map[string]any{"path": "decision.md", "content": "alice's decision"}); err != nil {
		t.Fatalf("alice write: %v", err)
	}
	// Bob 写同名 decision.md
	if _, err := execute(reg, bob, "write_file", map[string]any{"path": "decision.md", "content": "bob's decision"}); err != nil {
		t.Fatalf("bob write: %v", err)
	}

	// 两人在各自私有层
	alicePath := filepath.Join(resolvePrivateRoot(ws, "ou_alice"), "decision.md")
	bobPath := filepath.Join(resolvePrivateRoot(ws, "ou_bob"), "decision.md")
	assertContent(t, alicePath, "alice's decision")
	assertContent(t, bobPath, "bob's decision")

	// 互相不可见：Alice 读 decision.md 读到自己的，不是 Bob 的
	out, err := execute(reg, alice, "read_file", map[string]any{"path": "decision.md"})
	if err != nil {
		t.Fatalf("alice read: %v", err)
	}
	if out["content"] != "alice's decision" {
		t.Errorf("alice read decision.md = %v, want alice's", out["content"])
	}
}

func TestReadFile_FallbackToCommonKB(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"read_file"}, SandboxRoots{})
	alice := ctxWithSender("ou_alice")

	// kb/index.md 私有层没有 → fallback 通用层
	out, err := execute(reg, alice, "read_file", map[string]any{"path": "kb/index.md"})
	if err != nil {
		t.Fatalf("read kb fallback: %v", err)
	}
	if out["content"] != "shared kb content" {
		t.Errorf("kb fallback content = %v, want shared kb content", out["content"])
	}
}

func TestWriteFile_NoSenderDegradesToSingleRoot(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{})

	// 无 sender ctx → 写落 workspace 根（现状）
	ctx := context.Background()
	if _, err := execute(reg, ctx, "write_file", map[string]any{"path": "legacy.py", "content": "x"}); err != nil {
		t.Fatalf("no-sender write: %v", err)
	}
	assertContent(t, filepath.Join(ws, "legacy.py"), "x")
}

func execute(reg *Registry, ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	return reg.Execute(ctx, name, args)
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("%s content = %q, want %q", path, data, want)
	}
}

// --- Step 5: entrypoint data_isolation 开关 ---

func TestWriteFile_DataIsolationDisabledGoesSingleRoot(t *testing.T) {
	// 即使 ctx 有 sender，entrypoint 没开 data_isolation（DataIsolation=false）
	// → 写落 workspace 根（单层，向后兼容）。
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{})
	ctx := ctxWithSenderNoIsolation("ou_alice")

	if _, err := execute(reg, ctx, "write_file", map[string]any{"path": "single.py", "content": "x"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 落 workspace 根，不是 users/sender_alice/
	assertContent(t, filepath.Join(ws, "single.py"), "x")
	privatePath := filepath.Join(resolvePrivateRoot(ws, "ou_alice"), "single.py")
	if _, err := os.Stat(privatePath); !os.IsNotExist(err) {
		t.Errorf("isolation disabled but file landed in private layer: %s", privatePath)
	}
}

func TestWriteFile_DataIsolationEnabledGoesPrivate(t *testing.T) {
	// entrypoint 开了 data_isolation + 有 sender → 写落私有层。
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{})
	ctx := ctxWithSender("ou_alice") // DataIsolation=true

	if _, err := execute(reg, ctx, "write_file", map[string]any{"path": "iso.md", "content": "private"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	privatePath := filepath.Join(resolvePrivateRoot(ws, "ou_alice"), "iso.md")
	assertContent(t, privatePath, "private")
	// 不落 workspace 根
	if _, err := os.Stat(filepath.Join(ws, "iso.md")); !os.IsNotExist(err) {
		t.Error("file should NOT be in workspace root when isolation enabled")
	}
}

// --- 覆盖率补充：错误分支（§5.2 契约函数达 100%）---

func TestResolveWrite_EmptyPathRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	if _, err := resolveWrite("", ws, "ou_a", []string{ws}); err == nil {
		t.Error("empty path should be rejected")
	}
	if _, err := resolveWrite("   ", ws, "ou_a", []string{ws}); err == nil {
		t.Error("whitespace path should be rejected")
	}
}

func TestResolveRead_EmptyPathRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	if _, err := resolveRead("", ws, "ou_a", []string{ws}); err == nil {
		t.Error("empty path should be rejected")
	}
}

func TestResolveWrite_AbsoluteOutsideRootsRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	if _, err := resolveWrite("/etc/passwd", ws, "ou_a", []string{ws}); err == nil {
		t.Error("absolute path outside roots should be rejected")
	}
}

func TestResolveWrite_AbsoluteInsideRootOK(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	// 绝对路径在 writeRoots 内（不 rewrite）
	abs := filepath.Join(ws, "kb", "index.md")
	got, err := resolveWrite(abs, ws, "ou_a", []string{ws})
	if err != nil {
		t.Fatalf("absolute in roots: %v", err)
	}
	if got != abs {
		t.Errorf("absolute rewritten: got %q, want %q", got, abs)
	}
}

func TestResolveReadCtx_MissingPathArg(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"read_file"}, SandboxRoots{})
	ctx := ctxWithSender("ou_a")
	// args 没有 path 字段 → 报错
	if _, err := execute(reg, ctx, "read_file", map[string]any{}); err == nil {
		t.Error("missing path arg should be rejected")
	}
}

func TestResolveWriteCtx_MissingPathArg(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{})
	ctx := ctxWithSender("ou_a")
	if _, err := execute(reg, ctx, "write_file", map[string]any{"content": "x"}); err == nil {
		t.Error("missing path arg should be rejected")
	}
}

func TestSenderIDFromCtx_NilAndNoChatKey(t *testing.T) {
	// nil ctx / 无 ChatKey → 返回 ""（走单层）
	if got := senderIDFromCtx(nil); got != "" {
		t.Errorf("nil ctx senderID = %q, want empty", got)
	}
	if got := senderIDFromCtx(context.Background()); got != "" {
		t.Errorf("plain ctx senderID = %q, want empty", got)
	}
	// 有 ChatKey 但 DataIsolation=false → 返回 ""（向后兼容）
	ctx := ctxWithSenderNoIsolation("ou_a")
	if got := senderIDFromCtx(ctx); got != "" {
		t.Errorf("DataIsolation=false senderID = %q, want empty", got)
	}
}

// --- Step 6: symlink 逃逸回归（#W2 防护不能被 per-sender root 绕过）---

func TestResolveWrite_SymlinkEscapeFromPrivateRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	sender := "ou_a"
	privateRoot := resolvePrivateRoot(ws, sender)
	mustMkdir(t, privateRoot) // 建私有层目录
	writeRoots := []string{ws, privateRoot}

	// 私有层放一个 symlink 指向通用层的 kb/index.md，试图借 rewrite 写通用层
	linkPath := filepath.Join(privateRoot, "evil_link.md")
	target := filepath.Join(ws, "kb", "index.md")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Skipf("symlink not supported on this platform: %v", err)
	}

	// 写 evil_link.md 实际想写 kb/index.md → 应被边界检查拒（#W2: symlink 解析后
	// 指向通用层，不在私有 root 内）。
	_, err := resolveWrite("evil_link.md", ws, sender, writeRoots)
	if err == nil {
		t.Error("resolveWrite via symlink escaping private root should be rejected")
	}
}

func TestResolveRead_SymlinkEscapeRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	sender := "ou_a"
	privateRoot := resolvePrivateRoot(ws, sender)
	mustMkdir(t, privateRoot)
	readRoots := []string{ws, privateRoot}

	// 私有层 symlink 指向 /etc/passwd（root 外）
	linkPath := filepath.Join(privateRoot, "etc_passwd.md")
	if err := os.Symlink("/etc/passwd", linkPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	// 读 evil → symlink 解析后指向 root 外 → 拒
	_, err := resolveRead("etc_passwd.md", ws, sender, readRoots)
	if err == nil {
		t.Error("resolveRead via symlink escaping roots should be rejected")
	}
}

func TestResolveRead_FallbackNotFooledBySymlink(t *testing.T) {
	// 私有层 symlink 指向通用层文件。read「私有优先」命中 symlink，但 #W2 的
	// pathWithinRoots 对私有 root 做边界检查——symlink 解析后指向通用层（不在私有
	// root 内）→ 被拒。这是 #W2 正确工作的表现：私有层的 symlink 不能借「私有优先」
	// 绕过边界。私有层不该放指向外部的 symlink（要读通用层，直接用 kb/... 相对路径，
	// 走 fallback，不要 symlink）。
	ws := setupOverlayWorkspace(t)
	sender := "ou_a"
	privateRoot := resolvePrivateRoot(ws, sender)
	mustMkdir(t, privateRoot)
	readRoots := []string{ws, privateRoot}

	linkPath := filepath.Join(privateRoot, "kb_link.md")
	target := filepath.Join(ws, "kb", "index.md")
	if err := os.Symlink(target, linkPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	if _, err := resolveRead("kb_link.md", ws, sender, readRoots); err == nil {
		t.Error("private-layer symlink to common layer should be rejected by #W2 boundary check")
	}
}
