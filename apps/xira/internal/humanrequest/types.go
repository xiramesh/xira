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

type HumanOption struct {
	ID    string `json:"id" yaml:"id"`
	Label string `json:"label" yaml:"label"`
}

type HumanRequest struct {
	ID           string        `json:"id" yaml:"id"`
	WorkspaceID  string        `json:"workspace_id" yaml:"workspace_id"`
	WorkspaceKey string        `json:"workspace_key" yaml:"workspace_key"`
	RunID        string        `json:"run_id" yaml:"run_id"`
	AgentID      string        `json:"agent_id" yaml:"agent_id"`
	SessionID    string        `json:"session_id" yaml:"session_id"`
	ToolCallID   string        `json:"tool_call_id,omitempty" yaml:"tool_call_id,omitempty"`
	Source       string        `json:"source,omitempty" yaml:"source,omitempty"`
	Kind         RequestKind   `json:"kind" yaml:"kind"`
	Status       RequestStatus `json:"status" yaml:"status"`
	Question     string        `json:"question" yaml:"question"`
	Options      []HumanOption `json:"options,omitempty" yaml:"options,omitempty"`
	DedupeKey    string        `json:"dedupe_key,omitempty" yaml:"dedupe_key,omitempty"`
	// ChatKey is the originating chat key (runtime.ChatKey.String(),
	// "channel/chat_id/sender_id") so the Store can answer "which pending HITL
	// belongs to this chat". Stored as string to avoid a humanrequest→runtime
	// import cycle. Empty for requests created before the field existed and for
	// flow_bridge (which has no inbound context yet). #91-A.
	ChatKey    string            `json:"chat_key,omitempty" yaml:"chat_key,omitempty"`
	CreatedAt  time.Time         `json:"created_at" yaml:"created_at"`
	ResolvedAt *time.Time        `json:"resolved_at,omitempty" yaml:"resolved_at,omitempty"`
	Response   *HumanResponse    `json:"response,omitempty" yaml:"response,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Audit      []AuditRecord     `json:"audit,omitempty" yaml:"audit,omitempty"`
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
	ID           string
	WorkspaceID  string
	WorkspaceKey string
	RunID        string
	AgentID      string
	SessionID    string
	ToolCallID   string
	Source       string
	Kind         RequestKind
	Question     string
	Options      []HumanOption
	DedupeKey    string
	// ChatKey is the originating chat key (runtime.ChatKey.String()). Optional —
	// flow_bridge can't fill it yet. #91-A.
	ChatKey   string
	CreatedAt time.Time
	Metadata  map[string]string
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
	// ChatKey filters by originating chat (runtime.ChatKey.String()). Empty = no
	// filter (returns all, backward compatible). #91-A.
	ChatKey string
}
