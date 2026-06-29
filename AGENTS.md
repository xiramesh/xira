# Flowdeck / Xira — Agent 工作约定

本文件是所有 AI agent（Claude / Codex / 其它）在本仓库工作时的项目级约定与硬契约。
`CLAUDE.md` 与本文件同源，以本文件为准；改动请改这里。

## 1. Runtime 事件契约（最容易踩坑，务必先读）

事件相关代码在 `apps/xira/internal/runtime/`。设计背景见
`docs/architecture/xira-conversation-progress-feed-v0.zh.md`。

### 1.1 事件投递:per-chat-key Sink(无全局 bus)

> **Phase 6b(#60)更新**:全局 per-Service `EventBus` 已删除。事件投递走
> per-chat-key `EventBus`(`ChatContext`),通过 `context.Value` 注入,直接 `Deliver`。
> 原来的 `event_bus.go`(buffer 256 + 优先级驱逐)已删,本节描述的是**新投递模型**。

事件相关代码在 `apps/xira/internal/runtime/`(`dispatchEvent`、`event_dispatch.go`、
`event_sink.go`)。设计背景见 `docs/architecture/xira-per-chat-key-architecture-rfc-v0.zh.md`。

`dispatchEvent(ctx, evt)` 是事件投递的唯一入口(所有 `recordEvent`/`recordChildEvent`
闭包都调它)。它把 `RuntimeEvent` 映射成 `Event`(sealed),然后投递到 `ctx` 里的
`EventBus`(per-chat-key 的 `ChatContext`)。**没有全局 bus**——投递是点对点的
(sink 在 ctx 里,`Deliver` 直接调)。

- **per-chat-key 隔离**:每个 turn 的 ctx 携带自己的 `EventBus`(`Router.Handle` 注入)。
  不同 chatKey 的事件天然隔离,不需要 scope 匹配。
- **bus==nil 时有 Debug log**:`dispatchEvent` 在 `EventBusFromContext(ctx)` 为 nil 时,
  signal 类事件被丢 + `slog.Debug`(与 non-signal 路径对称)。排障时可查 Debug log 确认丢弃。
- **历史持久化不受影响**:`recorder.appendEvent` 在闭包里**先于** `dispatchEvent` 执行,
  run history(session hydrate 用)不依赖 sink。
- **spawn 子结果不走 sink signal**:`spawn_turn` 的子结果走 `SpawnBus`(`SpawnCollector`),
  `poll_turn` 拉取——和事件投递完全独立。

### 1.2 `assistant.final` —— 已发布，成功时的白名单信号

- `service.go` 在 `run.finished` 之前**已发布** `assistant.final`（service.go:610），条件是
  **whitelist（白名单）**：`final != "" && resp.Status == "completed"`。
- **不是 blacklist**——不是"只要不是 waiting_human 就发"。失败 run 即使 `final` 非空（如
  verification 失败但草稿已生成）也**不发**：那种情况下 forwarder 队列里的 `delegate.failed`/
  `timeout` 进度正是用户需要的信号，drain 掉会丢。见 service.go:600-608 注释和回归测试
  `TestRunDoesNotEmitAssistantFinalOnFailed`。
- HITL（`waiting_human`）不发：没有 final 就绪。
- payload 只放 `final_chars`（rune 数），final 全文不进事件。
- 语义：`assistant.final` = 「agent 的最终回复已就绪」。它是下游（ChatContext 渲染收尾、
  未来消费者）判断「final 来了，该收尾」的**契约化信号**。

### 1.3 `run.finished` ≠ final 就绪信号

`service.go` 的 `run.finished` 是**无条件**发的：`completed` / `failed` / `waiting_human` 都发。

- request-bound 模型下，无论哪种状态 `RunAgent` 都会返回、ChatContext 都会随之 stop；HITL 时停掉、
  resume 另起是新模型本身，不是「误停」。所以 `run.finished` 用来「停 sink」没有错。
- 但 **`run.finished` 不代表「final 已就绪」**——它在 HITL/纯失败时也发，而那些情况没有 final。
  需要「final 已就绪」语义时（如 sink 提前 drain、避免 progress 与 final 乱序），用
  `assistant.final`（见 1.2）。两者职责不同：`run.finished` = run 结束；`assistant.final` =
  final 回复就绪。

### 1.4 事件渲染分流:Event 类型 switch(原 `eventVisibility` 陷阱已随架构演进解决)

**曾经**（全局 EventBus + Forwarder 时代）有个 kind→visibility 映射（`eventVisibility` switch,
8 个 case + default）,Forwarder 按 `Visibility.Conversation` 过滤哪些事件进 IM。**陷阱**:新加的
fact kind 忘在 switch 里开 `conversation=true` 就被静默 drop。

**现状**（per-chat-key 架构,Phase 6b `a32dae7`）:全局 EventBus + Forwarder + scopeMatcher **全删**。
`eventVisibility` switch + `RuntimeEventVisibility` 字段也随 #43 删除（它们已无生产消费者 —— 渲染
不再读 Visibility）。现在事件分流走两条路:

- **渲染分流**:`renderEventText`（`progress/render_event.go`）按 **Event 类型 switch** 决定哪些进 IM
  （`AgentTurnFailed`/`HumanRequested`/`AssistantStatus`/`ToolCalled` 渲染;`AgentTurnStarted`/
  `Completed`/`Canceled`/`HumanResponded`/`AssistantFinal`/`ToolResult` 不渲染）。sealed 类型穷举,
  新加 Event 类型必须显式 case,不会再"忘开 visibility 被静默 drop"。
- **投递分流**:`runtimeEventToEvent`（`event_mapping.go`）只把 ~14 信号 kind 映射成 Event 投 EventBus;
  其余 ~34 个 observability/audit kind 走 `ok=false → slog.Info` 结构化日志（#43 升级,带 payload）。

**警惕仍适用**:新加的信号 kind 如果该进 IM,要在 `runtimeEventToEvent` 加映射 + `renderEventText`
加渲染 case,否则它走 non-signal 的 slog 路径(不进 IM)。这是 §2"缺口要补不要绕"在新架构下的延续。

## 2. 设计评审方法论（本次复盘总结）

- **先核实，再判断。** 评审设计文档时，文档声称存在的代码契约（事件、接口、字段）必须先
  `grep` / 读源码核实，再下结论。**核实方法本身也要核实**——`grep` 的正则、路径、锚定如果有
  盲区（比如只匹配 `recordEvent("literal")` 漏掉 `kind="..."; recordEvent(kind,...)` 动态形式），
  "我 grep 过了"也是没核实。见 §5.4。
- **写规则/契约文档时，核实纪律更要加码。** 本文件 §1 曾凭记忆写了多处与代码不符的断言
  （buffer 64 实际 256、assistant.final "从不发布"实际已发布、visibility "8 个 case" 实际已随
  架构演进整体删除），栽在"禁止不核实"的规则文件本身上。模式很清楚：**代码实现时 TDD 严，一到
  "总结经验/固化契约" 核实纪律就垮**。下次写"X 很重要"的规则文档时，先把文档里每一个事实断言
  grep 一遍——尤其 track 进 git 的，因为 track 赋予权威，带错的权威比没有更糟。
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
- **"修一个破一个"的循环 = scope 失控信号，要立刻停。** review 每轮都对、每轮都修对了，但
  连续 ≥3 轮每轮的修法都触发下一轮的**新**边界（典型：并发 → 事务 → 回滚 → 事务边界），
  这不是"还没修干净"，是**这个 PR 的范围本质上需要一套完整模型，而你在 review 压力下逐轮拼
  凑**。继续修只会把 bug 推到更隐蔽的层（且都是"不崩、只随机失效"的 silent bug，见下条）。
  正确动作：**立刻停，退一步问"这个 PR 真正该交付什么"**，把超出 PR 目标的复杂度划成
  out-of-scope（独立 RFC / follow-up），或收缩到能闭环的最小子集。PR #97 在第 4-7 轮连续 4 轮
  每轮修法触发新边界（reserved 三态破坏 single-active → 新状态机 → 又触发），直到第 7 轮才
  收缩到单连接——那 3 轮是浪费。**早停 3 轮，比晚停 3 轮省一个数量级的精力。**
- **收缩要精确：砍"多余复杂度"，不砍"核心语义"。** scope 收缩（上条的解法）最容易犯的错是
  矫枉过正——一刀切删掉看起来"相关"但其实是**核心语义**的东西。判断标准：**这个机制是为了
  解决 PR 引入的问题（多余，可砍），还是 PR 要支持的场景本来就需要的（核心，不能砍）？** PR #97
  收缩到单连接时，把"不需要两阶段 Route/Start"错误延伸成"可以忽略 Router outcome"——但单连接
  只消除了多物理连接并发，**没有消除 Router steering**（同连接连发消息仍 steer，ilink/feishu 也
  有）。这把核心语义当多余砍了，导致 activeRequest 泄漏。**收缩前逐个机制问一遍"它解决的是
  PR 引入的问题，还是 PR 场景固有的？"**——前者砍，后者留。
- **硬约束先满足，再谈例外；"诚实说明" ≠ 通过条件。** 面对仓库硬规则（如 §5.2 覆盖率 ≥85%），
  正确顺序是：**先尝试满足**（补测试 / 补 fake conn），满足不了再谈例外（改规则 / 标 follow-up）。
  反过来的倾向是"先解释为什么到不了，希望 reviewer 接受例外"——这听起来诚恳，但本质是想用
  解释绕过硬契约。reviewer 正确的回答是"诚实不等于通过条件"。PR #97 第 9 轮对 58% 给"没有
  fake conn 所以上限"的说明，reviewer 没接受；第 10 轮补真实 loopback 到 85.1% 才过——而补
  fake conn 其实不难。**硬约束的存在就是让你满足的，不是让你论证它不合理的。**

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
- **推荐用 `task live-test`**（Taskfile 已固化上述流程）：它自动设双门控 env、
  `tee` 输出到 `/tmp/xira-live-test.log`、**检查有无 live test 的 SKIP 行**
  （grep `XIRA_DEEPSEEK_LIVE` / `DEEPSEEK_API_KEY` 的 skip——平台相关的合理 skip
  如大小写不敏感 FS 不算）。有 live skip 则 exit 1。
  合并 / 发布前必跑，防"静默绿"。

### 5.4 核实方法本身也要核实（verify the verifier）

§5.1-5.3 讲的是"要核实"。本条讲的是：**核实方法如果选错了，等于没核实**。

- 核实代码契约时，先问"我的核实方法会不会漏"。典型盲区：
  - 只 `grep recordEvent("literal")` → 漏掉 `kind = "..."; recordEvent(kind, ...)` 动态形式（PR #32
    正是栽在这，`agent.delegate.timeout` 被字面量 grep 漏掉）
  - 核实行号引用时读本地脏工作树 → 行号因未提交改动偏移（PR #30 栽在这：读本地脏工作树以为
    `ephemeral_worker:` 赋值在 `delegation.go:996`，实际 origin/main 上在 977）。**引用代码用符号名
    （函数名 / 字符串字面量），不用行号**——行号会漂移，符号名稳定
  - `grep` 的正则太宽（`[^"]+`）→ 匹配到 grep 自身的注释文本，产出伪命中
  - **验证范围太窄** → 只跑单包测试（`go test ./apps/xira/internal/runtime/`）以为绿，跨包编译
    （`go build ./apps/xira/...`）却炸了（PR #46 栽在这：EventBus struct→interface 改了 runtime 包，
    channelrunner/progress 跨包引用编译失败，但单包测试看不见）。**"全量验证"必须包含全仓库编译
    （`go build ./...`）+ 全量测试（`go test ./...`），不是单包测试。** 窄范围的核实 = 没核实。
  - **宣布修复 ≠ 实际提交** → commit message 写了"修了 types.go"，但 `git add` 漏了这个文件，
    实际 staged set 不包含它（本 PR commit 7874b7e 栽在这：message 说修了 cross-package compile，
    但 progress/types.go 没进 commit，推上去的 PR 会炸 main）。**提交后核实 `git show --stat HEAD`
    真的包含修改的文件**，不只信 commit message 的描述。重要修改让 reviewer/CI 二次确认，不靠自查。
  - **恢复链只核显眼字段 + 测试数据形态 ≠ 生产形态**（PR #71 栽在这）：核实 serialize→persist→
    deserialize 的恢复链时（如 `run.SessionScope/Metadata → inboundContextFromScope → target →
    channel Emit`），只盯一个显眼字段（context_token）完整 ≠ 整条链完整。PR #71 核了 context_token
    恢复对就倾向 approve，漏了 **sender 字段断了**：`canonicalSenderID` 给 sender 加 `"ilink:"` 前缀，
    scope 存带前缀值，但 `inboundContextFromScope` 对 chat/space 用 `scopeValueID` 剥前缀、**唯独
    sender 直接取不剥** → ilink resume 把 final 投到 `ToUserID="ilink:wxid_abc"`（不存在）。**警惕
    对称性破坏**：一个函数处理多个同类字段（chat/space/sender），其中一个走不同代码路径（剥前缀
    vs 不剥）= bug 高发点。更隐蔽的是**测试盲区**：`emit_test`/`resume_delivery_test` 都手搓干净
    `SenderID:"user-9"` / `scopeWith sender:"user-1"`，**绕过了 `canonicalSenderID` 的加前缀逻辑**，
    所以全绿但没覆盖真实前缀形态——"测试全绿"和"运行时生效"之间隔着"测试数据形态 ≠ 生产数据形态"。
    - 对策：核实恢复链时**逐字段**核 round-trip——问"这条链有几个字段？我核了几个？没核的那些有
      没有不对称处理？"；测试用**真实变换产物**（走 `canonicalSenderID`/`BuildScope` 的真实 scope）
      而非手搓干净值——问"测试构造的数据，是不是生产路径里变换函数的真实输出？还是我图省事手搓的？"。
- 反射动作：每次核实后追问四句——"我的 grep/读法会不会漏？" + "我的验证范围够不够？" + "我宣布修的，实际提交了吗？" + "恢复链我逐字段核了吗？测试数据是真实变换产物还是手搓干净值？"。
  不满意就加一路（如双路 grep、`go build ./... && go test ./...`、`git show --stat HEAD`、用真实
  scope 产物重跑恢复链测试）。
- **flaky 测试不能搪塞**：全量跑 FAIL 时，不能"大概是 flaky"跳过。必须用 isolation 重跑 + `-race`
  证实。如果是真 flaky（已有 live LLM 测试），标注来源 + 开 issue 跟踪治理，不默默忽略。
- **"不对称"≠"bug"，先核实是不是 by-design**（PR #68 栽在这）：核实两条相似路径（如两条
  resume 路径 `resumeDirectHumanRequest` vs `resumeRunAfterApprovedToolOutput`）发现它们的
  ctx 钥匙集合不一致时，**不要默认"这是要消除的断裂"**。先读各自语义：那条多一把
  `contextWithRuntimeNativeToolsDisabled` 的路径，语义是"approved tool 已执行，resume 只产 final，
  不该再有工具"——禁 native 工具是**对的**；另一条不禁，语义是"问题答完了，接着干"——保留工具
  是**对的**。不对称是 by-design。对策：发现不对称先问"两条路径的语义相同吗？"——语义不同的
  不对称是契约不是 bug，写测试把它**锁定**（pin）成"diff = 仅语义决定的那把"，而不是"抹平"。
  顺带核实"能补的钥匙为什么没补"——可能是"补不了"（如 AllowedTools 不持久化在 run，要改 schema）
  或"不该补"（如异步 resume 无 per-chat-key sink，硬补违反无状态命题），**这类缺口要 documented +
  开 issue，不要硬绕也不要硬补**。
- **这条尤其适用于"自信的场景"**：写规则文档、固化经验、做架构总结时，人（和 agent）最容易
  跳过核实。本文件 §1 曾连续多处凭记忆写错（buffer 数、发布状态、case 数），栽在"禁止不核实"
  的规则文件本身上。详见 §2 的元教训。
