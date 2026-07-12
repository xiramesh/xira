# Owner Mention Turn Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make an exact Feishu `@owner` mention start the existing Agent Turn with owner-assistant semantics, while ordinary group messages remain observe-only.

**Architecture:** Preserve mention targets and addressed-party facts as first-class channel context. Feishu computes exact bot/owner addressing from structured mention identities, Ingest uses those facts only to choose observe versus dispatch, and the existing runtime adds a static owner-assistant contract to the normal system instruction. No classifier model, second loop, or private owner-delivery mechanism is introduced in this PR.

**Tech Stack:** Go, Lark/Feishu SDK, existing channelrunner Ingest, ChatKeySession, runtime instruction composition, Go unit/integration tests.

---

## Design decision

Three approaches were considered:

1. **Raw metadata only:** encode `owner_mentioned=true` in `InboundContext.Raw`. Smallest diff, but it turns a core routing/identity fact into an undocumented string convention and makes cross-package behavior easy to drift.
2. **Separate owner turn/classifier:** add a lightweight model or owner-specific loop before the current runtime. Rejected because the mention is already an explicit activation signal and the existing Agent Loop already decides whether to stop, reply, or call tools.
3. **First-class addressing facts (selected):** add typed mention targets plus `agent`/`owner` addressed-party values to the existing inbound context. Keep `Mentioned` for compatibility, but make owner addressing explicit and durable.

The selected shape separates facts from judgment: the channel adapter establishes who was structurally mentioned; the model decides what the message means and what useful action, if any, to take.

## Scope boundary

This PR completes perception and activation only:

- exact `@owner` matching;
- one existing Agent Turn for `@owner`, `@agent`, or both;
- complete shared `[chat]` history hydration;
- owner-assistant runtime instruction;
- intentional empty-final silence remains supported.

Private `notify_owner` delivery remains the next PR because current owner bindings do not carry a private destination and Feishu outbound accepts only a chat ID.

### Task 1: Add the channel addressing contract

**Files:**
- Modify: `apps/xira/internal/channel/types.go`
- Test: `apps/xira/internal/channel/types_test.go`

**Step 1: Write failing tests**

Add behavior tests proving that normalization:

- preserves structured mention targets (`id`, `id_type`, `name`);
- accepts only known addressed-party values (`agent`, `owner`);
- trims and de-duplicates addressed parties;
- copies slice data rather than aliasing caller-owned storage;
- restores addressed-party facts from persisted raw metadata when typed fields are absent.

**Step 2: Run the focused tests and verify RED**

Run:

```bash
go test ./apps/xira/internal/channel -run 'TestNormalizeInboundContext.*Address' -count=1
```

Expected: compile/test failure because the addressing types and fields do not exist.

**Step 3: Implement the minimal typed contract**

Add:

```go
type AddressTarget string

const (
    AddressTargetAgent AddressTarget = "agent"
    AddressTargetOwner AddressTarget = "owner"
)

type MentionTarget struct {
    ID     string `json:"id" yaml:"id"`
    IDType string `json:"id_type,omitempty" yaml:"id_type,omitempty"`
    Name   string `json:"name,omitempty" yaml:"name,omitempty"`
}
```

Extend `InboundContext` with `MentionTargets []MentionTarget` and `AddressedTo []AddressTarget`. Normalize/copy both fields, serialize known addressed targets into `Raw["addressed_to"]`, and restore them from that key for resume paths. Unknown values must not become authoritative runtime semantics.

**Step 4: Run focused tests and verify GREEN**

Run:

```bash
go test ./apps/xira/internal/channel -count=1
```

### Task 2: Make Ingest dispatch owner-addressed group messages

**Files:**
- Modify: `apps/xira/internal/channelrunner/ingest/ingest.go`
- Test: `apps/xira/internal/channelrunner/ingest/ingest_test.go`

**Step 1: Write failing tests**

Add table cases proving:

- no address → observe;
- `agent` address → dispatch;
- `owner` address → dispatch;
- both → one dispatch decision;
- mention of an ordinary member without agent/owner address → observe;
- unauthorized sender remains rejected even when owner is addressed;
- `MessageInput.InboundContext()` preserves mention targets and addressed-party facts.

**Step 2: Run focused tests and verify RED**

```bash
go test ./apps/xira/internal/channelrunner/ingest -run 'TestGate.*Address|TestMessageInput.*Address' -count=1
```

**Step 3: Implement the minimal gate change**

Extend `MessageInput` with the typed fields. The group observe condition becomes “neither the existing bot-mentioned fact nor a known agent/owner addressed-party fact is present.” Authorization remains unchanged and still runs before dispatch.

**Step 4: Run package tests and verify GREEN**

