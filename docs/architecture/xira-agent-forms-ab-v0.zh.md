# Xira Agent 双形态：通用助手（A）与个人助手（B）

> ⚠️ **本文档已废弃**。改用统一架构文档 [`xira-ownership-isolation-v0.zh.md`](./xira-ownership-isolation-v0.zh.md)。
> "A / B 双形态"框架被证伪：A/B 的所有差异都是"owner 绑定存在与否"的派生，不是架构层面的根本不同（详见新文档附录 A）。本文档保留以供历史参考，**请勿据此设计**。
> 下面 §7（数据隔离目标态）的内容已迁移到新文档 §3，措辞已修正。

> 状态：草案，供团队讨论。
> 目的：明确 Xira 作为一个 To B 平台需要同时支持的两种 agent 形态，界定各自的适用场景、身份模型、行为差异，为后续设计（身份绑定、群聊行为、权限控制）提供统一基础。

## 一句话概括

Xira 平台要同时支持两种 agent 形态：**通用助手**（无主人，群里谁 @ 都回答，像共享知识库）和**个人助手**（有主人，替主人盯群、@ 主人时默默处理、@ agent 时公开回复，像私人秘书）。一个 Xira 实例可以同时跑两种 agent，形态在 agent profile / entrypoint 级别配置，不是平台级二选一。

---

## 1. 两种形态的定义

### 形态 A：通用助手（Shared Assistant）

**定位**：团队共享的工具型 agent。没有"主人"，服务于所有授权用户。

**典型场景**：
- 公司知识库 bot：员工 @ 它问"年假政策是什么"，它从知识库查答案回复。
- 群里 FAQ bot：@ 它问"这个接口怎么调"，它回复文档链接。
- 流程审批 bot：群里 @ 它发起审批流程。

**核心特征**：
- **无主人**：不绑定任何个人的身份或数据。
- **@ 谁触发**：@ agent 本身。
- **响应对象**：所有被授权的用户（allowlist），人人平等。
- **数据归属**：公共知识库 / 共享工作区，不绑定个人。
- **业界对标**：OpenClaw / Hermes 的群助手模式（群里 @ bot 才响应，allowlist 控制谁能用）。

### 形态 B：个人助手（Personal Assistant）

**定位**：某个人的私人秘书。有主人，替主人在群里工作。

**典型场景**：
- 大明的个人助理进了工作群：
  - 同事 @ 大明 问"这个 bug 你看下" → agent 识别"这是叫我主人" → **默默处理**（建 task、记到 vault、汇总信息），不公开回复。
  - 同事 @ agent 问"大明在吗" → agent **公开回复**"大明暂时不在，我能帮你处理，什么事？"
  - 同事闲聊 → agent 不动。
- 主人私聊 agent → 完整个人助理体验（outcome 管理、任务规划、私密操作）。

**核心特征**：
- **有主人**（owner）：绑定一个个人身份（IM id + 名字）。
- **@ 谁触发**：既响应 @ agent，也响应 @ 主人。
- **@ agent 行为**：公开回复（但知道"是主人的同事在问"）。
- **@ 主人行为**：默默处理（不回复，替主人干活——这是 A 形态没有的核心能力）。
- **数据归属**：主人的私人数据（vault / 私人 workspace）。
- **owner 特权**：某些 skill（如 outcome）只主人能用；群里别人 @ agent 能用通用 skill，用不了主人的私人 skill。
- **业界对标**：**业界基本没做**。OpenClaw / Hermes 是 A 形态，没有"@ 主人 → 代理处理"这个概念。

---

## 2. 两种形态的对照表

| 维度 | A. 通用助手 | B. 个人助手 |
|---|---|---|
| **主人** | 无 | 有（owner） |
| **部署者意图** | 给团队部署共享工具 | 给个人部署私人秘书 |
| **@ agent → 响应** | ✅ 公开回复，人人平等 | ✅ 公开回复（知道是主人的同事） |
| **@ 主人 → 响应** | ❌ 无关，不动 | ✅ **默默处理**（不回复，替主人干） |
| **群里无关消息** | ❌ 不动 | ❌ 不动 |
| **私聊** | 受 allowlist 控制 | owner 私聊 = 完整个人助理 |
| **数据** | 公共知识库 / 共享工作区 | 主人私人 vault |
| **权限模型** | allowlist（谁能用 bot） | allowlist + owner 特权（谁能用哪些 skill） |
| **身份绑定** | 不需要 owner 绑定 | 需要 owner 绑定（IM id + 名字 + 别名） |
| **pairing（用户授权）** | 需要（控制谁能用） | 需要（控制谁能 @ agent） |
| **业界对标** | OpenClaw / Hermes | 无（新能力） |

