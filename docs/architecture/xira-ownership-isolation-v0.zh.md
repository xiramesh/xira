# Xira Agent 身份与隔离架构 v0

> 状态：草案，供团队讨论。
> 取代：`xira-agent-forms-ab-v0.zh.md`（"双形态"框架已废弃，原因见附录 A）。
> 目的：定义 Xira 的统一运行时架构——owner 绑定作为唯一关系原语、两层隔离模型、workspace 分层、多实例部署。为后续身份注入、数据隔离、多实例分发的实现提供正北方向。

## 一句话

Xira 是**统一架构**，不是两种形态。owner 是 sender↔agent 的**所有权关系**（per-instance 基数 0 或 1），不是 agent 的"类型"。"通用助手 / 个人助手"是这个统一架构的**两种部署配置**——差别只在 owner 绑定存在与否，不在架构本身。

---

## 1. 核心命题

### 1.1 统一架构，一个不是两个

任何 Xira agent 实例运行时都做同一件事：对每条入站消息查两个关系——

1. **授权**了吗？（这个 sender 能用这个 agent 吗？）
2. **是 owner** 吗？（这个 sender 是这个 agent 的主人吗？）

第二个为真才走 owner 分支（特权 skill、@ owner 代理）。为假就是普通授权用户。**没有第三种 agent 类型。**

- **"通用助手"（原 A）**：这个实例配置上**永不接受所有权绑定** → owner 永远是 0 → 所有人都是平等授权用户。
- **"个人助手"（原 B）**：这个实例**绑定了一个 owner** → owner 走特权分支，其他 sender 走普通分支。

两者跑的是同一套代码、同一个运行时，差别只是绑定关系存在与否。

### 1.2 两个不同的关系（不要混）

"绑定"在口语里揉了两层，必须分开：

| 关系 | 基数 | 含义 | A 配置 | B 配置 |
|---|---|---|---|---|
| **授权**（authorization） | N | sender 可以使用这个 agent | N 个授权用户 | owner + 其他被授权者 |
| **所有权**（ownership） | **0 或 1** | sender 是这个 agent 的主人 | 0（永不发生） | 1（owner） |

后文说"owner 绑定"特指所有权。授权 A/B 都有（A 也有 allowlist）。

### 1.3 owner 基数 0-or-1 的双重保证

"一个实例的 owner 是 0 或 1"不是靠代码自觉，是**逻辑 + 物理双重保证**：

- **逻辑**：一个 agent 实例只有一个所有权关系位。
- **物理**（B 配置）：有 owner 的需求走 per-instance Docker 部署（§5），一个 Docker 一个 owner，容器边界让"两个 owner 共占一个实例"在物理上不可能。

如果 owner 退化成 N，"所有权"就退化成"又一堆授权用户"，B 的语义崩塌。这个基数约束是硬约束。

### 1.4 owner 绑定是"能力闸门"，不是"行为本身"

绑定存在 = 某些能力**可能**触发，不等于一定触发：

- **@ owner 代理**（群里 @ 主人 → agent 默默处理）：只在 owner 绑定存在时才可能识别"@ 主人"；且 owner 可 config 关闭这个行为（不想让 agent 默默替自己干活）。
- **特权 skill**（如 outcome 写 owner vault）：只在 sender 是 owner 时可用。

所以：**绑定 = 能力闸门；config = 用不用**。绑了主人但关掉 @ owner 代理是合法配置。

---

## 2. 隔离的两层模型

Xira 的数据隔离由**两层边界**共同保证，各管一端。缺哪层都漏：

### 2.1 实例级隔离（Inter-instance，Docker / 进程边界）

- 不同 owner 用不同 Docker 实例**物理隔离**：文件系统、进程、网络都不共享。
- 张三的 Docker 和李四的 Docker 永不共享状态。
- 这是**硬边界**（OS 级）。
- B 配置默认 per-instance。

### 2.2 实例内隔离（Intra-instance，per-sender workspace）

- 同一实例内，按 **sender** 隔离数据（§3 详述）。
- B 实例监控群聊时：owner 的 vault 与群里同事的交互数据分开——同事也是这个实例的 sender，但不是 owner，数据不能进 owner vault，owner vault 也不能漏给同事。
- A 实例：N 个授权 sender 各自的 workspace 分开。
- 这是**软边界**（应用层 `pathWithinRoots` + `session.dimensions`）。

### 2.3 为什么必须两层

