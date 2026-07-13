# HumanRequest Interaction Foundation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add channel-neutral responder/correlation/delivery metadata and a crash-safe, idempotent HumanRequest resume lifecycle shared by Agent Run and Flow.

**Architecture:** Extend the existing file-backed HumanRequest record instead of adding a second queue. Store the human response and `resume_pending` atomically, claim and persist resume transitions around the existing Agent/Flow resume functions, and reconcile interrupted work after startup. Platform rendering and input parsing remain follow-up adapter issues.

**Tech Stack:** Go 1.26.5, file-backed atomic JSON stores, existing Xira RunStore/Flow Store, Google ADK/DeepSeek tests.

---

### Task 1: Add responder, correlation, delivery, and resume domain types

**Files:**
- Modify: `apps/xira/internal/humanrequest/types.go`
- Modify: `apps/xira/internal/humanrequest/store.go`
- Modify: `apps/xira/internal/humanrequest/store_test.go`

**Steps:**

1. Write failing tests that create current-sender and owner-bound requests and assert persisted responder, generated correlation, expiry, delivery, and `waiting_response` resume state.
2. Write a compatibility test that loads a pre-#163 JSON record with empty new fields and does not classify it as resumable.
3. Run `go test ./apps/xira/internal/humanrequest -run 'Responder|Correlation|Legacy|Delivery|ResumeState' -count=1` from `apps/xira` and confirm RED.
4. Add sealed enums and records for responder, delivery, and resume state; add `ExpiresAt` and correlation to create input and persisted output.
5. Default newly created requests to `current_sender`, generate correlation with `uuid`, and initialize `resume_waiting_response`; preserve zero-value legacy decoding.
6. Mark validation/state functions `coverage: contract (100% required)` and cover every branch.
7. Rerun the focused tests and confirm GREEN.

### Task 2: Make response persistence exact and idempotent

**Files:**
- Modify: `apps/xira/internal/humanrequest/types.go`
- Modify: `apps/xira/internal/humanrequest/store.go`
- Modify: `apps/xira/internal/humanrequest/store_test.go`

**Steps:**

1. Write failing tests for exact response success and wrong request correlation, responder, sender type, entrypoint, delivery message ID, and expiry.
2. Write failing tests for same-key/same-payload idempotency and same-key/different-payload conflict.
3. Run the exact tests and confirm RED.
4. Add `HumanResponseEnvelope` and an atomic exact-resolve path that validates persisted authority inside the store lock.
5. Refactor legacy `Resolve` through one response writer; on new tracked requests atomically change resume state from `waiting_response` to `pending`.
6. Rerun tests and confirm GREEN.

### Task 3: Add delivery and resume transition stores

**Files:**
- Modify: `apps/xira/internal/humanrequest/store.go`
- Modify: `apps/xira/internal/humanrequest/store_test.go`

**Steps:**

1. Write failing table tests for legal/illegal delivery and resume transitions.
2. Write failing tests for atomic resume claim, concurrent duplicate claim, failure retry, completion, resumable listing, and stale-running recovery.
3. Run the transition tests and confirm RED.
4. Implement store methods to record delivery attempts/receipts, claim resume, complete/fail resume, list resumable records, and recover interrupted `running` records.
5. Keep legacy empty resume state out of reconciliation.
6. Rerun tests including `-race` and confirm GREEN.

### Task 4: Route runtime resolution through durable resume state

**Files:**
- Modify: `apps/xira/internal/runtime/human_requests.go`
- Modify: `apps/xira/internal/runtime/runtime.go`
- Modify: `apps/xira/internal/runtime/human_request_e2e_test.go`
- Create: `apps/xira/internal/runtime/human_request_reconcile_test.go`

**Steps:**

1. Write failing E2E tests proving resolution persists `resume_completed` for Agent Run and Flow.
2. Write a failing crash-window test by storing a response as `resume_pending`, reconstructing Service on the same state dir, calling reconciliation, and asserting the original run completes.
3. Write failure/retry tests where the first resume attempt fails and a later reconciliation succeeds.
4. Run focused runtime tests and confirm RED.
5. Refactor `ResolveHumanRequest` to resolve, claim, dispatch by source, and persist completed/failed state.
6. Add exact `ResolveHumanResponse` and `ReconcileHumanRequests` methods; runtime performs owner `IsOwner` revalidation before exact store mutation.
7. Return refreshed persisted HumanRequest state to callers.
8. Rerun focused tests and confirm GREEN.

### Task 5: Bind current responder identity for Agent Run and Flow

**Files:**
- Modify: `apps/xira/internal/runtime/human_requests.go`
- Modify: `apps/xira/internal/runtime/flow_bridge.go`
- Modify: `apps/xira/internal/runtime/human_request_interrupt_test.go`
- Modify: `apps/xira/internal/runtime/flow_bridge_test.go`

**Steps:**

1. Write failing tests using real transformed inbound context, including typed sender and exact entrypoint.
2. Run the focused tests and confirm RED.
3. Add one runtime helper that creates a current-sender responder policy from authoritative `InboundContext`; use it in both Agent and Flow creation paths.
4. Do not expose owner target to the model until #164 adds deliverability preflight.
5. Rerun tests and confirm GREEN.

### Task 6: Wire startup recovery and document the contract

**Files:**
- Modify: `apps/xira/internal/runtime/service.go`
- Modify: `apps/xira/cmd/xira/main.go`
- Modify: `apps/xira/internal/runtime/startup_pending_scan_test.go`
- Modify: `apps/xira/cmd/xira/main_test.go`
- Modify: `docs/architecture/xira-agent-hitl-v0.zh.md`

**Steps:**

1. Write failing tests that stale `resume_running` is recovered at startup and reconciliation is invoked only after channel dependencies are installed.
2. Run focused tests and confirm RED.
3. Recover stale running records synchronously during Service construction; after channel Manager starts, run best-effort reconciliation with structured logs.
4. Document the new record/state contract and the #164-#166 adapter boundary.
5. Rerun focused tests and confirm GREEN.

### Task 7: Coverage, full verification, and delivery

**Files:**
- Modify: PR description after creation

**Steps:**

1. Run `gofmt` on changed Go files and `git diff --check`.
2. Run `go test -coverprofile` for `humanrequest` and `runtime`; verify package coverage is at least 85% and every new contract function is 100%.
3. Run `go build ./...`, `go test ./... -count=1`, and `go test -race ./... -count=1` from `apps/xira`.
4. Run `task live-test` from repository root and confirm no live-gated SKIP.
5. Commit in coherent TDD slices, verify `git show --stat HEAD`, push `codex/163-humanrequest-interaction`, and open a PR closing #163 and linking #155.
6. Include package coverage and uncovered core/contract functions in the PR description.
