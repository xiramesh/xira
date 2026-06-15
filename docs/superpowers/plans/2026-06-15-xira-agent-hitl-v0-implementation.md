# Xira Agent HITL v0 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement Agent HITL v0 exactly as specified in `docs/architecture/xira-agent-hitl-v0.zh.md`, with `child waiting_human` included in v0, a unified `RunInterrupt` / `RuntimeSuspendCollector` runtime contract, minimal response API before replay/resume, and a test suite that makes every state transition and boundary explicit.

**Architecture:** Add a persistent `HumanRequest` domain/store below runtime, add a runtime suspend contract above model/tool execution, make native and ADK execution paths stop on suspend instead of continuing model calls, make `delegate_agent` suspendable, and expose minimal response/list/show surfaces through API and CLI. `HITL` is the feature name; package names, file names, config keys, and persisted protocol fields use `humanrequest`, `human_request`, `human_response`, `interrupt`, `suspend`, or `delegation` according to the runtime concept they represent. Fake-model tests are the primary correctness gate; real DeepSeek tests are env-gated smoke tests for model/tool integration only.

**Tech Stack:** Go, Xira runtime, file-backed state under `stateRoot`, Cobra CLI, `net/http` API tests with `httptest`, fake DeepSeek client for deterministic runtime tests, optional live DeepSeek tests gated by `XIRA_DEEPSEEK_LIVE=1` and `DEEPSEEK_API_KEY`.

---

## Implementation Progress Snapshot

Status as of 2026-06-15 implementation pass:

- Completed: `internal/humanrequest` domain store, workspace-safe file layout, request resolve, replay CAS, replay failure recording, and audit records.
- Completed: minimal response API before replay/resume, API list/show, and `xira human list/show/approve/deny/cancel/answer`.
- Completed: `RunInterrupt` / `RuntimeSuspendCollector`, `waiting_human` run status, native/ADK short-circuit, `human.request`, and trusted-context `human.respond`.
- Completed: ADK and native `RequireConfirmation` gate, action snapshot creation, synchronous approve replay, deny/cancel no-replay behavior, replay idempotency, replay lease conflict, and workspace containment enforcement for file tools.
- Completed: delegation child `waiting_human` propagation, persisted `.xira/runs/<parent_run_id>/delegations/<delegation_join_id>.yaml`, active `max_parallel` release, persisted `max_outstanding` counting, child answer/approve resume, deny/cancel materialization, parent delegate output materialization, parent continuation after child resolution, and restart recovery.
- Completed: deterministic fake-model E2E for direct `human.request`, runtime tool approval replay, delegation completed, delegation child waiting approve resume, delegation child waiting cancel, process restart before response, retry dedupe, and workspace isolation.
- Completed: env-gated live DeepSeek smoke tests using `deepseek-v4-pro`; they skip unless `XIRA_DEEPSEEK_LIVE=1` and `DEEPSEEK_API_KEY` are set. The live suite was explicitly executed in this pass and passed.

Verified commands in this pass:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/humanrequest ./apps/xira/internal/agents ./apps/xira/internal/tools ./apps/xira/internal/runtime ./apps/xira/cmd/xira
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/api -run 'TestPostHumanRequestResponse|TestListHumanRequests|TestShowHumanRequest'
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestE2E'
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestHumanRequest|TestHumanRespond|TestRequireConfirmation|TestReplay|TestDelegate'
XIRA_DEEPSEEK_LIVE=1 XIRA_DEEPSEEK_MODEL=deepseek-v4-pro DEEPSEEK_API_KEY="$DEEPSEEK_API_KEY" GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run TestRealDeepSeekHITL -v
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/...
```

The `./apps/xira/...` command passed outside the restricted sandbox. Inside the sandbox it fails because `httptest` cannot bind `127.0.0.1:0` / `[::1]:0`.

The repository root command below is not a valid Go workspace command in this repo because `go.work` only lists `./apps/xira`:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./...
```

Observed result:

```text
pattern ./...: directory prefix . does not contain modules listed in go.work or their selected dependencies
```

---

## Non-Negotiable Constraints

- [x] Do not start implementation until the failing tests for the current task exist.
- [x] Do not modify `docs/architecture/xira-agent-hitl-v0-review.zh.md` in this development pass.
- [x] Keep `docs/architecture/xira-agent-hitl-v0.zh.md` as the source spec. This plan may reference it, but implementation changes belong in Go code and focused tests.
- [x] Keep v0 scope to agent-run HITL. No flow-level approval graph, no multi-approver quorum, no UI approval panel, no broad policy DSL.
- [x] Treat `waiting_human` as a first-class run status.
- [x] Treat `canceled` as delegate output status only. Unless the architecture document is changed later, run status remains `failed` with `error_type=canceled` for canceled child runs.
- [x] Minimal `POST /human-requests/{id}/responses` must land before child resume and snapshot replay.
- [x] A suspended child waiting for human input releases active `max_parallel` execution slots, but still counts against `max_outstanding`.
- [x] All real DeepSeek tests must be skipped unless explicitly enabled with `XIRA_DEEPSEEK_LIVE=1`.

---

## AI Handoff Context Protocol

This document is intended for a fresh AI worker that may not have the original discussion context. Use the following markers instead of vague notes:

- `AI-TODO-VERIFY`: the worker must verify this from the live repo before editing code.
- `OPEN-DECISION`: the worker must resolve this before implementing the affected phase. Use the documented default unless the live code or architecture doc contradicts it.
- `ASSUMPTION`: proceed with this as the implementation rule. Do not ask the user unless it conflicts with code or tests.
- `DEFERRED-v0`: intentionally out of v0 scope. Do not implement it in this pass.
- `BLOCKED-UNTIL`: work cannot be fully verified until the listed external condition exists.
- `WATCH`: known risk area that needs an explicit regression test.

Fresh-window startup checklist:

