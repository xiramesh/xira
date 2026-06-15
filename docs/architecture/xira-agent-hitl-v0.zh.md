# Xira Agent Runtime Boundary v0 设计草案

> 状态：草案，供人和 AI review。
> 分支：`feature/agent-hitl-v0`
> 目标：先定义 agent-first 场景下的运行时边界能力：HITL、agent delegation、suspend/resume，再让 Flow Run Kernel 复用同一套能力。

## 摘要

Agent Runtime Boundary v0 定义三件基础能力：

```text
HumanRequest:
  agent run 遇到人类介入点时，runtime 把它变成可展示、可回复、可审计、可恢复的状态。

AgentDelegation:
  parent agent 通过 delegate_agent 启动 child agent run，runtime 定义 child context、child result、parent wait、parallel join。

Suspend/Resume:
  tool call / child run / HumanRequest 都可以把当前 run 挂起，后续通过持久化状态恢复，而不是挂住 goroutine。
```

HITL 是其中一部分，不是 Flow 独占能力。它解决的问题是：

```text
agent 运行到一半，遇到不能或不应该自己越过的边界，
runtime 需要把这个中断点变成可展示、可回复、可审计、可恢复的状态。
```

Agent-only 场景也必须支持 HITL。否则用户直接和 agent 操作时，Xira 只能在两个坏选择之间摇摆：

- 太弱：高风险动作只能拒绝，无法继续。
- 太危险：agent 可以直接执行写文件、shell、push、merge、客户系统写入等动作。

v0 设计结论：

```text
HumanRequest = 运行中临时生成的人类介入请求和存档结构。
AgentDelegation = 父 agent 通过 delegate_agent 启动的子 agent run 边界。
DelegationJoinState = parent 等待一个或多个 child 结果的持久化 join 状态。

Agent 可以主动创建 HumanRequest。
Runtime 必须在 ToolPolicy.RequireConfirmation 为 true 时强制创建 HumanRequest。
Parent agent 可以用宽松输入调用 child agent，但 child context / child result / waiting / join / resume 语义由 runtime 定义。
child agent 触发 HumanRequest 时，parent 必须进入 `waiting_human` 并记录 `blocked_by=child_human_request`；这不是附属能力，而是 AgentDelegation 作为真实 child run 的必要语义。
Flow 未来复用同一套 HumanRequest，只是 scope 指向 flow_run + step。
每个 HumanRequest 必须归属于一个 canonical workspace；workspace 是状态/权限边界，不是 scope type。
```

这意味着 v0 主线不只是 HITL file store 和 approval replay。v0 同时承认一个更重的 runtime 事实：

```text
delegate_agent 不是普通同步函数调用。
delegate_agent 是 suspendable runtime control tool。
child waiting_human 会把 parent delegate tool call 变成 continuation。
DelegationJoinState 是恢复 parent 的权威 continuation state，而不只是审计记录。
```

因此，child `waiting_human` 进入 v0 是刻意选择：如果 child 是真实 agent run，它的 HumanRequest、tool calls、audit、resume 状态就不能从 parent / Flow 视角消失。

review 后修正的核心边界：

```text
协作型 HITL：
  agent 主动 human.request。
  HumanResponse 由 API / CLI / transport-structured response 写入。
  解决澄清、选择、低风险确认。
  不声明自己提供 runtime 强制安全边界。

强制型 HITL：
  runtime 消费 ToolPolicy.RequireConfirmation。
  执行前保存 tool input 快照，创建 approval HumanRequest，并短路原 tool。
  用户 approve 后 runtime 按快照重放，保证“批准什么就执行什么”。
```

因此，v0 不做通用命令风险分类器，但要接通已有的声明式 `RequireConfirmation` gate。否则 HITL 只是 agent 自觉调用 `human.request` 的协作协议，不能防止 agent 忘问或被 prompt injection 绕过。

## 非目标

- 不做完整 Flow Run Kernel。
- 不做复杂 UI。
- 不让 HTTP request 长时间阻塞等待用户。
- 不恢复原 goroutine；恢复必须通过持久化 continuation state 和新的 runtime turn。
- 不把 HITL 设计成只服务审批；它也服务澄清、选择和风险门禁。
- 不做 fire-and-forget delegation。
- 不把 parent agent 的完整 session history、隐藏推理或全部 tool outputs 默认传给 child agent。
- 不让 child agent 直接继承 parent 的全部 tools、secrets 或权限。
- 不做 Task/Todo 工具；Task/Todo 是未来的 planning state，不是 delegation runtime contract 的前置条件。

## 核心概念

### AgentDelegation

AgentDelegation 是父 agent 通过 `delegate_agent` 启动的子 agent run。

它不是 workflow step，也不是要求父 agent 填一个强 schema 的任务对象。父 agent 的调用保持宽松：

```json
{
  "agent_id": "reviewer",
  "task": "帮我 review 这次改动，重点看 HITL replay 状态机。",
  "context_refs": [
    "tool://parent_run/call_001/output"
  ]
}
```

runtime 必须在这个宽松调用外层补齐四个确定语义：

```text
1. child agent 看到什么 context
2. child agent 回传什么
3. parent agent 是否等待 child
4. parent agent 能不能并行启动多个 child
```

这四个语义是 Agent HITL 和未来 Flow 的共同前置。否则 child run 里发生的工具调用、HITL、文件改动和审计都会从 parent run 视角消失。

关键判断：

```text
如果 delegate_agent 启动的是 child run，
那么 child waiting_human 就是 child run 的合法状态，
runtime 不能把它压扁成 failed / timeout / empty FinalResponse。
```

所以 `delegate_agent` 不能长期停留在“同步调用 child、等待 FinalResponse、校验 JSON”的模型。v0 必须把它升级成可挂起 runtime control tool：

- child completed / failed / timeout 时，materialize 成 parent 可见的 `delegate_agent` tool output。
- child waiting_human 时，parent delegate tool call 进入 suspended 状态，parent run 返回 `waiting_human`。
- child HumanRequest resolve 后，runtime 先恢复 child，再 materialize child output，再把该 output 注入 parent tool history，最后恢复 parent。

这也是 AgentDelegation 和普通工具调用的分界：普通工具只需要返回 output；`delegate_agent` 需要保存 parent/child 两侧 continuation。

### HumanRequest

HumanRequest 不是预先规划的步骤。它是运行时中断点的持久化信封。

```text
agent 正在执行
  -> 需要人补充信息、做选择或批准风险动作
  -> runtime 创建 HumanRequest
  -> agent run 返回 waiting_human
  -> 用户提交 HumanResponse
  -> runtime 保存 HumanResponse
  -> 后续 agent run / resume turn 读取这个结果继续
```

HumanRequest 的核心职责：

- 告诉用户现在需要决定什么。
- 记录它属于哪个 agent run / session。
- 保存上下文、选项、状态和审计信息。
- 接收用户 HumanResponse。
- 让后续 resume 能找到人类决定。

### HumanResponse

HumanResponse 是用户对 HumanRequest 的回复。

HumanResponse 在 v0 来自结构化入口：

- CLI / API 直接提交 `approve`、`deny`、`answer`、`choose`。
- channel/router 解析出的结构化 HumanResponse，且必须带 transport 层身份和 entrypoint 信任配置。

不同结构化入口的信任级别不同：

| 来源 | 信任级别 | 可用于强制型 approval |
| --- | --- | --- |
| CLI / API / UI button | 高，用户身份由 transport 层确认 | 可以 |
| channel 结构化 shortcut，且由 channel/router 解析成 HumanResponse | 中，取决于 channel 认证 | v0 默认不可以，需显式 per-channel 开启 |

`HumanResponse.signal` 常见值：

- `approve`
- `deny`
- `answer`
- `choose`
- `cancel`

HumanResponse 不直接执行动作。它只是改变 HumanRequest 状态，并为后续 agent run / tool retry / snapshot replay 提供依据。

普通自然语言消息在 v0 不通过模型可见的 `human.respond` 落库。它先作为普通 user turn 进入 session；如果要 resolve HumanRequest，必须由 API / CLI / channel router 转成结构化 HumanResponse。这样可以避免模型自己调用 response 工具来批准或取消请求。

## HumanRequest 类型

v0 支持三类：

| kind | 说明 | 例子 |
| --- | --- | --- |
| `approval` | 是否允许某个风险动作 | 是否允许 `git push` |
| `clarification` | 信息不足，请用户补充 | 你要修哪个 repo |
| `choice` | 多个方案，需要用户选择 | 最小修复还是重构 |

后续可以增加：

- `handoff`
- `credential_required`
- `policy_exception`
- `risk_acceptance`

## 数据结构草案

```yaml
schema_version: xira.human_request.v0
id: hr_20260613_001
workspace: /Users/yinwm/work/flowdeck
kind: approval
status: pending
request_source: runtime_tool_gate
trust_level: enforced

scope:
  type: agent_run
  id: 20260613-223000-xira-assistant
  session_id: cli-default:user-local
  entrypoint_id: cli-local
  channel: cli

requester:
  agent_id: xira-assistant
  run_id: 20260613-223000-xira-assistant
  tool_call_id: call_001

question: 是否允许执行 git push?
reason: 用户要求发布当前分支；该动作会写入远端仓库。

context:
  action_type: tool_call
  tool: command.run
  input:
    program: git
    args:
      - push
      - -u
      - origin
      - HEAD
    cwd: /Users/yinwm/work/flowdeck
  expected_effect:
    reads:
      - local git repository
    writes:
      - remote git branch
    network: true
  risk_level: high

action_snapshot:
  type: tool_call
  tool: command.run
  tool_call_id: call_001
  input:
    program: git
    args:
      - push
      - -u
      - origin
      - HEAD
    cwd: /Users/yinwm/work/flowdeck
  env_snapshot:
    workspace_root: /Users/yinwm/work/flowdeck
    cwd: /Users/yinwm/work/flowdeck
    git_head: abc1234
    captured_at: 2026-06-13T22:30:00+08:00
  env_hash: sha256:...
  replay_ttl: 15m
  replay_policy: runtime_snapshot_replay
  replay_status: pending
  replay_attempt_id:
  replay_started_at:
  replay_finished_at:

options:
  - id: approve
    label: 允许一次
  - id: deny
    label: 拒绝

created_at: 2026-06-13T22:30:00+08:00
resolved_at:
resolution:
  signal:
  actor:
  message:
```

最小 Go 类型可以比 YAML 更小，但要保留这些概念面：

- identity
- workspace
- kind/status
- request_source/trust_level
- scope
- requester
- question/reason
- context
- action_snapshot
- options
- timestamps
- resolution

`request_source` 建议先支持：

| request_source | 说明 | 执行语义 |
| --- | --- | --- |
| `agent_request` | agent 主动调用 `human.request` | 不携带强执行保证，resolve 后由 agent 继续 |
| `runtime_tool_gate` | runtime 因 `ToolPolicy.RequireConfirmation` 拦截 tool call | 必须携带 `action_snapshot`，approve 后按快照重放 |

