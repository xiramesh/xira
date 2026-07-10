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
	reg := NewBuiltinRegistry(ws, []string{"write_file", "read_file"}, SandboxRoots{}, "")
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
	reg := NewBuiltinRegistry(ws, []string{"read_file"}, SandboxRoots{}, "")
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
	reg := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{}, "")

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
	reg := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{}, "")
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
	reg := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{}, "")
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

func TestWriteFile_MissingContentRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{}, "")
	if _, err := execute(reg, context.Background(), "write_file", map[string]any{"path": "x.md"}); err == nil {
		t.Error("missing content should be rejected")
	}
	// content 非 string
	if _, err := execute(reg, context.Background(), "write_file", map[string]any{"path": "x.md", "content": 123}); err == nil {
		t.Error("non-string content should be rejected")
	}
}

func TestListDir_NonexistentPathRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"list_dir"}, SandboxRoots{}, "")
	_, err := execute(reg, context.Background(), "list_dir", map[string]any{"path": "no_such_dir/"})
	if err == nil {
		t.Error("nonexistent dir should error")
	}
}

func TestReadFile_NonexistentPathRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"read_file"}, SandboxRoots{}, "")
	_, err := execute(reg, context.Background(), "read_file", map[string]any{"path": "no_such_file.md"})
	if err == nil {
		t.Error("nonexistent file should error")
	}
}

func TestEditFile_OldTextNotFoundRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	mustWrite(t, filepath.Join(ws, "edit.md"), "hello world")
	reg := NewBuiltinRegistry(ws, []string{"edit_file"}, SandboxRoots{}, "")
	_, err := execute(reg, context.Background(), "edit_file", map[string]any{
		"path": "edit.md", "old_text": "not present", "new_text": "x",
	})
	if err == nil {
		t.Error("old_text not found should error")
	}
}

func TestEditFile_MissingArgsRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	mustWrite(t, filepath.Join(ws, "e2.md"), "x")
	reg := NewBuiltinRegistry(ws, []string{"edit_file"}, SandboxRoots{}, "")
	// 缺 old_text
	if _, err := execute(reg, context.Background(), "edit_file", map[string]any{"path": "e2.md", "new_text": "y"}); err == nil {
		t.Error("missing old_text should be rejected")
	}
	// 缺 new_text
	if _, err := execute(reg, context.Background(), "edit_file", map[string]any{"path": "e2.md", "old_text": "x"}); err == nil {
		t.Error("missing new_text should be rejected")
	}
}

func TestEditFile_SuccessReplacesText(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	mustWrite(t, filepath.Join(ws, "replace.md"), "foo bar baz")
	reg := NewBuiltinRegistry(ws, []string{"edit_file"}, SandboxRoots{}, "")
	out, err := execute(reg, context.Background(), "edit_file", map[string]any{
		"path": "replace.md", "old_text": "bar", "new_text": "QUX",
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if out["replacements"] != 1 {
		t.Errorf("replacements = %v, want 1", out["replacements"])
	}
	assertContent(t, filepath.Join(ws, "replace.md"), "foo QUX baz")
}

func TestListDir_RootItself(t *testing.T) {
	// list_dir 不传 path → 默认 "."（workspace 根），列出内容。
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"list_dir"}, SandboxRoots{}, "")
	out, err := execute(reg, context.Background(), "list_dir", map[string]any{})
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	entries, _ := out["entries"].([]map[string]any)
	if len(entries) == 0 {
		t.Error("list_dir('.') should return workspace entries")
	}
}

func TestSearchFile_NoQueryRejected(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"search_file"}, SandboxRoots{}, "")
	if _, err := execute(reg, context.Background(), "search_file", map[string]any{"query": ""}); err == nil {
		t.Error("empty query should be rejected")
	}
}