- [x] AI-TODO-VERIFY: Read `docs/architecture/xira-agent-hitl-v0.zh.md` before touching code.
- [x] AI-TODO-VERIFY: Read this plan from top to bottom and keep checkbox status current while working.
- [x] AI-TODO-VERIFY: Run `git status --short` and identify user-owned changes before editing.
- [x] AI-TODO-VERIFY: Inspect current runtime files: `apps/xira/internal/runtime/service.go`, `apps/xira/internal/runtime/delegation.go`, `apps/xira/internal/runtime/types.go`.
- [x] AI-TODO-VERIFY: Inspect current API/CLI entry points: `apps/xira/internal/api/server.go`, `apps/xira/cmd/xira/main.go`.
- [x] AI-TODO-VERIFY: Run the Phase 0 baseline command and record whether failures predate HITL work.

Known decisions and defaults:

- [x] ASSUMPTION-HITL-001: `waiting_human` is a real run status for v0.
- [x] ASSUMPTION-HITL-002: `canceled` is not a run status in v0. Canceled child runs are `failed` with `error_type=canceled`; parent delegate output status is `canceled`.
- [x] ASSUMPTION-HITL-003: Minimal response API lands before replay/resume and is the synchronous trigger point for v0.
- [x] ASSUMPTION-HITL-004: Fake-model tests are correctness tests. Real DeepSeek tests are smoke tests and must not replace deterministic tests.
- [x] ASSUMPTION-HITL-005: Response handlers must not trust caller-supplied workspace. Resolve workspace from persisted request/runtime context.
- [x] OPEN-DECISION-HITL-001: Store encoding must follow existing Xira state-store conventions after inspection. Default if there is no clear convention: JSON with atomic temp-file write plus rename. The behavior tests in Phase 1 are authoritative; formatting is not.
- [x] OPEN-DECISION-HITL-002: If `env_hash` or execution-context hash already exists, replay must enforce it. If it does not exist, v0 must still store enough snapshot metadata to add the check later and must include a `WATCH` test around policy/path/allowlist revalidation.
- [x] OPEN-DECISION-HITL-003: If current API has no authenticated workspace identity, response API must still prevent workspace override and must return `404` rather than leaking cross-workspace existence.
- [x] BLOCKED-UNTIL-HITL-001: Live DeepSeek smoke tests require `DEEPSEEK_API_KEY` and `XIRA_DEEPSEEK_LIVE=1`.
- [x] DEFERRED-v0-HITL-001: UI approval panel is out of scope.
- [x] DEFERRED-v0-HITL-002: Flow-level approval graph is out of scope.
- [x] DEFERRED-v0-HITL-003: Multi-approver quorum is out of scope.
- [x] DEFERRED-v0-HITL-004: Broad policy DSL is out of scope.

---

## Current Implementation Risk Map

The current runtime is mostly synchronous:

- `RunAgent -> generate` returns final/tool calls/error and then the service continues toward completed/failed.
- The native tool path can execute tools and send tool output into a second model call.
- `delegate_agent` calls `RunChildAgent` synchronously and expects `childResp.FinalResponse` to validate into a delegate result.
- Existing `activeChildren` slot accounting is in-memory and released by `defer`.

The implementation must therefore add a suspend channel that sits above both model paths and delegation:

```go
type RunInterrupt struct {
    Status              string
    Reason              string
    HumanRequests       []humanrequest.HumanRequest
    BlockedBy           []BlockedBy
    SuspendedToolCalls  []SuspendedToolCall
    DelegationJoinIDs   []string
    Metadata            map[string]any
}

type RuntimeSuspendCollector interface {
    RequestHuman(ctx context.Context, req humanrequest.CreateRequest) (*humanrequest.HumanRequest, error)
    SuspendToolCall(ctx context.Context, call SuspendedToolCall) error
    Interrupt() *RunInterrupt
    HasInterrupt() bool
}
```

The exact struct fields may evolve during implementation, but the runtime contract must preserve these observable meanings:

- `status=waiting_human`
- a concrete list of `human_requests`
- `blocked_by` entries that explain whether the block is direct human request, runtime tool confirmation, or child run request
- suspended tool calls that can be replayed or materialized later
- delegation join ids for parent resume
- no native/ADK second model call after suspend

---

## File-Level Target Shape

- [x] Add `apps/xira/internal/humanrequest/types.go`
  - Owns `HumanRequest`, `HumanResponse`, `ActionSnapshot`, `ReplayState`, `RequestStatus`, `ResponseKind`, `WorkspaceRef`.
- [x] Add `apps/xira/internal/humanrequest/store.go`
  - Owns file-backed create/list/show/resolve/replay-CAS operations.
- [x] Add `apps/xira/internal/humanrequest/store_test.go`
  - Pure store tests, no runtime.
- [x] Add `apps/xira/internal/runtime/interrupt.go`
  - Owns `RunInterrupt`, `BlockedBy`, `SuspendedToolCall`, `RuntimeSuspendCollector`, and service-backed collector implementation.
- [x] Modify `apps/xira/internal/runtime/types.go`
  - Add `TurnResponse.Interrupt`, `TurnResponse.HumanRequests`, and status constants if local patterns support them.
- [x] Modify `apps/xira/internal/runtime/service.go`
  - Wire `HumanRequest` store, suspend collector, status transition to `waiting_human`, replay entry points, and native/ADK short-circuit.
- [x] Add or modify `apps/xira/internal/runtime/human_requests.go`
  - Runtime-owned `human.request` and `human.respond`.
- [x] Modify `apps/xira/internal/runtime/delegation.go`
  - Convert child `waiting_human` into parent suspend; persist delegation join state; separate active slots from outstanding suspended children.
- [x] Add `apps/xira/internal/runtime/human_request_interrupt_test.go`
  - Runtime control tool tests.
- [x] Add `apps/xira/internal/runtime/human_request_interrupt_test.go`
  - Direct suspend and native/ADK short-circuit tests.
