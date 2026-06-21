# Xira AgentTurn + MessageBus 设计总纲 (RFC v0)

- **状态**: Draft / 评审中
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

### 2.3 MessageBus：强类型、多订阅者、全双工

**决策**：bus 消息用 sealed interface 强类型，每种 kind 一个 struct，编译期保证 switch 完备。

```go
type Message interface {
    isMessage()
    ID()         string          // 消息唯一 ID
    AgentTurnID() AgentTurnID    // 归属哪个 turn
    Timestamp()  time.Time
}

// 每种 kind 一个具体类型（初版）
type InboundMessage struct{ ... }        // IM → 系统
type OutboundMessage struct{ ... }       // 系统 → IM
type AgentTurnStarted struct{ ... }      // turn 生命周期
type AgentTurnCompleted struct{ ... }
type AgentTurnFailed struct{ ... }
type AgentTurnCanceled struct{ ... }
type ToolCalled struct{ ... }
type ToolResult struct{ ... }
type HumanRequested struct{ ... }
type HumanResponded struct{ ... }

func (InboundMessage) isMessage()       {}
func (OutboundMessage) isMessage()      {}
// ...
```

**为什么强类型**：当前 `RuntimeEvent.Kind = "run.waiting_human"` 是弱类型，AGENTS.md §1.4 的 visibility 陷阱根因就是 `Kind string` + switch 漏 case。强类型从编译期杜绝"忘了加 case"。

**为什么 sealed**：加新 kind 时所有 type-switch 编译报错，强制显式决策每个新 kind 的语义。

**接口（初版）**：

```go
type MessageBus interface {
    Publish(ctx context.Context, msg Message) error
    Subscribe(filter Filter) (SubID, <-chan Message)
    Unsubscribe(SubID)
}

type Filter struct {
    AgentTurnID    *AgentTurnID    // nil = 所有
    Kinds          []MessageKind   // nil = 所有 kind
    IncludeChildren bool           // 是否含子 turn 的事件
}
```

**订阅 scope / 权限**（防信息泄露）：
- 子 turn **不能**订阅兄弟 turn（同父的其它子）的事件。
- 父可订阅直接子 turn 的生命周期事件，但**不订阅子的内部 LLM token 流**。
- 默认 filter 只收自己的事件 + 直接子的生命周期事件。

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
            bus.Publish(AgentTurnCompleted{
                AgentTurnID: childTurnID,
                Result:      result,
            })
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
- 子结果不通过 tool return 回父，而是通过 `bus.Publish(AgentTurnCompleted)`；父在 Phase 4 的 checkpoint 消费（见 §4 Phase 4）。
- 如果 Phase 4 checkpoint 未落地，父拿到 `spawned` 后只能靠下一个 model turn 主动调 `wait_turn(childID)` 阻塞等子（见下文"等不等子由 LLM 决定"）。

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
- **bus 可靠性策略**（⚠️ 盲区，必须 Phase 1 决策：best-effort / 背压 / 持久化）
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

### Phase 2 — in-memory MessageBus 实现 + EventBus 迁移（旁路）
**目标**：实现 in-memory bus，让现有 EventBus 成为 bus 的 transport/adapter，现有 `recordEvent` 调用点双写到新 bus，progress forwarder 改订阅新 bus（保留旧 EventBus 兜底）。

**产出**：
- `messagebus/memory.go`（in-memory 实现，含 scope filter）
- `EventBus` → `MessageBus` adapter
- `progress.Forwarder` 改订阅新 bus

**验收**：现有 progress feed 行为不变；新 bus 能被新代码 Publish/Subscribe。**双写过渡，老路径优先**。

### Phase 3 — spawn_turn + 异步子 turn（核心价值）
**目标**：实现 `spawn_turn`（StreamingFunctionTool），父 turn 用 `spawn_turn` 启动子 turn，子 turn 在 goroutine 跑，完成时 bus.Publish；父通过 bus 订阅拿结果。

