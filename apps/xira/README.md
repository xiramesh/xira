# Xira

Xira is the Go runtime application for this repository. It owns the CLI, HTTP API
server, channel runners, session store, run audit store, model adapter, and
built-in runtime tools.

The private Go implementation lives under `apps/xira/internal`. GUI applications
must not import these packages directly.

- Xira runtime inspection and management use the HTTP API exposed by `xira serve`.
- Conversation clients use channel endpoints. XiraGarden talks through the
  `xiragarden` channel at `/api/v1/channels/xiragarden/messages` and receives
  channel run events from `/api/v1/channels/xiragarden/events`.
- Channel account pairing is controlled by entrypoint APIs while `xira serve` is
  running. For iLink, configure an enabled `ilink` entrypoint with
  `allow_runtime_pairing: true`, then create a QR pairing with:

  ```bash
  curl -X POST http://127.0.0.1:8089/api/v1/entrypoints/<entrypoint-id>/pairings
  ```

  The response contains a `pairing_id`, `qr_code`, and `qr_image_content`. Poll
  the pairing until it is confirmed:

  ```bash
  curl http://127.0.0.1:8089/api/v1/entrypoints/<entrypoint-id>/pairings/<pairing-id>
  ```

  Confirmed accounts are persisted under the entrypoint state directory and are
  started by the channel runner. Use
  `GET /api/v1/entrypoints/<entrypoint-id>/accounts` to list them and
  `DELETE /api/v1/entrypoints/<entrypoint-id>/accounts/<account-id>` to remove
  one.

Common commands from the repository root:

```bash
task build
task serve
task agent:list
task runs:list
```