- [x] Add `apps/xira/internal/runtime/human_request_replay_test.go`
  - Snapshot replay tests.
- [x] Add `apps/xira/internal/runtime/delegation_suspend_test.go`
  - Child waiting propagation, join, cancel, deny, and slot accounting tests.
- [x] Modify `apps/xira/internal/api/server.go`
  - Add minimal `POST /api/v1/human-requests/{id}/responses`; later add list/show.
- [x] Modify `apps/xira/internal/api/server_test.go`
  - API response/list/show tests.
- [x] Modify `apps/xira/cmd/xira/main.go`
  - Add `human list/show/approve/deny/answer`.
- [x] Add or modify CLI tests under `apps/xira/cmd/xira`.
- [x] Add `apps/xira/internal/runtime/deepseek_hitl_live_test.go`
  - Env-gated live DeepSeek HITL smoke tests.

---

## Phase 0 - Baseline And Test Harness

- [x] Record dirty state before implementation.

  ```bash
  git status --short
  ```

  Expected: existing docs changes may be present; do not revert user-owned work.

- [x] Run current focused test baseline before adding failing tests.

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime ./apps/xira/internal/api
  ```

  Expected: either all pass, or existing failures are documented before HITL work starts.

- [x] Inspect current runtime status strings and avoid introducing duplicate enum definitions if constants already exist.

  ```bash
  rg "waiting_human|completed|failed|Status" apps/xira/internal/runtime apps/xira/internal/api apps/xira/cmd/xira
  ```

- [x] Add shared test helpers only when a second test file needs them:
  - `newHITLTestService(t)`
  - `mustRunAgent(t, svc, req)`
  - `assertWaitingHuman(t, resp)`
  - `assertNoSecondModelCall(t, fakeClient)`
  - `writeTempStateRoot(t)`

Completion gate:

- [x] Baseline result is written in the implementation notes or commit message.
- [x] No production behavior changed in this phase.

---

## Phase 1 - HITL Domain Model And File Store

Write tests first in `apps/xira/internal/humanrequest/store_test.go`.

### Required Store Tests

- [x] `TestStoreCreateHumanRequestWritesWorkspaceScopedPendingFile`
  - Create request with `workspace_id`, `workspace_key`, `run_id`, `agent_id`, `session_id`, `question`, `kind=freeform`.
  - Assert file path lives under `stateRoot/workspaces/<workspace_key>/human_requests/<request_id>.yaml`.
  - Assert status is `pending`.
  - Assert created timestamps are non-zero.
  - Assert the stored request never uses raw `workspace_id` as a path segment.

- [x] `TestStoreCreateHumanRequestRejectsPathTraversalWorkspaceKey`
  - Inputs: `../x`, `/tmp/x`, `a/../b`, empty string, string with path separator.
  - Expected: create fails with validation error; no file is created outside state root.

- [x] `TestStoreCreateHumanRequestIsIdempotentForSameRunAndDedupeKey`
  - Same `run_id`, `tool_call_id`, `question_hash`, and pending status returns the existing request.
  - Different `tool_call_id` creates a distinct request.
  - Completed request does not block a new request with same question later in the same run unless the spec requires stronger dedupe.

- [x] `TestStoreResolveApprovePersistsResponseAndAudit`
  - Resolve pending request with `response_kind=approve`.
  - Assert status becomes `resolved`.
  - Assert `response.actor`, `response.message`, `response.created_at`, and `resolved_at`.
  - Assert audit append records old status, new status, actor, and signal.

- [x] `TestStoreResolveDenyPersistsResponseAndPreventsReplay`
  - Resolve pending request with `response_kind=deny`.
  - Assert replay state is terminal and no suspended tool call is executable.

- [x] `TestStoreResolveCancelPersistsCanceledSignal`
  - Resolve pending request with `response_kind=cancel`.
  - Assert downstream code can distinguish deny from cancel.

- [x] `TestStoreResolveAnswerKeepsAnswerPayload`
  - Resolve with freeform answer.
  - Assert answer text is stored exactly.
  - Assert empty answer is rejected for answer-required request kinds.

- [x] `TestStoreRejectsDoubleResolve`
  - First response succeeds.
  - Second response with any kind returns conflict.
  - Stored response remains the first response.

- [x] `TestStoreListPendingFiltersByWorkspaceAndStatus`
  - Create pending/resolved requests across two workspaces.
  - Assert list only returns requested workspace and status.
  - Assert stable sort: newest first or oldest first, whichever implementation documents.

- [x] `TestStoreShowMissingRequestReturnsNotFound`
  - Missing id returns typed not-found error.
  - Wrong workspace returns not-found, not authorization detail leakage.

- [x] `TestStoreReplayCASPendingRunningCompleted`
  - Action snapshot starts `replay_status=pending`.
  - CAS pending -> running succeeds once.
  - Second runner sees conflict/running.
  - running -> completed stores result digest.
  - completed is idempotent for repeated HTTP retry with same idempotency key.

- [x] `TestStoreReplayLeaseCanRecoverAfterCrash`
  - Mark replay running with old lease timestamp.
  - New runner can reclaim after lease timeout.
  - New runner cannot reclaim before lease timeout.

- [x] `TestStoreLoadCorruptFileReportsErrorButDoesNotPanic`
  - Write invalid YAML/JSON into a request file.
  - Listing returns an error that identifies corrupt request id/path safely.
  - No panic.

- [x] `TestStoreAtomicWriteDoesNotLeavePartialFile`
  - Force write error using a read-only temp directory if possible.
  - Assert no partially decoded request is visible.

### Domain Fields

Implement the minimum domain needed for all later phases:

```go
type HumanRequest struct {
    ID             string
    WorkspaceID    string
    WorkspaceKey   string
    RunID          string
    AgentID        string
    SessionID      string
    Source         string
    Kind           RequestKind
    Status         RequestStatus
    Question       string
    Options        []HumanOption
    ActionSnapshot *ActionSnapshot
    DedupeKey      string
    CreatedAt      time.Time
    ResolvedAt     *time.Time
    Response       *HumanResponse
    Metadata       map[string]any
}
```

The implementation can choose YAML or JSON based on existing state-store conventions. The tests must assert behavior, not formatting.

Completion gate:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/humanrequest
```