`approval` 只有在 `request_source=runtime_tool_gate` 且有 `action_snapshot` 时，才表示“有一个被 runtime 挂起的确定动作”。`request_source=agent_request` 的 approval 更接近“意图确认”，不能承诺批准后执行的动作一定等于用户当时理解的动作。

### HumanResponse 数据结构草案

```yaml
schema_version: xira.human_response.v0
id: hres_20260613_001
human_request_id: hr_20260613_001
signal: approve
actor:
  type: user
  id: user-local
resolver:
  type: cli
  id: xira-human-command
source:
  type: api_request
idempotency_key: msg_20260613_001
created_at: 2026-06-13T22:35:00+08:00
```

结构化 API / CLI 入口直接记录 `actor` 和 `resolver`。channel/router 结构化入口还要记录 source message id / text preview，便于审计用户原文和 router 解析结果；v0 不记录 agent interpreter，因为模型不负责 resolve HumanResponse。

## Scope 设计

HITL 是 runtime 基础能力，所以 scope 必须能覆盖 agent-only 和未来 flow。

但 workspace 不是 scope。workspace 是状态边界、权限边界和文件存储边界；scope 是当前 HumanRequest 正在中断或恢复的运行对象。

因此 HumanRequest 必须有顶层 `workspace`：

```yaml
workspace: /Users/yinwm/work/flowdeck
scope:
  type: agent_run
  id: 20260613-223000-xira-assistant
```

不要把 workspace 写成 scope type：

```text
agent_workspace
flow_workspace
```

原因是这会把“运行中断点”和“资源/权限归属”混在一个字段里。一个 workspace 里可以同时有 agent-only run、agent session、flow run、flow step，它们都应该共享同一个 canonical `workspace`，但 scope 指向不同的运行对象。

### workspace v0 载体

v0 先按“一个 runtime service 实例对应一个 workspace”实现。

`workspace` 由 runtime 配置解析，不从 agent 输出里取，也不让普通 `TurnRequest` 任意指定：

```yaml
workspace: /Users/yinwm/work/flowdeck
state_root: .xira/state
```

v0 约定：

- `workspace` 是 canonical absolute path，和现有 `WorkspaceRoot` 对齐。
- HumanRequest 创建时从 `Service` / runtime context 读取 canonical `workspace`。
- API/CLI 如果携带 `workspace`，只能用于校验必须等于当前 runtime workspace；不能用它切换 workspace。
- 未来多 workspace 时，由 transport / control plane 先选择 workspace，再进入 runtime；`TurnRequest.workspace` 只作为已认证 workspace 的回显或校验字段，不作为 agent 可控输入。

不使用 `workspace_id` 或 `local` 这类弱身份。`local` 在每台机器、每个 runtime 里都会重复，不能作为稳定边界。

file store v0 需要目录分片时，使用 runtime 内部派生的 `workspace_key`，不要把它暴露成业务协议：

```text
workspace_key = "ws_" + first16(hex(sha256(canonical_workspace)))
```

按 `workspace_key` 分目录，避免后续多 workspace 或共享 state root 时混写：

```text
.xira/state/workspaces/<workspace_key>/human-requests/<human_request_id>.yaml
.xira/state/workspaces/<workspace_key>/human-responses/<human_response_id>.yaml
.xira/state/workspaces/<workspace_key>/replay-results/<human_request_id>.yaml
```

v0：

```text
agent_run
agent_session
```

未来：

```text
flow_run
flow_step
```

建议 scope：

```yaml
scope:
  type: agent_run
  id: 20260613-223000-xira-assistant
  session_id: ...
  entrypoint_id: ...
  channel: ...
```

区别：

- `agent_run`：请求绑定某一次 run，适合 tool approval。
- `agent_session`：请求绑定会话，适合澄清信息和跨 turn answer。
- `flow_run`：未来绑定整个 flow。
- `flow_step`：未来绑定 flow 的某个 step。

如果未来需要“workspace 级批准”，例如“在当前 workspace 内允许某 agent 本 session 使用某个 tool”，也不要新增 `agent_workspace` scope。它应该表现为某次 HumanResponse 的效果：

```yaml
workspace: /Users/yinwm/work/flowdeck
scope:
  type: agent_session
  id: cli-default:user-local

response_effect:
  type: workspace_grant
  subject:
    type: agent
    id: dev-implementer
  permission:
    tool: command.run
  duration: session
```

也就是说，workspace 级能力属于 policy / grant / response_effect，不属于 HumanRequest 的 primary scope。

## Agent Delegation v0 行为

Xira 的 agent 调用保持松散。`delegate_agent` 不要求父 agent 提交完整 workflow schema，也不要求父 agent 精确描述所有上下文边界。

但 runtime 必须把每次 delegation 变成可审计、可挂起、可恢复的 child run 边界。这里的“等待 child”是**逻辑等待**，不是阻塞 goroutine 等人回复。

```text
parent delegate_agent call
  -> create child run
  -> create DelegationJoinState
  -> child completed/failed/timeout 时 materialize delegate output
  -> child waiting_human 时 suspend parent delegate tool call
  -> child resume 后再 materialize delegate output
  -> parent resume with materialized child outputs
```

### 为什么 child waiting_human 必须吃进 v0

child `waiting_human` 不是 delegation 的边缘异常，而是“child 是真实 run”的直接结果。

如果 runtime 不吃进来，会出现三个语义破口：

1. **状态不诚实**：child 实际在等人，但 parent 只能看到 failed / timeout / invalid result。
2. **审计断裂**：child HumanRequest、HumanResponse、tool call 和最终 delegate result 无法从 parent run 串起来。
3. **Flow 复用失败**：Flow Kernel 未来只能看 parent agent step 的状态；如果 child waiting 不能冒泡到 parent，Flow 就必须理解 delegation 内部细节。

因此，v0 明确选择：

```text
child waiting_human
  -> parent delegate tool call suspended
  -> parent run waiting_human
  -> parent response metadata blocked_by child_human_request
```

这个选择同时带来实现代价：`delegate_agent` 必须从“同步等待 child FinalResponse”的调用模型，升级为“可持久化 continuation 的 runtime control tool”。现有同步执行路径只能覆盖 child completed / failed / timeout，不能独立承载 child waiting_human。

### 1. Child agent 看到什么 context

child agent 默认不继承 parent agent 的完整上下文。runtime 构造 child input context：

```text
child context =
  parent delegate task
  + canonical workspace
  + parent lineage
  + selected context refs
  + runtime context packet summary
  + target agent profile / tool boundary
```

v0 默认传给 child：

- `task`：父 agent 调用 `delegate_agent` 时写的自然语言任务。
- `workspace`：canonical workspace root。
- `parent_lineage`：`parent_run_id`、`parent_tool_call_id`、`parent_agent_id`、`delegation_depth`。
- 当前用户消息：当父 agent 没有显式 `context_refs` 时，runtime 默认包含当前 turn 的用户消息摘要。
- `context_refs`：父 agent 显式传入的 artifact / tool output 引用。
- `context_packet`：runtime 生成的结构化包，记录哪些上下文被包含、截断、拒绝或重写。
- child 的 profile/tool 边界：child 能用什么 tools、profile version / instruction hash。

v0 默认不传：

- parent 完整 session history。
- parent 隐藏推理。
- parent 所有 tool outputs。
- parent 的全部 tools、secrets 和权限。
- 未显式引用的大文件内容。

`context_refs` 是引用，不是任意路径逃逸。v0 只允许 runtime 能解析和授权的 ref，例如：

```text
conversation://current-turn/user-message
tool://<parent_run_id>/<tool_call_id>/output
artifact://<parent_run_id>/artifacts/tool-outputs/<tool_call_id>.json
```

runtime 对每个 ref 做 materialize：

```text
parent ref
  -> validate belongs to parent run / allowed artifact area
  -> copy or summarize into child run artifacts/context/
  -> record context item with source_run_id/source_tool_call_id/content_hash/truncation
  -> expose context://<child_run_id>/context/<item_id> to child
```

child 看到的是任务包，不是 parent transcript：

```text
You are running as a Xira delegate worker.

Task:
  帮我 review 这次改动，重点看 HITL replay 状态机。

ContextPacket:
  caller: parent agent/run/tool call
  target: child agent/profile/tool boundary
  items: selected refs and previews
  output_schema: delegate_result_v1
```

### 2. Child agent 回传什么

child 不把完整 transcript 塞回 parent。child run 完成后，runtime 把结果 materialize 成 `delegate_agent` tool output。

v0 child 模型输出只需要返回结构化结果的业务部分：

```json
{
  "summary": "发现 replay_status 缺 running 中间态，会导致并发重放。",
  "evidence_refs": [
    "context://child_run/context/ctxitem_tool_output_call_001"
  ],
  "limitations": [
    "未运行完整端到端测试"
  ],
  "confidence": "high",
  "followup_needed": false
}
```

runtime 负责补齐和校验 runtime-owned 字段，交给 parent 的 tool output 是：

```json
{
  "agent_id": "reviewer",
  "run_id": "20260614-101010-reviewer-ab12",
  "status": "completed",
  "summary": "发现 replay_status 缺 running 中间态，会导致并发重放。",
  "evidence_refs": [
    "context://child_run/context/ctxitem_tool_output_call_001",
    "artifact://child_run/artifacts/context/context_packet.json"
  ],
  "limitations": [
    "未运行完整端到端测试"
  ],
  "confidence": "high",
  "followup_needed": false
}
```

规则：

- parent 默认只看到 result envelope，不看到 child 完整 transcript。
- child 的完整 run log、tool calls、audit、artifacts 留在 child run。
- `evidence_refs` 只能引用 runtime 允许的 child artifacts / context refs，不能伪造 parent run 或其他 run 的 ref。
- child 不能伪造 `agent_id`、`run_id`、`status`、`error`、policy、scope、correlation 等 runtime-owned 字段。
- 如果 child 输出不符合 schema，runtime 返回 `status=failed` 的 delegate tool output，并保存 raw child result 供审计。

信任边界也要明确：

- runtime 只校验 schema、runtime-owned fields、evidence ref 归属，不校验 `summary` 的事实真伪。
- parent 在 v0 会把 child `summary` 当作另一个 agent 的报告使用；如果 child 被污染或幻觉，parent 会继承这个风险。
- `confidence` 和 `followup_needed` 只是提示字段，不是 runtime policy。

未来可以扩展 `delegate_result_v2`，加入 `artifact_refs`、`changed_files`、`human_requests` 等字段；v0 先以 `summary + evidence_refs + limitations + confidence + followup_needed` 为最小闭环。

### 3. Parent agent 是否等待 child

v0 的 `delegate_agent` 是可挂起 tool call：parent 默认逻辑等待 child 的 tool output，但 runtime 不为了等人而挂住 goroutine。

```text
parent model call
  -> emits delegate_agent tool call
  -> runtime starts child run
  -> child completed/failed/timeout
       materialize delegate_agent tool output
       parent model continues
  -> child waiting_human
       persist suspended delegate tool call
       parent run exits with waiting_human
```

child 状态到 parent 的映射：

