# Xira WebSocket Channel v0 标准

> 本文定义 Xira 自身的通用 WebSocket channel 标准：别人连进 `xira serve`，
> 通过同一条 WebSocket 连接向 runtime 发送 inbound 消息，并接收 outbound
> ack、runtime events、最终回复、interrupt 和错误。`assistant_delta`、
> `human_response` resume-over-WS、`outbound_message` 是 Channel Contract
> 保留帧，待 channel adapter / streaming / proactive dispatch slice 落地后
> 才会在 `ready.capabilities` 广告。
>
> 通用 channel inbound / outbound 语义见
> `docs/architecture/xira-channel-contract-v0.zh.md`。本文只定义 WebSocket
> transport binding。

## 1. 定位

### 1.1 结论

Xira 的通用 WebSocket channel 是 **inbound runtime API**，不是
`channelrunner`。

```
client / gateway / UI / local tool
        -> WS -> xira serve
        -> runtime.RunAgent / Flow
        <- ack / event / response / interrupt / error
```

`channelrunner` 只用于必须由 Xira 主动连出去的平台，例如 Feishu / iLink
这类 SDK 长连接。通用 WebSocket 不属于这个模型。

### 1.2 术语

| 术语 | 含义 |
|---|---|
| WebSocket endpoint | `xira serve` 暴露的 HTTP upgrade 入口，属于 API server。 |
| channel | runtime 来源身份。v0 固定为 `websocket`。 |
| entrypoint | Xira 路由配置，决定默认 agent、允许 agent、session policy、鉴权策略。 |
| inbound frame | 客户端发给 Xira 的帧，例如 `message`、`ping`；`human_response` 是保留帧。 |
| outbound frame | Xira 发给客户端的帧，例如 `ack`、`event`、`response`、`interrupt`、`error`；`assistant_delta`、`outbound_message` 是保留帧。 |
| request_id | 单次 inbound 请求的相关 ID。所有对应 outbound 帧必须回带。 |
| run_id | Xira runtime 生成的 agent run ID。 |

### 1.3 非目标

- v0 不做 `apps/xira/internal/channelrunner/websocket`。
- v0 不让 Xira 主动 dial 外部 WebSocket server。
- v0 不支持二进制帧、文件传输、语音流、模型 token 级流式输出。
- v0 不支持任意 runtime channel 名；通用 WS 入口的 channel 是 `websocket`。
- v0 不要求替代现有 HTTP API；它是新的实时交互入口。

## 2. Endpoint

### 2.1 路由

```
GET /api/v1/channels/websocket/messages
```

该路由执行 WebSocket upgrade。实现位置：

```
apps/xira/internal/api/server.go
```

它和现有 runtime event WS 的区别：

| Endpoint | 角色 |
|---|---|
| `/api/v1/events` | 订阅所有 runtime events。 |
| `/api/v1/channels/xiragarden/events` | 订阅指定 channel 的 runtime events。 |
| `/api/v1/channels/websocket/messages` | 双向 channel：发消息进 runtime，同时收事件和回复。 |

### 2.2 连接参数

v0 支持两种方式指定 entrypoint：

```
GET /api/v1/channels/websocket/messages?entrypoint_id=websocket-default
```

或在每个 `message` 帧里传：

```json
{
  "type": "message",
  "id": "msg_001",
  "data": {
    "entrypoint_id": "websocket-default",
    "message": "你好",
    "context": {
      "chat_id": "chat-1",
      "chat_type": "direct",
      "sender_id": "user-1"
    }
  }
}
```

如果 query 和帧里都传了 `entrypoint_id`，二者必须一致。v0 推荐显式传
`entrypoint_id`，避免多个 websocket entrypoint 时路由不稳定。

### 2.3 Entrypoint 配置

示例：

```yaml
entrypoints:
  - id: websocket-default
    enabled: true
    channel: websocket
    default_agent: xira-assistant
    allowed_agents:
      - xira-assistant
      - research-assistant
    session:
      dimensions:
        - chat
        - sender
    respond_to_unmentioned_group_messages: false
```

注意：

- 不需要 `ws_url`。别人连进 Xira，不是 Xira 连出去。
- `channel` 必须是 `websocket`。
- 若要限制客户端可请求的 `agent_id`，必须配置显式 websocket entrypoint
  和 `allowed_agents`；未配置 entrypoint 时，runtime 隐式 entrypoint 会走
  Xira 默认 agent 解析策略。
