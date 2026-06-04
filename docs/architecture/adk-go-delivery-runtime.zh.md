# 基于 ADK Go 的 Xira 客户交付运行时设计草案

> 日期：2026-06-03  
> 定位：新产品 / 新运行时的架构草案，不是 PicoClaw 的重构计划。  
> 结论：采用 ADK Go 作为 Agent Loop 内核是可行的，但产品边界应该由 Xira Business Runtime 定义。

## 摘要

如果从零做一个面向客户交付的桌面 / 私有化 Xira 运行时，可以采用 **ADK Go first, but not ADK only** 的设计。

ADK Go 负责：

- LLM 与 tool call 的核心 agent loop
- Runner、event stream、session、memory 的基础能力
- MCP、A2A、多 agent 协作等通用 agent 能力
- 后续评测、debug、部署生态的复用

自有运行时负责：

- flow、客户、workspace、渠道、会话作用域
- flow pack、技能包、闭源 CLI、ERP / SaaS connector 的交付与权限
- secrets、审计、日志、策略、版本、升级、回滚
- task spec、context pack、verification、run log、artifact、evolution candidate
- 桌面 / Web GUI 控制台
- 飞书、企业微信、WebSocket、自定义 API 等 channel gateway

核心判断是：**ADK Go 可以定义 agent 怎么跑，但不应该定义 Xira 交付什么。**

## Xira 与 Flow 定义

Xira 的 `flow` 不是传统意义上的 workflow、DAG、审批流或任务编排引擎。

这里的 flow 是对 B 交付的业务流程视角：

- 从最早的一段提示词开始，沉淀成可执行 agent。
- 再从单个 agent 上升为一个可交付的业务 flow。
- 一个 flow 围绕客户真实业务目标组织多个 agent、skills、connectors、secrets、policy、人工审批、审计和交付物。
- 客户购买和验收的不是 prompt，也不是单个 agent，而是一个能嵌入业务流程并持续运行的 flow。

因此 Xira 的产品边界应该是：

```text
Business Flow
  -> Task Spec / Context Pack
  -> Agent Profiles
  -> Skills
  -> Connectors
  -> Tools
  -> Artifacts
  -> Verification / Evaluation
  -> Run Log / Audit
  -> Harness Evolution
```

Flow 可以包含多 agent 协作，但 flow 本身首先是业务对象，不是底层 agent 编排图。

运行入口不等于交付边界。Xira 应该同时支持两类 entrypoint：

```text
Agent Run
  -> 直接运行一个 agent profile
  -> 适合探索、实施、调试、一次性任务、内部助理

Flow Run
  -> 运行一个业务 flow
  -> 适合客户交付、长期复用、验收、审计和持续演进
```

很多真实工作并不需要先建模成 flow。第一版只要把 agent profile、tool call、built-in tools、run log 和 XiraGarden GUI channel 跑通，就可以开始干活；flow pack 可以作为后续对 B 交付和复用时的上层包装。

## 背景

目标产品是一个面向客户交付的 Xira：

- 有统一的 `xira` CLI/runtime API 入口，以及 XiraGarden GUI 入口。
- 能把同一套 runtime 部署到不同客户环境。
- 支持直接运行 agent，也支持运行业务 flow。
- 每个客户获得定制化 flow packs、skills、connectors、closed-source CLI tools。
- 每个 flow 可以围绕一个业务流程组合多个 agent，而不是只交付单个 agent。
- 客户自己的 ERP、SaaS、内部系统可以被接入。
- agent loop 本身尽量使用统一、成熟、可维护的实现。
- 交付物可以闭源，并且可安装、可升级、可审计、可回滚。

PicoClaw 提供的参考思想是：

- 多 channel gateway
- 平台无关 inbound / outbound bus
- 结构化 session scope
- hook / process hook
- MCP tool 接入
- Web launcher / dashboard
- runtime event 和日志
- steering、subturn、异步工具等真实聊天场景能力

但新产品不应直接复制 PicoClaw 的代码结构。PicoClaw 可以作为经验来源，而不是 implementation base。

## 设计目标

1. **统一交付底座**
   同一套 runtime 可服务不同客户，只替换配置、flows、skills、connectors、policy。

2. **ADK 原生 agent loop**
   新系统不再自己从零维护完整 LLM/tool loop，而是把 ADK Go 作为核心执行引擎。

3. **业务边界独立于 ADK**
   agent、flow、客户、渠道、权限、审计、secrets、交付包版本不绑定 ADK 的内部抽象。

4. **客户定制能力可逐步封装**
   可以先交付 agent profile 和 connector，等业务目标、验收和复用边界稳定后，再封装成 flow pack。

5. **私有化优先**
   默认支持客户本地、客户 VPC、客户内网、单机桌面部署。云端控制平面可以后置。

6. **可观测、可追责**
   每次 agent turn、tool call、connector 调用、权限拒绝、人工审批都应该有审计事件。

7. **可验证、可进化**
   每个 agent run / flow run 都应该留下 run log、artifact、verification result 和可选的 evolution candidate，让客户交付能力能从真实运行中沉淀。

## 非目标

- 不做通用开源 agent framework。
- 不和 ADK、LangChain、CrewAI 竞争底层 agent 抽象。
- 不做传统 workflow engine，不和 Temporal、Inngest、Hatchet 竞争 DAG / durable execution。
- 不在第一版支持所有 PicoClaw channel。
- 不在第一版支持复杂 multi-agent 编排。
- 不要求第一版所有任务都必须建模为 flow。
- 不把客户业务 connector 编译进核心 runtime。
- 不直接把 ADK Skills 当作 Xira 的交付格式。

## 高层架构

```text
XiraGarden / Web Console / CLI
        |
        v
Control Plane / Runtime API
        |
        v
Business Runtime
  - flow routing
  - workspace / customer
  - channel routing
  - session scope
  - permissions
  - secrets
  - audit
  - policy
  - verification
  - evolution
        |
        v
ADK Go Agent Core
  - runner
  - agent
  - tools
  - sessions
  - memory
  - events
        |
        v
Capabilities
  - built-in tools
  - MCP servers
  - closed-source CLI tools
  - HTTP / gRPC connectors
  - flow packs
  - skill packs
  - customer ERP adapters
        |
        v
Harness Stores
  - run logs
  - audit events
  - artifacts
  - eval cases
  - evolution candidates
```

分层原则：

| 层 | 主要职责 | 是否依赖 ADK |
| --- | --- | --- |
| Console | 配置、日志、运行状态、人工审批 | 否 |
| Channel Gateway | 飞书、WebSocket、企业微信、自定义 API | 否 |
| Business Runtime | flow 路由、session、权限、secrets、审计、策略 | 可调用 ADK，但不被 ADK 定义 |
| ADK Agent Core | 单 turn agent loop、tool call、event stream | 是 |
| Capabilities | built-in tools、flow pack、skill pack、MCP、CLI、HTTP connector | 可被编译成 ADK agent / tools |
| Harness Stores | run log、artifact、audit、eval、evolution candidate | 否 |

## 核心原则

### 1. ADK 做内核，不做产品边界

ADK Go 的 Runner / Event Loop 适合处理一个 turn 内的模型调用、工具调用和事件产出。

但以下概念应该属于自有运行时：

- customer
- workspace
- business flow
- flow pack
- channel account
- conversation scope
- agent profile
- skill pack
- connector
- permission policy
- secret binding
- delivery version
- audit event

这样即使未来更换 ADK、并行支持其他 agent engine，产品层也不会重写。

### 2. Channel 先归一化，再进入 agent

所有入口都应该先被转换为统一的 inbound envelope：

```text
Channel Message
  -> InboundEnvelope
  -> RouteDecision
  -> SessionScope
  -> AgentTurnRequest
  -> ADK Runner
```

