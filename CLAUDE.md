# Flowdeck / Xira

本项目所有 AI agent（含 Claude）的工作约定、runtime 契约与评审方法论，统一记录在
[`AGENTS.md`](./AGENTS.md) —— **以 AGENTS.md 为准，本文件不再重复维护**，改动请改 AGENTS.md。

进仓库后至少记住这两条（最容易踩、本次花代价才搞清的）：

1. **事件投递走 per-chat-key Sink（无全局 bus）**（Phase 6b 删了 EventBus；`dispatchEvent`
   把 Event 直接 Deliver 到 ctx 里的 `EventSink`/`ChatContext`，per-chat-key 隔离）。
   sink==nil 时 signal 会被丢（有 Debug log）。详见 AGENTS.md §1.1。
2. **`assistant.final` 已发布，是成功时的白名单信号**（`final != "" && status == "completed"` 才发，
   不是 blacklist；HITL/failed 不发）。`run.finished` ≠ final 就绪信号（HITL/failed 也发）。
   详见 AGENTS.md §1.2 / §1.3。

其余（评审先核实再判断、核实方法本身也要核实、缺口要补不要绕、silent data loss 最贵、
TDD/覆盖率/真 key 硬规则等）见 AGENTS.md §2 / §5。
