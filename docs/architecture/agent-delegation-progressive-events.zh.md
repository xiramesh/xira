# Agent 委派与阶段性事件流设计草案

> **状态: Historical draft。** 本文保留早期事件流 / UI surface 设计背景；
> 其中涉及 XiraGarden 的迁移假设已经废弃。当前事件投递与 channel 契约以
> `docs/architecture/xira-runtime-current-contract.zh.md`、
> `docs/architecture/xira-channel-contract-v0.zh.md`、`AGENTS.md` 和源码为准。

> 日期：2026-06-05
>
> 定位：Xira 运行时架构草案。
>
> 结论：这是架构级设计，因为它定义 channel、runtime、agent、tool、Activity、Run Inspector 和 policy 的边界，不是单个 UI 或 prompt 的实现细节。

## 摘要

Xira 的默认对话入口应该是一个 conversation owner，也就是 `main_agent`。用户和 `main_agent` 建立连续对话关系；工具调用、子 agent 调用、审计和阶段性进度都由 runtime 以事件流形式记录和投递。

阶段性回复不是一个独立能力，而是 agent run 的 event stream 在不同展示层上的投递结果：

```text
User message
  -> ChannelGateway
  -> SessionScope
  -> main_agent run
  -> assistant.status
  -> tool / child-agent events
  -> assistant.final
  -> Activity / Run Inspector / run log
```

多 agent 协作只是 event stream 里的一个事件类型。第一版不做自由多 agent 网络，而是采用受控的 orchestrator / worker 模式：

```text
main_agent
  -> delegate(research-assistant)
  -> synthesize final answer
```

## 设计目标

1. 让主对话像 Codex / Claude Code 一样自然，有阶段性进度，而不是等待最终答案。
2. 让 `main_agent` 保持用户上下文、口吻和最终回答责任。
3. 让 specialist agent 能被发现和委派，但所有发现、调用、权限和审计都经过 runtime。
4. 把用户可见对话、可读 Activity、完整 Run Inspector / trace 分层，避免 raw trace 污染回答正文。
5. v1 只实现足够的 Delegation Policy，不提前建设完整 policy engine。

## 非目标

- 不做所有 agent 自由互相发现、互相递归调用的网络。
- 不让 sub-agent 默认接管用户对话。
- 不把 `PROFILE.md` 当作可用 agent / tool 的唯一真相。
- 不在 v1 做复杂的 Scope Policy、Cost Policy、Approval Policy 分层。
- 不把 runtime 固定状态文案作为主要用户体验。

## 核心边界

### 0. main_agent 不是固定 agent ID

`main_agent` 是一个运行时角色，不是一个全局固定 agent。

当前代码里，入口配置位于 `workspace/entrypoints.yaml`，每个 entrypoint 都可以声明自己的 `default_agent` 和 `allowed_agents`。例如：

```yaml
entrypoints:
  - id: tui-local
    channel: tui
    default_agent: xira-assistant
    allowed_agents:
      - xira-assistant
      - research-assistant

  - id: feishu-yihao
    channel: feishu
    default_agent: yangsheng-yihao
    allowed_agents:
      - yangsheng-yihao
```

因此在一个具体 turn 中：

```text
main_agent = EntrypointResolver 解析出的 entrypoint.default_agent
```

如果请求显式带了 `agent_id`，并且该 agent 在当前 entrypoint 的 `allowed_agents` 内，则当前 turn 的 conversation owner 可以变成该 requested agent。

需要区分两类 allow-list：

```text
entrypoint.allowed_agents
  channel 入口层允许用户直接选择 / 路由到哪些 agent

profile.delegation.allow
  当前 caller agent 运行中允许委派给哪些 worker agent
```

这两者不能合并。前者是入口路由边界，后者是 agent-to-agent 调用边界。

### 1. Channel 只负责入口和投递

Channel 不直接理解 agent 协作。它只把平台消息归一化为 inbound envelope，并把 runtime 事件投递回具体入口。

```text
Channel Message
  -> InboundEnvelope
  -> RouteDecision
  -> SessionScope
  -> AgentTurnRequest
  -> RuntimeEvent stream
  -> Outbound messages
```

### 2. main_agent owns conversation

默认情况下，用户只和 `main_agent` 直接对话。`main_agent` 可以使用工具，也可以委派 sub-agent，但最终回答由 `main_agent` 生成。

普通任务优先走：

```text
main_agent -> tool -> final
```

只有在需要专业角色、并行处理、独立审查、长上下文压缩或稳定业务阶段时，才走：

```text
main_agent -> delegate(sub_agent) -> structured result -> final
```

### 3. sub-agent 默认是 worker

sub-agent 默认不直接给用户发消息，只返回结构化结果给 caller：

```yaml
summary: "..."
evidence_refs:
  - "kb://product/spec#ingredients"
confidence: "medium"
followup_needed: false
```

sub-agent 的 raw output、工具过程和内部 Activity 进入 run log / Activity / Run Inspector，不直接进入主对话。

## Event 与 Hook

PicoClaw 里 Event 和 Hook 不是同一个东西，Xira 也应该保留这个边界。

```text
Runtime Event
  发生了什么。只读、可订阅、可过滤、可用于 UI / audit / metrics / trace。

Hook / Interceptor
  谁能介入。只在明确检查点同步执行，可以修改、拒绝、审批或中止。
```

PicoClaw 的 `pkg/events.Event` 是 runtime event envelope，包含：

```text
id
kind
time
source
scope
correlation
severity
payload
attrs
```

PicoClaw 的 hook 设计建立在 event bus 之上，但职责不同：

```text
runtime event bus 负责广播
HookManager 负责拦截
HookMount 负责 hook 从哪里加载
```

对 Xira 来说，阶段性回复、Activity、Run Inspector 应该主要消费 Runtime Event，而不是 Hook。

适合 Runtime Event 的内容：

```text
run.started
assistant.status
agent.delegate.started
agent.delegate.completed
tool.started
tool.completed
capability_gap
assistant.final
```

适合 Hook / Interceptor 的内容：

```text
before_llm
after_llm
before_tool
after_tool
approve_tool
before_delegate_agent
after_delegate_agent
```

v1 不需要先实现完整 HookManager，但文档和事件 schema 必须预留这个边界。否则后续把“展示事件”和“同步拦截”混在一起，会让 Activity、审批、权限和上下文改写互相污染。

### Xira RuntimeEvent v1 应该升级的方向

当前 Xira 的 `RuntimeEvent` 只有：

```text
id
run_id
kind
time
source
severity
message
payload
```

这够做简单 WebSocket 推送，但不够支撑多 agent、child run、Activity 过滤和 Run Inspector。v1 delegation 需要向 PicoClaw event envelope 靠拢，至少补：

```yaml
id: evt_xxx
schema_version: 1
run_id: run_parent        # compatibility field; keep until API/UI filters migrate
kind: agent.delegate.started
time: "2026-06-05T..."
severity: info
source: runtime           # compatibility field; keep string in v1

source_detail:
  component: runtime
  name: delegate_agent

scope:
  entrypoint_id: ilink-muying-yuesao
  channel: ilink
  account: ""
  channel_app_id: ""
  bot_id: ""
  conversation_session_id: conversation:abc
  agent_session_id: session:main-agent:abc
  run_id: run_parent
  agent_id: main_agent
  child_agent_id: research-assistant
  chat_id: room-1
  chat_type: group
  topic_id: thread-1
  space_id: workspace-1
  space_type: tenant
  sender_id: user-1
  message_id: msg-1
  reply_to_message_id: msg-0
  reply_to_sender_id: user-0

correlation:
  trace_id: trace_abc
  parent_run_id: run_parent
  child_run_id: run_child
  parent_event_id: evt_parent
  tool_call_id: call_delegate_1

visibility:
  conversation: false
  activity: true
  inspector: true
  audit: true

payload:
  task_preview: "查找当前 workspace agent 和 tool registry"
```

`visibility` 是 Xira 自己需要的展示层字段。它不等于权限，只表示默认应该投递到哪个 UI 层。

