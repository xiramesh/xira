# HumanRequest Explicit Text Protocol and iLink Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace iLink's ambiguous first-pending HITL replies with a precise explicit text protocol supporting current-sender and owner-private HumanRequests for Agent Run and Flow.

**Architecture:** Keep parsing/rendering in `humanrequest`, reference lookup and authorization in runtime, and transport details in the iLink runner. Add a receipt-returning HumanRequest delivery port routed by `channelrunner.Manager`; all responses converge on the existing exact resolver and durable resume state machine.

**Tech Stack:** Go 1.26.5, file-backed HumanRequest store, existing channelrunner Manager/iLink SDK, Agent Run and Flow runtime, real DeepSeek live tests.

---

### Task 1: Add the shared explicit text protocol

**Files:**
- Create: `apps/xira/internal/humanrequest/text_protocol.go`
- Create: `apps/xira/internal/humanrequest/text_protocol_test.go`

**Steps:**

1. Write table tests for full UUID reference formatting, malformed/short references, non-command chat, malformed `/answer`, freeform payloads, and option label-to-ID normalization.
2. Run `go test ./apps/xira/internal/humanrequest -run 'TextReference|TextResponse|TextAnswer' -count=1`; expect compile/test failure.
3. Implement `TextReference`, `RenderTextRequest`, `ParseTextResponse`, and `NormalizeTextAnswer` as sealed contract functions.
4. Rerun focused tests; expect PASS and 100% for marked contract functions.
5. Commit `feat: add explicit human request text protocol`.

### Task 2: Resolve text references through the exact runtime contract

**Files:**
- Modify: `apps/xira/internal/humanrequest/types.go`
- Modify: `apps/xira/internal/humanrequest/store.go`
- Modify: `apps/xira/internal/humanrequest/interaction_resolve_test.go`
- Modify: `apps/xira/internal/runtime/runtime.go`
- Modify: `apps/xira/internal/runtime/human_requests.go`
- Create: `apps/xira/internal/runtime/human_text_response_test.go`

**Steps:**

1. Write failing Store tests for unique correlation lookup, missing token, duplicate token, and resolved-record lookup for idempotent retry.
2. Write failing runtime tests for exact current-sender chat, wrong chat/sender/type/entrypoint, owner response, option normalization, expiry, and idempotent retry.
3. Run the focused Store/runtime tests; expect RED.
4. Add `TextResponseEnvelope`, `FindByCorrelation`, `ResolveHumanTextResponse`, and a sealed current-sender chat check.
5. Map the parsed reference to the existing `HumanResponseEnvelope`; do not add a second resolve state machine.
6. Rerun focused tests and coverage; expect GREEN and contract functions at 100%.
7. Commit `feat: resolve explicit human responses exactly`.

### Task 3: Add a receipt-returning HumanRequest delivery port

**Files:**
- Modify: `apps/xira/internal/runtime/runtime.go`
- Modify: `apps/xira/internal/channelrunner/manager.go`
- Modify: `apps/xira/internal/channelrunner/manager_emit_test.go`
- Modify: `apps/xira/internal/channelrunner/ilink/runner.go`
- Modify: `apps/xira/internal/channelrunner/ilink/emit_test.go`

**Steps:**

1. Write failing Manager tests for exact entrypoint routing, missing delivery implementation, typed-recipient route validation, and receipt propagation.
2. Write failing iLink tests proving origin delivery uses `SendText`, owner delivery uses `Push`, rendered content contains the exact reference, and the SDK message ID is returned.
3. Run focused channelrunner tests; expect RED.
4. Add `HumanRequestDeliveryTarget`, `HumanRequestDeliveryReceipt`, and `HumanRequestDeliverer`.
5. Implement route-local Manager validation/delivery and iLink rendering/receipt behavior; advertise `CapabilityInteractiveHumanResponse` only after the path works.
6. Rerun tests; expect GREEN.
7. Commit `feat: deliver human requests through ilink`.

### Task 4: Bind responder choice and persist delivery outcome

**Files:**
- Modify: `apps/xira/internal/runtime/delegation.go`
- Modify: `apps/xira/internal/runtime/human_requests.go`
- Modify: `apps/xira/internal/runtime/human_request_interrupt_test.go`
- Modify: `apps/xira/internal/runtime/flow_bridge.go`
- Modify: `apps/xira/internal/flow/types.go`
- Modify: `apps/xira/internal/flow/executor.go`
- Modify: `apps/xira/internal/flow/executor_approval.go`
- Modify: `apps/xira/internal/flow/definition.go`
- Modify: `apps/xira/internal/flow/human_request_test.go`
- Modify: `docs/schemas/xira-flow-v0.schema.json`

**Steps:**

