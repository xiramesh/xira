package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiramesh/xira/internal/chatkey"
	"github.com/xiramesh/xira/internal/session"
)

// isolation.go 实现 per-sender 数据隔离（#126）的 overlay 两级结构：
//
//	通用层（workspace 根）    ← 部署者预置：kb/ skills/ agents/ 等，只读
//	私有层（workspace/users/sender_{id}/）← 用户写的，per-sender 隔离
//
// 写操作（write_file/edit_file）永远落私有层；读操作（read_file/list_dir/
// search_file）先查私有层，没有 fallback 通用层；通用层只读。
//
// 部署者在 entrypoint 配 data_isolation.enabled: true 启用（Step 5 接线）。
// 未启用（senderID 为空）→ resolvePrivateRoot 返回 ""，上层走单层逻辑（现状）。

// privateRootSegment 是私有层在 workspace 下的子目录前缀。
const privateRootSegment = "users"

// senderIDFromCtx 从 ctx 取 senderID（#126 overlay 隔离用）。
// 只有 entrypoint 开启 data_isolation（ChatKey.DataIsolation=true）且 SenderID 非空时，
// 才返回 senderID（走 overlay）；否则返回 ""（走单层，向后兼容）。
func senderIDFromCtx(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if !chatkey.DataIsolationEnabledFromContext(ctx) {
		return ""
	}
	id, _ := chatkey.SenderIDFromContext(ctx)
	return strings.TrimSpace(id)
}

// resolvePrivateRoot 算出 sender 的私有层根目录：
//
//	workspaceRoot/users/sender_{safePathID(senderID)}
//
// senderID 为空（TrimSpace 后）→ 返回 ""，表示「未启用隔离」，上层走单层逻辑。
// senderID 经 session.SafePathID 清洗，保证不含 / .. 等危险字符（防路径穿越）。
//
// coverage: contract (100% required)
func resolvePrivateRoot(workspaceRoot, senderID string) string {
	senderID = strings.TrimSpace(senderID)
	if senderID == "" {
		return ""
	}
	safe := session.SafePathID(senderID)
	return filepath.Join(workspaceRoot, privateRootSegment, "sender_"+safe)
}

// resolveWrite 把 rawPath 解析为写入的绝对路径。Overlay 语义：
//
//   - 隔离启用（senderID 非空）：相对路径永远落私有层，且必须在私有层 root 内
//     （用 ../ 逃逸私有层会被拒，即使通用层在 writeRoots 里）。
//   - 隔离未启用（senderID 空）：退化为单层——相对路径 Join workspaceRoot，
//     在 writeRoots 内即可（现有 resolveWithin 行为）。
//   - 绝对路径：边界检查（在 writeRoots 内），不 rewrite。
//
// coverage: contract (100% required)
func resolveWrite(rawPath, workspaceRoot, senderID string, writeRoots []string) (string, error) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return "", errPathRequired
	}
	privateRoot := resolvePrivateRoot(workspaceRoot, senderID)

	// 绝对路径：不 rewrite。隔离启用时必须落在私有层内（防借绝对路径写通用层 /
	// 他人私有层——review Bypass 2）；隔离未启用时只要在 writeRoots 内即可。
	if filepath.IsAbs(rawPath) {
		abs := filepath.Clean(rawPath)
		if privateRoot != "" {
			if !pathWithinRoots(abs, []string{privateRoot}) {
				return "", errPathOutsideRoots
			}
			return abs, nil
		}
		if !pathWithinRoots(abs, writeRoots) {
			return "", errPathOutsideRoots
		}
		return abs, nil
	}

	// 相对路径。
	if privateRoot != "" {
		// 隔离启用：落私有层，且必须留在私有层（防 ../ 逃逸写通用层）。
		abs := filepath.Clean(filepath.Join(privateRoot, rawPath))
		if !pathWithinRoots(abs, []string{privateRoot}) {
			return "", errPathOutsideRoots
		}
		return abs, nil
	}
	// 隔离未启用：现有行为（Join workspaceRoot，在 writeRoots 内）。
	abs := filepath.Clean(filepath.Join(workspaceRoot, rawPath))
	if !pathWithinRoots(abs, writeRoots) {
		return "", errPathOutsideRoots
	}
	return abs, nil
}