| 漏哪层 | 后果 |
|---|---|
| 只有实例级 | B 实例监控的群聊里，同事的交互数据会混进 owner vault（同事是同实例 sender） |
| 只有实例内 | 不同 owner 共享进程/文件系统，B 的"私人"承诺破产（owner A 能读到 owner B 的文件） |
| 两层都有 | 实例级分 owner，实例内分 sender，闭合 |

A 配置（无 owner、单实例多 sender）只需要实例内这层；B 配置两层都需要。

---

## 3. workspace 的两层含义（实例内）

> 本节是**目标态记录**，不是当前实现。现状差距见 §7。目的是把隔离的正北方向定下来，后续增量逼近。

无论 A 还是 B 配置，实例内的 workspace 都分两层，**隔离边界不同**：

### 3.1 per-sender 层（"我的部分"）

该 sender 自己的数据，其他 sender 看不到。内部按生命周期再分（都是 per-sender，但存储/淘汰策略不同）：

| 子类 | 现状 | 增长 / 生命周期 |
|---|---|---|
| 聊天记录 | ✅ 已隔离（`session.dimensions: [chat, sender]`） | 快，偏短期 |
| `user.md`（用户档案，未来） | ❌ 未实现 | 慢，小（agent 整理出的"这个用户是谁、偏好什么"） |
| memory（未来） | ❌ 未实现 | 慢，小（交互记忆） |
| 工具数据 | ⚠️ 未隔离（write_file / outcome 等落共享 workspace） | 中，工具产物（如 decision log） |

### 3.2 per-agent 层（"agent 的部分"）

所有 sender 共享、不属于任何单个 sender 的东西：

- agent 人设 / persona（`agents/{agent}/`）
- 技能定义（skills，`skills.LoadFromWorkspace`）
- 公共知识库（knowledge base）
- Agent memory（Agent 自己接受的事项、工作经验和跨 sender 上下文；#159）

memory 的归属由内容语义决定，不由 mention 机械决定。`addressed_to=owner|agent` 只描述本轮角色；模型在同一个 Agent Loop 中按 prompt 选择 `scope=sender|agent`，runtime 分别绑定当前真实 SenderID 或当前 Agent ID，模型不能指定任意身份。

> B 配置下 per-agent 层是 per-instance 的（每个 owner 的 Docker 各有一份），不跨 owner 共享。但模型不变——每个实例内部仍是这两层。

### 3.3 `user.md`：per-sender 用户档案

每个 sender 一个 `user.md`，记录"这个人是谁、习惯是什么"。

**命名选 `user.md` 的理由**：

- 文件**内容**记的是"人"的档案，不是"发消息者"这个 channel 角色。`sender` 是角色词（谁发了这条消息），`user` 是主体词（这个人）。
- 未来跨 channel 同一 user 会来（§6 deferred）：届时一个 user 可能对应多个 sender_id（飞书一个、ilink 一个），但"人"是一个。`user.md` 扛得住这次升级；`sender.md` 到那时得改名。
- **隔离的钥匙（key）仍然是 `sender_id`**——与代码一致（`InboundContext.SenderID`、`session.dimensions: [chat, sender]`）。目录按 sender 隔离、文件描述 user，两者不矛盾：`users/{sender_id}/user.md`。

**安全条件（必须记死）**：`user.md` 之所以在 Xira 安全，是因为它物理上在 per-sender 目录里，和 agent 人设（`agents/{agent}/`）隔开了。OpenClaw / Hermes 的 `USER.md` 撞车（agent 自我和对话方档案语义混淆）是因为它俩都把这两样塞进同一个全局 `memories/`。**这个安全性是目录结构给的，不是名字给的**——以后挪目录前先想清楚。

---

## 4. 两种典型部署配置

原"形态 A / 形态 B"降级为**部署配置**。差别都是 owner 绑定存在与否的派生，不是架构层面的根本不同。

### 4.1 无 owner 配置（原 A：通用助手）

- **拓扑**：1 实例，0 owner，N 授权 sender。
- **数据**：per-agent KB（公共）+ N 个 per-sender workspace。
- **行为**：@ agent 公开回复，人人平等；无 @ owner 代理（没有 owner 可 @）；无特权 skill。
- **隔离**：只需实例内 per-sender 这层。
- **典型**：公司知识库 bot、群 FAQ bot、流程审批 bot。

### 4.2 有 owner 配置（原 B：个人助手）

- **拓扑**：per-owner 实例（Docker，§5）。1 owner 绑定 + 可选其他授权 sender（群里同事）。
- **数据**：per-agent（此 owner 的 agent persona + skills + KB）+ per-sender（owner vault + 其他 sender 数据）。
- **行为**：@ agent 公开回复；@ owner 默默处理（绑定存在才可能，可 config 关闭）；owner 特权 skill。
- **隔离**：实例级（Docker）+ 实例内 per-sender 两层都要。
- **典型**：个人助理进群替主人盯活。

