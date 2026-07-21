// Package cron 实现 Xira 的周期/一次性定时任务（Scheduled Turn）能力。
//
// 本包提供 Job、Run、Fire claim、Principal 等核心数据模型，以及 Scheduler、
// Manager、Store 等编排组件。模型与契约定义见
// docs/architecture/xira-cron-v0.zh.md（分支 codex/cron-design）。
//
// 切片 1（#192）只冻结类型与契约，行为实现由后续切片落地。
package cron

import "time"

// SchemaVersion 常量：持久化记录携带 schema version，便于后续迁移。
const (
	CronJobSchemaV1 = "cronjob.v1"
	CronRunSchemaV1 = "cronrun.v1"
)

// PrincipalVersion 用于 principal hash 的 versioned 前缀，避免未来调整规范化
// 规则时与历史 hash 冲突。
const PrincipalHashVersion = "cron-principal-v1"

// CronPrincipal 是 Cron Job 的所有权键和身份载体。
//
// 四元组 (EntrypointID, Channel, SenderIDType, SenderID) 唯一确定一个 sender
// 在某入口下的 namespace。模型与客户端均不能构造 Principal，只能由服务端从
// InboundContext（聊天 create 路径）或 Job 记录（Scheduled Turn 路径）派生。
//
// 规范化规则（RFC §3.1）：
//   - EntrypointID / Channel / SenderIDType：trim + lowercase
//   - SenderID：仅 trim，不 lowercase（外部 ID 可能区分大小写）
//   - typed identity（SenderIDType + SenderID）为空时不能创建 Cron
//
// 文件路径用四元组的 versioned SHA-256（见 PrincipalHash），不用 raw sender。
type CronPrincipal struct {
	EntrypointID string `json:"entrypoint_id"`
	Channel      string `json:"channel"`
	SenderIDType string `json:"sender_id_type"`
	SenderID     string `json:"sender_id"`
}

// JobType 区分周期任务和一次性任务（RFC §5.1、§7.1）。
type JobType string

const (
	// JobTypeRecurring 用 Expression + Timezone 触发，可多次 fire，支持 pause/resume。
	JobTypeRecurring JobType = "recurring"
	// JobTypeOneshot 用 FireAt 触发，fire 后 State 直接转 completed，不支持 pause/resume。
	JobTypeOneshot JobType = "oneshot"
)

// JobState 是 CronJob 的生命周期状态（RFC §7.1）。
//
// recurring 在 enabled/paused/deleted 之间流转；oneshot fire 后直接进 completed
// （execution=completed 或 failed/skipped_* 都进 completed，不重试）。
// completed 与 deleted 都是终态。
type JobState string

const (
	JobStateEnabled  JobState = "enabled"
	JobStatePaused   JobState = "paused"
	JobStateCompleted JobState = "completed" // 仅 oneshot 进入
	JobStateDeleted  JobState = "deleted"    // tombstone
)

// CronJob 是一份定时任务记录（RFC §7.1）。
//
// ID 使用 ULID，不含 sender、Prompt 或 ChatID。除 State 转换外，v0 不原地修改
// 字段；deleted 保留 tombstone。Principal 是唯一 recipient 来源——Job 不保存模型
// 选择的 recipient。
type CronJob struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`

	// 身份与所有权（Principal 是所有权键，不可由模型提供）
	Principal CronPrincipal `json:"principal"`

	// 类型与 Agent（Agent 由用户在 create 时刻显式选择并冻死，RFC §6.1）
	Type    JobType `json:"type"`
	AgentID string  `json:"agent_id"`

	// 用户可见元数据
	Name string `json:"name"`

	// 时间字段（recurring 用 Expression，oneshot 用 FireAt，两者互斥）
	Expression string     `json:"expression,omitempty"` // type=recurring 必填，严格五字段
	FireAt     *time.Time `json:"fire_at,omitempty"`    // type=oneshot 必填
	Timezone   string     `json:"timezone"`

	// 用户级任务输入（不进普通 Info 日志）
	Prompt string `json:"prompt"`

	// 生命周期状态
	State JobState `json:"state"`

	// 能力快照（RFC §6.2）：创建时刻 = 目标 Agent 工具 ∩ entrypoint/runtime 权限
	// 减去 Scheduled denylist；Fire 时 = 创建快照 ∩ 当前仍被允许的工具。
	AllowedToolsSnapshot []string `json:"allowed_tools_snapshot,omitempty"`

	// 幂等与溯源（RFC §5.3）
	CreateKey       string `json:"create_key"`
	CreatedByRunID  string `json:"created_by_run_id"`
	CreatedByCallID string `json:"created_by_call_id"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ExecutionStatus 是 CronRun 的执行状态机（RFC §7.3）。
type ExecutionStatus string

const (
	ExecutionClaimed        ExecutionStatus = "claimed"
	ExecutionQueued         ExecutionStatus = "queued"
	ExecutionRunning        ExecutionStatus = "running"
	ExecutionCompleted      ExecutionStatus = "completed"
	ExecutionFailed         ExecutionStatus = "failed"
	ExecutionSkippedOverlap ExecutionStatus = "skipped_overlap"
	ExecutionSkippedMisfire ExecutionStatus = "skipped_misfire"
	ExecutionSkippedQuota   ExecutionStatus = "skipped_quota"
	ExecutionBlocked        ExecutionStatus = "blocked"
	ExecutionInterrupted    ExecutionStatus = "interrupted"
)

// DeliveryStatus 是 CronRun 的投递状态机（RFC §7.3、§11.1）。
//
// execution 和 delivery 是两套独立状态机——投递失败不能重跑 Agent，因为 Agent
// 可能已经做了有副作用的事情（发邮件、扣库存、调外部 API）。
type DeliveryStatus string

const (
	DeliveryPending    DeliveryStatus = "pending"
	DeliverySent       DeliveryStatus = "sent"
	DeliveryNotNeeded  DeliveryStatus = "not_needed"  // finish_silent 成功
	DeliveryFailed     DeliveryStatus = "failed"
	DeliveryNotAllowed DeliveryStatus = "not_allowed" // execution 失败
)

// CronRun 是一次 Fire 的执行记录（RFC §7.3）。
//
// FireID 是 (jobID, scheduledAtUTC) 的 SHA-256，配合 create-if-absent 持久化
// claim 实现 durable 去重。模型、工具、事件、usage 仍在 Runtime RunStore；
// CronRun 只用 AgentRunID 关联。
type CronRun struct {
	SchemaVersion string `json:"schema_version"`
	JobID         string `json:"job_id"`
	FireID        string `json:"fire_id"`

	// PrincipalHash 冗余存一份，方便按 owner 索引；真相以 Job 为准
	PrincipalHash string `json:"principal_hash"`

	ScheduledAtUTC   time.Time `json:"scheduled_at_utc"`
	ScheduledAtLocal time.Time `json:"scheduled_at_local"`

	// AgentRunID 关联 Runtime RunStore 里的真实 run 记录
	AgentRunID string `json:"agent_run_id,omitempty"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	Execution ExecutionStatus `json:"execution"`
	Delivery  DeliveryStatus  `json:"delivery"`
}