`source` 顶层字符串是当前 API / UI 兼容字段；`source_detail` 才是新的结构化来源。Phase 1 不直接把 `run_id` 移进 `scope`，也不直接把 `source` 从 string 改成 object。否则会打断现有 Go / TypeScript contract、WebSocket channel filter 和 run store。兼容窗口内同时写顶层字段和结构化字段；等 API/UI 完成 schema-version 迁移后，再收敛读取路径。

## 上下文传递

上下文传递是这个设计里最容易出错的部分。不能让 sub-agent 默认继承全部 conversation、全部 tool result、全部 raw trace。

### 当前代码里的上下文来源

现有 Xira runtime 已经有这些上下文层：

```text
InboundContext
  channel、entrypoint_id、account、chat_id、sender_id、message_id、reply_to 等入口上下文

EntryPoint Decision
  default_agent、allowed_agents、session policy、matched_by

SessionScope
  根据 entrypoint/channel/chat/sender 等维度生成 conversation session

AgentSessionID
  conversation_session_id + agent_id 生成每个 agent 自己的底层 session

AgentHistory
  同一个 conversation 下，按 agent_id 分开的历史消息

Profile
  instructions、model policy、context policy、tool permissions、verification、artifacts

Run artifacts
  tool output、raw_output_path、LLM traces、events、audit events
```

这说明 Xira 已经不是“一个聊天历史塞给所有 agent”。它有 conversation session 和 per-agent session 的分层，这个方向是对的。

### v1 需要明确的 ContextPacket

agent 委派时，caller 不应该直接把一大段 prompt 拼给 child。runtime 应该构建一个显式 `ContextPacket`。

建议结构：

```yaml
context_packet:
  id: ctxpkt_001
  mode: delegate_worker
  created_at: "2026-06-05T..."

  caller:
    agent_id: main_agent
    run_id: run_parent
    conversation_session_id: conversation:abc
    agent_session_id: session:main-agent:abc

  target:
    agent_id: research-assistant
    profile_version: 0.1.1
    profile_instruction_hash: sha256:...
    profile_instruction_ref: profile://research-assistant/instructions
    allowed_tools:
      - command.run
      - shell.run
      - tool_output.read
      - read_file
      - write_file
      - list_dir
      - edit_file
    allowed_tools_hash: sha256:...
    run_id: run_child
    session_mode: ephemeral_worker
    delegation_depth: 1

  task:
    user_intent: "核对 Xira Phase 1 delegation 设计是否能落地"
    worker_task: "只查架构文档、workspace agents 和当前 tool registry，返回证据摘要"
    output_schema: evidence_summary_v1
    constraints:
      - "不要直接修改代码"
      - "不要扩大到 Phase 2 HookManager"

  conversation:
    current_user_message:
      include: true
      text_ref: conversation://current-turn/user-message
    recent_messages:
      include: bounded
      max_messages: 6
      source_agent: main_agent
    summary:
      include: optional
      ref: conversation://summary/current

  evidence:
    refs:
      - kb://products/sku-123
      - tool://run_parent/tool_call_1/output
    max_inline_chars_per_ref: 2000
    raw_output_policy: preview_only

  channel_scope:
    entrypoint_id: ilink-muying-yuesao
    channel: ilink
    chat_type: group
    sender_id: user-1

  policy:
    expose_child_output_to_user: false
    allowed_tools_source: target_profile
    allow_secrets: false
    allow_cross_workspace: false

  redactions:
    secrets: removed
    raw_channel_payload: excluded
    unrelated_agent_history: excluded
```

#### Context refs resolution contract

`context_refs` 里的 ref 是 runtime 逻辑引用，不是 child run 可以直接读取的文件路径。

例如：

```text
tool://run_parent/tool_call_1/output
```

这类 ref 不能原样交给 child 让 `tool_output.read` 读取。当前 `tool_output.read` 只允许读取当前 run 目录下的相对 `artifacts/tool-outputs/*.json`，并且禁止跨 run 路径。Phase 1 必须补一个 runtime-owned ref resolver，在 `ContextPacket` 构建阶段完成授权和 materialize：

```text
parent run raw artifact
  -> runtime ref resolver
  -> bounded preview / child-local copied artifact
  -> ContextItem with provenance
```

resolver 只能做两件事：

1. 把允许传递的 preview 或 artifact 复制 / materialize 到 child run 自己的 artifact namespace，例如 `artifacts/context/<context_item_id>.json`。
2. 记录 provenance：`source_ref`、`source_run_id`、`source_tool_call_id`、hash、byte count、redaction / truncation reason。

如果 ref 不能解析、没有权限、超出 policy，runtime 必须发 `context.item.redacted` 或 `context.packet.failed`，不能把不可展开的 `tool://...` 当作已经传递的证据。

child run 内的工具只看到 child-local ref。除非未来实现了显式授权的跨 run artifact resolver，否则 worker 不允许通过 `../run_parent`、绝对路径或 parent run 的 `raw_output_path` 读取父 run 原始输出。

### 三种上下文模式

需要区分 direct run、worker delegate、handoff。

#### 1. Direct Agent Run

用户直接和某个 agent 对话。

```text
context = current user message
        + that agent's conversation history
        + profile instructions
        + allowed tools
```

这接近当前 `runtime.RunAgent` 的行为。

#### 2. Delegate Worker

main_agent 调 worker agent 做子任务。

```text
context = explicit task
        + bounded current-turn context
        + selected evidence refs
        + target profile instructions
        + target allowed tools
```

默认不继承：

```text
全量 conversation history
main_agent 内部推理
其他 agent 历史
raw trace
secret
未授权 KB
```

worker 返回结构化结果给 caller。worker 的 raw output 进入 Activity / Run Inspector，不直接进入 Conversation。

#### 3. Handoff

用户对话控制权转给另一个 agent。

```text
context = conversation summary
        + selected recent messages
        + handoff reason
        + target profile instructions
```

handoff 是后置能力，v1 不实现。v1 只做 delegate worker。

### worker session 默认应是 ephemeral

当前 Xira 已有 `conversation_session_id + agent_id -> agent_session_id` 的持久 agent session 模型。这个模型适合 direct run，也适合长期 specialist 和用户直接对话。

但对于 `delegate_worker`，默认应该使用 ephemeral worker session：

```text
session_mode: ephemeral_worker
```

原因：

1. worker 是为当前任务服务，不应该把一次子任务污染成长期对话记忆。
2. 同一个 research-assistant 在不同 parent task 中看到的上下文不同。
3. child run 可审计即可，不一定要进入 child agent 的长期 conversation memory。

只有明确配置时，才允许：

```yaml
delegation:
  child_session_mode: conversation_agent
```

这个模式适合稳定业务 flow 或真正的 handoff，不适合普通 worker。

### 上下文选择规则

runtime 构建 ContextPacket 时应按顺序选择：

1. 当前用户消息。
2. caller 给 worker 的明确 task。
3. caller 指定的 evidence refs。
4. 当前 conversation 的 bounded recent messages。
5. 已存在的 conversation summary。
6. target profile 的 required context。
7. target profile 允许的 optional context。

然后过滤：

1. target profile forbidden context。
2. 当前 entrypoint/session scope 不允许的数据。
3. secret、token、raw credential。
4. 其他 agent 的 raw internal output。
5. 超出 token budget 的低优先级内容。

### ContextItem

每个传给 child 的上下文片段都应该带 provenance：

```yaml
id: ctxitem_001
kind: user_message | conversation_summary | evidence_ref | tool_result | kb_doc | artifact_ref
source: conversation://current-turn/user-message
owner_agent: main_agent
visibility: child_only
content_preview: "用户问：这版 delegation 设计是否能按当前代码落地？"
content_hash: sha256:...
included_chars: 48
redacted: false
```

Run Inspector 记录 ContextPacket 和 ContextItem 的 metadata。对于敏感内容，只记录 hash、ref 和 preview，不记录全文。

### child result

child agent 的输出必须是 caller 可消费的结构化结果，而不是自由聊天文本：

```yaml
delegate_result:
  agent_id: research-assistant
  run_id: run_child
  status: completed
  summary: "当前 workspace 存在 xira-assistant 和 research-assistant，profile 工具权限包含 read_file/list_dir/command.run。"
  evidence_refs:
    - context://run_child/context/ctxitem_profile_001
    - artifact://run_child/artifacts/tool-outputs/call_registry_scan.json
  limitations:
    - "当前 profile 没有显式 input/output schema 字段"
  confidence: medium
  followup_needed: true
```