- 如果未来需要鉴权，可复用 entrypoint 的 `token` / `token_env` 作为 inbound
  bearer token。

## 3. 帧信封

所有业务帧使用 JSON text frame。二进制帧 v0 不支持。

### 3.1 通用结构

```json
{
  "schema_version": "xira.websocket.v0",
  "type": "message",
  "id": "frm_001",
  "request_id": "msg_001",
  "time": "2026-06-18T02:30:00Z",
  "data": {}
}
```

字段规则：

| 字段 | 方向 | 必填 | 说明 |
|---|---|---|---|
| `schema_version` | 双向 | 否 | 缺省按 `xira.websocket.v0` 处理。 |
| `type` | 双向 | 是 | 帧类型。 |
| `id` | 双向 | 建议 | 当前帧 ID。客户端请求帧必须稳定。 |
| `request_id` | 出站必填 | 视情况 | 关联 inbound 请求。对 `ready`、`pong` 可为空。 |
| `time` | 出站必填 | 否 | RFC3339 时间。 |
| `data` | 双向 | 否 | 帧 payload。 |

客户端发送 `message` 时，若未显式提供 `request_id`，Xira 使用该帧 `id` 作为
`request_id`。如果 `id` 也为空，Xira 可以生成一个 server-side request id，
但客户端将失去幂等语义。

## 4. Inbound Frames

Inbound frame 指客户端发给 Xira 的帧。

### 4.1 `hello`

可选。用于声明连接级默认值。

```json
{
  "type": "hello",
  "id": "hello_001",
  "data": {
    "client_id": "xiragarden-local",
    "entrypoint_id": "websocket-default",
    "subscribe_events": true,
    "context_defaults": {
      "chat_type": "direct",
      "sender_id": "user-1"
    }
  }
}
```

语义：

- `entrypoint_id` 作为连接默认 entrypoint。
- `context_defaults` 只作为每个 `message.context` 的默认值，不覆盖帧内字段。
- 服务端回复 `ready`。

### 4.2 `message`

把用户消息送进 `runtime.RunAgent`。

```json
{
  "type": "message",
  "id": "msg_001",
  "data": {
    "entrypoint_id": "websocket-default",
    "agent_id": "xira-assistant",
    "message": "帮我看一下当前任务",
    "session_id": "",
    "context": {
      "channel": "websocket",
      "entrypoint_id": "websocket-default",
      "chat_id": "chat-1",
      "chat_type": "direct",
      "sender_id": "user-1",
      "message_id": "client-msg-001",
      "mentioned": true,
      "reply_to_message_id": "",
      "raw": {
        "client": "local-tool"
      }
    }
  }
}
```

字段规则：

| 字段 | 必填 | 说明 |
|---|---|---|
| `data.message` | 是 | 用户消息正文。 |
| `data.entrypoint_id` | v0 推荐必填 | 可由 query 或 `hello` 默认值补齐。 |
| `data.agent_id` | 否 | 请求指定 agent；必须被 entrypoint allow。 |
| `data.session_id` | 否 | 明确覆盖 conversation session；一般不传。 |
| `context.channel` | 否 | 若传，必须是 `websocket`。服务端最终会强制为 `websocket`。 |
| `context.entrypoint_id` | 否 | 若传，必须与 `data.entrypoint_id` 一致。 |
| `context.chat_id` | 是 | 会话维度。缺失会导致 session 不稳定。 |
| `context.chat_type` | 否 | 省略时 runtime 默认为 `direct`；显式传 `group`、`thread` 等中性值可启用对应策略。 |
| `context.sender_id` | 是 | 发送者身份。 |
| `context.message_id` | 强烈建议 | 幂等去重键。缺失时可使用 inbound frame `id`。 |
| `context.mentioned` | 否 | 群聊过滤使用。 |
| `context.raw` | 否 | 非路由元数据，必须是扁平 `map[string]string`；不要放 secrets。 |

服务端构造 `runtime.TurnRequest`：

