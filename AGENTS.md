# Flowdeck / Xira — Agent 工作约定

本文件是所有 AI agent（Claude / Codex / 其它）在本仓库工作时的项目级约定与硬契约。
`CLAUDE.md` 与本文件同源，以本文件为准；改动请改这里。

## 1. Runtime 事件契约（最容易踩坑，务必先读）

事件相关代码在 `apps/xira/internal/runtime/`。设计背景见
`docs/architecture/xira-conversation-progress-feed-v0.zh.md`。

### 1.1 `EventBus` 是 best-effort，不是可靠队列

`event_bus.go` 的 `Publish` 对每个订阅者**非阻塞投递**：缓冲满时按 kind 优先级**驱逐 +
`log.Warn`，从不静默**（`subscriber.enqueue` + `oldestBelowLocked`，`subscriberBufferSize = 256`）。

- per-Service **单例**（非 per-run），所有并发 run 的事件挤在同一个订阅者的 buffered 切片上，
  容量 **256**（`const subscriberBufferSize = 256`，event_bus.go:13）。
- 优先级三档（`eventPriority`，event_bus.go:138）：`critical`（`run.waiting_human` /
  `assistant.final`）> `important`（`agent.delegate.failed` / `agent.delegate.timeout`）>
  `droppable`（其余）。满载时新事件可驱逐严格更低优先级的最老事件；同级或更低的新事件被丢。
  **两种情况都 `slog.Warn`，不静默**（`enqueue` 内）。
- 因此 **任何订阅者（progress forwarder、XiraGarden feed、未来消费者）必须读写解耦**：
  bus 消费 goroutine 只做「读 channel + 入内部队列 + scope 过滤」，**绝不**在消费 goroutine 里
  做同步 IO（HTTP 发送、磁盘写入）。发送由独立 goroutine + 内部队列承担。
- 凡是「靠 bus 事件触发的停止/drain」，都必须有非 bus 的兜底路径（如 `Stop()`），因为停止信号
  本身也可能在满载时被驱逐/丢弃。

### 1.2 `assistant.final` —— 已发布，成功时的白名单信号

- `service.go` 在 `run.finished` 之前**已发布** `assistant.final`（service.go:610），条件是
  **whitelist（白名单）**：`final != "" && resp.Status == "completed"`。
- **不是 blacklist**——不是"只要不是 waiting_human 就发"。失败 run 即使 `final` 非空（如
  verification 失败但草稿已生成）也**不发**：那种情况下 forwarder 队列里的 `delegate.failed`/
  `timeout` 进度正是用户需要的信号，drain 掉会丢。见 service.go:600-608 注释和回归测试
  `TestRunDoesNotEmitAssistantFinalOnFailed`。
- HITL（`waiting_human`）不发：没有 final 就绪。
- payload 只放 `final_chars`（rune 数），final 全文不进事件。
- 语义：`assistant.final` = 「agent 的最终回复已就绪」。它是下游（forwarder、XiraGarden 收尾、
  未来 CLI/WS）判断「final 来了，该收尾」的**契约化信号**。
- visibility：`events.go` 已定义 `conversation=true, activity=false, inspector=true, audit=false`。

### 1.3 `run.finished` ≠ final 就绪信号

`service.go` 的 `run.finished` 是**无条件**发的：`completed` / `failed` / `waiting_human` 都发。

- request-bound 模型下，无论哪种状态 `RunAgent` 都会返回、forwarder 都会随之 stop；HITL 时停掉、
  resume 另起新 forwarder 是模型本身，不是「误停」。所以 `run.finished` 用来「停 forwarder」没有错。
- 但 **`run.finished` 不代表「final 已就绪」**——它在 HITL/纯失败时也发，而那些情况没有 final。
  需要「final 已就绪」语义时（如 forwarder 提前 drain、避免 progress 与 final 乱序），用
  `assistant.final`（见 1.2）。两者职责不同：`run.finished` = run 结束；`assistant.final` =
  final 回复就绪。

