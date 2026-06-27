# Xira ChatKey-Session Engine RFC v0

> **状态**：Draft · 2026-06-26 · 基于源码核实（HEAD `8ef3a4d`），可与代码逐条对照
> **作者**：架构评审产出
> **前置阅读**：`xira-per-chat-key-architecture-rfc-v0.zh.md`（per-chat-key 投递模型）、
> `xira-spawn-parent-child-comm-rfc-v0.zh.md`（HITL 无状态命题）

## 0. 一句话

把今天 **ilink 私有** 的 per-chatKey turn 处理骨架（Router + ChatContext + SteeringQueue +
SpawnCollector + ChildCancelRegistry + dedupe 收尾 + steering retry loop），提升为
**channelrunner 公共引擎** `ChatKeySession`；新增 channel 只写"协议适配"（把自家消息翻译成
`(chatKey, message)` 喂引擎 + 实现 `Emit` 往自家协议发）。引擎内显式建模 chatKey 的
**三态状态机**（active / idle / hitl-paused），把今天隐藏在 Router bool 后面的 HITL 缝暴露出来。

---

## 1. 现状核实：三个 channel，三套不一致的 turn 处理

逐个读源码核实（非凭文档），三者的差异**不是 by-design 取舍，是实现进度的拖尾**。

### 1.1 能力矩阵（核实后）

| 能力 | ilink | feishu | websocket |
|---|---|---|---|
| per-chatKey Router（`entry.active` 路由） | ✅ `progress.NewRouter()` | ❌ | ❌ |
| steering（用户插话中断） | ✅ `ErrSteered` retry loop | ❌ | ❌ |
| spawn 子任务取消（steering 时） | ✅ `ChildCancelRegistry` | ❌ | ❌ |
| SpawnCollector 清理（防内存泄漏） | ✅ `Reset()` defer | ❌ | ❌ |
| 进度 sink（ChatContext） | ✅ `progress.NewChatContext` | ✅ | ❌（直接投 frame） |
| dedupe 收尾 | ✅（闭包 defer） | ✅（另写一套 `messageProcessed`） | ✅（又一套） |

**锚点**：
- ilink Router 接入：`channelrunner/ilink/runner.go` `NewRunner` 里 `r.router = progress.NewRouter()`，
  dispatch 走 `r.router.Handle(chatKey, content, ctx, runTurn)`。
- feishu：`channelrunner/feishu/runner.go` `handleMessageReceive` 里**直接同步**
  `r.runtime.RunAgent(runCtx, ...)`，无 Router、无 steering、无 spawn 治理。
- websocket：`internal/api/websocket_channel.go` `handleWebSocketMessage` 末尾
  **`go func()` 起独立 goroutine** 跑 `RunAgent`（`:274-302`）——**每个请求一个 goroutine**，
  同 chatKey 连发两条消息 = **两个并发 turn**。

### 1.2 三个 channel 三种并发语义（这是最严重的）

同一个 chatKey 在 turn 进行中又来一条消息，三个 channel 的行为**完全不同**：

| channel | 行为 | 后果 |
|---|---|---|
| ilink | Router 看 `active=true` → `SteeringQueue.Enqueue`（插话） | ✅ 正确：steering |
| feishu | `handleMessageReceive` 阻塞在前一个 `RunAgent` 上（lark ws dispatcher 是否串行投递事件决定） | ❌ 串行：丢 steering；或并发：破坏单 active 契约 |
| websocket | `go func()` → 两个 goroutine 并发跑两个 `RunAgent` | ❌ **直接破坏 per-chatKey 单 active turn 契约** |

per-chat-key RFC #48 的核心命题是"每个 chatKey 同一时刻至多一个 active turn"。
**今天只有 ilink 满足**。feishu/websocket 都不满足。这是"不能每个 channel 写一遍"的
直接证据——抄写不一致已经造成了行为分叉。

### 1.3 不对称不是 by-design

`router.go` / `ilink/runner.go` 顶部注释都标注 "Phase 4, RFC #48"。RFC #48 是围绕
**steering** 展开 的能力，ilink 的产品形态（微信类、用户连续发多条）对它需求最直接，先落了。
feishu / websocket 走的是 Phase 4 之前的同步 / per-request-goroutine 路径，**还没有接 Router**。
注释里**没有一句**说"feishu 故意不要 steering"——所以这不是 by-design 的取舍，是**实现拖尾**
（对应 AGENTS.md §5.4：发现不对称先核实是不是 by-design——核实结果：这条不是）。