不要让 ADK 直接理解飞书、企业微信、Telegram、WebSocket 或客户内部 API。

### 3. Agent 是最小执行入口，Flow 是交付包，Skill 是能力单元

实际干活时，最小可运行对象可以是 agent profile。

Agent profile 负责定义：

- model policy
- instructions
- allowed skills / tools / connectors
- permissions
- context binding
- verification defaults

面向客户长期交付时，最终交付对象不应该只是一段 prompt，也不应该只是单个 agent 或 skill。

Xira 的交付单位应该是 flow pack。一个 flow pack 应该包含：

- flow definition
- instructions
- agent profiles
- tools
- connectors
- examples
- permissions
- required secrets
- policy
- tests
- version metadata

运行时把 flow pack 装配成 agent profiles、ADK agent instructions、ADK tools、MCP server、process tool 或 HTTP connector。

因此三者关系是：

```text
Agent Profile
  -> 最小执行入口
  -> 可以独立运行

Flow Pack
  -> 业务交付包装
  -> 可以引用一个或多个 agent profiles

Skill Pack
  -> 复用能力单元
  -> 被 agent profile 或 flow pack 引用
```

Skill pack 可以继续存在，但它是 agent / flow 内部复用的能力单元，不是产品交付边界。

### 4. Connectors 默认进程外

客户 ERP / SaaS / 内部系统 connector 不应该默认编译进核心 runtime。

优先级：

1. MCP server
2. closed-source CLI over stdio / JSON
3. HTTP / gRPC connector
4. 原生 Go plugin / native module

native module 只用于平台基础能力，不作为默认客户定制方式。

### 5. 审计是一等公民

所有关键动作都应该写入 audit log：

- inbound message
- route decision
- session allocation
- LLM request / response metadata
- tool call request
- tool call result
- permission denied
- human approval
- secret access
- connector error
- outbound message

注意：audit log 不一定保存完整敏感内容，但要保留足够的可追溯 metadata。

### 6. Harness 是 Xira 的工程闭环

Xira 不只是让 agent 能调用工具，而是要让每次真实运行都能成为下一版交付能力的证据。

可以把 Xira 的 Harness 动作定义为：

| 动作 | Xira 中的含义 | 主要落点 |
| --- | --- | --- |
| Specify | 把客户需求变成可执行规格和验收标准 | `agent_profile.yaml`、`task_spec.yaml`、flow objective、acceptance cases |
| Ground | 给 agent 正确上下文，排除过期材料 | context pack、source of truth、forbidden context |
| Equip | 提供 built-in tools、connectors、MCP、skills | tool runtime、connector runtime、skill pack |
| Constrain | 限制权限、secrets、目录、网络和高风险动作 | policy、approval gate、sandbox |
| Orchestrate | 安排 agent run 或业务 flow 的阶段、检查点和 handoff | agent plan、flow stages、protocol、handoff artifacts |
| Verify | 用确定性检查和 eval 判断是否完成 | verification commands、schema、golden tasks |
| Observe | 保存 trace、run log、audit、artifact | event store、audit store、artifact store |
| Improve | 把失败和用户纠正转化为改进候选 | evolution events、candidate patches、promotion gate |

这意味着 Xira 同时有两个闭环：

```text
执行闭环：
Inbound -> Agent / Flow -> Tool / Command -> Verification -> Output

进化闭环：
Run Log -> Failure Attribution -> Evolution Candidate -> Verification -> Promote / Rollback
```

第一版不追求全自动自进化，但要把证据结构先建好。否则后续无法判断一个 flow pack、skill、connector 或权限策略是否真的变好了。

## 主要组件

### Runtime Daemon

本地或私有化部署的主进程。

职责：

- 加载 workspace 配置
- 启动 channel gateway
- 启动 runtime API
- 管理 ADK agent core
- 加载 agent profiles
- 加载 flow packs / skill packs
- 注册 connectors
- 管理 secrets
- 写入 audit / event log
- 暴露 health / status / logs

建议命令：

```text
xira serve
```

### Xira CLI

面向开发者、交付工程师和运维人员。

职责：

- 初始化 workspace
- 安装 / 升级 flow pack
- 安装 / 升级 skill pack
- 运行 / 调试 agent
- 运行 / 调试 flow
- 运行受控 command
- 查看 tool call
- 查看 session
- 导出日志
- 打包交付物

建议命令：

```text
xira
xira init
xira serve
xira agent run
xira flow install
xira flow run
xira command run
xira audit export
```

### Console

桌面或 Web GUI 控制台。

第一版优先做 XiraGarden。XiraGarden 是客户交付和实施调试的重要入口，但它不应该硬编码任何 flow、agent、tool 或 CLI。

XiraGarden 启动后应该通过 Runtime API 读取当前 workspace 中已经安装 / 启用的：

- flow packs
- agent profiles
- sessions
- audit events

XiraGarden 可以触发 agent run、flow run、tool call 查看、ad-hoc exec request 和 audit export，但所有动作都必须经过 Business Runtime。XiraGarden 不在自己进程里直接执行客户 CLI；它应该调用 Runtime API，由 Runtime 的 built-in tools 执行、记录和回传结果。

第一版应该支持：

- runtime 状态
- channel 配置
- model 配置
- flow pack 列表
- agent profile 列表
- skill pack 列表
- built-in exec tool
- tool call 日志
- audit event 查询
- session 查看
- 人工审批
- secrets 绑定状态

### Channel Gateway

把不同入口转换为统一 inbound envelope。

第一版建议只做：

- WebSocket / Pico-like local chat
- Feishu
- HTTP API

后续再加：

- WeCom
- DingTalk
- Slack
- Telegram
- 客户内部 webhook

### Business Runtime

这是产品核心。

职责：

- route inbound message to flow / agent profile
- allocate session scope
- apply workspace policy
- bind agent profiles and optional flow packs / skill packs
- expose secrets to connector under policy
- convert connector/tool metadata to ADK tools
- consume ADK event stream
- convert final / partial output to outbound messages
- write audit events

Business Runtime 不应该把 ADK 的 session id 当成唯一业务 session。

### ADK Agent Core

基于 ADK Go 实现。

职责：

- 创建 agent
- 创建 runner
- 管理 ADK session
- 执行单个 turn
- 触发 ADK tool calls
- 产出 ADK events
- 返回 final response

ADK Core 应该通过 adapter 被 Business Runtime 调用。

建议接口：

```go
type AgentEngine interface {
    RunTurn(ctx context.Context, req TurnRequest) (<-chan RuntimeEvent, error)
}
```

其中 `adk.Engine` 是一个实现。未来可以有：

- `native.Engine`
- `codexcli.Engine`
- `claudecode.Engine`

### DeepSeek Model Adapter

第一版只支持 DeepSeek，不先做通用 OpenAI-compatible provider。

支持模型：

- `deepseek-v4-flash`
- `deepseek-v4-pro`

能力范围：

- chat completion
- streaming response
- tool calls

非目标：

- 不支持旧模型别名，例如 `deepseek-chat`、`deepseek-reasoner`。
- 不暴露任意 OpenAI-compatible `base_url` 作为第一版能力。
- 不在第一版做多 provider fallback。
- 不在第一版支持 Anthropic-format endpoint。

建议配置：

```yaml
model:
  provider: deepseek
  model: deepseek-v4-flash
  api_key_env: DEEPSEEK_API_KEY
```

运行时校验：

1. `provider` 必须是 `deepseek`。
2. `model` 必须是 `deepseek-v4-flash` 或 `deepseek-v4-pro`。
3. 未配置 `DEEPSEEK_API_KEY` 时启动失败。
4. tool calls 必须经过 Business Runtime 的 policy / audit wrapper。

这样做的目的不是封死未来模型选择，而是让第一版先证明 Xira 的 runtime 边界、XiraGarden GUI channel、agent profile、connector、audit 链路。模型扩展应该在 `AgentEngine` 和 model adapter 稳定之后再做。

