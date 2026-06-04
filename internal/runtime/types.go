package runtime

import (
	"time"

	fsession "github.com/ai-daming/xira/internal/session"
)

type EntrypointType string

const (
	EntrypointAgent EntrypointType = "agent"
	EntrypointFlow  EntrypointType = "flow"
)

type TurnRequest struct {
	AgentID      string            `json:"agent_id" yaml:"agent_id"`
	EntrypointID string            `json:"entrypoint_id,omitempty" yaml:"entrypoint_id,omitempty"`
	Message      string            `json:"message" yaml:"message"`
	UserID       string            `json:"user_id,omitempty" yaml:"user_id,omitempty"`
	SessionID    string            `json:"session_id,omitempty" yaml:"session_id,omitempty"`
	Channel      string            `json:"channel,omitempty" yaml:"channel,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

type TurnResponse struct {
	RunID              string                 `json:"run_id" yaml:"run_id"`
	AgentID            string                 `json:"agent_id" yaml:"agent_id"`
	EntrypointID       string                 `json:"entrypoint_id,omitempty" yaml:"entrypoint_id,omitempty"`
	SessionID          string                 `json:"session_id" yaml:"session_id"`
	SessionScope       *fsession.SessionScope `json:"session_scope,omitempty" yaml:"session_scope,omitempty"`
	RouteMatchedBy     string                 `json:"route_matched_by,omitempty" yaml:"route_matched_by,omitempty"`
	Message            string                 `json:"message" yaml:"message"`
	FinalResponse      string                 `json:"final_response" yaml:"final_response"`
	Status             string                 `json:"status" yaml:"status"`
	StartedAt          time.Time              `json:"started_at" yaml:"started_at"`
	EndedAt            time.Time              `json:"ended_at" yaml:"ended_at"`
	ToolCalls          []ToolCallRecord       `json:"tool_calls,omitempty" yaml:"tool_calls,omitempty"`
	VerificationResult VerificationResult     `json:"verification" yaml:"verification"`
	EvolutionCandidate *EvolutionCandidate    `json:"evolution_candidate,omitempty" yaml:"evolution_candidate,omitempty"`
	Artifacts          []string               `json:"artifacts,omitempty" yaml:"artifacts,omitempty"`
	Events             []RuntimeEvent         `json:"events,omitempty" yaml:"events,omitempty"`
	AuditEvents        []AuditEvent           `json:"audit_events,omitempty" yaml:"audit_events,omitempty"`
	Metadata           map[string]string      `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

type RuntimeEvent struct {
	ID       string         `json:"id" yaml:"id"`
	RunID    string         `json:"run_id,omitempty" yaml:"run_id,omitempty"`
	Kind     string         `json:"kind" yaml:"kind"`
	Time     time.Time      `json:"time" yaml:"time"`
	Source   string         `json:"source" yaml:"source"`
	Severity string         `json:"severity,omitempty" yaml:"severity,omitempty"`
	Message  string         `json:"message,omitempty" yaml:"message,omitempty"`
	Payload  map[string]any `json:"payload,omitempty" yaml:"payload,omitempty"`
}

type AuditEvent struct {
	ID      string         `json:"id" yaml:"id"`
	RunID   string         `json:"run_id,omitempty" yaml:"run_id,omitempty"`
	Time    time.Time      `json:"time" yaml:"time"`
	Action  string         `json:"action" yaml:"action"`
	Actor   string         `json:"actor,omitempty" yaml:"actor,omitempty"`
	Target  string         `json:"target,omitempty" yaml:"target,omitempty"`
	Allowed bool           `json:"allowed" yaml:"allowed"`
	Reason  string         `json:"reason,omitempty" yaml:"reason,omitempty"`
	Meta    map[string]any `json:"meta,omitempty" yaml:"meta,omitempty"`
}

type ToolCallRecord struct {
	ID        string         `json:"id" yaml:"id"`
	RunID     string         `json:"run_id,omitempty" yaml:"run_id,omitempty"`
	Name      string         `json:"name" yaml:"name"`
	Input     map[string]any `json:"input,omitempty" yaml:"input,omitempty"`
	Output    map[string]any `json:"output,omitempty" yaml:"output,omitempty"`
	Error     string         `json:"error,omitempty" yaml:"error,omitempty"`
	StartedAt time.Time      `json:"started_at" yaml:"started_at"`
	EndedAt   time.Time      `json:"ended_at" yaml:"ended_at"`
}

type VerificationResult struct {
	Status string   `json:"status" yaml:"status"`
	Checks []string `json:"checks,omitempty" yaml:"checks,omitempty"`
	Errors []string `json:"errors,omitempty" yaml:"errors,omitempty"`
}

type EvolutionCandidate struct {
	ID           string    `json:"id" yaml:"id"`
	RunID        string    `json:"run_id" yaml:"run_id"`
	Trigger      string    `json:"trigger" yaml:"trigger"`
	FailureLayer string    `json:"failure_layer" yaml:"failure_layer"`
	Evidence     []string  `json:"evidence" yaml:"evidence"`
	Status       string    `json:"status" yaml:"status"`
	CreatedAt    time.Time `json:"created_at" yaml:"created_at"`
}
