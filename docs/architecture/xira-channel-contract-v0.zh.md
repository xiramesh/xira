# Xira Channel Inbound/Outbound Contract v0

> 本文定义 Xira 的 transport-neutral channel 通信契约。
> WebSocket、CLI/TUI、XiraGarden、Feishu、iLink 和未来 channel 都应映射到这套
> inbound / outbound 语义，而不是各自发明一套消息类型。

## 1. 定位

### 1.1 结论

Xira channel contract 是 runtime 与外部会话媒介之间的通用语义层：

```
channel transport / adapter
        -> Channel Inbound Contract
        -> Xira runtime / flow / HITL
        -> Channel Outbound Contract
        -> channel transport / adapter
```

它不是 WebSocket 私有协议，也不是某个 SDK runner 的实现细节。

### 1.2 分层

| 层 | 职责 |
|---|---|
| Runtime | 执行 agent / flow，生成 run、runtime events、final response、interrupt。 |
| Channel Contract | 定义 inbound / outbound 的中性消息类型、关联字段、顺序与交付语义。 |
| Transport Binding | 把 contract 编码到具体传输，例如 WebSocket JSON frame、CLI stdout、HTTP response。 |
| Channel Adapter | 处理平台能力差异，例如 Feishu/iLink 只能安全地节流或合并消息。 |

### 1.3 Channel 不是 Transport

在 Xira 现有 runtime 中，`channel` 是来源/目标身份，不是透明传输层标签。

例如：

- `websocket` 表示通过 Xira inbound WebSocket API 进入 runtime 的会话身份。
- `xiragarden` 表示 XiraGarden 当前 channel surface。
- `feishu` / `ilink` 表示对应平台 channel runner 归一化后的会话身份。

不能让一个 transport 请求伪装成另一个 runtime channel。这样会破坏
entrypoint routing、session scope、runtime event scope 和 audit 解释性。

## 2. 通用信封

所有 inbound / outbound 消息都应能表达为一个中性信封。不同 transport 可以
改变编码格式，但字段语义不变。

```json
{
  "schema_version": "xira.channel.v0",
  "type": "assistant_final",
  "id": "out_001",
  "request_id": "req_001",
  "run_id": "run_xxx",
  "time": "2026-06-19T02:30:00Z",
  "source": {},
  "target": {},
  "correlation": {},
  "data": {}
}
```

| 字段 | 方向 | 说明 |
|---|---|---|
| `schema_version` | 双向 | Contract 版本。v0 为 `xira.channel.v0`。 |
| `type` | 双向 | 消息类型。 |
| `id` | 双向 | 当前消息 ID。发送方生成，便于追踪与去重。 |
| `request_id` | 双向 | 一次用户请求的相关 ID。请求外主动消息可为空。 |
| `run_id` | 出站常用 | 关联 Xira agent / flow run。 |
| `time` | 出站必填 | 服务端生成时间。 |
| `source` | 双向 | 消息来源。 |
| `target` | 出站常用 | 目标 channel / chat / sender。 |
| `correlation` | 双向 | parent/child run、tool call、trace 等相关性。 |
| `data` | 双向 | 类型专属 payload。 |

### 2.1 Source

```json
{
  "channel": "websocket",
  "entrypoint_id": "websocket-default",
  "chat_id": "chat-1",
  "chat_type": "direct",
  "sender_id": "user-1",
  "message_id": "msg-1"
}
```

`source` 与 `channel.InboundContext` 对齐。它是 inbound message 进入
`runtime.TurnRequest.Context` 的来源。

### 2.2 Target

```json
{
  "channel": "feishu",
  "entrypoint_id": "feishu-yihao",
  "chat_id": "oc_xxx",
  "chat_type": "group",
  "sender_id": "xira"
}
```

`target` 用于出站投递，尤其是 proactive `outbound_message`。请求内响应通常可
从原始 `source` 派生 target。

### 2.3 Correlation

```json
{
  "trace_id": "run_parent",
  "parent_run_id": "run_parent",
  "child_run_id": "run_child",
  "tool_call_id": "tool_001",
  "parent_message_id": "msg-1"
}
```