func TestSearchFile_FileRoot(t *testing.T) {
	// root 指向文件（非目录）→ 只搜该文件。
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"search_file"}, SandboxRoots{}, "")
	out, err := execute(reg, context.Background(), "search_file", map[string]any{
		"query": "shared", "root": "kb/index.md",
	})
	if err != nil {
		t.Fatalf("search file root: %v", err)
	}
	matches, _ := out["matches"].([]map[string]any)
	if len(matches) == 0 {
		t.Error("search on a file root should find matches in it")
	}
}

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

func TestResolveWrite_AbsoluteInPrivateRootOK(t *testing.T) {
	// 隔离启用时：绝对路径在自己的私有层内 → 允许（不 rewrite）。
	ws := setupOverlayWorkspace(t)
	abs := filepath.Join(resolvePrivateRoot(ws, "ou_a"), "notes.md")
	got, err := resolveWrite(abs, ws, "ou_a", []string{ws, resolvePrivateRoot(ws, "ou_a")})
	if err != nil {
		t.Fatalf("absolute in private root: %v", err)
	}
	if got != abs {
		t.Errorf("absolute in private rewritten: got %q, want %q", got, abs)
	}
}

func TestResolveWrite_AbsoluteInsideRootNoIsolationOK(t *testing.T) {
	// 隔离未启用（senderID 空）：绝对路径在 writeRoots 内即可（现有行为）。
	ws := setupOverlayWorkspace(t)
	abs := filepath.Join(ws, "kb", "index.md")
	got, err := resolveWrite(abs, ws, "", []string{ws})
	if err != nil {
		t.Fatalf("absolute in roots (no isolation): %v", err)
	}
	if got != abs {
		t.Errorf("absolute rewritten: got %q, want %q", got, abs)
	}
}

func TestResolveReadCtx_MissingPathArg(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"read_file"}, SandboxRoots{}, "")
	ctx := ctxWithSender("ou_a")
	// args 没有 path 字段 → 报错
	if _, err := execute(reg, ctx, "read_file", map[string]any{}); err == nil {
		t.Error("missing path arg should be rejected")
	}
}

