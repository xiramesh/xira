# XiraGarden

XiraGarden is the GUI client for Xira runtime inspection and operation.

Boundary:

- Uses HTTP APIs for Xira runtime inspection and management.
- Uses the `xiragarden` channel for conversation turns:
  `POST /api/v1/channels/xiragarden/messages`.
- Uses `WS /api/v1/channels/xiragarden/events` for live activity and run
  inspection events related to that channel.
- Does not import Go packages from `apps/xira/internal`.
- Does not read or write `.xira` state directly.
- Keeps user-facing conversation, turn activity, and raw run inspection as separate UI surfaces.

Planned source areas:

- `src/api`: Xira HTTP and WebSocket client code.
- `src/features/conversation`: readable user/agent transcript.
- `src/features/activity`: turn-level progress and summarized steps.
- `src/features/run-inspector`: raw events, audit events, tool calls, usage, and artifacts.
- `src/features/agents`: agent list and profile views.
- `src/features/sessions`: durable conversation/session views.
- `src/features/entrypoints`: Feishu, iLink, and local entrypoint status.
