# Xira Agent HITL v0 设计 Review

> 对象文档：`docs/architecture/xira-agent-hitl-v0.zh.md`
> 评审基准：分支 `feature/agent-hitl-v0` 当前代码（`apps/xira/internal/runtime/{types.go,service.go,service_adk.go,config.go,delegation.go}`、`apps/xira/internal/tools/registry.go`）
> 评审历史：
> - **R1**（首轮）：基于原始草案，提出 P0/P1/P2 共 10 项问题。
> - **R2**（第二轮）：文档采纳 R1 全部意见，R1 闭环；提出 N1–N5。
> - **R3**（第三轮）：文档采纳 R2 全部意见并引入 `workspace_id`，R2 闭环；提出 M1–M5。
> - **R4**（第四轮）：文档采纳 R3 全部意见并引入 AgentDelegation，R3 闭环；delegation 章节核实为真实已有代码的契约化。但 delegation 与 HITL 的交互（child waiting_human）暴露出文档严重低估的实现冲突 D1。
> - **R5**（第五轮）：文档选择保留 child waiting_human 进入 v0，并补齐 `RunInterrupt`、`RuntimeSuspendCollector`、最小 response API、`canceled` 边界和 `max_outstanding`，R4 主问题闭环；剩余 1 个文档一致性问题 E1。
> - **R6**（本轮）：文档同步 `第一版实现边界` 与正式 `落地顺序`，E1 闭环；当前无剩余 blocking review 项。

## 总体评价

文档经过六轮修订，HITL 主线已高度收敛：

- 协作型 / 强制型双层，`request_source` / `trust_level` 分流。
- `runtime_tool_gate` snapshot + 同步 replay + CAS 状态机（R3 补全 running 态）。
- native / ADK 共享 HITL store + hydration。
- `workspace` 用 canonical `WorkspaceRoot`，内部 `workspace_key` 分片（R3 修正弱身份）。
- 主动引入 AgentDelegation，把已有 `delegation.go`（1291 行）的行为收敛为四语义契约。
- 明确选择 child `waiting_human` 进入 v0，并承认这是最大的 runtime 改造点。
- 用 `RunInterrupt` / `RuntimeSuspendCollector` 统一表达 run 级中断，避免各路径用普通 tool output、FinalResponse 或 ad-hoc metadata 表达 suspend。

R4 里最重的判断仍然成立：现有 `delegation.go` 是同步阻塞等待 child `FinalResponse` 的模型，而目标语义要求 suspendable delegation。区别是 R5 文档不再低估这个冲突，而是选择正面承接它：把 `delegate_agent` 升级为可持久化 continuation 的 runtime control tool。

当前没有新的 P0/P1/P2 blocking review 项。主文档可以作为 v0 实现依据进入拆分执行。

严重度分级沿用：

```text
P0 必须在 v0 实现前解决
P1 影响 v0 可用性/安全,强烈建议同步处理
P2 边界与文档完整性,可在落地顺序里补
```

---

## 历史闭环

### R1（10 项）— 全部闭环

P0-1 native 非 loop / P0-2 绅士协议 / P1-1 RequireConfirmation 接通 / P1-2 重放策略 / P1-3 approval 塌缩 / P1-4 signal 可信度 / P2-1 status 边界 / P2-2 allowlist / P2-3 去重限流 / P2-4 跨 turn 恢复。均已处理，不再展开。

### R2（N1–N5）— 全部闭环

N1 replay 死循环（`executeReplay` + `replay=true` bypass）/ N2 approve 悬空（responses API 同步 replay + 补偿路径）/ N3 channel 默认值（默认 deny）/ N4 ADK hydration（AgentHistory 共享 store）/ N5 compaction（滑动窗口 K=20）。均已处理。

### R3（M1–M5）— 全部闭环