上面的 `evidence_refs` 是 runtime 校验 / 规范化后的 canonical refs，不是 child model 可以自由返回的 `workspace://...` URI。原始 workspace 文件或 parent run output 必须先由 runtime materialize 到 ContextPacket 或 child run artifact，再进入 `DelegateAgentResult.EvidenceRefs`。

main_agent 只能基于 `delegate_result` 和 evidence refs 综合最终回答。不能直接把 child raw answer 原样转给用户，除非 `expose_child_output_to_user=true`。

### 上下文事件

上下文传递本身也要发 runtime events：

```text
context.packet.started
context.item.included
context.item.redacted
context.packet.completed
context.packet.truncated
```

这些事件默认进 Activity / Run Inspector，不进 Conversation。

`context.packet.truncated` 是 Phase 1 独立事件，不只是 `context.packet.completed.payload.truncated=true`。当 ContextPacket 因 token budget、item limit、raw output policy 或 forbidden context 被裁剪时，runtime 必须发 `context.packet.truncated`；`context.packet.completed` 可以在 payload 里冗余 `truncated=true` 和 summary，方便旧 UI 显示。

## 阶段性回复

阶段性回复应来自模型生成的自然状态文本，而不是 runtime 写死的模板文案。

推荐事件：

```text
assistant.status
tool.started
tool.completed
agent.delegate.started
agent.delegate.completed
capability_gap
assistant.final
```

`tool.completed` 是 Phase 1 之后的 canonical name；当前代码里的 `tool.finished` 是 legacy alias，UI / event normalizer 需要在一个兼容窗口内把它映射成 `tool.completed`。

示例：

```text
assistant.status:
  我先核一下架构文档和当前 workspace profiles，避免凭印象回答。

agent.delegate.started:
  research-assistant

tool.started:
  read_file

agent.delegate.completed:
  research-assistant returned evidence summary

assistant.final:
  根据当前资料，Phase 1 可以用 xira-assistant 委派 research-assistant，但需要先补 profile delegation 字段和 runtime-owned delegate_agent。
```

`assistant.status` 可以自然，但必须遵守一个边界：只能描述正在做什么，不能提前声明工具或子 agent 尚未返回的事实。

允许：

```text
我先核一下资料和规则。
```

不允许：

```text
我查到这版一定可以直接落地。
```

runtime 固定文案只做兜底，例如模型没有输出 status、channel 不支持流式、或 Activity 需要稳定状态标签。

### assistant.status producer contract

`assistant.status` 不能从普通 ADK streaming text 里隐式猜测。否则状态文本可能被 `latestText` fallback 当成 final，也可能被写入长期 session history。

Phase 1 必须采用一个明确 producer，并在 event payload 中记录 `producer`：

```text
runtime.status_tool       推荐。runtime-owned emit_status(message) tool，message 只生成 assistant.status。
adk.reserved_status_event ADK 如果支持 reserved event / metadata，可以映射成 assistant.status。
stream.status_parser      仅允许解析有保留 framing 的 status token，不能解析任意普通文本。
runtime.fallback          模型没有 status 或 channel 不支持流式时的固定兜底。
```

无论采用哪种 producer，都必须满足：

```text
assistant.status.message is user-readable status text
assistant.status payload includes producer
assistant.status is emitted as an event, not returned as ADK final text
assistant.status is excluded from latestText / final fallback
assistant.status is excluded from AppendAgentMessages and durable session history
assistant.status may appear in Conversation / Activity / Run Inspector according to visibility
```

## Agent Registry、Profile 与 Delegation Policy

可用 agent 和可用 tool 应采用同一类机制：registry 提供实际能力，profile 声明设计上限，policy 决定本 turn 能否使用。

```text
AgentRegistry
  当前 runtime 实际安装、启用、可发现的 agent

AgentProfile
  当前 agent 的设计意图、instructions、tool allow-list、delegation allow-list

DelegationPolicy
  当前 agent 能否委派、能委派给谁、能委派几层、子输出是否给用户

Runtime
  根据 registry + profile + policy 生成本 turn 的真实可用能力
```

`PROFILE.md` 或 `agent_profile.yaml` 不应该是唯一真相。它适合声明上限和意图，不适合声明当前 runtime 实际存在什么。实际存在、版本、schema、启用状态应该来自 registry。

Phase 1 的 `AgentRegistry` 先做当前代码能支撑的薄封装：

```text
installed = profile loaded by agents.Manager
valid = profile passed Profile.Validate()
enabled = installed && valid
discoverable = enabled && visible through runtime list APIs
delegatable = enabled && caller profile delegation.allow contains target agent_id
```

也就是说，Phase 1 不新增 agent-level `enabled` frontmatter 字段。`entrypoint.enabled` 只表示入口是否启用；`entrypoint.allowed_agents` 只限制这个入口可以直接选择哪个 conversation owner；`profile.delegation.allow` 才限制 caller 可以委派给谁。

`input_schema` / `output_schema` 在 Phase 1 可以是 registry metadata 的可选字段。当前 profile 没有 schema 字段时，registry 返回默认 schema：

```text
input_schema = delegate_task_v1
output_schema = delegate_result_v1
```

如果后续 profile 增加显式 schema 字段，registry 再优先使用 profile schema。安全检查里的 “target agent is enabled” 在 Phase 1 等价于 “target profile exists in agents.Manager and is valid”。

推荐 v1 profile 片段：

```yaml
id: xira-assistant

permissions:
  tools:
    - read_file
    - list_dir
    - command.run

delegation:
  enabled: true
  allow:
    - research-assistant
  max_depth: 1
  max_parallel: 1
  expose_child_output_to_user: false
  return_to: caller
```

## v1 Delegation Policy

第一版以 Delegation Policy 为主就够，但它要覆盖最小安全边界：

```text
能不能调
能调谁
最多几层
最多并发几个
子 agent 输出给谁看
结果怎么回到 caller
```

推荐默认值：

```yaml
delegation:
  enabled: false
  allow: []
  max_depth: 1
  max_parallel: 1
  default_max_duration_ms: 30000
  max_duration_ms: 120000
  expose_child_output_to_user: false
  return_to: caller
```

只有主 agent 或明确授权的 coordinator agent 可以开启 delegation。sub-agent 默认不能继续 spawn sub-agent。

## DelegateAgentTool

v1 可以把 agent 委派暴露成一个受控工具形态，而不是让模型绕过 runtime 自由调用。但它不是普通 builtin tool，不能落进 `internal/tools` registry。

`delegate_agent` 应由 runtime 注入到 ADK tool set：它需要访问 `Service`、caller profile、DelegationPolicy、depth / parallel accounting、ContextPacket、child session mode、parent / child run correlation。这些上下文不属于普通 workspace tool registry。

```text
delegate_agent(agent_id, task, context_refs, expected_output_schema)
```

输入：

```yaml
agent_id: research-assistant
task: "查找当前 workspace 中可用 agent 和 built-in tools，并返回证据摘要"
context_refs:
  - "conversation://current-turn"
expected_output_schema: "evidence_summary_v1"
```

输出：

```yaml
agent_id: research-assistant
status: completed
summary: "workspace profiles 包含 research-assistant，profile 工具权限包含 read_file/list_dir/command.run。"
evidence_refs:
  - "context://run_child/context/ctxitem_profile_001"
  - "artifact://run_child/artifacts/tool-outputs/call_registry_scan.json"
confidence: high
followup_needed: false
```

输出示例里的 `evidence_refs` 表示 DelegateExecutor 返回给 caller 的 canonical refs。child 原始 JSON 不能通过返回裸 `workspace://` 或 parent `tool://...` 来声明证据；这些 ref 必须先通过 ContextPacket materialization 或 child run artifact registration。

runtime 在执行前检查：

```text
agent_id 是否存在于 AgentRegistry
caller profile 是否允许 delegate 给该 agent
requested_child_depth = parent_delegation_depth + 1
requested_child_depth <= max_depth
active_child_count_before_start < max_parallel
该 agent 是否启用且 schema 可用
```

Depth / parallel 的检查口径必须以“即将启动的 child run”计算，而不是只看当前 parent 状态：

