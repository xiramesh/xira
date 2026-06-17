// Package flow implements the Xira Flow v0 runtime: a small state machine that
// advances a business-case protocol across goal-driven steps, delegating work
// steps to the existing agent runtime and pausing on HumanRequest-driven
// human-in-the-loop. See docs/architecture/xira-flow-v0.zh.md.
package flow

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xiramesh/xira/internal/channel"
)

// SchemaVersionDefinition is the schema version string for flow definitions.
const SchemaVersionDefinition = "xira.flow.v0"

// SchemaVersionRun is the schema version string for flow run state.
const SchemaVersionRun = "xira.flow_run.v0"

// ReservedTransitionTerminal marks a transition target that ends the flow.
// Currently no terminal targets are reserved; flows complete when a completed
// step has no outgoing transition.
const ReservedTransitionTerminal = ""

// RunStatus is the persisted status of a flow run.
type RunStatus string

const (
	RunPending       RunStatus = "pending"
	RunRunning       RunStatus = "running"
	RunWaitingHuman  RunStatus = "waiting_human"
	RunWaitingSignal RunStatus = "waiting_signal"
	RunRecoverable   RunStatus = "recoverable"
	RunFailed        RunStatus = "failed"
	RunCanceled      RunStatus = "canceled"
	RunCompleted     RunStatus = "completed"
)

// StepStatus is the persisted status of a single flow step.
type StepStatus string

const (
	StepPending      StepStatus = "pending"
	StepRunning      StepStatus = "running"
	StepWaitingHuman StepStatus = "waiting_human"
	StepWaiting      StepStatus = "waiting"
	StepSkipped      StepStatus = "skipped"
	StepFailed       StepStatus = "failed"
	StepRecoverable  StepStatus = "recoverable"
	StepCompleted    StepStatus = "completed"
)

// HumanRequestSource enumerates the source values Flow writes onto the
// HumanRequests it creates or observes. Only Flow-owned explicit approvals
// use SourceFlowHumanApproval; the other two are produced by the agent runtime.
const (
	SourceFlowHumanApproval = "flow_human_approval"
	SourceAgentRequest      = "agent_request"
	SourceRuntimeToolGate   = "runtime_tool_gate"
)

// Metadata keys Flow writes onto HumanRequests in v0. See ASSUMPTION-FLOW-003.
const (
	MetadataScopeType  = "scope_type"
	MetadataFlowRunID  = "flow_run_id"
	MetadataFlowStepID = "flow_step_id"
	MetadataFlowID     = "flow_id"
)

const MetadataScopeTypeValue = "flow_run"

