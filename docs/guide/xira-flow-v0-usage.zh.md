# Xira Flow v0 使用指南

> 适用对象：需要编写、运行、调试 Xira Flow 的工程师和 AI agent。
> 目标：读完后能写出一个可运行的 Flow，知道如何启动、推进、审批、恢复、排错和验证。

## 1. 先理解 Flow 是什么

Xira Flow 是一个有状态的 case 推进协议。它不是 agent profile，也不是传统脚本。

```text
Agent = 谁来干活
Skill = agent 用什么方法和约束干活
Flow = 这件事如何分阶段推进、暂停、恢复、验收、留证据
Step = 一个目标合同，不是一个 shell 命令
```

一个 Flow run 会持久化状态，包括：

- 当前 step
- 每个 step 的状态
- 每个 step 的输出 slots
- agent run id
- pending human requests
- artifacts 引用
- 完成、失败、等待人工、取消等状态

Flow v0 的核心能力：

- 多入口 `entrypoints`
- 顺序 step
- 条件分支 `transitions.branches`
- agent executor
- `human_approval` 人工审批
- `decision` 决策 step
- `wait_signal` 等待外部信号
- retry / on_exhausted / on_failure
- CLI / API 启动、推进、恢复

## 2. 什么时候该用 Flow

应该用 Flow 的场景：

- 一个任务需要多个阶段推进。
- 中间需要人工审批、等待信息、暂停恢复。
- 需要多个 agent 协作。
- 需要把每一步结果作为后续输入。
- 需要 durable run 状态和可追踪证据。
- 任务失败后需要 retry、fallback 或 report。

不应该用 Flow 的场景：

- 只问一个 agent 一句话。
- 一次性工具调用就能完成。
- 没有阶段、没有状态、没有恢复需求。

简单判断：

```text
如果你只需要“回答一次”，用 agent。
如果你需要“推进一个 case”，用 flow。
```

## 3. Flow 文件最小结构

一个最小 Flow 文件长这样：

```yaml
schema_version: xira.flow.v0
id: hello-flow
name: Hello Flow
version: 0.1.0
objective: Turn a request into a short answer.

entrypoints:
  - id: ad_hoc
    start_step: answer
    required_inputs:
      - request

steps:
  - id: answer
    objective: Answer the user request in one short paragraph.
    executor:
      agent: xira-assistant
    output_contract:
      required_slots:
        - id: final_answer
```

关键字段：

| 字段 | 作用 |
| --- | --- |
| `schema_version` | 必须是 `xira.flow.v0` |
| `id` | Flow 定义 id |
| `entrypoints` | Flow 从哪里开始 |
| `required_inputs` | 启动时必须提供的输入 |
| `steps` | Flow 的阶段列表 |
| `objective` | 每个 step 的目标 |
| `executor.agent` | 哪个 agent 执行这个 step |
| `output_contract.required_slots` | step 必须产出的结果槽 |

## 4. 启动和推进 Flow

CLI 支持四个命令：

```bash
xira flow run <flow-file> --entrypoint <entrypoint-id> --input key=value
xira flow status <flow-run-id>
xira flow advance <flow-run-id>
xira flow resume <flow-run-id> --human-request <human-request-id>
```

启动最小 Flow：

```bash
./apps/xira/xira flow run docs/examples/flows/hello/flow.yaml \
  --entrypoint ad_hoc \
  --input request="summarize this task"
```

返回的是 JSON，其中最重要的字段：

```json
{
  "id": "fr_...",
  "flow_id": "hello-flow",
  "status": "running",
  "current_step_id": "answer"
}
```

推进一步：

```bash
./apps/xira/xira flow advance fr_...
```

查看状态：

```bash
./apps/xira/xira flow status fr_...
```

如果 Flow 进入人工审批，会看到：

```json
{
  "status": "waiting_human",
  "current_step_id": "approve_plan",
  "pending_human_requests": ["hrq_..."]
}
```

人工处理 HumanRequest 后恢复：

```bash
./apps/xira/xira flow resume fr_... --human-request hrq_...
```

## 5. Step 如何传递输入输出

Flow 的 step 不应该猜上一步写了哪个文件。正确做法是声明 output slot，然后下游通过 `${outputs.step_id.slot_id}` 引用。

例子：

