# Xira Channel Adapter Mapping v0

> 本文是 `docs/architecture/xira-channel-contract-v0.zh.md` 的实现映射说明，
> 用于把现有 CLI、WebSocket、Feishu 和 iLink surface 对齐到
> Channel Inbound/Outbound Contract v0。它描述当前行为、目标映射和降级策略；
> 不要求一次性重写所有 channel runner。

## 1. 结论

Xira 的 channel outbound 语义统一为：

| Contract outbound | 含义 |
|---|---|
| `ack` | 已接受、忽略或命中重复请求。 |
| `runtime_event` | run/activity/inspector 使用的结构化事件。 |
| `assistant_delta` | 面向用户的增量文本，可选。 |
| `assistant_final` | 一次 request-bound 的最终用户可见结果。 |
| `interrupt` | run 暂停，等待 human response 或外部条件。 |
| `outbound_message` | request-independent proactive message。 |
| `error` | 请求处理或投递失败。 |

当前 Xira runtime 仍以 `runtime.TurnResponse` 和 `runtime.RuntimeEvent` 为主要输出。
#15 的边界是先建立 mapping 和小型中性类型，不把所有 transport 改成同一套
dispatcher。

## 2. 中性代码接口

中性类型放在：

```
apps/xira/internal/channel
```

该包现在定义：

| 类型 | 用途 |
|---|---|
| `OutboundType` | `ack` / `runtime_event` / `assistant_delta` / `assistant_final` / `interrupt` / `outbound_message` / `error`。 |
| `OutboundEnvelope` | transport-neutral outbound 信封，包含 `source`、route `target`、可选 typed `recipient`、`request_id`、`run_id`、`correlation` 和 `data`。 |
| `Capability` / `CapabilitySet` | channel adapter 能力声明，例如 `streaming_delta`、`proactive_outbound`、`typed_recipient_outbound`。 |
| `OutboundEmitter` | 小型接口：声明 capabilities，并发送一个 `OutboundEnvelope`。 |

约束：

- `internal/channel` 不引用 `runtime`，避免 runtime/channel 循环依赖。
- 具体发送仍由 API server、CLI 或 `channelrunner/*` 持有。
- Vendor SDK、连接生命周期、重试和鉴权继续留在对应 adapter/runner。

## 3. 当前能力分类

| Surface | 当前入口 | 当前分类 | 当前 outbound | 未实现 / 保留 |
|---|---|---|---|---|
| CLI | `xira agent run` | final-only | `assistant_final` 映射为 stdout final response；`--json` 输出完整 `TurnResponse`。 | `assistant_delta` streaming、inline `interrupt` prompt、proactive `outbound_message`。 |
| TUI | 无内置 TUI command | not implemented | 无。 | 若未来新增 TUI，按 CLI + HTTP/WebSocket contract 实现。 |
| WebSocket | `GET /api/v1/channels/websocket/messages` | final-only + runtime-event-stream + interactive-human-response | `ack`、`event`(binding of `runtime_event`)、`response`(binding of `assistant_final`/无 recipient 的 proactive resume)、`interrupt`、`error`；structured `human_response` 支持 current sender。 | `assistant_delta`、owner/typed-recipient 私信。 |
| Feishu | `channelrunner/feishu` | final-only + proactive-outbound | `assistant_final` 发到 `target.chat_id`；`outbound_message` 可发到 typed `open_id` / `user_id` / `union_id` recipient。 | delta 默认 buffer/drop；`runtime_event` 只进 run log；owner 双向 HITL 尚未实现。 |
| iLink | `channelrunner/ilink` | final-only + proactive-outbound | `assistant_final` 通过 `SendText` 或 `Push`；`outbound_message` 可使用 typed user recipient。 | delta 默认 buffer/drop；`runtime_event` 只进 run log；owner 双向 HITL 尚未实现。 |

## 4. Surface Mapping

### 4.1 CLI / TUI

| Contract outbound | CLI 当前行为 | TUI 目标行为 |
|---|---|---|
| `ack` | 不单独输出；命令同步执行。 | 可显示 accepted/loading state。 |
| `runtime_event` | 默认不输出；`--json` 可通过 `TurnResponse.events` 查看。 | Activity panel 或 verbose stream。 |
| `assistant_delta` | 未实现；未来可逐段写 stdout。 | 主消息区增量渲染。 |
| `assistant_final` | 默认打印 `final_response`；`--json` 输出完整 run response。 | 最终消息态和 completion marker。 |
| `interrupt` | 当前通过 `xira human list/show/approve/deny/cancel` 处理，不在 `agent run` inline prompt。 | HITL prompt / approval panel。 |
| `outbound_message` | 不支持。 | 本地 notification 或消息流，需订阅/队列。 |
| `error` | Cobra command error / stderr。 | Error panel。 |

CLI v0 保持 final-only。若要支持 `assistant_delta`，必须先让 runtime/model 层产生
增量，而不是在 CLI 里伪造 delta。

### 4.2 WebSocket

WebSocket binding 已在 `docs/architecture/xira-websocket-channel-v0.zh.md` 定义。
当前 PR #17 合入后的能力是：

| Contract outbound | WebSocket frame |
|---|---|
| `ack` | `ack` |
| `runtime_event` | `event` |
| `assistant_final` | `response` |
| `interrupt` | `interrupt` |
| `error` | `error` |
| inbound `human_response` | exact request/correlation + current live ChatKey connection；commit 后异步 resume |