```bash
go test ./apps/xira/internal/channelrunner/ingest -count=1
```

### Task 3: Extract Feishu mention identities and match the owner exactly

**Files:**
- Modify: `apps/xira/internal/channelrunner/feishu/runner.go`
- Test: `apps/xira/internal/channelrunner/feishu/runner_test.go`
- Test: `apps/xira/internal/channelrunner/feishu/runner_concurrent_test.go`

**Step 1: Write failing contract tests**

Add tests for:

- mention identity extraction uses the same priority as sender extraction: `user_id > open_id > union_id`;
- empty/nil mention records are ignored;
- owner matching uses strict ID equality through `OwnerResolver.IsOwner` and passes `definition.ID`;
- `@普通成员` is not owner-addressed;
- `@owner` without `@bot` starts exactly one existing `RunAgent` call;
- `@owner + @bot` also starts exactly one call and carries both addressed-party facts;
- the runtime request retains mention targets and the group chat ID/sender ID.

Use a real group-shaped Lark event in the integration tests; do not hand-build a clean `InboundContext` that bypasses the Feishu transformation.

**Step 2: Run focused tests and verify RED**

```bash
go test ./apps/xira/internal/channelrunner/feishu -run 'TestExtractMentionTargets|TestIsOwnerMentioned|TestFeishuOwnerMention' -count=1
```

**Step 3: Implement exact classification**

- Extract one canonical `MentionTarget` per Lark mention using the sender-ID priority.
- Keep `isBotMentioned` based on bot `open_id`.
- Add `isOwnerMentioned(ctx, targets)` that asks the injected owner resolver about canonical target IDs within the current entrypoint.
- Populate `AddressedTo` with `agent`, `owner`, or both.
- Pass the typed facts through `MessageInput.InboundContext()` into `ChatKeySession`; do not create another runtime path.

**Step 4: Run Feishu tests and verify GREEN**

```bash
go test ./apps/xira/internal/channelrunner/feishu -count=1
```

### Task 4: Inject owner-assistant semantics into the existing instruction

**Files:**
- Modify: `apps/xira/internal/runtime/service.go`
- Modify: `apps/xira/internal/runtime/events.go`
- Test: `apps/xira/internal/runtime/service_test.go`
- Test: `apps/xira/internal/runtime/events_scope_test.go`

**Step 1: Write failing tests**

Prove that:

- an owner-addressed turn receives the static “owner's AI intern” contract;
- the instruction says the model may judge whether to stay silent, reply as itself, prepare work, or use available tools;
- it explicitly forbids impersonating the owner or making owner decisions/commitments;
- agent-only turns do not receive the owner contract;
- dual-addressed turns retain both facts without a hard-coded priority;
- a persisted run metadata round trip restores owner addressing for resume hydration.

**Step 2: Run focused tests and verify RED**

```bash
go test ./apps/xira/internal/runtime -run 'Test.*OwnerAddress|TestInboundContextFromScope.*Address' -count=1
```

**Step 3: Implement static runtime semantics**

Render the sanitized structured mention targets in `# Conversation Context`. Render known addressing facts in a separate static section; when `owner` is present, append an authoritative paragraph explaining the AI-intern role and non-impersonation boundary. Never render arbitrary addressed-party strings as instructions. Restore addressed-party facts from persisted `run.Metadata` in `inboundContextFromScope` via normal context normalization.

**Step 4: Run runtime tests and verify GREEN**

```bash
go test ./apps/xira/internal/runtime -count=1
```

### Task 5: Verify the cross-package contract

**Files:**
- Modify if needed: tests/docs touched by failures only

**Step 1: Format and inspect the diff**

```bash
gofmt -w apps/xira/internal/channel apps/xira/internal/channelrunner/ingest apps/xira/internal/channelrunner/feishu apps/xira/internal/runtime
git diff --check
git diff --stat
```

**Step 2: Run targeted race tests**

```bash
go test -race ./apps/xira/internal/channelrunner/ingest ./apps/xira/internal/channelrunner/feishu ./apps/xira/internal/runtime -count=1
```

**Step 3: Run full build and test**

```bash
go build ./apps/xira/...
go test ./... -count=1
```

**Step 4: Measure affected-package coverage**

Run coverprofiles for `channel`, `channelrunner/ingest`, `channelrunner/feishu`, and `runtime`; confirm package coverage meets the repository floor and every new addressing/owner-matching contract branch is covered.

**Step 5: Run the real live suite**

```bash
task live-test
```

Expected: success with no DeepSeek live-test skip.

**Step 6: Review the actual commit content**

After committing, verify `git show --stat HEAD` includes every production and test file described above. Keep #124 open because private owner notification is intentionally the next PR.
