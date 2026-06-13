# Xira Agent Human-in-the-loop v0 设计草案

> 状态：草案，供人和 AI review。
> 分支：`feature/agent-hitl-v0`
> 目标：先定义 agent-first 场景下的人类介入机制，再让 Flow Run Kernel 复用同一套能力。

## 摘要

Human-in-the-loop，简称 HITL，是 Xira runtime 的基础能力，不是 Flow 独占能力。

它解决的问题是：

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

Agent 可以主动创建 HumanRequest。
Runtime 必须在 ToolPolicy.RequireConfirmation 为 true 时强制创建 HumanRequest。
Flow 未来复用同一套 HumanRequest，只是 scope 指向 flow_run + step。
```

review 后修正的核心边界：

```text
协作型 HITL：
  agent 主动 human.request / human.respond。
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
- 不实现跨进程自动继续执行复杂 agent loop。
- 不把 HITL 设计成只服务审批；它也服务澄清、选择和风险门禁。

## 核心概念

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

HumanResponse 可以来自两种入口：

- 结构化入口：CLI / API 直接提交 `approve`、`deny`、`answer`、`choose`。
- 普通消息入口：用户说“没问题干吧”“先别动”“按方案 A”，agent 理解后调用 `human.respond`，runtime 校验状态并落库。

两种入口的信任级别不同：

| 来源 | 信任级别 | 可用于强制型 approval |
| --- | --- | --- |
| CLI / API / UI button | 高，用户身份由 transport 层确认 | 可以 |
| channel 结构化 shortcut，且由 channel/router 解析成 HumanResponse | 中到高，取决于 channel 认证 | 可以配置为可以 |
| agent 解释自然语言后调用 `human.respond` | 低，语义解释由 LLM 完成 | v0 默认不可以 |

`HumanResponse.signal` 常见值：

- `approve`
- `deny`
- `answer`
- `choose`
- `cancel`

HumanResponse 不直接执行动作。它只是改变 HumanRequest 状态，并为后续 agent run / tool retry / snapshot replay 提供依据。

普通消息入口里，agent 不能只在最终回复里说“用户同意了”。它必须调用 runtime tool，把解释结果变成可审计的状态变更。

但 `human.respond` 只能证明“某个 agent 把某条用户消息解释成了某个 HumanResponse”，不能证明用户语义上一定批准了风险动作。强制型 approval 必须依赖结构化 HumanResponse，或者依赖 transport/router 直接解析出来的结构化 HumanResponse。

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
  input_hash: sha256:...
  input:
    program: git
    args:
      - push
      - -u
      - origin
      - HEAD
    cwd: /Users/yinwm/work/flowdeck
  replay_policy: runtime_snapshot_replay
  replay_status: pending

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
interpreter:
  type: agent
  id: xira-assistant
source:
  type: user_message
  message_id: msg_20260613_001
  text_preview: 没问题干吧
