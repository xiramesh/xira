# Xira Agent HITL v0 设计 Review

> 对象文档：`docs/architecture/xira-agent-hitl-v0.zh.md`
> 评审基准：分支 `feature/agent-hitl-v0` 当前代码（`apps/xira/internal/runtime/{types.go,service.go,service_adk.go,config.go}`、`apps/xira/internal/tools/registry.go`）
> 评审历史：
> - **R1**（首轮）：基于原始草案，提出 P0/P1/P2 共 10 项问题。
> - **R2**（第二轮）：文档采纳 R1 全部意见，R1 闭环；针对修订版提出 N1–N5。
> - **R3**（本轮）：文档采纳 R2 全部意见并主动引入 `workspace_id`，R2 闭环；针对修订版提出 M1–M5。主线架构已收敛，剩余均为实现层工程洞。

## 总体评价

文档经过三轮修订，**主线架构已收敛得很干净**：

- 协作型 / 强制型 HITL 双层，`request_source` / `trust_level` 分流。
- `runtime_tool_gate` approval 走 snapshot + 同步 replay，`agent_request` approval 走 agent 继续。
- native / ADK 共享同一份 HITL store + hydration。
- 强制型 approval 的信任默认值收敛到 transport_authenticated。
- 主动引入 `workspace_id` 分离"运行中断点"与"资源/权限归属"。

R3 剩余问题（M1–M5）**全部是实现层工程洞，不是架构洞**。但其中 M1（replay 并发状态机）和 M3（workspace_id 载体）是两个会卡住整个 replay 链路的硬依赖，建议先拍板再进落地顺序第 1 步。

严重度分级沿用：

```text
P0 必须在 v0 实现前解决
P1 影响 v0 可用性/安全,强烈建议同步处理
P2 边界与文档完整性,可在落地顺序里补
```

---

## R1 闭环确认

R1 提出的 10 项问题已在文档第二轮修订中处理（详见下表），R3 不再重复展开。

| R1 问题 | 结论 |
| --- | --- |
| P0-1 native 非 loop / session 回灌前提 | ✅ |
| P0-2 v0 是"绅士协议"，安全增益≈0 | ✅ |
| P1-1 `RequireConfirmation` 已存在却不接 | ✅ |
| P1-2 "不自动重放"可能选反 | ✅ |
| P1-3 approval 语义塌缩 | ✅ |
| P1-4 `human.signal` 来源可信度矛盾 | ✅ |
| P2-1 status 边界 | ✅ |
| P2-2 allowlist 冲突 | ✅ |
| P2-3 无限追问 | ✅ |
| P2-4 跨 turn 状态恢复 | ✅ |

---

## R2 闭环确认

R2 提出的 N1–N5 已在文档第三轮修订中处理：

| R2 问题 | 修订位置 | 结论 |
| --- | --- | --- |
| N1 replay 死循环 | 新增"replay execution mode"（643–667）：`executeReplay` + `replay=true` bypass RequireConfirmation，**保留** allowlist/path/timeout/audit | ✅ 精准——只跳 gate，不跳其他校验 |
| N2 approve 后悬空 | "responses API 契约"（764–808）同步触发 replay；跨 turn 中的 replay **降级为补偿路径**（1157），只处理崩溃 | ✅ 主路径同步 + 补偿路径异步，拆法正确 |
| N3 channel 默认值 | 信任表（96）改为"v0 默认不可以，需 per-channel 开启"；已定决策 928 | ✅ 默认值方向正确 |
| N4 ADK hydration | 新增"ADK Hydration"（961–990），通过 AgentHistory + session message kind 共享同一份 HITL store | ✅ 未另起一套状态 |
| N5 compaction | 滑动窗口 K=20 / max_history_chars=24000（581–602），明确不做语义 compaction | ✅ 截断边界清晰 |

另：文档主动引入 `workspace_id`（把运行中断点与资源归属分离），方向正确。

R2 全部闭环。以下为 R3 新增问题。

---

## R3 新增问题

### M1（P0）：同步 replay 引入新的并发安全洞——`replay_status` 缺 `running` 中间态

N2 的解法是"responses API 同步触发 replay"。这带来一个文档未处理的新问题。

`replay_status` 当前只有 `pending / completed / failed`（189、662 行），**没有 `running`**。考虑以下序列：

```text
POST /responses approve
  -> runtime 看到 replay_status=pending，开始执行 replay（git push，耗时 40s）
  -> HTTP 客户端 30s 超时，连接断开
  -> 客户端重试 POST /responses approve（或用户再点一次）
  -> runtime 仍看到 replay_status=pending（第一次还没写完）
  -> 第二次也执行 replay
  -> 违反"replay 必须最多执行一次"（1162）
```

这是 N2 解法自己制造的并发洞：同步执行可能很慢的 tool + 可重试的 HTTP + 无中间态的状态机 = 并发重放。

更隐蔽的是，它**重新制造了 N2 想消灭的悬空**，只是从"approve-signal 悬空"变成"replay-result 悬空"——HTTP 超时后客户端拿到 connection error，但 replay 仍在后台跑，用户不知执行了没有。补偿路径（RunAgent start 检查 `replay_status=pending` 重试，1152–1157）只覆盖"进程崩溃"，**不覆盖"还在跑"**——进程没崩、只是 HTTP 超时，后台那次的 status 仍是 pending，又触发一次。

必须修：

```text
replay_status: pending -> running -> completed/failed
POST /responses 抢占式 CAS 把 pending 置 running（失败说明已有别人在跑）
running 期间的重试请求直接返回 replay 进行中状态，不重复执行
```

至少补 `running` 中间态 + 原子抢占。否则"最多一次"在同步 HTTP 下保证不了。