```go
frt.TurnRequest{
    EntrypointID: data.EntrypointID,
    AgentID:      data.AgentID,
    Message:      data.Message,
    SessionID:    data.SessionID,
    Context: channel.InboundContext{
        Channel:      "websocket",
        EntrypointID: data.EntrypointID,
        ChatID:       data.Context.ChatID,
        ChatType:     data.Context.ChatType,
        SenderID:     data.Context.SenderID,
        MessageID:    data.Context.MessageID,
        Mentioned:    data.Context.Mentioned,
        Raw:          data.Context.Raw,
    },
}
```

### 4.3 `human_response`

用于回答 runtime 发出的 human request。当前 #14 transport slice 只保留协议形状，
不在 `ready.capabilities` 广告，也不处理该帧；客户端需要继续使用现有 HTTP
human-request API。完整 resume-over-WS 需要后续 slice 把 resume 后的新 run 与
原 WebSocket request 重新关联。

```json
{
  "type": "human_response",
  "id": "hrsp_001",
  "data": {
    "human_request_id": "hr_20260618_001",
    "kind": "approve",
    "actor": "user-1",
    "message": "同意执行",
    "idempotency_key": "hrsp_001"
  }
}
```

### 4.4 `ping`

```json
{"type": "ping", "id": "ping_001"}
```

服务端回复 `pong`。

## 5. Outbound Frames

Outbound frame 指 Xira 发给客户端的帧。

### 5.1 `ready`

`hello` 成功后的连接确认。

```json
{
  "type": "ready",
  "id": "srv_ready_001",
  "request_id": "hello_001",
  "data": {
    "channel": "websocket",
    "entrypoint_id": "websocket-default",
    "server": "xira",
    "capabilities": [
      "message",
      "event",
      "response",
      "interrupt"
    ]
  }
}
```

### 5.2 `ack`

服务端已接收并开始处理 `message`。

```json
{
  "type": "ack",
  "id": "srv_ack_001",
  "request_id": "msg_001",
  "data": {
    "status": "accepted",
    "entrypoint_id": "websocket-default",
    "channel": "websocket",
    "message_id": "client-msg-001"
  }
}
```

若消息被群聊过滤忽略，返回：

```json
{
  "type": "ack",
  "request_id": "msg_001",
  "data": {
    "status": "ignored",
    "reason": "unmentioned_group_message"
  }
}
```

若消息命中幂等窗口，返回：

```json
{
  "type": "ack",
  "request_id": "msg_001",
  "data": {
    "status": "duplicate",
    "message_id": "client-msg-001"
  }
}
```

### 5.3 `event`

runtime event 的实时推送。

```json
{
  "type": "event",
  "id": "srv_evt_001",
  "request_id": "msg_001",
  "run_id": "run_xxx",
  "data": {
    "event": {
      "id": "...",
      "schema_version": 1,
      "run_id": "run_xxx",
      "kind": "run.started",
      "source": "runtime",
      "scope": {
        "channel": "websocket",
        "entrypoint_id": "websocket-default",
        "chat_id": "chat-1",
        "sender_id": "user-1"
      },
      "payload": {}
    }
  }
}
```

事件过滤规则：

- 只推送当前连接已接受 message 对应的 run events。
- 识别 run 后，后续同 `run_id` 的事件必须继续推送。
- child run 事件必须沿用现有 `eventBelongsToChannel` 的 parent/child
  correlation 规则，避免 delegation 进度丢失。
- 优先使用 `evt.Scope.Channel` / `evt.Scope.EntrypointID`；legacy
  `payload["channel"]` 只做兼容兜底。

### 5.4 `assistant_delta`

面向用户的中间文本增量。它来自通用 Channel Contract 的
`assistant_delta`，WebSocket 只负责把它编码成 JSON frame。

当前 #14 transport slice 不实现该帧，也不会在 `ready.capabilities` 广告；
待 runtime streaming / channel adapter slice 落地后再启用。

```json
{
  "type": "assistant_delta",
  "id": "srv_delta_001",
  "request_id": "msg_001",
  "run_id": "run_xxx",
  "data": {
    "sequence": 1,
    "content": "我先看一下当前状态...",
    "content_format": "markdown"
  }
}
```

`assistant_delta` 可多次发送，不代表最终完成。最终结果仍由 `response`
发送。

### 5.5 `response`