reason: 用户明确允许继续执行当前 pending request。
created_at: 2026-06-13T22:35:00+08:00
```

结构化 API / CLI 入口可以没有 `interpreter`，因为 response 是用户直接提交的。普通消息入口必须记录 `interpreter`，因为 agent 在这里负责把自然语言解释成 `HumanResponse.signal`。

## Scope 设计

HITL 是 runtime 基础能力，所以 scope 必须能覆盖 agent-only 和未来 flow。

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

### 2. Agent 提交 HumanResponse

用户不需要必须使用 `/approve` 这类命令。用户可以用自然语言回复 pending HumanRequest：

```text
没问题干吧
可以
先别动
按方案 A
这个不要做
```

agent 看到 session 里有 pending HumanRequest 后，负责判断这条普通消息是不是对某个 request 的回复。

如果能明确判断，agent 调用 runtime tool：

```text
human.respond
```

示例：

```json
{
  "human_request_id": "hr_20260613_001",
  "signal": "approve",
  "source": "user_message",
  "source_message_id": "msg_20260613_001",
  "source_text": "没问题干吧",
  "reason": "用户明确允许继续执行当前 pending request。"
}
```

runtime 负责校验：

- request 必须存在且状态是 `pending`。
- request 必须属于当前 session / actor 可见范围。
- `response.signal` 必须符合 request kind 和 options。
- source message 必须来自当前 inbound user message，或来自 transport/router 提供的用户消息引用；不接受 agent 任意填写历史 message id 作为高信任来源。
- 同一个 request 只能 resolve 一次。

但 runtime 不能校验 LLM 对自然语言的语义理解是否正确。因此 v0 对 `human.respond` 做信任分级：

- 可以 resolve `clarification` / `choice`。
- 可以 resolve `request_source=agent_request` 的低风险 approval。
- 默认不能 resolve `request_source=runtime_tool_gate` 的强制型 approval。

强制型 approval 必须走 CLI / API / UI button，或走 channel/router 直接解析出的结构化 HumanResponse。也就是说，“没问题干吧”可以成为好的交互体验，但不能默认成为高风险 tool gate 的唯一安全依据。

如果当前 session 只有一个 pending request，`没问题干吧` 可以被解释为对它的 `approve`。

如果有多个 pending request，agent 应该追问用户指的是哪一个，而不是猜。

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
| `human.respond` | 所有 agent 可用，但按信任级别限制 resolve 范围 |
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
7. agent 通过后续普通 turn 读取 HumanResponse，或者通过 `human.respond` 将用户普通消息 resolve 成 HumanResponse。

这类 approval 是协作型确认，不提供“批准什么就一定执行什么”的保证。

当 `RequireConfirmation` gate 拦截 tool call：

1. `executeToolCall` 创建 `HumanRequest(kind: approval, request_source: runtime_tool_gate)`。
2. runtime 保存 `action_snapshot`，包括 tool name、tool call id、input、input hash、cwd、run id。
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
       compacted prior user/assistant/tool messages
       human request/response/replay summary
       current user message
```

同时，`waiting_human` run 也要写入 session history，至少包含：

- 用户原始请求。
- 被挂起的 HumanRequest 摘要。
- 如果是 `runtime_tool_gate`，保存 tool call input 快照引用。
- runtime 给用户的 waiting message。

没有 native session hydration 前，v0 只能可靠支持 `clarification` / `choice` 这类纯问答；不能声称支持 approval resume。

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
  -> runtime 检查 snapshot 未过期、未重放、scope 匹配
  -> runtime 执行 action_snapshot(A)
  -> 保存 replay result
  -> 后续 turn 把 replay result 注入 agent 上下文
```

理由：

- 用户批准的是 A，就只能执行 A。
- agent 不能在 approve 后重新生成一个 B。
- snapshot 过期是数据问题，可以用 TTL、cwd、workspace revision、input hash 检查发现。
- 重新构造命令导致“批准 A 执行 B”是安全问题，runtime 很难检测。

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

这些消息先作为普通消息进入 runtime router。runtime 给 agent 注入当前 session 的 pending HumanRequest 摘要，agent 判断这条消息是否在回答某个 request。

如果判断明确，agent 可以调用 `human.respond`。如果判断不明确，agent 继续追问。

注意：自然语言 response 是低信任路径，默认只能 resolve 澄清、选择、协作型 approval。对 `request_source=runtime_tool_gate` 的强制型 approval，v0 需要结构化 HumanResponse。

Channel slash command 只是可选快捷写法：

```text
/approve hr_20260613_001
/deny hr_20260613_001
/answer hr_20260613_002 repo 是 /Users/yinwm/work/flowdeck
```

v0 决策：channel 里的自然语言回复先作为普通消息进入 runtime router。`/approve` / `/deny` / `/answer` 可以先作为普通消息进入 runtime router；如果要用于强制型 approval，必须由 channel/router 解析成结构化 HumanResponse 并带上 transport 层用户身份。

原因：

- channel runner 保持薄，只做消息归一化。
- agent 可以看到用户的自然语言上下文，不局限于机械命令。
- 与“agent_request 由 agent 继续、runtime_tool_gate 由 snapshot replay”一致。
- 未来如果 UI 需要秒级审批 inbox，再增加专门 API 或 channel shortcut。

自然语言 response 路径：

```text
用户消息："没问题干吧"
  -> runtime router 接收普通消息
  -> runtime 注入 pending HumanRequest 摘要
  -> agent 判断这是 approve
  -> agent 调用 human.respond
  -> runtime 校验 session / actor / request 状态与信任级别
  -> 如果 request 允许低信任 response，HumanRequest.status = approved
  -> 如果 request 是强制型 approval，runtime 拒绝低信任 response 并要求结构化 approve
  -> run log 和 audit 记录原始用户消息、解释结果和 trust_level
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