```text
root main_agent run depth = 0
first delegated worker depth = 1
requested_child_depth = parent_delegation_depth + 1
allow depth only when requested_child_depth <= max_depth

active_child_count_before_start = same parent run 下尚未进入 terminal state 的 child 数
allow new child only when active_child_count_before_start < max_parallel
```

所以 `max_depth=1` 表示允许 main -> worker，但拒绝 worker -> grandchild；`max_parallel=1` 表示同一个 parent run 已有 1 个 active child 时必须拒绝再启动第二个 child。

## Capability Gap

如果现有 agent 或工具不足以完成任务，main_agent 不应该硬答。runtime 应该记录能力缺口：

```json
{
  "type": "capability_gap",
  "needed_capability": "runtime hook approval policy",
  "reason": "Phase 1 only defines built-in delegation policy, HookManager is deferred to Phase 2",
  "attempted_agents": ["research-assistant"],
  "suggested_action": "create_ephemeral_agent"
}
```

处理顺序：

```text
临时缺能力 -> ephemeral agent / extra tool
重复缺能力 -> draft new agent profile
高风险缺能力 -> human approval / handoff
资料不足 -> ask user for scope / more info
```

v1 可以先只记录 `capability_gap` 并让 main_agent 向用户说明限制，不急着自动创建长期 agent。

## 展示层

同一个 runtime event stream 应分三层展示：

```text
Conversation
  用户可读的 assistant.status 和 assistant.final

Activity
  当前 turn 下的可读 agent/tool 进度摘要

Run Inspector
  raw runtime events、tool payload、agent spans、permissions、artifacts、session scope
```

主对话不展示 raw trace。Activity 可以展示简短进度，Run Inspector 承载完整审计。

## v1 交付范围

建议第一版只实现：

```text
AgentRegistry
  list loaded+valid profiles as enabled agents in Phase 1
  expose id / name / description / version
  expose default input_schema / output_schema when profile has no explicit schema
  invalid profile semantics are fail-fast in Phase 1, matching current loader behavior

Entrypoint conversation owner resolution
  current main_agent = resolved default_agent or allowed requested agent
  entrypoint.allowed_agents remains separate from delegation.allow

Profile delegation field
  delegation.enabled
  delegation.allow
  delegation.max_depth
  delegation.max_parallel
  delegation.default_max_duration_ms
  delegation.max_duration_ms
  delegation.expose_child_output_to_user
  delegation.child_session_mode default ephemeral_worker

RuntimeEvent envelope
  keep top-level run_id and string source for compatibility
  add schema_version and source_detail as structured source
  scope with entrypoint/channel/conversation/agent/run fields
  correlation with parent_run_id/child_run_id/tool_call_id
  visibility for conversation/activity/inspector/audit

DelegateAgentTool
  runtime-owned ADK tool injection
  runtime checked agent delegation
  ContextPacket construction
  structured child result

Runtime events
  assistant.status
  agent.delegate.requested
  agent.delegate.rejected
  agent.delegate.allowed
  agent.delegate.started
  agent.delegate.completed
  agent.delegate.failed
  agent.delegate.timeout
  agent.delegate.cancelled
  agent.delegate.result_delivered
  tool.started
  tool.completed
  tool.failed
  context.packet.started
  context.item.included
  context.item.redacted
  context.packet.completed
  context.packet.truncated
  context.packet.failed
  capability_gap
  assistant.final

This is the Phase 1 required event surface, not only conversation-facing highlights. It includes new delegation events plus existing runtime diagnostic events that Phase 1 depends on. Rejection, failure, timeout, context item and result-delivery events are required so the state machine and acceptance cases can be implemented without relying on logs.

Activity / Run Inspector mapping
  readable summary in Activity
  raw details in Run Inspector
```

Phase 1 的 `enabled agent` 语义是 `loaded+valid`，不是“加载失败但显示 disabled”。当前 workspace loader 遇到任一 invalid profile 会返回错误并阻止 runtime 启动。为了避免 registry 语义和 loader 行为冲突，Phase 1 先沿用 fail-fast：

```text
invalid PROFILE.md / SOUL.md / model policy
  -> workspace agent loader returns error
  -> runtime startup / agent manager construction fails
  -> registry is not partially published
```

如果后续需要在 UI 里展示 disabled / unavailable agent，必须先把 loader 改成 error-collecting registry，并在 registry entry 上暴露 `status=disabled|invalid`、`reason` 和可审计诊断；不能在 Phase 1 文档里同时要求 fail-fast loader 和 partially enabled registry。

暂缓：

```text
full policy engine
full HookManager
before_llm / after_llm / before_tool / after_tool interceptors
multi-level recursive agent graph
conversation handoff
automatic permanent agent creation
cross-workspace / cross-tenant delegation
```

## Phase 1 Implementation Spec

Phase 1 明确只实现 Runtime Event + controlled delegation + ContextPacket。

Hook / Interceptor 是 Phase 2。Phase 1 的正确性不能依赖 Hook；安全检查、上下文过滤和 delegate 权限必须由 runtime 内置执行。

```text
Phase 1
  RuntimeEvent envelope
  AgentRegistry
  DelegationPolicy
  DelegateAgentTool
  ContextPacket
  Activity / Run Inspector mapping

Phase 2
  HookManager
  before_llm / after_llm
  before_tool / after_tool
  approve_tool
  before_delegate_agent / after_delegate_agent
  external process hooks
```

### Current-Code Constraints

Phase 1 不能把本文的目标形态直接一次性替换进当前代码。当前实现已有 API / UI contract、session persistence 和 tool registry 边界，落地时必须先兼容这些事实。

#### 1. ephemeral_worker 不能默认复用 RunAgent

当前 `runtime.RunAgent` 是 direct run 路径。它会：

```text
1. 通过 entrypoint / session policy 分配普通 conversation session
2. 为 agent 构造 agent_session_id
3. ADK 路径 hydrate 既有 agent conversation history
4. 成功后 AppendAgentMessages，持久化 user/tool/assistant messages
```

这和 `ephemeral_worker` 的语义冲突。worker 默认只服务当前 parent run，不应污染长期 conversation memory，也不应自动 hydrate target agent 的既有历史。

因此 Phase 1 必须新增内部 child-run path，而不是直接调用公开的 `RunAgent`：

```text
DelegateAgentTool
  -> DelegateExecutor
  -> BuildContextPacket
  -> RunChildAgent
      skip entrypoint Resolve
      skip normal session allocation
      skip ADK history hydrate by default
      skip AppendAgentMessages by default
      persist child run log / events / audit / artifacts
      attach parent_run_id / child_run_id correlation
  -> DelegateAgentResult
```

`RunChildAgent` 可以复用底层 model/tool execution primitives，但不能复用 direct-run 的 session lifecycle。

允许的 child session mode：

```text
ephemeral_worker
  default。只保留 run log，不写长期 agent history，不 hydrate 历史。

conversation_agent
  仅显式配置时使用。可 hydrate / persist target agent history，适合 handoff 或稳定 flow。
```

#### 2. RuntimeEvent schema 必须兼容当前 API / UI

当前 Go / TypeScript contract 依赖：

```text
RuntimeEvent.run_id    顶层字段
RuntimeEvent.source    string
RuntimeEvent.payload   map / Record
```

当前 channel WebSocket 过滤还依赖：

```text
evt.RunID
payload["channel"]
```

所以 Phase 1 不能把 `run_id` 直接移进 `scope`，也不能把 `source` 直接从 string 改成 object。

Phase 1 采用 additive schema：

```go
type RuntimeEvent struct {
    ID            string            `json:"id"`
    RunID         string            `json:"run_id,omitempty"` // keep for API/UI compatibility
    Kind          string            `json:"kind"`
    Time          time.Time         `json:"time"`
    Source        string            `json:"source"` // keep legacy string
    Severity      string            `json:"severity,omitempty"`
    Message       string            `json:"message,omitempty"`
    Payload       map[string]any    `json:"payload,omitempty"`

    SchemaVersion int               `json:"schema_version,omitempty"`
    SourceDetail  *EventSource      `json:"source_detail,omitempty"`
    Scope         *EventScope       `json:"scope,omitempty"`
    Correlation   *EventCorrelation `json:"correlation,omitempty"`
    Visibility    *EventVisibility  `json:"visibility,omitempty"`
}
```

