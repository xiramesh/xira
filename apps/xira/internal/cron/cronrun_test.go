package cron

import "testing"

func TestCheckExecutionTransition(t *testing.T) {
	// 覆盖 RFC §7.3 execution 状态机的关键转换
	// 不强求枚举所有 (from, to) 组合，但要覆盖关键路径和禁止路径
	tests := []struct {
		name    string
		from    ExecutionStatus
		to      ExecutionStatus
		wantErr bool
	}{
		// claim → queued → running → completed 正常路径
		{"claimed → queued", ExecutionClaimed, ExecutionQueued, false},
		{"queued → running", ExecutionQueued, ExecutionRunning, false},
		{"running → completed", ExecutionRunning, ExecutionCompleted, false},
		{"running → failed", ExecutionRunning, ExecutionFailed, false},

		// skipped 路径
		{"claimed → skipped_overlap", ExecutionClaimed, ExecutionSkippedOverlap, false},
		{"claimed → skipped_misfire", ExecutionClaimed, ExecutionSkippedMisfire, false},
		{"claimed → skipped_quota", ExecutionClaimed, ExecutionSkippedQuota, false},

		// interrupted
		{"running → interrupted", ExecutionRunning, ExecutionInterrupted, false},
		{"queued → interrupted", ExecutionQueued, ExecutionInterrupted, false},

		// blocked
		{"queued → blocked", ExecutionQueued, ExecutionBlocked, false},
		{"blocked → queued", ExecutionBlocked, ExecutionQueued, false},

		// 终态不可流转
		{"completed 是终态，不能转出", ExecutionCompleted, ExecutionRunning, true},
		{"failed 是终态，不能转出", ExecutionFailed, ExecutionRunning, true},
		{"interrupted 是终态，不能转出", ExecutionInterrupted, ExecutionRunning, true},

		// 倒退禁止
		{"running → claimed 倒退拒绝", ExecutionRunning, ExecutionClaimed, true},
		{"completed → queued 倒退拒绝", ExecutionCompleted, ExecutionQueued, true},

		// 非法值
		{"非法 to 拒绝", ExecutionClaimed, ExecutionStatus("weird"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckExecutionTransition(tt.from, tt.to)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckExecutionTransition(%s→%s) error = %v, wantErr %v",
					tt.from, tt.to, err, tt.wantErr)
			}
		})
	}
}

func TestCheckDeliveryTransition(t *testing.T) {
	tests := []struct {
		name    string
		from    DeliveryStatus
		to      DeliveryStatus
		wantErr bool
	}{
		// pending → sent/not_needed/failed/not_allowed
		{"pending → sent", DeliveryPending, DeliverySent, false},
		{"pending → not_needed", DeliveryPending, DeliveryNotNeeded, false},
		{"pending → failed", DeliveryPending, DeliveryFailed, false},
		{"pending → not_allowed", DeliveryPending, DeliveryNotAllowed, false},

		// 终态不可流转
		{"sent 是终态", DeliverySent, DeliveryPending, true},
		{"not_needed 是终态", DeliveryNotNeeded, DeliveryPending, true},
		{"failed 是终态", DeliveryFailed, DeliveryPending, true},
		{"not_allowed 是终态", DeliveryNotAllowed, DeliveryPending, true},

		// 非法值
		{"非法 to 拒绝", DeliveryPending, DeliveryStatus("weird"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckDeliveryTransition(tt.from, tt.to)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckDeliveryTransition(%s→%s) error = %v, wantErr %v",
					tt.from, tt.to, err, tt.wantErr)
			}
		})
	}
}

func TestExecutionIsTerminal(t *testing.T) {
	terminals := []ExecutionStatus{
		ExecutionCompleted, ExecutionFailed,
		ExecutionSkippedOverlap, ExecutionSkippedMisfire, ExecutionSkippedQuota,
		ExecutionInterrupted,
	}
	nonTerminals := []ExecutionStatus{
		ExecutionClaimed, ExecutionQueued, ExecutionRunning, ExecutionBlocked,
	}
	for _, s := range terminals {
		if !IsTerminalExecution(s) {
			t.Errorf("IsTerminalExecution(%s) = false, want true", s)
		}
	}
	for _, s := range nonTerminals {
		if IsTerminalExecution(s) {
			t.Errorf("IsTerminalExecution(%s) = true, want false", s)
		}
	}
}

func TestDeliveryIsTerminal(t *testing.T) {
	terminals := []DeliveryStatus{
		DeliverySent, DeliveryNotNeeded, DeliveryFailed, DeliveryNotAllowed,
	}
	nonTerminals := []DeliveryStatus{DeliveryPending}
	for _, s := range terminals {
		if !IsTerminalDelivery(s) {
			t.Errorf("IsTerminalDelivery(%s) = false, want true", s)
		}
	}
	for _, s := range nonTerminals {
		if IsTerminalDelivery(s) {
			t.Errorf("IsTerminalDelivery(%s) = true, want false", s)
		}
	}
}