---

## 2. 为什么放 `channelrunner/progress/`，不放 `channel/`

用户直觉："这种 chatKey 抽象当然应该在 channel 上"。方向对，但 `channel/` 这个具体包放不下。

### 2.1 现有分层（核实）

```
internal/channel/
  ├ outbound.go     OutboundEnvelope / OutboundEmitter 接口
  └ types.go        InboundContext / CapabilitySet / 纯类型
internal/channelrunner/progress/
  ├ router.go           map[ChatKey]*chatEntry + active 路由
  ├ chatcontext.go      per-chatKey 进度 sink（render/throttle/quota）
  ├ steering_queue.go   SteeringQueue（implements runtime.SteeringBus）
  ├ spawn_collector.go  SpawnCollector（implements runtime.SpawnBus）
  └ child_cancel_registry.go  per-chatKey spawn 取消注册表
internal/channelrunner/{ilink,feishu}/ + internal/api/ (ws)
    各自接入：协议事件 → RunAgent + Emit
```

`channel/` 包**不依赖 runtime**（只有 `InboundContext` 等纯类型，没有 import runtime）。
而 Router 的字段是 `runtime.ChatKey`，它注入的 `SteeringBus`/`SpawnBus` 来自 runtime 的
context helper。**把 Router 塞进 `channel/` → 纯类型层反向依赖 runtime → 分层 inversion。**

### 2.2 正确的落点

引擎放 `channelrunner/progress/`（已存在、已依赖 runtime、三件套已在此）。`channel/` 保持
纯类型层不动。`progress/` 新增一个聚合对象 `ChatKeySession`（见 §4），把今天散在 ilink
闭包里的副作用收敛成公共 API。

---

## 3. WebSocket 是 transport，不是唯一入站协议

### 3.1 客户端面 vs 适配器面（不要搅）

- **客户端面（client-facing）**：WS 作为面向"我来连"的客户端的规范 surface——**对**。
  用户的"熙壤"个人助理、未来任何自带 WS 客户端的应用，都走 `websocket_channel.go`。
- **适配器面（adapter-facing）**：不能强制 WS。feishu 推事件是 webhook（HTTP POST）或
  自家 lark-ws；Discord 是 gateway WS + REST 发消息；邮件是 SMTP/IMAP。这些**不是 WS**，
  硬套 WS = 适配器假装自己是 WS，反而更复杂。

### 3.2 抽象的真正形状

不是"一切皆 WS"，而是更抽象一层：

> **按 chatKey 索引的双向消息流**。WS 是这个抽象的一个 transport；feishu webhook 是另一个；
> Discord gateway 是第三个。引擎只认 `(chatKey, inbound) → outbound`，不关心底下是什么协议。

这样 feishu/Discord 适配器各自只写"协议 → (chatKey, msg)"的翻译 + `Emit` 实现，不用硬塞 WS 框子。
WS 作为客户端面的规范地位保留，但不让它污染适配器面。

---

## 4. 三态状态机：今天隐藏在 `active` bool 后面的 HITL 缝

### 4.1 用户论证里被忽略的洞

用户论证："per-chatKey 单 active turn → 判断坍缩成一个 bool → 新问题起 run / 插话 steer /
其它，都简单"。**对 2/3 场景对，第 3 个场景（HITL）不成立**。

核实（agent 报告 + 代码逐条核对）：HITL 暂停时，`markComplete` 照常把 `active` 翻 false
（`router.go:84` defer 在 onNewTurn，`waiting_human` 也让 RunAgent 返回）。所以 **HITL 期间
Router 看到 `active=false`**。这时来一条消息，Router 会说"没有 active turn → 起新 turn"——
但用户真实意图可能是"回答你刚才问我的那个 HITL 问题"，该走 **resume** 那条**不经 Router** 的路径。

### 4.2 chatKey 的真实三态

```
state = active          → turn 正在跑（新消息 = steer enqueue）
state = idle            → 空闲（新消息 = 起新 turn）         ← Router 的 bool 只区分这两种
state = hitl-paused     → 暂停等回答（新消息 = ???）          ← bool 看不见这层
```

