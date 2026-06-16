# Xira Flow v0 Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. Do not skip the failing-test step in any implementation task.

**Goal:** Implement the first runnable Xira Flow v0 runtime from the existing Flow schema and DevRun example, using the Agent Runtime / HumanRequest / waiting_human foundation already present on `main`.

**Architecture:** Flow is the stateful business-case protocol above agent runs. The Flow layer loads a flow definition, creates a durable flow run, advances one step at a time, delegates work steps to the existing agent runtime, records output slots and artifacts, pauses on explicit approval or agent-generated `waiting_human`, and resumes after the existing HumanRequest system is resolved. Flow must not reimplement tool execution, approval replay, child delegation, or model loops.

**Tech Stack:** Go, YAML/JSON schema validation, Xira runtime under `apps/xira`, file-backed state under the runtime state root, Cobra CLI, `net/http` API tests with `httptest`, deterministic fake-agent/fake-runtime tests, existing HumanRequest and RunInterrupt types.

---

## Current Branch Context

- Working branch: `feature/flow-schema-v0`.
- Latest `main` has been merged into this branch.
- Existing Flow spec files:
  - `docs/architecture/xira-flow-v0.zh.md`
  - `docs/architecture/xira-flow-v0-wip-notes.zh.md`
  - `docs/schemas/xira-flow-v0.schema.json`
  - `docs/schemas/xira-flow-run-v0.schema.json`
  - `docs/examples/flows/devrun/flow.yaml`
  - `docs/examples/flows/devrun/flow_run.waiting_approval.yaml`
- Existing HITL / runtime foundation:
  - `apps/xira/internal/humanrequest/types.go`
  - `apps/xira/internal/humanrequest/store.go`
  - `apps/xira/internal/runtime/interrupt.go`
  - `apps/xira/internal/runtime/human_requests.go`
  - `apps/xira/internal/runtime/human_request_resume.go`
  - `apps/xira/internal/runtime/delegation_join.go`
  - `apps/xira/internal/runtime/delegation_resume.go`

---

## Non-Negotiable Boundaries

- [ ] Do not build a generic workflow engine. Flow v0 is a small state machine for Xira case progression.
- [ ] Do not add direct command/shell executors to Flow v0. Work steps use `executor.agent`; command execution remains an agent/runtime/tool concern.
- [ ] Do not create a second approval system. Flow explicit approval steps and agent-generated approval both use `HumanRequest`.
- [ ] Do not make Flow inspect child delegation internals. If child `waiting_human` matters, the parent agent run must surface `waiting_human`; Flow only watches the parent run result.
- [ ] Do not store large stdout/stderr or full model transcripts in `flow_run.yaml`. Store artifact refs, run refs, output slots, and summaries.
- [ ] Do not implement multi-approver quorum, UI approval panel, broad policy DSL, or full DAG scheduling in v0.
- [ ] Keep all tests deterministic. Live model tests are optional smoke tests only and must be env-gated.

---

## AI Handoff Protocol

Use these markers consistently while implementing:

- `AI-TODO-VERIFY`: verify this from the live repo before editing.
- `ASSUMPTION`: proceed using this rule unless live code contradicts it.
- `OPEN-DECISION`: make the smallest local decision, document it in this plan, and continue.
- `DEFERRED-v0`: intentionally out of scope.
- `WATCH`: high-risk edge that needs a regression test.

Fresh worker startup checklist:

- [ ] AI-TODO-VERIFY: Run `git status --short --branch` and confirm user-owned changes before editing.
- [ ] AI-TODO-VERIFY: Read `docs/architecture/xira-flow-v0.zh.md` from top to bottom.
- [ ] AI-TODO-VERIFY: Read `docs/architecture/xira-agent-hitl-v0.zh.md` sections containing `Flow 未来`, `RunInterrupt`, `waiting_human`, and `HumanRequest`.
- [ ] AI-TODO-VERIFY: Read `docs/examples/flows/devrun/flow.yaml` and `docs/examples/flows/devrun/flow_run.waiting_approval.yaml`.
- [ ] AI-TODO-VERIFY: Inspect existing runtime files before adding Flow code:
  - `apps/xira/internal/runtime/service.go`
  - `apps/xira/internal/runtime/types.go`
  - `apps/xira/internal/runtime/store.go`
  - `apps/xira/internal/runtime/human_requests.go`
  - `apps/xira/internal/runtime/human_request_resume.go`
- [ ] AI-TODO-VERIFY: Inspect existing CLI/API entry points:
  - `apps/xira/cmd/xira/main.go`
  - `apps/xira/internal/api/server.go`
- [ ] AI-TODO-VERIFY: Baseline command:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/...
```

Expected: pass outside restricted sandbox. If `httptest` bind failures appear inside sandbox, rerun outside sandbox or record the sandbox limitation.

---

## Core Runtime Model

Implement Flow around these concepts:

```text
FlowDefinition
  Static YAML definition. Contains entrypoints, agents, steps, transitions, output contracts.

FlowRun
  One durable instance of a FlowDefinition. Contains input, status, current step, step states, artifact refs, agent run refs, HumanRequest refs, and event refs.

FlowKernel
  Small state machine. start -> advance -> pause -> resume -> complete/fail/cancel.

Agent Runtime
  Existing execution substrate. Flow invokes it for agent steps and observes TurnResponse.