## Runtime 状态变化

`TurnResponse` 增加：

```go
HumanRequests []HumanRequest `json:"human_requests,omitempty" yaml:"human_requests,omitempty"`
```

新增 run status：

```text
waiting_human
```

HumanRequest 也写入：

```text
.xira/state/human-requests/
.xira/state/human-responses/
```

或：

```text
.xira/human-requests/
```

建议先放在 state root：

```text
.xira/state/human-requests/<human_request_id>.yaml
.xira/state/human-responses/<human_response_id>.yaml
```

run log 继续保存摘要：

```text
.xira/runs/<run_id>/human_requests.jsonl
.xira/runs/<run_id>/human_responses.jsonl
```

## Audit / Event

每个 HumanRequest 和 HumanResponse 都应该有 event 和 audit。

Runtime events：

```text
human.request.created
human.response.created
human.request.resolved
human.request.expired
```

Audit events：

```text
human.request
human.response
tool.approval_required
```

要求：

- audit 不一定保存完整敏感内容。
- 必须保存足够复盘的信息：谁问、问什么、属于哪个 run、谁批准、什么时候批准。
- 自然语言 response 必须保存原始用户消息引用，例如 `source_message_id`、`source_text_preview`、`interpreted_by_agent_id`。
- audit 中要区分 `actor` 和 `interpreter`：批准者是用户，解释者是 agent。
- HumanResponse 必须记录 `trust_level`：`transport_authenticated`、`router_structured` 或 `agent_interpreted`。
- runtime snapshot replay 必须记录 action snapshot hash、replay status、replay output ref、过期/拒绝原因。

## Policy v0

v0 不做完整 policy engine，也不做 `command.run` 风险分类器。

第一版只保留最小运行时事实：

```text
human.request -> allow for all agents
human.respond -> allow for all agents, but trust-limited
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
human.respond -> 所有 agent 默认可用，但默认是低信任 agent_interpreted response
write_file/edit_file -> v0 不默认 ask
command.run -> v0 不做内置风险分类器，但执行 ToolPolicy.RequireConfirmation
runtime_tool_gate approval -> snapshot + runtime replay
agent_request approval -> agent 后续 turn 继续，不提供执行一致性保证
channel 自然语言回复 -> 普通消息 + 低信任 human.respond
CLI/API/UI/结构化 channel response -> 可用于强制型 approval
native DeepSeek -> v0 必须实现 session history / HumanRequest / replay result 回灌
state root -> 暂定 .xira/state/human-requests
```

`state root` 仍然保留为轻度待验证项，因为后续如果 session store / run store 已经有统一 state root，应该跟随现有目录结构。

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

## 与 Flow 的关系

Flow 不重新实现 HITL。

未来 Flow 使用同一套 HumanRequest：

```yaml
scope:
  type: flow_step
  id: fr_20260613_devrun_001:approve_merge
```

两种情况：

1. Flow 显式 `human_approval` step 创建 HumanRequest。
2. Flow 的 agent step 内部触发 HumanRequest，Flow Kernel 暂停当前 step。

所以 Agent HITL 是 Flow HITL 的前置能力。

## 第一版实现边界

v0 推荐先实现：

1. `internal/hitl` 包：
   - types
   - file store
   - list/get/create/resolve
   - action snapshot
   - replay result
2. `human.request` / `human.respond` 内置 tool。
3. `TurnResponse.HumanRequests`。
4. `waiting_human` run status。
5. native session hydration：回灌 history、pending/resolved HumanRequest、replay result。
6. native `RequireConfirmation` gate。
7. snapshot replay for `runtime_tool_gate` approval。
8. API list/show/response。
9. CLI list/show/approve/deny/answer。
10. tests。

v0 暂不实现：

- UI approval panel。
- 自动恢复同一个 goroutine。
- flow scope。
- 多人审批。
- 超时自动处理。
- 复杂 policy DSL。
- 通用命令风险分类器。

## 现有代码挂点

当前 repo 已经有几个可以直接落地的挂点：

