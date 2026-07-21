package cron

import "errors"

// 切片 1（#192）只冻结契约。下面这些行为函数是测试的规格（red），
// 实现由切片 2 落地。留桩确保测试可编译、可运行，但行为必然失败。
//
// 实现时把 panic("not implemented") 替换为真实逻辑，测试从 red 转 green。

// NormalizePrincipal 按规范化规则处理 Principal（RFC §3.1）：
//   - EntrypointID / Channel / SenderIDType：trim + lowercase
//   - SenderID：仅 trim，不 lowercase（外部 ID 可能区分大小写）
func NormalizePrincipal(p CronPrincipal) CronPrincipal {
	panic("not implemented: 切片 2 落地（RFC §3.1）")
}

// ValidatePrincipal 检查 Principal 是否完整可用。
// typed identity（SenderIDType + SenderID）为空时拒绝（RFC §3.1）。
// 在 NormalizePrincipal 之后做。
func ValidatePrincipal(p CronPrincipal) error {
	panic("not implemented: 切片 2 落地（RFC §3.1）")
}

// PrincipalHash 返回 Principal 四元组的 versioned SHA-256（RFC §3.1、§10）。
// 用于文件路径和 per-sender namespace 隔离。version 前缀见 PrincipalHashVersion。
func PrincipalHash(p CronPrincipal) string {
	panic("not implemented: 切片 2 落地（RFC §3.1、§10）")
}

// validateErrs 复用错误，避免散落字符串。
var (
	errEmptyTypedIdentity = errors.New("cron: typed identity required (sender_id_type + sender_id)")
	errEmptyEntrypoint    = errors.New("cron: entrypoint_id required")
	errEmptyChannel       = errors.New("cron: channel required")
)