HumanRequest
  Existing durable human-intervention envelope. Flow explicit approvals create it; agent steps may also surface it via TurnResponse.
```

Flow run statuses:

```go
const (
    FlowRunPending      FlowRunStatus = "pending"
    FlowRunRunning      FlowRunStatus = "running"
    FlowRunWaitingHuman FlowRunStatus = "waiting_human"
    FlowRunCompleted    FlowRunStatus = "completed"
    FlowRunFailed       FlowRunStatus = "failed"
    FlowRunCanceled     FlowRunStatus = "canceled"
)
```

Flow step statuses:

```go
const (
    FlowStepPending      FlowStepStatus = "pending"
    FlowStepRunning      FlowStepStatus = "running"
    FlowStepWaitingHuman FlowStepStatus = "waiting_human"
    FlowStepCompleted    FlowStepStatus = "completed"
    FlowStepFailed       FlowStepStatus = "failed"
    FlowStepSkipped      FlowStepStatus = "skipped"
)
```

ASSUMPTION-FLOW-001: `canceled` is allowed at the Flow run/step level even though Agent run status does not use `canceled`. Flow is a business-case protocol and can end a case as canceled after a user rejects/cancels an approval.

ASSUMPTION-FLOW-002: Flow explicit approval is represented as `HumanRequest.Kind=approval` with `Source=flow_human_approval`. Agent/tool approval remains `agent_request` or `runtime_tool_gate`.

ASSUMPTION-FLOW-003: v0 may record Flow scope in `HumanRequest.Metadata` if the existing `HumanRequest` struct has not yet been generalized. Required metadata keys:

```text
scope_type=flow_run
flow_run_id=<id>
flow_step_id=<step id>
flow_id=<flow id>
```

Do not block the whole implementation on a broad `HumanRequest.Scope` refactor unless tests show metadata is insufficient.

---

## File-Level Target Shape

Create:

- `apps/xira/internal/flow/types.go`
- `apps/xira/internal/flow/definition.go`
- `apps/xira/internal/flow/definition_test.go`
- `apps/xira/internal/flow/store.go`
- `apps/xira/internal/flow/store_test.go`
- `apps/xira/internal/flow/kernel.go`
- `apps/xira/internal/flow/kernel_test.go`
- `apps/xira/internal/flow/executor.go`
- `apps/xira/internal/flow/executor_test.go`
- `apps/xira/internal/flow/human_request_test.go`
- `apps/xira/internal/flow/devrun_test.go`

Modify:

- `apps/xira/internal/runtime/service.go`
- `apps/xira/internal/runtime/config.go`
- `apps/xira/internal/api/server.go`
- `apps/xira/internal/api/server_test.go`
- `apps/xira/cmd/xira/main.go`
- `apps/xira/cmd/xira/main_test.go`
- `docs/architecture/xira-flow-v0.zh.md`
- `docs/architecture/xira-flow-v0-wip-notes.zh.md`
- `docs/schemas/xira-flow-v0.schema.json`
- `docs/schemas/xira-flow-run-v0.schema.json`
- `docs/examples/flows/devrun/flow_run.waiting_approval.yaml`

Do not modify unless needed:

- `apps/xira/internal/humanrequest/store.go`
- `apps/xira/internal/runtime/delegation.go`
- `apps/xira/internal/runtime/human_request_resume.go`

---

## Milestone 0: Align Flow HITL Semantics In Docs And Schema

### Objective

Make the spec match the implemented Agent HITL foundation before runtime implementation begins.

### Task 0.1: Update Flow Architecture Document

**Files:**

- Modify: `docs/architecture/xira-flow-v0.zh.md`
- Modify: `docs/architecture/xira-flow-v0-wip-notes.zh.md`

**Required content:**

- Flow explicit approval uses `HumanRequest`.
- Agent-generated `HumanRequest` pauses the current Flow step.
- Runtime tool gate approval pauses the current Flow step through the agent run's `waiting_human`.
- Child delegation `waiting_human` pauses the parent agent run; Flow only observes parent `waiting_human`.
- Flow Kernel resume is driven by resolved `HumanRequest` and the persisted FlowRun current step.

**Test/verification:**

```bash
rg -n "HumanRequest|waiting_human|flow_run \\+ step|agent-generated|child" docs/architecture/xira-flow-v0.zh.md
```

Expected: all concepts are explicitly covered.

### Task 0.2: Update Flow Run Schema For HITL Links

**Files:**

- Modify: `docs/schemas/xira-flow-run-v0.schema.json`
- Modify: `docs/examples/flows/devrun/flow_run.waiting_approval.yaml`

**Required schema support:**

- `status` enum includes `waiting_human`.
- each step state can include:
  - `agent_run_id`
  - `human_request_ids`
  - `interrupt`
  - `outputs`
  - `artifacts`
- `pending_signals` can reference `human_request:<id>` or is replaced by `pending_human_requests`.

**Test/verification:**

```bash
python3 -m json.tool docs/schemas/xira-flow-run-v0.schema.json >/dev/null
python3 - <<'PY'
import json
from pathlib import Path
import yaml
from jsonschema import Draft202012Validator
schema = json.loads(Path("docs/schemas/xira-flow-run-v0.schema.json").read_text())
data = yaml.safe_load(Path("docs/examples/flows/devrun/flow_run.waiting_approval.yaml").read_text())
Draft202012Validator.check_schema(schema)
errors = sorted(Draft202012Validator(schema).iter_errors(data), key=lambda e: list(e.path))
assert not errors, [e.message for e in errors]
print("flow_run example OK")
PY
```

Expected: `flow_run example OK`.

### Task 0.3: Update Flow Definition Schema Only If Needed

**Files:**

- Modify: `docs/schemas/xira-flow-v0.schema.json`
- Modify: `docs/examples/flows/devrun/flow.yaml`

**Rules:**

- Keep `human_approval` executor.
- Do not add command executor.
- Add optional `human_request` block only if the current schema cannot express the required approval question/options.
- Keep agent executor as the only work executor.

**Test/verification:**

```bash
python3 -m json.tool docs/schemas/xira-flow-v0.schema.json >/dev/null
python3 - <<'PY'
import json
from pathlib import Path
import yaml
from jsonschema import Draft202012Validator
schema = json.loads(Path("docs/schemas/xira-flow-v0.schema.json").read_text())
data = yaml.safe_load(Path("docs/examples/flows/devrun/flow.yaml").read_text())
Draft202012Validator.check_schema(schema)
errors = sorted(Draft202012Validator(schema).iter_errors(data), key=lambda e: list(e.path))
assert not errors, [e.message for e in errors]
print("flow definition example OK")
PY
```

Expected: `flow definition example OK`.

---

## Milestone 1: Flow Definition Loader

### Objective

Load and validate `flow.yaml` into typed Go structs without executing it.

### Task 1.1: Add Flow Definition Types

**Files:**

- Create: `apps/xira/internal/flow/types.go`
- Test: `apps/xira/internal/flow/definition_test.go`

**Write failing tests first:**

- `TestLoadDefinitionDevRun`
- `TestLoadDefinitionRejectsDuplicateStepID`
- `TestLoadDefinitionRejectsMissingEntrypointStep`
- `TestLoadDefinitionRejectsMissingTransitionTarget`
- `TestLoadDefinitionRejectsUnknownExecutor`

**Minimum types:**

```go
package flow

