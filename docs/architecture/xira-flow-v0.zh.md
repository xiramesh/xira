# Xira Flow v0 Schema 设计说明

> 状态：草案，供人和 AI review。<br>
> 目标：先定义 Flow 的概念边界、文件形态和完整 DevRun 示例，再实现 Flow Run Kernel。

## 结论

Xira Flow v0 不是 agent profile 的别名，也不是传统 action-first workflow。

它的定义是：

```text
Flow = 目标驱动的、有状态 case 推进协议。
```

更具体地说：

- Agent 是执行者，回答“谁来干活”。
- Skill 是执行者加载的方法、知识或能力约束。
- Flow 是业务 case 的推进协议，回答“这件事如何跨阶段达成、暂停、恢复、验收和留下证据”。
- Step 是目标合同，不是动作脚本。
- Executor 默认是完成 step 目标的 agent。这个 agent 内部可以使用 ADK、Codex CLI、Claude CLI、MCP、本地命令或其他工具，但这些实现细节不进入 flow step 的主结构。

## 入口模型

入口可以直接指向 agent，也可以指向 flow。

```yaml
entrypoints:
  - id: cli-local
    channel: cli
    default_target:
      type: agent
      id: xira-assistant
    allowed_flows:
      - devrun

  - id: ilink-devrun
    channel: ilink
    default_target:
      type: flow
      id: devrun
```

同一个 agent-first 入口也可以通过 slash invocation 启动 flow：

```text
/devrun 帮我修这个 bug
/dev-flow 从 issues 里挑一个适合的修
```

这类 slash invocation 不是 skill activation。它创建或恢复一个 `flow_run`。

## Skill 与 Flow 的区别

| 概念 | Skill | Flow |
| --- | --- | --- |
| 主要用途 | 增强当前 agent 的方法和约束 | 推进一个有状态业务 case |
| 是否有 durable run | 没有 | 有 |
| 是否有 current step | 没有 | 有 |
| 是否支持 pause/resume | 不支持 | 支持 |
| 是否有 artifacts/approval/history | 依附 agent run | 自己保存并引用 child run |
| 调用形式 | agent profile 默认加载或显式激活 | entrypoint target 或 slash invocation |

## Flow Definition / Flow Run / Kernel

```text
Flow Definition
  定义目标、入口、step、executor、output contract、transition、权限和验收。

Flow Run
  某一次 flow 实例，保存 input、current step、step result、pending approval、artifacts、child runs。

Flow Run Kernel
  推进状态机：start、advance、pause、resume、retry、cancel。
```

v0 不做完整 DAG。v0 支持：

- 多入口 `entrypoints`
- 顺序 step
- 条件跳转 `transitions.branches`
- 人工审批暂停
- 失败 retry
- child agent run / artifacts 引用

## Step 是目标合同

不推荐这样写：

```yaml
- id: code
  executor:
    type: command
    command:
      program: codex
```

推荐这样写：

```yaml
- id: implement
  objective: 根据实现计划完成代码修改，并留下变更摘要、文件列表和风险说明。
  executor:
    agent: dev-implementer
  instructions:
    - Use `/skill code-implementation`.
    - Prefer Codex for code edits when available.
    - Keep changes scoped to files implied by the implementation plan.
  constraints:
    - Do not merge.
    - Do not change unrelated files.
    - Ask for approval before destructive git operations.
  output_contract:
    required_slots:
      - id: change_summary
      - id: changed_files
      - id: risk_notes
    completion_criteria:
      - 代码修改已经落到 workspace。
      - 输出说明包含验证建议。
```

step 关注目标、产出和完成标准。`codex exec`、`claude -p`、`git`、`gh`、`task` 这类实现选择由 `dev-implementer` 这个 agent 在权限边界内决定，或由它加载的 skill / prompt 决定。

## Executor 类型

v0 把工作型 step 收敛到 agent executor：

