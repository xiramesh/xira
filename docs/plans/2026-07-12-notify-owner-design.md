# Owner Private Notification Design

## Context

PR #153 completed the first slice of #124: a structured `@owner` mention starts the existing Agent Turn, preserves the real mention facts, and gives the model the owner's AI-intern/non-impersonation contract. The remaining one-way gap is delivery: the model can decide that the owner should be notified, but the runtime cannot resolve a safe private recipient or send through the exact entrypoint that owns that identity.

Issue #154 owns this one-way slice. Issue #155 separately owns owner replies and cross-chatKey HITL resume.

## Requirements

- A model may call `notify_owner` from the existing Agent Loop. The model supplies only message content; it cannot choose a recipient.
- Runtime resolves the owner from the current authoritative entrypoint and emits a proactive outbound envelope through that exact runner.
- A successful private notification does not require a public group final.
- Missing/unroutable owners, unsupported channels, and adapter/API failures are returned honestly to the model and audit log.
- Existing owner authorization semantics stay unchanged. `notify_owner` is not an owner-only skill: a non-owner may explicitly `@owner`, and the AI intern may notify its owner.
- The design must not guess identity type from display names or free text.

## Alternatives Considered

### 1. Send to `OwnerSenderID` as a chat ID

Rejected. Feishu distinguishes `chat_id`, `open_id`, `user_id`, and `union_id`; the current binding discards that type. Treating a sender ID as a chat ID would silently misroute or fail.

### 2. Put a full private-chat ID in the owner binding

Rejected as the primary contract. The bind event reliably contains a typed person identity, but a private chat ID may not exist yet and differs by platform. The channel adapter should translate a typed recipient identity into its native send API.

### 3. Typed recipient plus existing route context

Chosen. Keep `OutboundEnvelope.Target` as the existing route context and add an optional typed `Recipient`. Chat/resume delivery remains backward compatible; proactive owner delivery uses the recipient. This is smaller than replacing every target with a new outbound DTO while still making the ambiguous part explicit.

## Architecture

### Typed sender and binding

`channel.InboundContext` and shared ingest gain `SenderIDType`. Normalization persists it into raw metadata so resume paths retain it. Feishu derives `(id, id_type)` through the same `canonicalUserIdentity` function already used for structured mentions. Dynamic owner bindings persist both fields.

Existing binding files remain readable. A binding without `owner_sender_id_type` remains valid for authorization but is not routable. When the same already-bound owner sends `/bind ...` through a typed channel event, the runtime safely enriches and persists the missing type before returning the idempotent "already owner" result. The runtime never guesses the missing type.

Static owner configuration may declare an optional `owner_id_type`. A static owner without it remains authorization-only and `notify_owner` fails closed.

### Owner target resolution

`OwnerTargetResolver` is separate from `OwnerResolver`. `IsOwner` remains the authorization gate. `ResolveOwnerTarget(entrypointID)` returns an authoritative route context derived from the entrypoint definition plus a typed recipient derived from the dynamic/static binding. The model cannot override channel, entrypoint, account, app, bot, recipient ID, or recipient type.

### Outbound routing and adapters

`OutboundEnvelope` gains `Recipient {ID, IDType}`. Manager routing follows this order:

1. If `Target.EntrypointID` is set, match runner ID exactly and verify any supplied channel agrees.
2. Otherwise, allow channel-only fallback only when exactly one matching runner exists.
3. Multiple channel matches without an entrypoint are an explicit ambiguity error.

Feishu keeps `chat_id` sends for ordinary finals and adds direct sends for `open_id`, `user_id`, or `union_id`. iLink may use the recipient ID as its user target; unsupported recipient types fail explicitly. Typed-recipient delivery is a separate capability from ordinary proactive delivery and is checked on the runner selected by `EntrypointID`, not against the Manager fleet union. WebSocket remains connection/chat-bound: it retains ordinary proactive delivery for resume finals but rejects every envelope carrying `Recipient`.

### Runtime-native tool

`notify_owner` accepts one required `message` string. Runtime obtains the current `runExecutionContext`, resolves the owner target from its entrypoint, builds an `OutboundProactiveMessage`, and calls the injected emitter. The tool returns structured `sent`, `rejected`, or `failed` status. It records a tool call, runtime event, and audit decision without logging full private message content. One Agent run may complete at most one successful owner notification; a failed delivery may be retried. Intentional silence requires at least one `notify_owner` attempt to be `sent`: failed/rejected attempts of the same tool do not undo a successful delivery, while any other tool failure still blocks silence.

The production ADK path calls the shared executor. The legacy `generateNativeDeepSeek` path is not production-dispatched and does not advertise this tool. Flow runtime tool allowlists still apply. The tool description tells the model that successful delivery can be followed by an empty final when no public reply is needed.

## Failure Modes

- No entrypoint/run execution context: reject.
- No owner: reject.
- Legacy/static binding without ID type: reject with a rebind/configuration instruction.
- No outbound emitter or no exact runner: fail.
- Ambiguous channel-only routing: fail rather than select the first bot.
- Unsupported recipient ID type: fail at the adapter.
- Remote API failure: fail and return the adapter error; never claim delivery.

## Testing

- Contract tests cover every normalization, resolution, and routing branch at 100%.
- Production-path `/bind` tests use typed inbound context and restart the store.
- Manager tests use two same-channel runners to prove exact entrypoint selection and ambiguity failure.
- Feishu adapter tests assert the actual `receive_id_type` and `receive_id` request.
- Runtime tests exercise the production ADK wrapper through the shared executor, including repeated calls in one run.
- Live DeepSeek test asks the real model to call `notify_owner` against a recording emitter and verifies no live gate was skipped. Real Feishu delivery remains an operator smoke test because CI has no Feishu tenant credentials.

## Explicit Non-goals

- Owner reply correlation or HITL resume (#155).
- Cross-channel person graphs.
- A second Agent Loop or pre-classifier.
- Natural-language owner detection.