Router 的 `active` bool **只能区分前两种**，第三种对它不可见。当前设计是"Router 不负责 HITL，
resume 走另一条路"——这是 by-design 的取舍（无状态 resume 命题要求 resume 不依赖 Router 内存）。
但**新抽象如果只暴露 `active` bool，HITL 缝就被封装进抽象内部、对外不可见**——下游 channel 适配器
会发现"我明明 active=false，怎么消息没起新 turn 反而被 resume 吃掉了"，行为不可解释。

### 4.3 身份断裂（核实发现，比想象更糟）

不只状态看不见，**key 都对不上**：

| 存储 | keyed by | 锚点 |
|---|---|---|
| `Router.entries` | `runtime.ChatKey` = (Channel, ChatID, SenderID) | `progress/router.go` `map[runtime.ChatKey]*chatEntry` |
| `humanrequest.Store` | **`workspaceKey`**（`ws_` + sha256(workspaceID)[:16]，跨所有 chat 扁平） | `humanrequest/store.go` `WorkspaceKeyFor` / `requestPath` |
| `RunStore` | **`runID`** | `runtime/store.go` `RunDir` |

**humanrequest.Store 没有 ChatKey 字段**（`humanrequest/types.go` `HumanRequest` 只有 RunID/SessionID/
AgentID/ToolCallID/DedupeKey）。所以**入站消息无法通过 chatKey 找到自己 pending 的 HITL 请求**——
resume 今天只能由 request-ID 驱动（HTTP `POST /human-requests/{id}/responses` 或 CLI `approve`）。

**结构性后果**：一个 chatKey 可以**同时** `active=false`（Router）AND 有 pending HITL 请求
（humanrequest.Store）——这时用户发消息会被当成"起新 turn"，HITL 请求被 orphan（只在启动日志里 surface）。
这不是理论，是核实确认的代码现状。

### 4.4 resume 如何绕过 Router（核实）

两条 resume 路径都在 `runtime/human_request_resume.go`：
- `resumeRunAfterApprovedToolOutput`（:111）—— approve 工具门 resume
- `resumeDirectHumanRequest`（:278）—— agent-request（child→parent question）resume

两者**都不调 RunAgent，直接调 `s.generate(...)`**；**都不经 Router.Handle**；
ctx 钥匙里**都没有** EventBus / SteeringBus / SpawnBus / ChatKey / ChildCancelRegistry /
tool allowlist（已在源码注释里 documented，是 by-design 的取舍，不是疏忽）。
final 通过 `s.deliverResumeFinal` → `s.outbound.Emit`（= Manager.Emit）按 SessionScope 路由回 IM。

> **这条契约必须保留**：resume 不依赖 Router 内存 = 无状态 resume 命题。新抽象不能为了"统一"
> 让 resume 反过来读 Router 状态——那会违反这个 codebase 最核心的命题之一（见
> `xira-spawn-parent-child-comm-rfc-v0.zh.md` 方案 A 被弃的理由）。

---

## 5. 设计：`ChatKeySession` 引擎

### 5.1 职责收敛

把今天 ilink `runTurn` 闭包（`runner.go:630-797`）里**跨 channel 通用的副作用**收敛进
`progress/` 包的一个对象。哪些是通用的、哪些是 ilink 专属：

| 闭包里的代码段 | 通用 / 专属 | 去处 |
|---|---|---|
| `defer messages.Complete(dedupeKey)` | 通用 | 引擎（dedupe 收尾） |
| `NewChatContext` + Start/Stop | 通用 | 引擎（进度 sink 生命周期） |
| `NewChildCancelRegistry` + defer CancelAll/Reset | 通用 | 引擎（spawn 治理） |
| `defer collector.Reset()` | 通用 | 引擎（SpawnCollector 清理） |
| steering retry loop（`for { RunAgent; if ErrSteered ... }`） | 通用 | 引擎（turn 重试） |
| `r.send(final)` | **专属**（feishu 发 card，ws 发 frame，ilink 调 openilink） | 适配器 `Sender` / `Emit` |

收敛后，ilink / feishu / ws 适配器各自**只剩**：协议事件解析 → `session.Handle(chatKey, msg, inbound)`；
+ 实现 `Sender`（进度）和 `Emit`（final/resume）。

### 5.2 状态机显式建模（核心变更）