`assistant_delta` 和 `outbound_message` 是 contract-defined，但当前 WebSocket
transport 不在 `ready.capabilities` 广告。`human_response` 已广告；它不接受
客户端自报 sender/chat/entrypoint，只支持 untyped current sender，owner fail closed。

### 4.4 Feishu

当前 Feishu runner 行为：

- inbound platform message -> `runtime.TurnRequest`，`Context.Channel = "feishu"`。
- `assistant_final` -> 优先卡片，失败后文本。
- 空 final response 不发送消息。
- `runtime_event` 保留在 runtime event/run log，不默认刷到群。

目标 mapping：

| Contract outbound | Feishu 策略 |
|---|---|
| `assistant_delta` | v0 buffer/drop；未来如启用，必须按时间窗口合并，避免刷屏。 |
| `assistant_final` | 卡片或文本。 |
| `runtime_event` | Run Inspector / activity，不默认进聊天。 |
| `interrupt` | 平台消息或卡片，包含 human request 摘要和外部 approve 链接；当前未实现。 |
| `outbound_message` | 使用 Feishu send API，target 至少需要 `entrypoint_id` + `chat_id`。 |

### 4.5 iLink

当前 iLink runner 行为：

- inbound OpenIlink message -> `runtime.TurnRequest`，`Context.Channel = "ilink"`。
- `assistant_final` -> 若原消息有 `context_token`，用 `SendText` 回复；否则用
  `Push`。
- `runtime_event` 保留在 runtime event/run log，不默认进聊天。

目标 mapping：

| Contract outbound | iLink 策略 |
|---|---|
| `assistant_delta` | v0 buffer/drop；未来如启用，必须按时间窗口合并。 |
| `assistant_final` | `SendText` 或 `Push`。 |
| `runtime_event` | Run Inspector / activity，不默认进聊天。 |
| `interrupt` | 文本或卡片化消息，包含 human request 摘要和外部 approve 链接；当前未实现。 |
| `outbound_message` | 使用 `Push`，target 需要 `entrypoint_id`、account/bot 选择和接收人。 |

## 5. Proactive `outbound_message` Target Model

通用 target 先复用 `channel.InboundContext` 字段：

| 字段 | 说明 |
|---|---|
| `channel` | 必填。`websocket`、`feishu`、`ilink` 等。 |
| `entrypoint_id` | 强烈建议。决定凭证、默认 agent、channel policy。 |
| `account` | 多账号 channel 的账号或租户维度。 |
| `channel_app_id` / `bot_id` | 平台 app/bot 选择。 |
| `chat_id` | 目标会话。Feishu 是 chat id；iLink 可是 room/user conversation。 |
| `chat_type` | `direct`、`group`、`thread` 等。 |
| `topic_id` | 平台 thread/topic。 |
| `sender_id` | 请求外消息中可表示 Xira/bot，也可作为 recipient fallback。 |
| `raw` | 平台专属补充字段，例如 iLink `context_token`。不要放 secrets。 |

Channel-specific 约束：

| Channel | 最小 target | 投递说明 |
|---|---|---|
| WebSocket | `channel=websocket` + `entrypoint_id` + `chat_id` | 只能投递给当前已连接且匹配 target 的 client；保留普通 `proactive_outbound` 供 resume 使用，但不声明 `typed_recipient_outbound`，带 recipient 时 fail closed；离线队列不属于 v0。 |
| Feishu | `channel=feishu` + `entrypoint_id` + `chat_id` | 使用 entrypoint 凭证发送 chat message。 |
| iLink | `channel=ilink` + `entrypoint_id` + account/bot + recipient | 有 `context_token` 时可 reply；request-independent 场景通常用 `Push`。 |
| CLI/TUI | `channel=cli` + local session | v0 不支持；未来可映射成本地 notification。 |

## 6. Delta Throttling Rules

Feishu/iLink 这类聊天平台默认不实时发送 `assistant_delta`。后续启用前必须同时定义：

- 合并窗口，例如 500ms-2000ms 内的 delta 合并为一条。
- 最小字符阈值，避免一两个 token 刷屏。
- 最大更新频率。
- 是否编辑上一条消息，还是追加新消息。
- 失败时是否只发送 `assistant_final`。

在这些策略落地前，Feishu/iLink adapter 应视为 final-only。

## 7. Implementation Checklist

- 新 channel 先声明 `CapabilitySet`。
- 不支持 `streaming_delta` 时，只发送 `assistant_final`。
- 不支持 `runtime_event_stream` 时，runtime events 仍必须留在 run/event store。
- 不支持 `interactive_human_response` 时，human response 走 HTTP/API/UI。
- 不支持 `proactive_outbound` 时，`outbound_message` 必须记录为 unsupported /
  delivery_failed，不能假装已投递。
- 不支持 `typed_recipient_outbound` 时，带 typed recipient 的消息必须拒绝，不能忽略 recipient
  后退化成 target chat 投递；能力必须在 Manager 选出的最终 runner 上检查，不能只看 fleet 并集。
- Vendor-specific connection management 留在对应 runner；common layer 只处理 contract
  envelope、capability 和 target 语义。
- `target.entrypoint_id` 存在时必须精确选择 runner；同 channel 多 runner 时不得取第一个。
- 平台用户私信使用 typed `recipient`；不得把 sender ID 当 `chat_id`，也不得按显示名猜 ID type。