Expected:

```text
ok   github.com/xiramesh/xira/internal/humanrequest
```

---

## Phase 2 - Minimal Response API Before Replay Or Child Resume

This phase exists to prevent the old landing-order bug: replay and child resume cannot be validated without a response endpoint.

### Tests First

Add tests in `apps/xira/internal/api/server_test.go`.

- [x] `TestPostHumanRequestResponseApprove`
  - Seed pending request in HITL store.
  - `POST /api/v1/human-requests/{id}/responses` with `{"kind":"approve","actor":"test-user"}`.
  - Expect `200`.
  - Assert response body includes request id, status `resolved`, response kind `approve`.

- [x] `TestPostHumanRequestResponseAnswer`
  - Seed pending freeform request.
  - POST answer text.
  - Assert answer is persisted and returned.

- [x] `TestPostHumanRequestResponseRejectsInvalidKind`
  - POST `{"kind":"maybe"}`.
  - Expect `400`.
  - Store remains pending.

- [x] `TestPostHumanRequestResponseConflictOnResolved`
  - Resolve once.
  - Resolve again.
  - Expect `409`.

- [x] `TestPostHumanRequestResponseMissingRequest`
  - Unknown id returns `404`.

- [x] `TestPostHumanRequestResponseWrongWorkspaceDoesNotLeak`
  - If workspace is inferred from request context, wrong workspace returns `404`.
  - If workspace identity is not yet authenticated in v0, the handler must still resolve workspace from the persisted request and reject caller-supplied workspace overrides.

- [x] `TestPostHumanRequestResponseTriggersResumeHookButDoesNotRequireReplayYet`
  - Install fake resume dispatcher.
  - Resolve request.
  - Assert dispatcher receives request id once.
  - Replay implementation may be a no-op in this phase, but the hook shape must exist.

### Implementation

- [x] Add API route:

  ```text
  POST /api/v1/human-requests/{id}/responses
  ```

- [x] Request JSON:

  ```json
  {
    "kind": "approve",
    "message": "looks good",
    "actor": "user-or-test",
    "idempotency_key": "optional-client-key"
  }
  ```

- [x] Supported `kind` values:
  - `approve`
  - `deny`
  - `cancel`
  - `answer`

- [x] The API must call the store only through a service method, not directly from handler code, if existing API patterns route through runtime/service.

Completion gate:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/api -run 'TestPostHumanRequestResponse'
```

Expected:

```text
ok   github.com/xiramesh/xira/internal/api
```

---

## Phase 3 - RunInterrupt And RuntimeSuspendCollector

This is the critical runtime contract. It must be implemented before `human.request`, `RequireConfirmation` replay, or child waiting propagation can be trusted.

### Tests First

Add tests in `apps/xira/internal/runtime/human_request_interrupt_test.go`.

- [x] `TestRunInterruptSetsWaitingHumanStatus`
  - Force a runtime suspend through a fake collector.
  - Assert `TurnResponse.Status == "waiting_human"`.
  - Assert `TurnResponse.Interrupt.Status == "waiting_human"`.
  - Assert run store has waiting state, not completed/failed.

- [x] `TestRunInterruptIncludesHumanRequestsAndBlockedBy`
  - Create one human request through collector.
  - Assert response contains the request id.
  - Assert `BlockedBy` includes type `human_request`.

- [x] `TestNativePathStopsBeforeSecondModelCallOnInterrupt`
  - Fake model first response emits a tool call that suspends.
  - Assert model call count is exactly 1.
  - Assert no tool output is fed into the second model call.

- [x] `TestADKPathStopsBeforeSecondModelCallOnInterrupt`
  - Use the existing ADK hydration test style.
  - Force the ADK tool loop to suspend.
  - Assert no post-tool model call happens.

- [x] `TestRunInterruptDoesNotValidateFinalResponse`
  - Fake model response has no final response and only a suspending tool call.
  - Assert runtime does not fail final response validation.

- [x] `TestRunInterruptPersistsUsageAndEvents`
  - Assert LLM usage from the first model call remains recorded.
  - Assert event stream includes a waiting/suspended event.

- [ ] v0.1 follow-up scenario: interrupt/error priority when one generator turn returns both a real execution error and a suspend signal.
  - Current v0 observable paths either return a suspend or a normal error, not both from the same generator call.
  - Add the priority test when `generate` exposes a stable `RunInterrupt`/error composite return contract.

### Implementation

- [x] Add `RunInterrupt` and related types to runtime.
- [x] Add `RuntimeSuspendCollector` implementation that is created per run.
- [x] Add `TurnResponse.Interrupt *RunInterrupt`.
- [x] Add `TurnResponse.HumanRequests []humanrequest.HumanRequest` or expose them only through `Interrupt.HumanRequests`; choose one and keep API responses consistent.
- [x] Modify native and ADK execution so that after tool execution:
  - if collector has interrupt, return `waiting_human`;
  - do not continue model loop;
  - do not validate missing final response as failure;
  - persist run state as waiting.

Completion gate:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestRunInterrupt|TestNativePathStops|TestADKPathStops'
```

Expected:

```text
ok   github.com/xiramesh/xira/internal/runtime
```

---

## Phase 4 - Runtime Control Tools: human.request And human.respond

### Tests First

Add tests in `apps/xira/internal/runtime/human_request_interrupt_test.go`.

- [x] `TestHumanRequestToolIsAvailableToNativeProfiles`
  - Inspect tool catalog for a native profile.
  - Assert `human.request` schema exists.

- [x] `TestHumanRequestToolIsAvailableToADKProfiles`
  - Inspect ADK profile hydration.
  - Assert `human.request` schema exists.

