# Xira Per-Chat-Key 架构 RFC v0

- **状态**: Implemented / Historical RFC。当前权威契约见
  `docs/architecture/xira-runtime-current-contract.zh.md` 与源码。
- **日期**: 2026-06-23
- **作者**: ai-daming（基于 2026-06-23 架构对话）
- **背景**: 取代 per-Service 全局 EventBus + Forwarder 模型
- **关联 RFC**: `xira-agentturn-messagebus-rfc-v0.zh.md`（双 bus 设计，本 RFC 修订其 bus 拓扑）

---

## 0. TL;DR

当前 Xira 的 EventBus 是 **per-Service 单例**——所有并发 turn 的事件混在一个全局 bus 上，
每个消费者用 scopeMatcher 过滤"属于我的"。核实后发现：

1. **全局 EventBus 没有真正的受益者**——飞书/ilink 是 per-message-per-turn（不关心别的 turn），
   WS channel 用 `active[]` 自己管理多 turn。没有任何消费者需要"一次订阅看全部 turn"。
2. **全局 bus 的代价是 scopeMatcher + Forwarder**——一套复杂的匹配逻辑 + 一个独立 struct（2 goroutine
   + queue + drain）只为了从全局 bus 里"挑出我的事件"。
3. **Forwarder 的存在理由消失了**——它存在是因为全局 bus 需要 scopeMatcher；per-chat-key 模型下
   事件天然隔离。

本 RFC 提议：**per-chat-key 架构**——事件按 `(Channel, ChatID)` 隔离，全局 EventBus 和 Forwarder
都被消除。

---

## 1. 动机：为什么是 per-chat-key

### 1.1 两种并发情况不能混为一谈

IM channel（飞书/ilink）的并发有两种，必须区分：

**第一种：不同用户各自发消息（独立 turn）**
- 张三发消息 → turn A（张三的会话）
- 李四发消息 → turn B（李四的会话）
- 两个 turn 完全独立，不同 chat_id，互不相关
- 全局 bus 把它们混在一起 → scopeMatcher 按 chat_id 拆开 → **不必要的复杂度**

**第二种：同一用户在 turn 跑着时又发消息（steering 场景）**
- 张三发消息 → turn A 开始
- turn A 还在跑，张三又发一条 → **这不是并发 turn，是用户插话（steering）**
- 当前 Xira 的行为：Monitor 回调串行阻塞，第二条消息等 turn A 完成才处理
- **正确行为**：steering——用户插话打断/修正当前 turn（Phase 4 checkpoint）

### 1.2 WS channel 的"全局观察者"是假需求

核实（grep api/server.go + websocket_channel.go）：
- `/api/v1/events`：全局事件流，**不过滤**（调试端点，全量推给 WS 客户端）
- `/api/v1/channels/xiragarden/events`：按 channel 过滤（`eventBelongsToChannel`，自己过滤）
- `pumpWebSocketEvents`：按活跃请求过滤（`snapshotActive()` + `acceptEvent()`，自己过滤）

**三个 WS 消费者都在自己过滤**——全局 bus 对它们没有路由价值。它们用 `requestID` 而不是
`(ChatID, SenderID)` 作为 key，没有按 chat 拆分。如果 WS 也按 chat key 路由（像飞书一样），
全局 bus 的最后一个"理由"也消失了。

### 1.3 Forwarder 的全部 5 件事可以内联

Forwarder 做 5 件事：① 订阅+scopeMatcher 过滤 ② 挑选可投递 kind ③ 渲染成文字 ④ 节流/去重/配额
⑤ 发到 IM。

per-chat-key 模型下：
- ① scopeMatcher **消失**（per-chat-key 天然隔离）
- ②③ 变成纯函数（`progress.Render(Event) string` + kind switch）
- ④ 状态放 chat context 或 AgentTurn 上（`ProgressSent` / `LastProgressAt`）
- ⑤ channel adapter 的 `r.send`（它本来就在干）

**Forwarder 作为独立概念消失。**

---

## 2. 核心设计

### 2.1 chat key