| R3 问题 | 修订 | 结论 |
| --- | --- | --- |
| M1 replay 并发状态机 | `replay_status` pending→running→completed/failed + CAS 抢占 + `replay_attempt_id`/`replay_started_at`/`replay_lease_expires_at`（975–1004）；重试幂等返回 `running` | ✅ 专业级状态机 |
| M2 input_hash 装饰性 | 改名 `env_hash` + `env_snapshot`(workspace_root/cwd/git_head)，明确不做防篡改签名（1006–1012） | ✅ 改名 + 定义计算口径 |
| M3 workspace_id 载体 | 否决弱身份 `local`，改用 canonical `WorkspaceRoot`；内部 `workspace_key` 仅用于 file store 分片（317–349） | ✅ 比建议更彻底 |
| M4 ToolCallRecord 闭合 | 闭合章节 + sidecar `tool_replay_links.jsonl` + API materialize（1016–1051） | ✅ |
| M5 盲问代价 | "去重与限流"标注 v0 单 tool turn 边界（1666–1671） | ✅ |

### R4（D1–D3）— 主文档已闭环

R4 的建议是“强烈倾向 v0 砍掉 child waiting_human”。R5 主文档选择了另一条路线：**保留 child waiting_human，但把它明确为 v0 最大 runtime 改造点**。这个选择比 R4 建议更重，但现在文档已经补齐了主要语义面。

| R4 问题 | R5 修订 | 结论 |
| --- | --- | --- |
| D1 delegation 同步模型 vs HITL 异步模型 | 增加 `RunInterrupt`、`RuntimeSuspendCollector`、suspended delegate tool call、DelegationJoinState、parent/child continuation、native/ADK 短路规则 | ✅ 闭环，但实现量很大 |
| D2 join=all 下单个 child waiting 可无限阻塞 | 明确 `max_duration_ms` 只计 active execution；deny/cancel materialize `status=canceled`；新增 `max_outstanding` 和 `resume_pending` | ✅ 闭环 |
| D3 parent 信任 child result 边界 | 明确 runtime 只校验 schema/ref/runtime-owned fields，不校验 summary 真伪；`confidence` / `followup_needed` 仅为提示 | ✅ 闭环 |

### R5（E1）— 主文档已闭环

E1 原问题：`第一版实现边界` 与正式 `落地顺序` 不一致。

R6 修订后，主文档的 `第一版实现边界` 已同步为 11 个边界：

- 第 3 步是最小 response API。
- 第 5 步包含 `waiting_human` run status、`RunInterrupt`、`RuntimeSuspendCollector`。
- 第 9 步是 child waiting_human propagation。
- 第 10 步是 native `RequireConfirmation` gate + snapshot replay。
- 文档明确第 3 步是第 9 / 第 10 步的前置条件，不能后置到完整 CLI/API 展示阶段。

结论：E1 已闭环。

## R6 新问题

未发现新的 blocking review 项。

---

## 框架外提醒

R6 关键认知：主文档已经拍板“child waiting_human 进入 v0”，并且实现边界、落地顺序、RunInterrupt、response API 前置关系已经对齐。后续不再是文档设计问题，而是按 suspendable runtime control tool 的标准实现。

因此，后续实现时要把最小边界锁死：

- `waiting_human` 只能通过 `RunInterrupt` 表达。
- `delegate_agent` 一旦 child waiting，必须持久化 suspended tool call 和 DelegationJoinState。
- parent resume 不能靠自然语言总结拼接，必须把 materialized delegate output 注入 parent tool history / AgentHistory 摘要。
- response API 必须早于 child resume 和 snapshot replay 主路径。

---

## Review 结论

```text
结论：主文档的 runtime boundary 方向可以接受。

R1（10）、R2（5）、R3（5）、R4（3）、R5（1）均已闭环。
当前无 P0/P1/P2 blocking review 项。

主文档可以作为 v0 实现依据进入拆分执行。

child waiting_human 进入 v0 是一个可接受但很重的选择。
实现必须先落 `RunInterrupt` / `RuntimeSuspendCollector`，
再改 delegation continuation；不能在现有同步 child FinalResponse 模型上硬补。
```

## R6 优先级建议

1. 进入实现前，以 `RunInterrupt` / `RuntimeSuspendCollector` 为第一批代码契约，不要先改 delegation 同步路径。
2. 实现验证优先覆盖：agent request interrupt、runtime_tool_gate interrupt、child waiting_human interrupt 三条路径。
3. delegation continuation 开工前，先把 response API 的原子 resolve / AgentHistory 摘要写入跑通。
