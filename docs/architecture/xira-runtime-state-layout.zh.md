# Xira Runtime State Layout 整理

> 状态：讨论稿。  
> 目标：收敛 Xira 聊天历史、agent run、Flow run、HITL state 的存储边界，并重新定义 `root` 类配置在产品语义里的位置。

## 结论

Xira 的一等边界应该是 `workspace`，不是多套可见的 `root`。

推荐的用户心智模型是：

```text
workspace/
  agents/
  flows/
  entrypoints.yaml
  .xira/
    sessions/
    runs/
    flow-runs/
    workspaces/<workspace-key>/
      human-requests/
      human-responses/
      replay-results/
    usage-ledger.jsonl
```

其中：

- `workspace` 是产品边界、配置边界、默认文件权限边界和状态归属边界。
- `.xira/` 是这个 workspace 的 runtime state 目录。
- `sessions/`、`runs/`、`flow-runs/`、`workspaces/<workspace-key>/...` 是 `.xira/` 下的职责分区，不应该暴露成并列的一等用户概念。
- `allow_roots` / `readonly_roots` 仍然保留，但它们是 sandbox 外部访问授权，不是 runtime state 存储 root。

## 概念边界

### Agent 聊天历史

聊天历史跟 conversation / agent 走，不跟 Flow 编排层走。

```text
workspace/.xira/sessions/
  <channel>/<entrypoint>/<conversation-dir>/agents/<agent-id>/messages.jsonl
```

`messages.jsonl` 记录面向后续对话和模型 hydration 的消息序列：

- user message
- assistant message
- tool call
- tool result
- human request / human response 摘要

Flow 调 agent 时，不应该把聊天历史放进 `sessions/flow/...`。Flow 不是用户来源 channel。Flow 内的 agent step 应继承真实触发身份：CLI 触发就落 `sessions/cli/...`，飞书触发就落 `sessions/feishu/...`。

### Agent run

Agent run 是一次执行的审计证据，不是长期聊天历史。

```text
workspace/.xira/runs/<agent-run-id>/
  run.yaml
  events.jsonl
  audit.jsonl
  tool_calls.jsonl
  llm_calls.jsonl
  usage.json
  verification.json
  artifacts/
```

它回答的是“这次 agent 运行发生了什么”，包括事件、工具调用、模型调用、成本、验证结果和原始 artifact。

### Flow run

Flow 应该有自己的存储，但它存的是编排状态，不是聊天记录。

```text
workspace/.xira/flow-runs/<flow-run-id>/
  flow_run.yaml
  events.jsonl
  artifacts/
```

它回答的是“这个 case 推进到哪里了”：

- current step
- step states
- output slots
- pending human requests
- transitions / retry / pause / resume 状态
- child agent run id
- conversation session id / agent session id 引用

Flow run 和 agent run 的关系应该是引用关系：Flow run 引用 child agent run，agent run 引用 conversation session。不要把三者揉成一个大文件。

### HITL state

HumanRequest 是可恢复、可响应、可重放的业务状态，应该属于 workspace state。

推荐路径继续保留 workspace key 分片：

```text
workspace/.xira/workspaces/<workspace-key>/
  human-requests/<request-id>.yaml
  human-responses/<response-id>.yaml
  replay-results/<request-id>.yaml
```

原因：

- 当前 runtime 的 create/get/list/resolve 都通过 `WorkspaceKey()` 分片。
- HumanRequest 不是孤立文件；resolve 后还会写 `human-responses/` 和 `replay-results/`。
- `state_dir` 允许未来被多个 workspace 共享时，workspace key 是隔离边界。

不要在本轮把 HumanRequest 压平成 `stateDir/human-requests/`。如果未来要做单 workspace 压平，需要单独设计迁移、索引、response/replay 路径和共享 state_dir 冲突处理。

## `root` 的收敛

当前代码里有这些概念：

- `WorkspaceRoot`
- `StateRoot`
- `RunRoot`
- `SessionRoot`
- 各 store 构造函数里的 `root`

这些概念在实现层都可以存在，但用户侧不应该同时面对它们。

建议收敛为：

```text
WorkspaceRoot        一等产品概念，保留
StateDir             内部派生，默认 <WorkspaceRoot>/.xira
RunRoot              内部派生，默认 <StateDir>/runs
SessionRoot          内部派生，默认 <StateDir>/sessions
FlowStore root       内部派生，默认 <StateDir>
HumanRequest store root 内部派生，默认 <StateDir>
HumanRequest persisted paths <StateDir>/workspaces/<workspace-key>/{human-requests,human-responses,replay-results}
```