// Definition is a static flow definition loaded from YAML.
type Definition struct {
	SchemaVersion string          `yaml:"schema_version" json:"schema_version"`
	ID            string          `yaml:"id" json:"id"`
	Version       string          `yaml:"version" json:"version"`
	Name          string          `yaml:"name" json:"name"`
	Description   string          `yaml:"description,omitempty" json:"description,omitempty"`
	Objective     string          `yaml:"objective,omitempty" json:"objective,omitempty"`
	Invocation    *Invocation     `yaml:"invocation,omitempty" json:"invocation,omitempty"`
	Inputs        *InputSpec      `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Entrypoints   []Entrypoint    `yaml:"entrypoints,omitempty" json:"entrypoints,omitempty"`
	Context       *ContextSpec    `yaml:"context,omitempty" json:"context,omitempty"`
	Agents        []AgentRef      `yaml:"agents,omitempty" json:"agents,omitempty"`
	Permissions   *Permissions    `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	Artifacts     *ArtifactPolicy `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
	Verification  *Verification   `yaml:"verification,omitempty" json:"verification,omitempty"`
	Evolution     *Evolution      `yaml:"evolution,omitempty" json:"evolution,omitempty"`
	Steps         []Step          `yaml:"steps" json:"steps"`
}

// Invocation captures slash aliases and a short description for the flow.
type Invocation struct {
	Aliases     []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
}

// InputSpec declares required/optional flow inputs.
type InputSpec struct {
	Required []string `yaml:"required,omitempty" json:"required,omitempty"`
	Optional []string `yaml:"optional,omitempty" json:"optional,omitempty"`
}

// Entrypoint is a single entry into the flow, resolved from a start step.
type Entrypoint struct {
	ID             string   `yaml:"id" json:"id"`
	Description    string   `yaml:"description,omitempty" json:"description,omitempty"`
	StartStep      string   `yaml:"start_step" json:"start_step"`
	RequiredInputs []string `yaml:"required_inputs,omitempty" json:"required_inputs,omitempty"`
	OptionalInputs []string `yaml:"optional_inputs,omitempty" json:"optional_inputs,omitempty"`
	Aliases        []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
}

// ContextSpec declares required/optional/forbidden context files.
type ContextSpec struct {
	Required  []string `yaml:"required,omitempty" json:"required,omitempty"`
	Optional  []string `yaml:"optional,omitempty" json:"optional,omitempty"`
	Forbidden []string `yaml:"forbidden,omitempty" json:"forbidden,omitempty"`
}

// AgentRef references an agent profile that may execute flow steps.
type AgentRef struct {
	ID      string `yaml:"id" json:"id"`
	Role    string `yaml:"role,omitempty" json:"role,omitempty"`
	Profile string `yaml:"profile,omitempty" json:"profile,omitempty"`
}

// Permissions declares the runtime permission envelope for the flow.
type Permissions struct {
	PermissionMode string   `yaml:"permission_mode,omitempty" json:"permission_mode,omitempty"`
	Tools          []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	Secrets        []string `yaml:"secrets,omitempty" json:"secrets,omitempty"`
}

// ArtifactPolicy configures artifact output and retention.
type ArtifactPolicy struct {
	OutputDir string `yaml:"output_dir,omitempty" json:"output_dir,omitempty"`
	Retention string `yaml:"retention,omitempty" json:"retention,omitempty"`
}

// Verification configures default checks and acceptance cases.
type Verification struct {
	DefaultChecks   []string `yaml:"default_checks,omitempty" json:"default_checks,omitempty"`
	AcceptanceCases []string `yaml:"acceptance_cases,omitempty" json:"acceptance_cases,omitempty"`
	GoldenTasks     []string `yaml:"golden_tasks,omitempty" json:"golden_tasks,omitempty"`
}

// Evolution configures candidate evolution capture.
type Evolution struct {
	Enabled       bool   `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	CandidateOnly bool   `yaml:"candidate_only,omitempty" json:"candidate_only,omitempty"`
	CandidateDir  string `yaml:"candidate_dir,omitempty" json:"candidate_dir,omitempty"`
}

