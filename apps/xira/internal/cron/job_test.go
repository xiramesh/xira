package cron

import (
	"testing"
	"time"
)

func TestValidateJob(t *testing.T) {
	validRecurring := func() CronJob {
		return CronJob{
			SchemaVersion: CronJobSchemaV1,
			ID:            "cron_01TEST",
			Principal: CronPrincipal{
				EntrypointID: "feishu-main",
				Channel:      "feishu",
				SenderIDType: "feishu_open_id",
				SenderID:     "ou_yinwm",
			},
			Type:       JobTypeRecurring,
			AgentID:    "xira-assistant",
			Name:       "工作日销售日报",
			Expression: "0 9 * * 1-5",
			Timezone:   "Asia/Shanghai",
			Prompt:     "检查昨天的销售数据。",
			State:      JobStateEnabled,
		}
	}

	validOneshot := func() CronJob {
		fireAt := time.Date(2026, 7, 28, 9, 0, 0, 0, time.FixedZone("CST", 8*3600))
		j := validRecurring()
		j.Type = JobTypeOneshot
		j.Expression = ""
		j.FireAt = &fireAt
		return j
	}

	tests := []struct {
		name    string
		mutate  func(CronJob) CronJob
		wantErr bool
	}{
		{"recurring 完整通过", func(j CronJob) CronJob { return j }, false},
		{"oneshot 完整通过", func(j CronJob) CronJob { return validOneshot() }, false},

		// type 与时间字段一致性
		{"recurring 缺 expression 拒绝", func(j CronJob) CronJob { j.Expression = ""; return j }, true},
		{"oneshot 缺 fire_at 拒绝", func(j CronJob) CronJob {
			j.Type = JobTypeOneshot
			j.Expression = ""
			j.FireAt = nil
			return j
		}, true},
		{"recurring 多带 fire_at 拒绝（互斥）", func(j CronJob) CronJob {
			fireAt := time.Now().Add(24 * time.Hour)
			j.FireAt = &fireAt
			return j
		}, true},
		{"oneshot 多带 expression 拒绝（互斥）", func(j CronJob) CronJob {
			// 从 validRecurring 起步：先把 type 改成 oneshot 并补 fire_at，
			// 再保留 expression，触发互斥拒绝
			fireAt := time.Date(2026, 7, 28, 9, 0, 0, 0, time.FixedZone("CST", 8*3600))
			j.Type = JobTypeOneshot
			j.FireAt = &fireAt
			// j.Expression 仍保留 recurring 的值
			return j
		}, true},
		{"type 空拒绝", func(j CronJob) CronJob { j.Type = ""; return j }, true},
		{"type 非法值拒绝", func(j CronJob) CronJob { j.Type = JobType("weird"); return j }, true},

		// Principal
		{"Principal typed identity 空拒绝", func(j CronJob) CronJob {
			j.Principal.SenderID = ""
			return j
		}, true},

		// Agent
		{"AgentID 空拒绝", func(j CronJob) CronJob { j.AgentID = ""; return j }, true},

		// 时间字段
		{"recurring expression 空（被前面分支覆盖，这里覆盖非空）", func(j CronJob) CronJob { return j }, false},

		// SchemaVersion
		{"SchemaVersion 空拒绝", func(j CronJob) CronJob { j.SchemaVersion = ""; return j }, true},

		// State
		{"State 空拒绝", func(j CronJob) CronJob { j.State = ""; return j }, true},
		{"State 非法拒绝", func(j CronJob) CronJob { j.State = JobState("weird"); return j }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := tt.mutate(validRecurring())
			err := ValidateJob(j)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateJob error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestJobStateTransition(t *testing.T) {
	tests := []struct {
		name    string
		from    JobState
		to      JobState
		jobType JobType
		wantErr bool
	}{
		// recurring 状态机
		{"recurring: enabled → paused", JobStateEnabled, JobStatePaused, JobTypeRecurring, false},
		{"recurring: paused → enabled", JobStatePaused, JobStateEnabled, JobTypeRecurring, false},
		{"recurring: enabled → deleted", JobStateEnabled, JobStateDeleted, JobTypeRecurring, false},
		{"recurring: paused → deleted", JobStatePaused, JobStateDeleted, JobTypeRecurring, false},

		// recurring 不能进 completed（只有 oneshot 能）
		{"recurring: enabled → completed 拒绝", JobStateEnabled, JobStateCompleted, JobTypeRecurring, true},
		{"recurring: paused → completed 拒绝", JobStatePaused, JobStateCompleted, JobTypeRecurring, true},

		// recurring pause/resume 允许
		// （已覆盖）

		// oneshot 状态机
		// oneshot fire 后直接进 completed
		{"oneshot: enabled → completed 允许", JobStateEnabled, JobStateCompleted, JobTypeOneshot, false},

		// oneshot 不支持 pause/resume（§5.2）
		{"oneshot: enabled → paused 拒绝（不支持 pause）", JobStateEnabled, JobStatePaused, JobTypeOneshot, true},
		{"oneshot: paused → enabled 拒绝（不支持 resume）", JobStatePaused, JobStateEnabled, JobTypeOneshot, true},

		// oneshot 也能 delete
		{"oneshot: enabled → deleted 允许", JobStateEnabled, JobStateDeleted, JobTypeOneshot, false},

		// 终态不可流转
		{"completed 是终态，不能转出", JobStateCompleted, JobStateEnabled, JobTypeOneshot, true},
		{"deleted 是终态，不能转出", JobStateDeleted, JobStateEnabled, JobTypeRecurring, true},
		{"deleted → completed 拒绝（终态）", JobStateDeleted, JobStateCompleted, JobTypeRecurring, true},

		// 非法目标
		{"非法目标 state 拒绝", JobStateEnabled, JobState("weird"), JobTypeRecurring, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckJobStateTransition(tt.from, tt.to, tt.jobType)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckJobStateTransition(%s→%s, %s) error = %v, wantErr %v",
					tt.from, tt.to, tt.jobType, err, tt.wantErr)
			}
		})
	}
}

func TestJobIsTerminal(t *testing.T) {
	tests := []struct {
		state JobState
		want  bool
	}{
		{JobStateEnabled, false},
		{JobStatePaused, false},
		{JobStateCompleted, true},
		{JobStateDeleted, true},
	}
	for _, tt := range tests {
		if got := IsTerminalJobState(tt.state); got != tt.want {
			t.Errorf("IsTerminalJobState(%s) = %v, want %v", tt.state, got, tt.want)
		}
	}
}