```yaml
steps:
  - id: triage
    objective: Summarize the request into a compact brief.
    executor:
      agent: flow-intake
    output_contract:
      required_slots:
        - id: triage_brief
    transitions:
      on_success: design

  - id: design
    objective: Produce a design from the triage brief.
    executor:
      agent: flow-architect
    inputs:
      brief: "${outputs.triage.triage_brief}"
    output_contract:
      required_slots:
        - id: design_doc
```

支持的引用形式：

```yaml
from_input: "${input.request}"
from_output: "${outputs.triage.triage_brief}"
fallback: "${outputs.merge.merge_result || 'not_merged'}"
```

注意：

- `${input.xxx}` 来自启动 Flow 时的 `--input xxx=value`。
- `${outputs.step.slot}` 来自前面 step 的 `output_contract`。
- 如果 agent 只产出一个纯文本 required slot，Flow 会把文本保存在该 slot 的 summary，下游仍可用 `${outputs.step.slot}` 读取。

## 6. Agent step 怎么写

推荐写法：

```yaml
- id: architecture_plan
  objective: Draft a small architecture plan.
  instructions:
    - Return under 80 words.
    - Include risks and validation notes.
  constraints:
    - Do not modify files.
  executor:
    agent: flow-architect
  inputs:
    constraints: "${outputs.constraint_research.constraints}"
  output_contract:
    required_slots:
      - id: architecture_plan
  transitions:
    on_success: risk_review
```

不要把 step 写成 shell 脚本：

```yaml
# 不推荐
- id: run_tests
  executor:
    type: command
    command: go test ./...
```

Flow step 应该描述目标、输入、产出和约束。真正用 shell、Codex、MCP、GitHub API 等，是 agent profile / skill / runtime tool 的职责。

## 7. Human-in-the-loop 怎么写

Flow v0 的人工审批使用 `executor.type: human_approval`。

```yaml
- id: approve_plan
  objective: Human approval gate for the plan.
  executor:
    type: human_approval
    question: "Approve this plan?"
    options:
      - approve
      - reject
  output_contract:
    required_slots:
      - id: approval_signal
  transitions:
    branches:
      - when: "${outputs.approve_plan.approval_signal == 'approve'}"
        next: implementation
      - when: "${outputs.approve_plan.approval_signal == 'reject'}"
        next: final_report
```

运行到这个 step 时：

1. Flow 创建 runtime HumanRequest。
2. Flow 状态变成 `waiting_human`。
3. 当前 step 变成 `approve_plan`。
4. `approve_plan.human_request_ids` 里保存 HumanRequest id。
5. 人工 resolve 后，调用 `flow resume`。
6. Flow 把审批结果写入 `approval_signal`。
7. Flow 根据 branch 进入下一步。

审批结果常见值：

```text
approve
reject
deny
cancel
revise
answer
```

建议在 Flow 里只使用业务明确的选项，例如：

```yaml
options:
  - approve
  - reject
```

## 8. 条件分支怎么写

成功后固定进入下一步：

```yaml
transitions:
  on_success: verify
```

根据上游 output 分支：

```yaml
transitions:
  branches:
    - when: "${outputs.review.blocking_findings_count == 0}"
      next: approve_merge
    - when: "${outputs.review.blocking_findings_count > 0}"
      next: fix
```

根据 runtime policy 分支：

```yaml
transitions:
  branches:
    - when: "${runtime.policy.require_design_approval == true}"
      next: approve_design
    - when: "${runtime.policy.require_design_approval != true}"
      next: implement
```

当前 runtime policy v0 从 Flow input 读取布尔值，所以启动时这样传：

```bash
--input require_design_approval=true
```

## 9. Retry 和失败路由

step 可以配置 retry：

```yaml
- id: prepare_branch
  objective: Prepare a clean branch.
  executor:
    agent: dev-git-operator
  output_contract:
    required_slots:
      - id: branch
  retry:
    max_attempts: 2
    on_exhausted: report
  transitions:
    on_success: implement
```

语义：

- 第一次失败，如果 attempts 小于 `max_attempts`，Flow 会把 step 放回 pending。
- 再次 `advance` 会重新执行该 step。
- 达到 `max_attempts` 后，如果配置了 `on_exhausted`，Flow 路由到该 step。
- 没有 fallback 时，Flow run 进入 failed。

普通失败路由：

```yaml
transitions:
  on_failure: report
```

## 10. Decision step 怎么写

`decision` step 不调用 agent，只负责基于已有 outputs 跳转。