---

## 3. 为什么必须分开

这两种形态**不是同一套逻辑的参数开关**，而是**根本不同的产品定位**：

1. **数据隔离**：A 用公共数据，B 用主人私人数据。同一个 agent 实例不能同时既读公共知识库又读个人 vault——职责混乱。
2. **@ 行为**：A 只认 @ 自己；B 还要认 @ 主人。@ 主人时的"默默处理"是 B 独有的复杂行为（要判断处理什么、记到哪、要不要通知主人），A 不需要。
3. **权限**：A 的 allowlist 是"谁能用 bot"（平等）；B 的 owner 特权是"谁能用主人的私人 skill"（分层）。
4. **心理预期**：群里的同事 @ 一个通用 bot，预期是"它在回答大家的问题"；@ 一个个人助理 bot，预期是"它在替某人工作"。用户对两者的期待不同。

**结论**：一个 agent profile + entrypoint 配置成 A 或 B，不混合。同一个 Xira 实例可以跑多个 agent，有的是 A（知识库 bot），有的是 B（个人助理）。

---

## 4. Xira 现有代码基础（核实结果）

Xira 已经有部分 A 形态的基础设施：

| 能力 | 现状 | 位置 |
|---|---|---|
| **群里 @ 才响应** | ✅ 已实现：`shouldHandleMessage`（chatType=group 时，mentioned=false 则跳过，除非配置 `RespondToUnmentionedGroupMessages`） | `feishu/runner.go:374` |
| **mention 数据** | ✅ 飞书事件带 `message.Mentions`（@ 的 user id 列表），已解析 `mentioned := len(message.Mentions) > 0` | `feishu/runner.go:213` |
| **entrypoint 配置** | ✅ `Definition.RespondToUnmentionedGroupMessages`（群聊行为开关） | `entrypoints/registry.go:35` |
| **allowlist（谁能用）** | ⚠️ 部分——有 `AllowedAgentIDs`（哪些 agent 可用），但没有"哪些 sender 可用"的配置 | `entrypoints/registry.go:25` |
| **owner 概念** | ❌ 不存在（profile 没有 owner 字段，runtime 不注入 sender/owner 身份） | — |
| **@ 主人识别** | ❌ 不存在（不知道"主人是谁"，无法识别 @ 主人） | — |
| **sender name / chat name 注入** | ❌ 不存在（InboundContext 只有 id，无 name；composeInstructionText 不注入对话身份） | — |
| **pairing（用户授权）** | ❌ 不存在 | — |

---

## 5. 待设计的关键问题（供讨论）

以下问题需要团队讨论定案，本文档不预设答案。

### 5.1 身份感知（A 和 B 都需要）

**问题**：agent 不知道"跟谁说话、在哪说话"。

**现状**：`composeInstructionText`（system prompt）只注入 agent 身份 + 当前日期，**不注入 sender / chat / channel**。agent 说"我不知道你是谁"是真的。

**需要做的**：
- InboundContext 加 chat name + sender name（不只是 id）。
- feishu runner 从飞书事件提取 chat name + sender name（飞书带了，我们没用）。
- composeInstructionText 注入"当前发言人 + 对话场景（chat id/name + channel + 类型）"。

**影响**：A 和 B 都受益——A 能个性化回复（"你好，张三"）；B 额外能识别"发言人是同事还是主人"。

### 5.2 owner 绑定（只有 B 需要）

**问题**：B 形态需要知道"主人是谁"。但用户不知道自己的 IM id，id 只有 agent 能拿到（用户发消息时 agent 从 InboundContext 看到）。

**约束**：
- 用户纯 IM 使用，不碰 server / 命令行（部署时除外）。
- 不能要求用户预先知道自己的 sender id。
- 需要防冒领（别人抢先绑定）。

**业界参考**：
- **OpenClaw**：pairing code（8 位随机码）+ server 侧 CLI approve。但 approve 要碰 server，不适合纯 IM 用户。
- **Hermes**：同上——`hermes pairing approve <platform> <code>`，CLI approve。