### Agent Profile Manager

负责加载、校验、启用和运行 agent profile。

Agent profile 是 Phase 1 的最小业务执行单元。它不要求先有 flow pack，也不要求先定义完整业务流程。

建议 agent profile 结构：

```text
agents/
  research-assistant.yaml
  support.yaml
```

示例 `agents/research-assistant.yaml`：

```yaml
id: research-assistant
name: Research Assistant
version: 0.1.1
model_policy: default
model:
  provider: deepseek
  model: deepseek-v4-flash

instructions:
  - instructions/research-assistant.md

context:
  required:
    - context/research-sources.md
  optional: []
  forbidden:
    - context/outdated.md

skills:
  - local-research

tools:
  - exec
  - read_file
  - write_file
  - list_dir
  - edit_file

verification:
  default_checks:
    - output_has_sources

artifacts:
  output_dir: artifacts/
  retention: local

evolution:
  enabled: true
  candidate_only: true
```

Phase 1 的最小 run 可以是：

```text
Inbound message
  -> AgentProfileManager resolves `research-assistant`
  -> BusinessRuntime applies policy / connectors
  -> ADK Runner executes one turn
  -> RunLogWriter records run
```

Agent profile 可以独立运行，也可以被 flow pack 引用为 `entry_agent`、reviewer 或 worker。

#### Agent Profile 必填面

第一版 agent profile 应该比 flow pack 更轻：

| 面 | 作用 | 第一版要求 |
| --- | --- | --- |
| Identity | 定义 agent id、name、version | 必填 |
| Model Policy | 绑定 provider、model、参数和白名单 | 必填 |
| Instructions | 定义 agent 行为边界 | 必填 |
| Context | 定义 required / optional / forbidden 上下文 | 可选，建议支持 |
| Tools / Connectors | 定义可用外部能力 | 可选 |
| Permissions | 定义 tool、command、secret 权限 | 必填 |
| Verification Defaults | 定义轻量检查 | 可选 |
| Artifact Policy | 定义输出目录和保留策略 | 可选，默认本地 |
| Evolution Policy | 定义是否生成改进候选 | 可选，默认 candidate-only |

### Flow Pack Manager

负责加载、校验、启用、禁用和升级 flow pack。

Flow pack 是 agent profile 之上的业务交付包装。第一版可以先支持 agent run，再逐步把稳定 agent、connector、verification 和 artifact policy 封装成 flow pack。

建议 flow pack 结构：

```text
customer-support/
  flow.yaml
  task_spec.yaml
  context/
    required.md
    optional.md
    forbidden.md
  instructions.md
  agents/
    support.yaml
    refund-reviewer.yaml
  workflows/
    support-flow.yaml
    escalation-protocol.md
  examples/
    refund.md
    escalation.md
  skills/
    ticketing/
    refund-policy/
  tools/
    refund_lookup.yaml
    ticket_update.yaml
  integrations/
    youzan.yaml
    internal_erp.yaml
  verification/
    acceptance.case.yaml
    privacy.checklist.md
    golden_tasks.yaml
  tests/
    refund_lookup.case.yaml
  artifacts/
    README.md
  evolution/
    candidates/
    promoted/
    archived/
  README.md
```

示例 `flow.yaml`：

```yaml
id: customer-support
name: Customer Support
version: 0.1.1
description: Customer-specific support flow.

flow:
  entry_agent: support
  objective: Resolve customer support requests with order lookup, policy checks, and escalation.
  task_spec: task_spec.yaml
  workflow: workflows/support-flow.yaml
  context:
    required:
      - context/required.md
    optional:
      - context/optional.md
    forbidden:
      - context/forbidden.md

agents:
  - id: support
    model_policy: default
    instructions:
      - instructions.md
    skills:
      - ticketing
      - refund-policy
  - id: refund-reviewer
    model_policy: strict
    instructions:
      - agents/refund-reviewer.yaml

permissions:
  tools:
    - ticket.search
    - ticket.update
    - erp.order.read
  secrets:
    - youzan.api_token
    - erp.service_account

integrations:
  - id: youzan
    type: http
    config: integrations/youzan.yaml
  - id: internal-erp
    type: process
    config: integrations/internal_erp.yaml

tests:
  - tests/refund_lookup.case.yaml

verification:
  acceptance_cases:
    - verification/acceptance.case.yaml
  golden_tasks:
    - verification/golden_tasks.yaml
  privacy_checklist: verification/privacy.checklist.md

artifacts:
  output_dir: artifacts/
  retention: local

evolution:
  enabled: true
  candidate_dir: evolution/candidates
```

#### Flow Pack 必填面

第一版 flow pack 不需要一开始覆盖所有企业能力，但应该保留这些结构面：

| 面 | 作用 | 第一版要求 |
| --- | --- | --- |
| Task Spec | 定义目标、范围、非目标、验收 | 必填 |
| Context Pack | 定义 required / optional / forbidden 上下文 | 必填 |
| Agents | 定义 entry agent 和可选 reviewer / worker | 必填 |
| Skills | 复用能力单元 | 可选 |
| Connectors | 声明稳定外部能力 | 可选 |
| Workflow / Protocol | 定义阶段、检查点、异常处理 | Flow 交付时必填；agent-only 可选 |
| Verification | 定义确定性检查和验收 case | 必填 |
| Artifact Policy | 定义产物目录、保留策略、隐私边界 | 必填 |
| Evolution Policy | 定义是否生成改进候选及推广规则 | 可选，默认开启 candidate-only |

这样做的目的，是避免 flow pack 退化成“agent prompt + tools 列表”。对 B 交付时，客户需要验收的是完整业务流程，而不是模型回答能力。

### Skill Pack Manager

负责加载 flow 内可复用的能力单元。

职责：

- 校验 skill metadata
- 编译 skill instructions
- 声明 skill 需要的 tools / connectors / secrets
- 把 skill 暴露给 flow pack 选择

Skill Pack Manager 不决定客户交付边界；Flow Pack Manager 才决定一个客户业务流程如何被安装、启用、升级和验收。

### Built-in Tools

Xira 第一版需要一组类似 Codex/PicoClaw 的最小内置工具：`exec`、`read_file`、`write_file`、`list_dir`、`edit_file`。其中 `exec` 用来执行运行前未知的 shell 命令，四个文件工具负责稳定的本地文件读写和编辑。

原因：

- 客户现场常常已经有可执行 CLI、内部脚本、运维工具。
- Xira 在启动前无法知道这些命令的名字、路径、版本和参数。
- 交付工程师需要先通过 XiraGarden / agent 试跑命令、观察输出，再决定是否把它写入业务流程或后续工具声明。
- 本地知识库、临时脚本、HTML 图表和交付产物都需要稳定的文件读写能力。

`exec` 是内置平台能力，不等同于客户业务 connector。Phase 1 不为每个客户 CLI 编写专用 connector；模型如果需要探索外部工具，应通过 `exec` 调用 `which`、`--help`、`--version` 或具体命令，并把结果写入 audit/run log。

职责：

- 在指定 workspace / cwd 下执行 shell 命令。
- 读、写、列目录和精确替换 workspace 文件。
- 记录 command、cwd、env 摘要、exit code、stdout / stderr 摘要和耗时。
- 支持 timeout、取消、输出截断和流式输出。
- 所有执行都写 audit event。
- 支持把命令调用过程记录为可复盘 recipe，但不在 Phase 1 自动生成 connector。

建议执行流程：

```text
XiraGarden / Agent
  -> Runtime API: tool call request
  -> ToolRegistry resolves built-in tool
  -> exec / file tool executes
  -> RuntimeEvent stream stdout / stderr chunks
  -> AuditEvent records tool metadata
  -> optional: save command recipe artifact
```

约束：