| child 状态 | parent 看到什么 | parent run 状态 |
| --- | --- | --- |
| completed | `delegate_agent` output status=`completed` | parent 继续 |
| failed | `delegate_agent` output status=`failed` | parent 继续，让 parent 决定补救 |
| timeout | `delegate_agent` output status=`timeout` | parent 继续，让 parent 决定补救 |
| waiting_human | parent delegate tool call 挂起，记录 `blocked_by` | parent 进入 `waiting_human` |

如果 child 触发 HumanRequest：

```text
child run -> waiting_human
parent delegate_agent tool call -> suspended_waiting_child_human
parent run -> waiting_human
blocked_by:
  type: child_human_request
  child_run_id
  child_agent_id
  child_human_request_id
  parent_tool_call_id
  delegation_join_id
```

用户 resolve child HumanRequest 后：

```text
resume child
  -> child completes / fails / times out
  -> runtime materializes delegate_agent tool output
  -> update DelegationJoinState
  -> if join complete, resume parent with child result
```

parent 不伪造 child 的 HumanResponse，不把 child 的 approval 改写成 parent approval。parent 只被 child 的等待状态阻塞。

`max_duration_ms` 只计算 child active execution time，不计算人类等待时间。child 进入 `waiting_human` 后，execution timer 停止，run 变成持久化等待状态。用户或上层可以通过 deny/cancel 结束该 HumanRequest；v0 不做自动超时处理。

#### Suspended delegate tool call 状态机

child `waiting_human` 进入 v0 后，parent 的 `delegate_agent` tool call 必须有自己的 continuation 状态，不能只依赖 child run 状态。

```text
delegate_tool_call.status:
  running_child
  suspended_waiting_child_human
  materialized
  failed
  timeout
  canceled
```

状态流：

```text
running_child
  -> materialized                         # child completed / failed / timeout 并已生成 parent tool output
  -> suspended_waiting_child_human         # child run waiting_human

suspended_waiting_child_human
  -> running_child                         # child HumanRequest resolved, runtime resumes child
  -> canceled                              # user / parent / policy cancels waiting child

running_child
  -> materialized                          # resumed child reaches completed / failed / timeout
```

`suspended_waiting_child_human` 至少保存：

```yaml
parent_run_id: run_parent_001
parent_session_id: cli-default:user-local
parent_agent_id: dev-implementer
parent_tool_call_id: call_delegate_001
tool_batch_id: batch_001
delegation_join_id: djoin_20260614_001
child_run_id: run_child_001
child_agent_id: reviewer
child_human_request_id: hr_child_001
child_session_id: child-session-run_child_001
context_packet_ref: .xira/runs/run_child_001/artifacts/context/context_packet.json
resume_policy:
  on_child_completed: materialize_delegate_output
  on_child_failed: materialize_failed_delegate_output
  on_child_timeout: materialize_timeout_delegate_output
```

这份状态是 parent tool-call continuation。它的职责不是替代 HumanRequest store，而是在 child resolve 后告诉 runtime：应该恢复哪个 child、把结果写回哪个 parent tool call、什么时候恢复 parent。

### 4. Parent agent 能不能并行启动多个 child

可以。v0 应支持同一个 parent tool-call batch 里多个 `delegate_agent` 并行 fan-out。

```text
parent emits:
  delegate_agent(reviewer)
  delegate_agent(test-runner)
  delegate_agent(security-auditor)

runtime:
  create DelegationJoinState(join=all)
  start child runs in parallel up to max_parallel
  persist each child output or waiting state
  when all child outputs are materialized, inject them into parent in original tool call order
  parent model synthesizes
```

v0 固定 join policy：

```text
join = all
```

也就是说，parent 要等同一批 child 都进入终态后才继续。如果有 child waiting_human：

```text
reviewer completed
test-runner completed
security-auditor waiting_human

parent run -> waiting_human
completed child results are persisted in DelegationJoinState
resume only waits on pending child
after all child results are available, parent continues
```

v0 的 join 终态按 delegate call status 计算：

```text
delegate call terminal for join:
  completed
  failed
  timeout
  canceled
```

`waiting_human` 不是 join 终态，它是 parent run 的等待原因。用户或上层对 child HumanRequest 做 deny/cancel 时，runtime 必须把 child run 记为 failed + `error_type=canceled`，并把该 child delegation materialize 成 `status=canceled` 的 delegate output，随后更新 DelegationJoinState。这样 `join=all` 不会只能依赖“用户最终 approve 并让 child 完成”这一条路。

v0 不做自动超时处理，但必须提供显式退出路径：

```text
child HumanRequest denied/canceled
  -> child run failed with error_type=canceled
  -> delegate_agent output status=canceled
  -> DelegationJoinState updated
  -> if all child calls terminal, parent resumes
```

持久化 join state 示例：

```yaml
schema_version: xira.delegation_join.v0
id: djoin_20260614_001
workspace: /Users/yinwm/work/flowdeck
parent_run_id: run_parent_001
parent_agent_id: dev-implementer
tool_batch_id: batch_001
join_policy: all
status: waiting_human
calls:
  - parent_tool_call_id: call_review
    child_run_id: run_review
    child_agent_id: reviewer
    status: completed
    output_ref: .xira/runs/run_parent_001/delegations/call_review.output.json
  - parent_tool_call_id: call_security
    child_run_id: run_security
    child_agent_id: security-auditor
    status: waiting_human
    child_human_request_id: hr_security_001
```

并行边界由 caller profile delegation policy 控制：

```yaml
delegation:
  enabled: true
  allow:
    - reviewer
    - test-runner
  max_parallel: 3
  max_outstanding: 12
  max_depth: 2
  default_max_duration_ms: 120000
  max_duration_ms: 300000
  child_session_mode: ephemeral_worker
```

`max_parallel` 在 v0 只限制 active child execution，不限制 suspended waiting child：

```text
child active execution starts
  -> acquire active child slot under max_parallel

child enters waiting_human
  -> persist suspended delegate tool call + DelegationJoinState
  -> release active child slot
  -> count against max_outstanding, not max_parallel

child HumanRequest resolved
  -> resume request tries to acquire active child slot again
  -> if max_parallel is full, child stays resume_pending until a slot is available
```

`max_outstanding` 限制同一个 parent run 下尚未 materialize 的 child calls，包括 active、waiting_human、resume_pending。它防止大量 suspended child 长期堆积。若未配置，v0 默认 `max_outstanding = max_parallel * 4`。

进程重启后：

```text
active child slots:
  从当前 runtime 正在执行的 child goroutine / run lease 重建

outstanding child calls:
  从 parent run 的 DelegationJoinState 重建

suspended waiting child:
  不占 active child slot
  仍占 max_outstanding
```

v0 不做：

- `wait=false` / fire-and-forget。
- `join=any`。
- child 中间消息实时流进 parent context。
- child 直接长期接管用户会话。
- Task/Todo 工具驱动 delegation。

### 动态生成 child agent

动态生成 child agent 不改变以上四个语义。

区别只在 target 来源：

```text
existing child:
  target.agent_id + profile version

generated child:
  generated profile snapshot + generator agent/run/tool_call lineage
```

如果未来支持动态生成 agent，runtime 必须保存 generated profile snapshot，包括 instructions、tools、model policy、permissions 和创建来源。child 仍然只看 runtime 构造的 child context，仍然按 `delegate_agent` result envelope 回传。

### 与 Task / Todo 工具的关系

Task / Todo 工具不是 AgentDelegation 的前置条件。

```text
Task/Todo = planning state
delegate_agent = execution state
Flow = delivery state
```

父 agent 可以自己维护计划，也可以未来用 Todo 工具管理计划板。但 delegation 不依赖 Todo：

```text
parent agent
  -> delegate_agent
  -> child run
```

如果未来增加 Todo 工具，它只能表达“父 agent 的工作计划”，不能替代 child context、child result、parent wait、persistent join、HITL propagation 这些 runtime 语义。Todo item 可以由 agent 决定升级成 `delegate_agent` 调用，但 Todo 工具不自动创建 child run。

## 两种触发来源

### 1. Agent 主动请求

agent 发现需要用户介入时，调用 runtime tool：

```text
human.request
```

v0 决策：`human.request` 是所有 agent 都可用的内置 runtime tool，不需要在每个 agent profile 里单独声明 permission。

原因：

- HITL 是 agent runtime 的基础能力。
- 轻量 agent-only 场景也需要随时问人。
- 是否问人应该由 agent 的任务上下文、instructions、当前不确定性共同决定，而不是由 profile 的 tool allowlist 决定。

runtime 仍然需要对 `human.request` 做审计和基本校验，例如 question 不能为空、kind 必须合法、options 结构必须合法。

示例：

```json
{
  "kind": "clarification",
  "question": "你要修的是哪个 repo？",
  "reason": "当前请求没有仓库路径，无法安全执行开发任务。",
  "options": []
}
```

或者：

```json
{
  "kind": "choice",
  "question": "这个 bug 有两个修法，你选哪个？",
  "options": [
    { "id": "minimal_fix", "label": "最小修复" },
    { "id": "refactor", "label": "顺手重构" }
  ]
}
```

agent 主动请求主要覆盖语义层：

- 信息不足
- 业务偏好
- 多方案选择
- 需要用户确认意图

### 2. 用户提交 HumanResponse

用户可以用 API / CLI / UI button 提交结构化 HumanResponse。Channel 也可以提供 slash command 或按钮，但 v0 要求由 channel/router 解析成结构化 response 后再写入 store。

```text
xira human approve hr_20260613_001
xira human deny hr_20260613_001
xira human answer hr_20260613_002 --message "repo 是 /Users/yinwm/work/flowdeck"
```

HTTP 示例：

```json
{
  "kind": "approve",
  "actor": "yinwm",
  "idempotency_key": "msg_20260613_001"
}
```

runtime 负责校验：

- request 必须存在且状态是 `pending`。
- request 必须属于当前 session / actor 可见范围。
- `response.signal` 必须符合 request kind 和 options。
- source message 必须来自 transport/router 提供的用户消息引用；不接受 agent 任意填写历史 message id 作为高信任来源。
- 同一个 request 只能 resolve 一次。

强制型 approval 必须走 CLI / API / UI button，或走 channel/router 直接解析出的结构化 HumanResponse。也就是说，“没问题干吧”可以成为好的交互体验，但不能默认成为高风险 tool gate 的唯一安全依据。

如果当前 session 只有一个 pending request，channel/router 可以把明确的 `/approve`、按钮点击或强约束快捷回复解析成结构化 approve。

如果有多个 pending request，router 必须要求 request id；agent 可以追问用户指的是哪一个，但不能通过模型可见工具直接 resolve。

如果用户表达不明确，例如“看着办”，agent 不应该直接提交 approve，而应该继续澄清。

### 3. Runtime 强制拦截

runtime 可以在执行 tool 前做 policy check，但 v0 不把“命令风险分类器”作为核心能力。

v0 决策：不要在第一版内置 `command.run` 风险分类器。

原因：

