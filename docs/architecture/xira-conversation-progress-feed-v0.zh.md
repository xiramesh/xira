# Xira Conversation Progress Feed v0

> 日期：2026-06-19（2026-06-20 多轮收敛修订）
>
> 定位：Xira runtime 到用户聊天界面的过程信息投递设计。
>
> 结论：过程信息不是 iLink 私有能力，也不是把 run log 刷到群里。它应该是
> runtime event stream 的一个受控 conversation projection：runtime 负责产生可信事实，
> channel adapter 只负责按平台能力节流、去重和投递。

## 0. v0 范围速查（权威，冲突时以本表为准）

| 项 | v0 决定 |
|---|---|
| forwarder 服务的 channel | **仅 IM：iLink 直聊 + Feishu**（Feishu 用于证明非 iLink 私有能力） |
| v0 conversation 投递 | progress（计入 `MaxMessagesPerTurn` quota）：`silence notice`、`agent.delegate.failed`、`agent.delegate.timeout`；交互信号（独立投递，不计 quota）：`run.waiting_human` |
| `assistant.status` 的 chat projection | **整体 v1+，v0 任何 channel 都不做**（iLink/Feishu 推送噪音不发；WS/XiraGarden 已有 event feed，status 在 inspector/activity 可见，单独做聊天气泡投影是 v1+） |
| `emit_status` 工具 | v0 仍注入、仍产生事件，进 `events.jsonl` 与 XiraGarden inspector/activity；只是不做 chat projection |
| `assistant.final` 事件 | **v0 补发**（修 runtime 既有缺口 + forwarder drain 信号，见 §8.5） |
| 群聊 progress | **v0 不开启**（scope 隔离成本高，直聊先行） |
| entrypoint `progress` YAML | **v0 不开放**，用代码内默认值（v1+ 开放） |
| PROFILE.md 覆盖 | **v0 不参与** |

v1+ 蓝图（§6.2、§9.2、§11、§12 详述，v0 不实现）：`assistant.status` / `tool.long_running` 的
chat projection、XiraGarden chat timeline、entrypoint YAML、群聊 `@bot` 触发。

## 1. 背景

当前 `daming-xira` 暴露的问题是：agent run 真实做了很多事（委派 `code-agent`、等待外部 CLI、
执行 shell、改文件、提交推送），但用户在聊天里只能看到最终回复。细节其实已进入
`.xira/runs/*/events.jsonl`、`audit.jsonl`、`tool_calls.jsonl` 和 session history，但聊天面没有
过程反馈。造成两个体验问题：

1. 长任务期间用户不知道 Xira 是否还活着，不知道卡在模型、工具、委派还是等待人。
2. 最终回答之外的关键过程事实无法在聊天里形成信任，例如「已开始委派代码代理」、
   「代码代理超时，我改为主 agent 继续处理」、「正在跑验证」。

现有地基并非从零：

- `runtime.RuntimeEvent` 已有 `scope`、`correlation`、`visibility`。
- `assistant.status` 已定义为 `conversation=true`。
- `emit_status` 已作为 runtime-owned tool 存在，且不写入长期 session history。
- `channel.OutboundEnvelope` 已定义 `runtime_event`、`assistant_final`、`assistant_delta`、
  `interrupt`、`outbound_message`。
- XiraGarden 和通用 WebSocket 已有 runtime event feed（status 在 inspector/activity 可见）。
- Feishu/iLink runner 当前仍是 final-only：同步 `RunAgent`，结束后只发送 final。

因此 v0 的核心缺口不是「增加一个 iLink 进度消息」，而是补一层给「没有 event feed 的 IM channel」
用的 conversation progress forwarder。

## 2. 外部参照

主流工具大致收敛在同一原则：内部 trace 很丰富，但用户聊天面只展示语义化进度。

| 工具 | 可借鉴点 | Xira 取法 |
|---|---|---|
| Codex / OpenAI streaming | Responses streaming 是 typed event（`response.created`、`response.output_text.delta`、`response.completed`、`error`）。 | 保持 typed runtime events，不把文本流、工具日志和状态混成一种字符串。 |
| Claude Code hooks | 有 `SubagentStart` / `SubagentStop` 等生命周期 hook，hook 输入带 subagent id/type。 | 委派开始/结束是 runtime 事实事件，不靠模型自由叙述。 |
| Claude Code status line | status line 是展示层；输入是结构化 session JSON，输出是短文本，有 debounce。 | chat progress 也应是展示 projection，有节流和取消。 |
| PicoClaw runtime events | event publishing 与 event logging 分离，默认日志只打印安全摘要，不含 payload。 | 继续分离 event bus、run log、activity、conversation。 |

参考资料：

- OpenAI Codex CLI docs: <https://developers.openai.com/codex/cli>
- OpenAI streaming responses docs: <https://developers.openai.com/api/docs/guides/streaming-responses>
- Claude Code hooks docs: <https://code.claude.com/docs/en/hooks>
- Claude Code status line docs: <https://code.claude.com/docs/en/statusline>
- Local PicoClaw docs: `/Users/yinwm/projs/picoclaw/picoclaw/docs/architecture/runtime-events.md`
- Local OpenClaw docs: `/Users/yinwm/projs/openclaw/openclaw/docs/web/webchat.md`、
  `/Users/yinwm/projs/openclaw/openclaw/docs/tools/subagents.md`

