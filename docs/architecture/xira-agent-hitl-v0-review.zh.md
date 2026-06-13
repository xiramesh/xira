# Xira Agent HITL v0 设计 Review

> 对象文档：`docs/architecture/xira-agent-hitl-v0.zh.md`
> 评审基准：分支 `feature/agent-hitl-v0` 当前代码（`apps/xira/internal/runtime/{types.go,service.go,service_adk.go}`、`apps/xira/internal/tools/registry.go`）
> 评审历史：
> - **R1**（首轮）：基于原始草案，提出 P0/P1/P2 共 10 项问题。
> - **R2**（本轮）：文档已采纳 R1 全部意见并修订，R1 闭环；针对修订版提出 5 项新问题 N1–N5。

## 总体评价

文档方向正确，且 R1 反馈被**实质性地采纳**（不是"已记录"，而是改了结构）：

- HITL 定位为 runtime 基础能力，HumanRequest 持久化中断信封。
- 拆分"协作型 HITL / 强制型 HITL"，明确 v0 不自带 runtime 强制安全边界的旧表述已修正。
- 接通现成的 `ToolPolicy.RequireConfirmation`，按 `request_source` 区分 snapshot replay 与 agent 继续。
- native 路径明确依赖 session hydration，并诚实标注"无 hydration 前只能支持 clarification/choice"。

但修订版把"强制型 replay"做成子系统后，**新引入了 5 个实现级硬坑**，其中两个（N1、N2）是必须现在拍板、不能挂"待确认"的。

严重度分级沿用：

```text
P0 必须在 v0 实现前解决
P1 影响 v0 可用性/安全,强烈建议同步处理
P2 边界与文档完整性,可在落地顺序里补
```

---

## R1 闭环确认

R1 提出的全部问题已在修订版中处理：

| R1 问题 | 修订位置 | 结论 |
| --- | --- | --- |
| P0-1 native 非 loop / session 回灌前提不成立 | "Native 路径的 session 回灌前置条件"（514–545）；落地顺序第 3 步；545 行诚实标注无 hydration 前的限制 | ✅ 到位 |
| P0-2 v0 是"绅士协议"，安全增益≈0 | 摘要拆"协作型/强制型"，明确不接 gate 即为协作协议 | ✅ |
| P1-1 `RequireConfirmation` 已存在却不接 | 触发来源/已定决策/ADK 关系（815）；并补"ADK 已消费、native 必须消费，否则同 profile 不同 engine 行为不一致" | ✅ 比原建议更深 |
| P1-2 "不自动重放"可能选反 | 按来源分流：snapshot replay vs agent 继续 | ✅ |
| P1-3 approval 语义塌缩 | 新增 `request_source`/`trust_level`/`action_snapshot`；approval 两类表 | ✅ 把"塌缩"转为"明确两类" |
| P1-4 `human.signal` 来源可信度矛盾 | 重命名 HumanResponse/`human.respond`；信任分级表；校验改为"只接受 inbound 当前消息" | ✅ |
| P2-1 status 边界 | 新增"状态与统计边界"（907–921） | ✅ |
| P2-2 allowlist 冲突 | 新增"Runtime Control Tools 与 Allowlist"（923–944） | ✅ |
| P2-3 无限追问 | 新增"去重与限流"（946–957） | ✅ |
| P2-4 跨 turn 状态恢复 | 新增"跨 Turn 状态恢复"（959–984） | ✅ |

R1 全部闭环。以下为 R2 新增问题。

---

## R2 新增问题

### N1（P0）：snapshot replay 会再次撞上 RequireConfirmation gate → 死循环

强制型 approval approve 后，runtime 要"按 action snapshot 重放"（591 行），即真正执行那个 `command.run`。但重放执行的仍是同一个 tool，其 `def.Policy.RequireConfirmation` 仍为 true。若 replay 走 `executeToolCall`：

```text
replay command.run
  -> executeToolCall 检查 RequireConfirmation == true
  -> 再次创建 HumanRequest
  -> 再次 waiting_human
  -> 死循环
```

文档只写了"replay 最多执行一次"（982）和"replay 不让 agent 重新构造 input"（1018 测试），**完全没说 replay 执行时如何 bypass 同一个 gate**。这是 replay 能否跑通的命门，不是边角。

必须显式定义一条 replay 执行通道：

```text
replay 执行带 replay=true 标记
  -> 跳过 RequireConfirmation gate
  -> 仍走审计、快照校验、幂等检查
```

并在测试策略补一条："replay 执行的 tool 不再触发二次 RequireConfirmation。"

### N2（P0）：approve 之后，谁触发 replay + resume？契约悬空

跨 turn 恢复流程（965–977）画的是"RunAgent start -> if resume_human_request_id -> replay"。即 **replay 发生在下一次 RunAgent 开始时**，依赖外部再发一次请求驱动。

那么 `POST /human-requests/{id}/responses` approve 之后，系统处于悬空态：

```text
已 approved，但未 replay，也无人继续
-> 必须等用户再发一条消息，才会触发下一个 RunAgent -> 才 replay
```

