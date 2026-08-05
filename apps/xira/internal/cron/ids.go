package cron

import "time"

// CreateKey 计算 create 幂等键（RFC §5.3）：
//
//	CreateKey = sha256("cron-create-v1\x00" + runID + "\x00" + callID)
//
// 同一 (runID, callID) 返回同一 key，用于同一 tool call 重试时返回原 Job；
// deleted tombstone 保留 CreateKey，旧 call 重放也不重建。
//
// 注意：本公式假设 ADK 层重试 tool call 时 tool_call_id 稳定——切片 4 落地时
// 必须实测核实这个假设（#195）。
func CreateKey(runID, callID string) string {
	panic("not implemented: 切片 2 落地（RFC §5.3）")
}

// FireID 计算 Fire claim 的幂等键（RFC §7.3）：
//
//	FireID = sha256("cron-fire-v1\x00" + jobID + "\x00" + scheduledAtUTC)
//
// scheduledAtUTC 在 hash 前必须规范化到 UTC（time.Time.UTC()），保证不同时区
// 表达的同一时刻产生同一 FireID。
func FireID(jobID string, scheduledAtUTC time.Time) string {
	panic("not implemented: 切片 2 落地（RFC §7.3）")
}

// createKeyVersion / fireIDVersion 是公式的 versioned 前缀。改公式必须 bump，
// 并同步更新 principal_test.go / createkey_test.go / fireid_test.go 的 golden value。
const (
	createKeyVersion = "cron-create-v1"
	fireIDVersion    = "cron-fire-v1"
)