## 3. 目标

1. 让没有 event feed 的 IM channel 在长任务中得到用户可读的过程信息。
2. 保持主对话干净：只投递明确允许进入 conversation 的短状态，不投递 raw trace。
3. 区分 agent 主动状态和 runtime 事实事件。
4. v1+ 再让 XiraGarden / WebSocket 做高频 chat projection；v0 IM 走节流后的短消息。
5. 不把 progress 写入 agent session history，不影响下一轮模型上下文。
6. v0 只做 request-bound progress，不做全局 proactive dispatcher。

## 4. 非目标

- 不做 token 级 `assistant_delta`。
- 不把每个 tool started / completed 都发到群里。
- 不把 stdout/stderr、工具参数、文件内容、secret、完整错误堆栈发到 conversation。
- 不让 sub-agent 默认直接对用户说话。
- 不在 channel runner 里硬编码 agent 行为。
- 不要求 Feishu/iLink 支持消息编辑；v0 追加短文本。
- 不一次性实现完整 HookManager。
- v0 不做 `assistant.status` 的 chat projection（见速查表）。

## 5. 核心概念

### 5.1 四个展示面

| 展示面 | 用户 | 内容 | 默认入口 |
|---|---|---|---|
| Conversation | 真实聊天用户 | 短状态、等待人、能力缺口、最终回答 | Feishu/iLink/WebSocket/XiraGarden chat |
| Activity | 操作者/调试者 | 安全摘要级 timeline | XiraGarden activity panel |
| Run Inspector | 开发/运维 | 完整 event、tool、model、payload 摘要 | XiraGarden run inspector / API |
| Audit | 系统审计 | 安全和合规事实 | `.xira/runs/*/audit.jsonl` |

`visibility.conversation=true` 只表示「允许进入聊天面」，不表示必须无条件发送。最终是否发送
还要经过 channel policy、节流、去重、scope 匹配和安全渲染。

### 5.2 两类 progress source

| Source | Authority | 示例 | v0 是否做 chat projection |
|---|---|---|---|
| Agent-authored status | agent 主动表达意图和阶段 | `assistant.status`:「我先检查当前 worktree。」 | 否（v1+） |
| Runtime fact status | runtime 根据真实状态生成 | delegate failed/timeout、waiting human | 是（v0 投递） |

二者都通过 event bus 进入同一个 projection，但 v0 只把 runtime fact 的事件子集投到 IM：异常事实
（delegate failed/timeout）作为 progress，waiting human 作为交互信号（见速查表）。

## 6. 事件分类

把 conversation 投递事件分成三档。**v0 实际只投递：progress 类（silence notice + delegate failed/timeout）
+ 交互信号类（waiting human，见速查表）。** 6.1/6.2 是「哪些 kind 语义上允许进 conversation 面」的分类边界，
6.3 是禁止进 conversation 的；分类 ≠ v0 实现范围。

### 6.1 语义上可直接进 conversation（v0 投递子集见速查表）

| Event kind | Conversation | v0 是否投递 |
|---|---:|---|
| `run.silence_notice` | yes | v0 投递（forwarder timer 产生） |
| `agent.delegate.failed` / `agent.delegate.timeout` | yes | v0 投递 |
| `run.waiting_human` / `human.requested` | yes | v0 投递 |
| `assistant.status` | yes | **v0 不投递，v1+** |
| `capability_gap` | yes | v1+ |
| `assistant.final` | yes | 仅作 drain 信号，不作为 progress 投递（见 §8.5） |

### 6.2 条件投递（v1+ 蓝图）

| Event kind | 条件 | 渲染 |
|---|---|---|
| `agent.delegate.started` | 任务预计较长，或已超过 silence threshold |「已开始委派 `{child_agent_id}` 处理子任务。」|
| `agent.delegate.completed` | 长任务或后台任务 |「子任务已返回结果，正在汇总。」|
| `tool.long_running` | 工具运行超过阈值 |「仍在执行 `{tool}`，已运行约 `{duration}`。」|
| `run.retrying` | provider retry / transient error |「上游响应不稳定，正在重试。」|

### 6.3 不投递到 conversation

| Event kind | 原因 |
|---|---|
| `adk.event` | 太底层，可能含模型/SDK trace。 |
| `model.policy_resolved` | 操作事实，不是用户需要的过程信息。 |
| `context.item.included` | 可能泄漏上下文选择。 |
| `tool.started` / `tool.completed` raw | 频率高且可能泄漏工具名、参数、路径。 |
| `assistant_delta` | 不做 token/文本流。 |
| stdout/stderr | 必须留在 inspector/audit，不进聊天。 |

## 7. Runtime Event Contract 调整

`eventVisibility` 默认规则（unknown event `conversation=false, activity=true, inspector=true,
audit=true`）继续保留。v0 补两个约束：