- 命令风险不可能靠固定列表列全。
- 同一个命令在不同 repo、不同 cwd、不同客户环境里的风险不同。
- 过早做分类器会制造一种假的确定性。

但“不做命令风险分类器”不等于“不做 runtime gate”。

当前 tool 定义已经有：

```go
type ToolPolicy struct {
    Risk                string
    RequireConfirmation bool
}
```

v0 必须接通 `RequireConfirmation`：

```text
executeToolCall
  -> resolve tool definition
  -> if def.Policy.RequireConfirmation
       create HumanRequest(kind=approval, request_source=runtime_tool_gate)
       persist action_snapshot
       do not execute original tool
       return waiting_human
  -> else execute tool
```

这不是固定命令列表，也不是通用风险分类器。它是 per-tool / profile / workspace 可以声明的强制 gate，正好对应“确定性来自业务配置而非硬编码列表”。

v0 更合理的边界是：

| 能力 | v0 默认 |
| --- | --- |
| `human.request` | 所有 agent 可用 |
| HumanResponse resolve | 只通过 API / CLI / transport-structured response；不暴露模型可见 `human.respond` |
| `read_file` / `search_file` / `list_dir` | 不问 |
| `write_file` / `edit_file` | 不默认 ask |
| `tool_output.read` | 不问 |
| `command.run` / `shell.run` | 不做内置风险分类，但尊重 `RequireConfirmation` |

也就是说，v0 的核心不是“runtime 猜命令风险”，而是把 HumanRequest / HumanResponse / waiting_human / audit 链路做出来，并接通已有的声明式 gate。

如果某个业务或交付环境需要更确定的边界，应该通过 agent profile、workspace policy、flow step instructions、客户环境配置来声明，而不是把命令列表硬编码在 HITL core 里。

## Approval HumanRequest v0 行为

approval 分两类，不能一刀切：

| 来源 | 是否有确定动作 | approve 后策略 |
| --- | --- | --- |
| `agent_request` | 不一定有，只是 agent 描述的意图 | 由 agent 在后续 turn 继续 |
| `runtime_tool_gate` | 有，runtime 已拦截具体 tool call | runtime 按 `action_snapshot` 重放 |

当 agent 认为某个动作需要人工确认：

1. `human.request(kind: approval)` 创建 HumanRequest。
2. 当前动作不继续执行。
3. ToolCallRecord 或 runtime event 记录为 waiting human。
4. agent run 状态变成 `waiting_human`。
5. TurnResponse 返回 `human_requests`。
6. 用户 approve / deny。
7. runtime 通过 API / CLI / transport-structured response 写入 HumanResponse；agent 通过后续普通 turn 读取 HumanResponse。

这类 approval 是协作型确认，不提供“批准什么就一定执行什么”的保证。

当 `RequireConfirmation` gate 拦截 tool call：

1. `executeToolCall` 创建 `HumanRequest(kind: approval, request_source: runtime_tool_gate)`。
2. runtime 保存 `action_snapshot`，包括 tool name、tool call id、input、cwd、run id、env_snapshot、env_hash、replay_ttl。
3. 不执行原 tool。
4. ToolCallRecord 记录为 `waiting_human`。
5. agent run 状态变成 `waiting_human`。
6. 用户通过结构化 HumanResponse approve / deny。
7. approve 后 runtime 只允许按 `action_snapshot` 重放，不让 agent 重新构造命令。

工具返回给模型的内容类似：

```json
{
  "status": "waiting_human",
  "human_request_id": "hr_20260613_001",
  "message": "This action requires human approval before execution."
}
```

v0 不在同一个 HTTP 请求里阻塞等待人。

## Resume v0

v0 推荐用“新 turn 恢复”，而不是挂起 goroutine。

流程：

```text
POST /agent-runs
  -> agent 创建 HumanRequest
  -> run.status = waiting_human

POST /human-requests/{id}/responses
  -> status = approved / denied / answered

POST /agent-runs with same session_id and metadata.resume_human_request_id
  -> runtime 把 HumanResponse 注入上下文
  -> agent 继续
```

后续可以增加：

```text
POST /agent-runs/{run_id}/resume
```

但 v0 不必强行恢复同一个 goroutine 或同一个 model call。

### Native 路径的 session 回灌前置条件

当前 native DeepSeek 路径是固定两次 model call：

```text
first Chat -> tool_calls -> executeToolCall -> second Chat -> return
```

并且每次 `generateNativeDeepSeek` 都从 `{system, req.Message}` 重建 messages。如果不把 session history、pending HumanRequest、resolved HumanResponse、snapshot replay result 回灌到下一次 model call，所谓“后续 turn 继续”在 native 路径上不会成立。

所以 v0 必须增加 native session hydration：

```text
RunAgent 分配 session
  -> load AgentHistory(session_id, agent_id)
  -> load pending/resolved HumanRequest summary for session
  -> load replay result if metadata.resume_human_request_id exists
  -> build native messages:
       system
       latest K prior user/assistant/tool messages
       human request/response/replay summary
       current user message
```

v0 不做语义级 compaction。v0 采用最小滑动窗口：

```text
always include:
  active pending HumanRequests
  recently resolved HumanResponses
  replay results for the current session

history window:
  latest K session messages
  capped by max_history_chars
  older messages are omitted with a truncation marker
```

建议默认：

```text
K = 20 session messages
max_history_chars = 24000
```

如果后续需要更长上下文，再单独实现 session summary / semantic compaction；不要把它隐藏在 HITL v0 里。

同时，`waiting_human` run 也要写入 session history，至少包含：

- 用户原始请求。
- 被挂起的 HumanRequest 摘要。
- 如果是 `runtime_tool_gate`，保存 tool call input 快照引用。
- runtime 给用户的 waiting message。

没有对应 engine 的 HITL hydration 前，v0 只能可靠支持 `clarification` / `choice` 这类纯问答；不能声称支持 approval resume。

### approve 后是否自动重放 tool call

这里的“自动重放”指：

```text
agent 调用 command.run
  -> runtime 拦截并创建 HumanRequest
  -> 用户 approve
  -> runtime 不再问 agent，直接拿刚才那份 tool call input 执行一次
```

对 `runtime_tool_gate`，v0 选择自动重放，但必须基于快照：

```text
runtime 拦截 tool call A
  -> 保存 action_snapshot(A)
  -> 用户结构化 approve
  -> runtime 检查 snapshot 未过期、未重放、scope 匹配、trust_level 足够
  -> runtime 以 replay execution mode 执行 action_snapshot(A)
  -> 保存 replay result
  -> 后续 turn 把 replay result 注入 agent 上下文
```

理由：

- 用户批准的是 A，就只能执行 A。
- agent 不能在 approve 后重新生成一个 B。
- snapshot 过期是数据问题，可以用 TTL、cwd、workspace revision、env_hash 检查发现。
- 重新构造命令导致“批准 A 执行 B”是安全问题，runtime 很难检测。

### replay execution mode

snapshot replay 不能直接重新走普通 `executeToolCall` gate，否则同一个 `RequireConfirmation=true` tool 会再次创建 HumanRequest，形成死循环。

v0 必须定义专用 replay execution mode：

```text
executeReplay(action_snapshot, human_response)
  -> require request_source=runtime_tool_gate
  -> require response.signal=approve
  -> require response.trust_level=transport_authenticated
     or explicitly enabled router_structured channel
  -> require snapshot not expired
  -> require env_hash still matches current replay environment
  -> atomically claim replay:
       replay_status pending -> running
       set replay_attempt_id, replay_started_at, replay_lease_expires_at
       if claim fails because status=running, return in_progress
       if claim fails because status=completed/failed, return existing result
  -> execute tool with replay=true
       bypass RequireConfirmation gate
       keep all tool allowlist/workspace/path/timeout checks
       keep audit and raw output persistence
  -> if replay_attempt_id still matches, atomically write replay_status running -> completed/failed
```

`replay=true` 只跳过二次 `RequireConfirmation`。它不能跳过 tool allowlist、workspace path checks、timeout、secret policy、audit、raw output capture。

`replay_status` 必须是状态机，不是普通字段：

```text
pending -> running -> completed
pending -> running -> failed

running + lease expired -> pending 或 failed_recoverable
completed / failed 为终态，除非用户重新发起新的 HumanRequest
```

`POST /human-requests/{id}/responses` 的重试语义：

- 第一个请求用 CAS 抢占 `pending -> running` 并开始执行 replay。
- 并发或重试请求看到 `running` 时直接返回 replay `in_progress`，不得重复执行。
- 请求看到 `completed` / `failed` 时返回已有 replay result，保持幂等。
- 只有 `running` lease 过期时，补偿路径才允许重新抢占；否则不能把“HTTP 超时但进程仍在执行”误判成需要重放。

`env_hash` 不是防篡改签名。v0 用它做环境漂移检测，计算口径至少包含：

- `workspace_root`
- `cwd`
- git repo 的 `HEAD`，如果当前 workspace 是 git repo

如果不是 git repo，`git_head` 为空；此时主要依赖 `cwd`、TTL 和业务约束。v0 默认 `replay_ttl = 15m`。

测试必须覆盖：replay 执行的 tool 不再触发第二个 HumanRequest；并发 approve 只能有一个请求抢到 `running` 并执行 replay。

### ToolCallRecord 与 replay result 闭合

被 `RequireConfirmation` 拦截的原始 tool call 仍然要进入 run log，但它的状态是 `waiting_human`，不是执行成功或失败。

等待时的 ToolCallRecord output 至少包含：

```json
{
  "status": "waiting_human",
  "human_request_id": "hr_20260613_001",
  "action_snapshot_ref": ".xira/state/workspaces/ws_3f4a1c8e9b1200af/human-requests/hr_20260613_001.yaml",
  "replay_status": "pending"
}
```

replay 完成后必须写出闭合关系：

```json
{
  "human_request_id": "hr_20260613_001",
  "source_run_id": "20260613-223000-xira-assistant",
  "source_tool_call_id": "call_001",
  "replay_attempt_id": "rpa_20260613_001",
  "replay_status": "completed",
  "replay_ref": ".xira/runs/20260613-223000-xira-assistant/replay/call_001.json"
}
```

v0 file store 可以用 sidecar 实现，不必原地改写 append-only `tool_calls.jsonl`：

```text
.xira/runs/<run_id>/replay/tool_replay_links.jsonl
.xira/runs/<run_id>/replay/<source_tool_call_id>.json
```

但 API / CLI 展示 run log 时必须 materialize 这个关系，让审计者从原 ToolCallRecord 能看到 `human_request_id`、`replay_status` 和 `replay_ref`，不需要手工在多个文件之间猜关联。

对 `agent_request`，v0 不自动重放，因为本来就没有 runtime 捕获的确定 tool input。它仍然走 agent-first：

```text
agent 调用 human.request
  -> 用户 approve
  -> 后续 turn 把 approve response 放进上下文
  -> agent 自己决定下一步
```

因此，v0 的最终规则是：

```text
runtime_tool_gate approval -> snapshot + runtime replay
agent_request approval     -> agent 继续，不提供执行一致性保证
```