要求：

- child run / delegation event 必须带足够 correlation，避免 UI 或 channel
  event filter 丢失子 run 进度。
- `request_id` 关联用户请求；`run_id` 关联 runtime run；二者不能互相替代。
- proactive `outbound_message` 可以没有 `request_id`，但必须有 target。

## 3. Channel Inbound

Inbound 指 channel transport / adapter 发给 Xira runtime 的消息。

### 3.1 `message`

用户或外部系统向 Xira 发起一次会话请求。

```json
{
  "type": "message",
  "id": "in_001",
  "request_id": "req_001",
  "source": {
    "channel": "websocket",
    "entrypoint_id": "websocket-default",
    "chat_id": "chat-1",
    "chat_type": "direct",
    "sender_id": "user-1",
    "message_id": "client-msg-001"
  },
  "data": {
    "agent_id": "xira-assistant",
    "content": "帮我看一下当前任务",
    "session_id": ""
  }
}
```

字段规则：

| 字段 | 必填 | 说明 |
|---|---|---|
| `request_id` | 强烈建议 | 用于关联 ack、delta、event、final、error。 |
| `source.channel` | 是 | runtime channel 身份。 |
| `source.entrypoint_id` | 推荐 | 多 entrypoint 时必须显式指定。 |
| `source.chat_id` | 是 | conversation 维度。 |
| `source.chat_type` | 否 | 省略时 runtime 归一化为 `direct`；显式传 `group`、`thread` 等中性值可启用对应策略。 |
| `source.sender_id` | 是 | 用户或外部系统身份。 |
| `source.message_id` | 强烈建议 | 幂等去重键。 |
| `data.content` | 是 | 用户可见输入。 |
| `data.agent_id` | 否 | 请求指定 agent，必须被 entrypoint allow。 |
| `data.session_id` | 否 | 显式覆盖 conversation session；一般不传。 |

### 3.2 `human_response`

回答 Xira runtime 发出的 human request。

```json
{
  "type": "human_response",
  "id": "in_hr_001",
  "request_id": "req_001",
  "source": {
    "channel": "websocket",
    "entrypoint_id": "websocket-default",
    "chat_id": "chat-1",
    "sender_id": "user-1"
  },
  "data": {
    "human_request_id": "hr_001",
    "kind": "approve",
    "actor": "user-1",
    "message": "同意执行",
    "idempotency_key": "in_hr_001"
  }
}
```

`human_response` 既可以绑定原始 `request_id`，也可以在请求外发送。请求外发送
时，`human_request_id` 和 `idempotency_key` 是幂等关键字段。

### 3.3 `control`

连接或请求控制消息。v0 最小集合：

| kind | 说明 |
|---|---|
| `ping` | 健康检查。 |
| `cancel` | 请求取消。实现可以先保留协议。 |
| `subscribe` | 订阅 event / outbound_message。实现可以按 transport 能力决定。 |

示例：

```json
{
  "type": "control",
  "id": "ctrl_001",
  "request_id": "req_001",
  "data": {
    "kind": "cancel",
    "reason": "user_cancelled"
  }
}
```

Transport 可以把 `ping` 映射成自己的帧类型，例如 WebSocket binding 可保留
`type: "ping"` 作为语法糖，但语义仍属于 `control`。

## 4. Channel Outbound

Outbound 指 Xira runtime / channel dispatcher 发给 channel transport / adapter 的
消息。

### 4.1 `ack`

Xira 已接收 inbound message，并给出初步处理结果。

```json
{
  "type": "ack",
  "id": "out_ack_001",
  "request_id": "req_001",
  "source": {
    "channel": "websocket",
    "entrypoint_id": "websocket-default"
  },
  "data": {
    "status": "accepted",
    "message_id": "client-msg-001"
  }
}
```

`status`：

| status | 说明 |
|---|---|
| `accepted` | 已开始处理，会有后续 event / final / interrupt。 |
| `ignored` | 已按策略忽略，例如未 mention 的群消息。 |
| `duplicate` | 已命中幂等去重，不会重复执行。 |