### 1.4 `eventVisibility` 的 switch：8 个 case，其余走 default

`eventVisibility`（`events.go`）的 switch 显式处理 **8 个 case**：

| kind | conversation |
|---|---|
| `assistant.status` | true |
| `assistant.final` | true |
| `run.waiting_human` / `agent.delegate.failed` / `agent.delegate.timeout` | **true**（已打开，不再是缺口）|
| `adk.event` | false |
| `model.policy_resolved` | false |
| `context.packet.started` | false |
| `context.item.included` | false |
| `capability_gap` | true |

**其余 kind 全走 default**（`conversation=false, activity=true, inspector=true, audit=true`）。

含义：曾经有个缺口——`run.waiting_human` / `agent.delegate.failed` / `agent.delegate.timeout` 这
些 fact 事件被发布但 `conversation` 没打开，只过滤 `Conversation==true` 的 forwarder 会 drop 它们。
**这个缺口现已补上**（上述 case 已 `conversation=true`，见 events.go 注释"v0 progress forwarder
delivers these runtime-fact kinds into IM chat"）。**但仍要警惕**：新加的 fact kind 如果语义上该
对用户可见，必须显式在 switch 里开 `conversation=true`，否则会走 default 被静默 drop——这正是
AGENTS.md §2 "缺口要补不要绕" 的典型场景。

## 2. 设计评审方法论（本次复盘总结）

- **先核实，再判断。** 评审设计文档时，文档声称存在的代码契约（事件、接口、字段）必须先
  `grep` / 读源码核实，再下结论。**核实方法本身也要核实**——`grep` 的正则、路径、锚定如果有
  盲区（比如只匹配 `recordEvent("literal")` 漏掉 `kind="..."; recordEvent(kind,...)` 动态形式），
  "我 grep 过了"也是没核实。见 §5.4。
- **写规则/契约文档时，核实纪律更要加码。** 本文件 §1 曾凭记忆写了多处与代码不符的断言
  （buffer 64 实际 256、assistant.final "从不发布"实际已发布、visibility "7 个 case" 实际 8 个），
  栽在"禁止不核实"的规则文件本身上。模式很清楚：**代码实现时 TDD 严，一到"总结经验/固化契约"
  核实纪律就垮**。下次写"X 很重要"的规则文档时，先把文档里每一个事实断言 grep 一遍——尤其
  track 进 git 的，因为 track 赋予权威，带错的权威比没有更糟。
- **缺口要补，不要绕。** 遇到缺失的契约或技术债，正确做法是补上，而不是用「已有的近似物」
  凑合绕过。用替代品绕 = 留下一个会漂移、会被遗忘、会被下一个 agent 当 bug 重修的洞。只有当
  绕开的代价明确小于补的代价、且不污染核心契约时，才考虑绕；核心契约上的缺口永远补。
- **silent data loss 是最贵的 bug。** 像 `EventBus` 满载驱逐这种「不崩、只随机失效」
  的设计，必须配套并发测试（突发 + 慢消费者）和显式告警，否则测试全绿、线上偶发。
- **正确性 > 可读性 > 一致性 > 简单性。** 当冲突点落在核心契约上时，简单性要让位——「省一次
  runtime 改动」换「N 个下游各背一个 undocumented 特例」是亏本交易。
- **暴躁审视（双向）。** 每次都用审视的目光看用户输入的**潜在问题**，犀利的指出，并给出明显
  在用户思考框架之外的建议。觉得用户说的太离谱，就**骂回来**，帮用户瞬间清醒——而不是顺从。
  这条**双向**适用：
  - 对用户：不盲从。用户的指令可能含未审视的假设、倒果为因、范围含糊、或者干脆错的。agent 的
    职责不是执行，是**先把指令审视清楚**，发现问题就顶回，别等用户自己发现。
  - 对自己：agent 自己的判断同样要审视——尤其"我核实过了""我查过了"这种带正当感的自信场景
    （见 §5.4）。顺从自己的判断 = 顺从一个没被审视的输入。
  **不顺从 ≠ 找茬**。顶回要基于证据（grep / 读码 / 逻辑），不是为了显得犀利而杠。骂的前提是
  真有问题，且骂完要给建设性的修正方向。

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
- **Enforcement**：目前靠作者自测 + reviewer 核查（`go test -coverprofile` + 按语句精算）。
  CI 自动卡控（coverage gate）是 TODO——在它落地前，PR 描述必须附覆盖率数字，reviewer 必须独立
  复跑核实，不能只信描述。

