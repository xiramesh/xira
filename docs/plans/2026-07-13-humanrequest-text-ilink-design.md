# HumanRequest Explicit Text Protocol and iLink Design

## Context

PR #167 / issue #163 established the durable HumanRequest state machine, exact
typed-responder validation, delivery state, and crash-safe Agent/Flow resume.
Issue #164 supplies the first real channel adapter: a copyable explicit text
protocol for channels without native controls, with iLink as the production
implementation.

The existing `progress.TryResolveHITL` behavior is not sufficient. It chooses
the newest pending request for a chat and interprets arbitrary text as that
answer. With more than one pending request, this is inherently ambiguous. On
iLink, #164 replaces that implicit path with an explicit `/answer <ref> ...`
protocol; Feishu and WebSocket keep their current behavior until #165/#166.

## Requirements

- One opaque text reference identifies exactly one persisted HumanRequest. No
  first-pending or chat-local guessing.
- The reference is the full 128-bit correlation UUID rendered as
  `HR-<32 uppercase hex characters>`; short prefixes are rejected.
- `/answer` is consumed as protocol traffic even when malformed, expired, or
  unauthorized. It must never become an Agent Turn.
- Runtime, not the runner, maps the reference to a request and performs option,
  responder, entrypoint, current-chat, expiry, and idempotency checks.
- Text answers remain `ResponseAnswer`; option labels normalize to option IDs.
  The resumed model interprets freeform intent. Native buttons in #165 may use
  `approve` / `deny` response kinds directly.
- Agent `human.request` and Flow `human_approval` may choose only the responder
  type (`current_sender` default, or `owner`). Runtime binds all identities.
- iLink sends the rendered request to the original conversation for
  `current_sender`, or via typed private Push for `owner`, and returns the real
  platform message ID for `DeliveryState`.
- Owner requests fail before creation when the exact entrypoint has no typed
  owner route or the selected runner lacks HumanRequest delivery support.
- A transport failure after creation is persisted as `delivery_failed`; the
  run/flow remains paused. #164 does not add cron or background delivery retry.
- A response received in an owner DM resumes the original Run/Flow scope. The
  response chat must never replace the persisted origin SessionScope.

## Architecture

### Shared text protocol

`internal/humanrequest/text_protocol.go` owns pure rendering and parsing:

```go
type TextResponseCommand struct {
    CorrelationToken string
    Answer           string
}

func TextReference(correlation string) (string, error)
func RenderTextRequest(req HumanRequest) (string, error)
func ParseTextResponse(content string) (TextResponseCommand, bool, error)
func NormalizeTextAnswer(req HumanRequest, answer string) (string, error)
```

`bool=false` means ordinary chat. `bool=true` means the message started with
`/answer`; parse errors are protocol errors and are still consumed.

### Exact reference lookup and response

The Store scans the workspace's persisted requests under its lock, compares
correlations in constant time, and rejects zero or multiple matches. Resolved
records remain searchable so an identical transport retry can reach the
existing idempotency check. Runtime exposes `ResolveHumanTextResponse`. It converts the
parsed reference and authoritative iLink inbound identity into the existing
`HumanResponseEnvelope`, then calls `ResolveHumanResponse`; there is still one
authorization/resume state machine.

For `current_sender`, runtime also requires the persisted `ChatKey` to equal
the inbound chat key. For `owner`, the current owner plus persisted owner
snapshot checks from #163 remain authoritative. Runtime fills the persisted
delivery message ID into the exact envelope because the opaque reference was
issued by that delivery; the user does not type a second platform ID.

### Delivery port

Runtime defines a channel-neutral port:

```go
type HumanRequestDeliveryTarget struct {
    Route     channel.InboundContext
    Recipient *channel.OutboundRecipient
}

type HumanRequestDeliveryReceipt struct { MessageID string }

type HumanRequestDeliverer interface {
    ValidateHumanRequestDelivery(HumanRequestDeliveryTarget) error
    DeliverHumanRequest(context.Context, humanrequest.HumanRequest,
        HumanRequestDeliveryTarget) (HumanRequestDeliveryReceipt, error)
}
```

`channelrunner.Manager` performs route-local selection and capability checks;
the iLink runner renders text, calls `SendText` for an origin reply or `Push`
for an owner DM, and returns the SDK message ID. Existing `OutboundEmitter`
remains unchanged for normal final/proactive messages.

Current-sender requests fall back to the existing generic HITL presentation on
channels without this port. Owner requests do not fall back to a public chat.

### Event rendering

Runtime HumanRequested signals carry request ID, responder type, and delivery
status. When explicit delivery succeeded, the generic progress renderer drops
the duplicate. Owner-targeted requests are never rendered into the originating
group, including when private delivery fails.

### iLink inbound ordering

The iLink runner processes an authenticated message in this order:

1. ingest authorization and message dedupe;
2. build authoritative inbound context, including `ilink_user_id` type;
3. parse/consume `/answer` and call `ResolveHumanTextResponse`;
4. send a deterministic ACK or safe protocol error;
5. only non-protocol messages enter ChatKeySession / Agent Turn.

The old implicit `TryResolveHITL` call is removed from iLink. Other runners are
unchanged in #164.

## Failure Modes

| Failure | Required behavior |
|---|---|
| Short/malformed/unknown reference | consume command, safe error, no Agent Turn |
| Multiple records share one correlation | conflict, consume command, no first match |
| Wrong sender/type/entrypoint/chat | exact rejection without mutation |
| Owner reply in DM | authorize owner, resume persisted origin scope |
| Duplicate same iLink message | dedupe or exact idempotent response |
| Conflicting retry | conflict |
| Expired request | safe expired response, no resume |
| Unsupported owner route | reject before request creation |
| Send fails after create | persist `delivery_failed`, keep paused, log error |
| Receipt persistence fails after send | log explicitly; correlation remains resolvable |

## Alternatives Rejected

- **Short request prefixes (`HR-7K2P`)**: insufficient collision resistance and
  require ambiguous prefix search. The example remains conceptual; production
  uses the full correlation entropy.
- **Runner scans pending requests**: duplicates authorization and recreates
  first-pending ambiguity.
- **Put request ID and correlation in two user arguments**: precise but noisy;
  one opaque reference already carries full correlation entropy and runtime
  resolves the internal request ID.
- **Reuse generic `OutboundEmitter.Emit`**: it discards platform message IDs,
  so DeliveryState and later Feishu callback message validation would be fake.
- **Automatically retry delivery in a goroutine/cron**: needs persisted route
  scheduling and backoff policy; explicitly deferred.

## Testing

- Contract tables for reference formatting/parsing and option normalization.
- Store tests for exact lookup, duplicate-correlation conflict, and expiry path.
- Runtime tests for current sender chat binding, owner authority, idempotency,
  Agent resume, Flow resume, and origin SessionScope preservation.
- Manager route-local delivery tests and iLink receipt/render tests.
- iLink inbound tests proving malformed, unauthorized, duplicate, and valid
  commands never start an Agent Turn.
- Real DeepSeek live suite plus full build/test/race and package coverage.

## Non-goals

- Feishu card actions or fallback wiring (#165).
- WebSocket structured response frames (#166).
- Owner transfer/unbind (#168).
- Delivery cron, retry scheduler, or cross-channel person graph.
