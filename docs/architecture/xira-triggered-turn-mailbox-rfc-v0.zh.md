# Xira Triggered Turn 与 per-chat-key durable mailbox RFC v0

- **状态**：Proposed
- **日期**：2026-07-27
- **Gate Issue**：[#204](https://github.com/xiramesh/xira/issues/204)
- **Epic / Milestone**：[#202](https://github.com/xiramesh/xira/issues/202) / Managed Execution v0
- **目标分支**：`milestone/managed-execution-v0`

---

## 0. 一句话结论

后台 Execution 完成后，Xira 必须把一个**结构化、不可由模型伪造的 Triggered Turn**持久化到
原 conversation 的 mailbox；它不能伪装成用户消息，也不能进入 SteeringQueue。用户消息继续拥有
steering 语义，completion 只会排队，等当前 turn 结束后再创建一个新的 Agent Run。

```text
用户消息 ───────────────→ User ingress ──┐
                                         ├─→ per-chat-key Turn Coordinator ─→ Agent Loop
Execution terminal → durable mailbox ────┘
                         │
                         └─ active 时只排队，不 steering
```

本 RFC 冻结“怎样进入下一轮对话”的契约；Execution completion 从 producer outbox 到 mailbox 的
可靠交接、claim/ack 精确状态名和事务边界由 #205 冻结，worker/terminal/artifact 契约由 #206 冻结。

---

## 1. 为什么需要这个 Gate

Managed Execution v0 要支持：命令超过同步等待窗口后继续运行，完成时 Xira 自动回到原 conversation
继续处理结果。这个“自动回来”不是普通 IM 入站消息，当前代码也没有一个安全入口。

如果直接复用现有 Router：

1. completion 撞上 active turn 时会进入 `SteeringQueue`；
2. Agent Loop checkpoint 看到 pending steering 后返回 `ErrSteered`；
3. `ChatKeySession` 取消当前 spawned children、清空 turn 状态，并用新字符串重跑；
4. 系统 completion 因而会打断用户正在进行的工作。

这不是我们要的行为。用户插话可以抢占；后台完成只能排队。

---

## 2. 当前代码事实

以下结论基于 `milestone/managed-execution-v0` 起点提交 `315cb6a1` 的源码，引用符号名而非易漂移行号。

### 2.1 Runtime 只有“用户字符串”入口

- `runtime.TurnRequest` 只有 `Message string`，没有 trigger/source union。
- `Service.RunAgent` 把 `req.Message` 直接放进 `channel.InboundEnvelope.Content`。
- DeepSeek 路径把它构造成 `role=user`；ADK 路径同样以 user content 启动。
- `sessionMessagesForRun` 把它持久化为 `role=user + kind=message`。

因此，把 completion 格式化成一段字符串再调 `RunAgent`，在审计、模型上下文和 session history 中都会
撒谎：系统事件会被记录成“用户说过的话”。

### 2.2 Router 的 active 分支就是 steering

- `progress.Router.Handle` / `Route` 在 `entry.active=true` 时调用
  `entry.steering.Enqueue(msg)`。
- `SteeringQueue` 明确只承载 user interjection。
- `ChatKeySession.runTurn` 在 `ErrSteered` 后从 SteeringQueue 取字符串，取消 spawned children，并用它
  重启 Agent Run。
- `Router.markComplete` 只把 active 翻回 false；它不会从另一个 FIFO 领取“当前 turn 结束后再做”的工作。

所以当前 Router 能表达“插话”，不能表达“排在本轮之后”。

### 2.3 当前调度状态不是 durable，也不是全局唯一

- Feishu、iLink、WebSocket runner 各自持有一个 `progress.Router`。
- Router entries 只存在内存，重启后全部消失。
- `ChatKey=(Channel, ChatID, SenderID)` 只在某个 runner 内唯一；跨 entrypoint 使用时还必须包含
  `EntrypointID`。
- `ChatKeySession` 由 runner 针对每条 inbound 临时构造，其中的 `SendFinal` / `OnRawEvent` closure
  捕获了当时的 SDK message、connection 或 account。后台 dispatcher 没有这些临时 closure。

因此 durable mailbox 不能只是给 Router 再加一个 `[]string`。

### 2.4 已有可复用的恢复与 outbound mechanics

- `SessionScope` 已持久化 entrypoint、channel、account、chat、sender 等 conversation identity。
- `inboundContextFromScope` 已能重建 `channel.InboundContext`。
- `channelrunner.Manager.Emit` 已能把 `OutboundEnvelope` 路由到准确 entrypoint/channel runner。
- HITL resume 已使用上述路径把 final 回送原 conversation。

这些 mechanics 可以复用，但当前 HITL resume delivery 是 best-effort：失败只记日志。它不是 durable
mailbox，也没有 completion 的 retry/ack 语义。

### 2.5 #194 / #197 是依赖，不是已经存在的能力

- #194 尚未完成 Agent Loop Core 抽取。#204 需要的是“同一个 core、不同 typed input adapter”，而不是
  Cron 的 fresh-session Scheduled Turn 语义。
- #197 尚未完成 typed proactive direct。Execution continuation 回原 conversation，不投 Cron Principal；
  只能复用 `OutboundEnvelope` / exact-entrypoint routing 等 mechanics。

---

## 3. 需求与非目标

### 3.1 功能需求

1. completion 使用结构化 Trigger，而不是伪造 user text。
2. 同一 conversation 任意时刻最多一个 active Agent turn。
3. completion 到达 active conversation 时 durable enqueue，不 steering、不取消当前 Run/child。
4. 当前 turn 结束后自动领取 queued trigger，创建新的 run_id。
5. Triggered Turn 默认复用原 conversation session history，但重新解析当前 entrypoint/profile/policy。
6. Xira crash/restart 后 queued trigger 不丢、不会并发双跑。
7. 最终回复通过持久 route 回到原 conversation，而不是依赖原 inbound callback 仍然存活。
8. entrypoint/身份/session 已失效时 fail closed，记录 suppress reason，绝不猜测新 recipient。

### 3.2 非功能需求

- **单活**：同一 mailbox key 的 active turn 数恒为 0 或 1；不同 key 可并行。
- **耐久性**：durable enqueue 成功返回后的 mailbox RPO 为 0。
- **恢复**：服务 ready 后 30 秒内重新扫描并调度无有效 lease 的 queued work。
- **空闲调度**：conversation 空闲时，正常负载下 queued trigger 应在 1 秒内开始 claim（不含 LLM 与
  channel delivery 时间）。
- **有界**：mailbox payload 不携带无界 stdout/stderr；只携带 bounded facts 与 artifact references。
- **可观测**：至少暴露 queued count、oldest queued age、claim retry、suppressed reason、triggered run ID。
- **安全**：trigger kind/payload 由 Kernel 生成；公开 HTTP/WS/user message 入口不能构造 trusted trigger。

### 3.3 非目标

- 不在本 RFC 选择 SQLite 表结构或 claim lease 的精确字段（#205）。
- 不定义 Execution worker、terminal state、artifact layout（#206）。
- 不实现 PTY/stdin/service execution。
- 不把 mailbox 做成通用 Kafka/EventBus。
- 不承诺 exactly-once 的用户可见投递。
- 不改 HITL resume 的现有语义；它可以以后迁移到同一 coordinator，但不是 #204 的前置条件。

---

## 4. 决策

### 4.1 增加一等、sealed 的 Triggered Turn 输入

Runtime 必须区分两类输入：

```text
User Turn      = 用户真实发送的消息
Triggered Turn = Kernel 因可信系统事实创建的结构化触发
```

v0 的 trigger kind 只允许：

```text
execution.completed
execution.failed
execution.timed_out
execution.lost
```

建议的逻辑形状如下；最终 Go 文件和字段名可在 implementation issue 中微调，但语义不得退化成
`Message string`：

```go
type TriggeredTurn struct {
    SchemaVersion  int
    ContinuationID string
    Kind           TriggerKind
    OccurredAt     time.Time
    Conversation   ConversationRef
    Correlation    TriggerCorrelation
    Execution      ExecutionTerminalRef
}

type ConversationRef struct {
    EntrypointID string
    SessionID    string
    OriginRunID  string
    ScopeDigest  string
    Route        DeliveryRouteRef
}

// DeliveryRouteRef 是由 channel/runtime 从 InboundContext 投影出的
// durable、最小、可验证路由；不是任意 Raw metadata 的复制品。
type DeliveryRouteRef struct {
    Channel  string
    Account  string
    ChatID   string
    SenderID string
}

type TriggerCorrelation struct {
    ParentRunID string
    ToolCallID  string
    ExecutionID string
}

type ExecutionTerminalRef struct {
    ExecutionID string
    Status      string
    ResultRef   string
    ArtifactRefs []string
}
```

Trigger payload 不接受任意 prompt/instruction。模型可见文字由 Runtime 根据 sealed struct 生成；用户即使
发送一段完全相同的文字，内部仍是 `User Turn`，不能获得 trusted-trigger 权限。`DeliveryRouteRef` 只保存
channel 声明并验证过的最小路由字段；不得把整个 `InboundContext.Raw` 复制进 mailbox。确需 iLink
`context_token` 等 channel-private delivery metadata 时，由 channel adapter 定义白名单 projection，并受
原 Run/SessionScope retention 与 secret-handling 契约约束。

### 4.2 使用独立 Runtime adapter，复用 #194 的 Agent Loop Core

保留普通聊天入口：

```go
RunAgent(context.Context, TurnRequest) (TurnResponse, error)
```

新增内部入口：

```go
RunTriggeredAgent(context.Context, TriggeredTurnRequest) (TurnResponse, error)
```

两个入口分别验证/组装输入，然后调用 #194 抽取出的同一个私有 Agent Loop Core。不得复制第二套
generate/tool/session/verification 逻辑。

`RunTriggeredAgent` 不是公开 user API，也不直接负责 channel delivery；它返回并持久化 TurnResponse，
coordinator/outbox 再按 delivery 状态发送 final。

### 4.3 Trigger 在 session/history 中不是 user message

Triggered Turn：

- 创建新的 `run_id`，不复活产生 Execution 的旧 Run；
- 使用 `ConversationRef.SessionID` 恢复同一 conversation history；
- session store 增加 `MessageKindTrigger`，保留结构化 trigger metadata；
- 不得通过 `sessionMessagesForRun` 写成普通 `role=user + kind=message`；
- provider adapter 可以受协议限制把 Runtime 生成的 canonical trigger block 映射成模型输入，但 provider
  wire role 不是身份边界；可信性来自内部 sealed type 和不可公开调用的入口；
- 后续 history hydrate 必须能识别 trigger kind，不能把它错误恢复成 assistant 文本。

当前 entrypoint/profile/policy 在 Triggered Turn 开始时重新解析。权限采用“只收紧不放宽”：当前 policy
与原 Run 持久化的 execution policy 取交集；entrypoint 被禁用、原 sender 不再获授权、route identity
不再匹配时 suppress，不能借后台 completion 绕过新策略。

### 4.4 durable mailbox 与 SteeringQueue 永久分离

```text
SteeringQueue
  - 内容：用户插话
  - 生命周期：当前进程/当前 active turn
  - 效果：允许 ErrSteered、取消 child、重启 turn

Triggered Turn mailbox
  - 内容：Kernel trigger
  - 生命周期：跨 turn、跨 Xira restart
  - 效果：enqueue-after-active，绝不直接触发 ErrSteered
```

不允许一个 queue 用 flag 同时承担两种语义。混在一起会让调用者迟早忘记检查 flag，再次制造系统事件
打断用户 turn 的 silent bug。

### 4.5 新增唯一的 per-conversation Turn Coordinator

每个 entrypoint runner 需要一个长生命周期 coordinator，成为该 runner 下 per-conversation active 状态的
唯一权威。mailbox dispatcher 不能绕过 coordinator 直接调用 `RunTriggeredAgent`。

全局 mailbox key 为：

```text
(EntrypointID, Channel, ChatID, SenderID)
```

`EntrypointID` 必须存在，因为当前 `ChatKey` 只在某个 runner 内唯一。SessionScope 继续保存 Topic/Space
等 conversation 维度；Route snapshot 只保存 Account 与 channel adapter 明确白名单化的 delivery metadata，
这些字段不额外改变 per-chat-key 串行粒度。

coordinator 接受两个入口：

```text
SubmitUser(message)
NotifyTriggerAvailable(mailboxKey)
```

- `SubmitUser` 保留现有 Router steering 语义。
- `NotifyTriggerAvailable` 只唤醒 drain；实际 trigger 必须先 durable enqueue。
- active turn 结束时 coordinator 必须回调 drain mailbox。当前 `Router.markComplete` 没有这个能力，后续
  implementation 需要显式 completion hook，不能靠轮询 Router.IsActive 拼竞态。
- coordinator 只持 active/steering 等瞬时状态；queued trigger 的真相只在 durable mailbox。

### 4.6 顺序与优先级

| 场景 | 决策 |
|---|---|
| idle，只有 user message | 立即启动 User Turn |
| active，user message 到达 | 保留现有 steering；用户可以抢占 |
| idle，只有 trigger | claim 最老的 queued trigger，启动 Triggered Turn |
| active，trigger 到达 | durable enqueue；不 steering、不取消当前 Run/child |
| idle，user 与未 claim trigger 同时竞争 | user 优先；trigger 保持 queued |
| Triggered Turn 已启动后 user 到达 | user 仍可 steering；trigger 不能提前 ack，失败/steered 后由 #205 决定 requeue |
| 多个 trigger | 按 durable store 分配的 per-key sequence FIFO；不在 v0 合并/coalesce |

用户优先意味着持续活跃的 conversation 可能让 completion 等待。v0 不通过强行插嘴解决 starvation；必须
暴露 `oldest_queued_age`，由用户空闲后继续处理。未来如需 fairness policy，单独设计，不能偷偷改变
用户 steering 契约。

### 4.7 channel delivery 使用持久 route，不复用 inbound closure

Triggered Turn 没有仍然存活的 SDK callback、Feishu message object 或 WebSocket request closure。因此：

- 从 `ConversationRef.Route`、persisted SessionScope 与原 Run 的白名单 delivery metadata 恢复
  `InboundContext`；
- 用 exact `EntrypointID` 调 `channelrunner.Manager.Emit`；
- final 发回原 ChatID/Sender route，不自动改投当前 owner；
- v0 Triggered Turn 不发送 live progress，只在 Run 内持久化 RuntimeEvents，并在完成后发送 final；
- WebSocket 没有 live connection 时 `Emit` 必须显式失败，交给 #205 delivery retry/suppression，不能 silent nil；
- 复用 #197 的 outbound mechanics，不复用 Cron “固定投 Principal”语义。

### 4.8 conversation/session/身份失效时 fail closed

claim trigger 前必须重新验证：

1. entrypoint 仍存在且 enabled；
2. entrypoint 的 channel/account 与持久 route 相容；
3. 原 sender 在当前 policy 下仍获授权；
4. 原 session metadata 存在且 scope 与 `ConversationRef` 匹配；
5. route 能被 exact entrypoint 唯一解析。

任一不满足都 suppress，并保存 reason；禁止猜测、fallback 到 owner、按 channel 模糊选择 runner，或发送给
新绑定身份。

当前仓库没有 conversation reset/delete API，也没有 session generation。v0 不虚构一个不存在的操作；
契约规定：未来 reset/delete 必须生成新的 conversation generation/tombstone，旧 generation 的 queued
trigger 一律 suppress。实现 #204 时需要保留 generation 字段/validator 扩展点，但不因本 Gate 顺带新增
完整 reset 产品功能。

### 4.9 #204 与 #205 的耐久边界

#204 冻结以下 mailbox invariant：

- enqueue 成功后 durable；
- `ContinuationID` 唯一，重复 enqueue 返回原记录；
- 同一 mailbox key 最多一个有效 claim/active continuation；
- claim 有 lease，crash 后可恢复；
- Triggered Run 未可靠完成/ack 前不能从 mailbox 永久删除；
- handled、suppressed、retryable failure 必须可区分并审计。

#205 冻结以下实现细节：

- Execution outbox 与 mailbox 是否同库/同事务；
- queued/claimed/run-created/handled/final-delivered 等精确状态名；
- lease 时长、CAS、backoff、poison handling；
- `wait` observe 与 claim/ack 的关系；
- at-least-once dispatch + idempotent Run creation 的完整证明。

---

## 5. 端到端时序

### 5.1 completion 撞上用户 active turn

```text
Execution      Outbox/#205       Mailbox       Coordinator       User Run
    | terminal     |                |               |               |
    |------------->| enqueue        |               |               |
    |              |--------------->| durable       |               |
    |              |                |-- wake ------>| active=true   |
    |              |                |               | 不 steering    |
    |              |                |               |               | completes
    |              |                |<-- claim -----| active=false  |
    |              |                |               | RunTriggeredAgent
```

### 5.2 Xira 在 enqueue 后、Run 创建前 crash

```text
1. mailbox enqueue 已提交
2. dispatcher/coordinator crash
3. Xira restart：Router active 内存自然清空
4. mailbox recovery 扫描 queued / expired claim
5. coordinator 重新 claim 同一 ContinuationID
6. #205 的幂等 Run 创建保证不产生两个 continuation Run
```

### 5.3 用户抢占 Triggered Turn

```text
Triggered Turn active
  → user message 到达
  → 进入 SteeringQueue
  → Triggered Run 在 checkpoint 返回 ErrSteered
  → mailbox 不 ack；#205 release/requeue 原 continuation
  → User Turn 优先运行
  → User Turn 结束后再次 drain mailbox
```

这条路径意味着 Triggered Turn 是 at-least-once handling，不能假装外部工具副作用 exactly-once。后续实现
需要让 continuation Run creation 幂等，并在 PR 测试中显式覆盖 steering/requeue。

---

## 6. 失败模式与处理

| 失败点 | 不正确行为 | v0 处理 |
|---|---|---|
| enqueue 提交前 crash | 假装已投 mailbox | #205 outbox 保持 pending，重试 |
| enqueue 后、wake 前 crash | queued trigger 丢失 | restart recovery 扫描 durable mailbox |
| claim 后、Run 创建前 crash | 永久卡 claimed | lease expiry 后同 ContinuationID 重试 |
| Run 创建响应丢失 | 再创建一个 Run | continuation ID 唯一约束 + 幂等 Run 创建 |
| completion 撞 active user turn | 中断用户 | 只 enqueue，不进 SteeringQueue |
| user 抢占 Triggered Turn | 提前 ack 导致结果丢失 | 不 ack，release/requeue |
| entrypoint 被删/禁用 | fallback 到其他 runner | suppress: `entrypoint_unavailable` |
| sender 权限被收回 | 继续用旧权限运行 | suppress: `sender_unauthorized` |
| session/scope 不匹配 | 创建新会话或串话 | suppress: `conversation_unavailable` |
| WebSocket 已离线 | silent success | delivery failure 可重试/检查，不改 Execution terminal state |
| malformed/unknown trigger kind | 当普通文本喂模型 | reject/suppress: `unsupported_trigger` |
| mailbox backlog 超限 | 内存无限增长或 drop oldest | 显式 quota error；producer outbox 保留 retry，不 silent drop |

---

## 7. 备选方案

### A. 直接复用 SteeringQueue

**拒绝。** 它会触发 ErrSteered、取消 child、重跑当前 turn，语义与 enqueue-after-active 相反。

### B. 给 Router 加一个内存 `afterActive []func()`

**拒绝。** Xira restart 后丢失，closure 还会捕获已失效的 SDK connection/message/account。

### C. watcher 直接调用 `RunAgent("execution completed ...")`

**拒绝。** 绕过 per-chat-key 单活，把系统事件伪造成用户消息，并可能串 session/channel。

### D. 完成后只用 `Manager.Emit` 发一条固定通知

**拒绝作为 #202 目标。** 它能通知用户，但不会让 Agent 读取结果、继续推理和执行下一步。如果产品目标
收缩为“只通知、不继续 Agent”，可以另行删掉 #204/#205 的大部分范围；当前用户已明确需要自动继续。

### E. 复用 Cron Scheduled Turn

**拒绝语义复用，允许 core 复用。** Cron 使用 fresh session、固定 Principal 和独立 schedule identity；
Execution continuation 使用原 conversation session/route，不能互换。

### F. 引入 Kafka/Redis Streams/独立消息服务

**拒绝。** Xira v0 是单 daemon/local-first。需要的是 durable local mailbox + CAS，不是分布式基础设施。

---

## 8. 后续 contract test matrix

以下均属于 contract code，相关分支/case 必须 100% 覆盖：

| 测试 | 必须证明 |
|---|---|
| active user + completion | completion queued；无 ErrSteered；当前 child 不被取消 |
| idle completion | 创建新 run_id；复用原 SessionID/history |
| user/trigger idle race | 同 key 不并发；user 优先；trigger 仍 queued |
| two triggers same key | FIFO；一次只运行一个 |
| same continuation replay 100 次 | mailbox 一条、continuation Run 一个 |
| crash after enqueue | restart 后 trigger 仍被处理 |
| crash after claim | lease 恢复；无永久 claimed |
| user steers Triggered Turn | trigger 未 ack；User Turn 先完成；trigger 可重试 |
| user copies trigger text | 仍是 MessageKindMessage，不获得 trusted trigger kind |
| trigger history round-trip | MessageKindTrigger 不恢复成普通 user/assistant message |
| current policy tightened | 原 completion 不能扩大工具或 sender 权限 |
| entrypoint removed/ambiguous | suppress，不 fallback 到同 channel 其他 runner |
| group conversation | exact entrypoint/chat/sender；不投群内其他 sender |
| iLink account changed | route validation 失败可见；不猜 account |
| WebSocket offline | delivery error 可见、可重试；Execution 状态不变 |
| different chat keys | 可并行，互不 steering/claim |

测试数据必须来自真实 `BuildScope → persisted SessionScope → inboundContextFromScope` round-trip，不能手搓
“干净 ID”绕过 canonical sender/account/entrypoint 恢复链。

---

## 9. 影响与代价

### 正面

- Managed Execution 可以在不打断用户的前提下自动闭环。
- system trigger 与 user message 在审计和模型上下文中不再混淆。
- 同一调度入口以后可被 Cron/Webhook/HITL 借鉴，但 v0 不泛化实现。
- restart 后 completion 不依赖临时 goroutine/connection 存活。

### 负面

- Router/ChatKeySession 不再只是纯 inbound message 调度，需要抽出 coordinator completion hook。
- Runtime/session history 要增加 first-class trigger 类型和 hydrate 逻辑。
- channel final delivery 与 Run execution 分离，状态排障更复杂。
- 用户优先会让高活跃 conversation 的 completion 延迟；只能观测，不能强行插嘴。

### 中性

- 当前 ChatKey 不必改名，但 durable mailbox key 必须额外包含 EntrypointID。
- Triggered Turn v0 不提供 live progress；用户只看到最终 continuation 回复。

---

## 10. 与其他 Issue 的边界

### #194 Agent Loop Core

#194 的 core 必须允许至少两个 typed adapter：User Turn 与 Triggered Turn。Cron Scheduled Turn 仍可作为
第三个 adapter。#194 不得把 core 输入硬编码为 `Message string + fresh cron session` 二选一。

### #197 channel outbound

复用 exact-entrypoint `Manager.Emit`、`OutboundEnvelope` 和 route-local capability validation。Execution
continuation 的 Target 是原 conversation route，不是 Cron Principal，也不允许 channel-only 模糊 fallback。

### #205 completion outbox

负责 producer outbox → mailbox 的可靠交接、claim/ack/retry、幂等 Run 创建和 delivery state machine。

### #206 worker ownership

负责 Execution 何时 terminal、result/artifact 引用的可信来源，以及 restart 后 completed/failed/timed_out/lost
怎样产生 trigger。

---

## 11. 本 Gate 的 Accepted 条件

- [ ] 评审接受 User steering 与 Triggered mailbox 永久分离。
- [ ] 评审接受全局 mailbox key 包含 EntrypointID + ChatKey。
- [ ] 评审接受 `RunTriggeredAgent` 作为 #194 Agent Loop Core 的 typed adapter。
- [ ] 评审接受 Trigger 在 session/history 中不是普通 user message。
- [ ] 评审接受 user/trigger 的顺序、抢占和 starvation 策略。
- [ ] 评审接受 exact route 恢复、当前权限重验证和 fail-closed suppression。
- [ ] #205 已登记并引用本 RFC 的 mailbox invariants，后续由 #205 冻结精确 delivery state/CAS。
- [ ] #206 已登记并引用 TriggeredTurn 所需的 terminal/result/artifact references，后续由 #206 冻结精确字段。
- [ ] 本 RFC 由 Proposed 更新为 Accepted，并把最终结论回灌 #202。

在以上条件完成前，不创建 mailbox/TurnCoordinator/RunTriggeredAgent 的 implementation issue，不冻结物理
Store schema。