agent run 完成后的最终回复。它是通用 Channel Contract 里 `assistant_final`
在 WebSocket binding 里的帧名；正常情况下，同一 `request_id` 只发送一次。
WebSocket binding 把通用 `assistant_final.data.content` 重命名为
`data.final_response`，并保留 `data.content_format` 表达内容格式。

```json
{
  "type": "response",
  "id": "srv_resp_001",
  "request_id": "msg_001",
  "run_id": "run_xxx",
  "data": {
    "agent_id": "xira-assistant",
    "run_id": "run_xxx",
    "entrypoint_id": "websocket-default",
    "session_id": "conversation:abcd1234",
    "route_matched_by": "entrypoint.implicit",
    "status": "completed",
    "final_response": "这是处理结果。",
    "content_format": "markdown",
    "started_at": "2026-06-19T08:00:00Z",
    "ended_at": "2026-06-19T08:00:01Z",
    "tool_calls": [],
    "artifacts": [],
    "usage": {},
    "verification": {}
  }
}
```

实现可选择额外带完整 `run` 对象，但标准字段必须稳定，客户端不能依赖完整
`TurnResponse` 的所有内部字段。

### 5.6 `interrupt`

run 因 HITL 或其他 suspendable condition 暂停时发送。

```json
{
  "type": "interrupt",
  "request_id": "msg_001",
  "run_id": "run_xxx",
  "data": {
    "status": "waiting_human",
    "human_requests": [
      {
        "id": "hr_20260618_001",
        "kind": "approval",
        "question": "是否允许执行该操作？"
      }
    ]
  }
}
```

`human_requests[]` 直接使用 `humanrequest.HumanRequest` 的 JSON 形状；
问题文本字段是 `question`。

当前 #14 transport slice 中，客户端需通过现有 HTTP human-request API 回复；
`human_response` 帧保留给后续 resume-over-WS slice。

### 5.7 `outbound_message`

Xira 主动向已连接且匹配 target 的 WebSocket client 推送请求外消息。它来自通用
Channel Contract 的 proactive `outbound_message`。

当前 #14 transport slice 不实现该帧，也不会在 `ready.capabilities` 广告；
需要后续 channel outbound adapter / proactive dispatch slice。

```json
{
  "type": "outbound_message",
  "id": "srv_msg_001",
  "data": {
    "target": {
      "channel": "websocket",
      "entrypoint_id": "websocket-default",
      "chat_id": "chat-1"
    },
    "reason": "human_request.reminder",
    "content": "你有一个待确认事项。",
    "content_format": "markdown"
  }
}
```

v0 只要求能向当前已连接客户端推送；离线队列或 relay 不属于本 slice。

### 5.8 `error`

```json
{
  "type": "error",
  "id": "srv_err_001",
  "request_id": "msg_001",
  "data": {
    "code": "validation_failed",
    "message": "context.chat_id is required",
    "retryable": false
  }
}
```

标准错误码：

| code | 说明 |
|---|---|
| `bad_json` | JSON 解析失败。 |
| `unsupported_type` | 不支持的 frame type。 |
| `validation_failed` | 缺字段或字段冲突。 |
| `unauthorized` | 鉴权失败。 |
| `entrypoint_not_found` | entrypoint 不存在或未启用。 |
| `channel_conflict` | inbound channel 不是 `websocket`，或与 entrypoint channel 冲突。 |
| `agent_not_allowed` | 请求 agent 不在 entrypoint allowlist。 |
| `run_failed` | runtime.RunAgent 返回错误。 |
| `internal_error` | 其他服务端错误。 |

message id 命中去重窗口时返回 `ack(status=duplicate)`，不是 `error`。

### 5.9 `pong`

```json
{"type": "pong", "request_id": "ping_001"}
```

## 6. 处理语义

### 6.1 Channel 语义

v0 中，WebSocket endpoint 的 runtime channel 固定为 `websocket`。

服务端必须拒绝：

```json
{"context": {"channel": "feishu"}}
```

原因是 Xira 当前 `channel` 是 runtime 来源身份，不是透明 transport。让一个
WebSocket 请求伪装成其他 channel 会破坏 entrypoint routing、session scope、
event scope 和 audit 可解释性。

### 6.2 Entrypoint 语义

服务端解析 entrypoint 的优先级：

