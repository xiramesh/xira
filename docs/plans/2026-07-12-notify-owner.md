# Owner Private Notification Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a safe one-way `notify_owner` runtime tool that resolves a typed owner recipient and delivers through the exact bound entrypoint.

**Architecture:** Preserve the existing outbound route context and add a typed recipient. Persist sender identity type in owner bindings, resolve route and recipient inside runtime, route by entrypoint before channel, and expose the runtime-native tool through the production ADK path. Typed-recipient capability is enforced on the selected runner; connection-bound websocket remains authorization-only for owner binding.

**Tech Stack:** Go, Google ADK function tools, DeepSeek tool calling, Lark/Feishu OpenAPI SDK, file-backed runtime state.

---

### Task 1: Preserve typed sender identity in owner bindings

**Files:**
- Modify: `apps/xira/internal/channel/types.go`
- Modify: `apps/xira/internal/channel/types_test.go`
- Modify: `apps/xira/internal/channelrunner/ingest/ingest.go`
- Modify: `apps/xira/internal/channelrunner/ingest/ingest_test.go`
- Modify: `apps/xira/internal/channelrunner/feishu/runner.go`
- Modify: `apps/xira/internal/channelrunner/feishu/mention.go`
- Modify: `apps/xira/internal/channelrunner/feishu/mention_test.go`
- Modify: `apps/xira/internal/runtime/owner_bind.go`
- Modify: `apps/xira/internal/runtime/owner_bind_test.go`
- Modify: `apps/xira/internal/entrypoints/registry.go`

**Steps:**

1. Write failing tests for sender ID type normalization/raw round-trip, Feishu sender extraction symmetry, typed binding persistence/reload, legacy binding fail-closed routing, and same-owner `/bind` enrichment.
2. Run `go test ./apps/xira/internal/channel ./apps/xira/internal/channelrunner/ingest ./apps/xira/internal/channelrunner/feishu ./apps/xira/internal/runtime -run 'SenderIDType|OwnerBinding' -count=1` and confirm RED.
3. Add `SenderIDType`, propagate it through shared ingest/Feishu, and persist `OwnerSenderIDType` with backward-compatible JSON loading.
4. Add optional static `owner_id_type` configuration and typed owner target resolution tests.
5. Run the targeted tests and confirm GREEN.

### Task 2: Add typed outbound recipients and exact runner routing

**Files:**
- Modify: `apps/xira/internal/channel/outbound.go`
- Modify: `apps/xira/internal/channel/outbound_test.go`
- Modify: `apps/xira/internal/channelrunner/manager.go`
- Modify: `apps/xira/internal/channelrunner/manager_emit_test.go`
- Modify: `apps/xira/internal/channelrunner/feishu/runner.go`
- Modify: `apps/xira/internal/channelrunner/feishu/emit_test.go`
- Modify: `apps/xira/internal/channelrunner/ilink/runner.go`
- Modify: `apps/xira/internal/channelrunner/ilink/emit_test.go`

**Steps:**

1. Write failing tests for recipient normalization, exact entrypoint routing with two Feishu runners, channel mismatch, ambiguous fallback, and Feishu direct-user request shape.
2. Run `go test ./apps/xira/internal/channel ./apps/xira/internal/channelrunner -run 'Recipient|Emit' -count=1` and confirm RED.
3. Add `OutboundRecipient` to the envelope and normalize it without changing existing chat-target behavior.
4. Change Manager routing to prefer exact runner ID and reject ambiguous channel fallback.
5. Refactor Feishu send helpers to accept explicit receive ID type/ID; preserve the chat-ID wrapper used by live turns.
6. Let iLink use recipient ID when present and reject unsupported recipient types.
7. Run targeted tests and confirm GREEN.

### Task 3: Implement owner target resolution and `notify_owner`

**Files:**
- Create: `apps/xira/internal/runtime/notify_owner.go`
- Create: `apps/xira/internal/runtime/notify_owner_test.go`
- Modify: `apps/xira/internal/runtime/runtime.go`
- Modify: `apps/xira/internal/runtime/service.go`
- Modify: `apps/xira/internal/runtime/service_adk.go`
- Modify: `apps/xira/internal/runtime/delegation.go`
- Modify: `apps/xira/internal/runtime/service_test.go`

**Steps:**

1. Write failing contract tests for dynamic/static target resolution, missing type/no owner/no emitter, successful envelope shape, adapter failure, and message validation.
2. Write failing ADK wrapper tests proving the production path records the tool result, calls the delivery core, and limits one run to one successful notification.
3. Run `go test ./apps/xira/internal/runtime -run 'NotifyOwner|OwnerTarget' -count=1` and confirm RED.
4. Implement `OwnerTargetResolver` separately from authorization-only `OwnerResolver`.
5. Implement `notifyOwnerCore(ctx, args)` with authoritative target resolution, proactive envelope emission, structured result, event/audit hooks, and no recipient argument.
6. Register the runtime-native definition in the production ADK backend while preserving flow tool allowlists; do not advertise it from the undispatched legacy native generator.
7. Run targeted tests and confirm GREEN.

### Task 4: Documentation, coverage, and full verification

**Files:**
- Modify: `docs/architecture/xira-owner-bind-flow-v0.zh.md`
- Modify: `docs/architecture/xira-channel-contract-v0.zh.md` if present, otherwise the nearest outbound contract document
- Modify: PR description after creation

**Steps:**

1. Document typed binding migration, exact entrypoint routing, one-way semantics, and the #155 non-goal.
2. Run `gofmt -w` on changed Go files and `git diff --check`.
3. Run focused coverage profiles; verify all newly marked contract functions are 100% and affected packages remain at least 85%.
4. Run `go build ./...` and `go test ./... -count=1`.
5. Run race tests for channel, manager, Feishu, iLink, and runtime.
6. Run `task live-test`; inspect `/tmp/xira-live-test.log` for live-gated SKIP or FAIL.
7. Commit, verify `git show --stat HEAD`, push `codex/154-notify-owner`, open a PR linked to #154, and wait for CI.