这与"不阻塞 HTTP"形成软矛盾，且**体验是断的**：用户点了 approve 以为动作执行了，实际系统在等下一条消息。IM channel 场景尤其糟——用户不可能知道还要"再发一句话"。

文档把此问题甩到"待确认问题 5"（1037），但它**不是可挂起的小问题**，直接决定 responses API 契约：该 POST 是同步执行 replay 并返回结果，还是纯落库等异步触发？这是 v0 决策点。

建议现在就定：

```text
responses API 在 runtime_tool_gate approve 时同步触发 replay，
把 replay 结果/状态一并返回，不依赖下一条消息。
```

### N3（P1）：channel 的 router_structured 能否 enforce 强制 approval——默认值含糊

信任表（96 行）：`channel 结构化 shortcut，由 router 解析` 标为"中到高，**取决于 channel 认证**"，可用于强制 approval 是"可以配置为可以"。

"取决于"在安全语境里太软。现实里 IM channel（飞书/微信 bot）的"用户身份"常不可靠：群成员、可冒充 sender、无强认证 webhook。若 `git push` 的强制 approval 能被 IM 群里一句 `/approve` 放行，等于把强制 gate 焊在纸糊的墙上。

必须给 v0 明确默认，不能含糊：

```text
v0 默认：仅 transport_authenticated（CLI/API/本地 UI）可 resolve runtime_tool_gate approval。
channel router_structured 默认 deny 强制 approval，需显式 per-channel 配置开启。
```

文档现为"可配置"但无默认值，实现者大概率图省事默认允许——默认值方向错会出事。

### N4（P1）：native/ADK 不对称——ADK 路径的 hydration 完全没写

文档对 native session hydration 写得很细（514–545、875 挂点表新增行）。但 ADK 路径只有一句"创建/保存/resolve/replay 由 Xira runtime 负责"（813）。

问题：ADK 有自己的 session 机制。强制型 approval 的 HumanResponse 和 replay result，**怎么注入到 ADK session context 让 ADK agent 读到？** 一行都没有。落地顺序第 3 步也只写 "native session hydration"。

若 v0 要保证"ADK / native 对 RequireConfirmation 行为一致"（测试策略 1017），则 response/replay 注入也必须一致——否则 native 能 resume、ADK 不能 resume，正是 815 行自己反对的"同 profile 不同 engine 行为不一致"。

二选一，不能假装对称：

1. v0 把 ADK hydration 也写出来；或
2. 明确"v0 snapshot replay 仅 native，ADK 路径强制 approval 暂只创建不自动 replay"。

### N5（P1）：native hydration 依赖一个尚不存在的 compaction 能力

native hydration 要注入"compacted prior messages + request/response/replay summary + current message"（531–535）。但 native 路径是固定 2 次 call、从 `{system, req.Message}` 重建，**当前代码无 compaction/历史窗口机制**。session 一长，全量回灌直接顶爆 context window。

文档用"compacted"一词带过，但这是 native hydration 能否落地的硬依赖。必须二选一：

1. 标注"v0 native hydration 需一个最小 compaction/滑动窗口，否则历史超 N 轮会失效"；或
2. v0 只回灌最近 K 轮 + HumanRequest 摘要，显式截断。

否则实现者会在 compaction 上卡很久。

---

## 框架外提醒

修订版把 HITL 做成"协作型 + 强制型"双层。结构对了，但要警惕一个心理陷阱：**强制型（replay）的复杂度远高于协作型**——snapshot、hash、TTL、bypass gate（N1）、悬空触发（N2）、ADK 对称（N4）、compaction（N5）。这五条全是强制型带来的。

若 v0 想真正能交付，可考虑**把强制型 approval 的 replay 收敛到最小范围**：

```text
v0 强制型 replay 仅支持 CLI/API + 本地 workspace 的 RequireConfirmation tool；
channel 强制 approval 推到 v0.1。
```

这样 N2/N3/N4 的影响面都缩到最小，而协作型（澄清/选择/低风险确认）的核心价值不受影响。否则 v0 会被 replay 链路的工程复杂度拖住，反而连协作型都上不了线。

---

## 一句话总结

```text
R1 反馈已全部闭环，文档质量合格。
但修订引入的"强制型 replay"子系统有 5 个实现级硬坑（N1–N5），
其中 N1（死循环）和 N2（悬空触发）必须现在拍板，不能挂"待确认"。
```

## R2 优先级建议

1. **N1** 定义 replay 执行的 bypass 通道，避免二次 RequireConfirmation 死循环。
2. **N2** 拍板 responses API 契约：强制型 approve 同步触发 replay，不依赖下一条消息。
3. **N3** 给 channel router_structured 一个安全默认（默认 deny 强制 approval）。
4. **N4** ADK hydration：要么写出来，要么明确 v0 不支持 ADK 路径强制 replay。
5. **N5** native hydration 的 compaction 依赖：要么做最小滑动窗口，要么显式截断。
6. 视交付压力，评估是否把 v0 强制型 replay 收敛到 CLI/API + 本地 workspace。