| executor.type | 含义 |
| --- | --- |
| `executor.agent` | 调用一个 agent profile / agent adapter 完成目标。 |
| `executor.type: human_approval` | 生成审批请求并暂停 flow run，等待 signal。 |
| `executor.type: decision` | 根据上游 output slots 或 artifacts 做条件跳转。 |
| `executor.type: wait_signal` | 等待外部 channel、webhook 或手动 signal。 |
| `executor.type: subflow` | 后续扩展，调用另一个 flow。 |

不要在 flow step 里直接把 `claude -p`、`codex exec`、`git`、`gh` 写成结构化动作。需要确定性时，通过 step-local instruction、required skill、constraints 和 runtime policy 约束 agent：

```yaml
instructions:
  - Use `/skill claude-review`.
  - Review the current diff only.
  - Do not modify files.
constraints:
  - Do not call implementation tools in this step.
required_skills:
  - claude-review
```

如果某个 agent 本身就是 CLI-backed，可以在 agent profile / agent engine adapter 层定义：

```yaml
id: claude-reviewer
engine:
  type: cli
  program: claude
  args:
    - -p
    - "${runtime.prompt_file}"
```

flow 只引用：

```yaml
executor:
  agent: claude-reviewer
```

## Output Contract

flow 不要求每个 artifact 路径都预先写死。step 可以声明 slots，由 executor 或 post-processor 决定具体 artifact。

```yaml
output_contract:
  artifact_policy: agent_defined
  required_slots:
    - id: task_spec
      description: 可执行任务规格
      formats: ["yaml", "markdown"]
    - id: implementation_plan
      description: 实现计划
      formats: ["markdown"]
```

step 完成后，kernel 记录结构化结果：

```yaml
step_id: design
status: completed
outputs:
  task_spec:
    artifact: artifacts/design/task_spec.yaml
  implementation_plan:
    artifact: artifacts/design/implementation_plan.md
```

下游 step 引用 slot，而不是猜文件名：

```yaml
inputs:
  implementation_plan: "${outputs.design.implementation_plan}"
```

## DevRun Flow 的入口

DevRun 不应是一条固定脚本。它是同一个开发目标 flow，支持多个入口：

| entrypoint | start_step | 场景 |
| --- | --- | --- |
| `ad_hoc` | `intake` | 临时想起一个想法，直接修改。 |
| `bugfix` | `reproduce` | 看到一个 bug，先复现和诊断。 |
| `issue_pickup` | `select_issue` | 自动化触发，自己看 issues，挑一个合适的修。 |

它们最终收敛到：

```text
design -> approve_design? -> implement -> verify -> review -> fix? -> approve_merge? -> report
```

## Flow Run State 草案

Flow run 持久化在：

```text
.xira/flow-runs/<flow_run_id>/flow_run.yaml
```

最小状态：

```yaml
schema_version: xira.flow_run.v0
flow_run_id: fr_20260613_devrun_001
flow_id: devrun
flow_version: 0.1.0
status: waiting_approval
current_step_id: approve_merge
input:
  request: "修复会话恢复 bug"
  repo: /Users/yinwm/work/flowdeck
steps:
  intake:
    status: completed
    child_run_id: 20260613-101001-dev-intake
    outputs:
      task_spec:
        artifact: artifacts/intake/task_spec.yaml
  approve_merge:
    status: waiting
    approval_id: apr_20260613_002
pending_signals:
  - approval:apr_20260613_002
created_at: 2026-06-13T10:00:00Z
updated_at: 2026-06-13T10:05:00Z
```

## Human-in-the-loop 与 HumanRequest

Flow v0 不实现第二套审批系统。所有 human-in-the-loop 都走 runtime 已有的 `HumanRequest`，来源分成三类：