**待定方向**（均不完美，需讨论）：
- **a. 首次私聊 + 确认绑定**（先到先得，部署者自己抢先生成）。最简，但冒领风险（别人先私聊）。
- **b. 部署时设 claim token**（碰一次 server 设 token，之后纯 IM）。防冒领，但部署时多一步。
- **c. agent 启动时打印自己的 owner-pending 状态，等第一个私聊**（同 a，但有日志可见）。

### 5.3 @ 主人识别（只有 B 需要）

**问题**：B 形态下，群里有人 @ 主人（不是 @ agent），agent 要识别"这是叫我主人"并默默处理。

**技术基础**：飞书 @ 一个人时，`message.Mentions` 里有被 @ 的 user 的 open_id。agent 绑定 owner 后知道主人的 open_id，匹配 mention 列表就能判断"有没有 @ 主人"。

**待设计**：
- @ 主人时"默默处理"具体做什么？（建 task？记 vault？只记 session 不主动动作？）
- 处理后要不要通知主人？（私聊发一条"群里有人 @ 你说了 X，我做了 Y"？）
- @ 主人 + @ agent 同时发生时，优先级？（都 @ 了，公开回复还是默默处理？）

### 5.4 skill 权限（只有 B 需要）

**问题**：B 形态下，群里同事 @ agent 能用通用 skill，但不能用主人的私人 skill（如 outcome——写主人的 vault）。

**待设计**：
- skill 级权限模型：owner-only skill vs public skill。
- 配在哪（profile 声明 `owner_only: [outcome, ...]`？还是 skill 自己声明？）。
- 非 owner 调 owner-only skill 时怎么拒绝（"这个能力只我的主人能用"）。

### 5.5 pairing / 用户授权（A 和 B 都需要）

**问题**：控制"谁能 @ agent / 谁能用 bot"。

**A 形态**：allowlist（sender id 列表，配置在 entrypoint 或 profile）。
**B 形态**：allowlist + owner 绑定（owner 是特殊的 allowlist 成员，有额外特权）。

**待定**：allowlist 怎么配（静态配置？还是动态 pairing code？还是两者结合？）

---

## 6. 建议的实施分层（不预设顺序，供讨论）

```
第一层：身份感知（A + B 基础）
  - InboundContext 加 name 字段
  - feishu runner 提取 chat name + sender name
  - composeInstructionText 注入发言人 + 对话场景
  → 所有 agent 都能回答"跟谁说话、在哪说话"

第二层：allowlist（A + B）
  - entrypoint / profile 配 allowed senders
  - 群里非 allowlist 的人 @ agent → 忽略
  → 基本的访问控制

第三层：owner 绑定（B 专属）
  - 绑定流程（方案待定）
  - profile / entrypoint 存 owner 身份（IM id + 名字 + 别名）
  - composeInstructionText 注入"主人是谁"
  → B 形态的 agent 知道主人

第四层：@ 主人代理（B 专属，核心创新）
  - mention 列表匹配 owner id → 判定"@ 主人"
  - @ 主人 → 默默处理（处理逻辑待设计）
  - @ agent → 公开回复
  → B 形态的核心价值

第五层：skill 权限（B 专属）
  - owner-only skill vs public skill
  - 非 owner 调 owner-only → 拒绝
  → B 形态的权限隔离
```

第一层 + 第二层让 A 形态基本可用。第三到第五层让 B 形态完整。

---

## 7. 数据与隔离的目标态：sender 作为统一隔离轴

> 本节是**目标态记录**，不是当前实现，也不要求一次到位。目的是把隔离的"正北方向"定下来，后续增量逼近。实现节奏见 §6，本节只定义终态、不规定实现顺序。
>
> 背景共识：现在没有 memory，以后有了 memory 也是每个 sender 自己的 memory、自己的聊天记录。所以"我的部分"和"agent 的部分"天然是两重含义。

### 7.1 统一隔离原语：sender

无论 A 形态还是 B 形态，**数据隔离的轴只有一个：sender**。A 和 B 的差别是**部署拓扑**（一个 agent 挂几个 sender、谁拥有它），不是隔离方式：

- **A 形态**：一个 agent 服务 N 个 sender → per-sender 目录下有 N 个子目录，彼此隔离。
- **B 形态**：一个 agent 实质绑定 1 个 sender（owner）→ per-sender 目录下只有 1 个子目录。

原语完全相同。**不要为 A/B 设计两套隔离机制**——那是把部署拓扑当成了隔离原语。