## 用户交互

CLI：

```bash
xira human list
xira human show hr_20260613_001
xira human approve hr_20260613_001
xira human deny hr_20260613_001
xira human answer hr_20260613_002 --message "repo 是 /Users/yinwm/work/flowdeck"
```

Channel 普通消息：

```text
没问题干吧
可以，继续
先别动
按方案 A
这个不要做
```

这些消息先作为普通消息进入 runtime router。runtime 给 agent 注入当前 session 的 pending HumanRequest 摘要，agent 可以判断这条消息是否在回答某个 request 并继续追问，但不能直接通过模型可见工具 resolve。

注意：自然语言 response 不是 v0 的 resolve 写入口。对 `request_source=runtime_tool_gate` 的强制型 approval，v0 需要结构化 HumanResponse。

Channel slash command 只是可选快捷写法：

```text
/approve hr_20260613_001
/deny hr_20260613_001
/answer hr_20260613_002 repo 是 /Users/yinwm/work/flowdeck
```

v0 决策：channel 里的自然语言回复先作为普通消息进入 runtime router。`/approve` / `/deny` / `/answer` 如果要 resolve HumanRequest，必须由 channel/router 解析成结构化 HumanResponse、带上 transport 层用户身份；如果要用于强制型 approval，该 channel 还必须显式开启 strong approval。

原因：

- channel runner 保持薄，只做消息归一化。
- agent 可以看到用户的自然语言上下文，不局限于机械命令。
- 与“agent_request 由 agent 继续、runtime_tool_gate 由 snapshot replay”一致。
- v0 默认只有 CLI / API / local UI 的 `transport_authenticated` response 可以 resolve 强制型 approval。
- channel `router_structured` 默认拒绝强制型 approval，除非 per-channel 配置显式开启。

channel strong approval 放 entrypoint config，默认关闭：

```yaml
entrypoints:
  - id: feishu-support
    type: channel
    channel: feishu
    human_response:
      strong_approval:
        enabled: false
        accepted_response_sources:
          - router_structured
        require_transport_identity: true
```

只有当某个 entrypoint 的身份链路足够可信时，才能显式改成 `enabled: true`。workspace policy 不应该把所有 channel 一次性打开。

Channel 结构化 response 路径：

```text
用户消息："/approve hr_20260613_001"
  -> runtime router 接收普通消息
  -> channel/router 解析成结构化 HumanResponse
  -> runtime 校验 session / actor / request 状态与 entrypoint 信任级别
  -> 如果 request 允许该信任级别，HumanRequest.status = resolved
  -> 如果 request 是强制型 approval 且 channel 未开启 strong approval，runtime 拒绝并要求 CLI/API/UI approve
  -> run log 和 audit 记录原始用户消息、结构化 response 和 trust_level
```

API：

```text
GET  /api/v1/human-requests
GET  /api/v1/human-requests/{id}
POST /api/v1/human-requests/{id}/responses
```

Response body：

```json
{
  "signal": "approve",
  "actor": "user-local",
  "message": "Allow once.",
  "source": "api"
}
```

### responses API 契约

`POST /api/v1/human-requests/{id}/responses` 不只是落库。

v0 决策：

```text
request_source=runtime_tool_gate
  + response.signal=approve
  + trust_level 足够
  -> responses API 同步尝试 claim replay
  -> claim 成功则执行 snapshot replay
  -> claim 失败则返回当前 replay 状态
  -> response body 返回 HumanResponse + replay status/result ref

其他 request_source 或非 approve response
  -> responses API 只 resolve HumanRequest
```

返回示例：

```json
{
  "human_response": {
    "id": "hres_20260613_001",
    "human_request_id": "hr_20260613_001",
    "signal": "approve",
    "trust_level": "transport_authenticated"
  },
  "human_request": {
    "id": "hr_20260613_001",
    "status": "approved"
  },
  "replay": {
    "status": "completed",
    "attempt_id": "rpa_20260613_001",
    "artifact": ".xira/runs/fr_001/replay/call_001.json",
    "tool": "command.run"
  }
}
```

如果同一个 approve 请求被客户端重试，而 replay 仍在执行，返回示例：

```json
{
  "human_response": {
    "id": "hres_20260613_001",
    "human_request_id": "hr_20260613_001",
    "signal": "approve",
    "trust_level": "transport_authenticated"
  },
  "human_request": {
    "id": "hr_20260613_001",
    "status": "approved"
  },
  "replay": {
    "status": "running",
    "attempt_id": "rpa_20260613_001",
    "started_at": "2026-06-13T22:35:00+08:00"
  }
}
```

这解决两个问题：

- 用户 approve 后，动作立即执行，不需要再发一条消息触发下一个 `RunAgent`。
- HTTP 不等待用户；用户已经在这个请求里给出 response，runtime 只尝试抢占并执行一个有 timeout/lease 的 tool replay。
- HTTP 重试是幂等的；`running` / `completed` / `failed` 都返回已有 replay 状态，不重复执行。

v0 不自动启动新的 model call 来生成后续自然语言总结。CLI/API/Channel 可以直接展示 replay result；后续 agent turn 会通过 hydration 看到 replay result。

## Runtime 状态变化

`TurnResponse` 增加：

```go
HumanRequests []HumanRequest `json:"human_requests,omitempty" yaml:"human_requests,omitempty"`
Interrupt     *RunInterrupt `json:"interrupt,omitempty" yaml:"interrupt,omitempty"`
```

`HumanRequests` 是兼容性字段，方便 CLI/UI 直接展示待处理请求；`Interrupt` 是 runtime 的统一中断契约，所有会让当前 run 停在 `waiting_human` 的路径都必须写入它。

```go
type RunInterrupt struct {
    Status             string                 `json:"status" yaml:"status"` // waiting_human
    HumanRequests      []HumanRequestSummary  `json:"human_requests,omitempty" yaml:"human_requests,omitempty"`
    BlockedBy          *BlockedBy             `json:"blocked_by,omitempty" yaml:"blocked_by,omitempty"`
    SuspendedToolCalls []SuspendedToolCall     `json:"suspended_tool_calls,omitempty" yaml:"suspended_tool_calls,omitempty"`
    DelegationJoinIDs  []string               `json:"delegation_join_ids,omitempty" yaml:"delegation_join_ids,omitempty"`
    Metadata           map[string]any         `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}
```

新增 run status：

```text
waiting_human
```

v0 不新增 `canceled` 作为 `TurnResponse.Status` / run status。`canceled` 只用于 delegate tool-call continuation 和 parent 可见的 `delegate_agent` output。child HumanRequest 被 deny/cancel 后：

```text
child run status = failed
child run error_type = canceled
child run cancel_reason = human_request_denied | human_request_canceled | policy_canceled
delegate_agent output status = canceled
```

这样 run 级状态仍然保持 `completed / failed / waiting_human` 的最小集合，同时 parent 能区分“child 执行失败”和“child 等人后被取消”。

`waiting_human` 只能通过 `RunInterrupt` 返回。runtime 不允许各路径分别用普通 tool output、FinalResponse 字符串或 ad-hoc metadata 表达中断原因。

`RunInterrupt` 覆盖三类中断：

| 中断来源 | HumanRequests | BlockedBy | SuspendedToolCalls | DelegationJoinIDs |
| --- | --- | --- | --- | --- |
| agent 主动 `human.request` | 当前 run 创建的 request | 可空 | 空 | 空 |
| `runtime_tool_gate` approval | 被 gate 创建的 request | 可空 | 原 tool call 摘要可选 | 空 |
| child `waiting_human` | child run 创建的 request | `child_human_request` | parent `delegate_agent` tool call | parent join ids |

native / ADK 收到 `RunInterrupt.Status=waiting_human` 后必须立刻短路当前 model loop：

```text
tool execution creates RunInterrupt
  -> runtime persists request / suspended tool call / join state
  -> native path does not issue second model call
  -> ADK adapter does not ask model to continue
  -> RunAgent returns TurnResponse{Status: waiting_human, Interrupt: ...}
```

也就是说，`RunInterrupt` 是 run 级中断信号；`HumanRequestCollector` 只能收集“当前 run 创建了 HumanRequest”，不能表达 delegation suspend / blocked_by / join state，因此不能作为唯一 contract。

当 parent run 因 child agent 的 HumanRequest 被阻塞时，仍使用 `waiting_human`，但必须在 response / run metadata 中标明 `blocked_by`：

```yaml
blocked_by:
  type: child_human_request
  child_run_id: 20260614-101010-reviewer-ab12
  child_agent_id: reviewer
  child_human_request_id: hr_20260614_001
  parent_tool_call_id: call_delegate_001
  delegation_join_id: djoin_20260614_001