Compatibility rules：

```text
run_id remains top-level and also appears in scope.run_id when present.
source remains a string; source_detail is additive.
payload["channel"] remains populated until WebSocket filters use scope.channel.
payload["entrypoint_id"] remains populated for channel-originated events until filters use scope.entrypoint_id.
Run store keeps writing legacy fields until XiraGarden type is updated.
XiraGarden accepts both legacy and additive fields during migration.
```

Channel event delivery compatibility:

```text
All delegation, context and child-run events for a channel-originated turn must continue to write legacy payload["channel"] and payload["entrypoint_id"] from normalized InboundContext.
Child-run events must not rely only on scope.channel or child run_id during the migration window.
If a future channel filter registers correlated run IDs, the parent event must register child_run_id before the first child event can be emitted without legacy payload channel.
Until that run-set registration exists, every child run event must carry legacy payload channel identity itself.
```

This is required because the current channel WebSocket filter accepts an event only when either `evt.RunID` is already in the per-connection run set, or `payload["channel"]` matches the channel. A new child run ID is not known to the filter until an event with matching legacy channel payload registers it.

Only after API filters, run store loading and XiraGarden types are migrated can `source` become an object or `run_id` move fully into `scope`.

#### 3. delegate_agent 不是普通 builtin tool

`delegate_agent` 不能放进普通 `internal/tools` registry。

普通 tools registry 只知道 workspace tool execution；它没有 Service、caller profile、DelegationPolicy、depth、parallel accounting、ContextPacket、session mode、parent/child run correlation。如果把 `delegate_agent` 当普通 builtin tool，会造成循环依赖或绕不开 policy。

Phase 1 应把它实现为 runtime-owned ADK tool：

```text
Service.adkTools(...)
  -> normal tools from toolRegistry(profile)
  -> plus delegate_agent only when profile.delegation.enabled

delegate_agent tool handler
  -> DelegateExecutor
  -> runtime policy checks
  -> ContextPacket builder
  -> child-run path
```

它不进入 `internal/tools.NewBuiltinRegistry`。它也不要求 profile 在 `permissions.tools` 里列出 `delegate_agent`。是否暴露由 `profile.delegation.enabled`、`profile.delegation.allow` 和 runtime registry 共同决定。

#### 4. profile delegation 字段需要 loader 迁移

当前 `agents.Profile` 和 `profileFrontmatter` 没有 `delegation` 字段。Phase 1 需要同时改：

```text
apps/xira/internal/agents/profile.go
apps/xira/internal/agents/loader.go
profile validation / defaults
builtin profiles
workspace PROFILE.md frontmatter schema
```

Frontmatter 示例：

```yaml
---
id: xira-assistant
name: Xira Assistant
version: 0.1.4
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
delegation:
  enabled: true
  allow:
    - research-assistant
  max_depth: 1
  max_parallel: 1
  default_max_duration_ms: 30000
  max_duration_ms: 120000
  child_session_mode: ephemeral_worker
---
```

Defaulting：

```text
delegation.enabled = false
delegation.allow = []
delegation.max_depth = 1
delegation.max_parallel = 1
delegation.default_max_duration_ms = 30000
delegation.max_duration_ms = 120000
delegation.return_to = caller
delegation.child_session_mode = ephemeral_worker
delegation.expose_child_output_to_user = false
```

Validation：

```text
max_depth must be 0 or 1 in Phase 1
max_parallel must be >= 1 when delegation.enabled = true
default_max_duration_ms must be > 0
max_duration_ms must be >= default_max_duration_ms
child_session_mode must be ephemeral_worker or conversation_agent
allow entries must be non-empty agent IDs
```

Builtin profiles should default delegation disabled unless a profile explicitly opts in.

#### 5. event naming uses canonical names plus legacy aliases

Current code already emits:

```text
tool.started
tool.finished
tool.failed
adk.event with final=true
```

Phase 1 canonical names are:

```text
tool.started
tool.completed
tool.failed
assistant.final
```

Migration rule：

```text
emit canonical tool.completed in new code
keep tool.finished as legacy alias for one compatibility window, or let UI normalize it
emit assistant.final when final response is resolved
keep adk.event{final:true} for low-level inspector diagnostics
```

Activity / UI should normalize:

```text
tool.finished -> tool.completed
adk.event final=true -> assistant.final only for fallback display
```

### Phase 1 Schemas

#### RuntimeEvent

```go
type RuntimeEvent struct {
    ID            string            `json:"id"`
    RunID         string            `json:"run_id,omitempty"`
    Kind          string            `json:"kind"`
    Time          time.Time         `json:"time"`
    Source        string            `json:"source"`
    Severity      string            `json:"severity,omitempty"`
    Message       string            `json:"message,omitempty"`
    Payload       map[string]any    `json:"payload,omitempty"`
    SchemaVersion int               `json:"schema_version,omitempty"`
    SourceDetail  *EventSource      `json:"source_detail,omitempty"`
    Scope         *EventScope       `json:"scope,omitempty"`
    Correlation   *EventCorrelation `json:"correlation,omitempty"`
    Visibility    *EventVisibility  `json:"visibility,omitempty"`
}
```

`Source` 是当前兼容字段。`SourceDetail` 表示谁发出事件：

```go
type EventSource struct {
    Component string `json:"component"` // runtime, agent, tool, model, channel
    Name      string `json:"name,omitempty"`
}
```

`Scope` 表示事件属于哪个业务上下文：

```go
type EventScope struct {
    EntrypointID          string `json:"entrypoint_id,omitempty"`
    Channel               string `json:"channel,omitempty"`
    Account               string `json:"account,omitempty"`
    ChannelAppID          string `json:"channel_app_id,omitempty"`
    BotID                 string `json:"bot_id,omitempty"`
    ConversationSessionID string `json:"conversation_session_id,omitempty"`
    AgentSessionID        string `json:"agent_session_id,omitempty"`
    RunID                 string `json:"run_id,omitempty"`
    AgentID               string `json:"agent_id,omitempty"`
    ChildAgentID          string `json:"child_agent_id,omitempty"`
    ChatID                string `json:"chat_id,omitempty"`
    ChatType              string `json:"chat_type,omitempty"`
    TopicID               string `json:"topic_id,omitempty"`
    SpaceID               string `json:"space_id,omitempty"`
    SpaceType             string `json:"space_type,omitempty"`
    SenderID              string `json:"sender_id,omitempty"`
    MessageID             string `json:"message_id,omitempty"`
    Mentioned             bool   `json:"mentioned,omitempty"`
    ReplyToMessageID      string `json:"reply_to_message_id,omitempty"`
    ReplyToSenderID       string `json:"reply_to_sender_id,omitempty"`
}
```

`EventScope` 的 channel identity 必须来自 normalized `InboundContext`，至少覆盖 `channel`、`entrypoint_id`、`account`、`channel_app_id`、`bot_id`、`chat_id`、`chat_type`、`topic_id`、`space_id`、`space_type`、`sender_id`、`message_id`、`reply_to_message_id` 和 `reply_to_sender_id`。否则后续 Activity / Run Inspector 如果改成按 `scope` 过滤，会把同一 channel 下不同 app、bot、thread 或 space 的事件混在一起。

`Correlation` 用来串 root run、child run 和工具调用：

```go
type EventCorrelation struct {
    TraceID       string `json:"trace_id,omitempty"`
    ParentRunID   string `json:"parent_run_id,omitempty"`
    ChildRunID    string `json:"child_run_id,omitempty"`
    ParentEventID string `json:"parent_event_id,omitempty"`
    ToolCallID    string `json:"tool_call_id,omitempty"`
}
```

`Visibility` 是默认展示层，不是权限：

```go
type EventVisibility struct {
    Conversation bool `json:"conversation,omitempty"`
    Activity     bool `json:"activity,omitempty"`
    Inspector    bool `json:"inspector,omitempty"`
    Audit        bool `json:"audit,omitempty"`
}
```

#### DelegationPolicy