```yaml
- id: decide_fix
  objective: Decide whether fixes are required.
  executor:
    type: decision
  transitions:
    branches:
      - when: "${outputs.review.blocking_findings_count == 0}"
        next: approve_merge
      - when: "${outputs.review.blocking_findings_count > 0}"
        next: fix
```

适合用于：

- review 后判断是否修复
- 审批结果归一
- 根据结构化 slot 决定路径

## 11. 长 Flow 示例：10 step / 4 agent / HITL

下面是已通过真实 DeepSeek live 测试的长 Flow 结构。

```text
triage
  -> context_research
  -> constraint_research
  -> architecture_plan
  -> risk_review
  -> approve_plan (HITL)
  -> implementation_slice
  -> test_plan
  -> release_notes
  -> final_report
```

参与 Flow 的 4 个 agent：

```text
flow-intake
flow-research
flow-architect
flow-reviewer
```

HITL step：

```yaml
- id: approve_plan
  objective: Human approval gate for the live long flow plan.
  executor:
    type: human_approval
    question: "Approve the live 10-step DeepSeek long flow plan?"
    options:
      - approve
      - reject
  output_contract:
    required_slots:
      - id: approval_signal
  transitions:
    branches:
      - when: "${outputs.approve_plan.approval_signal == 'approve'}"
        next: implementation_slice
      - when: "${outputs.approve_plan.approval_signal == 'reject'}"
        next: final_report
```

这个 case 已经作为 live test 固化在：

```text
apps/xira/internal/runtime/deepseek_hitl_live_test.go
```

测试名：

```text
TestRealDeepSeekLongFlowFourAgentsWithHITL
```

它验证：

- 10 个 step 全部 completed。
- 9 个 agent step 都真实调用 DeepSeek。
- 1 个 HITL step 创建真实 HumanRequest。
- 人工 approve 后 Flow 能 resume。
- 4 个指定 agent 都确实被 runtime run store 记录使用。

## 12. 如何跑真实 DeepSeek Flow 测试

真实 DeepSeek 测试默认不会跑，必须显式打开：

```bash
export XIRA_DEEPSEEK_LIVE=1
export DEEPSEEK_API_KEY="$(cat DEEPSEEK_API_KEY)"
```

如果 key 在 repo 根目录的 `DEEPSEEK_API_KEY` 文件：

```bash
GOCACHE=$(pwd)/.cache/go-build \
XIRA_DEEPSEEK_LIVE=1 \
DEEPSEEK_API_KEY="$(tr -d '\r\n' < DEEPSEEK_API_KEY)" \
go test -count=1 ./apps/xira/internal/runtime \
  -run TestRealDeepSeekLongFlowFourAgentsWithHITL -v
```

只测 Flow + DeepSeek：

```bash
GOCACHE=$(pwd)/.cache/go-build \
XIRA_DEEPSEEK_LIVE=1 \
DEEPSEEK_API_KEY="$(tr -d '\r\n' < DEEPSEEK_API_KEY)" \
go test -count=1 ./apps/xira/internal/runtime \
  -run 'TestRealDeepSeekFlow(AgentStepCompletes|RoutesToHumanApproval)' -v
```

只测 HITL + DeepSeek：

```bash
GOCACHE=$(pwd)/.cache/go-build \
XIRA_DEEPSEEK_LIVE=1 \
DEEPSEEK_API_KEY="$(tr -d '\r\n' < DEEPSEEK_API_KEY)" \
go test -count=1 ./apps/xira/internal/runtime \
  -run 'TestRealDeepSeekHITL' -v
```

不要把 `DEEPSEEK_API_KEY` 提交到 Git。repo 的 `.gitignore` 已忽略根目录 `DEEPSEEK_API_KEY`。

## 13. API 用法

启动 Flow：

```http
POST /api/v1/flows/runs
Content-Type: application/json

{
  "flow_path": "docs/examples/flows/devrun/flow.yaml",
  "entrypoint_id": "ad_hoc",
  "input": {
    "repo": "/Users/me/work/repo",
    "request": "fix the failing test"
  }
}
```

推进 Flow：

```http
POST /api/v1/flows/runs/<flow-run-id>/advance
Content-Type: application/json

{}
```

查看 Flow：

```http
GET /api/v1/flows/runs/<flow-run-id>
```

恢复 Flow：

```http
POST /api/v1/flows/runs/<flow-run-id>/resume
Content-Type: application/json

{
  "human_request_id": "hrq_..."
}
```

错误语义：

