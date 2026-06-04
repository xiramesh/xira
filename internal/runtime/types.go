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
	ModelPolicy        ModelPolicySnapshot    `json:"model_policy,omitempty" yaml:"model_policy,omitempty"`
	Message            string                 `json:"message" yaml:"message"`
	FinalResponse      string                 `json:"final_response" yaml:"final_response"`
	Status             string                 `json:"status" yaml:"status"`
	StartedAt          time.Time              `json:"started_at" yaml:"started_at"`
	EndedAt            time.Time              `json:"ended_at" yaml:"ended_at"`
	LLMCalls           []LLMCallRecord        `json:"llm_calls,omitempty" yaml:"llm_calls,omitempty"`
	Usage              UsageSummary           `json:"usage,omitempty" yaml:"usage,omitempty"`
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

type ModelPolicySnapshot struct {
	AgentID         string   `json:"agent_id,omitempty" yaml:"agent_id,omitempty"`
	Provider        string   `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model           string   `json:"model,omitempty" yaml:"model,omitempty"`
	Stream          bool     `json:"stream,omitempty" yaml:"stream,omitempty"`
	Temperature     *float32 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	ThinkingType    string   `json:"thinking_type,omitempty" yaml:"thinking_type,omitempty"`
	Tools           []string `json:"tools,omitempty" yaml:"tools,omitempty"`
	ProfileSource   string   `json:"profile_source,omitempty" yaml:"profile_source,omitempty"`
	InstructionHash string   `json:"instruction_hash,omitempty" yaml:"instruction_hash,omitempty"`
}

type LLMCallRecord struct {
	RunID            string         `json:"run_id" yaml:"run_id"`
	AgentID          string         `json:"agent_id" yaml:"agent_id"`
	EntrypointID     string         `json:"entrypoint_id,omitempty" yaml:"entrypoint_id,omitempty"`
	Channel          string         `json:"channel,omitempty" yaml:"channel,omitempty"`
	SessionID        string         `json:"session_id,omitempty" yaml:"session_id,omitempty"`
	AgentSessionID   string         `json:"agent_session_id,omitempty" yaml:"agent_session_id,omitempty"`
	UserID           string         `json:"user_id,omitempty" yaml:"user_id,omitempty"`
	Provider         string         `json:"provider" yaml:"provider"`
	Model            string         `json:"model" yaml:"model"`
	RequestIndex     int            `json:"request_index" yaml:"request_index"`
	Status           string         `json:"status" yaml:"status"`
	StartedAt        time.Time      `json:"started_at" yaml:"started_at"`
	EndedAt          time.Time      `json:"ended_at" yaml:"ended_at"`
	LatencyMS        int64          `json:"latency_ms" yaml:"latency_ms"`
	Stream           bool           `json:"stream,omitempty" yaml:"stream,omitempty"`
	Temperature      *float32       `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	ThinkingType     string         `json:"thinking_type,omitempty" yaml:"thinking_type,omitempty"`
	MessageCount     int            `json:"message_count,omitempty" yaml:"message_count,omitempty"`
	ToolCount        int            `json:"tool_count,omitempty" yaml:"tool_count,omitempty"`
	PromptChars      int            `json:"prompt_chars,omitempty" yaml:"prompt_chars,omitempty"`
	ToolResultChars  int            `json:"tool_result_chars,omitempty" yaml:"tool_result_chars,omitempty"`
	PromptTokens     int64          `json:"prompt_tokens,omitempty" yaml:"prompt_tokens,omitempty"`
	CompletionTokens int64          `json:"completion_tokens,omitempty" yaml:"completion_tokens,omitempty"`
	TotalTokens      int64          `json:"total_tokens,omitempty" yaml:"total_tokens,omitempty"`
	UsageSource      string         `json:"usage_source" yaml:"usage_source"`
	Cost             *float64       `json:"cost,omitempty" yaml:"cost,omitempty"`
	Currency         string         `json:"currency,omitempty" yaml:"currency,omitempty"`
	Error            string         `json:"error,omitempty" yaml:"error,omitempty"`
	TraceRequestPath string         `json:"trace_request_path,omitempty" yaml:"trace_request_path,omitempty"`
	ProviderUsage    map[string]any `json:"provider_usage,omitempty" yaml:"provider_usage,omitempty"`
}

type UsageSummary struct {
	RunID                string                       `json:"run_id,omitempty" yaml:"run_id,omitempty"`
	AgentID              string                       `json:"agent_id,omitempty" yaml:"agent_id,omitempty"`
	EntrypointID         string                       `json:"entrypoint_id,omitempty" yaml:"entrypoint_id,omitempty"`
	Channel              string                       `json:"channel,omitempty" yaml:"channel,omitempty"`
	SessionID            string                       `json:"session_id,omitempty" yaml:"session_id,omitempty"`
	StartedAt            time.Time                    `json:"started_at,omitempty" yaml:"started_at,omitempty"`
	EndedAt              time.Time                    `json:"ended_at,omitempty" yaml:"ended_at,omitempty"`
	CallCount            int                          `json:"call_count,omitempty" yaml:"call_count,omitempty"`
	CompletedCalls       int                          `json:"completed_calls,omitempty" yaml:"completed_calls,omitempty"`
	FailedCalls          int                          `json:"failed_calls,omitempty" yaml:"failed_calls,omitempty"`
	PromptTokens         int64                        `json:"prompt_tokens,omitempty" yaml:"prompt_tokens,omitempty"`
	CompletionTokens     int64                        `json:"completion_tokens,omitempty" yaml:"completion_tokens,omitempty"`
	TotalTokens          int64                        `json:"total_tokens,omitempty" yaml:"total_tokens,omitempty"`
	Cost                 *float64                     `json:"cost,omitempty" yaml:"cost,omitempty"`
	Currency             string                       `json:"currency,omitempty" yaml:"currency,omitempty"`
	UsageSources         map[string]int               `json:"usage_sources,omitempty" yaml:"usage_sources,omitempty"`
	MissingUsageRequests []int                        `json:"missing_usage_requests,omitempty" yaml:"missing_usage_requests,omitempty"`
	Models               map[string]UsageModelSummary `json:"models,omitempty" yaml:"models,omitempty"`
}

type UsageModelSummary struct {
	CallCount        int      `json:"call_count,omitempty" yaml:"call_count,omitempty"`
	PromptTokens     int64    `json:"prompt_tokens,omitempty" yaml:"prompt_tokens,omitempty"`
	CompletionTokens int64    `json:"completion_tokens,omitempty" yaml:"completion_tokens,omitempty"`
	TotalTokens      int64    `json:"total_tokens,omitempty" yaml:"total_tokens,omitempty"`
	Cost             *float64 `json:"cost,omitempty" yaml:"cost,omitempty"`
	Currency         string   `json:"currency,omitempty" yaml:"currency,omitempty"`
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
