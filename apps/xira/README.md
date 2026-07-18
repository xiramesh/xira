# Xira

Xira is the Go runtime application for this repository. It owns the CLI, HTTP API
server, channel runners, session store, run audit store, model adapter, and
built-in runtime tools.

The private Go implementation lives under `apps/xira/internal`. GUI applications
must not import these packages directly.

- Xira runtime inspection and management use the HTTP API exposed by `xira serve`.
- Conversation clients use channel endpoints. The current runtime surface is
  CLI, HTTP API, and configured channel runners (`feishu`, `ilink`,
  `websocket`).
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

## Agent response format

Agent profiles can require a JSON object response in the `PROFILE.md`
frontmatter:

```yaml
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
  format: json
```

`format: json` is case-insensitive. Xira maps it through ADK to DeepSeek's
`response_format: {"type":"json_object"}` request parameter and rejects a
final response that is not exactly one JSON object. The Agent profile remains
responsible for describing the business-specific object shape.

When `format` is omitted, Xira preserves the existing plain-text behavior.
`format: text` is the explicit equivalent. Any other value makes the Agent
profile invalid instead of silently falling back to text.