| 文件 | 现状 | HITL v0 改动 |
| --- | --- | --- |
| `apps/xira/internal/runtime/types.go` | `TurnRequest` / `TurnResponse` 已经是 run 的外部协议 | 给 `TurnResponse` 增加 `HumanRequests []hitl.HumanRequest`，并允许 `Status = waiting_human` |
| `apps/xira/internal/tools/registry.go` | tool 定义已经有 `ToolPolicy{Risk, RequireConfirmation}` | 不扩展成命令风险分类器；v0 直接消费 `RequireConfirmation` |
| `apps/xira/internal/runtime/service.go` | `RunAgent` 创建 run、调用 `generate`、最后统一设置 completed/failed | 增加 waiting_human 分支；`waiting_human` 不算 failed，也不写成 completed |
| `apps/xira/internal/runtime/service.go` | `generateNativeDeepSeek` 每次从 `{system, req.Message}` 重建 messages | 增加 native session hydration，把 AgentHistory、HumanRequest summary、replay result 注入 messages |
| `apps/xira/internal/runtime/service.go` | `executeToolCall` 是 native DeepSeek 路径的 tool 执行入口 | v0 支持 `human.request` / `human.respond`，并在执行普通 tool 前检查 `RequireConfirmation` |
| `apps/xira/internal/runtime/service_adk.go` | ADK tool adapter 已接 `RequireConfirmationProvider` | v0 不依赖 ADK confirmation 做持久化；HumanRequest 的创建、保存、resolve、snapshot replay 由 Xira runtime 负责 |
| `apps/xira/internal/api` | 已有 run/status 类 HTTP API | 增加 human request list/show/response API |
| `apps/xira/internal/cli` 或现有 CLI 命令入口 | 已有本地操作入口 | 增加 `xira human list/show/approve/deny/answer` |

一个重要约束：

```text
HITL 不能只是返回一个普通 tool output。

因为 native DeepSeek 路径里，tool output 之后当前实现会继续发第二次 model call。
如果已经创建 pending HumanRequest，runtime 必须能把这个状态带回 RunAgent，
让最终 run.status = waiting_human，而不是 completed 或 failed。

同时，HITL 也不能只做当前 run 内 collector。跨 turn resume 必须从 file store 重新加载 pending/resolved HumanRequest、action snapshot 和 replay result，并注入下一次 native model call。
```

建议实现一个 run 内 collector：

```go
type HumanRequestCollector interface {
    Add(hitl.HumanRequest)
    Pending() []hitl.HumanRequest
    HasPending() bool
}
```

`RunAgent` 创建 collector 并放入 context。`human.request` tool 创建 HumanRequest 后写入 collector，返回一个 `waiting_human` tool output。`human.respond` tool resolve HumanRequest 后写入 event / audit。`executeToolCall` 遇到 `RequireConfirmation` 时创建强制型 HumanRequest 和 action snapshot。`generateNativeDeepSeek` 每次 tool call 后检查 collector，如果已有 pending request，则短路返回。`RunAgent` 再把 collector 内容写入 `TurnResponse.HumanRequests`，并设置 `Status = waiting_human`。

这比让 `executeToolCall` 直接修改 `TurnResponse` 更干净，因为 native DeepSeek、ADK adapter、未来 Flow Kernel 都可以共享同一个 HITL collector / store。

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

当前 builtin tool registry 按 profile `Permissions.Tools` allowlist 注册。`human.request` / `human.respond` 又要求所有 agent 默认可用。

v0 实现方式：

```text
profile tools:
  read_file, command.run, ...

runtime control tools:
  human.request, human.respond, emit_status, delegate_agent ...
```

`human.request` / `human.respond` 不放进普通 profile allowlist；它们作为 runtime control tools 注入 native / ADK tool surface。

约束：

- profile allowlist 仍然控制业务工具。
- runtime control tools 由 runtime 自己校验输入、scope、trust_level。
- `human.respond` 不能因为“所有 agent 可见”就绕过 HumanRequest scope/trust 校验。
- instruction 里的 “Available tools” 需要包含 runtime control tools，否则 agent 不知道可以调用。

## 去重与限流

agent 可以随时 `human.request`，所以 v0 必须避免重复追问。

规则：

