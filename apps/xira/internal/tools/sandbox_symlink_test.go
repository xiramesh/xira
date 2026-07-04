package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// sandbox_symlink_test.go: #110 follow-up —— symlink 逃逸防护。
// pathWithinRoots 必须解析 symlink,否则 workspace 内 symlink 指向外部,
// agent 能绕过边界写到边界外。

// TestPathWithinRootsRejectsSymlinkEscape 是核心安全回归门:
// workspace 内建 symlink → 外部目录,写 symlink/inside 时 pathWithinRoots
// 必须返回 false(symlink 解析后路径在外部,不在 root 内)。
func TestPathWithinRootsRejectsSymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	// workspace/evil -> outside (symlink pointing out of bounds)
	evilLink := filepath.Join(workspace, "evil")
	if err := os.Symlink(outside, evilLink); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	// Write a target inside the symlinked dir.
	targetInside := filepath.Join(evilLink, "secret.txt")

	// pathWithinRoots with root=workspace: the target's literal path is
	// workspace/evil/secret.txt (in-bound by string), but after resolving
	// the symlink it's <outside>/secret.txt (out-of-bound). Must reject.
	if pathWithinRoots(targetInside, []string{workspace}) {
		t.Errorf("SYMLINK ESCAPE: pathWithinRoots accepted workspace/evil/secret.txt (symlink → outside); must reject after resolving symlink")
	}
}

// TestPathWithinRootsAllowsGenuineInBoundPath 证正常 in-bound 路径仍通过
// (symlink 防护不破坏正常写)。
func TestPathWithinRootsAllowsGenuineInBoundPath(t *testing.T) {
	workspace := t.TempDir()
	genuine := filepath.Join(workspace, "tasks", "task.md")
	if !pathWithinRoots(genuine, []string{workspace}) {
		t.Errorf("genuine in-bound path rejected: %s", genuine)
	}
}

// TestPathWithinRootsAllowsNewFileUnderExistingDir 证写新文件(文件不存在)
// 仍被正确判断为 in-bound —— EvalSymlinks 对不存在的最终文件会失败,
// resolveSymlinkSafe 要回退到已存在的父目录解析。
func TestPathWithinRootsAllowsNewFileUnderExistingDir(t *testing.T) {
	workspace := t.TempDir()
	subdir := filepath.Join(workspace, "sub")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// newFile 不存在,但父目录 subdir 存在。
	newFile := filepath.Join(subdir, "brandnew.md")
	if !pathWithinRoots(newFile, []string{workspace}) {
		t.Errorf("new file under existing in-bound dir rejected: %s", newFile)
	}
}

// TestPathWithinRootsRootItselfSymlinked 证 root 本身是 symlink 时也能正确
// 匹配(root 解析后与路径解析后一致)。
func TestPathWithinRootsRootItselfSymlinked(t *testing.T) {
	realRoot := t.TempDir()
	linkToRoot := filepath.Join(t.TempDir(), "linkroot")
	if err := os.Symlink(realRoot, linkToRoot); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	// Write a real file under realRoot, access via the symlinked root.
	realFile := filepath.Join(realRoot, "x.txt")
	if err := os.WriteFile(realFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !pathWithinRoots(realFile, []string{linkToRoot}) {
		t.Errorf("file under symlinked root rejected: real=%s root=%s", realFile, linkToRoot)
	}
}
