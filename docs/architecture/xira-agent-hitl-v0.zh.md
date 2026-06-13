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
Runtime 可以在 tool / command 执行前强制创建 HumanRequest。
Flow 未来复用同一套 HumanRequest，只是 scope 指向 flow_run + step。
```

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
  -> 用户发送 signal
  -> runtime 保存 signal
  -> 后续 agent run / resume turn 读取这个结果继续
```

HumanRequest 的核心职责：

- 告诉用户现在需要决定什么。
- 记录它属于哪个 agent run / session。
- 保存上下文、选项、状态和审计信息。
- 接收用户 signal。
- 让后续 resume 能找到人类决定。

### HumanSignal

HumanSignal 是用户对 HumanRequest 的回复。

常见 signal：

- `approve`
- `deny`
- `answer`
- `choose`
- `cancel`

HumanSignal 不直接执行动作。它只是改变 HumanRequest 状态，并为后续 agent run / tool retry 提供依据。

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
- scope
- requester
- question/reason
- context
- options
- timestamps
- resolution

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

### 2. Runtime 强制拦截

runtime 可以在执行 tool 前做 policy check，但 v0 不把“命令风险分类器”作为核心能力。

v0 决策：不要在第一版内置 `command.run` 风险分类器。

原因：

- 命令风险不可能靠固定列表列全。
- 同一个命令在不同 repo、不同 cwd、不同客户环境里的风险不同。
- 过早做分类器会制造一种假的确定性。

v0 更合理的边界是：

| 能力 | v0 默认 |
| --- | --- |
| `human.request` | 所有 agent 可用 |
| `read_file` / `search_file` / `list_dir` | 不问 |
| `write_file` / `edit_file` | 不默认 ask |
| `tool_output.read` | 不问 |
| `command.run` / `shell.run` | 不做内置风险分类 |

也就是说，v0 的核心不是“runtime 猜命令风险”，而是先把 HumanRequest / HumanSignal / waiting_human / audit 链路做出来。

如果某个业务或交付环境需要更确定的边界，应该通过 agent profile、workspace policy、flow step instructions、客户环境配置来声明，而不是把命令列表硬编码在 HITL core 里。

## Tool Approval v0 行为

当 tool 需要人工确认：

1. `executeToolCall` 创建 HumanRequest。
2. 不执行原 tool。
3. ToolCallRecord 记录为 waiting human。
4. agent run 状态变成 `waiting_human`。
5. TurnResponse 返回 `human_requests`。
6. 用户 approve / deny。
7. 后续 resume turn 或 agent retry 读取该 signal。

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

POST /human-requests/{id}/signals
  -> status = approved / denied / answered

POST /agent-runs with same session_id and metadata.resume_human_request_id
  -> runtime 把 HumanSignal 注入上下文
  -> agent 继续
```

后续可以增加：

```text
POST /agent-runs/{run_id}/resume
```

但 v0 不必强行恢复同一个 goroutine 或同一个 model call。

### approve 后是否自动重放 tool call

这里的“自动重放”指：

```text
agent 调用 command.run
  -> runtime 拦截并创建 HumanRequest
  -> 用户 approve
  -> runtime 不再问 agent，直接拿刚才那份 tool call input 执行一次
```

另一个方案是：

```text
agent 调用 command.run
  -> runtime 拦截并创建 HumanRequest
  -> 用户 approve
  -> 后续 turn 把 approve signal 放进上下文
  -> agent 自己决定是否重新调用 command.run