- [x] `TestHumanRequestToolCreatesPendingRequestAndInterrupt`
  - Fake model calls `human.request`.
  - Assert one pending request in store.
  - Assert response status `waiting_human`.
  - Assert no second model call.

- [x] `TestHumanRequestToolDedupesRepeatedSameToolCall`
  - Retry same run/tool call.
  - Assert same request id is returned and no duplicate pending request is created.

- [x] `TestHumanRequestToolAllowsMultipleDistinctQuestions`
  - Same run, two different tool call ids or dedupe keys.
  - Assert two pending requests when the model explicitly asks two independent questions.

- [x] `TestHumanRespondToolRequiresTrustedRuntimeContext`
  - Direct model attempt to call `human.respond` without trusted runtime context is rejected.
  - This prevents the model from self-approving its own request.

- [x] `TestHumanRespondToolCanResolveInTrustedResumeContext`
  - Runtime resume path invokes `human.respond`.
  - Assert response is persisted.

- [x] `TestHumanRequestToolRejectsInvalidOptions`
  - Empty question rejected.
  - Options with duplicate ids rejected.
  - Oversized question rejected if the project has max payload conventions.

### Implementation

- [x] Add `human.request` as a runtime-owned tool.
- [x] Add `human.respond` as a runtime-owned tool but only executable from trusted runtime resume context.
- [x] Ensure tool results for `human.request` are not sent back into model loop in the same run; they become `RunInterrupt`.
- [x] Store request provenance:
  - run id
  - agent id
  - session id
  - tool call id
  - model call id if available
  - workspace id and workspace key

Completion gate:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestHumanRequestTool|TestHumanRespondTool'
```

---

## Phase 5 - Runtime Tool Confirmation Snapshot And Replay

This phase implements `RequireConfirmation` using the same `HumanRequest` response API.

### Tests First

Add tests in `apps/xira/internal/runtime/human_request_replay_test.go`.

- [x] `TestRequireConfirmationCreatesActionSnapshot`
  - Configure a runtime tool requiring confirmation.
  - Fake model calls the tool.
  - Assert human request contains action snapshot:
    - tool name
    - normalized args
    - run id
    - agent id
    - session id
    - env hash or execution context hash if implemented
    - allowlist/policy decision id if available

- [x] `TestRequireConfirmationReturnsWaitingHuman`
  - Assert run status `waiting_human`.
  - Assert tool is not executed before approval.

- [x] `TestApproveReplaysSnapshotExactlyOnce`
  - Approve via response API/service.
  - Replay executes tool once.
  - Repeated approve or repeated dispatcher event does not execute again.

- [x] `TestReplayBypassesOnlyConfirmationGate`
  - On replay, confirmation requirement is bypassed.
  - Allowlist, path restrictions, timeout, workspace, and audit checks still run.

- [x] `TestReplayRejectsChangedToolArgs`
  - Tamper stored snapshot args.
  - Replay fails with validation/audit error.
  - v0 stores and validates snapshot `context_hash` for normalized tool args. Strict env/workspace policy hash enforcement remains a v0.1 follow-up because the current runtime has no stable env hash primitive.

- [x] `TestDenyDoesNotReplaySnapshot`
  - Resolve with deny.
  - Assert tool execution count is zero.
  - Assert run reaches failed or waiting terminal transition according to runtime design.

- [x] `TestCancelDoesNotReplayAndMaterializesCanceledOutput`
  - Resolve with cancel.
  - Assert suspended tool call materializes cancellation output.

- [x] `TestReplayRunningLeasePreventsConcurrentExecution`
  - Two goroutines try to replay same approved request.
  - Assert exactly one tool execution.
  - Other goroutine receives running/conflict/idempotent completed response.

- [x] `TestReplayLeaseRecoveryAfterCrash`
  - Mark replay running and stale.
  - Resume again.
  - Assert execution can be reclaimed once.

- [x] `TestReplayRecordsAuditTrail`
  - Assert approval actor, replay start, replay completion, and final tool result digest are audit-visible.

### Implementation

- [x] Extend `ActionSnapshot` to include enough data for deterministic replay.
- [x] Add service method similar to:

  ```go
  ResolveHumanRequest(ctx context.Context, requestID string, response humanrequest.HumanResponseInput) (*humanrequest.HumanRequest, error)
  ```

- [x] Add internal replay dispatcher entry point.
- [x] Make replay synchronous for v0 response API unless architecture doc says otherwise.
- [x] Do not let replay call the model until the suspended tool call has been materialized into the run state.

Completion gate:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestRequireConfirmation|TestReplay|TestDeny|TestCancel'
```

---

## Phase 6 - Delegation Completed Path Hardening

This phase protects the child result trust boundary before adding child suspend.

### Tests First

Add tests in `apps/xira/internal/runtime/delegation_suspend_test.go` or existing delegation tests.

- [x] `TestDelegateCompletedResultAccepted`
  - Fake child returns valid final delegate result.
  - Parent receives materialized `delegate_agent` output.

- [x] `TestDelegateRejectsChildResultWithRuntimeFields`
  - Child final response tries to set parent run id, tool ids, workspace paths, or privileged fields.
  - Runtime strips/rejects them.

- [x] `TestDelegateRejectsInvalidEvidenceRefs`
  - Child references files/session ids outside scoped context packet.
  - Runtime rejects result.

- [x] `TestDelegateDoesNotExposeParentTranscriptToChild`
  - Context packet includes only scoped summary/refs.
  - Child cannot see full parent transcript unless explicitly included by policy.

- [x] `TestDelegateOutputEnvelopeHidesInternalChildState`
  - Parent sees status/result/error/evidence envelope.
  - Parent does not get raw child system prompt, raw model trace, or hidden runtime metadata.

- [ ] v0.1 follow-up scenario: delegate join-all completed children preserve deterministic order.
  - Multiple simultaneous child delegates require a batched tool-call execution model. Current v0 short-circuits on the first runtime interrupt, so the implemented join state is a single-child `delegate_agent` suspension record.