| 来源 | `source` | 触发者 | 说明 |
| --- | --- | --- | --- |
| Flow 显式审批 | `flow_human_approval` | Flow Kernel | `executor.type: human_approval` 控制步直接创建 `HumanRequest`，kind 为 `approval`。 |
| Agent 主动请求 | `agent_request` | Agent runtime | agent 在执行步内调用 `human.request` 生成请求（approval 或 freeform）。 |
| Runtime 工具闸门 | `runtime_tool_gate` | Runtime | 工具策略要求 `RequireConfirmation`，runtime 创建 approval 请求并 suspend 当前 tool call。 |

三者都暂停 Flow，Kernel 只观察 parent agent run 的状态：

- Flow 显式审批：步执行器创建 `HumanRequest` 后直接返回 `waiting_human`。
- Agent 主动请求 / 工具闸门：agent runtime 在 `TurnResponse.Status` 上呈现 `waiting_human`，并附带 `HumanRequests` 和 `Interrupt`；Flow 只看 parent run 的 `waiting_human`，**不**展开 inspect 子委派内部。
- 子委派 `waiting_human`：parent agent run 在 delegate tool call suspend 时把自己标成 `waiting_human`，Flow 据此暂停，不需要查看 child run 文件或 delegation join 状态。

### 暂停与恢复语义

```text
flow.run.started
  -> flow.step.started
     -> flow.agent_run.started        (work step)
     -> flow.step.waiting_human       (任一来源)
        -> flow.human_request.linked
  -> flow.step.completed / failed
-> flow.run.completed / failed / canceled
```

- 暂停：当步结果为 `waiting_human` 时，step 状态 = `waiting_human`，run 状态 = `waiting_human`，`current_step_id` 停在该步，不评估 transition，`human_request_ids` 持久化到 step state。
- 恢复：`Kernel.Resume(flowRunID, humanRequestID)` 仅当 run 处于 `waiting_human` 且当前步包含该 `humanRequestID` 时执行；加载 `HumanRequest`，pending 直接拒绝；resolved 才进入映射：
  - 显式审批：把响应（`approve`/`deny`/`cancel`/`revise`/`reject`）映射成 step 输出 slot，再评估 transition。
  - agent 主动请求 / 工具闸门：依赖 runtime 已有的 resume 路径（`ResolveHumanRequest` 在 approve + ActionSnapshot 时自动 replay），Flow 不发明 model-visible 的 `human.respond`，也不重复推进同一个 agent run。

> WATCH：runtime 的 `ResolveHumanRequest` 在 approve 且带 `ActionSnapshot` 时会自动 replay approved tool。Flow 显式审批不携带 `ActionSnapshot`，不会触发误 replay；agent-generated / runtime tool gate 的恢复必须能证明不会双重推进（见 milestone 5/8 的回归测试）。

### Flow scope metadata

在 v0，`HumanRequest.Scope` 还未泛化。Flow 把 scope 写入 `Metadata`：

```text
scope_type=flow_run
flow_run_id=<id>
flow_step_id=<step id>
flow_id=<flow id>
```

等后续 `HumanRequest.Scope` 重构落地再回收。

## Review Checklist

请重点 review：

- Flow 是否和 Agent / Skill 有清晰边界。
- Step 是否足够目标驱动，而不是退化成命令脚本。
- `executor.agent + instructions/constraints/required_skills` 是否能覆盖 `claude -p`、`codex exec` 这类实际用法。
- Output slots 是否足够表达“产物由 agent 决定，但 flow 要能引用”。
- DevRun 的三个入口是否覆盖真实开发习惯。
- v0 是否仍然太大，哪些字段应延后。

## 对应文件

- JSON Schema: `docs/schemas/xira-flow-v0.schema.json`
- Flow Run JSON Schema: `docs/schemas/xira-flow-run-v0.schema.json`
- 完整示例: `docs/examples/flows/devrun/flow.yaml`
- Flow Run 状态示例: `docs/examples/flows/devrun/flow_run.waiting_approval.yaml`