```

HumanRequest 也写入：

```text
.xira/state/workspaces/<workspace_key>/human-requests/<human_request_id>.yaml
.xira/state/workspaces/<workspace_key>/human-responses/<human_response_id>.yaml
.xira/state/workspaces/<workspace_key>/replay-results/<human_request_id>.yaml
```

run log 继续保存摘要：

```text
.xira/runs/<run_id>/human_requests.jsonl
.xira/runs/<run_id>/human_responses.jsonl
```

DelegationJoinState 是 parent run 的 continuation state，写入 parent run 目录：

```text
.xira/runs/<parent_run_id>/delegations/<delegation_join_id>.yaml
.xira/runs/<parent_run_id>/delegations/<parent_tool_call_id>.state.yaml
.xira/runs/<parent_run_id>/delegations/<parent_tool_call_id>.output.json
.xira/runs/<child_run_id>/artifacts/context/context_packet.json
```

它不写入 `.xira/state/workspaces/<workspace_key>/...`，因为它不是 workspace 级权威状态，而是 parent run 的挂起 tool-call / join 恢复状态。workspace 级权威状态仍然是 HumanRequest / HumanResponse / replay result；DelegationJoinState 只回答“parent 的哪个 delegate tool call 还在等哪个 child 结果”。

delegate tool-call continuation 示例：

```yaml
schema_version: xira.delegate_tool_call.v0
parent_run_id: run_parent_001
parent_tool_call_id: call_delegate_001
delegation_join_id: djoin_20260614_001
status: suspended_waiting_child_human
child_run_id: run_child_001
child_agent_id: reviewer
child_human_request_id: hr_child_001
context_packet_ref: .xira/runs/run_child_001/artifacts/context/context_packet.json
output_ref: .xira/runs/run_parent_001/delegations/call_delegate_001.output.json
```

API / CLI resolve 后还必须写入 session AgentHistory 摘要，用于后续 native / ADK hydration：

```text
kind: human_response_summary
producer: runtime.human_request
human_request_id: hr_...
human_response_id: hres_...
signal: approve
replay_status: completed|running|failed|none
```

child resume 后 materialized delegate output 也必须写入 parent session AgentHistory 摘要：

```text
kind: delegate_tool_output_summary
producer: runtime.delegation
parent_tool_call_id: call_delegate_001
child_run_id: run_child_001
delegation_join_id: djoin_20260614_001
status: completed|failed|timeout|canceled
output_ref: .xira/runs/<parent_run_id>/delegations/call_delegate_001.output.json
```

不把完整 HumanResponse 或 child transcript 当作普通用户消息注入；只写结构化摘要，避免污染后续 agent 的对话语义。

## Audit / Event

每个 HumanRequest 和 HumanResponse 都应该有 event 和 audit。

Runtime events：

```text
human.request.created
human.response.created
human.request.resolved
human.request.expired
agent.delegate.requested
agent.delegate.allowed
agent.delegate.rejected
agent.delegate.started
agent.delegate.suspended
agent.delegate.resumed
agent.delegate.completed
agent.delegate.failed
agent.delegate.timeout
agent.delegate.canceled
agent.delegate.result_delivered
delegation.join.created
delegation.join.updated
delegation.join.completed
delegation.join.blocked
delegation.join.resumed
context.packet.started
context.item.included
context.item.redacted
context.packet.completed
```

Audit events：

```text
human.request
human.response
tool.approval_required
agent.delegate
```

要求：

- audit 不一定保存完整敏感内容。
- 必须保存足够复盘的信息：谁问、问什么、属于哪个 run、谁批准、什么时候批准。
- router_structured response 必须保存原始用户消息引用，例如 `source_message_id` 和 `source_text_preview`。
- audit 中要区分 `actor` 和 `resolver`：批准者是用户，解析/写入者是 API、CLI 或 channel router。
- HumanResponse 必须记录 `trust_level`：`transport_authenticated` 或 `router_structured`。
- runtime snapshot replay 必须记录 action snapshot hash、replay status、replay output ref、过期/拒绝原因。
- agent delegation 必须记录 parent_run_id、parent_tool_call_id、child_run_id、caller_agent_id、target_agent_id、delegation_depth、max_parallel / max_outstanding / max_depth policy 结果。
- delegation join 必须记录每个 parent_tool_call_id 对应的 child_run_id、状态、output_ref、child_human_request_id 和更新原因。
- context packet 必须记录 included / redacted / truncated items，保留 source ref、materialized ref、content hash 和截断信息。
- delegate result delivery 必须记录 child result schema、summary preview、evidence ref count、limitations count、confidence 和 followup_needed。

## Policy v0

v0 不做完整 policy engine，也不做 `command.run` 风险分类器。

第一版只保留最小运行时事实：

```text
human.request -> allow for all agents
HumanResponse resolve -> API / CLI / transport-structured response only; no model-visible human.respond in v0
delegate_agent -> allow only when caller delegation policy enables target agent
read_file/search_file/list_dir -> allow
write_file/edit_file -> allow by default
tool_output.read -> allow
command.run/shell.run -> no built-in classifier, but RequireConfirmation is enforced
```

如果需要更确定的执行边界，由业务配置决定：

```text
agent profile instructions
agent profile tool allowlist
workspace policy
flow step instructions / constraints
customer deployment policy
```

workspace policy v0 只放 workspace 级默认值，不放具体 channel 信任开关。例如 replay TTL：

```yaml
human_request:
  replay:
    ttl: 15m
```

如果未配置，runtime 使用默认 `15m`。具体 channel 是否能做 strong approval 不放在 workspace policy，因为这是入口身份可信度问题，不是 workspace 全局能力。

例如开发类 agent 可以被提示：

```yaml
instructions:
  - Use /skill code-implementation for code edits.
  - Ask the user before merge, publish, or remote destructive operations.
  - Keep changes scoped to files implied by the task.
```

这类确定性来自业务上下文和 agent instruction，不来自 HITL core 的硬编码命令分类。

## 已定决策

```text
human.request -> 所有 agent 默认可用
HumanResponse resolve -> 只通过 API / CLI / transport-structured response；v0 不暴露模型可见 human.respond
write_file/edit_file -> v0 不默认 ask
command.run -> v0 不做内置风险分类器，但执行 ToolPolicy.RequireConfirmation
runtime_tool_gate approval -> snapshot + runtime replay
agent_request approval -> agent 后续 turn 继续，不提供执行一致性保证
channel 自然语言回复 -> 普通消息；结构化 slash/button response 可由 router 写入 HumanResponse
CLI/API/local UI transport_authenticated response -> 可用于强制型 approval
channel router_structured response -> 默认不能用于强制型 approval，需 per-channel 显式开启
delegate_agent input -> 保持宽松，只要求 agent_id/task，可选 context_refs/expected_output_schema/max_duration_ms
delegate_agent child context -> runtime 构造 scoped context packet，不默认传 parent 完整 session/tool outputs/secrets
delegate_agent result -> child 返回 delegate_result_v1，runtime 补齐 agent_id/run_id/status 并校验证据 refs
delegate_agent trust -> runtime 只校验 schema/ref/runtime fields，不校验 child summary 真伪；confidence/followup_needed 仅为提示
delegate_agent wait -> parent 逻辑等待 child；child waiting_human 会 suspend parent delegate tool call，不挂 goroutine，且要求 parent/child continuation state
delegate_agent parallel -> 同一 parent tool-call batch 可 fan-out 多个 child，v0 join=all；active execution 受 max_parallel 限制，active+waiting_human+resume_pending 受 max_outstanding 限制
delegation_join_state -> parent run continuation state，保存 completed child outputs、pending child HumanRequest、suspended delegate tool call 和恢复顺序
responses API -> 强制型 approve 同步触发 replay 并返回 replay status/result ref
replay execution -> 使用 replay=true bypass 二次 RequireConfirmation，但保留其他校验
replay_status -> pending/running/completed/failed；responses API 必须 CAS 抢占 running，保证最多一次
env_hash -> 只做 replay 环境漂移检测，不做防篡改签名；v0 默认 replay_ttl=15m
native DeepSeek -> v0 必须实现滑动窗口 hydration + HumanRequest / HumanResponse / replay result 回灌
ADK -> v0 必须通过 session AgentHistory 注入同一份 HITL context
workspace -> HumanRequest 顶层必填；v0 从 runtime config 的 canonical WorkspaceRoot 解析，不新增 agent_workspace / flow_workspace scope type
workspace_key -> runtime 内部从 canonical workspace 派生，只用于 file store 目录分片
state root -> HITL state 使用 state_root/workspaces/<workspace_key>/{human-requests,human-responses,replay-results}，不并入 run/session root
API/CLI resolve -> 写 HumanRequest store + run audit + session AgentHistory 摘要
replay_ttl -> workspace policy 可配置，未配置时使用默认 15m
channel strong approval -> per-entrypoint config 显式开启，不放 workspace policy 默认打开
Task/Todo -> v0 不做；未来作为 planning state，不能替代 delegate_agent execution state
```

`state_root` 是 HITL / session / run 等状态的共同根，但子目录职责不同：

```text
state_root/
  workspaces/<workspace_key>/human-requests/
  workspaces/<workspace_key>/human-responses/
  workspaces/<workspace_key>/replay-results/
  sessions/
  runs/ 或外部 run_root
```

HumanRequest store 是权威状态；run log 保存当次 run 的摘要；session AgentHistory 保存给后续 agent hydration 用的摘要。三者用途不同，不能只写其中一个。

## 与 ADK Tool Confirmation 的关系

当前 ADK adapter 已经有 `RequireConfirmationProvider` 接入点。

但 Xira 不能只依赖 ADK 的确认机制，因为：

- Xira 还要支持非 ADK agent engine。
- Xira 要把 request 持久化到 state store。
- Xira 要支持 channel / CLI / API response。
- Xira 要生成自己的 audit 和 run log。

因此：

```text
ADK confirmation 可以作为 UI/adapter 层提示。
Xira HumanRequest 才是 runtime 权威状态。
```

实现时，`RequireConfirmationProvider` 可以先和 Xira policy 保持一致，但真正的创建、保存、resolve、snapshot replay 应由 Xira runtime 完成。

当前 ADK adapter 已经消费 `def.Policy.RequireConfirmation`。v0 必须让 native DeepSeek 路径也在 `executeToolCall` 前消费同一个字段，否则同一个 profile 在不同 agent engine 下会有不同安全行为。

## ADK Hydration

ADK 路径也必须看到 HumanRequest / HumanResponse / replay result / delegate output，否则 native 和 ADK 的 resume 行为会不一致。

v0 不为 ADK 设计第二套 HITL / delegation 状态。做法是：

```text
HumanRequest / HumanResponse / replay result / delegate output
  -> 写入 HITL store
  -> 同时写入 session AgentHistory，作为 runtime context message
  -> hydrateADKSession 读取 AgentHistory
  -> adkEventFromSessionMessage 转为 ADK session event
```

建议新增 session message kind：

```text
human_request_summary
human_response_summary
human_replay_result
delegation_join_summary
delegate_tool_output_summary
```

如果 v0 不新增 kind，也可以先用 `kind=message`、`role=user`、`metadata.producer=runtime.human_request|runtime.delegation` 写入一段结构化摘要，让 ADK session hydration 以普通用户上下文恢复。

约束：

- ADK 和 native 使用同一份 HITL store。
- ADK 和 native 使用同一套 replay result。
- ADK 和 native 使用同一套 DelegationJoinState / delegate output summary。
- ADK 不自己决定 replay；replay 仍由 Xira `responses` API / runtime 执行。
- `RequireConfirmationProvider` 只是 adapter 层提示，不能替代 Xira HumanRequest 状态。

## 与 Flow 的关系

Flow 不重新实现 HITL。

未来 Flow 使用同一套 HumanRequest：

```yaml
workspace: /Users/yinwm/work/flowdeck
scope:
  type: flow_step
  id: fr_20260613_devrun_001:approve_merge
```

两种情况：

1. Flow 显式 `human_approval` step 创建 HumanRequest。
2. Flow 的 agent step 内部触发 HumanRequest，Flow Kernel 暂停当前 step。

Flow 也不重新实现 agent delegation。

如果 flow step 运行的是 agent，而该 agent 又调用 child agent：

```text
flow_step
  -> parent agent run
     -> delegate_agent tool call
        -> child agent run