type Definition struct {
    SchemaVersion string       `yaml:"schema_version" json:"schema_version"`
    ID            string       `yaml:"id" json:"id"`
    Version       string       `yaml:"version" json:"version"`
    Name          string       `yaml:"name" json:"name"`
    Description   string       `yaml:"description,omitempty" json:"description,omitempty"`
    Entrypoints   []Entrypoint `yaml:"entrypoints,omitempty" json:"entrypoints,omitempty"`
    Agents        []AgentRef   `yaml:"agents,omitempty" json:"agents,omitempty"`
    Steps         []Step       `yaml:"steps" json:"steps"`
}

type Entrypoint struct {
    ID        string   `yaml:"id" json:"id"`
    Channel   string   `yaml:"channel,omitempty" json:"channel,omitempty"`
    Aliases   []string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
    StartStep string   `yaml:"start_step" json:"start_step"`
}

type Step struct {
    ID              string                 `yaml:"id" json:"id"`
    Name            string                 `yaml:"name,omitempty" json:"name,omitempty"`
    Objective       string                 `yaml:"objective" json:"objective"`
    Executor        Executor               `yaml:"executor" json:"executor"`
    Instructions    []string               `yaml:"instructions,omitempty" json:"instructions,omitempty"`
    Constraints     []string               `yaml:"constraints,omitempty" json:"constraints,omitempty"`
    RequiredSkills  []string               `yaml:"required_skills,omitempty" json:"required_skills,omitempty"`
    Inputs          map[string]string      `yaml:"inputs,omitempty" json:"inputs,omitempty"`
    OutputContract  OutputContract         `yaml:"output_contract,omitempty" json:"output_contract,omitempty"`
    Transitions     Transitions            `yaml:"transitions,omitempty" json:"transitions,omitempty"`
    Metadata        map[string]interface{} `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}

type Executor struct {
    Agent string `yaml:"agent,omitempty" json:"agent,omitempty"`
    Type  string `yaml:"type,omitempty" json:"type,omitempty"`
}
```

Adjust field names to match the live `flow.yaml`; tests are authoritative.

**Run failing test:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestLoadDefinition -v
```

Expected before implementation: compile failure or failing loader tests.

### Task 1.2: Implement Loader And Structural Validation

**Files:**

- Create: `apps/xira/internal/flow/definition.go`
- Modify: `apps/xira/internal/flow/types.go`
- Test: `apps/xira/internal/flow/definition_test.go`

**Implementation rules:**

- Use a YAML parser already available in the module. If none exists, add the same dependency pattern used elsewhere in `apps/xira`.
- `LoadDefinition(path string) (*Definition, error)`
- `ValidateDefinition(def *Definition) error`
- Do not evaluate transition expressions here.
- Return errors that include enough context: step id, entrypoint id, missing target id.

**Required validation:**

- `schema_version == "xira.flow.v0"`
- `id` is non-empty.
- at least one step.
- step ids are unique and non-empty.
- every entrypoint `start_step` exists.
- every transition target exists, except reserved terminal targets if explicitly documented.
- each step has exactly one executor form:
  - work step: `executor.agent` non-empty and `executor.type` empty
  - control step: `executor.type` one of `human_approval`, `decision`, `wait_signal`, `subflow`
- no `command` executor.

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestLoadDefinition -v
```

Expected: all loader tests pass.

### Task 1.3: Add Entrypoint Resolution

**Files:**

- Modify: `apps/xira/internal/flow/definition.go`
- Test: `apps/xira/internal/flow/definition_test.go`

**Tests:**

- `TestResolveEntrypointAdHoc`
- `TestResolveEntrypointBugfix`
- `TestResolveEntrypointIssuePickup`
- `TestResolveEntrypointRejectsUnknown`

**Behavior:**

```go
func (d *Definition) ResolveEntrypoint(id string) (*Entrypoint, *Step, error)
```

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run 'TestResolveEntrypoint|TestLoadDefinition' -v
```

Expected: pass.

---

## Milestone 2: Flow Run Store

### Objective

Persist FlowRun state safely and read it after process restart.

### Task 2.1: Add Flow Run Types

**Files:**

- Modify: `apps/xira/internal/flow/types.go`
- Create or modify: `apps/xira/internal/flow/store_test.go`

**Write failing tests:**

- `TestStoreCreateAndGetFlowRun`
- `TestStoreUpdateCurrentStep`
- `TestStoreRecordsOutputSlotsAndArtifacts`
- `TestStoreRejectsPathTraversalRunID`
- `TestStoreAppendEvents`

**Minimum types:**

```go
type RunStatus string
type StepStatus string

type Run struct {
    SchemaVersion string               `yaml:"schema_version" json:"schema_version"`
    ID            string               `yaml:"flow_run_id" json:"flow_run_id"`
    FlowID        string               `yaml:"flow_id" json:"flow_id"`
    FlowVersion   string               `yaml:"flow_version" json:"flow_version"`
    Status        RunStatus            `yaml:"status" json:"status"`
    CurrentStepID string               `yaml:"current_step_id,omitempty" json:"current_step_id,omitempty"`
    EntrypointID  string               `yaml:"entrypoint_id,omitempty" json:"entrypoint_id,omitempty"`
    Input         map[string]string    `yaml:"input,omitempty" json:"input,omitempty"`
    Steps         map[string]StepState `yaml:"steps,omitempty" json:"steps,omitempty"`
    Artifacts     []ArtifactRef        `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
    CreatedAt     time.Time            `yaml:"created_at" json:"created_at"`
    UpdatedAt     time.Time            `yaml:"updated_at" json:"updated_at"`
}

type StepState struct {
    Status          StepStatus                 `yaml:"status" json:"status"`
    AgentRunID      string                     `yaml:"agent_run_id,omitempty" json:"agent_run_id,omitempty"`
    HumanRequestIDs []string                   `yaml:"human_request_ids,omitempty" json:"human_request_ids,omitempty"`
    Outputs         map[string]OutputRef       `yaml:"outputs,omitempty" json:"outputs,omitempty"`
    Interrupt       map[string]interface{}     `yaml:"interrupt,omitempty" json:"interrupt,omitempty"`
    Error           string                     `yaml:"error,omitempty" json:"error,omitempty"`
    StartedAt       *time.Time                 `yaml:"started_at,omitempty" json:"started_at,omitempty"`
    CompletedAt     *time.Time                 `yaml:"completed_at,omitempty" json:"completed_at,omitempty"`
}
```

**Run failing tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestStore -v
```

Expected before implementation: compile failure or failing store tests.

### Task 2.2: Implement File-Backed Store

**Files:**

- Create: `apps/xira/internal/flow/store.go`
- Test: `apps/xira/internal/flow/store_test.go`

**Required behavior:**

- `NewStore(root string) *Store`
- `CreateRun(ctx, CreateRunRequest) (*Run, error)`
- `GetRun(ctx, flowRunID string) (*Run, error)`
- `UpdateRun(ctx, flowRunID string, fn func(*Run) error) (*Run, error)`
- `AppendEvent(ctx, flowRunID string, Event) error`
- Atomic write via temp file and rename, matching existing store style if present.
- Path traversal rejection for run ids and artifact refs.

**File layout:**

```text
<stateRoot>/flow-runs/<flow_run_id>/flow_run.yaml
<stateRoot>/flow-runs/<flow_run_id>/events.jsonl
<stateRoot>/flow-runs/<flow_run_id>/artifacts/
```

If the existing runtime uses `.xira/runs` under workspace rather than raw state root, follow the local convention but preserve these conceptual paths.

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestStore -v
```

Expected: pass.

### Task 2.3: Add Store Idempotency And Concurrency Guard

**Files:**

- Modify: `apps/xira/internal/flow/store.go`
- Modify: `apps/xira/internal/flow/store_test.go`

**Tests:**

- `TestStoreCreateRunIdempotentForSameID`
- `TestStoreConcurrentUpdateDoesNotCorruptRun`
- `TestStoreRejectsCompletedStepOverwriteWithoutRetry`

**Behavior:**

- Creating the same run id twice with same flow id returns the existing run or a clear already-exists error. Pick the existing store pattern.
- Concurrent updates must not produce malformed YAML or lost status.
- A helper should prevent accidental overwrite of completed step state unless the caller explicitly requests retry.

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -race -count=1 ./apps/xira/internal/flow -run TestStore -v
```

Expected: pass. If `-race` is too slow in the environment, run without `-race` and document why.

---

## Milestone 3: Minimal Flow Kernel

### Objective

Start a FlowRun and advance control steps without calling real agents yet.

### Task 3.1: Add Kernel Skeleton

**Files:**

- Create: `apps/xira/internal/flow/kernel.go`
- Create: `apps/xira/internal/flow/kernel_test.go`

**Tests:**

- `TestKernelStartFlowFromAdHocEntrypoint`
- `TestKernelStartFlowFromBugfixEntrypoint`
- `TestKernelRejectsUnknownEntrypoint`
- `TestKernelCompletesFlowWhenNoNextStep`

**Minimum API:**

```go
type Kernel struct {
    Store *Store
    Definitions DefinitionSource
    Executor StepExecutor
}

type StartRequest struct {
    FlowPath     string
    FlowID       string
    EntrypointID string
    Input        map[string]string
}

func (k *Kernel) Start(ctx context.Context, req StartRequest) (*Run, error)
func (k *Kernel) Advance(ctx context.Context, flowRunID string) (*Run, error)
func (k *Kernel) Resume(ctx context.Context, flowRunID string, humanRequestID string) (*Run, error)
```

**Run failing tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestKernel -v
```

Expected before implementation: failing tests.

### Task 3.2: Implement Start And Simple Advance

**Files:**

- Modify: `apps/xira/internal/flow/kernel.go`
- Modify: `apps/xira/internal/flow/kernel_test.go`

**Behavior:**

- `Start` loads definition, resolves entrypoint, creates run, initializes first step as `pending`, sets run status `running`, sets `current_step_id`.
- `Advance` marks current step `running`, delegates to `StepExecutor`, stores result, then advances according to transition.
- For v0, if a step has no transition and completes, Flow completes.

**Test executor:**

Use a fake executor in tests:

```go
type fakeStepExecutor struct {
    results map[string]StepExecutionResult
}
```

No real model calls in kernel tests.

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestKernel -v
```

Expected: pass.

### Task 3.3: Implement Transition Resolution

**Files:**

- Modify: `apps/xira/internal/flow/kernel.go`
- Modify: `apps/xira/internal/flow/kernel_test.go`

**Tests:**

- `TestKernelAdvancesToExplicitNextStep`
- `TestKernelDecisionBranchApprove`
- `TestKernelDecisionBranchRevise`
- `TestKernelDecisionBranchCancel`
- `TestKernelRejectsUnresolvableTransition`

**Behavior:**

- Support simple `next` transitions.
- Support `branches` with `when` expressions only for the expression forms already present in DevRun.
- If expression parsing becomes too broad, implement a deliberately tiny evaluator:
  - `${outputs.<step>.<slot> == 'value'}`
  - `${runtime.policy.<key> == true}`
  - `${runtime.policy.<key> != true}`
- No general-purpose scripting.

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run 'TestKernel.*Branch|TestKernelAdvances|TestKernelRejects' -v
```

Expected: pass.

---

## Milestone 4: Agent Step Execution Adapter

### Objective

Call the existing Agent Runtime for `executor.agent` steps and translate `TurnResponse` into Flow step state.

### Task 4.1: Define StepExecutor Interface And Result Contract

**Files:**

- Create: `apps/xira/internal/flow/executor.go`
- Create: `apps/xira/internal/flow/executor_test.go`

**Tests:**

- `TestAgentExecutorBuildsTurnRequestFromStep`
- `TestAgentExecutorMapsCompletedResponse`
- `TestAgentExecutorMapsFailedResponse`
- `TestAgentExecutorMapsWaitingHumanResponse`

**Minimum contracts:**

```go
type StepExecutor interface {
    ExecuteStep(ctx context.Context, run *Run, def *Definition, step Step) (StepExecutionResult, error)
}

type StepExecutionResult struct {
    Status          StepStatus
    AgentRunID      string
    HumanRequestIDs []string
    Outputs         map[string]OutputRef
    Artifacts       []ArtifactRef
    Interrupt       map[string]interface{}
    Error           string
}
```

**Run failing tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestAgentExecutor -v
```

Expected before implementation: failing tests.

### Task 4.2: Implement Agent Executor Adapter

**Files:**

- Modify: `apps/xira/internal/flow/executor.go`
- Modify if needed: `apps/xira/internal/runtime/service.go`
- Test: `apps/xira/internal/flow/executor_test.go`

**Behavior:**

- Build `runtime.TurnRequest` from:
  - Flow input
  - step objective
  - step instructions
  - constraints
  - required skills
  - resolved input slots
  - output contract
- Call an interface around `RunAgent` rather than concrete service where possible:

```go
type AgentRunner interface {
    RunAgent(ctx context.Context, req runtime.TurnRequest) (runtime.TurnResponse, error)
}
```

- Map response:
  - `completed` -> step `completed`
  - `failed` -> step `failed`
  - `waiting_human` -> step `waiting_human`
- Save `resp.RunID` as `AgentRunID`.
- Save `resp.HumanRequests[].ID` as `HumanRequestIDs`.
- Save `resp.Interrupt` summary as step interrupt.

**Output slot mapping:**

v0 acceptable minimum:

- If the agent response exposes structured artifact refs, map them to declared output slots.
- If no structured refs exist, create a conservative `summary` output from final response only when declared.
- Required output slots must be present for `completed`.

**Tests:**

- Completed agent with all required slots -> completed.
- Completed agent missing required slot -> failed with error mentioning slot.
- Waiting human agent -> waiting_human with human_request_ids.
- Failed agent -> failed.

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestAgentExecutor -v
```

Expected: pass.

### Task 4.3: Resolve Upstream Output Slot References

**Files:**

- Modify: `apps/xira/internal/flow/kernel.go`
- Modify: `apps/xira/internal/flow/executor.go`
- Test: `apps/xira/internal/flow/kernel_test.go`

**Tests:**

- `TestKernelResolvesInputFromPreviousStepOutput`
- `TestKernelFailsOnMissingRequiredInputOutput`

**Behavior:**

- Resolve strings like `${outputs.design.implementation_plan}` from prior step state.
- Do not evaluate arbitrary templates.
- Return clear error if referenced step/slot is missing.

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run 'TestKernelResolvesInput|TestKernelFailsOnMissingRequiredInput' -v
```

Expected: pass.

---

## Milestone 5: HumanRequest And Resume Integration

### Objective

Use existing `HumanRequest` for explicit Flow approval and correctly pause/resume agent-generated waiting.

### Task 5.1: Explicit `human_approval` Step Creates HumanRequest

**Files:**

- Modify: `apps/xira/internal/flow/executor.go`
- Create or modify: `apps/xira/internal/flow/human_request_test.go`

**Tests:**

- `TestHumanApprovalStepCreatesHumanRequest`
- `TestHumanApprovalStepSetsFlowWaitingHuman`
- `TestHumanApprovalStepStoresFlowScopeMetadata`

**Behavior:**

- For `executor.type: human_approval`, create `HumanRequest` through the existing runtime/service or humanrequest store adapter.
- Store:
  - workspace id/key from runtime config
  - kind `approval`
  - source `flow_human_approval`
  - question from step definition
  - options from step definition or default approve/deny/cancel
  - metadata keys listed in `ASSUMPTION-FLOW-003`
- Return `StepExecutionResult{Status: waiting_human, HumanRequestIDs: []string{id}}`.

**Run failing test:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestHumanApproval -v
```

Expected before implementation: failing tests.

### Task 5.2: Kernel Pauses On Waiting Human

**Files:**

- Modify: `apps/xira/internal/flow/kernel.go`
- Test: `apps/xira/internal/flow/human_request_test.go`

**Tests:**

- `TestKernelPausesFlowOnHumanApproval`
- `TestKernelPausesFlowOnAgentGeneratedHumanRequest`
- `TestKernelDoesNotAdvancePastWaitingHumanStep`

**Behavior:**

- If step result is `waiting_human`:
  - current step status `waiting_human`
  - run status `waiting_human`
  - `current_step_id` remains the waiting step
  - human request ids are persisted
  - no transition is evaluated yet

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run 'TestKernelPauses|TestHumanApproval' -v
```

Expected: pass.

### Task 5.3: Resume After HumanRequest Resolve

**Files:**

- Modify: `apps/xira/internal/flow/kernel.go`
- Modify if needed: `apps/xira/internal/humanrequest/types.go`
- Test: `apps/xira/internal/flow/human_request_test.go`

**Tests:**

- `TestKernelResumeApprovalApproveAdvances`
- `TestKernelResumeApprovalDenyBranches`
- `TestKernelResumeApprovalCancelCancelsFlow`
- `TestKernelResumeRejectsUnresolvedRequest`
- `TestKernelResumeIsIdempotent`

**Behavior:**

- `Resume(ctx, flowRunID, humanRequestID)`:
  - loads flow run
  - confirms run status is `waiting_human`
  - confirms current step contains `humanRequestID`
  - loads HumanRequest
  - rejects if request is still pending
  - maps response:
    - `approve` -> expose approval output slot as `approve`
    - `deny` -> expose approval output slot as `deny` or `reject`
    - `cancel` -> cancel flow unless step transition explicitly handles it
    - `answer` -> resume agent step if the waiting came from agent-generated request
- For explicit `human_approval`, after response is mapped, evaluate transitions.
- For agent-generated waiting, call existing runtime resume path if available; otherwise start a continuation turn using existing Agent Runtime behavior. Do not invent model-visible `human.respond`.

WATCH-FLOW-001: Existing `ResolveHumanRequest` may automatically resume/replay agent/tool paths. Flow resume must not double-resume the same agent run. Tests must prove no double advance.

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestKernelResume -v
```

Expected: pass.

### Task 5.4: Child Waiting Propagation Scenario

**Files:**

- Modify: `apps/xira/internal/flow/human_request_test.go`
- Modify: `apps/xira/internal/flow/executor.go`

**Test:**

- `TestKernelPausesWhenParentAgentRunWaitingOnChildHumanRequest`

**Scenario:**

Fake agent runner returns:

```go
runtime.TurnResponse{
    Status: runtime.StatusWaitingHuman,
    RunID: "parent-run-1",
    HumanRequests: []humanrequest.HumanRequest{{ID: "hr_child_1", Source: "agent_request"}},
    Interrupt: &runtime.RunInterrupt{
        Status: runtime.StatusWaitingHuman,
        BlockedBy: []runtime.BlockedBy{{Type: "child_human_request", HumanRequestID: "hr_child_1"}},
    },
}
```

Expected:

- Flow step status `waiting_human`.
- Flow run status `waiting_human`.
- `AgentRunID` is parent run id.
- Flow does not inspect child run files or delegation join state.

---

## Milestone 6: API And CLI Surfaces

### Objective

Expose enough commands/endpoints to run and inspect Flow v0 without UI work.

### Task 6.1: Add Runtime Service Wiring

**Files:**

- Modify: `apps/xira/internal/runtime/service.go`
- Modify: `apps/xira/internal/runtime/config.go`
- Test: existing runtime tests plus new API/CLI tests.

**Behavior:**

- Runtime service owns or can construct:
  - Flow definition loader
  - Flow store
  - Flow kernel
- Provide methods:

```go
StartFlow(ctx context.Context, req flow.StartRequest) (*flow.Run, error)
AdvanceFlow(ctx context.Context, flowRunID string) (*flow.Run, error)
ResumeFlow(ctx context.Context, flowRunID string, humanRequestID string) (*flow.Run, error)
GetFlowRun(ctx context.Context, flowRunID string) (*flow.Run, error)
```

Keep package boundaries clean: `runtime.Service` can orchestrate Flow, but core Flow logic stays in `internal/flow`.

### Task 6.2: Add CLI Commands

**Files:**

- Modify: `apps/xira/cmd/xira/main.go`
- Modify: `apps/xira/cmd/xira/main_test.go`

**Commands:**

```bash
xira flow run <flow-file> --entrypoint ad_hoc --input request="..."
xira flow status <flow_run_id>
xira flow advance <flow_run_id>
xira flow resume <flow_run_id> --human-request <id>
```

**Tests:**

- `TestFlowRunCommandStartsRun`
- `TestFlowStatusCommandShowsCurrentStep`
- `TestFlowAdvanceCommandAdvancesStep`
- `TestFlowResumeCommandRequiresHumanRequestID`

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/cmd/xira -run TestFlow -v
```

Expected: pass.

### Task 6.3: Add API Endpoints

**Files:**

- Modify: `apps/xira/internal/api/server.go`
- Modify: `apps/xira/internal/api/server_test.go`

**Endpoints:**

```text
POST /api/v1/flows/runs
GET  /api/v1/flows/runs/{flow_run_id}
POST /api/v1/flows/runs/{flow_run_id}/advance
POST /api/v1/flows/runs/{flow_run_id}/resume
```

**Tests:**

- `TestPostFlowRunStartsRun`
- `TestGetFlowRun`
- `TestPostFlowRunAdvance`
- `TestPostFlowRunResume`
- `TestPostFlowRunRejectsUnknownEntrypoint`

**Run tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/api -run Test.*Flow -v
```

Expected: pass.

---

## Milestone 7: DevRun Vertical Slice

### Objective

Make the existing DevRun example the primary acceptance scenario.

### Task 7.1: Run DevRun Happy Path With Fake Agent

**Files:**

- Create or modify: `apps/xira/internal/flow/devrun_test.go`

**Test:**

- `TestDevRunHappyPathCompletesWithFakeAgent`

**Scenario:**

- Load `docs/examples/flows/devrun/flow.yaml`.
- Start with `entrypoint=ad_hoc`.
- Fake agent returns required outputs for:
  - intake
  - design
  - prepare_branch
  - implement
  - verify
  - create_pr
  - review
  - report
- Policy disables design/merge approval for this test.

**Expected:**

- Flow completed.
- Every completed step has status `completed`.
- Required output slots are present.
- `flow_run.yaml` only references artifacts/slots, no large logs.

### Task 7.2: DevRun Design Approval Pause/Resume

**Files:**

- Modify: `apps/xira/internal/flow/devrun_test.go`

**Test:**

- `TestDevRunDesignApprovalPausesAndResumes`

**Scenario:**

- Policy requires design approval.
- Advance until `approve_design`.
- Flow becomes `waiting_human`.
- Resolve the created HumanRequest with `approve`.
- Resume flow.
- Flow continues to `prepare_branch` or next expected step.

**Expected:**

- HumanRequest source `flow_human_approval`.
- Metadata includes `flow_run_id` and `flow_step_id=approve_design`.
- No implementation step executes before approval.

### Task 7.3: DevRun Merge Approval Deny

**Files:**

- Modify: `apps/xira/internal/flow/devrun_test.go`

**Test:**

- `TestDevRunMergeApprovalDenyDoesNotMerge`

**Scenario:**

- Flow reaches `approve_merge`.
- Human denies.
- Flow follows deny/reject/cancel path according to DevRun transition.

**Expected:**

- `merge` step is not executed.
- Flow ends as `canceled` or completed-with-rejected-merge depending on documented transition.
- Report step records remaining risk/status if transition sends it to report.

### Task 7.4: DevRun Review Fix Loop

**Files:**

- Modify: `apps/xira/internal/flow/devrun_test.go`

**Test:**

- `TestDevRunReviewBlockingFindingsEnterFixLoop`

**Scenario:**

- review step output indicates blocking findings.
- `fix_or_approve` decision sends flow to `fix`.
- fake fix agent completes.
- flow returns to verify/review or documented next step.

**Expected:**

- `fix` step executed exactly once.
- post-fix verification executes.
- completed flow includes review artifact and fix artifact refs.

### Task 7.5: DevRun Verify Failure Stops PR Creation

**Files:**

- Modify: `apps/xira/internal/flow/devrun_test.go`

**Test:**

- `TestDevRunVerifyFailureDoesNotCreatePR`

**Scenario:**

- implement completed.
- verify failed.

**Expected:**

- create_pr step is not executed.
- flow failed or enters documented fix/error transition.
- failure includes verify result artifact ref.

**Run all DevRun tests:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestDevRun -v
```

Expected: pass.

---

## Milestone 8: Hardening And Regression Suite

### Objective

Prevent demo-only behavior and preserve runtime boundaries.

### Task 8.1: Idempotency And Double Resume

**Files:**

- Modify: `apps/xira/internal/flow/kernel_test.go`
- Modify: `apps/xira/internal/flow/store_test.go`

**Tests:**

- `TestFlowResumeSameHumanRequestTwiceAdvancesOnce`
- `TestFlowAdvanceCompletedStepDoesNotReexecute`
- `TestFlowAdvanceWaitingHumanDoesNotExecute`

**Expected:**

- no duplicate agent execution
- no duplicate output slots
- no duplicate events except explicit idempotent audit

### Task 8.2: Workspace And Artifact Boundary

**Files:**

- Modify: `apps/xira/internal/flow/store_test.go`
- Modify: `apps/xira/internal/flow/executor_test.go`

**Tests:**

- `TestFlowStoreRejectsArtifactPathTraversal`
- `TestFlowExecutorRejectsOutputArtifactOutsideRunDir`
- `TestFlowDoesNotBypassAgentToolPolicy`

**Expected:**

- artifact refs like `../../secret` are rejected.
- Flow cannot directly execute shell/command.
- agent/runtime still owns tool policy.

### Task 8.3: Event Chain Observability

**Files:**

- Modify: `apps/xira/internal/flow/kernel.go`
- Modify: `apps/xira/internal/flow/store.go`
- Modify: `apps/xira/internal/flow/kernel_test.go`

**Tests:**

- `TestFlowEventsLinkFlowStepAgentRunAndHumanRequest`

**Expected events:**

```text
flow.run.started
flow.step.started
flow.agent_run.started
flow.step.waiting_human
flow.human_request.linked
flow.step.completed
flow.run.completed
```

Names can follow existing event conventions, but tests must assert that a reader can trace:

```text
flow_run_id -> step_id -> agent_run_id -> human_request_id
```

### Task 8.4: Full Package Test Gate

**Run:**

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/api
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/cmd/xira
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/...
```

Expected: all pass outside restricted sandbox.

---

## Recommended Commit Sequence

Commit after each milestone, or after each task when the diff is large:

```bash
git add docs/architecture/xira-flow-v0.zh.md docs/architecture/xira-flow-v0-wip-notes.zh.md docs/schemas docs/examples/flows/devrun
git commit -m "docs: align flow hitl semantics"

git add apps/xira/internal/flow
git commit -m "feat: load and validate flow definitions"

git add apps/xira/internal/flow
git commit -m "feat: add flow run store"

git add apps/xira/internal/flow
git commit -m "feat: add minimal flow kernel"

git add apps/xira/internal/flow apps/xira/internal/runtime
git commit -m "feat: execute flow agent steps"

git add apps/xira/internal/flow
git commit -m "feat: pause and resume flow human requests"

git add apps/xira/internal/api apps/xira/cmd/xira apps/xira/internal/runtime
git commit -m "feat: expose flow run cli and api"

git add apps/xira/internal/flow
git commit -m "test: add devrun flow vertical slice"
```

---

## Final Acceptance Checklist

Flow v0 runtime is complete when all items below are true:

- [ ] `docs/examples/flows/devrun/flow.yaml` loads through Go loader.
- [ ] `docs/examples/flows/devrun/flow.yaml` validates against JSON Schema.
- [ ] `docs/examples/flows/devrun/flow_run.waiting_approval.yaml` validates against JSON Schema.
- [ ] `xira flow run docs/examples/flows/devrun/flow.yaml --entrypoint ad_hoc` creates a FlowRun.
- [ ] FlowRun persists to disk and can be read after process restart.
- [ ] Flow can advance across at least three fake-agent steps.
- [ ] Flow records `agent_run_id` for agent executor steps.
- [ ] Flow records output slots and artifact refs.
- [ ] Flow fails a completed agent step when required output slots are missing.
- [ ] Flow explicit `human_approval` step creates a `HumanRequest`.
- [ ] Flow pauses as `waiting_human` on explicit `human_approval`.
- [ ] Flow resumes after approval and advances to the correct next step.
- [ ] Flow handles deny/cancel without executing forbidden later steps.
- [ ] Agent-generated `HumanRequest` pauses the current Flow step.
- [ ] Runtime tool gate `waiting_human` pauses the current Flow step.
- [ ] Child delegation `waiting_human` pauses Flow through parent agent run status.
- [ ] Re-resolving or re-resuming the same HumanRequest does not double-advance.
- [ ] DevRun happy path completes with fake agents.
- [ ] DevRun design approval pause/resume passes.
- [ ] DevRun merge approval deny does not execute merge.
- [ ] DevRun review fix loop passes.
- [ ] DevRun verify failure does not create PR.
- [ ] Flow state never stores large stdout/stderr or full raw transcripts.
- [ ] Flow cannot directly run shell/command outside agent runtime/tool policy.
- [ ] `GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/...` passes.

---

## Definition Of Done For This Epic

The epic is done when a fresh AI worker can:

1. Start from this plan with no prior chat context.
2. Run the baseline tests.
3. Implement each task in order using TDD.
4. Verify the full DevRun vertical slice.
5. Explain the Flow/HITL boundary using only code, tests, and docs in this repository.