- 同一 `scope`、同一 `requester.agent_id`、同一 `kind`、相同 `question_hash` 的 pending request 只能有一个。
- 如果重复创建，runtime 返回已有 `human_request_id`。
- 同一 request 被 denied 后，agent 再次提交相同 question，应返回 `denied_recently`，并建议 agent 结束或换问题。
- 一个 run 内最多创建一个 pending HumanRequest；创建后短路当前 run。

native 路径目前固定两次 model call，这个限制可以避免一个 turn 内反复问人。

## 跨 Turn 状态恢复

run 内 collector 只解决当前 run 短路。跨 turn resume 必须走 file store。

v0 resume 数据流：

```text
RunAgent start
  -> allocate session
  -> load pending HumanRequests by session_id / agent_id
  -> load recently resolved HumanResponses
  -> if resume_human_request_id:
       load request
       validate response
       if request_source=runtime_tool_gate and approved:
         replay action_snapshot if not replayed
         persist replay result
  -> inject compact summary into model context
```

并发要求：

- HumanRequest resolve 必须原子写。
- `runtime_tool_gate` replay 必须最多执行一次。
- replay result 写入 request state 和 run artifact。
- 如果 replay 失败，HumanRequest 不回到 pending，而是记录 `replay_failed`，让 agent/用户决定下一步。

## 落地顺序

建议拆成六个小提交：

1. `hitl` 数据模型和 file store。
2. runtime control tools：`human.request` / `human.respond` + `TurnResponse.HumanRequests`。
3. `waiting_human` 状态、session history 写入、native session hydration。
4. native `RequireConfirmation` gate + action snapshot。
5. structured response resolve + snapshot replay。
6. API / CLI response resolve。

第一阶段可以只做 file store，不需要数据库：

```text
.xira/state/human-requests/<human_request_id>.yaml
.xira/runs/<run_id>/human_requests.jsonl
```

第二阶段再考虑：

- UI inbox。
- policy 配置文件。
- flow step scope。

## 测试策略

必须覆盖：

- agent 主动调用 `human.request` 会生成 pending HumanRequest。
- 所有 agent profile 默认都能看到 `human.request` 和 `human.respond`。
- native 路径执行 `RequireConfirmation=true` 的 tool 时不执行原 tool，而是生成 `runtime_tool_gate` HumanRequest 和 action snapshot。
- ADK / native 对 `RequireConfirmation` 的可见行为一致。
- 结构化 approve 后 runtime 按 action snapshot 重放，且不会让 agent 重新构造 tool input。
- 同一个 action snapshot 最多 replay 一次。
- 用户说“没问题干吧”时，agent 可以通过 `human.respond` 把唯一低信任 pending request resolve 为 approved。
- 低信任 `human.respond` 不能 resolve `runtime_tool_gate` approval。
- 多个 pending request 时，agent 不应该直接 resolve，而应该追问。
- 不明确回复不能直接 resolve 为 approved。
- `response.signal=approve` 会把 HumanRequest 改成 approved。
- `response.signal=deny` 会把 HumanRequest 改成 denied。
- run log 和 audit 中能看到 request 和 response。
- waiting_human run 填充 EndedAt，写 usage ledger，不生成 failure evolution candidate。
- 相同 session 的后续 native turn 能看到已 resolved HumanRequest、pending HumanRequest 和 replay result 摘要。
- 重复 question 会返回已有 HumanRequest，连续 deny 后不会无限追问。

## 待确认问题

1. `.xira/state/human-requests` 是否和现有 run/session state root 完全一致？
2. API/CLI resolve 后是否也要写入 session memory，还是只写 HumanRequest store 和 run audit？
3. channel slash shortcut 是否需要一个轻量 parser，把 `/approve hr_xxx` 转成结构化 HumanResponse，从而可用于强制型 approval？
4. snapshot replay 的默认 TTL 是多少？是否需要记录 workspace revision / git HEAD？
5. replay result 是否立即触发 agent resume，还是等下一条用户消息触发？

## 推荐下一步

先实现最小 Agent HITL：

```text
human.request tool
human.respond tool
HumanRequest file store
native session hydration
RequireConfirmation gate
snapshot replay
TurnResponse.human_requests
CLI/API response resolve
waiting_human 状态传播
```

不要先做完整 policy，也不要先接 Flow。完成后再回到 Flow schema，把 flow 的 `human_approval` 和 agent-generated HumanRequest 的关系补清楚。