### 5.3 用真 API key 跑 live 测试，不用 mock

- live 测试有**双门控**，两个环境变量缺一不可（`deepseek_hitl_live_test.go` 等）：
  1. `DEEPSEEK_API_KEY`——仓库根目录的 `DEEPSEEK_API_KEY` 文件里。注入：
     `export DEEPSEEK_API_KEY="$(cat DEEPSEEK_API_KEY)"`
  2. `XIRA_DEEPSEEK_LIVE=1`——显式开关。注入：`export XIRA_DEEPSEEK_LIVE=1`
- **漏设 `XIRA_DEEPSEEK_LIVE=1` 时 live 测试会 `t.Skip`，测试照常"全绿"但实际没跑**——这正是
  本条要防的"静默绿"。所以每次提交前确认两个变量都设了，且测试输出里**没有 `SKIP` 行**。
- 涉及 LLM 的测试用真 key 跑，不用 mock。mock 只用于纯单元测试里不涉及外部服务的部分（如纯
  逻辑、类型契约）。
- 完整命令模板：
  ```
  export DEEPSEEK_API_KEY="$(cat DEEPSEEK_API_KEY)"
  export XIRA_DEEPSEEK_LIVE=1
  go test ./... -v 2>&1 | grep -E 'SKIP|FAIL|ok'   # 确认无 SKIP
  ```

### 5.4 核实方法本身也要核实（verify the verifier）

§5.1-5.3 讲的是"要核实"。本条讲的是：**核实方法如果选错了，等于没核实**。

- 核实代码契约时，先问"我的核实方法会不会漏"。典型盲区：
  - 只 `grep recordEvent("literal")` → 漏掉 `kind = "..."; recordEvent(kind, ...)` 动态形式（PR #32
    正是栽在这，`agent.delegate.timeout` 被字面量 grep 漏掉）
  - 核实行号引用时读本地脏工作树 → 行号因未提交改动偏移（PR #30 栽在这：读本地脏工作树以为
    `ephemeral_worker:` 赋值在 `delegation.go:996`，实际 origin/main 上在 977）。**引用代码用符号名
    （函数名 / 字符串字面量），不用行号**——行号会漂移，符号名稳定
  - `grep` 的正则太宽（`[^"]+`）→ 匹配到 grep 自身的注释文本，产出伪命中
  - **宣布修复 ≠ 实际提交** → commit message 写了"修了 types.go"，但 `git add` 漏了这个文件，
    实际 staged set 不包含它（本 PR commit 7874b7e 栽在这：message 说修了 cross-package compile，
    但 progress/types.go 没进 commit，推上去的 PR 会炸 main）。**提交后核实 `git show --stat HEAD`
    真的包含修改的文件**，不只信 commit message 的描述。重要修改让 reviewer/CI 二次确认，不靠自查。
- 反射动作：每次核实后追问两句——"我的 grep/读法会不会漏？" + "我宣布修的，实际提交了吗？"。
  不满意就加一路（如双路 grep、`git show --stat HEAD`）。
- **这条尤其适用于"自信的场景"**：写规则文档、固化经验、做架构总结时，人（和 agent）最容易
  跳过核实。本文件 §1 曾连续多处凭记忆写错（buffer 数、发布状态、case 数），栽在"禁止不核实"
  的规则文件本身上。详见 §2 的元教训。