**产出**：
- `spawn_turn` tool（StreamingFunctionTool）
- context packet 异步版（via bus payload）
- 异步结果校验
- fallback 状态机

**验收**：新 agent 用 `spawn_turn`，子 turn 真异步跑，父 turn 不阻塞。老 `delegate_agent` 保留（灰度共存）。

### Phase 4 — checkpoint + steering（控制平面）
**目标**：给 `generate()` 内部加 checkpoint 轮询，实现 `wait_turn` 工具，实现 steering queue（用户中途插话）。

**产出**：
- checkpoint 机制（generate 内部）
- `wait_turn` tool
- steering queue（用户插话 → skip 剩余 tool + 合成 tool result）

**验收**：父 turn spawn 子后能继续推理；用户能中途插话；子完成事件能注入父的下一个 model turn。

### Phase 5 — HITL / 跨进程韧性
**目标**：bus WAL（write-ahead log）持久化，进程重启后从 WAL 重放重建 turn 状态；HITL 跨小时不再靠磁盘 join state 接力，靠 bus WAL。

**产出**：
- `messagebus/wal.go`（bus 消息持久化）
- 进程重启 → WAL 重放 → turn 状态重建
- HITL 恢复路径改造（API → bus 重放，不再走磁盘 join state）

**验收**：HITL 场景进程重启后能恢复；外部 worker（`claude -p`）长任务跨进程可观察。

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

以下是落地前必须决策、但本 RFC 未定死的开放问题，分配到各 Phase issue 讨论：

### Phase 1 开放问题
1. **bus 可靠性策略**：best-effort（满则丢 + log）还是背压（Publish 阻塞）还是持久化（WAL）？影响 bus 接口签名。
2. **Session ↔ Turn 关系**：turn 属于 session 吗？一个 session 多 turn？turn 跨 session？影响 `AgentTurn.SessionScope` 字段。
3. **Payload 字段细化**：`FlowPayload` / `AgentPayload` 的具体字段，要不要把现有 `TurnResponse` 的字段全搬过去。
4. **命名迁移范围**：`runtimeEventBase` 怎么处理（保留还是迁到 AgentTurn）。

### Phase 3 开放问题
5. **上下文传递**：spawn 时 context packet 走 bus payload 还是单独 channel？大小上限？
6. **结果校验异步化**：bus 消息能伪造吗？谁校验 `evidence_refs`？
7. **fallback**：子失败事件回来时父该暂停、重试、还是放弃？状态机怎么画？

### Phase 4 开放问题
8. **并发上限**：一个 turn 能 spawn 多少子？checkpoint 能收多少异步结果？
9. **steering 语义**：用户插话时，正在跑的子 turn 取消还是继续？

### Phase 5 开放问题
10. **WAL 粒度**：全量持久化还是只持久化 lifecycle 事件（started/completed/failed）？
11. **WAL 清理**：turn 完成后 WAL 何时清？

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

## 变更日志

- **2026-06-21**：RFC v0 初稿，基于设计对话复盘创建。覆盖 6 个 Phase、11 个开放问题、ADK v1.4.0 能力盘点。
- **2026-06-22**：修正两处事实性错误（PR #29 review 反馈）：
  1. **StreamingFunctionTool 路径错位**：`base_flow.go:1066` 的 fire-and-forget 只对 Live API（`RunLive`）生效；Xira 走常规 `runner.Run()`（`service_adk.go:89`），StreamingFunctionTool 在此走 `base_flow.go:1108` else 分支的**同步迭代**。修正 §1.2、§1.3、§2.4、附录 A，并据此重写 spawn_turn 实现假设（靠迭代器早关闭 + detach goroutine，而非 fire-and-forget）。这也是对方法论 AGENTS.md §2"先核实再判断"的自审：把 Live API 专属行为当通用能力引用，恰恰是没核实到位。
  2. **行号错**：`RequestConfirmation()` 定义在 `agent/callback_context.go:196`，`tool.go:199`（实际 203）只是调用点之一。修正附录 A。
