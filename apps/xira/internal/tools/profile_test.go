package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiramesh/xira/internal/chatkey"
)

// UserProfilePath 算出 sender 的 user.md 路径（#127）。
// 独立于 #126 的 data_isolation 开关——每个 sender 无条件有 user.md。

func TestUserProfilePath(t *testing.T) {
	ws := "/data/workspace"
	cases := []struct {
		name     string
		senderID string
		want     string
	}{
		{"normal", "ou_大明", filepath.Join(ws, "users", "sender_ou_大明", "user.md")},
		{"ascii", "wxid_abc", filepath.Join(ws, "users", "sender_wxid_abc", "user.md")},
		{"local user", "local-user", filepath.Join(ws, "users", "sender_local-user", "user.md")},
		{"empty sender -> no path", "", ""},
		{"whitespace -> no path", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UserProfilePath(ws, tc.senderID)
			if got != tc.want {
				t.Errorf("UserProfilePath(%q) = %q, want %q", tc.senderID, got, tc.want)
			}
		})
	}
}

// 路径必须和 #126 的 resolvePrivateRoot 对齐（读写 round-trip）。
func TestUserProfilePath_AlignsWithPrivateRoot(t *testing.T) {
	ws := "/data/workspace"
	sender := "ou_test"
	privateRoot := resolvePrivateRoot(ws, sender) // #126 的私有层根
	userPath := UserProfilePath(ws, sender)
	if userPath != filepath.Join(privateRoot, "user.md") {
		t.Errorf("UserProfilePath %q != resolvePrivateRoot+user.md (%q)", userPath, filepath.Join(privateRoot, "user.md"))
	}
}

// --- Step 2: user.md 读写 store ---

func TestLoadUserProfile_NonexistentReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "user.md")
	p, err := loadUserProfile(path)
	if err != nil {
		t.Fatalf("load nonexistent: %v", err)
	}
	if p.Content != "" {
		t.Errorf("nonexistent profile content = %q, want empty", p.Content)
	}
	if p.Exists {
		t.Error("nonexistent profile should have Exists=false")
	}
}

func TestLoadUserProfile_ExistingReadsContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "user.md")
	body := "# 用户档案\n\n## 偏好\n- name: 大明\n"
	os.WriteFile(path, []byte(body), 0o600)

	p, err := loadUserProfile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !p.Exists {
		t.Error("existing profile should have Exists=true")
	}
	if p.Content != body {
		t.Errorf("content = %q, want %q", p.Content, body)
	}
}

func TestUpdateProfileSection_CreateNewFile(t *testing.T) {
	// user.md 不存在 → updateSection 首次创建（MkdirAll + 写）
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "user.md") // 父目录不存在

	err := updateProfileSection(path, "偏好", "- name: 大明\n- reply_style: 简洁\n")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	p, _ := loadUserProfile(path)
	if !p.Exists {
		t.Fatal("file not created")
	}
	if p.Content == "" {
		t.Error("created file should have content")
	}
}

func TestUpdateProfileSection_ReplaceExistingSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "user.md")
	os.WriteFile(path, []byte("# 用户档案\n\n## 偏好\n- old value\n\n## 身份\n- sender: ou_x\n"), 0o600)

	err := updateProfileSection(path, "偏好", "- name: 大明\n- reply_style: 简洁\n")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	p, _ := loadUserProfile(path)
	// 偏好 section 被替换
	if !contains(p.Content, "- name: 大明") || !contains(p.Content, "- reply_style: 简洁") {
		t.Errorf("偏好 section not updated:\n%s", p.Content)
	}
	if contains(p.Content, "- old value") {
		t.Errorf("old 偏好 value not replaced:\n%s", p.Content)
	}
	// 身份 section 不受影响
	if !contains(p.Content, "- sender: ou_x") {
		t.Errorf("身份 section should be untouched:\n%s", p.Content)
	}
}

func TestUpdateProfileSection_AppendNewSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "user.md")
	os.WriteFile(path, []byte("# 用户档案\n\n## 身份\n- sender: ou_x\n"), 0o600)

	err := updateProfileSection(path, "偏好", "- name: 大明\n")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	p, _ := loadUserProfile(path)
	if !contains(p.Content, "## 偏好") {
		t.Errorf("new 偏好 section not appended:\n%s", p.Content)
	}
	if !contains(p.Content, "- name: 大明") {
		t.Errorf("appended content missing:\n%s", p.Content)
	}
	// 原身份 section 保留
	if !contains(p.Content, "## 身份") {
		t.Errorf("original section lost:\n%s", p.Content)
	}
}

func TestUpdateProfileSection_RoundTripMultiple(t *testing.T) {
	// 连续多次更新不同 section → 都保留
	dir := t.TempDir()
	path := filepath.Join(dir, "user.md")

	updateProfileSection(path, "身份", "- name: 大明\n")
	updateProfileSection(path, "偏好", "- reply_style: 简洁\n")
	updateProfileSection(path, "背景", "- role: 产品经理\n")

	p, _ := loadUserProfile(path)
	for _, want := range []string{"大明", "简洁", "产品经理", "## 身份", "## 偏好", "## 背景"} {
		if !contains(p.Content, want) {
			t.Errorf("round-trip missing %q:\n%s", want, p.Content)
		}
	}
}

func TestUserProfileFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "user.md")
	updateProfileSection(path, "偏好", "- x\n")
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("user.md perm = %o, want 0600", info.Mode().Perm())
	}
}