```go
// progress/chatkey_session.go (新)

type ChatKeyState int
const (
    StateIdle        ChatKeyState = iota // 无 turn，可起新 turn
    StateActive                          // turn 正在跑（新消息 = steer）
    StateHitLPaused                      // HITL 暂停等回答（新消息 = 走 resume 分支）
)

// ChatKeySession 是 per-chatKey 的 turn 引擎。每个 chatKey 一个实例。
type ChatKeySession struct {
    key       runtime.ChatKey
    state     ChatKeyState          // 三态，取代今天的 active bool
    steering  *SteeringQueue
    spawn     *SpawnCollector
    cancels   *ChildCancelRegistry
    chatCtx   *ChatContext
    // hitl
    pendingHR *humanrequest.HumanRequest // state==HitLPaused 时非 nil
    // ...dedupe handle, runRef 等
}

// Handle 是唯一入口。返回值告诉适配器"消息被如何处理"——HITL 缝对外可见。
type HandleOutcome int
const (
    OutcomeStartedTurn   HandleOutcome = iota // 起了新 turn
    OutcomeSteered                            // 插话塞进 SteeringQueue
    OutcomeResumed                            // 触发了 HITL resume（不经 Router）
    OutcomeIgnored                            // 重复消息等，被 dedupe 吃掉
)

func (s *ChatKeySession) Handle(ctx context.Context, msg string, inbound channel.InboundContext) HandleOutcome
```

**关键**：`HandleOutcome` 让 HITL 缝对适配器**可见**——适配器能区分"我起的 turn 跑完了"
vs "我的消息触发了 resume（异步，final 会通过 Emit 回来）"。今天的 Router 不给这个信息。

### 5.3 HITL 暂停怎么让 Session 知道（不破坏无状态命题）

这是设计上最 delicate 的一点。两条约束要同时满足：

1. **resume 不依赖 Router 内存**（无状态命题，§4.4）——不能让 resume 读 Session.state。
2. **Session 要知道"这个 chatKey 有 pending HITL"**——否则三态建不起来。

解法（草案，待评审）：**Session 不持有 HITL 真相，只持有一个轻量 hint**。
turn 跑完返回 `waiting_human` 时，引擎把 state 置 `HitLPaused` + 记下 `runID`（仅作 hint，
不作真相源）。下次 `Handle` 进来：

- `state == HitLPaused` → 查 humanrequest.Store（**用 runID 反查**，不用 chatKey——因为
  humanrequest.Store 不按 chatKey 索引，§4.3）确认 pending 请求还在；
  - 还在 → 走 resume 分支（`ResolveHumanRequest`），返回 `OutcomeResumed`；
  - 不在了（已被 HTTP/CLI resolve 了 / 过期了）→ state 回 `Idle`，起新 turn。
- resume 分支本身**仍然不经 Router.Handle**（保无状态命题）——它调 `ResolveHumanRequest`，
  `s.generate`，`deliverResumeFinal` → `Emit`，和今天一模一样。Session 只是"路由分叉点"，
  不是 resume 的执行者。

**这样无状态命题保住**：resume 的真相源还是 RunStore + humanrequest.Store（落盘），
Session 的 `HitLPaused` 只是 in-memory 路由 hint，进程崩了 → Session 重建 → 下个 chatKey
首次消息会走 `idle` → `RunAgent` → 如果这个 run 其实是 waiting_human，runtime 自己会返回
`waiting_human`，Session 重新进 `HitLPaused`。**没有真相只存在内存里**。

### 5.4 身份补全：humanrequest 加 ChatKey 字段

§4.3 指出的身份断裂，最干净的修法是给 `humanrequest.HumanRequest` 加一个 `ChatKey` 字段
（创建 HITL 请求时从 inbound 填），并给 Store 加一个 `ListByChatKey(chatKey)` 索引。
这样 Session 的 `HitLPaused` 路由就不必"用 runID 反查"——直接 chatKey 查 pending 请求。

**这是 schema 变更**（对应 AGENTS.md §5.4 里"AllowedTools 不持久化在 run，要改 schema"同类），
范围可控（humanrequest 是 per-workspace yaml 文件，加字段向后兼容）。**列为 RFC 必修项**，
不做这一步，§5.3 的"用 runID 反查"就是绕，会把"chatKey 找不到自己 HITL"这个洞永久固化。

---

## 6. 落地路径（增量，非大爆炸）

遵循 AGENTS.md §4 "增量优于全部重构"。分三步，每步独立可发布、可回滚。

### Step 1：提取 `ChatKeySession`（不改行为）