- [ ] v0.1 follow-up scenario: delegate join-all deny/cancel closes or cancels remaining children.
  - This requires the same multi-child join state as above. v0 validates single-child deny/cancel materialization and parent continuation.

### Implementation

- [x] Keep `RunChildAgent` as a runtime boundary.
- [x] Make result validation explicit and reusable by completed and resumed child paths.
- [x] Introduce persisted delegation join state before implementing child waiting:
  - parent run id
  - parent tool call id
  - child run ids
  - join mode
  - child statuses
  - materialized outputs

Completion gate:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestDelegate.*Completed|TestDelegateRejects|TestDelegateOutputEnvelope'
```

---

## Phase 7 - Child waiting_human Propagation And Parent Resume

This is the biggest v0 runtime change.

### Tests First

Add tests in `apps/xira/internal/runtime/delegation_suspend_test.go`.

- [x] `TestDelegateChildWaitingHumanSuspendsParent`
  - Parent model calls `delegate_agent`.
  - Fake child run returns `Status=waiting_human` with one `HumanRequest`.
  - Parent response status is `waiting_human`.
  - Parent interrupt contains:
    - child human request
    - blocked_by type `child_run`
    - suspended parent `delegate_agent` tool call
    - delegation join id
  - Parent does not call model again.

- [x] `TestDelegateChildWaitingHumanPersistsJoinState`
  - Assert persisted join state contains parent run id, child run id, child request id, and suspended tool call id.

- [x] `TestDelegateChildWaitingHumanReleasesActiveSlot`
  - Configure `max_parallel=1`.
  - Child suspends.
  - Assert active slot count returns to zero.
  - Assert outstanding suspended count is one.

- [x] `TestDelegateChildWaitingHumanCountsAgainstMaxOutstanding`
  - Configure `max_outstanding=1`.
  - First child suspends.
  - Second delegation attempt returns queue/reject/resume_pending according to final implementation decision.

- [x] `TestDelegateResumeAfterChildApprovedMaterializesOutput`
  - Resolve child request with approve/answer.
  - Resume child run to completed result.
  - Parent suspended `delegate_agent` output is materialized.
  - Parent resumes from the suspended point and may continue model loop.

- [x] `TestDelegateResumeAfterChildAnswerInjectsOnlyChildOutput`
  - Human answer goes to child run.
  - Parent sees only validated child delegate output, not raw human response unless child result includes it.

- [x] `TestDelegateResumeDenyMaterializesFailedOutput`
  - Resolve child request with deny.
  - Child run becomes failed with deny reason.
  - Parent `delegate_agent` output status is failed.
  - Parent join=all exits according to doc.

- [x] `TestDelegateResumeCancelMaterializesCanceledOutput`
  - Resolve child request with cancel.
  - Child run status is failed with `error_type=canceled`.
  - Parent delegate output status is `canceled`.
  - Run status is not `canceled`.

- [ ] v0.1 follow-up scenario: delegate join-all with mixed completed and waiting children.
  - Child A completed, child B waiting.
  - Parent waits.
  - After B resolves, parent receives A and B outputs once each.

- [ ] v0.1 follow-up scenario: delegate join-any does not block when another child completes.
  - v0 does not expose join-any. Add validation rejection or implementation together with multi-child join support.

- [x] `TestDelegateResumeIsIdempotent`
  - Duplicate response dispatcher events do not materialize duplicate parent tool outputs.

- [x] `TestDelegateResumeAfterProcessRestart`
  - Recreate service with same state root.
  - Resolve child request.
  - Parent join state is loaded and resumed.

- [ ] v0.1 follow-up scenario: suspended child timeout excludes human wait.
  - v0 releases active `max_parallel` slots while suspended and keeps suspended children in `max_outstanding`.
  - A separate persisted active-execution timer is required before this can be tested precisely across process restarts.

- [ ] v0.1 follow-up scenario: active execution timeout still applies after child resume.
  - Resume currently uses the normal model call path without a persisted remaining-time budget. Add this with the active-execution timer follow-up.

### Implementation

- [x] Split child slot accounting:
  - active execution slots for `max_parallel`
  - persisted outstanding child runs for `max_outstanding`

- [x] Convert synchronous `RunChildAgent` result handling:
  - completed child -> validate result -> materialize output
  - failed child -> materialize failed output
  - waiting child -> persist join state -> return parent `RunInterrupt`

- [x] Add parent resume entry point:

  ```go
  ResumeRunAfterHumanResponse(ctx context.Context, requestID string) error
  ```

  It should locate whether the request belongs to direct run HITL, action replay, or child delegation.

- [x] Ensure native and ADK parent paths use the same resume machinery.

Completion gate:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestDelegate.*Waiting|TestDelegateResume|TestDelegateJoin|TestDelegate.*Slot|TestDelegate.*Timeout'
```

---

## Phase 8 - API List/Show And CLI

### API Tests First

Add tests in `apps/xira/internal/api/server_test.go`.

- [x] `TestListHumanRequests`
  - Seed pending/resolved requests.
  - `GET /api/v1/human-requests?status=pending`.
  - Assert filter and stable sort.

- [x] `TestShowHumanRequest`
  - `GET /api/v1/human-requests/{id}`.
  - Assert full request detail includes response/snapshot only when present.

- [x] `TestShowHumanRequestMissing`
  - Missing returns `404`.

- [x] `TestListHumanRequestsRejectsInvalidStatus`
  - Invalid status returns `400`.

### CLI Tests First

Add tests under `apps/xira/cmd/xira`, matching existing command test conventions.

- [x] `TestHumanListCommandPrintsPendingRequests`
- [x] `TestHumanShowCommandPrintsRequestDetail`
- [x] `TestHumanApproveCommandPostsResponse`
- [x] `TestHumanDenyCommandPostsResponse`
- [x] `TestHumanCancelCommandPostsResponse`
- [x] `TestHumanAnswerCommandRequiresMessage`
- [x] `TestHumanCommandsReturnNonZeroOnAPIError`