### 4.3 配置差异对照（真差异，都是 owner 绑定的派生）

| 维度 | 无 owner 配置（A） | 有 owner 配置（B） |
|---|---|---|
| **owner 关系** | 不存在（0） | 存在（1） |
| **部署拓扑** | 单实例多 sender | per-owner 实例（Docker） |
| **@ agent → 响应** | 公开回复，人人平等 | 公开回复（知道是 owner 的同事） |
| **@ owner → 响应** | 无 owner，不适用 | 默默处理（可 config 关闭） |
| **skill 权限** | 平等 | owner 特权 skill 存在 |
| **实例边界** | 通常单实例 | per-owner Docker（硬隔离） |
| **业界对标** | OpenClaw / Hermes | 无（@ owner 代理是新能力） |

注意：这张表里的每一行差异，**根因都是"owner 绑定存在与否"**，不是独立的架构轴。

---

## 5. 多实例分发（Docker 集中管理）

### 5.1 per-owner 实例

有 owner 需求时，**每个 owner 一个 Docker 实例**。这是 B 配置的标准部署形态。

- 一人可拥有**一或多个实例**（如工作助手 + 生活助手，两个 Docker，各自独立）。
- 约束是 **per-instance 的 owner 是 0 或 1**，不是 per-user 的 instance 数。一个 user 可以是多个 instance 的 owner。

### 5.2 为什么用 Docker

- **硬隔离**：文件系统 / 进程 / 网络都不共享，OS 级边界。
- **owner 基数双重保证**（§1.3）：物理上不可能出现"两个 owner 共占一个实例"。
- **集中管理**：统一分发、升级、回滚、监控。
- **故障隔离**：一个实例挂了不影响其他 owner。

### 5.3 什么时候 NOT 用 Docker（中间态允许）

- **无 owner 配置（A）**：一个实例服务 N sender，不需要 per-owner Docker。单进程多 agent 即可。
- **小规模 B 配置**：少量有 owner 需求时，单实例多 agent 也能跑。这是**计划内的中间态**，不是目标态。

> **中间态记账（不是约束，是已知 gap）**：单实例跑 B 配置时，§1.3 退化为**仅逻辑保证**（owner 0-or-1 靠代码 + 信任部署环境，没有 Docker 物理层），§2.1 的"实例级硬隔离"暂缺，唯一防线退到实例内 per-sender 软边界。这是迭代节奏的取舍，不是 bug。目标态仍是 Docker 部署（§5.1）；过渡期这样跑没问题，但**别把这个中间态当成权威契约引用**——owner 隔离硬度会随实现推进而上调。规模变大（多人都要个人助手）时切 Docker。

Docker 是 B 配置的**目标部署形态**，不是过渡期强制。

---

## 6. deferred open questions

以下问题**真实存在但不在本文定义终态**，留作独立 RFC / follow-up：

### 6.1 sender memory 与 Agent memory 的写入边界

agent 跟张三对话时学到了一条对所有用户都有用的事实（如某 API 用法变了）——写哪？写 per-agent KB 有泄露张三私有上下文的风险；写 per-sender 则其他用户享受不到。Hermes / Mem0 都卡在这。

**安全默认**：runtime 绝不把 sender memory 自动复制或“提升”成 Agent memory。模型只能通过 sealed `scope=sender|agent` 显式选择地址空间：关于当前人的事实写 sender；Agent 自己接受的事项、经验和上下文写 agent。如何判断由 Agent prompt + 模型负责，runtime 只绑定真实身份并隔离存储。两类 memory 都作为 untrusted data 注入，不因 Agent 自写而升级为 system instruction。

未来 cron 只会周期性触发同一个 Agent Loop 读取 Agent memory；不增加新的记忆类型或第二套 Loop。

### 6.2 跨 channel 同一 user

未来一个 user 跨多 channel（飞书 + ilink），`sender_id` 会裂成多个。届时：

- 隔离钥匙是否从 `sender_id` 升级成 `user_id`？
- `user.md` 按 user 存还是按 sender 存（多个 sender_id 共享一份）？

当前用 `sender_id` 作钥匙、`user.md` 描述 user，是为这次升级留的空间。但具体怎么映射（账户合并流程）deferred。

### 6.3 访问控制细化（owner admin 能力）

数据隔离是平的（每个 sender 一个目录），但**权限不是平的**：B 配置的 owner 是 admin（能配置 agent、看分析、用特权 skill），群里别人不是。两层 workspace 模型盖不住权限维度。与 §1.4 的 skill 权限相关，但范围更大（配置、审计、管理操作），独立 RFC。