```go
type DelegationPolicy struct {
    Enabled                 bool     `json:"enabled" yaml:"enabled"`
    Allow                   []string `json:"allow,omitempty" yaml:"allow,omitempty"`
    MaxDepth                int      `json:"max_depth,omitempty" yaml:"max_depth,omitempty"`
    MaxParallel             int      `json:"max_parallel,omitempty" yaml:"max_parallel,omitempty"`
    DefaultMaxDurationMS    int      `json:"default_max_duration_ms,omitempty" yaml:"default_max_duration_ms,omitempty"`
    MaxDurationMS           int      `json:"max_duration_ms,omitempty" yaml:"max_duration_ms,omitempty"`
    ExposeChildOutputToUser bool     `json:"expose_child_output_to_user,omitempty" yaml:"expose_child_output_to_user,omitempty"`
    ReturnTo                string   `json:"return_to,omitempty" yaml:"return_to,omitempty"`
    ChildSessionMode        string   `json:"child_session_mode,omitempty" yaml:"child_session_mode,omitempty"`
}
```

默认值：

```yaml
delegation:
  enabled: false
  allow: []
  max_depth: 1
  max_parallel: 1
  default_max_duration_ms: 30000
  max_duration_ms: 120000
  expose_child_output_to_user: false
  return_to: caller
  child_session_mode: ephemeral_worker
```

Duration guard 由 runtime 强制执行，不能依赖模型是否在 `DelegateAgentRequest` 里传 `max_duration_ms`。

```text
effective_max_duration_ms =
  if request.max_duration_ms > 0:
    min(request.max_duration_ms, delegation.max_duration_ms)
  else:
    delegation.default_max_duration_ms

effective_max_duration_ms must be > 0
effective_max_duration_ms must be <= delegation.max_duration_ms
```

所以 request 可以要求更短的 child timeout，但不能绕过或放大 policy 上限。缺省 request 也必须 bounded。

`DelegateExecutor` 必须用 bounded child context 包住整个 child execution：

```text
childCtx = context.WithTimeout(parentCtx, effective_max_duration_ms)
RunChildAgent(childCtx, ...)
  -> ADK model runner
  -> runtime-owned delegate/status tools
  -> workspace tools
```

timeout / cancel 的 contract：

```text
child context is passed to model and every tool execution
slow command.run / shell.run is killed through context cancellation
agent.delegate.timeout or agent.delegate.cancelled is terminal for that child run
no late agent.delegate.completed is emitted after timeout / cancelled
no agent.delegate.result_delivered is emitted after timeout / cancelled unless status remains timeout / cancelled
ephemeral_worker timeout / cancellation never appends child messages to long-term agent history
Run Inspector records the terminal reason and any partial artifacts that were already safely written
```

#### DelegateAgentRequest

```go
type DelegateAgentRequest struct {
    AgentID              string            `json:"agent_id"`
    Task                 string            `json:"task"`
    ContextRefs          []string          `json:"context_refs,omitempty"`
    ExpectedOutputSchema string            `json:"expected_output_schema,omitempty"`
    MaxDurationMS        int               `json:"max_duration_ms,omitempty"`
}
```

`DelegateAgentRequest` 是模型可控输入，只能表达目标 agent、任务、允许解析的 context refs、期望输出 schema 和更短的 timeout。它不能携带任意 `metadata`。

`delegate_agent` tool JSON schema 必须设置 `additionalProperties=false`。未知字段必须拒绝为 invalid request，并发 `agent.delegate.rejected`，不能被静默忽略后继续执行。

runtime-owned scope / correlation / policy identity 永远不能来自 delegate tool input，包括：

```text
entrypoint_id
channel
account
channel_app_id
bot_id
chat_id
chat_type
sender_id
message_id
reply_to_message_id
source
parent_run_id
child_run_id
tool_call_id
provenance fields
policy decisions
```

这些字段必须来自 normalized parent `InboundContext`、parent run state、runtime-allocated child run ID、caller profile、target profile 和 DelegationPolicy。Phase 1 不提供 delegate request labels；如果未来需要 labels，必须是 fixed allow-list，且不得和 scope / correlation / provenance / policy 字段重名。

#### DelegateAgentResult

```go
type DelegateAgentResult struct {
    AgentID        string   `json:"agent_id"`
    RunID          string   `json:"run_id,omitempty"`
    Status         string   `json:"status"`
    Summary        string   `json:"summary,omitempty"`
    EvidenceRefs   []string `json:"evidence_refs,omitempty"`
    Limitations    []string `json:"limitations,omitempty"`
    Confidence     string   `json:"confidence,omitempty"`
    FollowupNeeded bool     `json:"followup_needed,omitempty"`
    Error          string   `json:"error,omitempty"`
}
```

`RunChildAgent` 不能把当前 ADK final text 原样当作 `DelegateAgentResult`。当前底层 ADK path 返回的是字符串 final；Phase 1 必须增加结构化解析和校验层：

```text
RunChildAgent
  -> call child model / ADK runner
  -> collect raw_child_final_text
  -> write raw_child_final_text to Run Inspector artifact
  -> parse raw_child_final_text as JSON
  -> validate against expected_output_schema or delegate_result_v1
  -> canonicalize runtime-owned fields and evidence_refs
  -> build DelegateAgentResult
  -> return only DelegateAgentResult to caller
```

Child prompt / model config 必须明确要求 JSON-only output for `DelegateAgentResult`。如果 provider 支持 JSON schema / response schema，优先使用强 schema output；否则使用 prompt-level JSON contract 加 runtime validation。

解析或校验失败时：

```text
emit agent.delegate.failed with reason=result_parse_failed or result_schema_failed
transition child_run_completed -> result_rejected
return DelegateAgentResult{status: "failed", error: "invalid_child_result"}
do not return raw_child_final_text to caller
keep raw_child_final_text only as Run Inspector artifact
```

`DelegateAgentResult` 里的审计字段是 runtime-owned，不是 child model 自证字段：

```text
AgentID is set to the authorized target agent_id selected by DelegateExecutor.
RunID is set to the runtime-allocated child run ID.
Status is set from the delegation state machine terminal state.
Error is set by runtime on rejected / failed / timeout / cancelled paths.
```

child JSON 如果包含 `agent_id`、`run_id`、`status`、`error`，只能当作候选值校验；runtime 必须覆盖这些字段。候选值和 runtime-owned identity / terminal state 不一致时，转为 `result_schema_failed`，不把该 child output 交给 caller synthesis。

`EvidenceRefs` 也必须由 runtime 校验和规范化。允许集合只能来自：

```text
ContextPacket materialized refs that were explicitly included for this child run
child run artifacts produced by allowed tools and registered in the child run store
runtime-created result / summary artifacts for this child run
```

child 不能返回任意 `workspace://`、`tool://run_parent/...`、绝对路径、其他 run artifact、未 materialize 的 parent raw output，或伪造 provenance。任一 evidence ref 不在 allowed set 内时，runtime 必须发 `agent.delegate.failed` with reason=`result_schema_failed`，状态转 `child_run_completed -> result_rejected`，并只在 Run Inspector 保留 raw child text 和失败原因。

这保证 child output 的自由文本不会进入 caller synthesis，也不会因为解析失败被误当成可信结构化 evidence。

#### ContextPacket

```go
type ContextPacket struct {
    ID          string                `json:"id"`
    Mode        string                `json:"mode"` // delegate_worker
    CreatedAt   time.Time             `json:"created_at"`
    Caller      ContextActor          `json:"caller"`
    Target      ContextTarget         `json:"target"`
    Task        ContextTask           `json:"task"`
    Items       []ContextItem         `json:"items,omitempty"`
    Budget      ContextBudget         `json:"budget,omitempty"`
    Redactions  []ContextRedaction    `json:"redactions,omitempty"`
}
```

```go
type ContextActor struct {
    AgentID               string `json:"agent_id"`
    RunID                 string `json:"run_id,omitempty"`
    ConversationSessionID string `json:"conversation_session_id,omitempty"`
    AgentSessionID        string `json:"agent_session_id,omitempty"`
}

type ContextTarget struct {
    AgentID                string   `json:"agent_id"`
    ProfileVersion         string   `json:"profile_version,omitempty"`
    ProfileInstructionHash string   `json:"profile_instruction_hash,omitempty"`
    ProfileInstructionRef  string   `json:"profile_instruction_ref,omitempty"`
    AllowedTools           []string `json:"allowed_tools,omitempty"`
    AllowedToolsHash       string   `json:"allowed_tools_hash,omitempty"`
    RunID                  string   `json:"run_id,omitempty"`
    SessionMode            string   `json:"session_mode"` // ephemeral_worker by default
    DelegationDepth        int      `json:"delegation_depth"`
}

type ContextTask struct {
    UserIntent   string   `json:"user_intent,omitempty"`
    WorkerTask   string   `json:"worker_task"`
    OutputSchema string   `json:"output_schema,omitempty"`
    Constraints  []string `json:"constraints,omitempty"`
}
```

