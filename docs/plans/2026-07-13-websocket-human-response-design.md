# WebSocket Structured HumanResponse Design

## Goal

Implement issue #166 by letting a live WebSocket client answer an exact
`HumanRequest` with a structured `human_response` frame. The adapter supports
only `current_sender`; owner-targeted requests remain unsupported because the
WebSocket transport has no server-verified typed person identity or private
recipient route.

## Trust boundary

A physical WebSocket connection is not itself a user identity. One connection
may carry several ChatKeys, and the client supplies the chat and sender values
on ordinary `message` frames. The usable authority is narrower: an accepted
message registers one ChatKey under one live connection after entrypoint,
sender allowlist, and mention gating.

The response frame therefore does not carry sender, sender type, chat, owner,
entrypoint, or delivery identity. Runtime first loads the persisted request by
`request_id`. The runner then requires all of the following:

- responder is `current_sender`;
- persisted sender type is empty, as required for untrusted WebSocket identity;
- persisted request ChatKey parses as a WebSocket ChatKey;
- persisted responder sender exactly matches that ChatKey sender;
- the same physical connection is still the current registry owner of that
  ChatKey.

Only after those checks does the runner build a `HumanResponseEnvelope` from
the persisted responder and connection-owned ChatKey. A stale or displaced
connection cannot answer, even if it still has an open socket. Owner requests
return a structured unsupported error and never reach exact resolution.

## Frame contract

Inbound:

```json
{
  "type": "human_response",
  "id": "frame-42",
  "request_id": "hrq_123",
  "correlation_token": "550e8400-e29b-41d4-a716-446655440000",
  "action": "approve",
  "answer": ""
}
```

`request_id`, full correlation token, and action are required. `answer` is
required only for `action=answer`. Approval requests accept
`approve/deny/cancel`; freeform requests accept `answer/cancel`. Declared
freeform options are normalized through the shared HumanRequest option logic.
Non-answer actions reject a non-empty answer.

On successful atomic commit, the server replies with an `ack` whose status is
`human_response_accepted`. Validation, correlation, expiry, state, and
idempotency failures return a generic `human_response_rejected` error. The
adapter derives a stable SHA-256 idempotency key from the persisted ChatKey,
entrypoint, request, correlation, normalized response kind, and message, so an
identical frame retry converges even after reconnect.

## Resume and ordering

WebSocket uses the existing async exact resolver: authority and response are
committed before the ack, while Agent Run or Flow resume proceeds through the
shared durable resume claim. This keeps the read loop responsive and preserves
the same ordering as Feishu native callbacks: accepted response first, resumed
final later through the existing connection-bound `Emit` route.

The `interrupt` frame already exposes the created HumanRequest, so #166 does
not add another delivery mechanism or duplicate prompt frame. WebSocket starts
advertising `interactive_human_response` only after the structured handler is
wired in production.

## Legacy removal

The old message-text preflight selected the newest pending HumanRequest. Once
structured response frames exist, that fallback violates the exact-correlation
contract and must be deleted, including its manager injection and unused
classification helpers. A normal text message such as `approve` is again just
an Agent Turn message; it can never consume a pending request implicitly.

## Testing

- table-test every authority and response-kind branch;
- real loopback WebSocket tests for accepted ack, duplicate retry, wrong token,
  unknown request, owner unsupported, and a connection that does not own the
  persisted ChatKey;
- prove sender/type fields in arbitrary JSON are ignored because the frame
  schema has no such authority fields;
- prove capability and `ready.capabilities` match the production handler;
- run package coverage, full build/test/race, and real DeepSeek live tests.