1. `assistant.status` 保持 `conversation=true, activity=true, inspector=true, audit=false`。
2. runtime fact progress 不能靠默认 unknown event 进入 conversation；必须显式列入 allowlist 或
   显式设置 `Visibility.Conversation=true`。
3. **v0 投递的 runtime fact 事件必须在 `events.go` 的 `eventVisibility` 显式设
   `conversation=true`**：`run.waiting_human`、`agent.delegate.failed`、`agent.delegate.timeout`
   当前都走 default（`conversation=false`），会被 forwarder 的 visibility 过滤 drop，allowlist
   形同虚设。这是一处 runtime 侧改动（与 §8.5 补 `assistant.final` 同性质：定义上该可见的事件，
   实际 visibility 没打开）。渲染必须模板化，payload 不原样输出（见 §14）。
   **`agent.delegate.waiting_human` 不在此列**：child 等人时（`delegation.go:568-595`），parent 会把
   child 的 human request 注入自己的 suspend collector（578-582），从而 parent run 也进入 waiting，
   由 `service.go:481` 发出 `run.waiting_human`。因此「委派子 agent 等人」场景已被 parent 的
   `run.waiting_human` 覆盖，forwarder 不单独处理 `agent.delegate.waiting_human`。

推荐新增 progress-oriented kinds，不复用 raw tool events：

```text
run.started
run.silence_notice
run.waiting_human
tool.long_running
agent.delegate.progress
run.retrying
run.recovered
```

`agent.delegate.*` 现有事实事件可继续存在，但进入 conversation 的应是模板化 projection，不是 raw
event 原样输出。

## 8. 通用 Forwarder

### 8.1 位置

新增包 `apps/xira/internal/channelrunner/progress`。职责：

- 订阅 `runtime.EventBus()`。
- 绑定一次 inbound request 的 target/scope。
- 过滤分两类：`silence_notice` 由 forwarder 内部 timer 直接产生，不经 visibility；其余事件需
  `Visibility.Conversation == true` **且**在 v0 投递集合内才渲染发送。因此投递集合里需过 visibility 的
  fact 事件（progress：`agent.delegate.failed` / `agent.delegate.timeout`；交互信号：`run.waiting_human`）
  必须在 `events.go` 显式标 `conversation=true`（见 §7、Phase 0.5），否则被 default visibility drop。
- 把 runtime event 渲染成 channel-neutral `ProgressMessage`。
- 做节流、去重、最大条数限制。
- 调用 channel runner 注入的 send function。

不放在 iLink runner 里，是因为 Feishu/iLink 都需要同一套策略，平台只提供发送函数和能力声明。
（WebSocket/XiraGarden 已有 event feed，v0 不经此 forwarder。）

### 8.2 接口草案

```go
type Message struct {
    EventID string
    Kind    string
    Text    string
    Level   string
}

type Sender interface {
    SendProgress(ctx context.Context, msg Message) error
}

type Policy struct {
    InitialSilenceThreshold time.Duration
    MinInterval             time.Duration
    MaxMessagesPerTurn      int
    MaxChars                int
}

type Forwarder struct {
    RuntimeEvents *runtime.EventBus
    Policy        Policy
    Sender        Sender
}
```

runner 用法：

```go
forwarder := progress.Start(ctx, progress.Request{
    EventBus: r.runtime.EventBus(),
    Scope:    inboundContext,
    Policy:   policyForChannel("ilink"),
    Sender:   senderFunc(...),
})
resp, err := r.runtime.RunAgent(ctx, req)
forwarder.Stop()
sendFinal(resp.FinalResponse)
```

### 8.3 Scope 匹配

forwarder 必须只转发当前 inbound request 相关事件。匹配顺序：

1. `event.Scope.EntrypointID == request.EntrypointID`
2. `event.Scope.Channel == request.Context.Channel`
3. `event.Scope.ChatID == request.Context.ChatID`
4. `event.Scope.SenderID == request.Context.SenderID`
5. `event.Scope.MessageID == request.Context.MessageID`——inbound 带 MessageID 时**必须**匹配（见下「直聊 scope 硬隔离」）。
6. 对 child run，接受 `event.Correlation.TraceID == parentRunID` 或
   `event.Correlation.ParentRunID == parentRunID`。

v0 难点是 `RunAgent` 返回前 parent `run_id` 才已知。解决：forwarder 启动时先用 inbound scope 匹配；
首个匹配到的 parent `run.started` 记录 `run_id`；之后同时匹配 `run_id`、`trace_id`、
`parent_run_id`、`child_run_id`。

scope 信息不足时，forwarder 必须宁可不发，不要串群。

**直聊 scope 硬隔离**：`EventBus` 是 per-Service 单例。同一用户在直聊里**连发两条**消息时，
EntrypointID + ChatID + SenderID 三者完全相同——只靠这三个字段无法区分两条 turn，在 parent
`run_id` 被发现之前，两个 forwarder 会同时吃下对方 run 的事件。因此 v0 scope 硬性要求：当 inbound
带 `MessageID` 时（IM 消息通常都有），**必须同时匹配 MessageID** 才放行（四者全等）。parent
`run_id` 发现后追加 run_id 匹配作为加固，但不能替代 MessageID——run_id 发现前的那段窗口仍依赖
MessageID 隔离。

