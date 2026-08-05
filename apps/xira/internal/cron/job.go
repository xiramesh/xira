package cron

import "errors"

// ValidateJob 检查 Job 字段一致性（RFC §5.1、§7.1）：
//   - type 与时间字段一致：recurring 配 expression / oneshot 配 fire_at，互斥
//   - Principal 完整、AgentID 非空、SchemaVersion/State 合法
func ValidateJob(j CronJob) error {
	panic("not implemented: 切片 2 落地（RFC §5.1、§7.1）")
}

// CheckJobStateTransition 校验状态转换合法性（RFC §5.2、§7.1）：
//   - recurring 在 enabled/paused/deleted 之间流转
//   - oneshot 可从 enabled 进 completed，不支持 pause/resume
//   - completed 和 deleted 是终态
func CheckJobStateTransition(from, to JobState, jobType JobType) error {
	panic("not implemented: 切片 2 落地（RFC §5.2、§7.1）")
}

// IsTerminalJobState 返回 true 当 state 是终态（completed/deleted）。
func IsTerminalJobState(s JobState) bool {
	panic("not implemented: 切片 2 落地")
}

var (
	errInvalidJobType      = errors.New("cron: invalid job type")
	errInvalidJobState     = errors.New("cron: invalid job state")
	errExpressionRequired  = errors.New("cron: recurring job requires expression")
	errFireAtRequired      = errors.New("cron: oneshot job requires fire_at")
	errExpressionFireMutex = errors.New("cron: expression and fire_at are mutually exclusive")
	errTerminalState       = errors.New("cron: cannot transition from terminal state")
)