// Step is one goal contract inside a flow definition.
type Step struct {
	ID               string            `yaml:"id" json:"id"`
	Description      string            `yaml:"description,omitempty" json:"description,omitempty"`
	Objective        string            `yaml:"objective" json:"objective"`
	Inputs           map[string]string `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Instructions     []string          `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	Constraints      []string          `yaml:"constraints,omitempty" json:"constraints,omitempty"`
	RequiredSkills   []string          `yaml:"required_skills,omitempty" json:"required_skills,omitempty"`
	PreferredMethods []string          `yaml:"preferred_methods,omitempty" json:"preferred_methods,omitempty"`
	Prompt           *PromptSpec       `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Executor         Executor          `yaml:"executor" json:"executor"`
	OutputContract   OutputContract    `yaml:"output_contract,omitempty" json:"output_contract,omitempty"`
	Transitions      Transitions       `yaml:"transitions,omitempty" json:"transitions,omitempty"`
	Retry            *RetryPolicy      `yaml:"retry,omitempty" json:"retry,omitempty"`
}

// Executor declares how a step is performed. Work steps use Agent; control
// steps use Type (one of human_approval, decision, wait_signal, subflow).
type Executor struct {
	Agent              string                         `yaml:"agent,omitempty" json:"agent,omitempty"`
	Type               string                         `yaml:"type,omitempty" json:"type,omitempty"`
	EngineHint         string                         `yaml:"engine_hint,omitempty" json:"engine_hint,omitempty"`
	Tools              []string                       `yaml:"tools,omitempty" json:"tools,omitempty"`
	ToolInputAllowlist map[string]map[string][]string `yaml:"tool_input_allowlist,omitempty" json:"tool_input_allowlist,omitempty"`
	toolsConfigured    bool
	// Control-step fields. Populated only for human_approval / wait_signal /
	// subflow executors.
	Prompt            string   `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Question          string   `yaml:"question,omitempty" json:"question,omitempty"`
	Options           []string `yaml:"options,omitempty" json:"options,omitempty"`
	Artifacts         []string `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
	Signal            string   `yaml:"signal,omitempty" json:"signal,omitempty"`
	TimeoutSeconds    int      `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	SubflowFlow       string   `yaml:"flow,omitempty" json:"flow,omitempty"`
	SubflowEntrypoint string   `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
}

func (e *Executor) UnmarshalYAML(node *yaml.Node) error {
	type executor Executor
	var decoded executor
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*e = Executor(decoded)
	e.toolsConfigured = yamlMappingHasKey(node, "tools")
	return nil
}

func yamlMappingHasKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}

// PromptSpec is a step-local prompt template (inline or path-backed).
type PromptSpec struct {
	Template string            `yaml:"template,omitempty" json:"template,omitempty"`
	Inline   string            `yaml:"inline,omitempty" json:"inline,omitempty"`
	Inputs   map[string]string `yaml:"inputs,omitempty" json:"inputs,omitempty"`
}

// OutputContract declares the slots a step must/should produce.
type OutputContract struct {
	ArtifactPolicy     string       `yaml:"artifact_policy,omitempty" json:"artifact_policy,omitempty"`
	RequiredSlots      []OutputSlot `yaml:"required_slots" json:"required_slots"`
	OptionalSlots      []OutputSlot `yaml:"optional_slots,omitempty" json:"optional_slots,omitempty"`
	CompletionCriteria []string     `yaml:"completion_criteria,omitempty" json:"completion_criteria,omitempty"`
}

// OutputSlot is either a bare id or a richer object; the ID is the canonical
// key used in outputs maps.
type OutputSlot struct {
	ID          string   `yaml:"id" json:"id"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Formats     []string `yaml:"formats,omitempty" json:"formats,omitempty"`
	Required    *bool    `yaml:"required,omitempty" json:"required,omitempty"`
}

// UnmarshalYAML accepts OutputSlot as either a bare string ("id") or a
// {"id": "...", ...} object, matching xira-flow-v0.schema.json outputSlot.
func (s *OutputSlot) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		return nil
	}
	if value.Kind == yaml.ScalarNode {
		var id string
		if err := value.Decode(&id); err != nil {
			return fmt.Errorf("decode output slot id: %w", err)
		}
		s.ID = id
		return nil
	}
	type plain OutputSlot
	var tmp plain
	if err := value.Decode(&tmp); err != nil {
		return fmt.Errorf("decode output slot: %w", err)
	}
	*s = OutputSlot(tmp)
	return nil
}

// Transitions describe how the kernel leaves a step.
type Transitions struct {
	OnSuccess string   `yaml:"on_success,omitempty" json:"on_success,omitempty"`
	OnFailure string   `yaml:"on_failure,omitempty" json:"on_failure,omitempty"`
	Branches  []Branch `yaml:"branches,omitempty" json:"branches,omitempty"`
}

// Branch is one conditional transition.
type Branch struct {
	When string `yaml:"when" json:"when"`
	Next string `yaml:"next" json:"next"`
}

// RetryPolicy bounds retry attempts for a step.
type RetryPolicy struct {
	MaxAttempts int    `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty"`
	OnExhausted string `yaml:"on_exhausted,omitempty" json:"on_exhausted,omitempty"`
}

