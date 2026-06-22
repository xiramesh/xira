# Flowdeck / Xira — Agent 工作约定

本文件是所有 AI agent（Claude / Codex / 其它）在本仓库工作时的项目级约定与硬契约。
`CLAUDE.md` 与本文件同源，以本文件为准；改动请改这里。

## 1. Runtime 事件契约（最容易踩坑，务必先读）

事件相关代码在 `apps/xira/internal/runtime/`。设计背景见
`docs/architecture/xira-conversation-progress-feed-v0.zh.md`。

### 1.1 `EventBus` 是 best-effort，不是可靠队列

`event_bus.go` 的 `Publish` 对每个订阅者用非阻塞投递：

```go
select {
case ch <- evt:
default: // 满了就静默丢弃，无 error、无日志
}
```

- per-Service **单例**（非 per-run），所有并发 run 的事件挤在一条 buffered channel 上，缓冲仅 64。
- 因此 **任何订阅者（progress forwarder、XiraGarden feed、未来消费者）必须读写解耦**：
  bus 消费 goroutine 只做「读 channel + 入内部队列 + scope 过滤」，**绝不**在消费 goroutine 里
  做同步 IO（HTTP 发送、磁盘写入）。发送由独立 goroutine + 内部队列承担。
- 内部队列满时按 kind 优先级丢弃，但必须 `log.Warn`，**不能静默**。
- 凡是「靠 bus 事件触发的停止/drain」，都必须有非 bus 的兜底路径（如 `Stop()`），因为停止信号
  本身也可能被 `default` 吞掉。

### 1.2 `assistant.final` —— 当前缺失，已立项补发

- `events.go` 已有 `assistant.final` 的 visibility 定义（`conversation=true, activity=false,
  inspector=true, audit=false`），**但 runtime 从不发布它**（无任何 `recordEvent` 调用点）。
  这是一个既有缺口。
- 已决定补发（见上述设计文档 §8.5、Phase 0.5）：在 `service.go` 的 `run.finished`（约 596 行）
  之前，**仅当 `final != "" && resp.Status != StatusWaitingHuman`** 时发，payload 只放
  `final_chars`，final 全文不进事件。
- 语义：`assistant.final` = 「agent 的最终回复已就绪」。它是下游（forwarder、XiraGarden 收尾、
  未来 CLI/WS）判断「final 来了，该收尾」的**契约化信号**，不是 v1 可选物。
- 在它真正补发落地之前，**不要在代码里假设它会被发布**。

### 1.3 `run.finished` ≠ final 就绪信号

`service.go` 的 `run.finished` 是**无条件**发的：`completed` / `failed` / `waiting_human` 都发。

- request-bound 模型下，无论哪种状态 `RunAgent` 都会返回、forwarder 都会随之 stop；HITL 时停掉、
  resume 另起新 forwarder 是模型本身，不是「误停」。所以 `run.finished` 用来「停 forwarder」没有错。
- 但 **`run.finished` 不代表「final 已就绪」**——它在 HITL/纯失败时也发，而那些情况没有 final。
  需要「final 已就绪」语义时（如 forwarder 提前 drain、避免 progress 与 final 乱序），用
  `assistant.final`（见 1.2）。两者职责不同：`run.finished` = run 结束；`assistant.final` =
  final 回复就绪。

### 1.4 fact 事件的 visibility 默认是 false（陷阱）

`eventVisibility`（`events.go`）的 switch 只显式处理 7 个 kind：`assistant.status`、
`assistant.final`、`adk.event`、`model.policy_resolved`、`context.packet.started`、
`context.item.included`、`capability_gap`。**其余全部走 default `conversation=false`**。

`run.waiting_human`、`agent.delegate.failed`、`agent.delegate.timeout`、
`agent.delegate.started/completed` 这些「事实事件」确实被 `recordEvent` 发布，但 visibility 是 false。