- 默认只在当前 workspace 或 flow pack 允许的 working directory 内执行。
- 默认不注入 secret；需要 secret 时必须显式绑定并审计。
- `exec` 结果可以被 agent 读取，但 agent 不能绕过 runtime 直接启动 shell。
- 可重复交付的客户能力应先沉淀为 flow / agent 的受控命令步骤；是否升级成独立 connector 是 Phase 1 之后的能力。

### External Tool Runtime

External Tool Runtime 是客户系统和 flow / agent 之间的能力桥。Phase 1 只实现内置 `exec` 和文件工具；不实现 CLI manifest wrapper，也不把 MCP 作为第一版必需能力。

后续可以支持四类：

| 类型 | 用途 | 备注 |
| --- | --- | --- |
| MCP | 标准工具生态 | 优先支持 |
| Process CLI | 闭源工具、客户定制工具 | 适合交付 |
| HTTP / gRPC | 企业系统、SaaS API | 易运维 |
| Native | 核心平台能力 | 谨慎使用 |

#### Process CLI 模式

CLI 是 Xira 对 B 交付里的重要形态。很多客户交付物本身就是一个可执行命令，Xira 在运行前不应该假设自己知道客户环境里有哪些命令、版本和能力。

Process CLI 后续可以支持两种模式：

| 模式 | 用途 | 说明 |
| --- | --- | --- |
| Protocol CLI | 专门为 Xira 编写的闭源工具 | 走 JSON stdin/stdout 协议 |
| Wrapped CLI | 已存在的客户 / 交付 CLI | 由 connector manifest 映射命令、参数、schema 和权限 |

Protocol CLI 建议协议：

```text
runtime -> connector: JSON request over stdin
connector -> runtime: JSON response over stdout
```

并要求：

- 明确超时
- 明确 schema
- 明确 permission
- 明确 secret binding
- stderr 进入 debug log
- stdout 只承载协议输出

Wrapped CLI 不要求原命令理解 Xira。运行时通过 flow pack / connector manifest 知道如何调用它。但这不是 Phase 1 的默认能力，不能在 core runtime 里硬编码任何具体 CLI。

运行时在安装或启动 flow pack 时做 capability discovery：

1. 解析 connector manifest。
2. 在 workspace / package bin / PATH 中解析 executable。
3. 如配置了 `version_args`，执行版本探测并记录结果。
4. 校验每个 tool 的 input / output schema。
5. 生成 capability snapshot，暴露给 XiraGarden 和 Runtime API。
6. tool call 执行时重新经过 policy、secret binding、rate limit、audit。

注意：

- Wrapped CLI connector 不扫描任意系统命令，只把 manifest 声明的 tool 暴露为可复用能力。
- 未在 flow pack / connector manifest 中声明的 CLI 不能作为稳定 tool 自动调用；如果需要临时运行，必须走 Built-in Command Runner 的审批、sandbox 和 audit。
- XiraGarden 展示的是 runtime-discovered agent 和其允许的 built-in tools，不是自己发现的本机命令。
- 对客户交付 CLI，建议同时提供 `--version` 和机器可读输出；但第一版不强制 CLI 原生支持 Xira 协议。

### Secrets Manager

职责：

- 保存客户密钥
- 绑定 secret 到 flow / skill / connector
- 控制 tool call 时的 secret 访问
- 记录 secret access audit event
- 支持本地加密和企业 secret backend

第一版可以支持：

- local encrypted file
- env var binding
- 1Password / Vault 作为后续能力

### Audit / Event Store

建议区分：

- Runtime events：面向 UI、debug、实时展示。
- Audit events：面向合规、追责、客户交付验收。

Event 可以更细、更频繁；Audit 应该更稳定、更结构化。

### Run Log / Artifact Store

Run log 和 artifact 是客户交付验收、失败复盘和 Harness 进化的基础。

建议区分四类记录：

| 类型 | 面向对象 | 内容 | 是否稳定 |
| --- | --- | --- | --- |
| Runtime Event | GUI、debug、实时观察 | 流式事件、delta、状态变化 | 可演进 |
| Audit Event | 合规、追责、客户验收 | 稳定 metadata、权限、审批、tool call | 应稳定 |
| Run Log | 复盘、handoff、evolution | 本次任务做了什么、看到什么、验证什么 | 半结构化 |
| Artifact | 交付、证据、回放 | 报告、CSV、截图、diff、evidence ledger | 应可定位 |

建议每次 agent run / flow run 生成目录：

```text
.xira/
  runs/
    20260603-143000-customer-support/
      run.yaml
      events.jsonl
      audit.jsonl
      command.log
      tool_calls.jsonl
      verification.json
      handoff.md
      artifacts/
        report.md
        evidence-ledger.csv
        screenshots/
      evolution/
        candidates/
```

`run.yaml` 建议包含：

```yaml
run_id: 20260603-143000-customer-support
workspace_id: local
customer_id: demo
flow_id: customer-support
agent_id: support
status: completed
started_at: 2026-06-03T14:30:00+08:00
ended_at: 2026-06-03T14:32:10+08:00
inputs:
  channel: xiragarden
  message_id: local-1
tools_used:
  - exec
commands_used:
  - rg --version
verification:
  status: passed
artifacts:
  - artifacts/report.md
  - artifacts/evidence-ledger.csv
```

Run log 不是 chat transcript 的替代品。它要回答：

- 目标是什么？
- 输入和上下文是什么？
- 调用了哪些 tools / commands？
- 哪些步骤成功或失败？
- 做了哪些验证？
- 交付物在哪里？
- 是否产生 evolution candidate？

### Harness Evolution Loop

Xira 的 evolution loop 负责把真实运行中的失败、成功模式和用户纠正转成可审查的改进候选。

第一版只做 candidate generation，不做自动 promote。

核心流程：

```text
1. Observe
   收集 run log、audit、tool result、verification result、user feedback。

2. Diagnose
   把问题归因到 Task Spec / Context / Tool / Permission / Memory / Skill / Workflow / Eval / Observability。

3. Propose
   生成具体改进候选，例如改 connector wrapper、补 eval、改 task spec、增加 approval rule。

4. Verify
   用 acceptance case、golden task、schema check 或人工 review 验证。

5. Promote / Rollback
   第一版只支持人工 promote；失败候选归档。
```

建议 evolution record：

```yaml
id: EV-20260603-001
trigger: user_correction
run_id: 20260603-143000-customer-support
failure_layer: Tool
evidence:
  - command output exceeded the configured truncation limit
proposed_change:
  type: command_policy
  target: policies/command-runner.yaml
patch_scope:
  - policies/command-runner.yaml
  - verification/command-output.case.yaml
verification:
  - run command policy regression test
promotion_criteria:
  - output truncation and audit metadata validate
  - one golden task passes
rollback_criteria:
  - approved command can no longer run in the expected workspace
status: candidate
```

Evolution 不应该直接改 production flow。所有改进先进入 candidate 区，由交付工程师或负责人 review 后再 promote。

### Command Recipe Capture

`exec` 负责探索未知命令。Phase 1 只记录可复盘的 command recipe，不把命令自动提升成 connector。

建议流程：

```text
ad-hoc command
  -> successful run record
  -> command recipe
  -> reviewed flow / agent command step
  -> regression case
```

示例命令探索：

```text
which rg
rg --version
rg -n "customer_id" .
```

如果这组命令在客户环境中稳定，可以生成 command recipe artifact：

```text
.xira/
  command-recipes/
    20260603-rg-search.yaml
    tests/
      rg-search.case.yaml
```

Review gate 至少检查：

1. executable 能解析。
2. 运行目录和参数符合 policy。
3. 输出截断和敏感信息处理符合 policy。
4. run log / audit 记录完整。
5. timeout 和错误输出行为明确。
6. audit metadata 足够复盘。