- 群聊（`ChatType != direct`）v0 不开启 progress；`@bot` 触发留到 v1，届时引入 request_id 强匹配。
- 极端情况：若某 inbound 确无 `MessageID`，v0 默认**关闭该 turn 的 IM progress**，不冒串群风险。

### 8.4 可靠性前置：EventBus 是 best-effort 投递

这是 v0 最易被忽视、却会打穿设计的约束。`runtime.EventBus.Publish`（`event_bus.go`）对每个
订阅者非阻塞投递：

```go
select {
case ch <- evt:
default: // 满了就静默丢弃，无 error、无日志
}
```

bus 是 per-Service 单例（非 per-run），缓冲仅 64。所有并发 run 的 `adk.event`、`tool.*`、
`context.*` 挤在一条 channel 上。forwarder 消费 goroutine 的真正瓶颈不是读 channel，而是它读完要
**同步渲染并调用 iLink/Feishu 的 HTTP `SendText`**（数百 ms）。一旦它在等网络，期间 bus 突发就
填满 channel，随后的关键事件（v0 里是 `agent.delegate.failed` / `run.waiting_human`）被 `default`
吞掉，**零日志、零告警**。

因此 v0 forwarder 必须做两件事：

1. **读写解耦**：bus 消费 goroutine 只做「读 channel + 入内部队列 + scope 过滤」，**绝不**在此
   goroutine 同步发 channel 消息。内部队列由独立 sender goroutine 消费，调用 `Sender.SendProgress`。
2. **内部队列背压**：满时按 kind 优先级丢弃（保留 delegate failed/timeout、waiting human，丢弃
   重复项），并 `log.Warn` 记录，不静默。

这是 v0 是否「看起来在发、实际随机丢」的分界线。Phase 1 必须有「bus 突发 + 慢 sender」并发测试。

### 8.5 可靠性前置：补发 live `assistant.final` 作为 drain 信号

核实出来的硬事实：`assistant.final` 在 `events.go` 有 visibility 定义（`conversation=true,
activity=false, inspector=true, audit=false`），但 **runtime 从不发布它**（无 `recordEvent` 调用点）。
这是 runtime 的既有缺口，不是 forwarder 该绕开的东西。

**v0 决策：这次就补上 `assistant.final`**。首要理由是修 runtime 既有契约缺口；forwarder 顺带用作
drain 信号（次要收益——它只比 `Stop()` 早几秒停发 progress，避免「silence 紧跟 final」的窄乱序）。

先厘清 forwarder 生命周期（request-bound）：forwarder 存活 = 一次 `RunAgent` 调用。无论 run 以
`completed`、`failed` 还是 `waiting_human` 结束，`RunAgent` 都会返回，runner 随即调
`forwarder.Stop()`。HITL 时 forwarder **会**随本次返回停止，resume 是下一次 request 起新 forwarder
——这是模型本身，不是缺陷。因此「停止 forwarder」由 `Stop()` 兜底在所有情况下都成立。

`assistant.final` 的作用不是「停止 forwarder」（那是 `Stop()` 的活），而是让 sender goroutine
**更早**进入 drain：在 `RunAgent` 返回、`sendFinal` 之前就停发 progress，避免「silence 紧跟 final」
乱序。

为什么选补 `assistant.final` 做 drain、而不是用已存在的 `run.finished`：

- **`assistant.final` 是已定义却未发布的契约事件**，补它是修 runtime 既有缺口；forwarder 顺带用。首要理由。
- **语义精确**：`assistant.final` =「final 回复就绪」，正是「该停发 progress」的时刻。`run.finished`
  =「run 结束」（含 failed/waiting_human），语义过宽，做 drain 还要额外判 `status`。

> 注：早期版本曾以「`run.finished` 会在 HITL 误停 forwarder」为由反对它——该论据不成立：
> request-bound 模型下 HITL 时 forwarder 本就会停，不存在「误停」。撤回该论据。

**实现（runtime 侧，落在 `service.go`）**：在 `run.finished`（约 596 行）之前，仅当
`final != "" && resp.Status == "completed"` 时补发：

```go
if final != "" && resp.Status == "completed" {
    recordEvent("assistant.final", "runtime", "assistant final response ready", map[string]any{
        "final_chars": utf8.RuneCountInString(final),
    })
}
```

约束：

- 采用**白名单**（`== "completed"`）而非黑名单（`!= StatusWaitingHuman`）：failed run
  可以有非空 final（例如 verification 在有内容草稿上判 failed），此时不该发 `assistant.final`
  ——否则 forwarder 会 drain，丢掉最该提示的 delegate failed/timeout 进度。HITL 同理不发。
- HITL / `final` 为空（纯失败）不发：没有 final 要投递，靠 `Stop()` 兜底。
- payload 只放 `final_chars`，**final 全文不进事件**（`resp.FinalResponse` 与 session messages 已存，避免三存）。

