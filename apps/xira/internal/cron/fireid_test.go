package cron

import (
	"testing"
	"time"
)

// fireIDGolden 锁定 FireID 公式：sha256("cron-fire-v1\x00<jobID>\x00<scheduledAtUTC>")。
const fireIDGolden = "REPLACE_WITH_REAL_HASH"

func TestFireID(t *testing.T) {
	scheduledAt := time.Date(2026, 7, 21, 23, 0, 0, 0, time.UTC)

	t.Run("versioned 前缀 + 公式 golden value", func(t *testing.T) {
		got := FireID("cron_01TEST", scheduledAt)
		if got != fireIDGolden {
			t.Errorf("FireID = %q, want golden %q", got, fireIDGolden)
		}
	})

	t.Run("jobID 不同 → FireID 不同", func(t *testing.T) {
		a := FireID("job_a", scheduledAt)
		b := FireID("job_b", scheduledAt)
		if a == b {
			t.Errorf("不同 jobID 应产生不同 FireID")
		}
	})

	t.Run("scheduledAt 不同 → FireID 不同（同一 job）", func(t *testing.T) {
		other := scheduledAt.Add(time.Hour)
		a := FireID("job_x", scheduledAt)
		b := FireID("job_x", other)
		if a == b {
			t.Errorf("不同 scheduledAt 应产生不同 FireID")
		}
	})

	t.Run("同一 (jobID, scheduledAt) → 同一 FireID（幂等基础）", func(t *testing.T) {
		a := FireID("job_x", scheduledAt)
		b := FireID("job_x", scheduledAt)
		if a != b {
			t.Errorf("同一 (jobID, scheduledAt) 应产生同一 FireID")
		}
	})

	t.Run("UTC 规范化：不同时区同一时刻 → 同一 FireID", func(t *testing.T) {
		cst := time.Date(2026, 7, 22, 7, 0, 0, 0, time.FixedZone("CST", 8*3600))
		utc := time.Date(2026, 7, 21, 23, 0, 0, 0, time.UTC)
		// 两者表示同一时刻
		if !cst.Equal(utc) {
			t.Fatalf("测试前提错误：CST 和 UTC 不表示同一时刻")
		}
		a := FireID("job_x", cst)
		b := FireID("job_x", utc)
		if a != b {
			t.Errorf("同一 UTC 时刻（不同时区表达）应产生同一 FireID")
		}
	})

	t.Run("jobID 含 NUL 字符也不冲突", func(t *testing.T) {
		a := FireID("ab\x00cd", scheduledAt)
		b := FireID("ab", scheduledAt)
		// 即使 jobID 差异在 NUL 之后，FireID 也应不同
		if a == b {
			t.Errorf("不同 jobID（含 NUL）应产生不同 FireID")
		}
	})
}
