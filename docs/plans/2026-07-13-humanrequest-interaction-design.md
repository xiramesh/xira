# HumanRequest Interaction and Reliable Resume Design

## Context

Issue #155 originally described owner-private HITL as one feature. Re-auditing current `main` showed that the hard part is broader: Agent Run and Flow already share persisted HumanRequests, but response correlation, delivery presentation, responder authorization, and crash-safe resume are not expressed as one channel-neutral contract. Owner is one responder policy; Feishu buttons, iLink text, and future Telegram/Discord controls are adapter presentations.

Issue #163 owns the common foundation. Issues #164-#166 own text/iLink, Feishu cards, and WebSocket frames.

## Requirements

- Preserve the existing HumanRequest store and the existing Agent/Flow resume implementations.
- Model HumanRequest source, responder, and channel presentation as independent dimensions.
- New requests carry an opaque correlation token and an authoritative responder binding when available.
- A resolved response and its resume progress are persisted separately so a crash cannot consume the answer while leaving the original run permanently paused.
- Repeated delivery or response is idempotent only when the same idempotency key carries the same payload; conflicting reuse fails closed.
- Expired, wrong-responder, wrong-entrypoint, wrong-correlation, and cross-request answers fail closed.
- Old HumanRequest JSON remains readable and is not replayed automatically merely because it lacks the new resume fields.
- No channel UI or platform callback schema enters the runtime/humanrequest packages.

## Alternatives Considered

### Owner-only HumanRequest type

Rejected. Agent Run/Flow approvals for the current sender need the same correlation, delivery receipt, and reliable resume behavior. An owner-only type would duplicate the state machine and force future adapters to learn two protocols.

### Put lifecycle and authorization in each runner

Rejected. Runners can authenticate platform events and extract typed identity, but they must not decide whether an identity is the request's authorized responder or mutate resume state independently. That would drift across Feishu, iLink, Telegram, and Discord.

### Keep `pending/resolved` and retry resume from the API error

Rejected. The current store commits `resolved` before resume. A process exit between those operations makes a retry conflict. Durable resume state and reconciliation are required.

### Add a second queue/database

Rejected for v0. The file-backed HumanRequest record can act as the durable work item if response and resume states are explicit and every transition is atomic. A separate broker would add operational complexity without removing the need for idempotent resume.

## Architecture

### Domain record

`HumanRequest` retains `pending/resolved` as the human-answer state and adds three orthogonal records:

- `ResponderPolicy`: `current_sender` or `owner`, plus runtime-bound entrypoint and typed identity.
- `DeliveryState`: optional `pending/sent/failed` status, attempts, platform message ID, timestamps, and last error.
- `ResumeState`: `waiting_response/pending/running/completed/failed`, attempts, timestamps, and last error.

New records receive a random correlation token. Old JSON has empty responder/correlation/resume fields and remains a legacy record. It is readable and resolvable through the existing trusted/same-chat API, but is not included in automatic resume reconciliation.

### Response resolution

The new exact response entry accepts a channel-neutral envelope containing request ID, correlation, entrypoint, typed actor identity, response kind/message, delivery message ID, and idempotency key. Runtime checks dynamic/static owner authority when responder type is owner. The store atomically rechecks persisted responder/correlation/expiry and writes the response plus `resume_pending`.

The legacy `ResolveHumanRequest` remains for existing same-chat/API callers during migration, but it uses the same durable resume lifecycle after the response is stored.

### Reliable resume

Runtime atomically claims `resume_pending/resume_failed` as `resume_running`, dispatches by source, then writes `resume_completed` or `resume_failed`.

- `agent_request` calls existing `resumeDirectHumanRequest`.
- `flow_human_approval` calls existing `ResumeFlow`.
- Other sources complete without a resume side effect, preserving current behavior.

Both resume implementations are already idempotent at their terminal state: Agent resume no-ops when the run is no longer `waiting_human`; Flow resume returns the already-advanced run. Therefore retry after “side effect succeeded, completion marker failed” is safe.

On startup, stale `resume_running` records are moved to `resume_failed`. Once the channel manager/outbound dependencies are installed, reconciliation scans `resume_pending/resume_failed` and retries. This is event/startup-driven, not cron.

### Adapter boundary

Future channel adapters translate between native interaction and the exact response envelope:

```text
Feishu card action ─┐
iLink /answer ──────┼─> HumanResponseEnvelope -> runtime validation/resume
Discord interaction ┘
```

The inverse delivery contract returns a channel-neutral receipt. Platform UI and callback JSON remain in the concrete runner.

## Key Decisions

| Decision | Trade-off |
|---|---|
| Responder and presentation are orthogonal | Slightly larger schema; prevents owner/platform special cases |
| Persist resume state in HumanRequest | More file writes; closes the resolved-before-resume crash gap |
| Exact idempotency requires key + identical payload | Callers must generate stable keys; conflicting retries cannot silently change decisions |
| Legacy records are not auto-reconciled | Old crash gaps remain manual; avoids replaying already-completed historical requests |
| Runtime revalidates owner | One extra lookup; prevents runner-specific authorization drift |

## Failure Modes

| Failure | Required behavior |
|---|---|
| Wrong correlation/request/entrypoint/responder | Reject without mutating request |
| Expired response | Reject without resume |
| Duplicate identical response | Return existing response; claim resume only if still pending/failed |
| Duplicate conflicting response | Conflict |
| Crash while `resume_running` | Startup marks failed and retries |
| Resume succeeds, completion state write fails | Retry is safe because Agent/Flow resume is terminal-idempotent |
| Unknown/legacy resume metadata | Do not auto-replay |
| Unsupported owner route | Later delivery layer rejects before creating an unreachable pending request |

## Testing

- Contract branch tests for responder, correlation, expiry, idempotency, delivery transitions, and every resume transition.
- E2E crash-window tests using a resolved request with `resume_pending`, a restarted service, and the real persisted RunStore.
- Separate Agent Run and Flow reconciliation tests.
- Compatibility test loading old JSON without new fields.
- Coverage: marked contract functions 100%; affected packages at least 85% by statement.
- Full build, tests, race tests, and live DeepSeek suite before PR.

## Explicit Non-goals

- Feishu card JSON/callbacks (#165).
- iLink text parsing (#164).
- WebSocket `human_response` frames (#166).
- Cross-channel person graph.
- A second Agent Loop, background scheduler, or generalized workflow engine.