- 在 `progress/` 新建 `chatkey_session.go`，把 ilink `runTurn` 闭包里的通用副作用搬进去。
- ilink 改成调 `session.Handle(...)`，行为**完全不变**（同一份代码换个壳）。
- 测试：ilink 现有的 router_test / steering_test / spawn 测试全绿。
- **不碰 feishu / ws**——它们还是老路径。

### Step 2：feishu 接入（补齐能力 + 修并发语义）

- feishu `handleMessageReceive` 改成 `session.Handle(...)`。
- 收益：feishu 自动获得 steering / spawn 治理 / 单 active 防护。
- 行为变更（要明示）：feishu 用户在 turn 跑时连发消息，从"串行阻塞/并发跑两个 turn"
  变成"第二条 steer"。这是**修 bug**，但要在 changelog 里写清楚。

### Step 3：websocket 接入 + 三态路由

- ws `handleWebSocketMessage` 的 `go func()` 改成 `session.Handle(...)`。
- **修掉** today's"同 chatKey 两个并发 turn"的契约违反。
- 实装三态状态机 + `OutcomeResumed` 分支（§5.2-5.3）。
- 配套：humanrequest 加 ChatKey 字段 + Store 加 `ListByChatKey`（§5.4）。

每步都有独立测试（progress 包覆盖率 ≥85%，关键契约 100%，AGENTS.md §5.2）；
涉及 LLM 的用真 key 跑 live test（§5.3）。

---

## 7. 明确不在本 RFC scope

- **不重做事件投递模型**（per-chat-key EventBus 投递已是现状，§1.1 稳定）。
- **不取消无状态 resume**（resume 不读 Router 内存这条契约，保）。
- **不改 RunStore schema**（只 humanrequest 加字段）。
- **不强制所有 channel 用 WS 入站**（§3）。
- **不改 steering 的协作式中断语义**（turn 自己在 checkpoint 检查 queue，§5.1 保留）。

---

## 8. 待评审的开放问题

1. **`HitLPaused` hint 的过期/失效**：进程不崩，但 HITL 请求被 HTTP/CLI 异步 resolve 了，
   Session 怎么及时知道？选项：(a) 下次 `Handle` 查 Store（lazy，最简）；(b) Session 订阅
   Store 变更（复杂，引入新耦合）。倾向 (a)。
2. **resume 分支的进度投递**：今天 resume 路径没有 EventBus sink（§4.4），final 之外的进度
   全丢。新抽象要不要给 resume 也接一个 sink？（这可能违反"resume 无状态"——要单独评估）。
3. **ChatKeySession 的生命周期**：等同今天的 routerEntryTTL（1h heuristic，§AGENTS.md 导航）？
   还是和 humanrequest TTL 联动？倾向：保持 1h，HitLPaused 状态例外（不 prune，直到 HITL
   resolve 或过期）。

---

## 9. 核实锚点速查

| 断言 | 锚点（符号名稳定，行号会漂移） |
|---|---|
| Router 靠 RunAgent 返回翻 active，不订阅事件 | `progress/router.go` `Handle` / `markComplete` |
| ilink 是唯一用 Router 的 channel | `channelrunner/ilink/runner.go` `NewRunner` 里 `progress.NewRouter()` |
| feishu 同步 RunAgent 无 Router | `channelrunner/feishu/runner.go` `handleMessageReceive` |
| ws per-request goroutine（并发 turn） | `internal/api/websocket_channel.go` `handleWebSocketMessage` 末尾 `go func()` |
| HITL 时 active 也翻 false | `progress/router.go` `markComplete`（defer 在 onNewTurn） |
| resume 不经 Router | `runtime/human_request_resume.go` `resumeRunAfterApprovedToolOutput` / `resumeDirectHumanRequest` |
| resume final 走 Manager.Emit | `runtime/human_request_resume.go` `deliverResumeFinal` → `s.outbound.Emit` |
| humanrequest.Store 不按 chatKey 索引 | `internal/humanrequest/store.go` `WorkspaceKeyFor` / `requestPath`；`HumanRequest` 无 ChatKey 字段 |
| ChatKey 定义 | `runtime/chatkey.go` `ChatKey{Channel,ChatID,SenderID}` / `ChatKeyFromInbound` |
| Router TTL = 1h（heuristic，非计算值） | `progress/router.go` `routerEntryTTL`（mirror dedupe） |