这个设计允许 Xira 像 Codex / Claude Code 一样先探索本机未知工具，同时又能把成熟命令沉淀成对 B 可交付、可验收、可回滚的 connector。

## 概念映射

| 业务运行时概念 | ADK Go 概念 | 处理方式 |
| --- | --- | --- |
| Workspace | app / custom metadata | 自有 runtime 保存，ADK 只接收必要 context |
| Customer | custom metadata | 不交给 ADK 做主键 |
| Entrypoint | 无直接等价 | runtime 识别具体入口实例，例如某个 Feishu bot、iLink 微信入口或本地 XiraGarden 入口 |
| Flow | 无直接等价 | runtime 作为业务交付对象管理 |
| Channel | 无直接等价 | runtime 归一化真实平台 / 协议，例如 feishu、ilink、xiragarden、cli |
| ConversationScope | session | runtime 生成业务 scope，再映射到 ADK session |
| AgentProfile | agent | runtime 装配后创建 ADK agent |
| FlowPack | agent + instructions + tools | runtime 编译成一个或多个 ADK agent 可用结构 |
| SkillPack | instructions + tools | 作为 FlowPack 内部能力单元被引用 |
| Connector | tool / MCP tool | runtime 包装为 ADK tool |
| PermissionPolicy | callbacks / wrapper | 优先在 runtime 层拦截 |
| AuditEvent | ADK event + runtime event | runtime 统一落库 |
| OutboundMessage | final response / event | runtime 转换并投递 |

## Turn 执行流程

```text
1. Channel receives message
2. ChannelGateway creates InboundEnvelope
3. BusinessRuntime authenticates channel/account
4. EntrypointResolver resolves channel_type + app/bot/install into entrypoint_id
5. Agent selector uses requested `agent_id` when allowed, otherwise entrypoint.default_agent
6. SessionManager allocates ConversationScope
7. PolicyEngine resolves allowed skills/tools/connectors
8. AgentProfileManager / FlowPackManager builds instructions and tool set
9. ADK Engine creates / loads agent session
10. ADK Runner executes turn
11. ConnectorRuntime handles tool calls
12. PolicyEngine checks tool permission and secret access
13. ADK emits events and final response
14. VerificationRunner executes deterministic checks / acceptance cases
15. RunLogWriter records run summary, artifacts, verification result
16. EvolutionEngine creates candidate if failure / correction / reusable success appears
17. BusinessRuntime writes audit events
18. ChannelGateway sends response to original entrypoint / channel
```

关键设计点：

- ADK event stream 需要被转换为 runtime event。
- Tool call 不能直接访问 secret，必须经过 runtime policy。
- 最终消息投递由 Channel Gateway 完成，不由 ADK 完成。
- 如果 channel 支持 streaming，runtime 可以把 ADK partial events 映射成 streaming outbound。
- agent-only run 不需要 flow_id；如果后续需要交付复用，可以提升为 flow pack。
- verification、run log 和 evolution candidate 属于 Business Runtime，不属于 ADK。

## Session 设计

业务 session 和 ADK session 分开。

业务 conversation：

```text
workspace_id
customer_id
flow_id          # optional for agent-only run
entrypoint_id    # concrete entry instance, e.g. feishu-expense-bot
channel          # platform/protocol, e.g. feishu / ilink
channel_account
channel_app_id   # optional platform app/install id
bot_id           # optional platform bot identity
space_id
chat_id
topic_id
sender_id
business_object_id
```

Agent run：

```text
conversation_id
turn_id
agent_id
parent_run_id    # optional for delegation / reviewer / worker runs
```

ADK session：

```text
app_name
user_id
session_id
```

映射规则：

```text
ConversationScope -> stable hash -> conversation_id
conversation_id + agent_id -> ADK session_id
```

这样做的好处：

- 业务层可以支持复杂隔离规则。
- 同一个 channel 下多个机器人不会混用 conversation，例如 Feishu 报销 bot 和请假 bot。
- Feishu 和 iLink 是不同 channel；iLink 是微信侧入口，不是 Feishu 子类。
- 同一通 conversation 可以调用不同 agent。
- ADK 层仍按 agent 隔离底层 session。
- 未来更换 agent engine 时不会丢失业务 session 语义。

## Flow Pack 与 ADK 的关系

不建议直接把 ADK Skills 作为 Xira 的交付格式。

原因：

- 客户交付的对象是业务 flow，不是单个 agent 能力描述。
- 客户交付需要 permissions、secrets、connectors、tests、version。
- ADK Skills 更偏 agent 能力描述，不覆盖完整交付生命周期。
- 不同语言和版本的 ADK skill 支持成熟度可能不同。

建议：

```text
自有 FlowPack
  -> 选择 agent profiles
  -> 组合 skills
  -> 编译 instructions
  -> 注册 ADK tools
  -> 启动 MCP servers
  -> 绑定 process / HTTP connectors
  -> 应用 permissions
  -> 生成 audit metadata
```

## Connector 权限模型

每个 connector 声明：

- tool names
- input schema
- output schema
- required secrets
- allowed scopes
- timeout
- rate limit
- audit level
- human approval requirement

示例：

```yaml
id: internal-erp
type: process
command:
  - /opt/customer/bin/erp-tool

tools:
  - name: erp.order.read
    description: Read order details from internal ERP.
    input_schema: schemas/order_read.input.json
    output_schema: schemas/order_read.output.json
    secrets:
      - erp.service_account
    permissions:
      scopes:
        - customer_support
      approval: false
    timeout_ms: 8000
    audit: full_metadata
```

Runtime 在 ADK tool call 前做：

1. tool 是否存在
2. agent 是否有权调用
3. 当前 session scope 是否允许
4. secret 是否已绑定
5. 是否需要人工审批
6. 是否超过 rate limit

通过后才执行 connector。

## 权限模式与审批

第一版建议内置四种 permission mode：

| 模式 | 含义 | 适用场景 |
| --- | --- | --- |
| `observe` | 只允许读、版本探测、help、列目录、查看状态 | 新客户环境首次进入 |
| `ask` | 未匹配低风险规则的动作都询问 | 默认模式 |
| `trusted-flow` | flow pack 中声明的低风险 tool 可自动执行，高风险仍审批 | 已验收客户 flow |
| `locked` | 禁止 ad-hoc command，只允许 manifest tool | 严格交付 / 演示 / 生产环境 |

审批请求不应该只是 `Allow command?`。

建议 approval payload：

```yaml
approval_id: APR-20260603-001
run_id: 20260603-143000-customer-support
action_type: command
command: rg -n "harness" .
cwd: /Users/customer/workspace
risk_level: read_only
reason: Explore whether local workspace evidence contains this keyword.
expected_effect:
  reads:
    - local workspace files
  writes: []
  network: false
alternatives:
  - skip local evidence search
decision_options:
  - allow_once
  - allow_for_flow
  - deny
```

所有审批都应该写入 audit event，并在 run log 中保留摘要。

## 部署形态

### 本地桌面模式

```text
Desktop App
  -> local xira serve
  -> local config / encrypted secrets
  -> local ADK runner
```

适合：

- 自己使用
- demo
- 小客户
- 离线交付

### 客户内网模式

```text
Customer Server
  -> xira serve
  -> customer DB / secret backend
  -> internal ERP connectors
  -> Feishu / WeCom channel
```

适合：

- 私有化交付
- 内部系统接入
- 数据不出客户环境

### 云控制平面 + 本地 runtime

```text
Cloud Control Plane
  -> package registry
  -> license / update / telemetry policy

Customer Runtime
  -> local execution
  -> local secrets
  -> local audit
```

适合后续规模化：

- 统一发版
- license 管理
- flow pack 分发
- 远程健康状态

## MVP 路线

### Phase 1：核心运行时

目标：证明 ADK Go 可以作为新产品 agent core。

范围：