---

## 7. 与现状的差距

> 实现增量逼近，不要求一次到位。下表是目标态 vs 现状，供排优先级。

| 能力 | 现状 | 目标 |
|---|---|---|
| 会话隔离（per-sender 历史） | ✅ `session.dimensions: [chat, sender]` | 已达标 |
| 身份注入（sender/chat → prompt） | ❌ 不注入 | §1 要求；A/B 都受益 |
| owner 绑定（所有权关系） | ❌ 不存在 | §1 核心原语 |
| 授权（allowlist） | ⚠️ 只有 allowed_agents，无 allowed_senders | §1.2 授权关系 |
| pairing（用户授权流程） | ⚠️ ilink 有基建，飞书无 | 跨 channel 授权 |
| per-sender 工具数据隔离 | ❌ 落共享 workspace | §3.1 |
| `user.md`（用户档案） | ❌ 未实现 | §3.3 |
| memory | ✅ per-sender（#128）；🔵 per-agent scope（#159） | §3.1 / §3.2 |
| @ owner 代理 | ❌ 不存在 | §1.4（B 配置核心） |
| 特权 skill | ❌ 不存在 | §1.4 |
| per-instance Docker 部署 | ❌ 单进程 | §5（B 配置推荐形态） |

---

## 8. 名词表

| 术语 | 定义 |
|---|---|
| **owner（主人）** | sender↔agent 的所有权关系。per-instance 基数 0 或 1。存在时该 sender 走特权分支 |
| **授权（authorization）** | sender 可以使用某 agent 的关系。基数 N。与所有权是两个不同关系 |
| **所有权绑定** | 建立 owner 关系的过程。本文特指 ownership，不含 authorization |
| **sender 隔离轴** | 实例内唯一的数据隔离原语。所有 per-sender 数据按 sender 分区 |
| **per-sender 层** | workspace 中"我的部分"——聊天记录、user.md、memory、工具数据，按 sender 隔离 |
| **per-agent 层** | workspace 中"agent 的部分"——人设、技能定义、公共知识库，实例内所有 sender 共享 |
| **实例级隔离** | Docker / 进程边界，分隔不同 owner（§2.1） |
| **实例内隔离** | per-sender workspace，分隔同一实例内的不同 sender（§2.2） |
| **`user.md`** | per-sender 的用户档案文件，记录"正在和我说话的这个人"。必须在 per-sender 目录里（§3.3） |
| **@ owner 代理** | owner 绑定存在时才可能的能力：群里 @ owner → agent 默默处理。可 config 关闭 |
| **特权 skill** | 只有 owner 能用的 skill（如 outcome——写 owner vault） |
| **无 owner 配置（原 A）** | 永不接受所有权绑定的部署配置。单实例多 sender，人人平等 |
| **有 owner 配置（原 B）** | 绑定了一个 owner 的部署配置。per-owner 实例，owner 走特权分支 |
| **pairing（配对）** | 用户通过验证码确认身份、获得 agent 授权的流程 |

---

## 附录 A：为什么废弃"双形态"框架

旧文档（`xira-agent-forms-ab-v0.zh.md`）把"通用助手 / 个人助手"当成**两种根本不同的 agent 形态**，并在 §3 给出"为什么必须分开"的四条理由。复核后发现这四条理由不成立或被误用：

1. **"数据隔离必须分开"**——**错**。A 和 B 都读 per-agent + per-sender 两层，数据架构完全相同。B 只是 per-sender 层只有 owner 一个 sender（N=1），不是另一种机制。操作系统里内核空间和用户空间同时存在不叫混乱，叫分层。
2. **"@ 行为不同"**——**是派生**。B 的 @ owner 代理是 owner 绑定存在时才可能的能力，是绑定的*后果*，不是独立架构轴。
3. **"权限不同"**——**是派生**。owner 特权只在 owner 绑定存在时有意义。无绑定 = 所有人平等 = A 语义。
4. **"心理预期不同"**——是产品语言层的差异，不是架构差异。

**结论**：A/B 的所有差异都是"owner 绑定存在与否"的派生。把它们当成"两种形态"会在数据隔离上引入伪区分（曾误设计 `multi_user` 开关切两套机制，已修正），在运行时上引入不必要的分支。统一为"一个架构 + 一个绑定关系"更准确，也更省实现复杂度。

产品语言上仍可保留"通用助手 / 个人助手"作为对部署者讲的概念（比讲"绑定布尔值"好懂），但**架构文档以统一模型为准**。
