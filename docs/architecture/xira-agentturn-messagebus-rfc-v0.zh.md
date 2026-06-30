# Xira AgentTurn + MessageBus 设计总纲 (RFC v0)

> **状态: Superseded / 历史背景。** 本文描述的是被后续 per-chat-key
> 架构取代的双 bus / Forwarder / WAL 设想。当前生产契约不是本文：
> 全局 EventBus 与 Forwarder 已删除，事件通过 context-carried
> per-chat-key sink 点对点投递；当前权威说明见
> `docs/architecture/xira-runtime-current-contract.zh.md`、`AGENTS.md` 和源码。

- **状态**: Superseded / 历史背景
- **里程碑**: [AgentTurn + MessageBus](https://github.com/xiramesh/xira/milestone/1)
- **Epic issue**: 待补（见本文末「Issue 清单」）
- **日期**: 2026-06-21
- **作者**: ai-daming（基于 2026-06-21 设计对话复盘）
- **背景文档**:
  - `xira-conversation-progress-feed-v0.zh.md`（progress feed 设计）
  - `xira-flow-v0.zh.md`（Flow v0 编排）
  - `xira-ilink-delegation-rca-2026-06-21.zh.md`（delegation 复盘）
  - `AGENTS.md` §1（Runtime 事件契约）

---

## 0. TL;DR

当前 Xira 的 agent 执行是**同步函数调用 + 磁盘状态机**模型，缺乏统一的执行单元抽象和消息总线。本 RFC 提议：

1. 引入 **`AgentTurn`** 作为全系统唯一的执行单元第一公民类型（envelope + payload）。
2. 引入 **`MessageBus`**（强类型消息、多订阅者、全双工）作为所有 turn / agent / channel / 观察者的统一通信管道。
3. 将 `delegate_agent`（同步 RPC）替换为 **`spawn_turn`**（基于 ADK `StreamingFunctionTool` 的异步 spawn）。
4. 用 6 个 Phase 渐进落地，全过程保持老路径可用（灰度共存）。

**核心原则**：缺口要补不要绕（AGENTS.md §2）。EventBus 的 best-effort drop、`assistant.final` 不发布、`runtimeEventBase` 散落——这些都是既有缺口，本 RFC 一并补齐。

---

## 1. 动机：为什么是现在

### 1.1 现状：五种机制缝合

当前系统里没有统一的执行单元和消息管道，信息传递散落在五个机制上：

| # | 机制 | 形态 | 承载什么 |
|---|---|---|---|
| 1 | IM 长轮询回调 | `client.Monitor(ctx, handler)` | 入站消息 |
| 2 | 同步 RPC | `runtime.RunAgent(ctx, TurnRequest)` | 入站 → runtime |
| 3 | 同步 RPC | `r.send(...)` / `client.SendText` | 出站最终回复 |
| 4 | EventBus | best-effort buffered chan + 订阅 | runtime 内部事实事件 → progress |
| 5 | 磁盘 join state | `runs/` + `delegations/*.output.json` | 父子 agent 跨进程接力 |
| 6 | context.Value | 隐式传参 | 单次 run 内部状态 |
| 7 | ADK runner 事件流 | `for ev := range run.Run()` | LLM 推理过程（局部） |

这导致：

- **"父在等子"系统不知道**——`RunChildAgent` 是同步阻塞，父子关系藏在调用栈里，外部不可见。
- **EventBus 管辖范围窄**——只覆盖 runtime 内部事实 → progress 投影，入站/出站/agent 间通信都不走它。
- **并发是假的**——`MaxParallel` 有计数器但父阻塞，实际串行。
- **用户中途插话不可能**——父卡死在子调用上，根本不在 checkpoint 上。
- **fact 事件 visibility 默认 false**——AGENTS.md §1.4 记录的陷阱：`Kind string` + switch 导致漏 case。

### 1.2 ADK v1.4.0 已有但我们没用的能力

Xira 已在用最新版 ADK v1.4.0（`go.mod` 已确认）。以下能力 ADK **原生支持**，但 Xira 没用上：

| 能力 | ADK 实现 | Xira 现状 |
|---|---|---|
| 一个 model turn 内多 tool 并行执行 | `base_flow.go:1031` 默认开 goroutine | 用同步 `functiontool`，无并发 |
| StreamingFunctionTool | `base_flow.go:1066` — ⚠️ fire-and-forget 仅 **Live API 路径**（`liveSess != nil`，即 `RunLive`）；Xira 走常规 `runner.Run()`（`liveSess == nil`），走的是 `base_flow.go:1108` 的 **else 分支：同步迭代 `RunStream` 直到结束**，不 fire-and-forget | 无 |
| HITL confirmation | 定义在 `agent/callback_context.go:196` `RequestConfirmation()`（`tool.go:203` 的 `confirmationTool.Run` 只是调用点之一） | 自建 `human.request` tool |
| Session 持久化 | `session/database/` Postgres/MySQL/Spanner | 自建 `runs/` 磁盘 |
| Agent 树（SubAgents） | `agent.go:228` `FindSubAgent` | 用 `delegate_agent` 动态委派 |

**结论**：ADK v1.4.0 的**并行 tool 执行**（`base_flow.go:1031`）对 Xira 直接可用。但 **StreamingFunctionTool 的 fire-and-forget 只在 Live API 路径生效**——Xira 走常规 `runner.Run()`，StreamingFunctionTool 在这里被**同步迭代消费**（`base_flow.go:1108` 的 else 分支），不会 fire-and-forget。因此 `spawn_turn` 的"不阻塞父"**不能直接靠 StreamingFunctionTool 实现**，必须靠 tool 函数内部主动早关闭流 + detach goroutine（见 §2.4 修正后的实现说明）。真正要自建的是 **MessageBus**、**AgentTurn 类型**、以及**在常规 runner 路径上的异步 spawn 机制**。

### 1.3 ADK 不提供、必须我们补的

- **全双工 MessageBus**：ADK 事件流是单向的（`runner.Run()` → `for ev := range`），只能消费不能注入。我们要的 bus 是任何 turn 都能 Publish、任何 turn 都能 Subscribe。
- **Turn 作为第一公民类型**：ADK 最接近的是 `InvocationContext` + `session.Event`，没有显式的 Turn 概念。
- **跨 agent 消息路由**：ADK session 是 per-agent 的，没有跨 agent 通信管道。

---

## 2. 核心设计决策

### 2.1 AgentTurn：唯一的执行单元第一公民（envelope + payload）

**决策**：所有执行单元（flow run、agent run、未来的 live turn / batch turn）统一为 `AgentTurn` 类型，用 `Kind` 字段区分载荷。

**不用继承（is-a），也不用简单组合（has-a），用 envelope + payload（sum type）**。

```go
// 唯一的第一公民类型
type AgentTurn struct {
    ID                AgentTurnID
    ParentAgentTurnID AgentTurnID    // 空 = 根 turn
    Kind              AgentTurnKind  // "flow" | "agent"
    Status            AgentTurnStatus
    StartedAt         time.Time
    EndedAt           time.Time
    // Session 归属（方案 Z，2026-06-22 确认）：
    //   - 指针允许 nil（系统维护 turn 等无 IM 触发身份的 turn）
    //   - InheritSession 控制子 turn 是否继承父的 session
    //     flow→agent 默认 true（保持 flow 编排下的会话连续性）
    //     agent→agent 默认 false（worker 用临时 session，对齐现有
    //     delegation.go:977 的 ephemeral_worker 语义，origin/main 行号）
    //
    //   nil 的传播语义（2026-06-22 PR #30 review 补充）：
    //   - 父 SessionScope=nil spawn 子：子也是 nil（nil 不因 spawn 变非 nil）。
    //     含义：系统维护 turn（无 IM 身份）spawn 的子，也保持无 IM 身份。
    //   - 父 SessionScope!=nil spawn 子 + InheritSession=true：子继承父的 SessionScope（深拷贝指针）。
    //   - 父 SessionScope!=nil spawn 子 + InheritSession=false：子 SessionScope=nil（worker 用临时 session，
    //     不挂在父的会话树下；但子的 MessageID/ChatID 仍从父继承，这是 IM 触发身份，不是 session 身份——
    //     两者是正交维度，不要混淆。见 §2.1 末尾"身份维度"）。
    //   - forwarder / WAL 遇 nil SessionScope：正常处理，nil 不影响消息投递（投递靠 AgentTurnID/ParentAgentTurnID，
    //     不靠 SessionScope）。SessionScope=nil 仅影响"这个 turn 挂在哪个会话树"的归属判断，用于 session 历史聚合。
    SessionScope      *fsession.SessionScope
    InheritSession    bool
    Payload           AgentTurnPayload  // sealed interface
}

// sealed: 每种 kind 一个 payload 实现
type AgentTurnPayload interface {
    isAgentTurnPayload()
}

type FlowPayload struct {
    Steps         []StepState
    CurrentStepID string
    // ... flow 特有
}
func (FlowPayload) isAgentTurnPayload() {}

type AgentPayload struct {
    LLMCalls   []LLMCallRecord
    ToolCalls  []ToolCallRecord
    FinalText  string
    // ... agent run 特有
}
func (AgentPayload) isAgentTurnPayload() {}
```

**为什么 envelope + payload**：

- bus / 状态机 / forwarder / WS 等控制层只看 envelope，不关心 payload → bus 消息类型统一。
- 父子关系统一在一个字段（`ParentAgentTurnID`），无论是 flow→agent 还是 agent→agent。
- 未来加 `LivePayload` / `BatchPayload` 零扩散（不改 envelope、不改控制层）。
- 符合 Go "favor composition over inheritance" 惯用法（sealed interface + type switch）。

**否决方案**：
- is-a 继承（`FlowTurn extends AgentTurn`）：身份漂移、序列化地狱、Go 不友好。
- 简单 has-a（`FlowTurn contains []AgentTurn`）：违反"统一第一公民"，bus 上两种类型，又回到散。

### 2.2 没有 SubTurn：子也是 AgentTurn

**决策**：不引入 `SubTurn` / `ChildTurn` 类型。子 agent 的 turn 和父 turn 是同一种类型，只多一个 `ParentAgentTurnID` 字段表达父子关系。

- routing / session / progress / audit 用同一套逻辑处理所有 turn。
- 不需要"子 turn 特殊路径"。
- 递归自然支持（子的子也是 `AgentTurn`，也有 `ParentAgentTurnID`）。

### 2.3 双 Bus：MessageBus（内容）+ EventBus（信号）

**决策**（2026-06-22 修订，原"单 MessageBus"废弃）：照搬 PicoClaw 验证过的**双 bus 结构**，按"内容 vs 信号"分界。

**为什么改双 bus**（修订理由，PR #41 review 后修正论据类型）：

> ⚠️ 本节前一版用"Xira 现状消费者不重叠"作为主论据，PR #41 review (W-1/W-2) 核实后**该现状论据不成立**——`OutboundMessage` 在 runtime 层目前是孤儿类型（真出站走 `r.send` RPC return，不经 bus），`session history` 是 turn 结束后整批写（`AppendAgentMessages`，崩溃窗口 inbound 丢）。**撤回现状论据。** 但双 bus 方向不撤，论据换成下面的"设计原则"——架构决策不该靠易变的现状消费者论证。

- **设计原则：内容 vs 信号是正交关注点。** "用户对话内容"（Inbound/Outbound，有业务载荷，频率低，不可丢）和"运行时事件信号"（turn 生命周期、tool 调用、progress，无业务载荷，频率差异大，可按优先级丢）是两个正交维度——可靠性策略、投递语义、消费者群都不同。把它们塞进一个 bus 要靠 `Reliable/Priority` 路由表强行分流，本质是单 bus 内部跑两套路径。物理分离让各自的实现简单、故障隔离、可靠性策略独立。
- **PicoClaw 验证过这个原则**（`pkg/bus` MessageBus + `pkg/events` EventBus），不是验证"某个现状消费者结构"。
- **现状会变，原则稳定。** 未来 Xira 可能有跨类消费者（如 CLI/WS 给用户看完整画面），但"内容 vs 信号正交"不因此改变——跨类消费者订阅两个 bus 比 single bus 内部分流更清晰。

**诚实标注现状**（不作为论据，仅说明差距）：
- `OutboundMessage` 在 runtime 层目前是孤儿（真出站走 RPC return）。Phase 2/3 要让它**真的进 MessageBus 被消费**——这是双 bus 落地的工作项，不是现状借口。
- `session history` 是 turn 结束后整批写。如果 inbound 内容要扛崩溃，MessageBus 需要自己的 WAL（或确认 turn 级崩溃可接受重发）。**这是 Phase 2 的开放设计点，不是"靠 session history 搪塞"。**

**对照 PicoClaw**（源码学习，独立于 #39——#39 是单 bus 立场，不背书双 bus；PicoClaw 的双 bus 是事实参考，非充分论据）：
- PicoClaw MessageBus（`pkg/bus`）：InboundMessage / OutboundMessage / OutboundMediaMessage / AudioChunk / VoiceControl —— **typed channel API**（`PublishInbound(msg)` / `InboundChan() <-chan InboundMessage`），无 Filter，阻塞投递。
- PicoClaw EventBus（`pkg/events`）：30+ kind（agent.turn.* / agent.llm.* / agent.tool.* / agent.subturn.* / channel.*）—— **统一 Event + ScopeFilter**，`Publish(ctx, Event)` / `PublishNonBlocking(Event)` 双 API。
- **关键警示**（W-4）：PicoClaw 的 spawn 父子结果传递**不走任何 bus**，走专用 `pendingResults` channel（`subturn.go:410`）——EventBus 只发 spawn 的 start/end 信号。我们的 §2.4 设计 spawn 结果走 EventBus，**与"内容 vs 信号"分界自相矛盾**（spawn 结果载荷是内容性质）。Phase 3 必须决策：spawn 结果走 MessageBus（内容）还是专用 channel（PicoClaw 式），不能默认走 EventBus。

#### 2.3.0a Bus-1：MessageBus（内容，typed API + WAL + 判重）

承载**有业务载荷的消息内容**（IM 边界）。类型少且固定，用 typed API（每类型一个 channel + Publish 方法），不用 sealed interface + Filter。

```go
// MessageBus 是内容 bus：用户对话内容（IM 边界）。
// typed API：每类型一个 Publish + channel，消费者按类型取。
// 投递：阻塞（publishPolicy.timeout=0），永不内存丢——内容不可丢。
// WAL：inbound 一进就落盘（先保留），见下方 D-1。
// 判重：复合 key (Channel, ChatID, MessageID)，不假设任何 channel 的 ID 语义。
// 无 Filter——类型即过滤。
type MessageBus interface {
    PublishInbound(ctx context.Context, msg InboundMessage) error   // 落盘 + 判重 + 投递
    PublishOutbound(ctx context.Context, msg OutboundMessage) error // 落盘 + 投递
    // 未来: PublishOutboundMedia / PublishAudioChunk / PublishVoiceControl
    InboundChan()  <-chan InboundMessage
    OutboundChan() <-chan OutboundMessage
    Close() error
}

// InboundMessage / OutboundMessage 不再实现 sealed Message interface（Phase 1 改动）。
// 它们是普通 struct，typed channel 承载。
```

**为什么 typed API 不用 sealed**：内容类型少（2 个，未来最多 5 个），typed channel 比 sealed interface + Filter 更直接、类型安全更强（编译期知道 channel 类型，不用 type switch）。PicoClaw 验证过。

**D-1：MessageBus 挂 WAL + 先保留再判重**（2026-06-22 决策，否定此前"无 WAL 靠 session history"）：

此前 RFC 写"内容靠 session history 持久化，MessageBus 无 WAL"——**此论据经 PR #41 review (W-1) 核实为假**：`AppendAgentMessages`（service.go:543）只在 turn 结束后整批写，崩溃窗口（LLM 推理几十秒）inbound 丢。

决策改为 **MessageBus 也挂 WAL**（跟 EventBus 同构），但**策略不同**：
- **先保留**：`PublishInbound` 一进就落盘（独立 fsync），不依赖 turn 完成。崩溃后 WAL 有 inbound 记录。
- **再判重**：channel 重发同一消息时，按复合 key `(Channel, ChatID, MessageID)` 判重——重复的 inbound 幂等吸收（不重复触发 turn）。
- **不假设 channel 能力**：Xira 是多 channel 架构（iLink/飞书/未来 Discord/Telegram...），**不同 channel 重发语义不同**（有的有重发，有的没有）。MessageBus 的可靠性**不能依赖任何 channel 的重发/ack 能力**——bus 自治保证不丢，channel 重发了 bus 判重，channel 不重发靠 WAL 兜底。这跟 EventBus 的 lifecycle WAL + CAS 幂等（附录 C.5）是**同构设计**：先持久化，再处理重复。

**为什么不只依赖 channel 重发**：reviewer 曾建议"核实 ilink ack 语义后定"——但这是**以偏概全**（拿一个 channel 的协议决定整个 MessageBus 可靠性）。channel 是可插拔的，每加一个 channel 要重新评估可靠性，不可持续。bus 自治 + 复合 key 判重是**正交于 channel 的**，一次设计覆盖所有 channel。

**成本**：每条 inbound 一次 fsync（SQLite WAL 模式毫秒级）。用户消息频率秒级（人打字速度），非高频，fsync 成本可接受。与"丢 inbound 导致用户重发、turn 乱序"的代价比，明确划算。

#### 2.3.0b Bus-2：EventBus（信号，统一 Event + Filter + WAL）

承载**无业务载荷的运行时事件信号**（agent 生命周期、tool、steering、HITL）。类型多（9+），用 sealed interface + Filter。

```go
// EventBus 是信号 bus：运行时事件（turn 生命周期、progress、HITL）。
// 统一 Event sealed interface + Filter（按 turn/parent 过滤）。
// 投递：lifecycle 阻塞（timeout=0 + WAL 落盘）/ progress 超时驱逐（timeout>0 + Warn）。
// WAL 只挂这里（lifecycle 信号落盘）。
type EventBus interface {
    Publish(ctx context.Context, evt Event) error          // lifecycle 阻塞 + WAL
    PublishNonBlocking(evt Event) PublishResult            // progress 非阻塞 + 驱逐
    Subscribe(filter Filter) (SubID, <-chan Event)
    Unsubscribe(SubID)
    Close() error
}

// Event 是 sealed interface（Phase 1 的 Message 改名 + 拆分后）。
// 原 11 个 Message 类型里，9 个进 EventBus（Content 的 Inbound/Outbound 移到 MessageBus）。
type Event interface {
    isEvent()
    ID()         string
    AgentTurnID() AgentTurnID
    ParentAgentTurnID() AgentTurnID
    Timestamp()  time.Time
    Reliable()   bool            // lifecycle = true（落 WAL + 阻塞）
    Priority()   EventPriority   // Critical > Important > Droppable
}

// 9 个 Event 类型（AgentTurnStarted/Completed/Failed/Canceled,
// HumanRequested/Responded, AssistantStatus, ToolCalled, ToolResult）
// 各自带 Reliable()/Priority() 路由标记，同 Phase 1 原设计。

// 特殊 Event（W-3，Phase 2 Step C 新增）：
// AssistantFinal —— forwarder 的 drain 控制信号。它不是 turn lifecycle
// （不驱动状态机），不是 progress（不渲染、不进 deliverable 队列、不发 IM），
// 是纯控制信号（触发 forwarder.drain()，见 forwarder.go:139）。
// 归 EventBus（信号），但 Reliable()=false, Priority()=Critical
// （不落 WAL——它不驱动跨进程状态机；但优先级高——drain 必须及时）。
// 这是"内容 vs 信号"分界下的第三类：控制信号。明确标注避免 Phase 2
// 实现时误归 lifecycle 或 progress。

// Filter 归 EventBus（MessageBus 不需要 Filter）。
type Filter struct {
    AgentTurnID    *AgentTurnID
    Kinds          []string
    IncludeChildren bool
}
```

**Phase 1 类型改动**（详见 §2.3 末尾"Phase 1 回头改"清单）：
- `Message` sealed interface → **删除**。拆成：Inbound/Outbound（普通 struct，进 MessageBus）+ `Event` sealed（9 类型，进 EventBus）。
- `MessageBus` interface → **拆**：新 `MessageBus`（内容，typed API）+ 新 `EventBus`（信号，Event + Filter）。
- `Filter` → **归 EventBus**。
- `MessagePriority` → **改名 `EventPriority`**（只有 Event 用）。

**为什么 sealed 仍保留（在 EventBus）**：EventBus 的 9+ 类型多，加新 kind 要强制显式决策 Reliable/Priority。sealed + source-scan 守卫（Phase 1 的 `sealed_exhaustive_test.go`）继续适用，只是 expected 集合从 11 改成 9。

#### 2.3.1 可靠性策略：EventBus 的 WAL（lifecycle 落盘）+ 优先级驱逐（内存 progress）

**决策**（2026-06-22 确认）：**两个 bus 都挂 WAL，但策略不同**（双 bus 分离的价值正在于此——可靠性策略独立）：
- **EventBus**：级别 4（WAL 持久化），只对 `Reliable()==true` 的 Event（lifecycle 类）落盘。进度类（`Reliable()==false`）永远 best-effort + log，不落盘。
- **MessageBus**：级别 4（WAL 持久化），**所有 inbound/outbound 都落盘 + 判重**（详见 §2.3.0a D-1）。**不依赖任何 channel 的重发能力**——bus 自治保证不丢。

**MessageBus WAL 决策历程**（诚实记录，防回归）：
- 初版（已 merge RFC）："无 WAL，靠 session history 持久化"——PR #41 review (W-1) 核实为假（`AppendAgentMessages` turn 结束后才写，崩溃窗口丢 inbound）。
- 中间版（PR #41 初稿）："Phase 2 开放设计点，候选 (a) 挂 WAL / (b) 接受丢"——搁置未定。
- 最终版（本决策，2026-06-22）：**挂 WAL + 先保留再判重**。否定 (b) 的理由：Xira 是多 channel 架构，channel 重发语义不一（有的有，有的没有），MessageBus 可靠性**不能依赖任何 channel 的 ack 能力**。bus 自治 + 复合 key `(Channel, ChatID, MessageID)` 判重，正交于 channel，一次设计覆盖所有 channel。

这同时满足两个诉求：
- **抗进程崩溃 / 抗订阅者崩溃**：lifecycle 事件（AgentTurnStarted/Completed/Failed、HumanRequested/Responded）落 SQLite WAL，进程重启后重放恢复，父 turn 不会因为 bus 丢消息而永远等不到子。
- **不被高发量进度事件拖垮**：`assistant.status` / `adk.event` / `tool.*` 等低价值高频事件不落盘，磁盘 IO 只承担每 turn 几条的 lifecycle 事件。

**不落盘的 Event**仍然走优先级驱逐（critical 抢 important 抢 droppable 的位子），满载时 droppable 先丢 + log.Warn，从不在 bus 层静默。

**阻塞语义按消息类型分两档**（统一 §2.3.1 与附录 C.4，2026-06-22 PR #30 review 修正）：
- **进度类 Event（`Reliable()==false`）**：Publish **非阻塞**。内存投递满载时按优先级驱逐 + log.Warn，永不阻塞调用方。理由：这类消息高频低价值（`assistant.status` / `adk.event` / `tool.*`），阻塞会拖慢 model turn。
- **lifecycle 类 Event（`Reliable()==true`）**：Publish **同步落盘（SQLite WAL fsync）后返回**。落盘失败返回 error，调用方决策（不吞错、不假装投递成功）。理由：这类消息低频高价值（每 turn 几条），同步 fsync 的延迟可接受；且"宁可报错也不丢 lifecycle"是硬契约。
- **MessageBus（内容）**：`PublishInbound`/`PublishOutbound` **永远阻塞投递（timeout=0）+ 落盘 WAL + 判重**（见 §2.3.0a D-1）。无驱逐——内容不可丢。满了等消费者（IM adapter 不会慢到拖垮生产者，且内容频率远低于 progress）。两个 bus 都挂 WAL，但 MessageBus 全保留+判重，EventBus 只 lifecycle 保留+progress 驱逐——**策略独立，正是双 bus 分离的价值**。

**为什么不用背压**：背压（满了阻塞 Publish）会让慢消费者（IM HTTP 卡顿）拖垮快生产者（LLM 推理），一路阻塞回 `recordEvent`，卡住 model turn——这是反模式。所以 EventBus 的进度类 Publish 永不阻塞。lifecycle 类的"阻塞"是**磁盘 IO 同步**（毫秒级、确定性），不是**消费者背压**（秒级、不可控）——两者性质不同，不能混为一谈。MessageBus 的阻塞是内容场景特有的（频率低、消费者快），不违反背压反模式。

#### 2.3.2 WAL 形态：独立 SQLite（glebarez/sqlite，纯 Go 无 cgo）

**驱动选型**：`github.com/glebarez/sqlite`——纯 Go（modernc.org/sqlite 的 fork，专为 GORM 优化），无 cgo，交叉编译无忧。同时 ADK 的 `session/database` 也用 GORM dialector，一套依赖两种用途。

**WAL 独立于 ADK session**（关键决策，避免踩坑）：MessageBus 的 WAL 用**独立的 SQLite db 文件 + 独立表**，不与 ADK 的 `session/database` 共用。原因：ADK 的 `AppendEvent` 存的是 `session.Event`（LLM 对话历史），会被 `hydrateADKSession`（`service_adk.go:160`）回放给下一次 LLM 推理。如果把 bus 消息塞进去，会污染 LLM 上下文。两者各管各的表，只是共享同一个 SQLite 驱动。

**表结构**（详见附录 C）：
```
bus_messages(id, seq, turn_id, parent_turn_id, kind, payload, created_at, consumed)
bus_subscriber_offsets(subscriber_id, last_consumed_seq)
```

**Publish 流程**：
```
1. 若 msg.Reliable()==true:
     SQLite 事务 INSERT bus_messages(...) → COMMIT (WAL 模式 fsync)
2. 投递到 in-memory 订阅者 channel（非阻塞 + 优先级驱逐）
3. 若 msg.Reliable()==false:
     跳过 1，直接投递 in-memory channel
```

**订阅者崩溃 / 重连恢复**：
```
订阅者启动 → 读 bus_subscriber_offsets 拿 last_consumed_seq
           → SELECT * FROM bus_messages WHERE seq > last AND match(filter) ORDER BY seq
           → 逐条投递 + 更新 offset
           → 追上后切换到实时 in-memory channel 模式
```

**清理**：turn 完成后，`DELETE FROM bus_messages WHERE turn_id = ?` + 对应 offset。TTL 或显式 ack 二选一（Phase 2 决策）。

**订阅 scope / 权限**（防信息泄露）：
- 子 turn **不能**订阅兄弟 turn（同父的其它子）的事件。
- 父可订阅直接子 turn 的生命周期事件，但**不订阅子的内部 LLM token 流**。
- 默认 filter 只收自己的事件 + 直接子的生命周期事件。

#### 2.3.3 Phase 1 回头改清单（双 bus 修订引发）

双 bus 修订（2026-06-22）使 Phase 1 的"单 MessageBus"契约失效。Phase 1 类型需回头改（**改成本最低的时机就是现在**——Phase 2 还没人在 Phase 1 契约上建东西）：

| Phase 1 原设计 | 双 bus 后 |
|---|---|
| `Message` sealed interface（11 类型）| **删除**。拆成：Inbound/Outbound（普通 struct，进 MessageBus）+ `Event` sealed（9 类型，进 EventBus）|
| `MessageBus` interface（`Publish(Message)` + `Subscribe(Filter)`）| **拆**：新 `MessageBus`（内容，typed API：`PublishInbound`/`PublishOutbound` + `InboundChan`/`OutboundChan`）+ 新 `EventBus`（信号，`Publish(Event)` + `PublishNonBlocking` + `Subscribe(Filter)`）|
| `Filter` | **归 EventBus**（MessageBus 不需要 Filter，typed channel 即过滤）|
| `MessagePriority` | **改名 `EventPriority`**（只有 Event 用）|
| `sealed_exhaustive_test.go`（source-scan 守卫）| expected 集合从 11 改成 9（Inbound/Outbound 移出），扫 `isEvent()` 而非 `isMessage()`|
| `agent_turn.go` | **不动**（AgentTurn/Payload/状态机/TransitionError 跟 bus 无关）|
| `IsValidTransition`（W1 修过的状态机缺口）| **保留**，重写测试时重新覆盖 |
| Filter.Match（W2 修过的空 turn 泄漏）| **保留**，归 EventBus 后重新覆盖 |

**W1-W4 的 bug 修复要保留**（状态机缺口、Filter 空 turn 泄漏、sealed 假性穷尽、lifecycle TurnKind 字段）——重写测试时重新防范，不能丢。

**老 `event_bus.go` 的 `EventBus` 改名 `LegacyEventBus`**，新 `EventBus` 占名（新设计是长期契约）。Phase 6 下线 LegacyEventBus。

### 2.4 spawn_turn 替代 delegate_agent

**决策**：用 `spawn_turn` 替代 `delegate_agent`（普通 FunctionTool 同步实现），让父 turn 不阻塞。

**⚠️ 实现路径的关键约束（修正于 2026-06-22 PR review）**：

RFC v0 初稿误以为可直接靠 ADK 的 StreamingFunctionTool "fire-and-forget"（`base_flow.go:1066`）。核实源码后发现：**fire-and-forget 只在 Live API 路径生效**（`liveSess != nil`，即 `RunLive`）。Xira 走常规 `runner.Run()`（`service_adk.go:89`），`liveSess == nil`，StreamingFunctionTool 在这里走的是 `base_flow.go:1108` 的 else 分支——**同步迭代 `RunStream` 直到流结束才 return**，不会 fire-and-forget。

因此在 Xira 的常规 runner 路径上，`spawn_turn` 的"不阻塞父"**靠 tool 函数主动让迭代器早关闭 + detach goroutine**，而不是靠 ADK 的 fire-and-forget 分支：

```go
// 伪代码 —— 常规 runner 路径上的异步 spawn
spawnTurnTool := streamingtool.New("spawn_turn", func(ctx, args) iter.Seq[string] {
    return func(yield func(string) bool) {
        childTurnID := newAgentTurnID()

        // 1. 先宣告子 turn 诞生（bus 可见）
        bus.Publish(AgentTurnStarted{
            AgentTurnID:        childTurnID,
            ParentAgentTurnID:  ctx.ParentAgentTurnID(),
            TargetAgent:        args.AgentID,
        })

        // 2. 真正的子 turn 工作放进 detached goroutine
        //    注意：不能继承 ctx（ctx 在 tool return 后被取消），
        //    要用 context.WithoutCancel 或独立的新 ctx。
        go func() {
            childCtx := context.WithoutCancel(ctx) // 或独立 ctx
            result := runAgentTurn(childCtx, childTurnID, args)

            // ★ D-3 决策（2026-06-22）：spawn 载荷与信号分离。
            //   - EventBus 只发 AgentTurnCompleted 信号（turn id + status，
            //     不带 Result 载荷）——给 forwarder/feed/audit 观察"子完成了"。
            //   - spawn 结果载荷（AgentPayload: FinalText/ToolCalls/...）
            //     走专用 pendingResults channel 给父 turn——点对点，不走 bus。
            //   理由：载荷是"内容"性质（有业务数据），按"内容 vs 信号"分界
            //   不该进信号 bus。EventBus 保持纯信号，载荷走 channel，bus 纯度高。
            //   参考 PicoClaw subturn.go:410 pendingResults（同构设计）。
            //   未来若有多消费者要看 spawn 结果（CLI/WS/XiraGarden），
            //   再把载荷挪进 MessageBus（内容 bus），不进 EventBus。
            eventBus.Publish(AgentTurnCompleted{
                AgentTurnID: childTurnID,
                TurnKind:    AgentTurnKindAgent,
                Status:      result.Status,  // 只带 status，不带 Result 载荷
            })
            pendingResults <- pendingResult{
                ChildTurnID: childTurnID,
                Result:      result,         // 载荷走专用 channel
            }
        }()

        // 3. ★ 只 yield 一条 "spawned" 然后立即 return，结束流。
        //    ADK 在 base_flow.go:1108 的 for-range 会因此立即退出，
        //    把这条作为 tool result，推进下一个 model turn。
        //    父 turn 不阻塞——这是"不阻塞"的真正来源。
        yield fmt.Sprintf(`{"agent_turn_id":%q,"status":"spawned"}`, childTurnID)
        // 不再 yield，迭代器结束
    }
})
```

**要点**：
- "不阻塞"来自**迭代器早关闭**（yield 一条后 return），不是 ADK 的 fire-and-forget。
- 子 turn 的 goroutine 必须 detach（`context.WithoutCancel`），否则 tool return 后 ctx 被取消，子也会被杀。
- **D-3：spawn 结果载荷走专用 `pendingResults` channel（给父 turn），EventBus 只发 `AgentTurnCompleted` 信号（不带载荷，给观察者）**。父在 Phase 4 checkpoint 同时 drain `pendingResults`（拿载荷）和 EventBus（拿信号）。bus 纯度高（只传信号），载荷点对点。
- 如果 Phase 4 checkpoint 未落地，父拿到 `spawned` 后只能靠下一个 model turn 主动调 `wait_turn(childID)` 阻塞等子（见下文"等不等子由 LLM 决定"）——`wait_turn` 从 `pendingResults` 取载荷，不从 bus。

**语义变化**：

| | 当前 `delegate_agent` | 新 `spawn_turn` |
|---|---|---|
| 父是否阻塞 | ✅ 阻塞等 return | ❌ 立即返回 TurnID |
| 结果传递 | 函数 return value | bus 订阅 `AgentTurnCompleted` |
| 并发 | 假（父串行） | 真（goroutine per spawn） |
| 父能干别的 | ❌ | ✅ |

**等不等子由 LLM 决定**：父想等子完成时，输出 `wait_turn(childID)` tool call，阻塞订阅 bus 直到收到完成事件。不强制框架级等待。

### 2.5 Flow run 也是一种 AgentTurn

**决策**：Flow run 是 `Kind=flow` 的 AgentTurn（payload 是 `FlowPayload`），它 spawn 的 agent turn 是 `Kind=agent` 的 AgentTurn（payload 是 `AgentPayload`），父子关系用 `ParentAgentTurnID` 统一表达。

- 整个系统的执行单元和父子关系统一成一种类型 + 一个字段。
- Flow 的 `flow_run_id` 不再是 metadata 字符串弱关联，而是 `ParentAgentTurnID` 结构化关系。

---

## 3. 议题地图（Issue 拆分依据）

完整议题分 6 组，对应 6 个 Phase issue：

### 3.1 核心类型层（Phase 1）
- AgentTurn envelope + payload sealed interface
- FlowPayload / AgentPayload 字段定义
- MessageBus 强类型 Message 接口
- Turn 状态机（`requested → running → completed/failed/canceled/timeout`）
- Session 与 AgentTurn 的关系（含 `SessionScope` 字段设计）
- 命名规范化（`Turn`→`AgentTurn`、`SubTurn`→无、`delegate_agent`→`spawn_turn`、`flow.AgentTurnRequest`→`AgentStepRequest`）

### 3.2 通信层（Phase 1-2）
- MessageBus 接口与强类型 Message
- 订阅 scope / 权限 filter
- **bus 可靠性策略**（✅ 已决策：级别 4 WAL，lifecycle 落盘 SQLite + 内存优先级驱逐，见 §2.3.1）
- Routing（谁触发 turn：entrypoint resolve / flow step / bus 订阅者）

### 3.3 执行语义层（Phase 3-4）
- spawn_turn（StreamingFunctionTool）
- 上下文传递（context packet via bus payload）
- 结果校验 / 信任边界（异步校验）
- 错误 fallback 状态机
- Checkpoint 轮询（让父 turn 能响应异步事件）
- Steering（用户中途插话）
- 并发模型（一个 turn 能 spawn 多少子、checkpoint 能收多少异步结果）

### 3.4 数据与信任层（Phase 3）
- context packet：父→子的上下文序列化（当前 `buildDelegateContextPacket` 的异步版）
- 结果校验：异步下 `validateDelegateAgentResult` 怎么工作（bus 消息能伪造吗？谁校验？）
- fallback_hint：子失败时父如何兜底（父可能已在干别的）

### 3.5 韧性层（Phase 5）
- HITL / 跨进程：bus 挂了、HITL 跨小时怎么办
- Session 重建：进程重启后从 WAL 重放 bus 消息重建 turn 状态
- Trace 观测：distributed tracing span 在 spawn 时继承

### 3.6 落地层（Phase 6）
- 迁移老 `delegate_agent` → `spawn_turn`（灰度共存）
- 老 session 历史里混了两种语义的 turn 怎么回放
- EventBus 下线

---

## 4. 落地路径：6 个 Phase

### Phase 1 — 类型与契约（纯设计，零代码风险）
**目标**：定义 `AgentTurn`、`MessageBus`、`Message` 类型，厘清 Session 关系，bus 可靠性策略。

**产出**：
- Go 类型定义（`agent_turn.go`、`message_bus.go` 接口，不含实现）
- bus 可靠性策略决策（写入本文档附录）
- Session ↔ Turn 关系设计决策

**验收**：类型编译通过 + 设计评审通过。**不改运行时行为**。

### Phase 2 — MessageBus 实现（in-memory + SQLite WAL）+ EventBus 迁移
**目标**：实现 in-memory bus + SQLite WAL 持久化（lifecycle 落盘），让现有 EventBus 成为 bus 的 adapter，现有 `recordEvent` 调用点双写，progress forwarder 改订阅新 bus（保留旧 EventBus 兜底）。

**产出**：
- `messagebus/memory.go`（in-memory 实现，含 scope filter + 优先级驱逐）
- `messagebus/wal.go`（**SQLite WAL，glebarez/sqlite 纯 Go**，只存 `Reliable()==true` 消息；表结构见附录 C）
- 订阅者崩溃/重连的 offset 恢复
- WAL 清理（turn 完成后 `DELETE WHERE turn_id = ?`）
- `EventBus` → `MessageBus` adapter
- `progress.Forwarder` 改订阅新 bus
- 并发测试（突发 + 慢消费者 + 订阅者崩溃恢复）

**验收（DoD）**：
- 现有 progress feed 行为**完全不变**（E2E 测试全绿）
- 新 bus Publish/Subscribe 可用，lifecycle 消息进 WAL
- 进程重启后订阅者能从 WAL 重放补齐漏掉的 lifecycle 消息
- 并发测试覆盖 AGENTS.md §1.1 场景，drop 时 log.Warn（不静默）
- scope filter 生效：子 turn 订不到兄弟 turn 的事件
- **WAL 清理策略已决策并实现**（C.6 候选 a/b 二选一），且有测试覆盖"慢订阅者崩溃 >TTL"场景不丢终态消息
- **offset 崩溃恢复可测验收**：构造"订阅者 offset 更新前崩溃 → 重启重放 → 重复投递 lifecycle → 订阅者基于 turn 状态机 CAS 正确处理（only-once spawn，no 重复副作用）"的端到端测试

**双写过渡，老路径优先**。

### Phase 3 — spawn_turn + 异步子 turn（核心价值）
**目标**：实现 `spawn_turn`（StreamingFunctionTool 早关闭迭代器 + detach goroutine），父 turn 用 `spawn_turn` 启动子 turn，子 turn 在 goroutine 跑，完成时 bus.Publish；父通过 bus 订阅拿结果。

**产出**：
- `spawn_turn` tool（§2.4 修正后的实现：迭代器早关闭 + `context.WithoutCancel` detach）
- context packet 异步版（via bus payload）
- 异步结果校验
- fallback 状态机

**验收**：新 agent 用 `spawn_turn`，子 turn 真异步跑，父 turn 不阻塞。老 `delegate_agent` 保留（灰度共存）。**spawn_turn 的父子结果传递靠 bus 可靠性（Phase 2 的 WAL 已支撑），不用超时查磁盘兜底**。

### Phase 4 — checkpoint + steering（控制平面）
**目标**：给 `generate()` 内部加 checkpoint 轮询，实现 `wait_turn` 工具，实现 steering queue（用户中途插话）。

**产出**：
- checkpoint 机制（generate 内部）
- `wait_turn` tool
- steering queue（用户插话 → skip 剩余 tool + 合成 tool result）

**验收**：父 turn spawn 子后能继续推理；用户能中途插话；子完成事件能注入父的下一个 model turn。

### Phase 5 — HITL resume 路径迁移（WAL 已在 Phase 2 落地）
**目标**：把 HITL 恢复路径从磁盘 join state 接力迁移到 bus WAL 重放。WAL 本身已在 Phase 2 实现，本 Phase 只做 resume 逻辑迁移。

**产出**：
- HITL 恢复路径改造（API → bus WAL 重放，不再走 `DelegationJoinState` 磁盘接力）
- distributed tracing span 在 spawn 时继承（`ParentAgentTurnID` → child span parent）

**验收**：HITL 场景进程重启后能恢复 turn 状态（靠 Phase 2 的 WAL）；外部 worker（`claude -p`）长任务跨进程可观察。`DelegationJoinState` 路径仍在（Phase 6 下线）。

### Phase 6 — 迁移与下线
**目标**：所有 agent 切到 `spawn_turn`，下线 `delegate_agent`，下线旧 EventBus，清理磁盘 join state。

**产出**：
- 老 agent 迁移
- `delegate_agent` 标记 deprecated → 移除
- EventBus 下线
- 磁盘 join state 清理

**验收**：代码库只剩 `spawn_turn` + MessageBus 一条路径；老 session 历史能正确回放。

---

## 5. 依赖关系

```
Phase 1 (类型)
    │
    ├──► Phase 2 (bus 实现 + 迁移)
    │         │
    │         └──► Phase 3 (spawn_turn)  ──► Phase 6 (下线)
    │                    │
    │                    └──► Phase 4 (checkpoint + steering)
    │                                │
    └────────────────────────────────┴──► Phase 5 (HITL / WAL)
```

- Phase 1 是地基，所有后续依赖。
- Phase 2 是 Phase 3 的前提（spawn 要 publish 到 bus）。
- Phase 3 是 Phase 4 的前提（checkpoint 要响应 spawn 的异步结果）。
- Phase 5 可在 Phase 2 之后任何时点插入（WAL 不依赖 spawn）。
- Phase 6 最后做（所有路径迁移完成才能下线）。

**可并行**：Phase 2 和 Phase 5 的 WAL 部分；Phase 3 和 Phase 4 的设计。

---

## 6. 待决策清单（写进各 Phase issue）

以下是落地前必须决策的开放问题，分配到各 Phase issue 讨论。✅ 表示已决策（2026-06-22）。

### Phase 1 开放问题
1. ✅ **bus 可靠性策略**：**已决策 = 级别 4 WAL**（SQLite 持久化 lifecycle 消息 + 内存优先级驱逐）。见 §2.3.1、§2.3.2、附录 C。
2. ✅ **Session ↔ Turn 关系**：**已决策 = 方案 Z**（`AgentTurn.SessionScope *fsession.SessionScope` 可空 + `InheritSession bool`）。flow→agent 默认 true，agent→agent 默认 false。见 §2.1。
3. **Payload 字段细化**：`FlowPayload` / `AgentPayload` 的具体字段，要不要把现有 `TurnResponse` 的字段全搬过去。
4. **命名迁移范围**：`runtimeEventBase` 怎么处理（保留还是迁到 AgentTurn）。

### Phase 3 开放问题
5. **上下文传递**：spawn 时 context packet 走 bus payload 还是单独 channel？大小上限？
6. **结果校验异步化**：bus 消息能伪造吗？谁校验 `evidence_refs`？
7. **fallback**：子失败事件回来时父该暂停、重试、还是放弃？状态机怎么画？

### Phase 4 开放问题
8. **并发上限**：一个 turn 能 spawn 多少子？checkpoint 能收多少异步结果？
9. **steering 语义**：用户插话时，正在跑的子 turn 取消还是继续？

### Phase 2 开放问题（WAL 已从 Phase 5 前移）
10. ✅ **WAL 粒度**：**已决策 = 只持久化 `Reliable()==true` 的 lifecycle 事件**（started/completed/failed/canceled/human_requested/human_responded）。进度类不落盘。见 §2.3.1。
11. **WAL 清理**：turn 完成后 WAL 何时清？（候选：TTL 延迟清理 vs 显式 ack；附录 C.6 列了两选项，Phase 2 实现时定。）

---

## 7. 设计原则（沿用 AGENTS.md §2）

1. **先核实再判断**：评审时文档声称的契约必须先 grep / 读源码核实。本次正是靠核实发现 ADK 已支持异步、`assistant.final` 不发布等。
2. **缺口要补不要绕**：EventBus best-effort drop 是缺口，要补可靠机制，不是用"已有的近似物凑合"。
3. **silent data loss 是最贵的 bug**：MessageBus 必须配套并发测试（突发 + 慢消费者）和显式告警。
4. **正确性 > 可读性 > 一致性 > 简单性**：核心契约上的简单性要让位。
5. **增量优于全部重构**：6 个 Phase 渐进，全过程老路径可用。

---

## 8. Issue 清单

Epic issue #22 + 6 个 Phase 子 issue，全部归入 milestone [AgentTurn + MessageBus](https://github.com/xiramesh/xira/milestone/1)。

| Issue | Phase | 标签 | 依赖 | 状态 |
|---|---|---|---|---|
| [#22](https://github.com/xiramesh/xira/issues/22) | Epic | `epic`, `agentturn-messagebus`, `rfc` | — | 本文 |
| [#23](https://github.com/xiramesh/xira/issues/23) | 1 | `phase-1`, `design`, `agentturn`, `messagebus` | — | 待开始 |
| [#24](https://github.com/xiramesh/xira/issues/24) | 2 | `phase-2`, `messagebus` | #23 | 待开始 |
| [#25](https://github.com/xiramesh/xira/issues/25) | 3 | `phase-3`, `delegation`, `agentturn` | #24 | 待开始 |
| [#26](https://github.com/xiramesh/xira/issues/26) | 4 | `phase-4`, `delegation` | #25 | 待开始 |
| [#27](https://github.com/xiramesh/xira/issues/27) | 5 | `phase-5`, `messagebus` | #24 | 待开始 |
| [#28](https://github.com/xiramesh/xira/issues/28) | 6 | `phase-6`, `delegation` | #25,#26,#27 | 待开始 |

每个 Phase issue 的 body 结构：
- 目标（一句话）
- 范围（含/不含什么）
- 产出清单
- 验收标准
- 开放问题（引用本 RFC §6）
- 依赖（前置 Phase）

---

## 附录 A：ADK v1.4.0 能力清单（对我们有用的）

| 能力 | 包路径 | 对我们的作用 |
|---|---|---|
| 多 tool 并行执行（默认 goroutine） | `internal/llminternal/base_flow.go:1031` | 一个 model turn 内并发 spawn（对 Xira 直接可用） |
| StreamingFunctionTool | `internal/llminternal/base_flow.go:1066`（Live API）/ `:1108`（常规 runner） | ⚠️ fire-and-forget **仅 Live API 路径**；Xira 走常规 runner，在此路径上 StreamingFunctionTool 被**同步迭代消费**。spawn_turn 不靠 fire-and-forget，靠 tool 函数让迭代器早关闭 + detach goroutine（见 §2.4） |
| HITL confirmation | 定义 `agent/callback_context.go:196` `RequestConfirmation()`；调用点之一 `tool/tool.go:203` | 替代自建 human.request |
| Session 持久化 | `session/database/` | Phase 5 WAL 的底层 |
| SubAgents 树 | `agent/agent.go:228` | 参考（但走动态 spawn 不走静态注册） |
| IsLongRunning() 接口 | `tool/tool.go:45` | 长任务标记 |

---

## 附录 B：否决方案记录

### B.1 is-a 继承（FlowTurn extends AgentTurn）
否决：身份漂移、序列化地狱、Go 不友好、加类型涟漪扩散。详见 §2.1。

### B.2 简单 has-a（FlowTurn contains []AgentTurn）
否决：违反统一第一公民，bus 上两种消息类型，又回到散。

### B.3 换成自研 agent loop（抛弃 ADK runner）
否决：等于重写 runtime，工作量极大。ADK v1.4.0 已有大部分能力，应优先用上。

### B.4 直接用 ADK SubAgents 树（静态注册子 agent）
否决：牺牲 LLM 动态委派灵活性。保留动态 spawn，但参考 ADK 的父子模型。

---

## 附录 C：SQLite WAL 选型与表结构（EventBus + MessageBus 持久化层）

> 双 bus 都挂 WAL（2026-06-22 D-1 决策）。C.1-C.6 原为 EventBus 设计，仍适用；C.7 新增 MessageBus 的 inbound/outbound 持久化 + 判重表。两个 bus 用**独立的 db 文件**（各自故障隔离，跟 ADK session 也独立）。

### C.1 驱动选型：glebarez/sqlite（纯 Go，无 cgo）

**选定**：`github.com/glebarez/sqlite`。

- 纯 Go，无 cgo，交叉编译无忧（Xira 需要在 macOS/Linux/容器多平台编译）。
- `modernc.org/sqlite` 的 fork，专为 GORM 优化，drop-in 替换 cgo 版 `gorm.io/driver/sqlite`。
- 与 ADK `session/database` 的 `gorm.Dialector` 接口兼容——一套依赖，两种用途（ADK session 一个 db 文件，MessageBus WAL 另一个 db 文件）。

**否决**：`mattn/go-sqlite3`（cgo，编译麻烦）、`modernc.org/sqlite`（纯 Go 但 GORM 集成需手动配置，不如 glebarez 省事）、自写 append-only 文件 WAL（无索引/无事务/清理麻烦）。

### C.2 WAL 独立于 ADK session（关键决策）

MessageBus WAL 用**独立的 SQLite db 文件 + 独立表**，不与 ADK 的 `session/database` 共用。

**原因**：ADK 的 `AppendEvent`（`session/database/service.go:319`）存的是 `session.Event`（LLM 对话历史），会被 `hydrateADKSession`（`service_adk.go:160`）回放给下一次 LLM 推理作为上下文。如果把 bus 消息塞进 ADK session，会污染 LLM 上下文——模型会在下一轮看到不该看到的 lifecycle 事件。

两者各管各的表，只是共享同一个 SQLite 驱动依赖。

### C.3 表结构

```sql
-- lifecycle 消息持久化（只存 Reliable()==true 的消息）
CREATE TABLE bus_messages (
    id              TEXT PRIMARY KEY,          -- Message.ID()
    seq             INTEGER,                   -- 全局自增序号（订阅者 offset 锚点）
                                                -- 并发安全：进程内 atomic 计数器取号 +
                                                -- 同一 SQLite 事务内 INSERT，事务保证 seq 单调
    turn_id         TEXT NOT NULL,             -- Message.AgentTurnID()
    parent_turn_id  TEXT,                      -- 父 turn（便于父订阅者按子过滤）
    kind            TEXT NOT NULL,             -- Message 类型名（用于过滤）
    payload         BLOB NOT NULL,             -- JSON 序列化的 Message
    created_at      INTEGER NOT NULL,          -- Message.Timestamp() UnixNano
    consumed        INTEGER NOT NULL DEFAULT 0 -- 本条消息已被多少订阅者消费（清理用）
                                                -- 见 C.6：清理策略必须考虑慢订阅者崩溃场景
);

CREATE INDEX idx_bus_messages_seq        ON bus_messages(seq);
CREATE INDEX idx_bus_messages_turn       ON bus_messages(turn_id);
CREATE INDEX idx_bus_messages_parent     ON bus_messages(parent_turn_id);

-- 订阅者读取进度（崩溃/重连恢复用）
CREATE TABLE bus_subscriber_offsets (
    subscriber_id      TEXT PRIMARY KEY,
    last_consumed_seq  INTEGER NOT NULL DEFAULT 0  -- 该订阅者已投递到哪条 seq
);
```

### C.4 Publish 流程

```
func (b *Bus) Publish(ctx context.Context, msg Message) error {
    if msg.Reliable() {
        // 1. 落盘（SQLite WAL 模式，COMMIT 时 fsync）
        if err := b.wal.Append(ctx, msg); err != nil {
            return err  // 落盘失败 → 返回 error，不投递内存（宁可阻塞也不丢 lifecycle）
        }
    }
    // 2. 投递 in-memory 订阅者（非阻塞 + 优先级驱逐）
    b.fanout(msg)
    return nil
}
```

**关键约束**：`Reliable()==true` 的消息**落盘失败必须返回 error**（不投递内存、不吞错）。这是 lifecycle 可靠性的硬契约——宁可 Publish 报错让调用方决策，也不能假装投递成功然后内存丢掉。对 `Reliable()==false` 的消息，内存投递满载时直接丢 + log.Warn（§2.3.1）。

### C.5 订阅者崩溃 / 重连恢复

```
订阅者启动/重连:
  1. 读 bus_subscriber_offsets[subID].last_consumed_seq = N
  2. SELECT * FROM bus_messages WHERE seq > N AND match(filter) ORDER BY seq
  3. 逐条投递到订阅者 channel
  4. 每投递一条（或批量）UPDATE bus_subscriber_offsets SET last_consumed_seq = seq
  5. 追上实时后，切换到内存 channel 模式（新 Publish 直接投递）
```

**offset 更新策略**：至少一次（at-least-once）——订阅者可能收到重复消息（offset 更新前崩溃，重启后从旧 offset 重放）。

**⚠️ lifecycle 订阅者的硬约束**（2026-06-22 PR #30 review 修正，纠正"lifecycle 天然幂等"的过强断言）：

"lifecycle 天然幂等"只在**数据层面**成立——重复收到一条 `AgentTurnCompleted`，它携带的 turn 状态不变。但在**副作用层面不成立**——如果父订阅者用 `AgentTurnStarted` 触发 spawn，重复投递 = 重复 spawn。崩溃恢复后内存去重集合是空的，"靠 Message.ID() 去重"会失效。

因此 lifecycle 订阅者处理消息必须遵守：
1. **基于 turn 状态机做 CAS 转移**，不得依赖内存 ID 去重。例如父收到 `AgentTurnStarted{turn: X}` 时，先 `CAS(parent.owned_turns, X not exists)`——只有 X 不在父的 owned 集合里才 spawn，否则忽略。这个 owned 集合必须持久化（不能只在内存），因为崩溃恢复后内存是空的。
2. **状态机本身要容忍重复事件**。`AgentTurnCompleted` 到达时，如果 turn 已经是 `completed`，直接忽略（no-op），不重复执行完成逻辑。这要求 turn 状态机的每个状态转移都是幂等的（`completed + completed = completed`）。
3. **Message.ID() 只用于日志关联**，不用于正确性保证。

这是 at-least-once 投递的必然代价——换 simpler-than-exactly-once 的实现，承担"订阅者必须做 CAS"的责任。Phase 2 的并发测试必须覆盖"崩溃恢复后重复投递 lifecycle"场景。

### C.6 清理

turn 完成后（`AgentTurnCompleted`/`Failed`/`Canceled` 之后），清理该 turn 的所有 WAL 记录。

**⚠️ 清理不能只看 turn 终态**（2026-06-22 PR #30 review 修正）：`DELETE WHERE turn_id = ?` 只按 turn 删，没看 `consumed` 列。如果一个慢订阅者在 turn 终态前崩溃，TTL 过期后 WAL 被清理，该订阅者重启后**永久漏掉 `AgentTurnCompleted`**——它的 offset 还停在 Started 之前。这会让父 turn 永远等不到子完成。

**修正策略**：清理必须满足"所有订阅者都已消费过该 turn 的终态消息"。两个候选（Phase 2 实现时定）：
- **(a) 引用计数**：`consumed` 列记录已被多少订阅者消费（每投递一次 `consumed++`），`DELETE WHERE turn_id=? AND consumed >= subscriber_count`。慢订阅者没追上就不删。
- **(b) 终态确认**：每个订阅者显式 `ACK(turn_id)`，收到所有订阅者的 ACK 后才删。订阅者崩溃则不删（靠其重连后 ACK，或超时降级）。

```sql
-- 候选 (a) 引用计数版
DELETE FROM bus_messages WHERE turn_id = ? AND consumed >= (SELECT COUNT(*) FROM bus_subscriber_offsets WHERE match(turn));
-- 候选 (b) 终态确认版见上
```

**清理时机**：turn 终态 + 上述条件满足后立即删，或延迟 TTL 兜底（防订阅者永久不 ACK）。**Phase 2 决策 (a) 还是 (b)**。

### C.7 MessageBus 的 inbound/outbound WAL + 判重（D-1 新增）

MessageBus 也挂 WAL（2026-06-22 D-1 决策），独立 db 文件。**先保留（一进就落盘），再判重**。

**表结构**：
```sql
-- MessageBus 内容持久化（独立 db 文件，与 EventBus 的 bus_messages 隔离）
CREATE TABLE inbound_messages (
    id              TEXT PRIMARY KEY,          -- 内部生成（uuid），非平台 MessageID
    channel         TEXT NOT NULL,             -- "ilink" / "feishu" / ...
    chat_id         TEXT NOT NULL,
    platform_msg_id TEXT NOT NULL,             -- 平台 MessageID（可能非全局唯一）
    sender_id       TEXT,
    content         TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    processed       INTEGER NOT NULL DEFAULT 0 -- 是否已触发 turn（清理/审计用）
);

-- 判重 key：复合 (channel, chat_id, platform_msg_id)，不假设任何 channel 的 ID 语义。
-- 有的 channel MessageID 全局唯一，有的只在 chat 内唯一——复合 key 覆盖两种情况。
CREATE UNIQUE INDEX idx_inbound_dedup ON inbound_messages(channel, chat_id, platform_msg_id);

CREATE TABLE outbound_messages (
    id              TEXT PRIMARY KEY,
    channel         TEXT NOT NULL,
    chat_id         TEXT NOT NULL,
    inbound_id      TEXT,                      -- 关联的 inbound（可空，系统主动消息无）
    content         TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    delivered       INTEGER NOT NULL DEFAULT 0 -- 是否已发到 IM（清理用）
);
```

**PublishInbound 流程**（先保留再判重）：
```
1. INSERT inbound_messages(...) — UNIQUE 约束触发判重:
     重复 → INSERT 失败（UNIQUE 冲突）→ 返回"已处理"，幂等吸收，不重复触发 turn
     新消息 → INSERT 成功 → 继续
2. fsync（SQLite WAL 模式）
3. 投递到 InboundChan（阻塞，timeout=0）
4. 消费者（runtime/flow）触发 turn
```

**为什么复合 key 而非单 MessageID**：不同 channel 的 MessageID 语义不一（全局唯一 vs chat 内唯一）。`(channel, chat_id, platform_msg_id)` 复合 key **正交于 channel 的 ID 语义**——一次设计覆盖所有 channel，不依赖任何 channel 的 ack/重发能力。这是 D-1 的核心："bus 自治，不假设 channel 能力"。

**清理**：inbound 触发的 turn 终态后（processed=1 + TTL），删该 inbound 记录。outbound 发送到 IM 后（delivered=1 + TTL）删。TTL 比 EventBus 长（内容可能有审计/回溯价值）。

---

---

## 变更日志

- **2026-06-21**：RFC v0 初稿，基于设计对话复盘创建。覆盖 6 个 Phase、11 个开放问题、ADK v1.4.0 能力盘点。
- **2026-06-22**：修正两处事实性错误（PR #29 review 反馈）：
  1. **StreamingFunctionTool 路径错位**：`base_flow.go:1066` 的 fire-and-forget 只对 Live API（`RunLive`）生效；Xira 走常规 `runner.Run()`（`service_adk.go:89`），StreamingFunctionTool 在此走 `base_flow.go:1108` else 分支的**同步迭代**。修正 §1.2、§1.3、§2.4、附录 A，并据此重写 spawn_turn 实现假设（靠迭代器早关闭 + detach goroutine，而非 fire-and-forget）。这也是对方法论 AGENTS.md §2"先核实再判断"的自审：把 Live API 专属行为当通用能力引用，恰恰是没核实到位。
  2. **行号错**：`RequestConfirmation()` 定义在 `agent/callback_context.go:196`，`tool.go:199`（实际 203）只是调用点之一。修正附录 A。
- **2026-06-22**：关闭两个关键盲区，调整 Phase 划分：
  1. **bus 可靠性策略定为级别 4（WAL 持久化）**：lifecycle 消息落 SQLite WAL（glebarez/sqlite 纯 Go），进度类 best-effort + 优先级驱逐。WAL 从 Phase 5 **前移到 Phase 2**，让 Phase 3 的 spawn_turn 一开始就有可靠 bus 支撑。新增 §2.3.1（可靠性策略）、§2.3.2（WAL 形态）、附录 C（SQLite 表结构与流程）。Phase 5 缩水为 HITL resume 逻辑迁移。
  2. **Session ↔ Turn 关系定为方案 Z**：`AgentTurn.SessionScope *fsession.SessionScope`（可空）+ `InheritSession bool`。flow→agent 默认 true，agent→agent 默认 false（对齐现有 ephemeral_worker 语义）。更新 §2.1 struct 定义。
  3. `Message` 接口新增 `Reliable() bool` 和 `Priority() MessagePriority`，从"按 Kind 字符串 switch"升级为"按 sealed 类型方法"（编译期保证）。更新 §2.3。
- **2026-06-22**：修正 PR #30 review 的 2 个 CRITICAL 设计矛盾 + 6 条 non-blocking：
  1. **CRITICAL 1：Publish 阻塞语义自相矛盾**。§2.3.1 说"Publish 对所有消息非阻塞"，附录 C.4 又说 lifecycle "宁可阻塞也不丢"。修正：把"非阻塞"限定到进度类，lifecycle 改为"同步落盘 fsync 后返回，失败返回 error"。§2.3.1 新增"阻塞语义按消息类型分两档"明确区分"磁盘 IO 同步"（毫秒级、确定性）与"消费者背压"（秒级、不可控）。
  2. **CRITICAL 2：「lifecycle 天然幂等」断言偏强**。混淆了"数据层幂等"（重复收到 Completed 不改变状态）与"副作用层不幂等"（重复收到 Started 触发重复 spawn）。且崩溃恢复后内存去重集合为空，"靠 Message.ID() 去重"失效。修正附录 C.5：lifecycle 订阅者必须基于 turn 状态机做 CAS 转移（only-once spawn），owned 集合必须持久化，Message.ID() 只用于日志。
  3. **non-blocking**：seq 并发原子性（进程内 atomic + 同事务 INSERT）；C.6 清理策略修正（不能只按 turn_id 删，必须考虑慢订阅者崩溃 >TTL 场景，给出引用计数/终态确认两个候选）；SessionScope=nil 传播语义（§2.1 注释补充）；§2.3 类型清单补 `AssistantStatus` 声明；Phase 2 DoD 明确"WAL 清理策略"和"offset 崩溃恢复可测验收"。
  4. **行号订正**：上一轮回复误称"经核实 996 引用正确"，核实有误——本地工作树 `delegation.go` 有未提交改动导致行号偏移，我读到的 996 是偏移后的行号，不是 origin/main 的行号。origin/main 上 `AgentSessionID = "ephemeral_worker:"` 在 **977**（996 是 `StartedAt: time.Now()`，998 是 `delegation_mode`）。RFC §2.1 行号已从 996 改为 977。这是对 AGENTS.md §2"先核实再判断"的教训：核实必须锚定正确的 ref（origin/main），而不是带本地 diff 的脏工作树。
- **2026-06-22**：**重大修订——单 MessageBus 改双 bus（内容 vs 信号）**。
  - **论据来源诚实声明**（PR #41 review 后修正）：本修订的论据是**设计原则**（"内容 vs 信号是正交关注点"），**不是** #39（#39 是单 bus 立场，TL;DR 明确肯定"统一 MessageBus"）、**不是** "Xira 现状消费者不重叠"（grep 核实后此现状论据不成立——OutboundMessage 是孤儿、session history 崩溃窗口丢 inbound，见 W-1/W-2）。PicoClaw 的双 bus 是**事实参考**，非充分论据（场景不同）。
  1. **§2.3 重写**：原"统一 MessageBus + Reliable/Priority 路由"废弃。改为双 bus：**MessageBus**（内容，typed API：Inbound/Outbound）+ **EventBus**（信号，Event sealed + Filter，lifecycle 阻塞+WAL / progress 驱逐）。
  2. **修订理由（清理编造后剩的真实论据）**：内容（有业务载荷、频率低、不可丢）与信号（无载荷、频率差异大、可按优先级丢）是**正交关注点**——可靠性策略、投递语义、消费者群都不同。塞进单 bus 要靠 Reliable/Priority 路由表强行分流，本质是单 bus 内部跑两套路径。物理分离让各自实现简单、故障隔离。**这是唯一论据，不叠加 PicoClaw/现状/#39 背书。**
  3. **§2.3.1 调整**：**两个 bus 都挂 WAL，策略不同**——EventBus 只 lifecycle 落盘 / progress 驱逐；MessageBus 全保留 + 判重（D-1 已定，见下）。
  4. **§2.3.3 新增**：Phase 1 回头改清单——拆 `Message` sealed → Content structs + `Event` sealed；拆 `MessageBus` interface → 新 MessageBus + 新 EventBus；Filter 归 EventBus；老 `event_bus.go` 改名 `LegacyEventBus`。W1-W4 bug 修复保留。
  5. **D-3 已定**（2026-06-22，原 W-4）：spawn 结果载荷走专用 `pendingResults` channel（给父 turn，点对点），EventBus 只发 `AgentTurnCompleted` 信号（不带载荷，给观察者）。bus 保持纯信号，载荷走 channel。§2.4 伪代码已改。未来多消费者要看 spawn 结果时，载荷挪进 MessageBus（内容 bus），不进 EventBus。
  6. **D-1 已定**（2026-06-22，原 W-1）：MessageBus 也挂 WAL + 先保留再判重。否定原"靠 session history"（假）和"接受丢 inbound"（依赖 channel 重发，多 channel 架构不可行）。判重 key = `(Channel, ChatID, MessageID)` 复合，**不假设任何 channel 的 ID 语义或重发能力**——bus 自治。附录 C.7 新增 MessageBus 表结构。**关键洞察来自用户**：不应只看 iLink，因为不同 channel 重发语义不一，bus 不能依赖任何 channel 能力。
  7. **W-3 已定**：AssistantFinal（assistant.final）是 forwarder 的 drain 控制信号（第三类：非内容非信号非 lifecycle 非 progress），归 EventBus，Reliable=false（不落 WAL）+ Priority=Critical（drain 必须及时）。§2.3 Event 清单已标注。