- `xira serve` runtime daemon
- `xira serve` 统一启动 enabled channel / entrypoint runner；不提供 `xira <channel> serve` 这类 per-channel daemon 命令
- `xira` 基础 CLI
- WebSocket channel
- ADK Go runner
- DeepSeek model adapter，只支持 `deepseek-v4-flash` / `deepseek-v4-pro`
- 内置 `exec` 工具，用于执行运行前未知的 shell 命令
- 内置 `read_file` / `write_file` / `list_dir` / `edit_file` 文件工具
- 一个内置 standalone agent profile
- agent profile manager
- flow pack wrapper 可选，不作为 Phase 1 必选项
- 本地 session store
- run log / artifact store
- verification runner
- evolution candidate 记录
- runtime event log
- 简单 XiraGarden / Web console

验收：

- 能从 WebSocket 发消息到 agent。
- Agent 能通过 DeepSeek adapter 完成 chat / stream。
- XiraGarden 能选择 agent profile 并通过 `xiragarden` channel 触发 agent run。
- Agent 能通过 `exec` 调用本地命令。
- Agent 能通过 `read_file` / `write_file` / `list_dir` / `edit_file` 读取、生成和修改 workspace 文件。
- GUI 的 agent 视图能显示 runtime-discovered agent 及其允许工具。
- Tool call 有 audit event。
- 每次 agent run 会生成 run log、artifact 目录和 verification result。
- 如果启用 flow wrapper，flow run 复用同一套 agent run、policy、verification 和 run log 链路。
- 失败或用户纠正可以生成 evolution candidate。
- Session 可以跨 turn 保持上下文。

### Phase 2：客户交付能力

目标：证明可以交付给真实客户。

范围：

- flow pack manager
- agent profile to flow pack promotion
- flow pack install / enable / disable
- skill pack manager
- integration permission policy
- secrets binding
- Feishu channel
- Feishu channel 由 `xira serve` 根据 entrypoint 配置自动启动，采用 Feishu SDK WebSocket 模式
- audit event 查询
- tool call replay / debug 页面
- package version metadata
- run log / artifact export
- command recipe review

验收：

- 一个客户 flow pack 可以安装并生效。
- 一个 Phase 1 中稳定的 agent profile 可以被包装成 flow pack。
- 一个客户 ERP 操作可以通过受控 command recipe 或后续 integration 接入。
- 未授权 tool call 会被拒绝并记录。
- 可以导出某次会话的审计链路。
- 一次 ad-hoc command 可以被保存为 command recipe，并通过 review gate 后纳入 flow pack。

### Phase 3：企业化

目标：支撑多个客户和稳定运维。

范围：

- workspace / customer 多租户模型
- RBAC
- enterprise secret backend
- update / rollback
- health check
- connector sandbox
- human approval
- evaluation cases
- evolution candidate review / promote / rollback
- multi-agent flow collaboration

验收：

- 不同客户配置完全隔离。
- flow pack 可以回滚。
- 关键工具调用可审批。
- 失败 connector 可定位。
- agent 行为可以用测试集回归。
- flow pack 改进候选可以通过 golden task 验证后再推广。

## 建议代码结构

```text
apps/
  xira/
    go.mod
    go.sum
    cmd/
      xira/
    internal/
      runtime/
        daemon.go
        entrypoints.go
        policy.go
        turn.go
        verification.go
      command/
        runner.go
        sandbox.go
        approval.go
      agent/
        engine.go
        adk/
          engine.go
          tools.go
          sessions.go
      agents/
        profile.go
        loader.go
        validator.go
      flows/
        pack.go
        loader.go
        compiler.go
        validator.go
      channels/
        runner.go
        feishu/
        websocket/
        httpapi/
      skills/
        pack.go
        loader.go
        compiler.go
        validator.go
      integrations/
        mcp/
        process/
        http/
        recipes/
      sessions/
      secrets/
      audit/
      events/
      artifacts/
      evolution/
        record.go
        diagnoser.go
        promoter.go
      evals/
      console/
  xiragarden/
    src/
      api/
      features/
      components/
      styles/

packages/
  xira-client/
```

其中：

- `apps/xira/cmd/xira` 提供单一命令入口，通过子命令承担 daemon 和运维动作。
- channel 生命周期由 `xira serve` 管理；禁止增加 `xira feishu serve`、`xira ilink serve` 这类 per-channel command。
- `apps/xira/internal/agent/engine.go` 定义 agent engine 抽象。
- `apps/xira/internal/agent/adk` 只负责 ADK adapter。
- `apps/xira/internal/agents` 定义可独立运行的 agent profile。
- `apps/xira/internal/runtime` 定义产品级 turn lifecycle。
- `apps/xira/internal/flows` 定义 flow pack 的加载、校验和编译。
- `apps/xira/internal/tools` 负责内置工具执行和协议。
- `apps/xira/internal/artifacts` 保存 run 产物、evidence ledger、handoff。
- `apps/xira/internal/evolution` 管理 evolution candidate、promote、rollback。
- `apps/xira/internal/evals` 管理 acceptance case、golden task 和 deterministic checks。
- `apps/xiragarden` 是 GUI 客户端，只通过 `xira serve` 的 HTTP/WebSocket API 访问 runtime，不直接 import `apps/xira/internal`。
- `packages/xira-client` 可以沉淀给 GUI 或外部客户端复用的 TypeScript API client。

## 关键 ADR

### ADR-001：采用 ADK Go 作为默认 Agent Engine

决策：

使用 ADK Go 作为新产品的默认 agent loop 内核。

理由：

- Go 生态与目标 runtime 语言一致。
- 避免自研完整 LLM/tool loop。
- ADK 已提供 runner、events、sessions、tools、MCP 等基础能力。
- 更容易跟进 Google agent 生态。

代价：

- 需要适配 ADK 抽象。
- 需要跟踪 ADK 版本变化。
- 部分业务语义不能直接交给 ADK。

缓解：

- 通过 `AgentEngine` adapter 隔离。
- Runtime 层不直接依赖 ADK 的业务概念。
- 保留 fake HTTP server / contract tests 作为测试手段；运行时不提供假模型回退。

### ADR-001A：第一版只支持 DeepSeek V4 Flash / Pro

决策：

第一版 model adapter 只支持 `deepseek-v4-flash` 和 `deepseek-v4-pro`。

理由：

- 这两个模型覆盖第一版 chat、stream、tool calls 验证需要。
- 模型成本适合高频本地调试和客户 demo。
- 限定模型范围可以避免第一版滑向通用 LLM gateway。
- DeepSeek adapter 可以作为后续 provider adapter 的最小样板。

代价：

- 第一版不能直接切换其他 OpenAI-compatible provider。
- 如果 DeepSeek API 语义变化，adapter 需要跟进。

缓解：

- 通过 `AgentEngine` / model adapter 隔离 provider 细节。
- model id 做白名单校验。
- 用 fake DeepSeek HTTP server 做 adapter contract tests，不在运行时保留替身模型 adapter。
- 后续需要时再新增 provider adapter，而不是把第一版改成通用 gateway。

### ADR-002：Business Runtime 位于 ADK 之上

决策：

flow、客户、渠道、权限、secrets、审计、flow pack 由 Business Runtime 管理，不由 ADK 管理。

理由：

- 这些是产品资产和商业边界。
- 这些概念需要适配客户交付场景。
- 未来更换 agent engine 时应保持稳定。

代价：

- 需要写 adapter。
- 需要维护一套自有 domain model。

缓解：

- 保持 domain model 小而明确。
- 只在 runtime 层做业务决策，ADK 层只做执行。

### ADR-003：客户 connector 默认进程外

决策：

客户定制 connector 默认使用 MCP、process CLI、HTTP / gRPC，不默认编译进核心 runtime。

理由：