1. Write failing Agent tests for default current sender, explicit owner, unsupported owner route rejection before creation, sent receipt, and failed delivery state.
2. Write failing Flow tests for `executor.responder`, enum validation, current sender, owner, and persisted delivery receipt.
3. Run focused tests; expect RED.
4. Add the sealed `responder` enum to the model tool schema and Flow executor schema; runtime binds authoritative IDs and targets.
5. Add one common create-and-deliver helper. Current sender may fall back on unsupported channels; owner must fail closed before create.
6. Record successful delivery receipts; atomically terminal-fail an
   undeliverable request and propagate the error without suspending.
7. Rerun tests; expect GREEN.
8. Commit `feat: bind and deliver human request responders`.

### Task 5: Consume explicit iLink answers before Agent Turn

**Files:**
- Modify: `apps/xira/internal/runtime/runtime.go`
- Modify: `apps/xira/internal/channelrunner/manager.go`
- Modify: `apps/xira/cmd/xira/main.go`
- Modify: `apps/xira/internal/channelrunner/ilink/runner.go`
- Modify: `apps/xira/internal/channelrunner/ilink/runner_test.go`
- Modify: `apps/xira/internal/channelrunner/ilink/runner_concurrent_test.go`

**Steps:**

1. Write failing tests for resolver injection and authoritative `ilink_user_id` preservation.
2. Write failing inbound tests for valid, malformed, unknown, unauthorized, expired, and duplicate `/answer` messages; assert no Runtime `RunAgent` call for every recognized command.
3. Run focused iLink tests; expect RED.
4. Inject `TextHITLResolver`, preserve sender ID type in metadata, and handle protocol traffic after authorization/dedupe but before ChatKeySession.
5. Remove iLink's call to implicit `TryResolveHITL`; leave Feishu/WebSocket unchanged.
6. Complete dedupe before best-effort ACK after a committed response; only
   uncommitted protocol errors are forgotten when their error ACK fails.
7. Rerun tests and `-race`; expect GREEN.
8. Commit `feat: handle explicit human responses in ilink`.

### Task 6: Prevent duplicate/public HumanRequest rendering

**Files:**
- Modify: `apps/xira/internal/runtime/message_bus.go`
- Modify: `apps/xira/internal/runtime/event_mapping.go`
- Modify: `apps/xira/internal/runtime/service.go`
- Modify: `apps/xira/internal/runtime/service_adk.go`
- Modify: `apps/xira/internal/runtime/delegation.go`
- Modify: `apps/xira/internal/channelrunner/progress/render_event.go`
- Modify: corresponding event mapping/render tests

**Steps:**

1. Write failing event roundtrip tests for request ID, responder type, and delivery status from real request creation output.
2. Write failing renderer tests: delivered explicit requests do not duplicate; owner requests never render publicly; terminal delivery failure does not create a waiting-human signal.
3. Run focused runtime/progress tests; expect RED.
4. Extend the sealed HumanRequested event and every producer/mapping case; update renderer switch explicitly.
5. Rerun tests and contract coverage; expect GREEN.
6. Commit `fix: keep private human requests out of public progress`.

### Task 7: Prove Agent Run and Flow end to end

**Files:**
- Create: `apps/xira/internal/runtime/human_text_response_e2e_test.go`
- Modify: `apps/xira/internal/runtime/resume_delivery_test.go`
- Modify: `apps/xira/internal/runtime/flow_bridge_test.go`
- Modify: `docs/architecture/xira-agent-hitl-v0.zh.md`

**Steps:**

1. Write an Agent Run E2E test: iLink-origin request, explicit response, completed durable resume, final delivered to the original scope.
2. Write a Flow E2E test for current sender and owner DM; assert the response DM never replaces the original flow/run context or final target.
3. Add multi-pending/cross-request and conflicting-retry tests using real generated references.
4. Run focused E2E tests; expect RED before wiring gaps, then GREEN after minimal fixes.
5. Document #164 text/delivery/response contracts and #165/#166 boundaries.
6. Commit `test: prove ilink human request interaction end to end`.

### Task 8: Full verification and PR

**Files:**
- Modify: PR description after creation

**Steps:**

1. Run `gofmt`, `git diff --check`, and verify no file exceeds 600 lines.
2. Run package coverage for humanrequest, runtime, channelrunner, progress, ilink, and flow; require package minimum 85% and every marked contract function 100%.
3. Run `go build ./apps/xira/...`, `go test ./apps/xira/... -count=1`, and `go test -race ./apps/xira/... -count=1`.
4. Copy the ignored local key only for validation, run `task live-test`, verify no FAIL/live-gated SKIP, then remove the copied key.
5. Verify every commit with `git show --stat`, push `codex/164-humanrequest-text-ilink`, and open a PR closing #164 and linking #155/#165.
6. List package coverage and every uncovered core/contract function with explanation in the PR description.
