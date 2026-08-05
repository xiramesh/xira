package cron

// CheckExecutionTransition 校验 execution 状态机转换合法性（RFC §7.3）。
//
// 正常路径：claimed → queued → running → completed/failed
// skip 路径：claimed → skipped_overlap/skipped_misfire/skipped_quota
// interrupted：running/queued → interrupted
// blocked 可逆：queued ↔ blocked
//
// completed/failed/skipped_*/interrupted 是终态。
func CheckExecutionTransition(from, to ExecutionStatus) error {
	panic("not implemented: 切片 2 落地（RFC §7.3）")
}

// CheckDeliveryTransition 校验 delivery 状态机转换合法性（RFC §7.3、§11.1）。
//
// pending → sent/not_needed/failed/not_allowed 四种终态。
// delivery 和 execution 是独立状态机——投递失败不能重跑 Agent。
func CheckDeliveryTransition(from, to DeliveryStatus) error {
	panic("not implemented: 切片 2 落地（RFC §7.3、§11.1）")
}

// IsTerminalExecution 返回 true 当 status 是终态。
func IsTerminalExecution(s ExecutionStatus) bool {
	panic("not implemented: 切片 2 落地")
}

// IsTerminalDelivery 返回 true 当 status 是终态。
func IsTerminalDelivery(s DeliveryStatus) bool {
	panic("not implemented: 切片 2 落地")
}