| 场景 | HTTP |
| --- | --- |
| unknown entrypoint / missing input | 400 |
| unknown flow run | 404 |
| unknown linked human request | 404 |
| human request still pending | 409 |

## 14. 常见错误和排查

### 14.1 missing required flow input

错误：

```text
missing required flow input(s): request
```

原因：

- entrypoint 声明了 `required_inputs`。
- 启动时没有传对应 `--input key=value`。

修复：

```bash
--input request="..."
```

### 14.2 step slot is not set

错误：

```text
step "design" slot "implementation_plan" is not set
```

原因：

- 下游引用了 `${outputs.design.implementation_plan}`。
- 上游 step 没有声明或没有产出 `implementation_plan`。

检查：

- 上游 `output_contract.required_slots` 是否包含该 slot。
- agent 输出是否能被映射到该 slot。
- 如果有多个 required slots，建议 agent 返回 fenced YAML / JSON。

### 14.3 agent completed but required output slots missing

原因：

- agent 完成了，但没有产出 required slot。
- 如果 required slot 不止一个，纯文本无法自动映射。

建议让 agent 返回：

````text
```yaml
task_spec: ...
acceptance_criteria: ...
```
````

或 JSON：

````text
```json
{
  "task_spec": "...",
  "acceptance_criteria": "..."
}
```
````

### 14.4 Flow 卡在 waiting_human

检查：

```bash
xira flow status <flow-run-id>
```

找到：

```json
"pending_human_requests": ["hrq_..."]
```

先处理 HumanRequest，再 resume：

```bash
xira flow resume <flow-run-id> --human-request hrq_...
```

如果 HumanRequest 还没 resolve，API 会返回 409。

### 14.5 live DeepSeek 测试 skip

日志：

```text
set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek ...
DEEPSEEK_API_KEY is required ...
```

修复：

```bash
export XIRA_DEEPSEEK_LIVE=1
export DEEPSEEK_API_KEY=...
```

### 14.6 sandbox 内 DNS 失败

错误：

```text
lookup api.deepseek.com: no such host
```

原因：

- sandbox 禁止网络或 DNS。

处理：

- 在允许网络的环境跑 live test。
- 普通 deterministic tests 不需要网络。

## 15. 写 Flow 的推荐步骤

1. 先写业务阶段，不写 YAML。

```text
intake -> design -> approve -> implement -> verify -> report
```

2. 给每个阶段指定 agent。

```text
intake: flow-intake
design: flow-architect
verify: flow-reviewer
report: flow-reporter
```

3. 给每个 step 定义 required output slot。

```yaml
output_contract:
  required_slots:
    - id: design_doc
```

4. 给下游 step 用 `${outputs.step.slot}` 连接输入。

5. 把人工决策点写成 `human_approval`。

6. 把机器决策点写成 `decision` 或 `transitions.branches`。

7. 给不稳定 step 加 retry。

8. 跑 deterministic tests。

9. 再跑 live DeepSeek smoke。

## 16. Flow 编写检查清单

写完一个 Flow 后，逐项检查：

- `schema_version` 是 `xira.flow.v0`。
- `id` 唯一且稳定。
- 每个 entrypoint 都有 `start_step`。
- 每个 required input 都能在启动命令里提供。
- 每个 step id 唯一。
- 每个 agent executor 引用的 agent profile 存在。
- 每个 downstream `${outputs.step.slot}` 的上游 slot 存在。
- 每个 branch 至少有一个可达路径。
- HITL step 有清晰 `question` 和 `options`。
- reject/cancel 路径不会读取不存在的 output。
- retry step 有明确 `max_attempts` 和 fallback。
- 最终 step 能让 Flow completed。
- live test 不会把 secret 打到日志。

## 17. 当前限制

Flow v0 当前不是完整 DAG engine。

已支持：

- 顺序推进
- 条件分支
- agent step
- human approval
- retry / failure routing
- CLI / API start/advance/resume

需要谨慎使用或后续扩展：

- 并行 step
- subflow
- wait_signal 的完整外部事件接入
- 复杂 policy DSL
- 复杂 artifact lifecycle
- UI 化 Flow 编辑器

## 18. 最短可用心智模型

如果只记一件事，记这个：

```text
Flow 文件不是写“怎么调用工具”。
Flow 文件是写“一个 case 如何跨阶段达成目标”。

每个 step 都应该回答：
1. 这个阶段目标是什么？
2. 谁来做？
3. 输入来自哪里？
4. 必须产出什么？
5. 成功、失败、审批后去哪里？
```
