# WebSocket Structured HumanResponse Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add an exact, connection-authorized WebSocket `human_response` frame for current-sender Agent Run and Flow HumanRequests.

**Architecture:** Load the persisted HumanRequest, bind it to the physical connection that currently owns its persisted ChatKey, reject owner or typed identities, and call the existing async exact resolver. Remove the legacy newest-pending text preflight after the structured path is live.

**Tech Stack:** Go 1.26.5, coder/websocket loopback tests, shared HumanRequest store/runtime state machine, SHA-256 idempotency.

---

### Task 1: Pin the structured frame and connection authority contract

**Files:**
- Modify: `apps/xira/internal/channelrunner/websocket/handle_connection_test.go`
- Modify: `apps/xira/internal/channelrunner/websocket/emit_test.go`

**Steps:**

1. Add a recording structured resolver implementing request lookup plus async exact resolution.
2. Add failing real-loopback tests for an accepted approval and stable duplicate retry.
3. Add failing tests for missing fields, wrong token, unknown request, owner responder, typed persisted responder, mismatched persisted ChatKey/sender, and a connection that does not own the request ChatKey.
4. Change the capability test to require `CapabilityInteractiveHumanResponse`.
5. Run `go test ./internal/channelrunner/websocket -run 'HumanResponse|Capabilities' -count=1` from `apps/xira` and confirm RED.

### Task 2: Implement the sealed WebSocket adapter

**Files:**
- Modify: `apps/xira/internal/runtime/runtime.go`
- Modify: `apps/xira/internal/channelrunner/websocket/runner.go`
- Modify: `apps/xira/internal/channelrunner/manager.go`
- Modify: `apps/xira/internal/channelrunner/manager_test.go`
- Modify: `apps/xira/cmd/xira/main.go`

**Steps:**

1. Add a small `StructuredHITLResolver` interface composed of request lookup and async exact resolution; assert `Service` implements it.
2. Inject that resolver only into WebSocket runners.
3. Extend the inbound frame with correlation/action/answer but no identity fields.
4. Implement contract helpers for persisted current-sender authority, current connection ownership, response-kind normalization, and stable idempotency.
5. Handle `human_response` synchronously through validation/commit, return structured ack/error, and let the shared async resolver resume.
6. Advertise the frame in both `ready.capabilities` and channel capabilities.
7. Run the focused tests and confirm GREEN, including `-race`.

### Task 3: Remove implicit newest-pending resolution

**Files:**
- Modify: `apps/xira/internal/channelrunner/websocket/runner.go`
- Modify: `apps/xira/internal/channelrunner/manager.go`
- Modify: `apps/xira/cmd/xira/main.go`
- Modify: `apps/xira/internal/runtime/runtime.go`
- Delete: `apps/xira/internal/channelrunner/progress/hitl_preflight.go`
- Delete: `apps/xira/internal/channelrunner/progress/hitl_preflight_test.go`
- Delete: `apps/xira/internal/channelrunner/progress/hitl_classify.go`
- Delete: `apps/xira/internal/channelrunner/progress/hitl_classify_test.go`

**Steps:**

1. Add or update a test proving a plain `approve` message enters the normal Agent Turn instead of resolving pending HITL.
2. Remove WebSocket's legacy resolver field, setter, manager wiring, and message preflight.
3. Delete the now-unreferenced progress helpers and legacy runtime interface.
4. Run WebSocket, manager, progress, and runtime tests and confirm GREEN.

### Task 4: Update the public protocol documentation

**Files:**
- Modify: `docs/architecture/xira-websocket-channel-v0.zh.md`
- Modify: `docs/architecture/xira-channel-adapter-mapping-v0.zh.md`
- Modify: `docs/architecture/xira-agent-hitl-v0.zh.md`

**Steps:**

1. Replace the reserved `human_response` section with the exact frame and ack/error contract.
2. Document connection-owned ChatKey authorization, current-sender-only scope, owner fail-closed behavior, and async resume ordering.
3. Remove stale statements that WebSocket response is unimplemented.
4. Run `rg` across docs and code to prove no production claim still says the frame is reserved or rejected.

### Task 5: Verification and delivery

**Files:**
- Modify: PR description after creation.

**Steps:**

1. Run `gofmt` and `git diff --check`.
2. Run WebSocket and affected package coverage; require package coverage >=85% and every new contract helper 100%.
3. Run `go build ./...`, `go test ./... -count=1`, and `go test -race ./... -count=1` from `apps/xira`.
4. Run `task live-test` from the repository root and confirm no live-gated SKIP.
5. Commit in coherent TDD slices, verify each commit's actual file set, push `codex/166-websocket-human-response`, and open a ready PR closing #166 and linking #155.
6. List package coverage plus every uncovered core/contract function in the PR body.
