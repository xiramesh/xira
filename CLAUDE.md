# Flowdeck / Xira

本项目所有 AI agent（含 Claude）的工作约定、runtime 契约与评审方法论，统一记录在
[`AGENTS.md`](./AGENTS.md) —— **以 AGENTS.md 为准，本文件不再重复维护**，改动请改 AGENTS.md。

进仓库后至少记住这两条（最容易踩、本次花代价才搞清的）：

1. **`EventBus` 是 best-effort**（`event_bus.go` 满载时按优先级驱逐 + `log.Warn`，从不静默；
   per-Service 单例，buffer 256）。任何订阅者必须读写解耦，不能在消费 goroutine 里做同步 IO。
   详见 AGENTS.md §1.1。
2. **`assistant.final` 已发布，是成功时的白名单信号**（`final != "" && status == "completed"` 才发，
   不是 blacklist；HITL/failed 不发）。`run.finished` ≠ final 就绪信号（HITL/failed 也发）。
   详见 AGENTS.md §1.2 / §1.3。

其余（评审先核实再判断、核实方法本身也要核实、缺口要补不要绕、silent data loss 最贵、
TDD/覆盖率/真 key 硬规则等）见 AGENTS.md §2 / §5。