### Implementation

- [x] Add API:
  - `GET /api/v1/human-requests`
  - `GET /api/v1/human-requests/{id}`
  - `POST /api/v1/human-requests/{id}/responses`

- [x] Add CLI:
  - `xira human list --status pending`
  - `xira human show <id>`
  - `xira human approve <id> --message "..."`
  - `xira human deny <id> --message "..."`
  - `xira human cancel <id> --message "..."`
  - `xira human answer <id> --message "..."`

Completion gate:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/api ./apps/xira/cmd/xira
```

---

## Phase 9 - Deterministic End-To-End Scenarios With Fake DeepSeek

These tests should run on every machine and in CI. They are more important than live DeepSeek smoke tests.

Add scenario tests under `apps/xira/internal/runtime` or a dedicated integration package if existing conventions support it.

- [x] `TestE2EDirectHumanRequestApproveAndResume`
  - Model calls `human.request`.
  - Runtime returns `waiting_human`.
  - API resolves with answer.
  - Runtime resumes.
  - Final response completed.

- [x] `TestE2EDirectHumanRequestDeny`
  - Deny direct request.
  - Runtime does not ask model to continue as if approved.
  - Terminal behavior matches spec.

- [x] `TestE2ERuntimeToolConfirmationApproveReplay`
  - Model calls protected tool.
  - Runtime snapshots and waits.
  - Approve response replays exactly once.
  - Final model call sees tool output and completes.

- [x] `TestE2ERuntimeToolConfirmationDeny`
  - Deny response materializes denied tool output.
  - Model continuation sees denial if spec allows continuation, or run fails if spec chooses fail-fast.

- [x] `TestE2EDelegateCompleted`
  - Parent delegates to child.
  - Child completes.
  - Parent completes with child result.

- [x] `TestE2EDelegateChildWaitingApproveResumeParent`
  - Parent delegates.
  - Child asks human.
  - Parent waits.
  - Human answers child.
  - Child completes.
  - Parent resumes and completes.

- [x] `TestE2EDelegateChildWaitingCancel`
  - Parent delegates.
  - Child waits.
  - Human cancels.
  - Parent sees canceled delegate output.

- [ ] v0.1 follow-up E2E scenario: multiple children with one waiting and one completed.
  - Join state remains stable across mixed states.
  - Parent resumes once all required children are terminal.

- [x] `TestE2EProcessRestartBeforeHumanResponse`
  - Run reaches waiting.
  - Create new service with same state root.
  - Resolve request.
  - Resume succeeds.

- [ ] v0.1 follow-up E2E scenario: process restart during replay running lease.
  - Mark replay running then restart.
  - Lease recovery executes exactly once through the runtime dispatcher.
  - Store-level stale lease recovery is covered in v0 by `TestReplayLeaseRecoveryAfterCrash`.

- [x] `TestE2EModelRetryDoesNotDuplicateHumanRequest`
  - Same model/tool call replayed after transient error.
  - One pending request only.

- [x] `TestE2EWorkspaceIsolation`
  - Two workspaces use same request id shape or same run id.
  - List/show/resolve cannot cross workspace boundaries.

Completion gate:

```bash
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestE2E'
```

Expected:

```text
ok   github.com/xiramesh/xira/internal/runtime
```

---

## Phase 10 - Real DeepSeek Smoke Tests

Live DeepSeek tests are not correctness proof. They verify that real model responses, tool-call serialization, and runtime-owned tools work together. They must be sparse, env-gated, and structurally asserted.

### Test Gating

Every live test must begin with:

```go
if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
    t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek HITL smoke tests")
}
if os.Getenv("DEEPSEEK_API_KEY") == "" {
    t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek HITL smoke tests")
}
```

Recommended command:

```bash
XIRA_DEEPSEEK_LIVE=1 \
DEEPSEEK_API_KEY="$DEEPSEEK_API_KEY" \
XIRA_DEEPSEEK_MODEL=deepseek-v4-flash \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run 'TestRealDeepSeekHITL' -v
```

### Live Test Rules

- [x] Do not assert exact prose.
- [x] Assert tool-call structure, status, persisted requests, and audit events.
- [x] Use low temperature if the client exposes it.
- [x] Use prompts that require one explicit tool call and a short deterministic argument.
- [x] Fail with clear diagnostics that include tool names and statuses, but never print `DEEPSEEK_API_KEY`.
- [x] Mark tests skipped by default, not passing fake.
- [x] Keep live tests outside normal CI unless CI has explicit secret opt-in.

### Required Live Tests

- [x] `TestRealDeepSeekHITLHumanRequestTool`
  - System prompt instructs: "Call `human.request` exactly once. Ask: `Approve shipping HITL v0 smoke test?`"
  - User prompt asks the agent to pause for approval.
  - Expected:
    - DeepSeek returns a `human.request` tool call or runtime reaches a clear unsupported-model diagnostic.
    - Runtime creates one pending HumanRequest.
    - Run status is `waiting_human`.
    - Model call count is one for the waiting run.

- [x] `TestRealDeepSeekHITLRequireConfirmationSnapshot`
  - Expose a harmless test tool requiring confirmation, for example `test.echo_confirmed`.
  - Prompt tells model to call it with `{"message":"hitl-live-smoke"}`.
  - Expected:
    - Runtime creates pending request with action snapshot.
    - Tool execution count before approval is zero.
    - Request can be approved through service response.
    - Replay executes test tool exactly once.

- [x] `TestRealDeepSeekHITLRespondsAfterApprovedToolOutput`
  - Continue from approved replay or run a separate scenario.
  - Expected:
    - Model receives materialized tool output after approval.
    - Final response completes.
    - Assertion checks only that final response is non-empty and run status completed.

- [x] `TestRealDeepSeekHITLDelegateCompleted`
  - Parent prompt instructs a single `delegate_agent` call to a test child agent.
  - Child prompt is deterministic and returns a valid delegate result.
  - Expected:
    - Parent completes with delegate output.
    - Result validates through trust boundary.

- [x] `TestRealDeepSeekHITLDelegateChildWaiting`
  - Parent delegates to child.
  - Child is configured/prompted to call `human.request`.
  - Expected:
    - Parent status is `waiting_human`.
    - Parent interrupt includes child request id and delegation join id.
    - No parent second model call before child response.

### Optional Live Matrix If DeepSeek Budget Is Available

- [x] Direct human answer path in Chinese prompt.
- [x] Direct human answer path in English prompt.
- [x] Tool confirmation approve path.
- [x] Tool confirmation deny path.
- [x] Child waiting approve path.
- [x] Child waiting cancel path.
- [x] Model retry with same tool call id if client exposes retry hooks.

Completion gate:

```bash
	  XIRA_DEEPSEEK_LIVE=1 XIRA_DEEPSEEK_MODEL=deepseek-v4-pro DEEPSEEK_API_KEY="$DEEPSEEK_API_KEY" GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestRealDeepSeekHITL' -v