func TestLoadUserProfile_ReadError(t *testing.T) {
	// 路径是目录（不是文件）→ ReadFile 失败（非 IsNotExist）→ 报错
	dir := t.TempDir()
	path := filepath.Join(dir, "user.md")
	os.MkdirAll(path, 0o700) // user.md 是个目录
	_, err := loadUserProfile(path)
	if err == nil {
		t.Error("loadUserProfile on a directory should error")
	}
}

func TestUpdateProfileSection_MkdirFail(t *testing.T) {
	// 父目录只读 → MkdirAll 失败（非根用户）
	ws := t.TempDir()
	ro := filepath.Join(ws, "readonly_root")
	os.MkdirAll(ro, 0o500)
	defer os.Chmod(ro, 0o700)
	// user.md 落 readonly_root/sub/user.md，sub 建不出来
	path := filepath.Join(ro, "sub", "user.md")
	err := updateProfileSection(path, "偏好", "- x\n")
	if err == nil && os.Geteuid() != 0 {
		t.Error("updateProfileSection into readonly parent should fail")
	}
}

func TestUpdateProfileTool_EmptySectionRejected(t *testing.T) {
	ws := t.TempDir()
	reg := NewBuiltinRegistry(ws, []string{"update_profile"}, SandboxRoots{})
	ctx := ctxWithSenderProfile("ou_x")
	if _, err := reg.Execute(ctx, "update_profile", map[string]any{"section": "  ", "content": "x"}); err == nil {
		t.Error("empty section should be rejected")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// --- Step 3: update_profile 工具 ---

func ctxWithSenderProfile(sender string) context.Context {
	// update_profile 用 chatkey.SenderIDFromContext（无 data_isolation 门控）。
	return chatkey.WithChatKey(context.Background(), chatkey.ChatKey{SenderID: sender})
}

func TestUpdateProfileTool_WritesUserMd(t *testing.T) {
	ws := t.TempDir()
	reg := NewBuiltinRegistry(ws, []string{"update_profile"}, SandboxRoots{})
	ctx := ctxWithSenderProfile("ou_大明")

	out, err := reg.Execute(ctx, "update_profile", map[string]any{
		"section": "偏好", "content": "- name: 大明\n- reply_style: 简洁\n",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out["updated"] != true {
		t.Errorf("updated = %v, want true", out["updated"])
	}
	// 文件落在 users/sender_ou_大明/user.md
	path := UserProfilePath(ws, "ou_大明")
	p, _ := loadUserProfile(path)
	if !p.Exists {
		t.Fatal("user.md not created")
	}
	if !contains(p.Content, "大明") {
		t.Errorf("content missing 大明:\n%s", p.Content)
	}
}

func TestUpdateProfileTool_NoSenderRejected(t *testing.T) {
	// 没 sender（ctx 无 ChatKey）→ 报错（写哪？）
	ws := t.TempDir()
	reg := NewBuiltinRegistry(ws, []string{"update_profile"}, SandboxRoots{})
	_, err := reg.Execute(context.Background(), "update_profile", map[string]any{
		"section": "偏好", "content": "x",
	})
	if err == nil {
		t.Error("update_profile without sender should error")
	}
}

func TestUpdateProfileTool_MissingArgsRejected(t *testing.T) {
	ws := t.TempDir()
	reg := NewBuiltinRegistry(ws, []string{"update_profile"}, SandboxRoots{})
	ctx := ctxWithSenderProfile("ou_x")
	// 缺 content
	if _, err := reg.Execute(ctx, "update_profile", map[string]any{"section": "偏好"}); err == nil {
		t.Error("missing content should error")
	}
	// 缺 section
	if _, err := reg.Execute(ctx, "update_profile", map[string]any{"content": "x"}); err == nil {
		t.Error("missing section should error")
	}
}

func TestUpdateProfileTool_IncrementalAcrossSections(t *testing.T) {
	// 同一 sender 连续更新不同 section → 都保留（增量，非覆盖）
	ws := t.TempDir()
	reg := NewBuiltinRegistry(ws, []string{"update_profile"}, SandboxRoots{})
	ctx := ctxWithSenderProfile("ou_a")

	reg.Execute(ctx, "update_profile", map[string]any{"section": "身份", "content": "- name: Alice\n"})
	reg.Execute(ctx, "update_profile", map[string]any{"section": "偏好", "content": "- lang: en\n"})

	p, _ := loadUserProfile(UserProfilePath(ws, "ou_a"))
	for _, want := range []string{"Alice", "lang: en", "## 身份", "## 偏好"} {
		if !contains(p.Content, want) {
			t.Errorf("missing %q:\n%s", want, p.Content)
		}
	}
}

func TestUpdateProfileTool_PerSenderIsolated(t *testing.T) {
	// Alice 和 Bob 各自的 user.md 互不干扰
	ws := t.TempDir()
	reg := NewBuiltinRegistry(ws, []string{"update_profile"}, SandboxRoots{})

	reg.Execute(ctxWithSenderProfile("ou_alice"), "update_profile", map[string]any{
		"section": "身份", "content": "- name: Alice\n",
	})
	reg.Execute(ctxWithSenderProfile("ou_bob"), "update_profile", map[string]any{
		"section": "身份", "content": "- name: Bob\n",
	})

	alicePath := UserProfilePath(ws, "ou_alice")
	bobPath := UserProfilePath(ws, "ou_bob")
	if alicePath == bobPath {
		t.Fatal("Alice and Bob paths identical")
	}
	a, _ := loadUserProfile(alicePath)
	b, _ := loadUserProfile(bobPath)
	if contains(a.Content, "Bob") {
		t.Error("Alice's profile leaked Bob's data")
	}
	if contains(b.Content, "Alice") {
		t.Error("Bob's profile leaked Alice's data")
	}
}