// resolveRead 把 rawPath 解析为读取的绝对路径。Overlay 语义：
//
//   - 绝对路径：不 rewrite，只做边界检查。
//   - 隔离启用（senderID 非空）+ 相对路径：先查私有层（文件存在 → 用私有层），
//     私有层不存在 → fallback 通用层（Join workspaceRoot）。两层都在 readRoots 内。
//   - 隔离未启用（senderID 空）：退化为单层（现有 resolveWithin 行为）。
//
// coverage: contract (100% required)
func resolveRead(rawPath, workspaceRoot, senderID string, readRoots []string) (string, error) {
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return "", errPathRequired
	}
	privateRoot := resolvePrivateRoot(workspaceRoot, senderID)

	// 绝对路径：不 rewrite，但隔离启用时禁止落在「他人私有层」内
	// （防借绝对路径读 users/sender_其他/...）。
	if filepath.IsAbs(rawPath) {
		abs := filepath.Clean(rawPath)
		if !pathWithinRoots(abs, readRoots) {
			return "", errPathOutsideRoots
		}
		if privateRoot != "" && isInPrivateNamespace(abs, workspaceRoot) && !pathWithinRoots(abs, []string{privateRoot}) {
			return "", errPathOutsideRoots
		}
		return abs, nil
	}

	// 相对路径。
	if privateRoot != "" {
		// 隔离启用：先查私有层。
		privateAbs := filepath.Clean(filepath.Join(privateRoot, rawPath))
		if _, err := os.Stat(privateAbs); err == nil {
			// 私有层存在 → 用私有层（边界检查防 symlink 逃逸）。
			if !pathWithinRoots(privateAbs, []string{privateRoot}) {
				return "", errPathOutsideRoots
			}
			return privateAbs, nil
		}
		// 私有层不存在 → fallback 通用层。但禁止借 fallback 读他人私有层
		// （workspace/users/ 是所有 sender 私有目录的物理位置，物理上在通用层树内）。
		commonAbs := filepath.Clean(filepath.Join(workspaceRoot, rawPath))
		if !pathWithinRoots(commonAbs, readRoots) {
			return "", errPathOutsideRoots
		}
		if isInPrivateNamespace(commonAbs, workspaceRoot) && !pathWithinRoots(commonAbs, []string{privateRoot}) {
			return "", errPathOutsideRoots
		}
		return commonAbs, nil
	}
	// 隔离未启用：现有行为。
	abs := filepath.Clean(filepath.Join(workspaceRoot, rawPath))
	if !pathWithinRoots(abs, readRoots) {
		return "", errPathOutsideRoots
	}
	return abs, nil
}

var (
	errPathRequired     = pathErr("path is required")
	errPathOutsideRoots = pathErr("path must stay within allowed roots")
)

type pathErr string

func (e pathErr) Error() string { return string(e) }

// isInPrivateNamespace 报告 absPath 是否落在 workspace 的「私有命名空间」
// （workspaceRoot/users/...）下。所有 sender 的私有目录物理上都挂在这个子树里，
// 所以隔离启用时必须额外检查：路径若在这个命名空间下，只允许是当前 sender 自己的
// 私有层（由调用方再用 pathWithinRoots(abs, []string{privateRoot}) 收紧）。
// 这堵住 review Bypass 1：Alice 借 fallback 读 workspace/users/sender_ou_bob/...。
func isInPrivateNamespace(absPath, workspaceRoot string) bool {
	privateTree := filepath.Join(workspaceRoot, privateRootSegment) + string(filepath.Separator)
	return strings.HasPrefix(filepath.Clean(absPath)+string(filepath.Separator), privateTree)
}