- 支持闭源交付。
- 降低核心 runtime 变更风险。
- 方便独立升级和隔离故障。
- 更适合接入客户内部系统。

代价：

- 进程通信有开销。
- 协议和 schema 需要严格设计。
- 调试链路更长。

缓解：

- 明确 JSON schema。
- 所有 connector 调用写 audit event。
- 提供 integration test harness。

### ADR-004：自定义 Flow Pack 格式

决策：

定义自有 flow pack 格式，并在运行时编译成 ADK agent profiles / instructions / tools / MCP servers / integrations。

理由：

- Xira 的客户交付对象是业务 flow，不是 prompt、agent 或 workflow DAG。
- 客户交付需要版本、权限、secrets、tests、integrations。
- ADK Skills 不应成为唯一交付格式。
- 自有格式可以稳定表达业务交付需求。

代价：

- 需要实现 flow loader、validator、compiler。
- 需要维护文档和 SDK。

缓解：

- 第一版格式保持小。
- 用 JSON schema / YAML schema 校验。
- 提供示例 flow pack 和测试工具。

### ADR-004A：Phase 1 采用 Agent-First Entrypoint

决策：

第一版运行入口优先支持 standalone agent profile。Flow run 是同一执行链路上的业务包装，可以后置，不作为 Phase 1 的强制能力。

理由：

- 很多真实工作是探索、查询、生成、排查和实施，不需要先建模成完整业务 flow。
- ADK Go 的最小验证对象天然是 agent runner。
- XiraGarden 第一版更适合围绕 agent profile、tool call、built-in tools 和 run log 打通体验。
- 等 agent、connector、verification 和 artifact policy 稳定后，再沉淀成 flow pack 更符合交付演进路径。

代价：

- 第一版不能过早证明完整 flow pack 安装 / 升级 / 回滚。
- 如果 agent profile 缺少边界约束，容易退回“prompt + tools”的松散形态。

缓解：

- agent profile 也必须声明 model policy、permissions、context、artifact policy 和 verification defaults。
- run log / audit / verification / evolution 对 agent run 和 flow run 使用同一套机制。
- Phase 2 再把稳定 agent profile 提升为 flow pack。

### ADR-005：Run Log / Artifact / Evolution 是 Harness Stores

决策：

把 run log、artifact、verification result、evolution candidate 作为 Xira 的 Harness Stores，而不是普通 debug log。

理由：

- Xira 的交付对象是可复盘、可验收、可改进的业务 flow。
- 客户现场失败通常不是单点 bug，而是 spec、context、tool、permission、workflow、verification 的组合问题。
- 没有结构化运行证据，就无法判断一次改动是否真的提升了交付质量。
- run log 和 artifact 是 handoff、复盘、客户验收和后续演进的共同基础。

代价：

- 第一版需要额外设计持久化目录、schema 和脱敏规则。
- 每次 run 的存储量会上升。
- 需要定义哪些内容进入 audit、哪些内容进入 run log、哪些内容作为 artifact。

缓解：

- 第一版使用本地文件目录和 JSONL / YAML，不急于引入复杂数据库。
- artifact policy 明确保留、脱敏、导出和删除规则。
- evolution candidate 只生成候选，不自动改 production flow。

### ADR-006：`exec` 用于探索未知工具

决策：

Xira 内置 `exec` 工具，允许运行运行前未知的本地命令。Phase 1 不把每个 CLI 命令包装成专用 tool 或 connector；稳定命令先沉淀为 command recipe 和 flow / agent command step。

理由：

- 客户环境里的 CLI、脚本和内部工具无法在产品发布前全部预配置。
- Codex / Claude Code 这类工具的关键能力之一，是通过 shell 发现和操作未知本地工具。
- 对 B 交付不能长期依赖自由命令；必须把成功探索沉淀为有权限、测试、审计和复盘材料的 command recipe。是否升级为独立 connector 是后续架构决策。

代价：

- `exec` 的安全边界必须足够清楚。
- ad-hoc command 和 flow command step 之间需要 review 流程。
- XiraGarden / agent / API 都可能触发执行路径，runtime policy 必须统一。

缓解：

- 第一版只先打通工具路径；cwd 限制、风险分类、审批和 sandbox 后续再收敛，不阻塞最小 kernel。
- review gate 检查 executable、cwd、参数、timeout、error behavior、输出截断和 audit metadata。

### ADR-007：XiraGarden 是 Runtime Client，不是执行器

决策：

第一版优先做 XiraGarden，但 XiraGarden 只通过 Runtime API 操作 Xira，不直接执行客户 CLI、不直接读取 secrets、不绕过 policy。

理由：

- CLI / XiraGarden 是交付给客户的重要入口，但安全、权限、审计和复盘必须集中在 Runtime。
- Xira 运行前不知道客户有哪些 flow、agent 或外部 CLI，XiraGarden 必须动态读取 runtime 状态。
- 如果 XiraGarden 直接执行命令，会让同一动作在 XiraGarden、agent、API 中产生不同安全语义。

代价：

- 第一版即使是本地 GUI，也需要启动或嵌入 runtime daemon。
- XiraGarden 交互要处理流式事件、审批请求和长任务状态。

缓解：

- 本地开发可以先要求用户启动 `xira serve`，桌面打包阶段再由 XiraGarden 管理 runtime 子进程。
- XiraGarden 的所有动作生成 runtime event；高风险动作生成 approval request；完成后写 run log。

## 风险与缓解

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| ADK Go API 变化 | adapter 需要更新 | 通过 `AgentEngine` 隔离，锁版本 |
| DeepSeek API 语义变化 | chat / stream / tool call adapter 需要更新 | model id 白名单、contract tests、fake HTTP server |
| 模型范围过早泛化 | 第一版变成 LLM gateway，拖慢 Xira 验证 | 只支持 `deepseek-v4-flash` / `deepseek-v4-pro` |
| 过早强制所有任务 flow 化 | 第一版实现变重，真实干活入口不顺 | Phase 1 采用 agent-first，flow wrapper 后置 |
| ADK Skills 成熟度不足 | flow 交付受阻 | 自定义 flow pack，ADK 只作为编译目标 |
| Integration 失败难排查 | 客户交付风险 | 强制 audit、debug log、test harness |
| XiraGarden 直接执行客户 CLI | 绕过 policy、secrets、audit | XiraGarden 只调用 Runtime API，CLI 由 Command Runner 执行 |
| 无约束 shell 命令被暴露给 agent | 安全风险 | Command Runner 默认 cwd 限制、timeout、审批、audit；可复用命令必须进入 flow command step |
| 权限绕过 | 安全风险 | tool call 前统一经过 runtime policy |
| Secret 泄露 | 高风险 | secret binding、最小权限、访问审计 |
| Channel 语义复杂 | 消息错投或上下文污染 | 业务 session scope 独立建模 |
| 过早做平台化 | 交付速度下降 | MVP 只保留 WebSocket、Feishu、process、MCP |
| Flow 被误建模为 workflow | 产品边界偏向底层编排，削弱对 B 交付表达 | FlowPack 以业务目标、agent profiles、integrations、artifacts、audit cases 建模 |
| Agent profile 退化成 prompt 文件 | 缺少权限、上下文、验证和产物边界 | agent profile schema 强制声明 model policy、permissions、artifact policy 和 verification defaults |
| 缺少 run log / artifact | 失败无法复盘，成功无法复用 | 每次 agent run / flow run 强制生成 `run.yaml`、events、audit、verification 和 artifact 索引 |
| Artifact 泄露客户敏感数据 | 合规和信任风险 | artifact policy、privacy checklist、脱敏标记、导出前检查 |
| Evolution candidate 未验证就上线 | flow 行为漂移，客户现场回归 | candidate-only 默认策略，必须通过 acceptance case / golden task 和人工 review 才能 promote |
| Context / memory 污染 | agent 后续判断被过期材料误导 | context pack 区分 required / optional / forbidden，evolution 记录来源和失效条件 |
| `exec` 探索成果没有沉淀 | 每次客户现场都重复探索 | successful command 生成 recipe，并通过 review gate 纳入 flow / agent command step |

