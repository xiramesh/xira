package humanrequest

import "time"

type RequestKind string

const (
	RequestFreeform RequestKind = "freeform"
	RequestApproval RequestKind = "approval"
)

type RequestStatus string

const (
	StatusPending  RequestStatus = "pending"
	StatusResolved RequestStatus = "resolved"
)

type ResponseKind string

const (
	ResponseApprove ResponseKind = "approve"
	ResponseDeny    ResponseKind = "deny"
	ResponseCancel  ResponseKind = "cancel"
	ResponseAnswer  ResponseKind = "answer"
)

type ReplayStatus string

const (
	ReplayPending   ReplayStatus = "pending"
	ReplayRunning   ReplayStatus = "running"
	ReplayCompleted ReplayStatus = "completed"
	ReplayDenied    ReplayStatus = "denied"
	ReplayCanceled  ReplayStatus = "canceled"
	ReplayFailed    ReplayStatus = "failed"
)

type HumanOption struct {
	ID    string `json:"id" yaml:"id"`
	Label string `json:"label" yaml:"label"`
}

type ActionSnapshot struct {
	ToolName      string         `json:"tool_name" yaml:"tool_name"`
	Arguments     map[string]any `json:"arguments,omitempty" yaml:"arguments,omitempty"`
	RunID         string         `json:"run_id,omitempty" yaml:"run_id,omitempty"`
	AgentID       string         `json:"agent_id,omitempty" yaml:"agent_id,omitempty"`
	SessionID     string         `json:"session_id,omitempty" yaml:"session_id,omitempty"`
	ToolCallID    string         `json:"tool_call_id,omitempty" yaml:"tool_call_id,omitempty"`
	ContextHash   string         `json:"context_hash,omitempty" yaml:"context_hash,omitempty"`
	PolicyTraceID string         `json:"policy_trace_id,omitempty" yaml:"policy_trace_id,omitempty"`
}

type ReplayState struct {
	Status          ReplayStatus `json:"status" yaml:"status"`
	LeaseOwner      string       `json:"lease_owner,omitempty" yaml:"lease_owner,omitempty"`
	LeaseUntil      *time.Time   `json:"lease_until,omitempty" yaml:"lease_until,omitempty"`
	ResultDigest    string       `json:"result_digest,omitempty" yaml:"result_digest,omitempty"`
	ResultReference string       `json:"result_reference,omitempty" yaml:"result_reference,omitempty"`
	IdempotencyKey  string       `json:"idempotency_key,omitempty" yaml:"idempotency_key,omitempty"`
	Error           string       `json:"error,omitempty" yaml:"error,omitempty"`
	UpdatedAt       time.Time    `json:"updated_at,omitempty" yaml:"updated_at,omitempty"`
}

type HumanRequest struct {
	ID             string            `json:"id" yaml:"id"`
	WorkspaceID    string            `json:"workspace_id" yaml:"workspace_id"`
	WorkspaceKey   string            `json:"workspace_key" yaml:"workspace_key"`
	RunID          string            `json:"run_id" yaml:"run_id"`
	AgentID        string            `json:"agent_id" yaml:"agent_id"`
	SessionID      string            `json:"session_id" yaml:"session_id"`
	ToolCallID     string            `json:"tool_call_id,omitempty" yaml:"tool_call_id,omitempty"`
	Source         string            `json:"source,omitempty" yaml:"source,omitempty"`
	Kind           RequestKind       `json:"kind" yaml:"kind"`
	Status         RequestStatus     `json:"status" yaml:"status"`
	Question       string            `json:"question" yaml:"question"`
	Options        []HumanOption     `json:"options,omitempty" yaml:"options,omitempty"`
	ActionSnapshot *ActionSnapshot   `json:"action_snapshot,omitempty" yaml:"action_snapshot,omitempty"`
	DedupeKey      string            `json:"dedupe_key,omitempty" yaml:"dedupe_key,omitempty"`
	CreatedAt      time.Time         `json:"created_at" yaml:"created_at"`
	ResolvedAt     *time.Time        `json:"resolved_at,omitempty" yaml:"resolved_at,omitempty"`
	Response       *HumanResponse    `json:"response,omitempty" yaml:"response,omitempty"`
	Replay         *ReplayState      `json:"replay,omitempty" yaml:"replay,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Audit          []AuditRecord     `json:"audit,omitempty" yaml:"audit,omitempty"`
}

type HumanResponse struct {
	ID             string       `json:"id" yaml:"id"`
	RequestID      string       `json:"request_id" yaml:"request_id"`
	Kind           ResponseKind `json:"kind" yaml:"kind"`
	Actor          string       `json:"actor,omitempty" yaml:"actor,omitempty"`
	Message        string       `json:"message,omitempty" yaml:"message,omitempty"`
	IdempotencyKey string       `json:"idempotency_key,omitempty" yaml:"idempotency_key,omitempty"`
	CreatedAt      time.Time    `json:"created_at" yaml:"created_at"`
}

type AuditRecord struct {
	Time       time.Time     `json:"time" yaml:"time"`
	Actor      string        `json:"actor,omitempty" yaml:"actor,omitempty"`
	Action     string        `json:"action" yaml:"action"`
	FromStatus RequestStatus `json:"from_status,omitempty" yaml:"from_status,omitempty"`
	ToStatus   RequestStatus `json:"to_status,omitempty" yaml:"to_status,omitempty"`
	Signal     ResponseKind  `json:"signal,omitempty" yaml:"signal,omitempty"`
	Message    string        `json:"message,omitempty" yaml:"message,omitempty"`
}

type CreateRequest struct {
	ID             string
	WorkspaceID    string
	WorkspaceKey   string
	RunID          string
	AgentID        string
	SessionID      string
	ToolCallID     string
	Source         string
	Kind           RequestKind
	Question       string
	Options        []HumanOption
	ActionSnapshot *ActionSnapshot
	DedupeKey      string
	CreatedAt      time.Time
	Metadata       map[string]string
}

type ResolveRequest struct {
	WorkspaceKey   string
	RequestID      string
	Kind           ResponseKind
	Actor          string
	Message        string
	IdempotencyKey string
	ResolvedAt     time.Time
}

type ListQuery struct {
	WorkspaceKey string
	Status       RequestStatus
}

type ReplayLeaseRequest struct {
	WorkspaceKey  string
	RequestID     string
	Owner         string
	LeaseDuration time.Duration
	Now           time.Time
}

type CompleteReplayRequest struct {
	WorkspaceKey    string
	RequestID       string
	Owner           string
	ResultDigest    string
	ResultReference string
	IdempotencyKey  string
	CompletedAt     time.Time
}

type FailReplayRequest struct {
	WorkspaceKey string
	RequestID    string
	Owner        string
	Error        string
	FailedAt     time.Time
}