**forwarder 停止逻辑（双保险）**：

1. 收到与当前 scope 匹配的 `assistant.final`（按 `run_id`），sender goroutine 立刻 drain：丢队列、
   停 silence timer。
2. `RunAgent` 返回后 `forwarder.Stop()` 兜底——覆盖 HITL/纯失败（不发 `assistant.final`）与 bus
   丢包（`assistant.final` 被 `default` 吞掉，呼应 §8.4）。

**避免 final 重复投递**：`assistant.final` 的 visibility 是 `conversation=true`，但 forwarder 的
progress allowlist **必须显式排除 `assistant.final`**——它只作 drain 控制信号，final 文本仍由
runner 的 `sendFinal` 投递。

为什么不留 v1：`assistant.final` 缺失会让每个下游各自发明「怎么知道 final 来了」的猜测。补上是
runtime 层一次解决、所有消费者共享的真相来源；不补则是在核心契约上留一个会被遗忘、被误修的洞。

### 8.6 技术债命名：全局 bus + 后置 scope 匹配（v0 过渡）

§8.3 的 scope 匹配与 §8.4 的读写解耦之所以存在，根因是 `EventBus` 为 per-Service 全局单例，
事件自带的路由信息不足以让消费者直接归属，只能由 forwarder 事后 reverse-engineer。v0 的
scope 匹配（MessageID 硬隔离、run_id 发现、parent/child correlation）是过渡方案，**不是终态设计**。

**v1+ 架构方向**：runtime 提供 `RunAgent(ctx, req, eventSink chan Event)` 形式的 per-run sink，
让每次调用拿到专属事件通道。届时：

- §8.3 的 scope 匹配整段消失——事件归属由构造保证，串群在构造上不可能；
- §8.4 的「全局 bus 突发吞掉关键事件」收敛为单 sink 背压问题，可独立设策略；
- MessageID 硬隔离这条最容易踩的坑自然消解。

v0 不改动 runtime 调用签名，暂走全局 bus 订阅 + 后置匹配。**扩展 forwarder 的任何人都要意识到
这套匹配逻辑是临时补偿，目标方向是 per-run sink**：不要在 v0 匹配逻辑上叠加更多全局协调
（跨 channel 广播、全局 request_id 强匹配等），那些应在 per-run sink 落地后重新设计。

## 9. 投递策略

### 9.1 默认阈值（v0，IM）

| 策略 | 默认值 | 说明 |
|---|---:|---|
| `InitialSilenceThreshold` | 20s | iLink 直聊：超过阈值仍无 final/progress 才发一条 working notice。 |
| `MinInterval` | 12s | 两条 progress 之间至少间隔 12s，异常事件可绕过。 |
| `MaxMessagesPerTurn` | 2 | IM 推送噪音控制：一个 turn 至多 2 条 **progress**（silence + delegate 异常）。`run.waiting_human` 与 final 属交互信号，独立投递，不计入此 quota。 |
| `MaxChars` | 180 | 聊天平台短消息。 |

**IM 推送噪音约束**：Feishu/iLink 每条 progress 是独立推送（手机震动一次）。v0 把
`MaxMessagesPerTurn` 收窄到 2（仅约束 progress：silence + delegate failed/timeout）。`run.waiting_human`
是交互请求（轮到用户了），与 final 同属必须投递的交互信号，独立于 progress quota，不受 2 条上限限制。
`assistant.status` 等高频项 v0 完全不做 chat projection（见速查表），与推送噪音
无关、与 channel 无关——一律 v1+。

### 9.2 平台策略

v0 第一落点是 **iLink 直聊**；同一个 v0 slice 复用到 **Feishu 直聊**，以证明非 iLink 私有能力
（策略与 iLink 相同）。WebSocket/XiraGarden 已有 event feed，不经此 forwarder。下表是各 channel
目标策略（**v0 实际只落地 iLink 直聊 + Feishu 直聊**，其余 v1+）：

| Channel | v1+ 目标 | 形态 |
|---|---|---|
| WebSocket | enabled | `runtime_event` frame + chat projection，由 client 决定展示。 |
| XiraGarden | enabled | Activity/Inspector 全量 + chat timeline。 |
| Feishu | enabled by entrypoint policy | 卡片或文本短消息，默认节流。 |
| iLink | enabled by entrypoint policy | `SendText` 或 `Push` 短消息，默认节流。 |
| CLI | optional | 默认不打印；未来 `--progress`。 |

### 9.3 Dedupe

去重 key：`request_id + event.kind + normalized_text`。同一 turn 内重复状态丢弃。

关于「final 前后乱序」：按 §8.5，runtime 补发 live `assistant.final`，forwarder 收到后 drain（丢队列、
停 timer）；`forwarder.Stop()` 覆盖 HITL/纯失败/bus 丢包。drain 期间到达的 progress，dedupe key
追加 `final_pending` 标记后丢弃。`assistant.final` 不进 progress allowlist，避免与 `sendFinal`
重复投递。

### 9.4 Error policy