```go
type ContextItem struct {
    ID             string `json:"id"`
    Kind           string `json:"kind"` // user_message, recent_message, evidence_ref, tool_result, kb_doc, artifact_ref, profile_instruction_ref, tool_allow_list_ref
    Source         string `json:"source"`
    OwnerAgentID   string `json:"owner_agent_id,omitempty"`
    Visibility     string `json:"visibility"` // child_only, caller_visible, inspector_only
    ContentPreview string `json:"content_preview,omitempty"`
    ContentHash    string `json:"content_hash,omitempty"`
    IncludedChars  int    `json:"included_chars,omitempty"`
    Redacted       bool   `json:"redacted,omitempty"`
}

type ContextBudget struct {
    MaxItems              int `json:"max_items,omitempty"`
    MaxInlineChars        int `json:"max_inline_chars,omitempty"`
    MaxInlineCharsPerItem int `json:"max_inline_chars_per_item,omitempty"`
    RecentMessageLimit    int `json:"recent_message_limit,omitempty"`
}

type ContextRedaction struct {
    ItemID string `json:"item_id,omitempty"`
    Reason string `json:"reason"`
}
```

`ContextTarget` 是审计 target profile 边界的主位置。`profile_instruction_hash` 应基于 child 实际使用的 effective instruction text 计算，包括 profile body 和 runtime 注入的必要约束；`allowed_tools` 必须是 target profile 在本次 child run 中实际可用的工具清单，不是 registry 全量工具。Run Inspector 可以用 `profile_instruction_ref`、`profile_instruction_hash`、`allowed_tools` 和 `allowed_tools_hash` 证明 child 拿到的是哪个 profile instruction 和 tool allow-list。

`ContextItem.kind=profile_instruction_ref` / `tool_allow_list_ref` 只用于 Inspector provenance，不要求把完整 instructions 或工具定义 inline 到 ContextPacket。真正的 child prompt / ADK agent instruction 仍由 runtime 从 target profile 生成。

### Delegation State Machine

每次 `delegate_agent` 调用必须落一个状态机，避免失败路径靠日志猜。

```text
requested
  -> rejected_policy
  -> allowed
  -> context_building
  -> context_built
  -> child_run_started
  -> child_run_completed
  -> result_delivered
```

异常路径：

```text
requested -> rejected_missing_agent
requested -> rejected_depth_limit
requested -> rejected_parallel_limit
context_building -> context_failed
child_run_started -> child_run_failed
child_run_started -> child_run_timeout
child_run_started -> child_run_cancelled
child_run_completed -> result_rejected
```

对应事件：

```text
agent.delegate.requested
agent.delegate.rejected
agent.delegate.allowed
context.packet.started
context.packet.completed
context.packet.truncated
context.packet.failed
agent.delegate.started
agent.delegate.completed
agent.delegate.failed
agent.delegate.timeout
agent.delegate.cancelled
agent.delegate.result_delivered
```

### Context Budget And Truncation

Phase 1 的 ContextPacket 裁剪规则必须是确定性的。

必带内容：

```text
1. caller 给 worker 的 task
2. 当前用户消息
3. target agent profile instructions, represented by target.profile_instruction_hash/ref
4. target agent tool allow-list, represented by target.allowed_tools/hash
```

必带不代表全部 inline。Profile instructions 可以通过 hash/ref 审计，tool allow-list 必须记录本次 child run 实际可用的工具名列表和 hash；Run Inspector 应能据此复核 child 的 prompt/profile 边界。

优先保留：

```text
1. caller 明确指定的 context_refs
2. 与 context_refs 关联的 evidence preview
3. 当前 turn 的 tool result preview
4. bounded recent messages
5. conversation summary
6. target profile optional context
```

默认预算：

```yaml
context_budget:
  recent_message_limit: 6
  max_inline_chars_per_item: 2000
  max_inline_chars: 12000
  raw_output_policy: preview_only
```

超预算时按这个顺序裁剪：

```text
1. optional context
2. old recent messages
3. long evidence previews
4. tool result previews
5. conversation summary
```

不能裁剪到缺失：

```text
worker_task
current_user_message
target profile instructions
policy constraints
```

默认禁止 inline：

```text
secrets
raw channel payload
raw stdout / stderr full content
unrelated agent history
other agent internal output
```

如果 worker 需要更多证据，只能通过受控 ref 和自己的 allowed tools 继续读取。

### Event To UI Mapping

| Event kind | Conversation | Activity | Run Inspector | Audit |
| --- | --- | --- | --- | --- |
| `assistant.status` | yes | yes | yes | no |
| `run.started` | no | yes | yes | yes |
| `model.policy_resolved` | no | no | yes | yes |
| `agent.delegate.requested` | no | yes | yes | yes |
| `agent.delegate.rejected` | maybe | yes | yes | yes |
| `agent.delegate.allowed` | no | yes | yes | yes |
| `context.packet.started` | no | no | yes | yes |
| `context.item.included` | no | no | yes | no |
| `context.item.redacted` | no | no | yes | yes |
| `context.packet.completed` | no | yes | yes | yes |
| `context.packet.truncated` | no | yes | yes | yes |
| `context.packet.failed` | maybe | yes | yes | yes |
| `agent.delegate.started` | no | yes | yes | yes |
| `agent.delegate.completed` | no | yes | yes | yes |
| `agent.delegate.failed` | maybe | yes | yes | yes |
| `agent.delegate.timeout` | maybe | yes | yes | yes |
| `agent.delegate.cancelled` | maybe | yes | yes | yes |
| `agent.delegate.result_delivered` | no | yes | yes | yes |
| `tool.started` | no | yes | yes | yes |
| `tool.completed` | no | yes | yes | yes |
| `tool.failed` | maybe | yes | yes | yes |
| `capability_gap` | yes | yes | yes | yes |
| `assistant.final` | yes | no | yes | no |

`maybe` 表示由 `main_agent` 生成用户可读解释，而不是直接把 raw event message 投递到 Conversation。

Compatibility: current `tool.finished` is a legacy alias of `tool.completed`. Current `adk.event` with `final=true` is a fallback signal for `assistant.final`; Phase 1 should emit `assistant.final` explicitly while preserving `adk.event` for Inspector diagnostics.

### Built-in Safety Checks

Phase 1 没有 Hook，所以 runtime 必须内置这些检查：

```text
requested agent exists in AgentRegistry
caller profile delegation.enabled is true
target agent_id is in caller profile delegation.allow
target agent is enabled, meaning loaded+valid in Phase 1
target agent has valid profile and model policy
requested_child_depth = parent_delegation_depth + 1
requested_child_depth <= max_depth
active_child_count_before_start < max_parallel
effective_max_duration_ms is resolved from request or policy default
effective_max_duration_ms > 0
effective_max_duration_ms <= delegation.max_duration_ms
child_session_mode defaults to ephemeral_worker
ContextPacket redaction completed before child run
raw output is not inlined into child context by default
child result is structured before returning to caller
child raw response is not exposed to user unless explicitly allowed
```

如果任一检查失败：

```text
emit agent.delegate.rejected
return DelegateAgentResult{status: "rejected", error: "..."}
do not start child run
```

### Acceptance Cases

Phase 1 至少要通过这些 smoke cases。

1. **普通 tool-only turn 不触发 delegation**

```text
Given current main_agent resolves to xira-assistant
And xira-assistant has read_file / list_dir / command.run
When user asks to inspect a workspace file or run a scoped command
Then no agent.delegate.* event is emitted
And assistant.final is produced
```

2. **允许 delegate 成功**