## 测试策略

第一版应该优先建立以下测试：

- flow pack schema validation
- agent profile schema validation
- agent run lifecycle tests
- skill pack schema validation
- flow routing tests
- DeepSeek model id whitelist tests
- DeepSeek chat / stream contract tests
- DeepSeek tool call mapping tests
- exec tool timeout / audit tests
- file tool read / write / list / edit tests
- command recipe review tests
- process integration timeout tests
- permission deny tests
- secret binding tests
- ADK tool call mapping tests
- session scope mapping tests
- channel inbound / outbound tests
- audit event golden tests
- agent-only run log tests
- agent profile to flow wrapper compatibility tests
- run log schema validation tests
- artifact index / retention / privacy policy tests
- verification runner acceptance case tests
- evolution record schema tests
- failure attribution classification tests
- command recipe review tests
- approval payload golden tests
- handoff completeness tests
- golden task regression tests
- end-to-end turn tests

建议每个 standalone agent profile 至少带一组 smoke cases：

```text
input message
expected agent
expected tools
expected permission behavior
expected final response shape
expected run log fields
expected artifact policy
```

建议每个客户 flow pack 都带最少一组 acceptance cases：

```text
input message
expected flow
expected route
expected tools
expected permission behavior
expected final response shape
expected audit events
expected artifacts
expected verification result
expected evolution behavior
```

建议每个 command recipe 都带最少一组 review cases：

```text
executable resolution
cwd policy
argument policy
timeout behavior
stderr behavior
exit code mapping
audit metadata
```

建议每个 evolution candidate 都带最少一组 promotion checks：

```text
source run exists
failure layer is classified
proposed patch scope is explicit
acceptance / golden task is attached
rollback condition is explicit
reviewer decision is recorded
```

## 与 PicoClaw 的关系

PicoClaw 可以参考：

- channel gateway 设计
- session scope 设计
- runtime event 设计
- hook / process hook 设计
- MCP 接入方式
- Web launcher / dashboard 思路
- tool result 区分 ForLLM / ForUser 的产品语义

Xira 还应该从 PicoClaw 的经验里保留三个教训：

- Runtime event 要服务 UI 和 debug，但 audit / run log 需要更稳定，不能混在一起。
- Hook / process hook 说明进程外扩展是有效路线，但 Xira 需要把它产品化为 connector manifest、schema 和 tests。
- Session scope 要从业务边界建模，再映射到底层 agent engine，而不是反过来。

但不建议复用：

- 当前 AgentLoop 代码结构
- 复杂 provider fallback 作为第一版核心
- steering / subturn 的完整实现
- 所有 channel 的历史兼容逻辑

新产品应该从 ADK Go 的 agent loop 出发，再逐步补齐真实交付场景需要的 runtime 能力。Xira 比 PicoClaw 更明确地把 Harness Stores、verification 和 evolution candidate 放进第一版边界，因为这是对 B 交付和后续复用的基础。

## 开放问题

1. 第一版交付目标是本地桌面还是客户服务器？
2. 是否需要云端 package registry？
3. flow pack 是否允许客户自己开发？
4. connector SDK 是否需要支持 Python / Node / Go？
5. audit log 是否需要满足特定行业合规要求？
6. 是否需要 license / activation 机制？
7. 是否允许 runtime 自动联网更新？
8. 是否需要把 Codex / Claude Code CLI 作为特殊 engine，而不仅是 tool？
9. run log / artifact 默认保留多久，客户是否能一键导出或销毁？
10. evolution candidate 的 promote 责任人是谁：交付工程师、客户管理员，还是产品维护者？
11. 每个 flow pack 至少需要多少 golden tasks 才允许交付？
12. exec tool 的 sandbox 后续用本地进程限制、Docker，还是客户环境提供的隔离机制？
13. artifact policy 是否需要区分内部证据、客户可见交付物和模型可见上下文？
14. XiraGarden 是否默认嵌入 runtime，还是要求用户先启动 `xira serve`？
15. 已探索成功的客户 CLI 命令什么时候可以进入 flow pack：按复用次数、客户验收、还是 artifact / audit 边界成熟度？
16. 后续是否需要独立 connector SDK，还是长期保持 exec + MCP 优先？

## 推荐下一步

1. 写一份更短的 product spec，锁定第一版客户场景、交付形态和 demo agent。
2. 锁定 ADK Go 版本，做最小 runner spike，验证 chat、stream、tool call event 映射。
3. 实现 DeepSeek adapter v0，只支持 `deepseek-v4-flash` / `deepseek-v4-pro`，同时准备 fake HTTP server contract tests。
4. 定义核心接口：`AgentEngine`、`TurnRequest`、`RuntimeEvent`、`ToolCall`、`VerificationResult`、`RunRecord`。
5. 定义 agent profile v0 schema，包含 model policy、instructions、permissions、artifact policy、verification defaults。
6. 实现 agent profile manager，并支持 `xira agent run`。
7. 实现 built-in tools v0：`exec`、`read_file`、`write_file`、`list_dir`、`edit_file`。
8. 实现模型工具调用路径：模型通过 `exec` 探索 `which`、`--help`、`--version` 和具体命令，不为每个 CLI 写专用 tool。
9. 实现 run log / artifact store v0：本地 `.xira/runs/<run_id>/`，支持导出。
10. 实现 verification runner v0：schema check、command check、agent smoke case、golden task。
11. 实现 evolution candidate v0：失败归因、候选记录、人工 review 状态，不自动 promote。
12. 做 XiraGarden + Runtime API + agent profile + built-in tools + ADK tool call 的端到端 demo。
13. 用一个真实 agent 任务验证：例如本地证据检索、客户资料汇总、售后处理或内部知识问答。
14. 定义 flow pack v0 schema，把稳定 agent profile 包装成 demo flow。
15. 把 successful ad-hoc command 保存为 command recipe，跑一次 review gate，验证 Xira 的交付闭环。

## 参考资料

- ADK Go Quickstart: https://adk.dev/get-started/go/
- ADK Event Loop: https://adk.dev/runtime/event-loop/
- ADK Session: https://adk.dev/sessions/session/
- ADK Skills: https://adk.dev/skills/
- ADK Go package: https://pkg.go.dev/google.golang.org/adk
- OpenAI Local Shell Tool: https://developers.openai.com/api/docs/guides/tools-local-shell
- Claude Code Tools Reference: https://code.claude.com/docs/en/tools-reference
- Claude Code Permissions: https://code.claude.com/docs/en/agent-sdk/permissions
- Claude Code Security: https://code.claude.com/docs/en/security
- WorkBuddy Product Guide: https://www.codebuddy.cn/docs/workbuddy/From-Beginner-to-Expert-Guide/Product-Guide
- Gemini CLI Configuration: https://github.com/google-gemini/gemini-cli/blob/main/docs/reference/configuration.md
- Goose Extensions: https://goose-docs.ai/docs/getting-started/using-extensions/
- OpenHands Sandbox: https://docs.openhands.dev/openhands/usage/sandboxes/overview
- Aider Git Integration: https://aider.chat/docs/git.html
- Harness Engineering Booklet: `/Users/yinwm/projs/weview/docs/research/harness-engineering-booklet.md`
- PicoClaw Session 系统: `docs/architecture/session-system.zh.md`
- PicoClaw Hook 系统: `docs/architecture/hooks/README.zh.md`
- PicoClaw Runtime Events: `docs/architecture/runtime-events.zh.md`
- PicoClaw Steering: `docs/architecture/steering.md`
- PicoClaw SubTurn: `docs/architecture/subturn.md`
