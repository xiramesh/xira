# RFC: spawn 父子通讯模型

> **状态**: v0 设计讨论(closes #63)
> **关联**: xira-agentturn-messagebus-rfc-v0.zh.md §2.4, xira-per-chat-key-architecture-rfc-v0.zh.md
> **依赖**: 命名统一(#62/PR #64 已 merged),MessageBus 实现(优先于 Phase 5)

## 1. 背景

spawn_turn 的当前设计是**完全隔离**的:子 turn 从 `context.Background()` 派生 ctx,
不继承父的任何 Bus(EventBus/SteeringBus/SpawnBus)。这修了"子污染父流"的问题
(PR #52 C3),但切断了三个合理的父子连接。

用户在讨论中确认三个架构缺口需要解决:

1. **双向通信** —— 子不能中途问父问题
2. **子事件 fire 给父** —— 父和用户看不到子的过程(进度不可见)
3. **steering 传递给子** —— 父被 steer 时子继续白烧 token

本文档分析每个缺口的现状、方案、取舍,给出推荐设计。

## 2. 现状(spawn 父子隔离的精确状态)

```go
// spawn_turn.go: childToolConstraintCtx
func childToolConstraintCtx(parent context.Context) context.Context {
    ctx := context.Background()  // ← 从 Background 起,不继承父的任何 Bus
    // 只 re-attach 工具约束(allowlist / inputAllowlist / nativeToolsDisabled)
    // EventBus / SteeringBus / SpawnBus 全部不继承
    return ctx
}
```

- 子 ctx 从 Background 起 → 子的 dispatchEvent 找不到 EventBus → 子事件被丢(有 Debug log)
- 子的 steering checkpoint 找不到 SteeringBus → 子不响应 steering
- 子的 poll_turn 找不到 SpawnBus → 子不能再 spawn 孙子(但也不需要)

**隔离是对的**——避免子事件污染父的 IM 流,避免 steering 误注入子。
但**完全隔离太极端**——合法的父子连接也被切断了。

## 3. 缺口 1:子事件 fire 给父的 EventBus

### 问题

子跑很久(30 秒、5 分钟)时,父和用户**完全看不到子在干什么**。父只能 poll 结果
(完成/失败/pending),看不到子的**过程**(思考、工具调用、进度)。

用户在 IM 里看到:"我已 spawn 了子任务" → 一片沉默 → poll 出结果。中间 30 秒没有反馈。

### 方案:子 ctx 继承父的 EventBus(选择性继承)

```go
func childCtx(parent context.Context) context.Context {
    ctx := context.Background()
    // re-attach 工具约束(已有)
    ctx = childToolConstraintCtx(parent)
    // 新增:继承父的 EventBus —— 子事件 route 到父的 chat key
    if bus := EventBusFromContext(parent); bus != nil {
        ctx = WithEventBus(ctx, bus)
    }
    return ctx
}
```

子的事件(`run.started` / `assistant.status` / `tool.called` / ...)经过 dispatchEvent
→ 送到父的 EventBus(同一个 ChatContext)→ 渲染到父的 IM 流。

用户看到:
```
我已 spawn 了 research-assistant...
  [research-assistant] 正在分析...
  [research-assistant] 调用了 web_search("LLM agent")
  [research-assistant] 分析完成
好的,子任务完成了,结果是:...
```

### 取舍

- ✅ 用户能看到子进度(体验大幅提升)
- ✅ 符合 per-chat-key RFC §2.4:"子 turn 事件 route 到父的 chat key"
- ✅ 改动小(childCtx 加一行 WithEventBus)
- ⚠️ 子事件和父事件混在同一个 EventBus → 需要渲染层区分来源(用 child_agent_id 或
  event 上的 run_id 区分父/子)。renderEvent 已经有 agent_id,只需渲染时加前缀。
- ⚠️ 高频子事件(adk.token)可能刷屏 → 渲染层需要限流/过滤(已有:ChatContext 的
  dedupe/throttle 机制)

### 推荐:✅ 采用

这是三个缺口里改动最小、收益最大的。一行代码(WithEventBus)就能让子进度可见。

## 4. 缺口 2:双向通信(子 → 父 提问)

### 问题

子不能中途问父问题。比如子在做研究,遇到需要判断的分叉——现在子只能自己猜,
或触发自己的 HITL(但 spawn 的 HITL 没有 resume 机制)。

### 方案 A:SpawnBus 扩展支持子→父提问(推荐)

子完成时 Deliver 的是 `{Status, Result}`。扩展一个中间状态:

```go
type PendingResult struct {
    TurnID string
    Result DelegateAgentResult
    Err    string
    Status string  // 新增:"completed" / "failed" / "asking" / "canceled"
    Question string // 新增:当 Status=="asking" 时,子向父的提问
}
```

子遇到分叉时 Deliver `{Status:"asking", Question:"这个研究方向对吗?"}`:
- 父 poll_turn 看到 `{status:"asking", question:"..."}`(不是 pending 也不是 completed)
- 父决定:自己回答(reply 到 SpawnBus),或转给用户(human.request)
- 子收到回答后继续

**不需要新通道**——复用 SpawnBus,只是 Deliver 的类型扩展。

### 方案 B:子调 human.request 直接问用户

子调 human.request tool → 用户看到子的问题 → 回答 → 子 resume。
不需要 SpawnBus 扩展,但依赖 spawn HITL resume(Phase 5)。

**缺点**:这是"子问用户",不是"子问父"。有些问题父 agent 能答(不用打扰用户),
但这个方案绕过了父。

### 取舍

- 方案 A(SpawnBus 扩展)更灵活:父能自己答,也能转给用户
- 方案 B(human.request)更简单:复用已有 HITL 路径,但依赖 Phase 5
- **两个不互斥**:方案 A 做基础(子→父),方案 B 做兜底(子→用户,Phase 5)

### 推荐:✅ 方案 A(SpawnBus 扩展),方案 B 作为 Phase 5 的补充

但**优先级低于缺口 1**(事件可见)——子能问父是"更好",子进度可见是"必须"。
先做缺口 1,缺口 2 留第二步。

## 5. 缺口 3:steering 传递给子(父被 steer 时取消子)

### 问题

用户插话("算了别研究了")→ 父的 steering checkpoint 检测到 → 父重启 turn。
但**已经在跑的子不知道**——它继续烧 token 直到自己完成或 timeout。

### 方案 A:父 steer 时取消所有 outstanding 子(推荐)

父的 steering retry 循环已经会 cancel + 重启父 turn。在重启前,取消所有
outstanding 子:

```go
// ilink runner steering retry,在 chatCtx.Reset() 旁边
if err != nil && errors.Is(err, ErrSteered) {
    // 取消所有 outstanding 子(SpawnBus 追踪的 + activeChildren 里的)
    rt.cancelOutstandingChildren(parentRunID)
    // ... 然后 chatCtx.Reset() + collector.Reset() + 重启
}
```

子的 ctx.Done() → RunChildAgent 中止 → Deliver `{Status:"canceled"}` → 释放 slot。

### 方案 B:steering 传递到子的 SteeringBus

用户的插话也进子的 SteeringBus → 子的 checkpoint 检测到 → 子也重启。

**缺点**:太复杂。子重启后要和父同步(父也重启了,两个重启怎么协调?)。
而且子的 restart 语义不清晰(子是被 spawn 的,不是用户直接对话的)。

### 取舍

- 方案 A(取消子)简单正确:用户改变方向,旧方向的子任务应该取消
- 方案 B(子也 steer)复杂且语义不清

### 推荐:✅ 方案 A(取消子)

需要一个新机制:`cancelOutstandingChildren(parentRunID)` —— 找到该 parent 的所有
active 子,cancel 它们的 ctx。spawnCore 的 goroutine 持有 childCtx 的 cancel func,
需要一个注册表(parentRunID → []cancelFunc)来追踪。

## 6. 推荐实施顺序

| 步骤 | 缺口 | 改动 | 优先级 | 依赖 |
|---|---|---|---|---|
| **1** | 子事件 fire 给父 | childCtx 继承父 EventBus(1 行)+ 渲染区分 | **高**(体验必须) | 无 |
| **2** | steer 取消子 | cancelOutstandingChildren + cancel 注册表 | **中**(防白烧) | 步骤 1 |
| **3** | 子→父提问 | PendingResult 扩展 Status/Question + poll_turn 处理 | **低**(增强) | 步骤 1-2 |

步骤 1 可以立即做(改动极小)。步骤 2-3 作为后续 PR。

## 7. 与 MessageBus / Phase 5 的关系

这三个改动**不依赖 MessageBus**——它们是 spawn 父子通讯的改进,和内容持久化独立。

MessageBus 的 WAL(优先于 Phase 5)解决的是"crash 不丢 inbound"——和父子通讯正交。
两个可以并行推进。

Phase 5 的 HITL resume 会依赖:
- MessageBus WAL(内容恢复)
- 本 RFC 步骤 3(子→父提问,如果采用方案 B 的 human.request 路径)

## 8. 不做的事

- **不给子独立的 IM 通道**:子不和用户直接对话(只有父对话)。子的事件 route 到父。
- **不给子独立的 SteeringBus**:子不被直接 steer(父 steer 时取消子,不是让子也 steer)。
- **不做子的 spawn 递归**:子不能再 spawn 孙子(depth guardrail 已限制 MaxDepth)。
