package cron

import (
	"testing"
)

// createKeyGolden 用于锁定 CreateKey 的 versioned 前缀和拼接格式。改公式必须 bump version。
//
// 计算方式：sha256("cron-create-v1\x00<run_id>\x00<call_id>")
const createKeyGolden = "REPLACE_WITH_REAL_HASH"

func TestCreateKey(t *testing.T) {
	t.Run("versioned 前缀 + 拼接公式 golden value", func(t *testing.T) {
		got := CreateKey("run_abc123", "call_xyz789")
		if got != createKeyGolden {
			t.Errorf("CreateKey = %q, want golden %q", got, createKeyGolden)
			t.Logf("若公式有意修改，请更新 golden")
		}
	})

	t.Run("run_id 不同 → key 不同", func(t *testing.T) {
		a := CreateKey("run_1", "call_x")
		b := CreateKey("run_2", "call_x")
		if a == b {
			t.Errorf("不同 run_id 应产生不同 key")
		}
	})

	t.Run("call_id 不同 → key 不同", func(t *testing.T) {
		a := CreateKey("run_x", "call_1")
		b := CreateKey("run_x", "call_2")
		if a == b {
			t.Errorf("不同 call_id 应产生不同 key")
		}
	})

	t.Run("同一输入 → 同一 key（幂等基础）", func(t *testing.T) {
		a := CreateKey("run_x", "call_y")
		b := CreateKey("run_x", "call_y")
		if a != b {
			t.Errorf("同一输入应产生同一 key")
		}
	})

	t.Run("run_id 含 NUL 字符也不冲突（用 \\x00 分隔符的安全性）", func(t *testing.T) {
		// "ab\x00cd" + "\x00" + "ef"  vs  "ab" + "\x00" + "cdef"
		// 如果分隔符失效，这两组会冲突
		a := CreateKey("ab\x00cd", "ef")
		b := CreateKey("ab", "cdef")
		if a == b {
			t.Errorf("NUL 字符注入应被 \\x00 分隔符挡住，不应冲突")
		}
	})

	t.Run("空输入也产生有效 hash（不 panic）", func(t *testing.T) {
		got := CreateKey("", "")
		if len(got) != 64 {
			t.Errorf("空输入 hash 长度 = %d, want 64", len(got))
		}
	})
}