```

Expected:

```text
--- PASS: TestRealDeepSeekHITLHumanRequestTool
--- PASS: TestRealDeepSeekHITLRequireConfirmationSnapshot
--- PASS: TestRealDeepSeekHITLRespondsAfterApprovedToolOutput
--- PASS: TestRealDeepSeekHITLDelegateCompleted
--- PASS: TestRealDeepSeekHITLDelegateChildWaiting
PASS
ok   github.com/xiramesh/xira/internal/runtime
```

If DeepSeek does not reliably emit the requested tool call, keep the failure as a live-model compatibility finding and do not weaken deterministic fake-model tests.

---

## Phase 11 - Full Regression Gate

Run these after all focused phases pass.

- [x] Formatting and static diff hygiene:

  ```bash
  gofmt -w apps/xira/internal/humanrequest apps/xira/internal/runtime apps/xira/internal/api apps/xira/cmd/xira
  git diff --check
  ```

- [x] HITL and runtime packages:

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/humanrequest ./apps/xira/internal/runtime
  ```

- [x] API and CLI:

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/api ./apps/xira/cmd/xira
  ```

- [x] All Xira Go tests:

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/...
  ```

- [x] Repository-level Go tests if package graph allows:

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./...
  ```

- [ ] Frontend/admin build only if touched:

  ```bash
  pnpm --filter xiragarden build
  ```

- [ ] Project build gate if currently used by repo:

  ```bash
  task build
  ```

- [x] Optional live DeepSeek smoke:

  ```bash
  XIRA_DEEPSEEK_LIVE=1 DEEPSEEK_API_KEY="$DEEPSEEK_API_KEY" GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run 'TestRealDeepSeekHITL' -v
  ```

Final acceptance requires:

- [x] deterministic tests pass without `DEEPSEEK_API_KEY`;
- [x] live tests skip cleanly when env is absent;
- [x] live tests pass when explicitly enabled and the key is available;
- [x] no test relies on exact LLM prose;
- [x] no approval/replay path can execute a protected tool twice;
- [x] no child `waiting_human` path continues parent model execution before human resolution;
- [x] canceled child never creates run status `canceled`;
- [x] active slot and outstanding child counts are separately tested;
- [x] response API is present before replay/resume tests rely on it.

---

## Edge Case Checklist

Use this as a final audit before requesting review.

- [x] Pending request with missing workspace.
- [x] Pending request with invalid workspace key.
- [x] Duplicate pending request from same tool call.
- [x] Two distinct pending requests in one run.
- [x] Resolve unknown request.
- [x] Resolve request from wrong workspace.
- [x] Resolve already resolved request.
- [x] Approve request without action snapshot.
- [x] Deny request with action snapshot.
- [x] Cancel child request.
- [x] Answer required but empty answer.
- [x] Human request option definitions with duplicate or invalid option ids.
- [x] Corrupt request file.
- [x] Partial write during request create.
- [x] Process restart before response.
- [ ] v0.1 follow-up: process restart during replay running lease at runtime-dispatcher level.
- [x] Two concurrent approvals.
- [x] Two concurrent replay dispatches.
- [ ] v0.1 follow-up: replay after strict env/workspace policy hash mismatch.
- [ ] v0.1 follow-up: replay after tool policy changed to disallow.
- [ ] v0.1 follow-up: replay after persisted active-execution timeout budget expires.
- [x] Parent run waiting on child request.
- [ ] v0.1 follow-up: parent run waiting on two child requests.
- [ ] v0.1 follow-up: child completed while sibling waiting.
- [ ] v0.1 follow-up: child denied while sibling running.
- [ ] v0.1 follow-up: child canceled while sibling waiting.
- [x] Suspended child releases active `max_parallel`.
- [x] Suspended child still counts `max_outstanding`.
- [x] Service restart reconstructs outstanding count.
- [x] Native path suspend.
- [x] ADK path suspend.
- [x] No second model call after suspend.
- [x] No final response validation after suspend.
- [x] Model cannot call `human.respond` to self-approve.
- [ ] v0.1 follow-up: CLI approve idempotency across transport/network retry.
- [x] CLI answer requires message.
- [x] API returns stable machine-readable errors.
- [x] Live DeepSeek test skips without env.
- [x] Live DeepSeek test never logs API key.

---

## Review Handoff Notes

When the implementation is complete, include in the PR description:

- [x] The 11-step spec order from `xira-agent-hitl-v0.zh.md` and which commits map to each step.
- [x] The fake-model test matrix and exact commands run.
- [x] The live DeepSeek command, whether it was skipped or passed, and the model name used.
- [x] Any intentional v0 omissions.
- [x] Any migration note for existing state roots.
- [x] Any known non-determinism in live model smoke tests.