```

Flow Kernel 只需要看到 parent agent run 的状态：

- child completed / failed / timeout：作为 parent `delegate_agent` tool output 回到 parent。
- child waiting_human：parent run 进入 `waiting_human`，Flow Kernel 暂停当前 step。
- parent resume 后，Flow step 再继续。

所以 Agent HITL 和 AgentDelegation 都是 Flow HITL 的前置能力。

## 第一版实现边界

v0 推荐按下面 11 个实现边界收敛；这组边界必须和后文“落地顺序”保持一致。

1. `workspace` canonicalization + internal `workspace_key` state root。
2. `internal/humanrequest` 数据模型和 file store：
   - types
   - list/get/create/resolve
   - HumanRequest / HumanResponse
   - action snapshot
   - env snapshot
   - replay result
   - replay_status CAS / running lease
3. 最小 response API：`POST /human-requests/{id}/responses` 原子写 HumanResponse / audit / AgentHistory 摘要，先不要求完整 CLI/UI。
4. runtime control tool：`human.request` + `TurnResponse.HumanRequests`；HumanResponse resolve 由最小 response API/CLI/transport-structured path 负责。
5. `waiting_human` run status + `RunInterrupt` + `RuntimeSuspendCollector`：
   - `TurnResponse.Interrupt`
   - `blocked_by`
   - `suspended_tool_calls`
   - `delegation_join_ids`
   - native / ADK 收到 interrupt 后短路当前 model loop
6. native / ADK 基础 hydration：回灌 HumanRequest / HumanResponse / replay result，确保后续 resume turn 有上下文。
7. delegation completed path：
   - child context packet
   - delegate_result_v1
   - parent/child lineage
   - completed/failed/timeout output materialize
8. delegation continuation：
   - DelegationJoinState
   - suspended delegate tool call
   - parent `blocked_by` metadata
   - delegate output summary hydration
   - max_parallel / max_outstanding 语义
9. child waiting_human propagation：
   - child resume
   - delegate output materialize
   - parent resume injection
   - join=all sibling output ordering
   - canceled/denied child HumanRequest materializes canceled delegate output
10. native `RequireConfirmation` gate + action snapshot / env_snapshot + synchronous snapshot replay。
11. API list/show + CLI list/show/approve/deny/answer + tests。

第 3 步的最小 response API 是第 9 步 child resume 和第 10 步 snapshot replay 的前置条件；不能后置到完整 CLI/API 展示阶段。

v0 暂不实现：

- UI approval panel。
- 自动恢复同一个 goroutine。
- flow scope。
- 多人审批。
- 超时自动处理。
- 复杂 policy DSL。
- 通用命令风险分类器。
- delegation fire-and-forget。
- delegation join=any。
- child 直接长期接管用户会话。
- Task/Todo tool。

这个边界意味着：delegation continuation / child waiting propagation 不是普通 delegation 增强，而是 v0 最大的 runtime 改造点。它必须和基础 hydration、RunInterrupt 一起设计，否则 parent resume 时没有地方注入 materialized child tool output。

## 现有代码挂点

当前 repo 已经有几个可以直接落地的挂点：

| 文件 | 现状 | HITL v0 改动 |
| --- | --- | --- |
| `apps/xira/internal/runtime/types.go` | `TurnRequest` / `TurnResponse` 已经是 run 的外部协议 | 给 `TurnResponse` 增加 `HumanRequests []humanrequest.HumanRequest` 和 `Interrupt *RunInterrupt`，并允许 `Status = waiting_human` |
| `apps/xira/internal/tools/registry.go` | tool 定义已经有 `ToolPolicy{Risk, RequireConfirmation}` | 不扩展成命令风险分类器；v0 直接消费 `RequireConfirmation` |
| `apps/xira/internal/runtime/service.go` | `RunAgent` 创建 run、调用 `generate`、最后统一设置 completed/failed | 增加 RuntimeSuspendCollector / RunInterrupt 分支；`waiting_human` 不算 failed，也不写成 completed |
| `apps/xira/internal/runtime/service.go` | `generateNativeDeepSeek` 每次从 `{system, req.Message}` 重建 messages | 增加 native session hydration，把 AgentHistory、HumanRequest summary、replay result 注入 messages |
| `apps/xira/internal/runtime/service.go` | `executeToolCall` 是 native DeepSeek 路径的 tool 执行入口 | v0 支持 `human.request`，HumanResponse resolve 走 API/CLI/transport 入口，并在执行普通 tool 前检查 `RequireConfirmation` |
| `apps/xira/internal/runtime/delegation.go` | 已有 `delegate_agent`、context packet、child run、delegate_result_v1、max_parallel / max_depth policy，但当前执行是同步阻塞等待 child FinalResponse | 升级为 suspendable delegation：completed/failed/timeout 仍 materialize output；child waiting_human 时保存 DelegationJoinState + delegate tool-call continuation，写入 RunInterrupt，parent run 返回 waiting_human；child resume 后 materialize delegate output 并恢复 parent |
| `apps/xira/internal/runtime/service_adk.go` | ADK tool adapter 已接 `RequireConfirmationProvider`，且 ADK 有自己的 session 机制 | v0 不依赖 ADK confirmation 做持久化；HumanRequest 的创建、保存、resolve、snapshot replay 由 Xira runtime 负责，并通过 AgentHistory 注入 ADK session context |
| `apps/xira/internal/api` | 已有 run/status 类 HTTP API | 增加 human request list/show/response API |
| `apps/xira/internal/cli` 或现有 CLI 命令入口 | 已有本地操作入口 | 增加 `xira human list/show/approve/deny/answer` |

一个重要约束：

```text
HITL 不能只是返回一个普通 tool output。

因为 native DeepSeek 路径里，tool output 之后当前实现会继续发第二次 model call。
如果已经创建 pending HumanRequest，runtime 必须能把这个状态带回 RunAgent，
让最终 run.status = waiting_human，而不是 completed 或 failed。

同时，HITL 也不能只做当前 run 内 collector。跨 turn resume 必须从 file store 重新加载 pending/resolved HumanRequest、action snapshot 和 replay result，并注入下一次 native / ADK model context。
```

建议实现一个 run 内 suspend collector。它不是只收集 HumanRequest，而是负责汇总所有会让 run 返回 `waiting_human` 的 runtime interrupt：

```go
type RuntimeSuspendCollector interface {
    Add(humanrequest.HumanRequest)
    SuspendToolCall(SuspendedToolCall)
    BlockBy(BlockedBy)
    AddDelegationJoin(id string)
    Pending() []humanrequest.HumanRequest
    HasInterrupt() bool
    Interrupt() RunInterrupt
}
```

`RunAgent` 创建 collector 并放入 context。`human.request` tool 创建 HumanRequest 后写入 collector，返回一个 `waiting_human` tool output。HumanResponse resolve 由 API / CLI / transport-structured path 写入 store、event 和 audit，不作为模型可见 tool 暴露。`executeToolCall` 遇到 `RequireConfirmation` 时创建强制型 HumanRequest 和 action snapshot。`delegate_agent` 遇到 child `waiting_human` 时写入 suspended tool call、blocked_by 和 DelegationJoinState。`generateNativeDeepSeek` 每次 tool call 后检查 collector，如果 `HasInterrupt()`，则短路返回，不再发第二次 model call。`RunAgent` 再把 collector 的 `Interrupt()` 写入 `TurnResponse.Interrupt` / `TurnResponse.HumanRequests`，并设置 `Status = waiting_human`。

这比让 `executeToolCall` 或 `delegate_agent` 直接修改 `TurnResponse` 更干净，因为 native DeepSeek、ADK adapter、未来 Flow Kernel 都可以共享同一个 suspend collector / store。

## 状态与统计边界

`waiting_human` 是一个已结束的 turn，不是一个仍在运行的 goroutine。

v0 约定：

- `TurnResponse.Status = waiting_human`。
- `EndedAt` 填充当前 turn 的结束时间。
- LLM usage 正常汇总并写入 usage ledger。
- run log 写入 `.xira/runs/<run_id>`。
- session history 正常写入，用于后续 native / ADK resume。
- verification 不应按 final answer 失败处理；可设为 `skipped` 或 `waiting_human`。
- evolution candidate 不应因为 `waiting_human` 自动生成。

这能避免 `waiting_human` 污染 failed run 统计，也避免 usage ledger 丢失本 turn 已经发生的模型调用。

## Runtime Control Tools 与 Allowlist

当前 builtin tool registry 按 profile `Permissions.Tools` allowlist 注册。runtime control tools 不完全等同于普通业务工具。

v0 实现方式：

```text
profile tools:
  read_file, command.run, ...

runtime control tools:
  always-on: human.request, emit_status
  policy-gated: delegate_agent
```

`human.request` 不放进普通 profile allowlist；它作为 always-on runtime control tool 注入 native / ADK tool surface。HumanResponse resolve 不是 tool surface，而是 API / CLI / transport-structured 写入口。

`delegate_agent` 也是 runtime control tool，但不能所有 agent 无条件可用。它由 caller profile 的 delegation policy 控制：

```yaml
delegation:
  enabled: true
  allow:
    - reviewer
  max_parallel: 3
  max_outstanding: 12
  max_depth: 2
```

约束：

- profile allowlist 仍然控制业务工具。
- runtime control tools 由 runtime 自己校验输入、scope、trust_level。
- 模型不能通过 `human.respond` 自行 resolve HumanRequest；所有 resolve 入口必须经过 HumanRequest scope/trust 校验。
- `delegate_agent` 不能因为是 runtime control tool 就绕过 delegation policy、target profile、max_depth、max_parallel、max_outstanding 和 child context policy。
- instruction 里的 “Available tools” 需要包含 runtime control tools，否则 agent 不知道可以调用。

## 去重与限流

agent 可以随时 `human.request`，所以 v0 必须避免重复追问。

规则：

- 同一 `scope`、同一 `requester.agent_id`、同一 `kind`、相同 `question_hash` 的 pending request 只能有一个。
- 如果重复创建，runtime 返回已有 `human_request_id`。
- 同一 request 被 denied 后，agent 再次提交相同 question，应返回 `denied_recently`，并建议 agent 结束或换问题。
- 一个 run 内最多创建一个 pending HumanRequest；创建后短路当前 run。

native 路径目前固定两次 model call，这个限制可以避免一个 turn 内反复问人。

代价也要明确：

- v0 的 `agent_request` 适合“单 tool turn”：agent 发现需要问人，就创建 HumanRequest 并结束当前 run。
- 如果同一次模型输出里同时包含 `read_file` / `search_file` 和 `human.request`，runtime 可能已经执行了前面的 read-only tool，但一旦出现 pending HumanRequest 就会短路当前 run。
- 这些已执行 tool 的结果要靠 session hydration 在后续 turn 回灌；不要期待同一个 native turn 继续把这些结果综合进“怎么问”。
- 更完整的多 tool + ask + continue 需要未来的 agentic loop / multi-turn tool loop，不放进 HITL v0。

## 跨 Turn 状态恢复

RuntimeSuspendCollector 只解决当前 run 短路。跨 turn resume 必须走 file store。

v0 resume 数据流：

```text
RunAgent start
  -> allocate session
  -> load pending HumanRequests by session_id / agent_id
  -> load recently resolved HumanResponses
  -> load replay results by session_id / agent_id
  -> load pending DelegationJoinState by parent_run_id / session_id
  -> if resume_human_request_id:
       load request
       validate response
       attach request/response/replay result summary
  -> if resume_delegation_join_id:
       load join state
       attach materialized child outputs in original parent tool-call order
  -> repair path:
       if an approved runtime_tool_gate request has replay_status=pending:
         try claim pending -> running, then executeReplay
       if replay_status=running and lease expired:
         mark stale attempt recoverable, then try claim again
  -> inject compact summary into model context