// Run is one durable instance of a Definition.
type Run struct {
	SchemaVersion        string               `yaml:"schema_version" json:"schema_version"`
	ID                   string               `yaml:"flow_run_id" json:"flow_run_id"`
	FlowID               string               `yaml:"flow_id" json:"flow_id"`
	FlowVersion          string               `yaml:"flow_version" json:"flow_version"`
	Status               RunStatus            `yaml:"status" json:"status"`
	CurrentStepID        string               `yaml:"current_step_id,omitempty" json:"current_step_id,omitempty"`
	EntrypointID         string               `yaml:"entrypoint_id,omitempty" json:"entrypoint_id,omitempty"`
	Input                map[string]string    `yaml:"input,omitempty" json:"input,omitempty"`
	// Context is the trigger identity (channel/chat/sender/space) of whoever
	// started this flow run. It persists into flow_run.yaml so cross-process
	// Advance/Resume can propagate it to agent steps — keeping flow-invoked
	// agent sessions under the same conversation tree as the direct trigger.
	Context              *channel.InboundContext `yaml:"context,omitempty" json:"context,omitempty"`
	Steps                map[string]StepState `yaml:"steps,omitempty" json:"steps,omitempty"`
	PendingSignals       []string             `yaml:"pending_signals,omitempty" json:"pending_signals,omitempty"`
	PendingHumanRequests []string             `yaml:"pending_human_requests,omitempty" json:"pending_human_requests,omitempty"`
	Artifacts            []ArtifactRef        `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
	EventsRef            string               `yaml:"events_ref,omitempty" json:"events_ref,omitempty"`
	CreatedAt            time.Time            `yaml:"created_at" json:"created_at"`
	UpdatedAt            time.Time            `yaml:"updated_at" json:"updated_at"`
}

// StepState is the persisted state of one step inside a Run.
type StepState struct {
	Status          StepStatus           `yaml:"status" json:"status"`
	StartedAt       *time.Time           `yaml:"started_at,omitempty" json:"started_at,omitempty"`
	CompletedAt     *time.Time           `yaml:"completed_at,omitempty" json:"completed_at,omitempty"`
	Attempts        int                  `yaml:"attempts,omitempty" json:"attempts,omitempty"`
	AgentRunID      string               `yaml:"agent_run_id,omitempty" json:"agent_run_id,omitempty"`
	HumanRequestIDs []string             `yaml:"human_request_ids,omitempty" json:"human_request_ids,omitempty"`
	Interrupt       map[string]any       `yaml:"interrupt,omitempty" json:"interrupt,omitempty"`
	Signal          string               `yaml:"signal,omitempty" json:"signal,omitempty"`
	Outputs         map[string]OutputRef `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	Artifacts       []ArtifactRef        `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
	Error           string               `yaml:"error,omitempty" json:"error,omitempty"`
}

// OutputRef is one declared output slot's resolved value/reference.
type OutputRef struct {
	Artifact  string   `yaml:"artifact,omitempty" json:"artifact,omitempty"`
	Artifacts []string `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
	Value     any      `yaml:"value,omitempty" json:"value,omitempty"`
	Summary   string   `yaml:"summary,omitempty" json:"summary,omitempty"`
}

// ArtifactRef is a reference to an artifact path on disk (relative to the run
// dir unless absolute).
type ArtifactRef struct {
	Path        string `yaml:"path" json:"path"`
	ContentType string `yaml:"content_type,omitempty" json:"content_type,omitempty"`
}

// Event is one durable flow event appended to events.jsonl.
type Event struct {
	ID             string         `yaml:"-" json:"id"`
	Time           time.Time      `yaml:"time" json:"time"`
	Kind           string         `yaml:"kind" json:"kind"`
	FlowRunID      string         `yaml:"flow_run_id,omitempty" json:"flow_run_id,omitempty"`
	StepID         string         `yaml:"step_id,omitempty" json:"step_id,omitempty"`
	AgentRunID     string         `yaml:"agent_run_id,omitempty" json:"agent_run_id,omitempty"`
	HumanRequestID string         `yaml:"human_request_id,omitempty" json:"human_request_id,omitempty"`
	Payload        map[string]any `yaml:"payload,omitempty" json:"payload,omitempty"`
}
