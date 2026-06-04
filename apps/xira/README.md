# Xira

Xira is the Go runtime application for this repository. It owns the CLI, HTTP API
server, channel runners, session store, run audit store, model integration, and
built-in runtime tools.

The private Go implementation lives under `apps/xira/internal`. GUI applications
must not import these packages directly.

- Xira runtime inspection and management use the HTTP API exposed by `xira serve`.
- Conversation clients use channel endpoints. XiraGarden talks through the
  `xiragarden` channel at `/api/v1/channels/xiragarden/messages` and receives
  channel run events from `/api/v1/channels/xiragarden/events`.

Common commands from the repository root:

```bash
task build
task serve
task agent:list
task runs:list
```