1. `message.data.entrypoint_id`
2. `hello.data.entrypoint_id`
3. query `entrypoint_id`
4. 默认 `websocket-default`（仅当实现明确提供）

如果解析出的 entrypoint channel 不是 `websocket`，必须拒绝。

### 6.3 Session 语义

WebSocket 连接不是 Xira conversation session。

conversation session 由 `InboundContext` + entrypoint session policy 计算。推荐
session dimensions：

```yaml
session:
  dimensions:
    - chat
    - sender
```

同一条 WS 连接可以承载多个 `chat_id` / `sender_id`，因此客户端必须为每条
`message` 提供稳定 context。

### 6.4 去重

去重键：

```
entrypoint_id + ":" + context.message_id
```

如果 `context.message_id` 为空，使用 inbound frame `id` 并写回 runtime
event scope。当前实现要求二者至少有一个；如果都为空，返回
`validation_failed`。

去重行为：

- 新 key：记录为 processing，然后触发 `runtime.RunAgent`。
- processing / completed 命中同 key：返回 `ack(status=duplicate)`，不再次
  调用 `runtime.RunAgent`。
- `runtime.RunAgent` 失败或最终帧发送失败：`Forget` / 删除去重记录，允许
  客户端重试。

当前实现使用 API server 进程内内存去重，TTL 为 1h，不跨进程重启或多副本共享。

当前 `dedupe` 包位于 `internal/channelrunner/dedupe`。如果 API server 也要复用，
建议先把它提升到中性包，例如 `internal/channel/dedupe` 或
`internal/messagededupe`，避免 API 层依赖 runner 命名空间。

### 6.5 群消息过滤

若：

- `context.chat_type == "group"`
- `context.mentioned == false`
- entrypoint `respond_to_unmentioned_group_messages == false`

服务端不调用 `RunAgent`，返回：

```json
{
  "type": "ack",
  "request_id": "msg_001",
  "data": {
    "status": "ignored",
    "reason": "unmentioned_group_message"
  }
}
```

### 6.6 连接断开

v0 默认：WebSocket 连接断开时，取消该连接上仍在执行的请求 context。

未来如果需要 detached run，可新增：

```json
{"type": "message", "data": {"detached": true}}
```

但 v0 不做 detached run 的重连恢复协议。

### 6.7 顺序与并发

同一连接可以发送多个 `message`。服务端允许并发处理，但 outbound 帧必须用
`request_id` 关联原始请求。

客户端不能假设不同 `request_id` 的 outbound 帧全局有序；只能假设同一
`request_id` 内大致顺序为：

```
ack -> event* -> interrupt? -> response | error
```

后续启用 streaming 后，同一 `request_id` 内可在最终帧前插入
`assistant_delta*`。

## 7. 鉴权与部署

v0 默认适合本地和内网。

最低要求：

- `xira serve` 默认仍监听 `127.0.0.1`。
- 如果监听公网地址，必须配置 entrypoint token。
- token 校验方式：

```
Authorization: Bearer <token>
```

也可以临时支持：

```
?token=<token>
```

但 query token 不应作为长期推荐方式，因为它容易进入日志。

Origin 策略：

- 本地开发可允许所有 origin。
- 非本地部署必须限制 Origin 或要求 bearer token。

## 8. 实现边界

### 8.1 需要新增 / 修改

```
apps/xira/internal/api/server.go
  - 注册 /api/v1/channels/websocket/messages
  - WebSocket upgrade
  - inbound frame read loop
  - outbound write loop
  - RunAgent 调用
  - per-request event filter

apps/xira/internal/api/websocket_channel.go
  - 推荐拆出帧结构、校验、写帧 helper

apps/xira/internal/api/server_test.go
  - WebSocket message -> response
  - event streaming
  - channel conflict
  - entrypoint mismatch
  - group ignored
  - duplicate ack
  - auth
```

可选整理：

```
apps/xira/internal/channelrunner/dedupe
  -> apps/xira/internal/channel/dedupe
```

### 8.2 不需要新增

```
apps/xira/internal/channelrunner/websocket
```

除非未来出现“Xira 必须主动 dial 某个外部 WebSocket server”的具体平台需求。
那应该是一个 vendor-specific outbound connector，而不是本标准。

## 9. 测试标准