```go
// ChatKey 唯一标识一个会话。同一 chat key 下的消息属于同一个对话。
type ChatKey struct {
    Channel  string // "ilink" / "feishu" / "websocket" / "xiragarden"
    ChatID   string // 平台的会话 ID（飞书 chat_id / ilink chat_id / WS 补的 chat_id）
    SenderID string // 发送者 ID（PR #48 review: 决策已定——含 SenderID）
}

// ChatKey 的多维度参考 PicoClaw InboundContext 的 dimensions（space/chat/topic/sender），
// 但不照搬 PicoClaw 的全局 bus 拓扑——PicoClaw 的 MessageBus 是全局单例（NewMessageBus
// 返回一个 struct 含 5 条全局 channel），恰好是本 RFC 要推翻的模型，不是 per-chat-key 先例。
```

- 飞书/ilink：已有 `ChatID` + `SenderID`（InboundContext），直接用
- WS channel：**需要补**——当前用 `requestID`，要改成按 `(Channel, ChatID, SenderID)` 路由
- 群聊 vs 单聊：ChatType 区分（群聊 ChatID = 群 ID + 可能的 topic）
- SenderID 的作用：群聊里区分不同发送者——每个 sender 跟**自己的** agent 说话，
  steering 永远回到**自己**的父 turn（群里 A/B 各自独立 turn，互不 steering）

### 2.2 per-chat-key 事件路由

```
消息进入 channel
  → @me / 提及过滤（群聊入站门：不 @me 的群消息不进 turn 处理）
  → 解析 ChatKey (Channel, ChatID, SenderID)
  → 查 ChatKey 是否有活跃父 turn
    ├─ 没有 → 启动 turn
    │         turn 的事件 → route 到这个 ChatKey 的输出 → channel r.send → IM
    │         （不需要 scopeMatcher，天然隔离）
    └─ 有 → steering（插话 route 到父 turn）
             父 turn 决定：打断/取消子/让子完成后再处理
             （Phase 4 checkpoint + steering queue）
```

> **@me 过滤 ≠ SenderID 归属**：两个正交维度。@me 是入站门（群消息是否触发 turn），
> SenderID 是 turn 归属（触发的 turn 属于哪个 sender）。别把 @me 塞进 ChatKey 维度。

### 2.3 steering 永远 route 到父 turn

用户在 IM 里说话，IM 会话只有一个——用户永远跟"父 turn"对话。子 turn（spawn 出来的）
是内部实现，用户不直接交互。steering route 到父 turn，父决定怎么处理。

### 2.4 全局 EventBus 消失

per-chat-key 模型下没有全局订阅者：
- IM channel：per-chat-key，事件天然隔离
- WS channel：按 chat key 路由（补 ChatID 解析后），不需要全局
- 调试端点 `/api/v1/events`：可选保留一个全局汇聚（所有 chat key 的事件 fan-out），但这是
  **可选的附加**，不是架构核心

---

## 3. 消除了什么

| 现状 | per-chat-key 模型 |
|---|---|
| 全局 EventBus（per-Service 单例）| **消失**——per-chat-key 路由 |
| scopeMatcher（4 套过滤逻辑）| **消失**——天然隔离 |
| Forwarder（独立 struct + 2 goroutine + queue + drain）| **消失**——内联到 chat context |
| WS 的 active[] + requestID 管理 | **简化**——按 chat key 路由 |
| Monitor 回调串行阻塞 | **改 steering**——Phase 4 |
| Visibility/Scope 元数据过滤 | **消失**——per-chat-key 天然可见 |

---

## 4. 影响（哪些设计被取代）

### 4.1 取代的设计

| 原设计 | 状态 | 取代为 |
|---|---|---|
| 双 bus（MessageBus + EventBus per-Service 单例）| **取代** | per-chat-key 事件路由（EventBus 不再是全局单例）|
| Forwarder | **删除** | 渲染纯函数 + channel adapter 直接发送 |
| scopeMatcher | **删除** | per-chat-key 天然隔离 |
| Filter（EventBus 的订阅过滤）| **简化** | per-chat-key 下大多不需要（天然隔离）；子 turn 事件可用 IncludeChildren |

### 4.2 保留的设计

| 设计 | 状态 | 理由 |
|---|---|---|
| AgentTurn 第一公民类型 | **保留** | 跟 bus 拓扑无关 |
| Event sealed（10 类型）| **保留** | 事件类型不变，只是路由方式变 |
| spawn_turn（Phase 3）| **保留** | spawn 逻辑不变，子 turn 事件 route 到父的 chat key |
| steering（Phase 4）| **保留 + 强化** | 从"可选优化"变成"架构必需"（per-chat-key 模型必须有 steering）|
| WAL 持久化（Phase 5）| **保留** | 跨进程韧性仍需要 |
| observability 剥离（#43）| **保留** | 仍需要把 trace/audit 从事件流剥出 |