### 4.2 `runtime_event`

结构化 runtime 事件，面向 activity、debug、run inspector 和审计辅助展示。

```json
{
  "type": "runtime_event",
  "id": "out_evt_001",
  "request_id": "req_001",
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

要求：

- `runtime_event` 不等同于用户可读回答。
- 它可以高频、多次发送。
- 它应保留原始 `RuntimeEvent` 的结构，避免丢失 tool、delegation、HITL、
  child-run correlation。

### 4.3 `assistant_delta`

面向用户的 assistant 中间文本增量。它是 channel contract 概念，不是
WebSocket 私有概念。

```json
{
  "type": "assistant_delta",
  "id": "out_delta_001",
  "request_id": "req_001",
  "run_id": "run_xxx",
  "data": {
    "sequence": 1,
    "content": "我先看一下当前状态...",
    "content_format": "markdown"
  }
}
```

语义：

- 可选发送。channel 不支持 streaming 时可以不发。
- 同一 `request_id` 内可多次发送。
- `assistant_delta` 不是最终答案，客户端不应把它当 run completion。
- `sequence` 在同一 `request_id` 内单调递增。
- 内容是 append delta，不是完整内容替换；如果未来需要替换模式，应另行定义显式字段，不在 v0 中隐含。

### 4.4 `assistant_final`

一次请求的最终用户可见结果。正常情况下，同一 `request_id` 只发送一次。

```json
{
  "type": "assistant_final",
  "id": "out_final_001",
  "request_id": "req_001",
  "run_id": "run_xxx",
  "data": {
    "agent_id": "xira-assistant",
    "entrypoint_id": "websocket-default",
    "session_id": "conversation:abcd1234",
    "status": "completed",
    "content": "这是完整最终回答。",
    "content_format": "markdown",
    "usage": {},
    "verification": {}
  }
}
```

语义：

- `assistant_final` 表示 request-bound completion。
- 它可以包含完整 final content，即使之前已经发送过 delta。
- 如果 run 进入 waiting human，应该发送 `interrupt`，而不是伪造 final。
- 如果 run 失败，应该发送 `error` 或带失败 status 的 final，具体由 transport
  binding 定义；v0 推荐使用 `error`。

### 4.5 `interrupt`

Xira 暂停执行，等待人类输入或外部条件。

```json
{
  "type": "interrupt",
  "id": "out_interrupt_001",
  "request_id": "req_001",
  "run_id": "run_xxx",
  "data": {
    "status": "waiting_human",
    "reason": "approval_required",
    "human_requests": [
      {
        "id": "hr_001",
        "kind": "approval",
        "question": "是否允许执行该操作？"
      }
    ]
  }
}
```

`interrupt` 是 request-bound 的暂停结果。`human_requests[]` 直接对齐
`humanrequest.HumanRequest` 的 JSON 形状，问题文本字段是 `question`。后续可
通过 `human_response` 恢复。

### 4.6 `outbound_message`

Xira 主动向某个 channel / chat 发消息。它不一定由当前用户请求触发。

```json
{
  "type": "outbound_message",
  "id": "out_msg_001",
  "target": {
    "channel": "websocket",
    "entrypoint_id": "websocket-default",
    "chat_id": "chat-1",
    "chat_type": "direct",
    "sender_id": "xira"
  },
  "data": {
    "reason": "human_request.reminder",
    "content": "你有一个待确认事项。",
    "content_format": "markdown",
    "priority": "normal"
  }
}
```

语义：

- `outbound_message` 是 request-independent proactive output。
- 它可以没有 `request_id`，但必须有足够 target。
- channel adapter 负责实际投递、排队或降级。
- WebSocket 只能向已连接且匹配 target 的连接推送；如果目标未连接，需要
  offline queue 或外部 relay，这不属于 v0 必做。

### 4.7 `error`

请求或投递失败。

```json
{
  "type": "error",
  "id": "out_err_001",
  "request_id": "req_001",
  "data": {
    "code": "validation_failed",
    "message": "source.chat_id is required",
    "retryable": false
  }
}
```

标准错误码：

| code | 说明 |
|---|---|
| `bad_json` | transport binding 的 JSON 解析失败。 |
| `unsupported_type` | 不支持的 inbound type。 |
| `validation_failed` | 缺字段或字段冲突。 |
| `unauthorized` | 鉴权失败。 |
| `entrypoint_not_found` | entrypoint 不存在或不可用。 |
| `channel_conflict` | inbound channel 与 entrypoint/runtime channel 冲突。 |
| `agent_not_allowed` | 请求 agent 不在 allowlist。 |
| `run_failed` | runtime.RunAgent 或 flow run 失败。 |
| `delivery_failed` | outbound_message 投递失败。 |
| `internal_error` | 未分类服务端错误。 |

幂等窗口内的重复消息不是错误；transport 应返回 `ack` 且
`data.status == "duplicate"`，并且不得再次触发 run。

## 5. Request-Bound 与 Proactive Outbound

### 5.1 Request-Bound

一次 inbound `message` 的正常 outbound 序列：

```
ack -> runtime_event* -> assistant_delta* -> interrupt? -> assistant_final | error
```

规则：

- `ack` 最多一次。
- `runtime_event` 可以多次。
- `assistant_delta` 可以多次，也可以一次都没有。
- `assistant_final` 正常最多一次。
- `interrupt` 表示暂停，不是完成。
- `request_id` 必须贯穿同一次请求的所有 outbound。

### 5.2 Proactive Outbound

`outbound_message` 不依赖当前 inbound request，适用于：

- human request reminder
- scheduled follow-up
- monitor alert
- flow timeout notification
- agent 主动通知

规则：

- 可以没有 `request_id`。
- 必须有 `target.channel`、`target.entrypoint_id`、`target.chat_id`。
- 如果 transport 不支持主动投递，应明确降级为 queued、ignored 或
  delivery_failed。

## 6. 幂等、顺序和并发

### 6.1 幂等

推荐 inbound message 去重键：

```
source.entrypoint_id + ":" + source.message_id
```

如果 `source.message_id` 为空，可退化为 inbound `id`。二者都为空时无法提供
幂等保证。

命中重复键时，transport 返回 `ack(status=duplicate)`，而不是 `error`。

### 6.2 顺序

同一 `request_id` 内，transport 应尽量保持发送顺序。不同 `request_id` 的
outbound 可以交错，客户端必须按 `request_id` 归属。

### 6.3 并发

同一 transport 连接可以承载多个并发 request。Channel Contract 不要求连接级
串行化，只要求 correlation 完整。

## 7. Channel Capability

不同 channel 能力不同。Contract 允许 adapter 降级，但必须显式。

| 能力 | 说明 |
|---|---|
| `streaming_delta` | 支持实时展示 `assistant_delta`。 |
| `runtime_event_stream` | 支持展示或传输 runtime events。 |
| `interactive_human_response` | 支持在 channel 内回答 HITL。 |
| `proactive_outbound` | 支持 Xira 请求外主动投递消息。 |
| `offline_queue` | 目标离线时可以排队。 |

Channel adapter 应声明自己支持哪些能力。没有能力时的默认降级：

- 不支持 `streaming_delta`：buffer delta，只发送 `assistant_final`。
- 不支持 `runtime_event_stream`：只保留 run log / inspector，不发给用户 channel。
- 不支持 `interactive_human_response`：通过 HTTP/API/UI 处理 human response。
- 不支持 `proactive_outbound`：记录 delivery_failed 或交给外部 relay。

## 8. Existing Channel Mapping

本节只给出 Channel Contract 的语义映射摘要。当前实现状态、能力分类和 proactive
target model 见 `docs/architecture/xira-channel-adapter-mapping-v0.zh.md`。

### 8.1 WebSocket

WebSocket 是 Channel Contract 的第一个实时 transport binding。当前实现支持：

- `message` -> JSON text frame -> `runtime.TurnRequest`
- `runtime_event` -> `event` frame
- `assistant_final` -> `response` frame
- `interrupt` -> `interrupt` frame
- `error` -> `error` frame

`assistant_delta`、`outbound_message` 和 resume-over-WS `human_response` 是保留能力，
未在当前 WebSocket `ready.capabilities` 中广告。

WebSocket 语义细节见 `docs/architecture/xira-websocket-channel-v0.zh.md`。

### 8.2 CLI / TUI

当前 CLI 是 final-only：

- `assistant_final` -> stdout final response / `--json` 完整 `TurnResponse`
- `runtime_event` -> `--json` 或 run log，默认不流式展示
- `interrupt` -> 通过 `xira human ...` 命令处理，不在 `agent run` 内联 prompt
- `assistant_delta` / `outbound_message` -> 保留

### 8.3 XiraGarden

当前 XiraGarden 是 final-only + runtime-event-stream：

- `assistant_final` -> `POST /api/v1/channels/xiragarden/messages` 返回的 final response
- `runtime_event` -> `WS /api/v1/channels/xiragarden/events`
- `assistant_delta` -> 目标为聊天区流式文本，当前未实现
- `interrupt` -> 目标为 HITL panel，当前依赖 human request APIs
- `outbound_message` -> 目标为 notification / 主动消息，当前未实现 dispatcher

### 8.4 Feishu / iLink

Feishu/iLink 的 channel runner 当前保持 final-only：

- `assistant_delta` -> buffer，不逐条发送
- `assistant_final` -> 平台文本/卡片消息
- `runtime_event` -> run log / inspector，不默认发到群
- `interrupt` -> 目标为平台消息或外部 approval UI 链接，当前未实现
- `outbound_message` -> 平台 send API 具备基础能力，但 generic proactive dispatcher
  当前未实现

若未来要把 delta 发到聊天平台，必须先定义节流、合并和撤回/编辑策略，避免刷屏。

### 8.5 Future Channel Runners

未来新增 channel runner 时，只应处理平台连接和消息归一化：

- inbound 平台消息 -> Channel Inbound Contract
- Channel Outbound Contract -> 平台发送能力

不要把 runtime routing、session identity、HITL 语义重新实现在 runner 内。

## 9. Implementation Notes

### 9.1 中性类型位置

Channel inbound/outbound 的中性类型位于：

```
apps/xira/internal/channel
```

`internal/channel` 可以被 API server、CLI 和 channel runner 共同使用，但不引用
runtime 或 vendor SDK。具体 transport 的连接生命周期、鉴权、重试和平台投递仍留在
API server 或对应 runner 内，不下沉到 common layer。

### 9.2 Dedupe 位置

当前去重实现位于：

```
apps/xira/internal/channelrunner/dedupe
```

如果 WebSocket API 或其他 inbound transport 也要复用，应移动到中性位置：

```
apps/xira/internal/channel/dedupe
```

或：

```
apps/xira/internal/messagededupe
```

## 10. Non-Goals

- 本 contract 不要求所有 channel 立刻支持 streaming delta。
- 本 contract 不要求当前 WebSocket slice 支持 `assistant_delta`、resume-over-WS
  `human_response` 或 proactive `outbound_message`。
- 本 contract 不重构 Feishu/iLink runner。
- 本 contract 不引入 `channelrunner/websocket`。
- 本 contract 不定义具体 UI 组件。

## 11. Acceptance Checklist

- Channel Inbound 类型已定义：`message`、`human_response`、`control`。
- Channel Outbound 类型已定义：`ack`、`runtime_event`、`assistant_delta`、
  `assistant_final`、`interrupt`、`outbound_message`、`error`。
- `assistant_delta` 是通用 channel 语义，不是 WebSocket 私有概念。
- `assistant_final` 是 request-bound final output，正常每个 `request_id` 一次。
- `outbound_message` 是 request-independent proactive output，必须带 target。
- 文档说明 `channel` 是 runtime 身份，不是透明 transport。
- 文档说明 WebSocket、CLI/TUI、XiraGarden、Feishu/iLink 的映射方式。
- 文档不要求实现 `channelrunner/websocket`。