```text
Given xira-assistant delegation.allow includes research-assistant
And research-assistant profile is loaded and valid
When xira-assistant calls delegate_agent(research-assistant)
Then runtime emits agent.delegate.requested
And emits agent.delegate.allowed
And emits context.packet.started
And emits context.packet.completed
And emits agent.delegate.started
And emits agent.delegate.completed
And emits agent.delegate.result_delivered
And returns structured DelegateAgentResult to xira-assistant
```

If ContextPacket is truncated in the successful path, `context.packet.truncated` is emitted before `agent.delegate.started`, and `context.packet.completed.payload.truncated=true`.

2a. **channel WebSocket 能看到 child run 进度**

```text
Given a channel-originated turn is streamed through /api/v1/channels/xiragarden/events
And xira-assistant starts child run run_child
When context and agent.delegate events are emitted for run_child
Then each event carries legacy payload["channel"] and payload["entrypoint_id"]
And the WebSocket receives the first child-run event
And subsequent child-run events are not dropped
```

If the implementation migrates the channel filter first, this case can pass by registering `child_run_id` into the channel run set before the first scope-only child event. Without that registration, scope-only child events are not acceptable in Phase 1.

2b. **delegate request 不能伪造 scope / metadata**

```text
Given xira-assistant calls delegate_agent with extra metadata or scope fields
And the extra fields include channel, entrypoint_id, sender_id or parent_run_id
When DelegateExecutor validates the request
Then runtime rejects the call as invalid_request
And emits agent.delegate.rejected
And no child run is started
And no runtime-owned scope, correlation or policy field is copied from the tool input
```

3. **ContextPacket 记录 target profile/tool 边界**

```text
Given xira-assistant calls delegate_agent(research-assistant)
When ContextPacket is built
Then target.profile_instruction_hash is non-empty
And target.profile_instruction_ref points to research-assistant instructions
And target.allowed_tools matches research-assistant tools for this child run
And target.allowed_tools_hash is recorded in Run Inspector
```

4. **未授权 delegate 被拒**

```text
Given xira-assistant delegation.allow only includes research-assistant
And yangsheng-yihao profile is loaded and valid
When xira-assistant calls delegate_agent(yangsheng-yihao)
Then runtime emits agent.delegate.rejected
And no child run is started
And final answer explains the limitation or asks for approval
```

5. **depth limit 生效**

```text
Given max_depth = 1
And parent_delegation_depth = 1 for an active child worker
When the child worker tries to call delegate_agent again
Then requested_child_depth = 2
And runtime rejects with rejected_depth_limit
And records the rejection in Run Inspector
```

6. **parallel limit 生效**

```text
Given max_parallel = 1
And active_child_count_before_start = 1 for the same parent run
When xira-assistant tries to start another delegate_agent call
Then runtime rejects with rejected_parallel_limit
And no second child run is started
```

7. **ContextPacket 不泄漏 raw output**

```text
Given parent run has command.run raw_output_path
When ContextPacket is built
Then child receives preview/ref only
And raw stdout/stderr full content is not inline
And any readable artifact is materialized as child-local context artifact
And provenance records source_run_id, source_tool_call_id and source_ref
And child cannot pass the parent raw_output_path directly to tool_output.read
```

8. **ContextPacket 裁剪可见**

```text
Given ContextPacket exceeds max_inline_chars or max_items
When ContextPacket is built
Then runtime emits context.packet.truncated
And context.packet.completed payload includes truncated=true
And Activity shows a readable truncation summary
And Run Inspector shows truncated item refs / reasons
```

9. **child result 解析失败**

```text
Given child final text is not valid JSON for delegate_result_v1
When RunChildAgent validates child result
Then runtime emits agent.delegate.failed with reason=result_parse_failed
And state moves child_run_completed -> result_rejected
And caller receives DelegateAgentResult{status: "failed", error: "invalid_child_result"}
And raw child text is stored only in Run Inspector artifact
```

9a. **child result 不能伪造 identity / evidence refs**

```text
Given child final JSON is syntactically valid
And it claims agent_id, run_id or status different from the runtime child run
Or it returns evidence_refs outside the ContextPacket materialized refs and child-run registered artifacts
When RunChildAgent canonicalizes the child result
Then runtime emits agent.delegate.failed with reason=result_schema_failed
And state moves child_run_completed -> result_rejected
And caller receives DelegateAgentResult{status: "failed", error: "invalid_child_result"}
And raw child text and rejected refs are stored only in Run Inspector
```

10. **child timeout 缺省也 bounded**

```text
Given delegation.default_max_duration_ms = 30000
And delegate request omits max_duration_ms
When child run exceeds 30000ms
Then runtime emits agent.delegate.timeout
And returns DelegateAgentResult{status: "timeout"}
And main_agent can continue with a bounded explanation
And any slow child tool is killed through child context cancellation
And no late agent.delegate.completed or success result is emitted for that child run
And ephemeral_worker does not append child messages to long-term agent history
```

11. **child timeout 不能超过 policy 上限**

```text
Given delegation.max_duration_ms = 120000
And delegate request has max_duration_ms = 300000
When runtime resolves effective_max_duration_ms
Then effective_max_duration_ms = 120000
And child run is still bounded by policy
```

12. **child timeout request 可以缩短上限**

```text
Given delegation.max_duration_ms = 120000
And delegate request has max_duration_ms = 10000
When child run exceeds the limit
Then runtime emits agent.delegate.timeout
And returns DelegateAgentResult{status: "timeout"}
And main_agent can continue with a bounded explanation
And no late agent.delegate.completed is emitted for that child run
```

13. **assistant.status 不污染 final / session**

```text
Given xira-assistant emits assistant.status before doing work
When ADK later produces final response text
Then assistant.status is visible as assistant.status event with payload.producer
And assistant.final contains only the final answer text
And latestText fallback never uses assistant.status as the final answer
And durable session history does not store assistant.status as an assistant message
```

14. **invalid profile fail-fast**

```text
Given one workspace agent profile is invalid
When AgentRegistry is built in Phase 1
Then loader returns an error
And runtime does not publish a partial enabled-agent registry
And delegate_agent cannot target the invalid profile as disabled-but-present
```

15. **capability gap**

```text
Given no allowed agent can perform the requested capability
When main_agent cannot safely answer
Then runtime records capability_gap
And Conversation receives a user-readable limitation
And no fake specialist result is generated
```

16. **Activity / Run Inspector separation**

```text
Given a delegated run completes
Then Conversation shows status/final only
And Activity shows readable child-agent progress
And Run Inspector shows ContextPacket metadata, child run, tool spans, audit events
```

## 参考模式

- OpenAI Agents SDK 区分 handoff 与 agent-as-tool。v1 更适合 agent-as-tool，handoff 后置。
- Anthropic 的 agent 设计建议是先用简单组合，只有任务需要动态拆解时才采用 orchestrator-workers。
- Claude Code 和 Devin 都把 sub-agent 作为独立上下文 worker，并限制工具、权限和嵌套。
- Google ADK 和 Magentic-One 都偏 coordinator / specialist 模式，而不是自由 agent 网络。

参考链接：

- OpenAI Agents SDK Handoffs: https://openai.github.io/openai-agents-js/guides/handoffs/
- OpenAI Agents SDK Agents as tools: https://openai.github.io/openai-agents-js/guides/tools/
- OpenAI Agents SDK Streaming: https://openai.github.io/openai-agents-js/guides/streaming/
- Anthropic Building effective agents: https://www.anthropic.com/engineering/building-effective-agents
- Claude Code Subagents: https://code.claude.com/docs/en/sub-agents
- Devin CLI Subagents: https://cli.devin.ai/docs/subagents
- Google ADK Multi-agent systems: https://google.github.io/adk-docs/agents/multi-agents/
- Microsoft Magentic-One: https://www.microsoft.com/en-us/research/publication/magentic-one-a-generalist-multi-agent-system-for-solving-complex-tasks/

## 架构决策

采用 `main_agent owns conversation + controlled delegation + progressive runtime events`。

理由：

1. 用户体验自然，主对话保持连续。
2. sub-agent 可以复用专业上下文，但不会污染用户对话。
3. registry + profile + delegation policy 能控制权限、审计、成本和失败边界。
4. Activity / Run Inspector 分层能同时满足可读性和可追责。
5. v1 能快速落地，后续可以平滑扩展到 handoff、flow pack 和完整 policy engine。