### 4.3 需要补的

| 补什么 | 工作量 |
|---|---|
| WS channel 补 ChatID 解析 | 小（WS 消息帧加 chat_id 字段）|
| ChatKey 路由层（channel → chat key → turn）| 中（新建，取代 Forwarder 的路由职责）|
| progress.Render 纯函数 | 小（从 Forwarder 的 Render 抽出来）|
| 节流状态放 chat context / AgentTurn | 小（从 Forwarder 的字段搬过去）|

---

## 5. 落地路径（修订 Phase 划分）

per-chat-key 架构改变了 Phase 2 的核心，但 Phase 3-6 大部分不变：

### Phase 2（修订）：per-chat-key 事件路由
- ChatKey 类型 + 路由层
- per-chat-key 事件投递（取代全局 EventBus + Forwarder）
- WS channel 补 ChatID 解析
- progress.Render 纯函数
- runtimeEventToEvent 映射（已 merge，A2a）
- observability 分流（slog，临时，#43 正式剥离）

### Phase 3（不变）：spawn_turn
- 子 turn 事件 route 到父的 chat key（spawn_turn 工具内部）

### Phase 4（强化）：checkpoint + steering
- steering 从"可选优化"变成"架构必需"
- per-chat-key 模型下，用户插话必须能 steering 到父 turn
- PicoClaw 式 4 checkpoint + steering queue

### Phase 5（不变）：HITL resume + WAL

### 过渡态：per-chat-key + steering 作为不可分上线单元

per-chat-key 模型下，用户在 turn 跑着时插话**必须能 steering**（否则用户被阻塞，体验比现在更差）。
因此 **Phase 2（ChatKey 路由）+ Phase 4（checkpoint + steering）是不可分的上线单元**——不能只上
Phase 2 不上 Phase 4，否则用户插话无处可去。Phase 3（spawn_turn）可以夹在中间或跟 Phase 4 同期。

### Phase 6（不变）：迁移下线

---

## 6. 待决策清单

1. ✅ **已定（PR #48 review）**：ChatKey 含 SenderID。参考 PicoClaw InboundContext 的
   dimensions。每个 sender 跟自己的 agent 说话，steering 永远回到自己的父 turn
   （群里 A/B 各自独立 turn，互不 steering）。
2. **调试端点 `/api/v1/events` 怎么办**？核实（PR #48 review）：零真用户（README 未提，全仓
   无消费者引用）。砍全局 bus = 砍这个裸调试端点，无兼容成本。可选保留全局汇聚（per-chat-key
   的 fan-out），但不是架构核心。
3. **per-chat-key 的 ChatContext 存在哪**？内存 map？要不要持久化（进程重启恢复）？
4. **子 turn 事件怎么 route 到父的 chat key**？子 turn 知道父的 ChatKey 吗（继承）？
5. **节流/去重/配额状态放哪**？ChatContext 还是 AgentTurn？
6. **deprecated 桥接的退役路径**：PR #46 加的 deprecated `Publish(RuntimeEvent)`/
   `Subscribe(ctx)` 不跟 per-chat-key 一起推翻——它是全局 bus 平滑退役的工具。per-chat-key
   路由建好后，老消费者逐步切走，最后删全局 bus + deprecated 方法（Phase 6）。

---

## 附录 A：对话推导过程

本 RFC 的架构结论来自以下对话推导链（每一步都核实了源码）：

1. "Forwarder 是干什么的" → 读 forwarder.go 全文 → 它是 runtime 事件 → IM 消息的投影器
2. "为什么需要 scopeMatcher" → 核实 EventBus 是 per-Service 单例 → 所有 turn 共享一个 bus
3. "全局观察者是谁" → 核实 4 处 Subscribe → 3 个 WS + 1 个 Forwarder → WS 都自己过滤
4. "WS 为什么需要全局" → 核实 WS 用 requestID 不用 ChatID → WS 没按 chat 拆分
5. "飞书也可能并发 turn" → 区分两种并发（独立用户 vs 同一用户插话）→ 第二种是 steering
6. "steering route 到父还是子" → 用户只跟父对话 → 永远 route 到父

每一步的"核实源码"记录见对话上下文。本 RFC 是这些核实的结论汇总。