```

正常路径下，`runtime_tool_gate` replay 由 `POST /human-requests/{id}/responses` 同步触发。`RunAgent start` 里的 replay 只作为恢复性补偿，用于处理 response 写入成功但 replay 因进程崩溃、进程退出或 running lease 过期未完成的状态。未过期的 `running` 不能被补偿路径重复执行。

child delegation 的 resume 数据流：

```text
parent run waiting_human because child waiting_human
  -> user resolves child HumanRequest
  -> resume child run with child session/context packet
  -> child reaches completed/failed/timeout
  -> runtime materializes delegate_agent tool output
  -> attach output to parent tool call
  -> resume parent run with child result summary
```

child HumanRequest 被 deny/cancel 时：

```text
parent run waiting_human because child waiting_human
  -> user denies/cancels child HumanRequest
  -> child run becomes failed with error_type=canceled
  -> runtime materializes canceled delegate_agent tool output
  -> update DelegationJoinState
  -> if join complete, resume parent with all child outputs
```

已完成的 sibling child results 必须保留；如果 parent 同一批启动多个 child，只恢复仍在 waiting 的 child，最后按原 parent tool-call batch 顺序把所有 child outputs 回灌给 parent。

并发要求：

- HumanRequest resolve 必须原子写。
- `runtime_tool_gate` replay 必须通过 CAS 抢占 `pending -> running`，最多一个执行者。
- `running` 必须有 `replay_attempt_id`、`replay_started_at`、`replay_lease_expires_at`。
- replay result 写入 request state 和 run artifact。
- 如果 replay 失败，HumanRequest 不回到 pending，而是记录 `replay_failed`，让 agent/用户决定下一步。

## 落地顺序

建议拆成十一个小提交：

1. `workspace` canonicalization + internal `workspace_key` state root。
2. `humanrequest` 数据模型和 file store，包括 replay_status CAS / running lease。
3. 最小 response API：`POST /human-requests/{id}/responses` 原子写 HumanResponse / audit / AgentHistory 摘要，先不要求完整 CLI/UI。
4. runtime control tool：`human.request` + `TurnResponse.HumanRequests`；HumanResponse resolve 继续走第 3 步 response API。
5. `waiting_human` 状态、RuntimeSuspendCollector、RunInterrupt、session AgentHistory 摘要写入。
6. native / ADK 基础 hydration：回灌 HumanRequest/HumanResponse/replay result，确保后续 resume turn 有上下文。
7. delegation completed path：context packet、delegate_result_v1、parent/child lineage、completed/failed/timeout output materialize。
8. delegation continuation：DelegationJoinState、suspended delegate tool call、parent `blocked_by` metadata、delegate output summary hydration。
9. child waiting_human propagation：child resume、delegate output materialize、parent resume injection、join=all sibling output ordering。
10. native `RequireConfirmation` gate + action snapshot / env_snapshot + synchronous snapshot replay。
11. API list/show + CLI list/show/approve/deny/answer + tests。

第 3 步只要求最小 response 写入口，因为第 9 步的 child resume 和第 10 步的 snapshot replay 都从 response resolve 开始。CLI 包装、list/show 和更完整的展示可以晚到第 11 步，但 `POST /human-requests/{id}/responses` 不能晚于 child waiting propagation / replay 主路径。

第一阶段可以只做 file store，不需要数据库：

```text
.xira/state/workspaces/<workspace_key>/human-requests/<human_request_id>.yaml
.xira/state/workspaces/<workspace_key>/human-responses/<human_response_id>.yaml
.xira/state/workspaces/<workspace_key>/replay-results/<human_request_id>.yaml
.xira/runs/<run_id>/human_requests.jsonl
.xira/runs/<run_id>/replay/tool_replay_links.jsonl
.xira/runs/<parent_run_id>/delegations/<delegation_join_id>.yaml
.xira/runs/<parent_run_id>/delegations/<parent_tool_call_id>.state.yaml
.xira/runs/<parent_run_id>/delegations/<parent_tool_call_id>.output.json
.xira/runs/<child_run_id>/artifacts/context/context_packet.json
```

第二阶段再考虑：

- UI inbox。
- policy 配置文件。
- flow step scope。

## 测试策略

必须覆盖：

- agent 主动调用 `human.request` 会生成 pending HumanRequest。
- 所有 agent profile 默认都能看到 `human.request`，且默认看不到 `human.respond`。
- `delegate_agent` 只在 caller delegation policy 允许时可用；未允许的 target agent 会被拒绝并记录 audit。
- child agent 默认只看到 task、canonical workspace、parent lineage、当前用户消息摘要和显式授权的 `context_refs`，看不到 parent 完整 session history。
- child context packet 会记录 included / redacted / truncated context item，且 context refs 只能解析 parent run 允许的 artifact/tool output。
- child 返回 `delegate_result_v1` 后，runtime 补齐 `agent_id`、`run_id`、`status`，并拒绝 child 伪造 runtime-owned 字段。
- runtime 只校验 child result 的 schema/ref/runtime-owned fields，不校验 summary 真伪；`confidence` / `followup_needed` 仅作为提示字段传给 parent。
- parent 逻辑等待 child 的 delegate tool output；child completed/failed/timeout 都 materialize 成 parent 可见的 tool output。
- child `waiting_human` 会 suspend parent delegate tool call，让 parent run 进入 `waiting_human`，并记录 blocked_by child run / child HumanRequest / parent tool call / delegation_join_id。
- suspended delegate tool call 会保存 parent_tool_call_id、delegation_join_id、child_run_id、child_human_request_id、context_packet_ref 和 output_ref。
- child HumanRequest resolve 后，runtime 先 resume child，再 materialize delegate output，再 resume parent。
- child HumanRequest deny/cancel 后，child run 记录 failed + `error_type=canceled`，runtime 会 materialize `status=canceled` delegate output，并在 join complete 后恢复 parent。
- 同一 parent tool-call batch 中多个 `delegate_agent` 可以并行 fan-out，v0 join=all；active child execution 受 `max_parallel` 限制，suspended waiting child 不占 active slot 但占 `max_outstanding`。
- child 从 `waiting_human` resume 时必须重新获取 active slot；如果 `max_parallel` 已满，child 进入 `resume_pending`，不能绕过并发限制。
- DelegationJoinState 会持久化 completed child outputs 和 pending child HumanRequest；parent resume 时按原 tool call 顺序注入所有 delegate outputs。
- `max_duration_ms` 只计算 child active execution time，不计算 child `waiting_human` 的人类等待时间。
- Task/Todo tool 不参与 v0 delegation；直接 `delegate_agent -> child run` 即可。
- native 路径执行 `RequireConfirmation=true` 的 tool 时不执行原 tool，而是生成 `runtime_tool_gate` HumanRequest 和 action snapshot。
- HumanRequest 记录 canonical `workspace`，file store 使用内部 `workspace_key` 分目录，不使用 `workspace_id` 或 `local` 这类弱身份。
- ADK / native 对 `RequireConfirmation` 的可见行为一致。
- ADK hydration 能看到 HumanRequest、HumanResponse 和 replay result 摘要。
- API/CLI resolve 后写入 HumanRequest store、run audit，并写 session AgentHistory 摘要；后续 agent turn 能看到该摘要。
- 结构化 approve 后 runtime 按 action snapshot 重放，且不会让 agent 重新构造 tool input。
- `POST /human-requests/{id}/responses` 对强制型 approve 同步触发 replay 并返回 replay status/result ref。
- 并发 `POST /human-requests/{id}/responses` 只有一个请求能 CAS 抢占 `pending -> running` 并执行 replay，其他请求返回 `running` 或已有结果。
- HTTP 超时后的重试不会在未过期 `running` lease 上重复执行 replay。
- replay 执行使用 `replay=true`，不会二次触发 `RequireConfirmation`。
- replay 环境漂移检测使用 `env_hash`，默认包含 canonical workspace、cwd、git HEAD；`env_hash` 不声明防篡改。
- 同一个 action snapshot 最多 replay 一次。
- 原始 ToolCallRecord 能通过 materialized `replay_ref` 看到 replay result，不需要审计者手动拼多个文件。
- channel `router_structured` 默认不能 resolve `runtime_tool_gate` approval，除非 per-channel 显式开启。
- native hydration 只注入最新 K 条消息 / max_history_chars + HITL 摘要，不无界回灌 session。
- native v0 对 `agent_request` 以单 pending tool turn 为主；多 tool + ask 的组合可能先执行部分 read-only tool，再因 pending request 短路，不能期待同一 turn 内继续综合这些结果。
- 用户说“没问题干吧”时，agent 可以继续追问或提示用户使用结构化 approve；不能通过模型可见 `human.respond` 自行 resolve。
- 强制型 `runtime_tool_gate` approval 只能由 API / CLI / UI button 或显式开启 strong approval 的 channel/router 结构化 response resolve。
- 多个 pending request 时，agent 不应该直接 resolve，而应该追问。
- 不明确回复不能直接 resolve 为 approved。
- `response.signal=approve` 会把 HumanRequest 改成 approved。
- `response.signal=deny` 会把 HumanRequest 改成 denied。
- run log 和 audit 中能看到 request 和 response。
- waiting_human run 填充 EndedAt，写 usage ledger，不生成 failure evolution candidate。
- 相同 session 的后续 native turn 能看到已 resolved HumanRequest、pending HumanRequest 和 replay result 摘要。
- 重复 question 会返回已有 HumanRequest，连续 deny 后不会无限追问。

## 已关闭决策

```text
HITL state root:
  使用 state_root/workspaces/<workspace_key>/...
  不并入 run root 或 session root。

API/CLI resolve:
  必须写 HumanRequest store。
  必须写 run audit / run summary。
  必须写 session AgentHistory 摘要，供后续 native / ADK hydration。

replay_ttl:
  workspace policy 可配置。
  未配置时默认 15m。

channel strong approval:
  放 entrypoint config。
  每个 channel / entrypoint 显式开启。
  workspace policy 不默认打开 channel strong approval。

child waiting_human:
  是正确 delegation 语义的一部分，本次 v0 需要实现。
  child waiting_human suspends parent delegate tool call。
  parent 通过 DelegationJoinState 持久化等待，不挂 goroutine。
  这是因为 delegate_agent 启动的 child 是真实 run，不是同步函数；child HumanRequest 不能从 parent / Flow 视角消失。
  这也是 v0 最大的 runtime 改造点，需要和 hydration / parent tool output injection 一起实现。

Task/Todo:
  v0 不做 Task/Todo tool。
  Task/Todo 是未来 planning state，不是 delegation execution state。
```

## 推荐下一步

先收敛最小 Agent runtime boundary：

```text
delegate_agent child context packet
delegate_result_v1
DelegationJoinState
suspended delegate tool call
parent/child continuation
parallel child fan-out + join=all
child waiting_human blocks parent
child HumanRequest deny/cancel materializes canceled delegate output
human.request tool
HumanRequest file store
workspace canonicalization
native/ADK HITL hydration
RequireConfirmation gate
synchronous snapshot replay with CAS
TurnResponse.human_requests
CLI/API response resolve
waiting_human 状态传播
```

不要先做完整 policy，也不要先接 Flow。完成后再回到 Flow schema，把 flow 的 `human_approval`、agent-generated HumanRequest、agent delegation child run 的关系补清楚。