progress 发送失败不能让主 run 失败。runner 记 channel delivery warning，继续等 `RunAgent` final。
只有 final 发送失败才沿用现有错误处理。

## 10. 渲染规范

### 10.1 文案原则

- 短句。只说当前阶段或已发生事实。
- 不暴露内部路径、命令参数、token、完整错误。
- 不用「已经完成」除非 runtime 已确认。不把 status 当 final answer。

### 10.2 示例模板（v0 实际使用的标 ✓）

| 场景 | 文案 | v0 |
|---|---|:---:|
| silence notice |「我还在处理，会在有结果或需要你确认时继续更新。」| ✓ |
| delegate failed |「子任务没有成功返回，我会改用当前上下文继续处理。」| ✓ |
| delegate timeout |「子任务超时，我会继续整理已获得的信息。」| ✓ |
| waiting human |「这里需要你确认后才能继续：`{summary}`」<br>（`summary` 源自 `run.waiting_human` 的 `payload.summary`，见 §14；当前 `service.go:481` payload 仅 `blocked_by`，runtime 需补人话 `summary`）| ✓ |
| delegate started |「已开始委派 `{agent}` 处理子任务。」| v1+ |
| delegate completed |「子任务已返回，正在汇总结果。」| v1+ |
| long tool |「仍在执行 `{tool}`，已运行约 `{duration}`。」| v1+ |
| agent status | 使用 `assistant.status.message`。 | v1+ |
| capability gap |「当前缺少 `{capability}`，我会说明替代方案或需要的配置。」| v1+ |

### 10.3 PROFILE.md 不参与 v0

v0 不开放 PROFILE.md 对 progress 的任何覆盖。原先设想的「profile 用自然语言调状态密度/语气」是
零收益、高耦合的半成品（没人用、不可测）。v0 行为由 runtime 默认 + channel policy 决定：

- runtime 默认注入 `emit_status`，agent 不需声明。
- runtime 默认发 silence notice、delegate failed/timeout 等事实类 progress，以及 waiting human 交互信号。
- channel policy 默认处理节流、去重和安全渲染。

产品验收不依赖任何 profile 文字。将来确有差异化语气需求，再以结构化字段（非自然语言指令）加入，
单独立项。

## 11. Entrypoint 配置（v1+ 蓝图）

v0 用代码内默认值，不开放 YAML。v1+ 计划在 entrypoint policy 加 progress 配置：

```yaml
entrypoints:
  - id: ilink-daming
    channel: ilink
    progress:
      enabled: true
      initial_silence_threshold: 20s
      min_interval: 12s
      max_messages_per_turn: 2
      include_delegate_facts: true
```

v1+ enabled 默认：xiragarden/websocket `true`；feishu/ilink 群聊 `false`、直聊 `true`。

## 12. XiraGarden 设计（v1+ 蓝图）

XiraGarden v0 已消费完整 runtime event stream（Activity/Inspector）。v1+ 增加第二条消费：

1. Conversation chat items：用户消息、progress projection、final answer。
2. Runtime event stream：完整 events（已有），用于 Activity 和 Run Inspector。

UI 行为：progress item 与 final answer 分开显示，可视觉附着同一 run；Activity 显示 delegate
started/completed、tool summaries；Inspector 显示完整 payload 与 correlation；`assistant.status`
不进入 session transcript 的 assistant final history；用户可设置「聊天中显示过程信息」开关。

## 13. Feishu/iLink 设计

Feishu/iLink runner 当前 final-only。v0 改造：

1. 构造 `TurnRequest` 前得到完整 `InboundContext`。
2. 启动 progress forwarder。
3. 同步调用 `RunAgent`。
4. forwarder 监听期间发送短 progress（v0 allowlist）。
5. `RunAgent` 结束后停止 forwarder。
6. 按现有逻辑发送 final。

注意：

- progress 发送失败只 log warning，不影响 run。
- final 为空时仍保留已发 progress，但 runner 记录最终空响应。
- iLink 有 `context_token` 时 progress 也优先 reply，同 final 策略。
- Feishu 卡片失败可降级文本，progress 默认直接文本。

## 14. 安全与隐私

禁止进入 conversation 的内容：raw tool args、raw stdout/stderr、absolute path（除非 final 明确需要
且用户上下文允许）、secret/env/token、未脱敏错误堆栈、model/system prompt、context packet、child
agent raw output、audit-only 事件。

`ProgressRenderer` 只能访问白名单字段：

```text
kind
message
severity
scope.agent_id
scope.child_agent_id
correlation.parent_run_id
correlation.child_run_id
payload.reason
payload.duration_ms
payload.capability
payload.summary
```

payload 默认不可直接拼接到文案。`payload.summary` 是唯一允许直接进文案的人话字段（waiting_human
用），由 runtime 生成、已脱敏；其余 payload 字段必须经模板，不原样拼接。当前 `service.go:481`
的 `run.waiting_human` payload 只有 `blocked_by`（内部原因串）+ `human_requests`（数量），缺人话
`summary`，runtime 需补（取 `interrupt.Reason` 或首条 HumanRequest 描述，经脱敏）。

