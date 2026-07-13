# Feishu HumanRequest Card Design

## Context and boundary

Issues #163 and #164 already provide the durable HumanRequest state machine,
exact responder validation, receipt-backed delivery, crash-safe resume, and the
full-correlation text protocol. Issue #165 adds a Feishu-native presentation;
it must not create a second request, authorization, or resume state machine.

The Feishu runner owns only platform facts: card JSON, the delivery message ID,
the authenticated card operator identities, callback parsing, the three-second
callback response, and text fallback. Runtime remains authoritative for the
request ID, correlation token, entrypoint, responder snapshot, current owner,
delivery message ID, expiry, idempotency, and Agent/Flow resume.

## Delivery and fallback

`Runner` implements the existing `HumanRequestDeliverer`. Current-sender
requests go to the originating `chat_id`; owner requests go to the runtime-
resolved typed user recipient. The card uses Feishu Card JSON 2.0 with
`update_multi=true`, a question body, and callback buttons. Approval requests
render approve/deny buttons. Freeform requests with options render one answer
button per option. Freeform requests without options show the copyable #164
`/answer` command because a button cannot collect arbitrary text.

The send response's real `message_id` is returned as the durable delivery
receipt. If card creation or sending fails, the runner renders #164's explicit
text request, sends it to the same private/origin route, and returns that text
message ID. Feishu therefore also consumes `/answer` before Agent Turn and no
longer uses implicit newest-pending resolution.

## Callback and async resume

`card.action.trigger` is registered on the existing Feishu long-connection
dispatcher. Button values carry request ID, full correlation token, action,
and optional answer. The handler takes the message ID and operator identities
only from the authenticated callback envelope. Feishu may provide both
`user_id` and `open_id`, while the persisted responder may use either; the
runtime selects the persisted ID type from this trusted identity set before
performing its existing exact atomic validation.

The callback must not wait for an LLM turn. A new runtime async exact-resolve
surface atomically validates and persists the response, schedules the existing
resume path, and returns. Only after that commit succeeds does the callback
return a success toast and a static approved/rejected/answered card. Invalid,
expired, cross-request, wrong-operator, wrong-message, or conflicting callbacks
return a safe error toast without replacing the card. A stable idempotency key
derived from entrypoint, card message, operator, action, and answer makes
duplicate callbacks converge on the same stored response.

## Failure and verification contract

- Card send failure falls back to explicit text; failure of both transports is
  recorded by the existing terminal delivery-failure path.
- Callback parse or authorization failure never enters Agent Turn and never
  mutates the card globally.
- Accepted callback resume runs asynchronously through the same Agent/Flow
  dispatcher and durable resume claim used by every other channel.
- Tests use real SDK callback and HTTP response shapes, including both
  `user_id` and `open_id`, rather than hand-built clean identities.
- Card parsing, action parsing, route selection, and identity selection are
  contract functions and require every branch to be covered.

## Non-goals

No WebSocket structured frame (#166), no cron/retry scheduler, no person graph,
no new Agent Loop, and no generic cross-channel card abstraction are added.