### M2（P1）：`input_hash` 是装饰性字段，威胁模型不成立

`replay execution mode` 要求 "snapshot input_hash matches persisted input"（656 行）。但 persisted input 就是 snapshot 自己——`input_hash` 必然匹配它对应的 `input`。在本地 file store 下，能改 input 的人就能改 hash，这个检查不防任何实际威胁。

真正有价值的漂移检测是"批准时的环境" vs "replay 时的环境"（workspace revision / git HEAD），而那个恰被放进"待确认问题 3"。也就是说：**该有的环境漂移检测没定，不该有的 input 自洽检查占了字段。**

建议二选一：

1. 删掉 `input_hash` 的校验语义（保留可无）；或
2. 改名为 `env_hash`，明确它检测的是 cwd/git HEAD 等环境漂移，并给出计算口径。

留着现在的 `input_hash` 会误导实现者以为 replay 有防篡改能力——实际没有。

### M3（P1）：`workspace_id` 引入了身份概念，但 runtime 没有对应载体

核对代码后发现的真实落地缺口。文档新增 `workspace_id` 顶层必填（31、135、258–274 行），示例为 `workspace_id: local`。但代码现状：

- runtime 里的 `workspace` 是 **`WorkspaceRoot` 文件路径**（`service.go:52` `s.workspace`、`config.go:67`），不是身份标识符。
- `TurnRequest` **没有** `workspace_id` 字段（`types.go:16–24` 已核实为空）。
- 那么 HumanRequest 创建时（`executeToolCall` / `human.request` tool）从哪个上下文拿到 `workspace_id`？

示例的 `local` 暗示 v0 单 workspace 硬编码。这属于"概念引入了、载体未定义"。必须现在回答三件事，否则该字段在 v0 是空壳：

1. v0 是否假设单 workspace（固定值 `local`）？
2. `workspace_id` 从哪解析——`TurnRequest` 要不要加字段？还是从 `entrypoint_id` / `channel` 推导？
3. 未来多 workspace 时谁分配 id、与 `WorkspaceRoot` 的关系？

别让它变成"必填但永远填 `local`"的僵尸字段。M3 还会反向影响 file store：human-requests 要不要按 `workspace_id` 分目录？这关系到落地顺序第 1 步。

### M4（P2）：被拦截的 ToolCallRecord 与 replay result 如何闭合

正常 `command.run` 走 `executeToolCall` 产生 `ToolCallRecord`，output=`waiting_human`。replay 走 `executeReplay`，结果进独立 replay artifact（800 行 `replay.artifact`）。**原 ToolCallRecord 要不要回填 replay ref？** 审计时若看到"command.run → waiting_human"还要另去 replay artifact 找结果，两套记录容易对不上。

建议明确闭合关系：原 ToolCallRecord 在 replay 完成后回填 `replay_ref`（指向 replay artifact），保持单条 run log 可完整复盘，而非让审计者在两个文件间跳。

### M5（P2，次要）：native 2-call + "一个 run 一个 pending" 的交互代价

"一个 run 内最多一个 pending，创建后短路"（1131）+ native 固定 2 次 model call，意味着：第一次 call 若同时返回 `read_file` + `human.request`，collector 短路时 `read_file` 已执行，但其结果**没机会参与 agent 决定"怎么问"**——agent 实际是盲问。

这是 native 架构的固有局限，文档未点明 UX 代价。建议至少在落地顺序里标注："v0 agent_request 以单 tool turn 为主，多 tool + ask 的组合需等 hydration/多轮增强"，避免实现者发现后回头改设计。

---

## 框架外提醒

到 R3，文档主线设计已收敛。剩余问题（M1–M5）全是实现层工程洞。这其实是设计文档进入"危险安全区"的信号：架构看着都对，容易误判为"可以开工了"。

建议**先别急着进落地顺序第 1 步**。M1（replay 并发状态机）和 M3（workspace_id 载体）是两个会卡住整个 replay 链路的硬依赖：

- M1 不定，snapshot replay 写不出来（"最多一次"无法保证）。
- M3 不定，连 HumanRequest 的 file store 路径都落不了（按 `workspace_id` 分目录还是不分？）。

把这两个拍板，再开工，能省掉返工。

---

## 一句话总结

```text
R1（10 项）、R2（5 项）全部闭环，主线架构收敛干净。
R3 剩余 5 项均为实现层工程洞：
  - M1（P0）replay 并发状态机缺 running 中间态 —— 同步 replay 的"最多一次"无法保证
  - M3（P1）workspace_id 概念引入但 runtime 无载体 —— file store 路径都落不了
  - M2（P1）input_hash 是装饰性字段，威胁模型不成立
  - M4（P2）被拦截 ToolCallRecord 与 replay result 闭合关系未定义
  - M5（P2）native 2-call + 单 pending 的盲问代价未点明
建议先拍 M1、M3 再落地。
```

## R3 优先级建议

1. **M1** 给 replay_status 补 `running` 中间态 + CAS 抢占，定清"最多一次"的并发保证。
2. **M3** 定 workspace_id 的载体（TurnRequest 字段 / 推导方式 / 是否单 workspace），并决定 file store 是否按 workspace_id 分目录。
3. **M2** `input_hash` 删除或改名为 `env_hash` 并定义环境漂移检测口径。
4. **M4** 定 ToolCallRecord 与 replay artifact 的闭合 ref 关系。
5. **M5** 在落地顺序标注 v0 agent_request 的单 tool turn 边界。
6. 视交付压力，重新评估落地顺序——M1/M3 未定前，第 4、5 步（RequireConfirmation gate + snapshot replay）无法真正完成。