## 15. 实施计划

### Phase 0: 文档和配置

- 增加本文档与 §0 速查表。
- 明确 `assistant.status`、runtime fact status、raw trace 的边界。
- entrypoint `progress` 配置 schema 列为 v1+，v0 用代码默认值。

### Phase 0.5: 可靠性前置（必须先于 forwarder）

- **runtime 侧**：在 `service.go` 补发 live `assistant.final`（§8.5），仅
  `final != "" && resp.Status == "completed"` 时发（白名单，failed run 即便 final 非空也不发），payload 只放 `final_chars`。
- **visibility（关键）**：在 `events.go` 的 `eventVisibility` 为 `run.waiting_human`、
  `agent.delegate.failed`、`agent.delegate.timeout` 显式设 `conversation=true`。当前它们走 default
  `conversation=false`，会被 forwarder drop，导致 v0 allowlist 一个事件都发不出去（§7）。渲染模板化。
- **EventBus**：维持 non-blocking `default` 时，forwarder 必须读写解耦 + 内部队列 + 优先级丢弃
  （§8.4）。先写并发测试再改实现。
- **forwarder drain**：收到匹配 `assistant.final` 即 drain；`Stop()` 兜底。

### Phase 1: 通用 forwarder + delegate 异常事实

- 新增 `internal/channelrunner/progress` 包。
- 实现：bus 读写解耦、scope matcher、renderer、throttle、dedupe、优先级丢弃。
- **v0 投递：progress（silence notice、delegate failed/timeout）+ 交互信号 waiting_human；不含 `assistant.status`。**
- 单元测试覆盖：conversation allowlist、scope 串群防护、节流、发送失败不影响 run、bus 突发+慢 sender
  不阻塞消费、并发 run 同 bus 不串发。

### Phase 2: iLink 接入（直聊先行）

- iLink runner 在 `RunAgent` 周围启动 forwarder。
- v0 iLink 只发：progress（silence notice 阈值 20s + delegate failed/timeout，`MaxMessagesPerTurn=2`）
  + 交互信号 waiting_human（独立投递，不计入 2 条 quota）。
- 测试 fake sender：delegate failed 先于 final 发送，`adk.event`/raw tool 不发送，final 仍发送。

### Phase 3: Feishu 接入（证明非 iLink 私有能力）

- Feishu runner 复用同一 forwarder，策略同 iLink。
- 测试 fake sender：progress 发送失败仍发 final。

### Phase 4（v1+）: chat projection 与高频状态

- `assistant.status` / `tool.long_running` 的 chat projection（WebSocket/XiraGarden）。
- XiraGarden chat timeline。
- entrypoint `progress` YAML 配置开放。

## 16. 验收标准

1. 一个超过 silence 阈值的 run，即使 agent 没主动 `emit_status`，iLink 用户也能收到至多一条
   「仍在处理」提示。
2. delegate failed / timeout / waiting human 可作为模板化短消息出现在 iLink/Feishu，且不暴露 child
   raw output。
3. `adk.event`、raw tool output、stdout/stderr 不会进入 conversation。
4. progress 不会写入 durable session assistant history。
5. 同一个 turn 的 progress（silence + delegate 异常）不超过 `MaxMessagesPerTurn` 上限。`run.waiting_human` 与 final 是交互信号，不计入此限；即使 progress 已达上限，waiting_human 仍须投递。
6. progress 发送失败不影响 final response 生成。
7. 不同 chat/sender/entrypoint 的 events 不会串发。
8. runtime 在 run 正常产出 final 时发布 live `assistant.final`（`final` 非空且非 `waiting_human`）；
   HITL 与纯失败不发。forwarder 收到后停止发送，不与 `sendFinal` 重复投递。

（`assistant.status` 的 chat projection 是 v1+ 验收项，不在 v0 验收内。）

## 17. 测试计划

Go 单元测试：

```text
apps/xira/internal/channelrunner/progress
  - TestForwarderSendsDelegateFailure        # v0 allowlist: delegate.failed 被发送
  - TestForwarderSendsSilenceNotice          # silence timer 触发
  - TestForwarderDropsNonAllowlistedEvents   # assistant.status / adk.event / raw tool 不发送
  - TestForwarderRequiresMatchingScope
  - TestForwarderDedupesAndThrottles
  - TestForwarderDeliversWaitingHumanRegardlessOfQuota  # waiting_human 是交互信号，progress 达 MaxMessagesPerTurn 上限仍须投递
  - TestForwarderSendErrorDoesNotCancelRun
  - TestForwarderSurvivesEventBusBurst       # 8.4: 慢 sender + bus 突发不阻塞消费、不静默丢 delegate 异常
  - TestForwarderConcurrentRunsDoNotCross    # 全局单 bus 下并发 run 不串发（含同 chat+sender、不同 MessageID 连发，§8.3）
  - TestForwarderDrainsOnAssistantFinal      # 8.5: 收到 assistant.final 后丢队列、停 timer
  - TestForwarderDoesNotRedeliverFinal       # 8.5: assistant.final 不进 allowlist，不重复投递 final
  - TestForwarderStopsOnFallbackWhenNoFinal  # 8.5: HITL/纯失败时 Stop() 兜底停止

apps/xira/internal/runtime
  - TestRunEmitsAssistantFinalOnlyOnCompleted              # 8.5: 仅 final!=空 && !waiting_human 发
  - TestAssistantStatusNotWrittenToSessionMessages         # 扩展既有 status 测试，断言 messages.jsonl 不含 status
  - TestV0FactEventsAreConversationVisible                 # run.waiting_human/delegate.failed/timeout 的 visibility.conversation==true（§7）
```