func TestResolveWriteCtx_MissingPathArg(t *testing.T) {
	ws := setupOverlayWorkspace(t)
	reg := NewBuiltinRegistry(ws, []string{"write_file"}, SandboxRoots{}, "")
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

// --- PR #145 review: 两个 bypass 复现（隔离启用时的 cross-sender 读 / 跨层写）---

func TestResolveRead_CannotReadOtherSendersPrivateViaFallback(t *testing.T) {
	// Bypass 1（review 坐实）：Alice 的私有层没有 users/sender_ou_bob/...，
	// fallback 到通用层 workspace/users/sender_ou_bob/...——而 Bob 的私有层物理上
	// 在 workspace 树内，workspace 又是 readRoot → Alice 能读 Bob 的文件。
	// 隔离语义要求：fallback 只能读通用层的「非 users/」区域，不能借 fallback
	// 读到其他 sender 的私有目录。
	ws := setupOverlayWorkspace(t)
	alice := "ou_alice"
	bob := "ou_bob"
	bobRoot := resolvePrivateRoot(ws, bob)
	mustWrite(t, filepath.Join(bobRoot, "secret.md"), "bob's secret")
	readRoots := []string{ws, resolvePrivateRoot(ws, alice)}

	// Alice 试读 Bob 的私有文件（用相对路径指向 users/sender_ou_bob/secret.md）
	_, err := resolveRead("users/sender_ou_bob/secret.md", ws, alice, readRoots)
	if err == nil {
		t.Error("BLOCKER: Alice can read Bob's private file via fallback to workspace/users/...")
	}
}

func TestResolveWrite_AbsolutePathCannotWriteCommonLayer(t *testing.T) {
	// Bypass 2（review 坐实）：隔离启用时，write 的绝对路径不 rewrite，只要在
	// writeRoots（含 workspace 根）内就允许 → agent 拿 read_file 返回的绝对路径
	// 喂给 edit_file，能改共享 KB。隔离语义要求：隔离启用时，写只能落私有层，
	// 绝对路径也必须落在私有 root 内（或拒绝）。
	ws := setupOverlayWorkspace(t)
	sender := "ou_a"
	privateRoot := resolvePrivateRoot(ws, sender)
	writeRoots := []string{ws, privateRoot} // workspace 根在 writeRoots 里

	// 用绝对路径直指通用层 kb/index.md
	kbAbs := filepath.Join(ws, "kb", "index.md")
	_, err := resolveWrite(kbAbs, ws, sender, writeRoots)
	if err == nil {
		t.Error("BLOCKER: absolute-path write to common layer allowed under isolation")
	}
}

// --- 补充回归：review 要求的 cross-sender 边界 + 绝对路径写他人私有层 ---

func TestResolveWrite_AbsoluteCannotWriteOtherSendersPrivate(t *testing.T) {
	// 隔离启用时，Alice 不能用绝对路径写 Bob 的私有层。
	ws := setupOverlayWorkspace(t)
	bobRoot := resolvePrivateRoot(ws, "ou_bob")
	writeRoots := []string{ws, resolvePrivateRoot(ws, "ou_alice")}

	_, err := resolveWrite(filepath.Join(bobRoot, "hijack.md"), ws, "ou_alice", writeRoots)
	if err == nil {
		t.Error("Alice should not write Bob's private layer via absolute path")
	}
}

func TestResolveRead_AbsoluteCannotReadOtherSendersPrivate(t *testing.T) {
	// 隔离启用时，Alice 不能用绝对路径读 Bob 的私有层。
	ws := setupOverlayWorkspace(t)
	bobRoot := resolvePrivateRoot(ws, "ou_bob")
	mustWrite(t, filepath.Join(bobRoot, "secret.md"), "bob's secret")
	readRoots := []string{ws, resolvePrivateRoot(ws, "ou_alice")}

	_, err := resolveRead(filepath.Join(bobRoot, "secret.md"), ws, "ou_alice", readRoots)
	if err == nil {
		t.Error("Alice should not read Bob's private layer via absolute path")
	}
}

func TestResolveRead_AbsoluteCanReadCommonLayer(t *testing.T) {
	// 隔离启用时，Alice 用绝对路径读通用层 KB（不在 users/ 命名空间）→ 允许。
	ws := setupOverlayWorkspace(t)
	kbAbs := filepath.Join(ws, "kb", "index.md")
	readRoots := []string{ws, resolvePrivateRoot(ws, "ou_alice")}

	got, err := resolveRead(kbAbs, ws, "ou_alice", readRoots)
	if err != nil {
		t.Fatalf("absolute read of common KB rejected: %v", err)
	}
	if got != kbAbs {
		t.Errorf("absolute common read rewritten: got %q, want %q", got, kbAbs)
	}
}

func TestResolveRead_CanReadOwnPrivateViaRelative(t *testing.T) {
	// 隔离启用时，Alice 读自己私有层里的 users/sender_ou_alice/... 相对路径 → 允许。
	// （回归：isInPrivateNamespace 不能误伤自己的私有层）
	ws := setupOverlayWorkspace(t)
	alice := "ou_alice"
	aliceRoot := resolvePrivateRoot(ws, alice)
	mustWrite(t, filepath.Join(aliceRoot, "mine.md"), "my data")
	readRoots := []string{ws, aliceRoot}

	// 用指向自己私有目录的相对路径
	rel := filepath.Join("users", "sender_ou_alice", "mine.md")
	got, err := resolveRead(rel, ws, alice, readRoots)
	if err != nil {
		t.Fatalf("read own private via relative: %v", err)
	}
	want := filepath.Join(aliceRoot, "mine.md")
	if got != want {
		t.Errorf("own private read = %q, want %q", got, want)
	}
}

// --- PR #145 review 2: search_file WalkDir 绕过隔离（递归进 users/） ---

func TestSearchFile_DoesNotLeakOtherSendersPrivate_RelativeRoot(t *testing.T) {
	// review 坐实：search_file(root=".") 的 WalkDir 从 workspace 根递归，
	// 会遍历进 users/sender_ou_bob/secret.md，返回 Bob 的私有内容。
	// 隔离语义要求：递归遍历跳过 workspace/users/ 命名空间（除非是自己 privateRoot）。
	ws := setupOverlayWorkspace(t)
	bobRoot := resolvePrivateRoot(ws, "ou_bob")
	mustWrite(t, filepath.Join(bobRoot, "secret.md"), "needle-bob-secret")
	// Alice 也有文件（证明她能搜到自己的）
	aliceRoot := resolvePrivateRoot(ws, "ou_alice")
	mustWrite(t, filepath.Join(aliceRoot, "mine.md"), "needle-alice-mine")

	reg := NewBuiltinRegistry(ws, []string{"search_file"}, SandboxRoots{}, "")
	alice := ctxWithSender("ou_alice")

	out, err := execute(reg, alice, "search_file", map[string]any{"query": "needle"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	matches, _ := out["matches"].([]map[string]any)
	for _, m := range matches {
		p, _ := m["path"].(string)
		if strings.Contains(p, "sender_ou_bob") {
			t.Errorf("BLOCKER: search_file leaked Bob's private file: %s (snippet=%v)", p, m["snippet"])
		}
	}
	// Alice 自己的文件应能搜到
	foundOwn := false
	for _, m := range matches {
		if p, _ := m["path"].(string); strings.Contains(p, "sender_ou_alice") {
			foundOwn = true
		}
	}
	if !foundOwn {
		t.Error("search_file should still find Alice's own private files")
	}
}

func TestSearchFile_DoesNotLeakOtherSendersPrivate_AbsoluteWorkspaceRoot(t *testing.T) {
	// 同上但用绝对路径作 root（reviewer 点名的第二个场景）。
	ws := setupOverlayWorkspace(t)
	bobRoot := resolvePrivateRoot(ws, "ou_bob")
	mustWrite(t, filepath.Join(bobRoot, "secret.md"), "needle-bob-secret")

	reg := NewBuiltinRegistry(ws, []string{"search_file"}, SandboxRoots{}, "")
	alice := ctxWithSender("ou_alice")

	out, err := execute(reg, alice, "search_file", map[string]any{"query": "needle", "root": ws})
	if err != nil {
		t.Fatalf("search abs root: %v", err)
	}
	matches, _ := out["matches"].([]map[string]any)
	for _, m := range matches {
		if p, _ := m["path"].(string); strings.Contains(p, "sender_ou_bob") {
			t.Errorf("BLOCKER: search_file(abs root) leaked Bob's private: %s", p)
		}
	}
}

func TestSearchFile_CanSearchOwnPrivateRoot(t *testing.T) {
	// 隔离启用时，Alice 显式搜自己的 privateRoot → 允许（不误伤）。
	ws := setupOverlayWorkspace(t)
	aliceRoot := resolvePrivateRoot(ws, "ou_alice")
	mustWrite(t, filepath.Join(aliceRoot, "mine.md"), "needle-alice")

	reg := NewBuiltinRegistry(ws, []string{"search_file"}, SandboxRoots{}, "")
	alice := ctxWithSender("ou_alice")

	out, err := execute(reg, alice, "search_file", map[string]any{"query": "needle", "root": "users/sender_ou_alice"})
	if err != nil {
		t.Fatalf("search own root: %v", err)
	}
	matches, _ := out["matches"].([]map[string]any)
	if len(matches) == 0 {
		t.Error("search should find Alice's own file in her private root")
	}
}

// --- PR #147 review: users/ 命名空间保护独立于 data_isolation ---

func TestResolveRead_UsersNamespaceProtectedEvenWithoutIsolation(t *testing.T) {
	// PR #147 review blocker 1：非隔离 entrypoint（senderID="" 模拟）下，
	// users/ 命名空间仍要保护——#126 工具数据隔离的私有层。
	ws := setupOverlayWorkspace(t)
	bobRoot := resolvePrivateRoot(ws, "ou_bob")
	mustWrite(t, filepath.Join(bobRoot, "user.md"), "bob's secret profile")

	readRoots := []string{ws}
	_, err := resolveRead("users/sender_ou_bob/user.md", ws, "", readRoots)
	if err == nil {
		t.Error("BLOCKER: non-isolated resolveRead can read other sender's user.md via users/ namespace")
	}
}

func TestReadFile_UsersNamespaceProtectedWithoutIsolation(t *testing.T) {
	// 集成测：非隔离 ctx（DataIsolation=false），read_file 读他人 user.md → 拒绝。
	ws := t.TempDir()
	mustWrite(t, UserProfilePath(ws, "ou_bob"), "bob secret")
	reg := NewBuiltinRegistry(ws, []string{"read_file"}, SandboxRoots{}, "")

	ctx := ctxWithSenderNoIsolation("ou_alice")
	_, err := execute(reg, ctx, "read_file", map[string]any{"path": "users/sender_ou_bob/user.md"})
	if err == nil {
		t.Error("BLOCKER: read_file without isolation can read other sender's user.md")
	}
}

func TestReadFile_CanReadOwnUserMdWithoutIsolation(t *testing.T) {
	// 非隔离 entrypoint 下，read_file 不该碰 user.md（哪怕自己的）——user.md 归
	// update_profile 工具管（它用无门控 senderID），不是通用文件工具。
	// 这里验证：非隔离下 read_file 读 users/ 下任何路径都被拒（保护语义一致）。
	// update_profile 自己的读写不受此限（它不走 resolveRead/resolveWrite）。
	ws := t.TempDir()
	mustWrite(t, UserProfilePath(ws, "ou_alice"), "alice's own profile")
	reg := NewBuiltinRegistry(ws, []string{"read_file"}, SandboxRoots{}, "")

	ctx := ctxWithSenderNoIsolation("ou_alice")
	_, err := execute(reg, ctx, "read_file", map[string]any{"path": "users/sender_ou_alice/user.md"})
	if err == nil {
		t.Error("read_file should not access users/ namespace even for own user.md (update_profile owns user.md)")
	}
}

func TestUpdateProfile_WorksUnderNonIsolatedEntrypoint(t *testing.T) {
	// update_profile 用无门控 senderID（chatkey.SenderIDFromContext），
	// 非隔离 entrypoint 也能写自己的 user.md（#127 独立于 data_isolation）。
	ws := t.TempDir()
	reg := NewBuiltinRegistry(ws, []string{"update_profile"}, SandboxRoots{}, ws)
	ctx := ctxWithSenderNoIsolation("ou_alice") // DataIsolation=false

	out, err := execute(reg, ctx, "update_profile", map[string]any{
		"section": "身份", "content": "- name: Alice\n",
	})
	if err != nil {
		t.Fatalf("update_profile under non-isolated entrypoint: %v", err)
	}
	if out["updated"] != true {
		t.Errorf("updated = %v, want true", out["updated"])
	}
	p, _ := loadUserProfile(UserProfilePath(ws, "ou_alice"))
	if !strings.Contains(p.Content, "Alice") {
		t.Errorf("user.md not written under non-isolated entrypoint:\n%s", p.Content)
	}
}

// --- PR #147 review blocker 4: command.run/shell.run 碰不到 user.md ---

func TestUserProfilePath_OutsideWorkspaceNotAccessibleByShell(t *testing.T) {
	// user.md 在 stateDir（非 workspace），command/shell 的 cwd 在 workspace，
	// exec 的进程碰不到 stateDir。验证路径拓扑：user.md 不在 workspace 树内。
	ws := t.TempDir()
	stateDir := t.TempDir()
	userPath := UserProfilePath(stateDir, "ou_target")
	mustWrite(t, userPath, "secret profile")
	// user.md 路径不在 workspace 下
	if strings.HasPrefix(userPath, ws) {
		t.Fatalf("user.md %q should not be under workspace %q", userPath, ws)
	}
	// workspace 下确实没有 users/ 目录（user.md 不在那）
	if _, err := os.Stat(filepath.Join(ws, "users")); !os.IsNotExist(err) {
		t.Errorf("workspace/users/ should not exist (user.md moved to stateDir)")
	}
}