```

v0 先选第二种：不自动重放，由 agent 在后续 turn 里自己继续。

原因：

- 更符合 agent-first：批准只是一个 signal，不等于 runtime 接管执行策略。
- 被拦截的 tool input 可能已经过期，例如 cwd、branch、文件状态、用户意图发生变化。
- `/approve hr_xxx` 作为普通消息进入 runtime router 后，也天然适合让 agent 读到 signal 再继续。
- 自动重放可以作为未来能力，但需要更严格的幂等、过期检查和 tool input 快照。

## 用户交互

CLI：

```bash
xira human list
xira human show hr_20260613_001
xira human approve hr_20260613_001
xira human deny hr_20260613_001
xira human answer hr_20260613_002 --message "repo 是 /Users/yinwm/work/flowdeck"
```

Channel slash command：

```text
/approve hr_20260613_001
/deny hr_20260613_001
/answer hr_20260613_002 repo 是 /Users/yinwm/work/flowdeck
```

v0 决策：channel 里的 `/approve` / `/deny` / `/answer` 先作为普通消息进入 runtime router，不让 channel runner 直接处理。

原因：

- channel runner 保持薄，只做消息归一化。
- agent 可以看到用户的自然语言上下文，不局限于机械命令。
- 与“approve 后不自动重放，由 agent 继续”一致。
- 未来如果 UI 需要秒级审批 inbox，再增加专门 API 或 channel shortcut。

API：

```text
GET  /api/v1/human-requests
GET  /api/v1/human-requests/{id}
POST /api/v1/human-requests/{id}/signals
```

Signal body：

```json
{
  "signal": "approve",
  "actor": "user-local",
  "message": "Allow once."
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
```

或：

```text
.xira/human-requests/
```

建议先放在 state root：

```text
.xira/state/human-requests/<human_request_id>.yaml
```

run log 继续保存摘要：

```text
.xira/runs/<run_id>/human_requests.jsonl
```

## Audit / Event

每个 HumanRequest 和 HumanSignal 都应该有 event 和 audit。

Runtime events：

```text
human.request.created
human.request.resolved
human.request.expired
```

Audit events：

```text
human.request
human.signal
tool.approval_required
```

要求：

- audit 不一定保存完整敏感内容。
- 必须保存足够复盘的信息：谁问、问什么、属于哪个 run、谁批准、什么时候批准。

## Policy v0

v0 不做完整 policy engine，也不做 `command.run` 风险分类器。

第一版只保留最小运行时事实：

```text
human.request -> allow for all agents
read_file/search_file/list_dir -> allow
write_file/edit_file -> allow by default
tool_output.read -> allow
command.run/shell.run -> no built-in classifier
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
write_file/edit_file -> v0 不默认 ask
command.run -> v0 不做内置风险分类器
approve 后 -> v0 不自动重放原 tool call，由 agent 在后续 turn 继续
channel /approve -> v0 作为普通消息进入 runtime router
state root -> 暂定 .xira/state/human-requests
```

`state root` 仍然保留为轻度待验证项，因为后续如果 session store / run store 已经有统一 state root，应该跟随现有目录结构。

## 与 ADK Tool Confirmation 的关系

当前 ADK adapter 已经有 `RequireConfirmationProvider` 接入点。

但 Xira 不能只依赖 ADK 的确认机制，因为：

- Xira 还要支持非 ADK agent engine。
- Xira 要把 request 持久化到 state store。
- Xira 要支持 channel / CLI / API signal。
- Xira 要生成自己的 audit 和 run log。

因此：

```text
ADK confirmation 可以作为 UI/adapter 层提示。
Xira HumanRequest 才是 runtime 权威状态。
```

实现时，`RequireConfirmationProvider` 可以先和 Xira policy 保持一致，但真正的创建、保存、resolve 应由 Xira runtime 完成。

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
2. `human.request` 内置 tool。
3. `TurnResponse.HumanRequests`。
4. `waiting_human` run status。
5. agent 主动创建 HumanRequest 的 run 状态传播。
6. API list/show/signal。
7. CLI list/show/approve/deny/answer。
8. tests。

v0 暂不实现：

- UI approval panel。
- 自动恢复同一个 goroutine。
- flow scope。
- 多人审批。
- 超时自动处理。
- 复杂 policy DSL。

## 现有代码挂点

当前 repo 已经有几个可以直接落地的挂点：

| 文件 | 现状 | HITL v0 改动 |
| --- | --- | --- |
| `apps/xira/internal/runtime/types.go` | `TurnRequest` / `TurnResponse` 已经是 run 的外部协议 | 给 `TurnResponse` 增加 `HumanRequests []hitl.HumanRequest`，并允许 `Status = waiting_human` |
| `apps/xira/internal/tools/registry.go` | tool 定义已经有 `ToolPolicy{Risk, RequireConfirmation}` | v0 暂不扩展成命令风险分类器；后续如需强制审批，由业务 policy 接入 |
| `apps/xira/internal/runtime/service.go` | `RunAgent` 创建 run、调用 `generate`、最后统一设置 completed/failed | 增加 waiting_human 分支：有 pending HumanRequest 时不算 failed，也不写成 completed |
| `apps/xira/internal/runtime/service.go` | `executeToolCall` 是 native DeepSeek 路径的 tool 执行入口 | v0 先支持 `human.request` 产生 pending request；后续业务 policy 可在这里接强制 gate |
| `apps/xira/internal/runtime/service_adk.go` | ADK tool adapter 已接 `RequireConfirmationProvider` | v0 不依赖 ADK confirmation；HumanRequest 的创建、保存、resolve 由 Xira runtime 负责 |
| `apps/xira/internal/api` | 已有 run/status 类 HTTP API | 增加 human request list/show/signal API |
| `apps/xira/internal/cli` 或现有 CLI 命令入口 | 已有本地操作入口 | 增加 `xira human list/show/approve/deny/answer` |

一个重要约束：

```text
HITL 不能只是返回一个普通 tool output。

因为 native DeepSeek 路径里，tool output 之后当前实现会继续发第二次 model call。
如果已经创建 pending HumanRequest，runtime 必须能把这个状态带回 RunAgent，
让最终 run.status = waiting_human，而不是 completed 或 failed。
```

建议实现一个 run 内 collector：

```go
type HumanRequestCollector interface {
    Add(hitl.HumanRequest)
    Pending() []hitl.HumanRequest
    HasPending() bool
}
```

`RunAgent` 创建 collector 并放入 context。`human.request` tool 创建 HumanRequest 后写入 collector，返回一个 `waiting_human` tool output。`generateNativeDeepSeek` 每次 tool call 后检查 collector，如果已有 pending request，则短路返回。`RunAgent` 再把 collector 内容写入 `TurnResponse.HumanRequests`，并设置 `Status = waiting_human`。

这比让 `executeToolCall` 直接修改 `TurnResponse` 更干净，因为 native DeepSeek、ADK adapter、未来 Flow Kernel 都可以共享同一个 HITL collector / store。

## 落地顺序

建议拆成四个小提交：

1. `hitl` 数据模型和 file store。
2. `human.request` 内置 tool + `TurnResponse.HumanRequests`。
3. agent 主动请求 + `waiting_human` 状态传播。
4. API / CLI signal resolve。

第一阶段可以只做 file store，不需要数据库：

```text
.xira/state/human-requests/<human_request_id>.yaml
.xira/runs/<run_id>/human_requests.jsonl
```

第二阶段再考虑：

- UI inbox。
- policy 配置文件。
- approve 后自动 replay 原 tool call。
- runtime 强制 tool gate。
- flow step scope。

## 测试策略

必须覆盖：

- agent 主动调用 `human.request` 会生成 pending HumanRequest。
- 所有 agent profile 默认都能看到 `human.request`。
- approve signal 会把 HumanRequest 改成 approved。
- deny signal 会把 HumanRequest 改成 denied。
- run log 和 audit 中能看到 request 和 signal。
- 相同 session 的后续 turn 能看到已 resolved 的 HumanRequest 摘要。

## 待确认问题

1. `.xira/state/human-requests` 是否和现有 run/session state root 完全一致？
2. `human.request` 的 signal 是否只通过后续普通 turn 注入，还是 API/CLI resolve 后也要写入 session memory？
3. channel 里的 `/approve hr_xxx` 是否需要一个轻量 parser，把它转成结构化 metadata，同时仍然作为普通消息进入 runtime？

## 推荐下一步

先实现最小 Agent HITL：

```text
human.request tool
HumanRequest file store
TurnResponse.human_requests
CLI/API resolve
waiting_human 状态传播
```

不要先做完整 policy，也不要先接 Flow。完成后再回到 Flow schema，把 flow 的 `human_approval` 和 agent-generated HumanRequest 的关系补清楚。