> 与 §3 的关系：§3 第一条"A 用公共数据，B 用主人私人数据，不能混合"是**部署拓扑视角**（一个 agent profile 承担一种角色，不能既读公共 KB 又读个人 vault）。从**隔离原语视角**看，两者都是 per-sender，区别只在挂几个 sender。两种视角不冲突，但别把拓扑差异当成"需要两套隔离机制"的理由。早先设计曾在这里走过弯路（用 `multi_user` 开关切两套机制），已修正为统一 sender 轴。

### 7.2 workspace 的两层含义

目标态的 workspace 分两层，**隔离边界不同**：

**第一层：per-sender（"我的部分"）**
该 sender 自己的数据，其他 sender 看不到。内部按生命周期再分（都是 per-sender，但存储/淘汰策略不同）：

| 子类 | 现状 | 增长 / 生命周期 |
|---|---|---|
| 聊天记录 | ✅ 已隔离（`session.dimensions: [chat, sender]`） | 快，偏短期 |
| memory（未来） | ❌ 未实现 | 慢，小（agent 整理出的"这个用户是谁、偏好什么"） |
| 工具数据 | ⚠️ 未隔离（write_file / outcome 等落共享 workspace） | 中，工具产物（如 decision log） |

**第二层：per-agent（"agent 的部分"）**
所有 sender 共享、不属于任何单个 sender 的东西：
- agent 人设 / persona（`agents/{agent}/`）
- 技能定义（skills，`skills.LoadFromWorkspace`）
- 公共知识库（knowledge base）

### 7.3 两个 deferred open question（先记下，不现在解）

这两条模型里真实存在，但**不在本节定义终态**，留作独立 RFC / follow-up：

**a. agent 自写提升（per-sender → per-agent KB）**
agent 跟张三对话时学到了一条对所有用户都有用的事实（比如某 API 用法变了）——写哪？写 per-agent KB 有泄露张三私有上下文的风险；写 per-sender 则其他用户享受不到。Hermes / Mem0 都卡在这。
**安全默认**：一律写 per-sender，**绝不自动提升**到 per-agent；要提升只能走显式 operator 审批。这是个 policy 问题，不是纯技术问题。

**b. 访问控制（owner vs 普通 sender）**
数据隔离是平的（每个 sender 一个目录），但**权限不是平的**：B 形态的 owner 是 admin（能配置 agent、看分析、用 owner-only skill），群里别人不是。两层 workspace 模型盖不住权限维度。留作独立设计（与 §5.4 skill 权限相关，但范围更大）。

### 7.4 命名避坑

业界（OpenClaw / Hermes）的 `USER.md` 语义是冲突的：OpenClaw 的 USER.md 是 **agent 自我设定**（agent 是谁、怎么说话），Hermes 的 USER.md 是 **对话方档案**（用户是谁、喜欢什么）。Xira 不要重蹈这个混淆：
- agent 人设 → 已在 `agents/{agent}/`，别叫 USER.md。
- 对话方档案（未来的 per-sender memory）→ 叫 `PROFILE.md` 或 `SENDER.md`，明确是"正在和我说话的这个人"，不是"我"。

---

## 8. 名词表

| 术语 | 定义 |
|---|---|
| **形态 A（通用助手）** | 无主人的共享 agent，群里谁 @ 都回答 |
| **形态 B（个人助手）** | 有主人的私人 agent，替主人盯群、@ 主人默默处理 |
| **owner（主人）** | B 形态中 agent 绑定的个人身份 |
| **pairing（配对）** | 用户通过验证码确认身份、获得 agent 使用授权的流程 |
| **allowlist（白名单）** | 允许使用 agent 的 sender id 列表 |
| **owner-only skill** | 只有 owner 能用的 skill（如 outcome——写主人私人 vault） |
| **@ 主人代理** | B 形态独有：群里 @ 主人时，agent 默默替主人处理（不公开回复） |
| **mention** | IM 消息里的 @ 提及，带被 @ 者的 user id |
| **sender 隔离轴** | Xira 唯一的数据隔离原语。所有 per-sender 数据按 sender 分区；A/B 形态共用，差别只在挂几个 sender |
| **per-sender 层** | workspace 中"我的部分"——聊天记录、memory、工具数据，按 sender 隔离 |
| **per-agent 层** | workspace 中"agent 的部分"——人设、技能定义、公共知识库，所有 sender 共享 |
| **PROFILE.md**（未来） | per-sender 的对话方档案文件，记录"正在和我说话的这个人"。**不叫 USER.md**，避免与 agent 人设混淆（见 §7.4） |