### 9.1 API 行为

- `GET /api/v1/channels/websocket/messages` 可以 upgrade。
- `message` 帧会触发 `runtime.RunAgent`。
- `response` 帧包含 `run_id`、`agent_id`、`session_id`、`final_response`、
  `content_format`。
- `event` 帧只包含当前连接当前请求相关 run。
- `interrupt` 帧能表达 waiting human。

### 9.2 路由与上下文

- `context.channel` 为空时被补成 `websocket`。
- `context.channel` 非 `websocket` 时返回 `channel_conflict`。
- `context.entrypoint_id` 与 `data.entrypoint_id` 不一致时返回
  `validation_failed`。
- entrypoint channel 非 `websocket` 时返回 `channel_conflict`。
- `chat_id` / `sender_id` 进入 session scope。

### 9.3 幂等与过滤

- 同一 `entrypoint_id + message_id` 在 TTL 内不会重复触发 run。
- 重复消息返回 `ack(status=duplicate)`。
- run 失败后允许重试。
- 未 mention 的 group message 在配置禁止时不触发 run。

### 9.4 安全

- 配置 token 后，无 token 连接失败。
- bearer token 正确时连接成功。
- 非本地部署时不依赖 `CheckOrigin: true` 作为安全策略。

## 10. 示例

### 10.1 连接后发送 hello

Client:

```json
{
  "type": "hello",
  "id": "hello_001",
  "data": {
    "client_id": "local-debugger",
    "entrypoint_id": "websocket-default"
  }
}
```

Server:

```json
{
  "type": "ready",
  "request_id": "hello_001",
  "data": {
    "channel": "websocket",
    "entrypoint_id": "websocket-default",
    "server": "xira",
    "capabilities": [
      "message",
      "event",
      "response",
      "interrupt"
    ]
  }
}
```

### 10.2 发送消息并收最终回复

Client:

```json
{
  "type": "message",
  "id": "msg_001",
  "data": {
    "entrypoint_id": "websocket-default",
    "message": "总结一下当前 run 的状态",
    "context": {
      "chat_id": "debug-chat",
      "chat_type": "direct",
      "sender_id": "yinwm",
      "message_id": "debug-msg-001"
    }
  }
}
```

Server:

```json
{
  "type": "ack",
  "request_id": "msg_001",
  "data": {
    "status": "accepted",
    "channel": "websocket",
    "entrypoint_id": "websocket-default",
    "message_id": "debug-msg-001"
  }
}
```

Server:

```json
{
  "type": "event",
  "request_id": "msg_001",
  "run_id": "run_xxx",
  "data": {
    "event": {
      "kind": "run.started",
      "run_id": "run_xxx",
      "scope": {
        "channel": "websocket",
        "entrypoint_id": "websocket-default"
      }
    }
  }
}
```

Server:

```json
{
  "type": "response",
  "request_id": "msg_001",
  "run_id": "run_xxx",
  "data": {
    "agent_id": "xira-assistant",
    "run_id": "run_xxx",
    "session_id": "conversation:abcd1234",
    "route_matched_by": "entrypoint.implicit",
    "status": "completed",
    "final_response": "当前 run 已完成。",
    "content_format": "markdown",
    "started_at": "2026-06-19T08:00:00Z",
    "ended_at": "2026-06-19T08:00:01Z",
    "tool_calls": [],
    "artifacts": []
  }
}
```

## 11. 与现有 Xira 表面的关系

| 现有表面 | 保留方式 |
|---|---|
| `POST /api/v1/agent-runs` | 保留，适合一次性 HTTP 调用。 |
| `POST /api/v1/channels/xiragarden/messages` | 保留，XiraGarden 当前兼容入口。 |
| `WS /api/v1/events` | 保留，通用 event 订阅。 |
| `WS /api/v1/channels/xiragarden/events` | 保留，XiraGarden channel event 订阅。 |
| `WS /api/v1/channels/websocket/messages` | 新增，通用双向实时 channel。 |
| `channelrunner/feishu`、`channelrunner/ilink` | 保留，处理必须由 Xira 主动连出去的平台。 |

最终目标不是把所有入口都变成 WebSocket，而是给 UI、本地工具、内部服务一个
统一、可实时、可关联 run/event/interrupt 的 channel 标准。