配置层建议只保留：

```yaml
workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
```

高级覆盖字段命名为 `state_dir`，不要继续叫 `state_root`：

```yaml
workspace: workspace
state_dir: /var/lib/xira/workspaces/acme
```

不建议继续让 `run_root`、`session_root`、`state_root` 同时出现在普通 workspace 配置里。它们让人误以为 run、session、flow、HITL 可以随意分散，而产品语义上这些都是同一个 workspace 的状态。新配置字段使用 `state_dir`，降低“多个 root”的心智负担。

## 旧实现偏差

旧 `xira.yaml` 曾经有：

```yaml
workspace: workspace
run_root: .xira/runs
entrypoints: workspace/entrypoints.yaml
```

这会让 `.xira/runs` 相对 `xira.yaml` 所在目录解析，而不是自然落到 `workspace/.xira/runs`。

`entrypoints: workspace/entrypoints.yaml` 也是相对 `xira.yaml` 所在目录解析。开局阶段不保留旧配置兼容，直接切到新语义：

- 缺省值：相对 workspace，默认读取 `workspace/entrypoints.yaml`。
- 显式配置：`entrypoints: entrypoints.yaml`，也按 workspace-relative 解析。
- 不再支持旧的 `entrypoints: workspace/entrypoints.yaml` config-relative 写法。

旧 `resolveRuntimeConfig` 的默认推导也以 `run_root` 为中心，并使用 `state_root` 命名：

```text
runRoot 默认 .xira/runs
sessionRoot 默认 filepath.Dir(runRoot)/sessions
stateRoot 默认 filepath.Dir(runRoot)/state
```

这和目标心智相反。目标应该以 `workspace` 为中心：

```text
stateDir    = workspace/.xira
runRoot     = stateDir/runs
sessionRoot = stateDir/sessions
flowRoot    = stateDir/flow-runs
```

## 迁移原则

1. 新 workspace 默认写入 `workspace/.xira/`。
2. 不保留 `run_root` / `session_root` / `state_root` 旧字段兼容；配置只接受 `state_dir`。
3. CLI status 应同时展示 `workspace` 和派生后的 state paths，方便排查。
4. 文档应停止推荐 `run_root: .xira/runs` 这种 repo-root 布局。
5. 如果检测到 repo-root `.xira/` 和 `workspace/.xira/` 同时存在，工具应给出清晰提示，而不是静默分裂状态；不要自动搬迁旧目录。
6. `entrypoints` 统一相对 workspace 解析；旧显式路径不兼容。

## 实现建议

第一步改默认推导和新字段名，移除旧 root 字段：

```text
if no explicit state_dir:
  stateDir = workspace/.xira
runRoot = stateDir/runs
sessionRoot = stateDir/sessions
```

配置读取规则：

```text
只读取 state_dir
不读取 state_root / run_root / session_root；strict YAML 错误应提示改用 state_dir
store 构造函数不接受空 stateDir；调用方必须传入 resolved state_dir，空值应直接失败
status 返回 state_dir
```

第二步把 Flow store 接到同一个 state dir：

```text
flow.NewStore(stateDir) -> stateDir/flow-runs/<id>/
```

第三步保留 HumanRequest 的 workspace-key 分片目录：

```text
stateDir/workspaces/<workspace-key>/
  human-requests/
  human-responses/
  replay-results/
```

这一步不要压平到 `stateDir/human-requests/`。压平会破坏当前 get/list/resolve 通过 workspace key 查找的契约，也会丢掉 response/replay 的目标路径定义。

第四步更新配置和文档：

- 删除示例里的 `run_root`
- `entrypoints` 新配置相对 workspace 解析，示例写 `entrypoints: entrypoints.yaml`
- 在 `xira status` 里展示最终 resolved paths
- 如果 repo-root `.xira/` 与 resolved `state_dir` 同时存在，启动时给出 warning；不自动迁移旧状态。

## 已确认决策

1. 配置字段名从 `state_root` 改成 `state_dir`；不保留旧字段读取兼容，status 只返回 `state_dir`。
2. 旧的 repo-root `.xira/` 不自动迁移；只提示，避免静默移动用户数据。
3. HumanRequest 不压平，继续使用 `stateDir/workspaces/<workspace-key>/{human-requests,human-responses,replay-results}`。
4. `entrypoints` 统一相对 workspace 解析；旧显式 config-relative 路径不兼容。
5. XiraGarden 查询 session/run/flow 暂不进入本轮范围。