含义：任何想把它们投递到 conversation 面的消费者（progress forwarder 等），**不能只过滤
`Visibility.Conversation==true`**——那会把这些事件全部 drop。要么先在 `events.go` 显式设
`conversation=true`，要么 forwarder 规则写成 `conversation=true OR fact allowlist`。v0 计划走前者
（见设计文档 §7、Phase 0.5）。这是与 1.2 同性质的缺口：事件被发布了、语义上该对用户可见，但
visibility 没打开。

## 2. 设计评审方法论（本次复盘总结）

- **先核实，再判断。** 评审设计文档时，文档声称存在的代码契约（事件、接口、字段）必须先
  `grep` / 读源码核实，再下结论。本次正是靠核实发现 `assistant.final` 从不发布、`run.finished`
  无条件发，否则会基于错误前提做设计。
- **缺口要补，不要绕。** 遇到缺失的契约或技术债（如不发布的 `assistant.final`），正确做法是
  补上，而不是用「已有的近似物」（如 `run.finished`）凑合绕过。用替代品绕 = 留下一个会漂移、
  会被遗忘、会被下一个 agent 当 bug 重修的洞。只有当绕开的代价明确小于补的代价、且不污染核心
  契约时，才考虑绕；核心契约上的缺口永远补。
- **silent data loss 是最贵的 bug。** 像 `EventBus` 的 `default` drop 这种「不崩、只随机失效」
  的设计，必须配套并发测试（突发 + 慢消费者）和显式告警，否则测试全绿、线上偶发。
- **正确性 > 可读性 > 一致性 > 简单性。** 当冲突点落在核心契约上时，简单性要让位——「省一次
  runtime 改动」换「N 个下游各背一个 undocumented 特例」是亏本交易。

## 3. 仓库导航

- 运行时核心：`apps/xira/internal/runtime/`（`service.go`、`event_bus.go`、`events.go`、
  `delegation.go`）
- channel 适配与 runner：`apps/xira/internal/channel/`、`apps/xira/internal/channelrunner/`
- 架构文档：`docs/architecture/`
- 构建/测试入口：`Taskfile.yml`、`go.work`

## 4. 通用工作哲学（与全局约定一致，此处不重复展开）

增量优于全部重构；代码要清晰而非聪明；YAGNI / KISS；单文件不超过 600 行；
规划 → 测试 → 实现 → 重构 → 提交。

## 5. 代码与测试硬规则（适用于所有代码改动，无例外）

这三条是**所有**代码改动的硬契约，不限于某个 phase 或某个模块。违反任何一条
都算未完成。

### 5.1 TDD 先行

- **先写失败测试，再写实现**。测试定义契约（red），实现让测试通过（green），
  最后重构（refactor）。不允许"写完实现再补测试"。
- 重构已有代码时同样适用：先确认现有测试覆盖了被改动的行为，改动后测试仍绿。
- 如果一条行为没有可写的测试，先问"这条行为是否必要"——YAGNI；如果必要，先
  想清楚怎么测，再写。

### 5.2 每个模块的测试覆盖率 ≥ 85%

- 用 `go test -coverprofile` + `go tool cover` 按语句精算，不按函数计数（零语句
  空方法在 `go tool cover -func` 下显示 0% 是工具假象，按语句精算不计分母）。
- **85% 是下限不是目标**——关键契约代码（状态机、sealed 穷尽、Filter 匹配等）
  应追求 100%。
- 覆盖率不达标 = 该模块未完成，不得提交。

### 5.3 用真 API key 跑 live 测试，不用 mock

- `DEEPSEEK_API_KEY` 在仓库根目录的 `DEEPSEEK_API_KEY` 文件里。注入方式：
  `export DEEPSEEK_API_KEY="$(cat DEEPSEEK_API_KEY)"`。
- 涉及 LLM 的测试用真 key 跑，不用 mock。mock 只用于纯单元测试里不涉及外部
  服务的部分（如纯逻辑、类型契约）。
- **这条防止"mock 全绿但 live 挂"的假性通过**——live 测试是真实性的最后一道闸。