> `TestAssistantStatusNotWrittenToSessionMessages` 放 runtime 包，不放 session 包——session 包不认识
> `emit_status`，只持久化传入 messages，判定 status 是否泄漏的知识在 runtime 侧。扩展既有
> `TestAssistantStatusToolEmitsStatusEventWithoutPersistingContent`（`service_test.go`）即可。

```text
apps/xira/internal/channelrunner/ilink
  - TestRunnerSendsProgressBeforeFinal
  - TestRunnerDoesNotSendRawRuntimeEvents

apps/xira/internal/channelrunner/feishu
  - TestRunnerSendsProgressBeforeFinal
  - TestRunnerProgressSendFailureStillSendsFinal
```

集成验证：

```bash
GOCACHE=/Users/yinwm/work/flowdeck/.cache/go-build go test ./apps/xira/...
git diff --check
```

手工验证（不碰 profile，只验默认 runtime progress）：

1. 不修改 `~/daming-xira` 的 `daming-agent` profile，发起一个会委派 `code-agent` 的长任务。
2. 确认 iLink 聊天里出现 runtime 默认 progress（silence notice、delegate failed/timeout）与 waiting human 交互信号。
3. 确认 `.xira/runs/*/events.jsonl` 仍保留完整事件。
4. 确认 `workspace/.xira/sessions/*/messages.jsonl` 不含 `assistant.status`（同时有自动化测试）。

## 18. 和既有 PR 的关系

| 已有能力 | 支撑点 |
|---|---|
| Delegation progressive events | `assistant.status`、`delegate_agent`、child run correlation。 |
| Channel contract / adapter mapping | `OutboundRuntimeEvent`、`assistant_final`、capability set。 |
| WebSocket channel | request-bound event/final/interrupt frame。 |
| State layout / chat history | run/session 分离，status 不写入 durable assistant history。 |
| XiraGarden event feed | inspector/activity 消费完整 runtime events。 |

缺的就是 projection 层：把允许进入 conversation 的过程事件，安全、节流、按 scope 发回原 IM channel。

## 19. 决策记录

1. 群聊 progress：**v0 群聊默认完全不开启**（`ChatType != direct`），`@bot` 触发留 v1。原因：全局
   单 bus 下群聊 scope 隔离成本高。
2. progress 文案前缀：**v0 不加固定前缀**，让平台 UI 控制样式。
3. long tool 名称暴露：**v0 不投递 `tool.long_running`**（高频且易泄漏工具名），v1 再决定 allowlist
   工具名 vs「外部工具」。
4. `assistant.status` severity：**整体 v1+ 决策**。v0 不做 status chat projection，severity 暂不议；
   v1+ 默认统一 `info`，`Message.Level` 字段保留。
5. progress 投递形式：**v0 用 `ProgressMessage` + `Sender.SendProgress`**，不强行收敛到
   `OutboundEmitter`，待 v1+ 高频路径落地再统一。
6. final 信号：**已决策补发 live `assistant.final`**（runtime 侧改动，落 `service.go`）。**首要理由是修
   runtime 既有契约缺口**（有 visibility 定义却从不发布）——这是独立于 forwarder 的 gap fix，即使没有
   forwarder 也该补。forwarder 顺带把它用作 drain 信号（次要收益）+ `Stop()` 兜底。`run.finished` 语义
   过宽（HITL/failed 也发），做 drain 需额外判 `status`，不如 `assistant.final` 精确。正确性优先：不为
   省一次 runtime 改动而在每个下游留「怎么知道 final 来了」的猜测。
7. `assistant.status` chat projection：**整体 v1+，v0 任何 channel 都不做**（见速查表）。

## 20. 最小落地路径

1. 先做 §8.4 可靠性前置（bus 读写解耦 + 并发测试），这是地基。
2. 新增 `channelrunner/progress`，处理 silence notice + delegate failed/timeout/waiting human。
   delegate 异常与 silence 同属 Phase 1，不再后置——silence 解决「没反馈」症状，delegate 异常解决
   背景 §1 的「信任」问题，共用同一 forwarder，分开做没意义。
3. 接入 iLink 直聊（痛点来源 `daming-xira`），v0 只发 silence + 异常事实。
4. 同一套 forwarder 接入 Feishu，证明不是 iLink 私有能力。
5. （v1+）`assistant.status` / `tool.long_running` 的 chat projection 与 XiraGarden chat timeline。

这样先解决「干了很久用户不知道干了啥」，同时不污染 run log、session history 和最终回答。
