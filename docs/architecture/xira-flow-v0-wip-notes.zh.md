# Xira Flow v0 WIP 暂存说明

> 分支：`feature/flow-schema-v0`  
> 用途：切换到 Agent HITL 工作前，记录当前 Flow schema 草案状态。  
> 状态：未提交，随当前工作区一起 stash。

## 当前结论

Flow v0 的方向已经从 action-first workflow 调整为 agent-first flow：

```text
Flow Step = 目标合同
Executor = agent
确定性 = step-local instructions + constraints + required_skills + output_contract
```

工作型 step 不再直接写 `codex exec`、`claude -p`、`git`、`gh`、`task` 等动作。Flow 只选择完成目标的 agent；具体使用什么 CLI、工具或 skill，由 agent profile / step instruction / runtime policy 决定。

## 当前新增文件

- `docs/architecture/xira-flow-v0.zh.md`
  - Flow / Agent / Skill / Step / Executor 边界说明。
  - 说明 slash command、Flow Run Kernel、agent-first step、output slots。

- `docs/schemas/xira-flow-v0.schema.json`
  - Flow Definition JSON Schema。
  - 工作 step 使用 `executor.agent`。
  - 控制 step 保留 `human_approval`、`decision`、`wait_signal`、`subflow`。

- `docs/schemas/xira-flow-run-v0.schema.json`
  - Flow Run State JSON Schema。
  - 保存 `flow_run_id`、`current_step_id`、step states、pending signals、child run refs。

- `docs/examples/flows/devrun/flow.yaml`
  - 完整 DevRun 示例。
  - 支持 `ad_hoc`、`bugfix`、`issue_pickup` 三种入口。
  - 主要 step：`select_issue`、`intake`、`reproduce`、`diagnose`、`design`、`prepare_branch`、`implement`、`verify`、`create_pr`、`review`、`fix_or_approve`、`fix`、`approve_merge`、`merge`、`report`。

- `docs/examples/flows/devrun/flow_run.waiting_approval.yaml`
  - 一个停在 `approve_merge` 的 flow run 状态示例。

- `docs/examples/flows/devrun/task_spec.md`
- `docs/examples/flows/devrun/context/required.md`
- `docs/examples/flows/devrun/context/forbidden.md`
- `docs/examples/flows/devrun/verification/acceptance.yaml`
- `docs/examples/flows/devrun/verification/golden_tasks.yaml`

## 已验证

已跑过：

```bash
python3 -m json.tool docs/schemas/xira-flow-v0.schema.json >/dev/null
python3 -m json.tool docs/schemas/xira-flow-run-v0.schema.json >/dev/null
```

已用 `jsonschema` 校验：

```text
docs/examples/flows/devrun/flow.yaml
  -> docs/schemas/xira-flow-v0.schema.json

docs/examples/flows/devrun/flow_run.waiting_approval.yaml
  -> docs/schemas/xira-flow-run-v0.schema.json
```

已跑过：

```bash
go test ./apps/xira/...
```

结果通过。

## 重要未决问题

下一步切到 Agent HITL 后，需要回头影响 Flow schema：

1. HITL 应该是 runtime 基础能力，不是 Flow 独占能力。
2. Agent run 也必须能发起 approval / clarification / risk gate。
3. Flow 的 `human_approval` 是显式控制 step，但任意 agent step 也可能生成 HITL request。
4. Flow kernel 遇到 agent-generated HITL request 时，应暂停当前 step，等待 signal 后 resume。
5. Approval / Signal Store 应该支持不同 scope：
   - `agent_run`
   - `agent_session`
   - `flow_run`

## 后续回收建议

完成 Agent HITL 设计后，回来更新：

- `docs/architecture/xira-flow-v0.zh.md`
- `docs/schemas/xira-flow-v0.schema.json`
- `docs/schemas/xira-flow-run-v0.schema.json`
- DevRun 示例里的 approval / wait_signal 表达

重点把 `human_approval` 和 agent-generated HITL request 的关系写清楚。
