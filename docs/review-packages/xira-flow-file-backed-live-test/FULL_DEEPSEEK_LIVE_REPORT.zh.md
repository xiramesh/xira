# Xira Flow + HITL 真实 DeepSeek 完整 Review 报告

日期：2026-06-16

本报告用于交给其他 reviewer 复核当前 Flow/HITL 分支。报告覆盖真实 DeepSeek API 跑出来的结果、保留的 replay 现场、测试场景、发现的问题、修复点、验证命令和 review 检查清单。

## 结论

最终结论：真实 DeepSeek API 完整 live suite 已通过。

最终通过命令：

```bash
DEEPSEEK_API_KEY="$(tr -d "\r\n" < DEEPSEEK_API_KEY)" \
XIRA_DEEPSEEK_LIVE=1 \
XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804 \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run "TestRealDeepSeek" -v
```

最终结果：

```text
PASS
ok  	github.com/xiramesh/xira/internal/runtime	201.297s
```

主证据目录：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804
```

目录结构说明：本轮修复后，`XIRA_LIVE_ARTIFACT_ROOT` 是总证据目录，每个 live test 会落到自己的子目录，避免多个真实 DeepSeek case 共用同一个 workspace/state/runs 而互相污染。

本轮完整 live suite 共有 10 个 preserved 子目录：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekhitlhumanrequesttool
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekhitlrequireconfirmationsnapshot
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekhitlrespondsafterapprovedtooloutput
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekhitldelegatecompleted
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekhitldelegatechildwaiting
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowagentstepcompletes
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowroutestohumanapproval
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseeklongflowfouragentswithhitl
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseeklongflowfouragentswithtoolsandhitl
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowfileartifactsskipreadwithskill
```

这次不是只跑 fake model，也不是只测最终文本。完整 suite 真实调用 DeepSeek，并验证了 Flow、HITL、delegate、多 agent、workspace tools、skill、文件落盘、跳步读取、tool contract 和 replay 证据链。

## Review 重点

Reviewer 应重点看三件事：

1. Flow 是否真的按业务链路执行，而不是模型口头声称完成。
2. HITL 是否真的暂停、审批、恢复，并且恢复时不会重新打开不该打开的 human request。
3. 每个关键步骤的工具调用是否被 runtime 正确暴露、约束、执行、记录和持久化。

最关键的证据不是最终回答，而是这些文件：

```text
tool_calls.jsonl
run.yaml
events.jsonl
audit.jsonl
usage.json
workspace/artifacts/flow-files/*.md
state/flow-runs/*/flow_run.yaml
state/workspaces/*/human-requests/*.yaml
```

## 完整证据链

下面是建议 reviewer 按顺序复核的完整证据链。不要只看最终模型文本，因为模型可能会“声称完成”；要用 runtime 的持久化记录证明每一步确实执行过。

### 1. 从最终 live suite 入口开始

主证据目录：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804
```

这个目录证明本轮不是临时内存态，而是保留了：

- runtime run records
- Flow run state
- session history
- human request state
- workspace artifacts
- tool call JSONL
- audit/events/usage

### 2. 找到 Flow run 定义和状态

file-backed Flow 的 Flow run state 在：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowfileartifactsskipreadwithskill/state/flow-runs/
```

Reviewer 应检查其中的：

```text
definition.yaml
flow_run.yaml
```

重点看：

- `current_step_id` 最终是否结束。
- 每个 step 的 `status` 是否为 `completed`。
- 每个 agent step 是否有 `agent_run_id`。
- `approve_plan` 是否有 `human_request_ids`，并且审批结果是否进入后续 step。

### 3. 用 `agent_run_id` 回查每一步 agent run

每个 step 的 `agent_run_id` 对应：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowfileartifactsskipreadwithskill/runs/<agent_run_id>/
```

每个 run 目录下至少要看：

```text
run.yaml
tool_calls.jsonl
events.jsonl
audit.jsonl
usage.json
llm_calls.jsonl
verification.json
```

这些文件分别证明：

| 文件 | 用途 |
| --- | --- |
| `run.yaml` | 本次 agent run 的整体状态、输入、输出、tool calls、模型策略、技能列表 |
| `tool_calls.jsonl` | 工具调用的真实记录，包含 tool name、input、output、error、时间 |
| `events.jsonl` | runtime event 流，能看到 tool started/finished、run started/finished |
| `audit.jsonl` | 权限和安全审计，比如 tool 是否被允许 |
| `usage.json` | token / call count / runtime usage |
| `llm_calls.jsonl` | 模型请求和响应摘要，证明真的调用了 DeepSeek |
| `verification.json` | runtime verification 结果 |

### 4. 检查 HITL 证据链

human request state 在：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowfileartifactsskipreadwithskill/state/workspaces/*/human-requests/
```

Reviewer 应确认：

- human request 曾经是 `pending`。
- 后续有 approve response。
- Flow 从 `waiting_human` resume。
- resume 后进入 `implementation_slice`，而不是重新打开同一个 human request。
- approved tool output replay 没有重新暴露 profile tools / runtime-native tools。

### 5. 检查 file-backed artifacts

最终 artifacts 在：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/
```

应存在：

```text
01-brief.md
02-research.md
03-constraints.md
04-plan.md
05-implementation.md
06-risk.md
07-test-plan.md
08-release.md
09-final-report.md
```

每个文件用 marker 串串起来：

| 文件 | 必须包含 |
| --- | --- |
| `01-brief.md` | `BRIEF-MARKER-101` |
| `02-research.md` | `RESEARCH-MARKER-202` |
| `03-constraints.md` | `CONSTRAINT-MARKER-303` |
| `04-plan.md` | `PLAN-MARKER-404`、`BRIEF-MARKER-101`、`CONSTRAINT-MARKER-303` |
| `05-implementation.md` | `IMPL-MARKER-505`、`PLAN-MARKER-404` |
| `06-risk.md` | `RISK-MARKER-606`、`RESEARCH-MARKER-202`、`IMPL-MARKER-505` |
| `07-test-plan.md` | `TEST-MARKER-707`、`BRIEF-MARKER-101`、`RISK-MARKER-606` |
| `08-release.md` | `RELEASE-MARKER-808`、`CONSTRAINT-MARKER-303`、`TEST-MARKER-707` |
| `09-final-report.md` | `FINAL-MARKER-909`、`BRIEF-MARKER-101`、`PLAN-MARKER-404`、`RELEASE-MARKER-808` |

### 6. 检查最终 step 的强证据

最终 file-backed step 的 agent run：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowfileartifactsskipreadwithskill/runs/20260616-210111-flow-research/
```

必须看到：

```text
tool_calls=5
list_dir artifacts/flow-files
read_file artifacts/flow-files/08-release.md
read_file artifacts/flow-files/01-brief.md
read_file artifacts/flow-files/04-plan.md
write_file artifacts/flow-files/09-final-report.md
```

上面按 preserved `tool_calls.jsonl` 的实际顺序列出。断言层面只要求 read set 完整，不要求 3 个 `read_file` 的顺序，因为 ADK 可以并发执行这些只读工具。

这条链路证明 final report 是基于真实目录 listing 和真实上游文件读取生成的。

## 并行边界说明

当前完整 live suite 里有多 agent，但没有并行 Flow agent step。

准确说：

- `TestRealDeepSeekLongFlowFourAgentsWithHITL` 是 10 step、4 agent，但 step 是顺序推进的。
- `TestRealDeepSeekLongFlowFourAgentsWithToolsAndHITL` 也是 10 step、4 agent、顺序推进。
- `TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill` 是 10 step、4 agent、顺序推进，中间有 HITL。
- 当前 Flow 定义使用 `transitions.on_success` 和 `branches`，没有 fan-out / fan-in 的并行 step。
- 当前测试里真实出现的是同一个 agent turn 内的并发 tool call，例如一个 step 内同时发多个 `read_file`。

这次暴露的并发 bug 正是“并发 tool call 记录丢失”，不是“并行 agent step 调度错误”。

并行 agent 相关能力在 delegation 方向有 policy，例如 `max_parallel`，但本次 live Flow review 不等价于“并行 agent fan-out 测试”。如果后续要覆盖并行 agent，需要新增一个专门 case：同一个 parent agent 一次发起多个 `delegate_agent` child run，并验证 `max_parallel`、join、child HITL、resume 后 materialize output。

## 测试覆盖矩阵

| 测试 | 覆盖内容 | 最终状态 |
| --- | --- | --- |
| `TestRealDeepSeekHITLHumanRequestTool` | 真实 DeepSeek 调 `human.request`，runtime 进入 `waiting_human` | 通过 |
| `TestRealDeepSeekHITLRequireConfirmationSnapshot` | 真实 DeepSeek 调需要确认的 `write_file`，保存 action snapshot | 通过 |
| `TestRealDeepSeekHITLRespondsAfterApprovedToolOutput` | 审批后的 tool output replay，模型继续完成，不能重新开 human request | 通过 |
| `TestRealDeepSeekHITLDelegateCompleted` | parent agent delegate 到 child agent，child 完成后 parent 收到结果 | 通过 |
| `TestRealDeepSeekHITLDelegateChildWaiting` | child agent 内部打开 human request，parent 正确挂起等待 | 通过 |
| `TestRealDeepSeekFlowAgentStepCompletes` | Flow 单 agent step 真实调用 DeepSeek 完成 | 通过 |
| `TestRealDeepSeekFlowRoutesToHumanApproval` | Flow 从 agent step 路由到 human approval | 通过 |
| `TestRealDeepSeekLongFlowFourAgentsWithHITL` | 10 step、4 agent、HITL gate 的长 Flow | 通过 |
| `TestRealDeepSeekLongFlowFourAgentsWithToolsAndHITL` | 10 step、4 agent、真实 workspace tools、HITL gate | 通过 |
| `TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill` | 10 step 文件落盘 Flow，带 skill、HITL、跳步读取、search/list/read/write contract | 通过 |

## 关键业务场景

最重要的业务场景是 `TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill`。

这个 case 模拟一个完整文件型 Flow：

1. 前 3 步独立写文件，不允许 `read_file`、`search_file`、`list_dir`。
2. 第 4 步跳步读取 `01-brief.md` 和 `03-constraints.md`，再写 `04-plan.md`。
3. 第 5 步进入 HITL 审批。
4. 审批后第 6 步读取 `04-plan.md`，写 `05-implementation.md`。
5. 第 7 步跳回读取 `02-research.md`，并读取 `05-implementation.md`，写 `06-risk.md`。
6. 第 8 步 `search_file` 搜 `BRIEF-MARKER-101`，再读取 `06-risk.md`，写 `07-test-plan.md`。
7. 第 9 步读取 `03-constraints.md` 和 `07-test-plan.md`，写 `08-release.md`。
8. 第 10 步 `list_dir` 后读取 `01-brief.md`、`04-plan.md`、`08-release.md`，写 `09-final-report.md`。

最终 full run 里，第 10 步证据如下：

```text
agent run: 20260616-210111-flow-research
tool_calls: 5
tool sequence: list_dir, read_file(08-release.md), read_file(01-brief.md), read_file(04-plan.md), write_file(09-final-report.md)
```

最终产物：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/09-final-report.md
```

这证明它不是“模型自己说读了”，而是 runtime 里真实有工具调用记录和落盘文件。

## 真实 DeepSeek 跑出来的问题

### 1. 否定句被误判为正向 read/search/list

真实 DeepSeek 生成过这类文本：

```text
do not call read_file
require no prior reads
no read/search/list
```

旧断言只看到 `read` / `read_file` 关键词，就误判为“模型声称初始步骤读取了文件”。这会让测试因为错误原因失败。

处理方式：

- `claimsPositiveReadSearchOrList` 增加否定语义识别。
- 覆盖 `do not`、`does not`、`without`、`no read/search/list`、`require no prior reads` 等真实输出形态。
- 增加回归测试 `TestClaimsPositiveReadSearchOrListAllowsNegatedDeepSeekPlanWording`。

### 2. `human.request` 缺少 `kind` 时被错误拒绝

真实 DeepSeek 调用 `human_request` 时出现只传 `question`、不传 `kind` 的情况。

旧逻辑：

```go
fmt.Sprint(args["kind"])
```

当 `kind` 缺失时会变成字符串 `"<nil>"`，然后 runtime 拒绝：

```text
unsupported human request kind "<nil>"
```

处理方式：

- 改成 nil-safe 的 `stringArg(args, "kind")`。
- 缺少 `kind` 时默认 `freeform`。
- 增加回归测试 `TestHumanRequestToolDefaultsMissingKindToFreeform`。

### 3. Tool-using Flow 需要明确 per-step tool contract

之前只靠 prompt 要求模型“必须调用某工具”，约束不够强。

现在 long tool Flow 每个工具步骤都有：

```yaml
executor:
  tools:
    - read_file
  tool_input_allowlist:
    read_file:
      path:
        - case/architecture.md
```

这样 runtime 层面会约束：

- 当前 step 暴露哪些工具。
- 当前工具允许哪些输入值。
- 模型如果调用不在 allowlist 内的工具或参数，会被记录为 contract failure。

### 4. ADK 并发 tool call 会丢记录

这是本轮真实 DeepSeek replay 挖出的最重要 runtime 问题。

现象：

- 日志显示 final step 实际执行了 3 个 `read_file`。
- 但 `tool_calls.jsonl` 里只保留了 2 个 `read_file`。
- 测试失败，因为 exact tool contract 要求 `list_dir + 3 read_file + write_file`。

问题原因：

- ADK 工具回调可能并发执行。
- `toolRecords`、`resp.Events`、`resp.AuditEvents`、`resp.LLMCalls` 是共享 slice。
- 这些 slice append 没有同步保护，导致并发写丢记录。

处理方式：

- 抽出 runtime recorder：`toolCallRecorder` 负责 tool calls，`runRecorder` 负责 events、audit、LLM calls。
- 生产路径使用 recorder 统一保护并发 append。
- 增加非 live 回归 `TestRuntimeRecordersPreserveConcurrentAppends`，直接并发写入 tool records、events、audit events、LLM calls，断言不丢记录。
- 增加 `-race` 定向验证，覆盖 recorder 并发写入。
- 真实 DeepSeek targeted rerun 通过。
- 真实 DeepSeek full suite rerun通过。

这个问题不能通过“改松测试”解决，因为业务上 replay 证据必须完整。测试失败是合理的，runtime 必须修。

### 5. Flow step 的 `tools: []` 必须表示“显式禁用所有工具”

真实 DeepSeek 在 `TestRealDeepSeekFlowRoutesToHumanApproval` 里曾经直接调用 `human_request`，导致原本应该完成的 agent step 变成 `waiting_human`。业务上这个 Flow 的审批应由下一个 `human_approval` step 负责，前一个 design agent step 不应该能打开 runtime-native human request。

处理方式：

- Flow executor 现在区分“没有配置 tools”和“显式配置空 tools”。
- `executor.tools: []` 会传递为 `AllowedToolsSet=true` 且 `AllowedTools=[]`。
- runtime 收到这个模式时会禁用 profile tools、workspace tools 和 runtime-native tools。
- 增加非 live 回归：
  - `TestAgentExecutorPassesExplicitEmptyStepAllowedTools`
  - `TestExecutorYAMLTracksExplicitEmptyTools`
  - `TestRuntimeToolAllowlistCanDisableAllTools`

最终真实 DeepSeek full suite 中，`TestRealDeepSeekFlowRoutesToHumanApproval` 通过，agent step `tool_calls=0`，随后 Flow 正常进入 human approval。

### 6. live evidence root 必须按 test case 隔离

真实 targeted rerun 暴露了一个 harness 问题：多个 live case 共用同一个 `XIRA_LIVE_ARTIFACT_ROOT/workspace`，导致 long Flow 的 `case/*.md` 残留被后面的 file-backed Flow 看到，模型把 `architecture.md`、`constraints.md` 等非 artifact 文件写进 `02-research.md`，触发 unknown artifact guard。

这个失败是合理的：file-backed Flow 的 business artifact 不应该引用外部 markdown 文件。但根因不是 Flow 设计，而是 live harness 把不同测试的 workspace 混在一起。

处理方式：

- `XIRA_LIVE_ARTIFACT_ROOT` 仍是总证据目录。
- 每个 live test 单独落到 `<root>/<sanitized-test-name>/workspace|runs|state`。
- 最终 full suite 的证据目录因此分成 10 个 preserved 子目录：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekhitlhumanrequesttool
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekhitlrequireconfirmationsnapshot
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekhitlrespondsafterapprovedtooloutput
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekhitldelegatecompleted
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekhitldelegatechildwaiting
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowagentstepcompletes
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowroutestohumanapproval
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseeklongflowfouragentswithhitl
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseeklongflowfouragentswithtoolsandhitl
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowfileartifactsskipreadwithskill
```

### 7. 只读工具的重复调用与并发调用要按业务语义处理

真实 DeepSeek 有时会对同一个允许的 `search_file` / `read_file` 做重复调用，或者在同一个 turn 内并发发多个 `read_file`。这本身不是业务违规：只要工具名在 step allowlist 内、输入值在 `tool_input_allowlist` 内、必须读取的 set 完整、最终 `write_file` 仍是唯一且正确的目标，就不应该失败。

处理方式：

- `write_file` 仍保持 exact：必须写指定 path，且 write 目标不能漂移。
- `read_file` / `search_file` / `list_dir` 改成 set 语义：必须覆盖 required input set，且 observed input 不能超出 allowlist。
- 未知工具、未知输入、缺失 required read/search/list 仍然失败。

### 8. 否定依赖语句不能误判为正向 read 声明

真实 DeepSeek 生成过：

```text
write_constraints: no dependency on 01-brief.md or 02-research.md (skip-step reads)
```

旧 checker 因为看到 `reads` 关键词误判为“write_constraints 使用了 read/search/list”。这不是业务失败，而是断言没有理解否定依赖语义。

处理方式：

- `claimsPositiveReadSearchOrList` 增加 `no dependency` / `no dependencies` 识别。
- `TestClaimsPositiveReadSearchOrListAllowsNegatedDeepSeekPlanWording` 增加该真实 DeepSeek 输出形态的回归。

### 9. agent-generated HITL resume 后不能残留 waiting interrupt

reviewer 在上一轮 preserved evidence 中发现：agent step 已经是 `status: completed`，但同一个 step 的 `interrupt.status` 仍然是 `waiting_human`。这会让 `flow_run.yaml` 同时表达“已完成”和“仍等待人工”，对 replay/review 工具是不一致的持久化状态。

处理方式：

- `resumeAgentGenerated` 在 terminal 分支同步清理 `s.Interrupt`。
- 覆盖三类 terminal 路径：approved 后 agent completed、approved 后 agent failed、deny/cancel 直接失败。
- 增加非 live 回归：
  - `TestKernelResumeAgentGeneratedCompletedClearsInterrupt`
  - `TestKernelResumeAgentGeneratedFailedClearsInterrupt`
  - `TestKernelResumeAgentGeneratedDeniedClearsInterrupt`
- 本轮真实 DeepSeek full suite 重新跑过，新的 file-backed Flow evidence 中，所有 agent step 都是 `status: completed` 且不再持久化 `interrupt`；唯一剩余的 `interrupt` 属于显式 Flow approval step `approve_plan`，用于保留人工审批问题文本。

新证据：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowfileartifactsskipreadwithskill/state/flow-runs/*/flow_run.yaml
```

核对命令：

```bash
rg -n "status: completed|interrupt:|waiting_human|human_request_ids" \
  /Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804/testrealdeepseekflowfileartifactsskipreadwithskill/state/flow-runs/*/flow_run.yaml
```

本轮结果：没有 `waiting_human` 字符串；agent step 没有 `interrupt` 字段。

## 重要代码位置

本轮与 review 最相关的代码：

```text
/Users/yinwm/work/flowdeck/apps/xira/internal/flow/kernel_resume.go
/Users/yinwm/work/flowdeck/apps/xira/internal/flow/human_request_test.go
/Users/yinwm/work/flowdeck/apps/xira/internal/runtime/service_adk.go
/Users/yinwm/work/flowdeck/apps/xira/internal/runtime/service.go
/Users/yinwm/work/flowdeck/apps/xira/internal/runtime/recorders.go
/Users/yinwm/work/flowdeck/apps/xira/internal/runtime/recorders_test.go
/Users/yinwm/work/flowdeck/apps/xira/internal/runtime/types.go
/Users/yinwm/work/flowdeck/apps/xira/internal/runtime/flow_bridge.go
/Users/yinwm/work/flowdeck/apps/xira/internal/runtime/human_requests.go
/Users/yinwm/work/flowdeck/apps/xira/internal/runtime/human_request_interrupt_test.go
/Users/yinwm/work/flowdeck/apps/xira/internal/runtime/deepseek_hitl_live_test.go
/Users/yinwm/work/flowdeck/apps/xira/internal/flow/types.go
/Users/yinwm/work/flowdeck/apps/xira/internal/flow/executor.go
/Users/yinwm/work/flowdeck/apps/xira/internal/flow/executor_test.go
```

建议 reviewer 重点看：

- `kernel_resume.go`：agent-generated HITL resume 后是否清理 terminal step 的 `Interrupt`。
- `human_request_test.go`：completed / failed / denied 三种 resume terminal 路径是否都有回归。
- `service_adk.go`：ADK tool callback 是否仍有并发写共享状态的风险。
- `service.go`：event/audit/LLM call 记录是否线程安全。
- `human_requests.go`：`human.request` 参数缺省处理和 action snapshot 是否合理。
- `types.go` / `executor.go` / `flow_bridge.go`：`executor.tools`、`tools: []`、`tool_input_allowlist` 是否正确传到 runtime。
- `deepseek_hitl_live_test.go`：live test 是否仍是业务合理约束，而不是为了通过而写死模型文本。
- `human_request_interrupt_test.go`：缺省 `kind` 的回归是否覆盖真实 DeepSeek 行为。

## 保留的证据目录

这些目录不要在 review 结束前删除：

```text
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-163440
/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-20260616-164015
/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-20260616-164423
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-164742
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-165700
/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-20260616-170311
/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804
/Users/yinwm/work/flowdeck/.xira/live-tests/targeted-deepseek-20260616-191602
/Users/yinwm/work/flowdeck/.xira/live-tests/fileflow-deepseek-20260616-192523
```

其中：

- `full-deepseek-20260616-165700`：暴露并发 tool record 丢失问题。日志能看到工具执行了，但 `tool_calls.jsonl` 少一条。
- `file-flow-skill-20260616-170311`：修复并发记录问题后的 targeted file-backed Flow 通过现场。
- `targeted-deepseek-20260616-191602`：暴露 live workspace 未隔离和否定依赖语句 false positive。
- `fileflow-deepseek-20260616-192523`：隔离与 checker 修复后的 targeted file-backed Flow 通过现场。
- `full-deepseek-20260616-205804`：最终完整 live suite 通过现场，是主证据。

## 复现命令

真实 DeepSeek 完整 suite：

```bash
DEEPSEEK_API_KEY="$(tr -d "\r\n" < DEEPSEEK_API_KEY)" \
XIRA_DEEPSEEK_LIVE=1 \
XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-$(date +%Y%m%d-%H%M%S) \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run "TestRealDeepSeek" -v
```

真实 DeepSeek file-backed targeted：

```bash
DEEPSEEK_API_KEY="$(tr -d "\r\n" < DEEPSEEK_API_KEY)" \
XIRA_DEEPSEEK_LIVE=1 \
XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/file-flow-skill-$(date +%Y%m%d-%H%M%S) \
GOCACHE=$(pwd)/.cache/go-build \
go test -count=1 ./apps/xira/internal/runtime -run "TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill$" -v
```

本地回归：

```bash
git diff --check
GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime ./apps/xira/internal/flow
GOCACHE=$(pwd)/.cache/go-build go test -race -count=1 ./apps/xira/internal/runtime -run "TestRuntimeRecordersPreserveConcurrentAppends"
```

本轮最终本地结果：

```text
git diff --check: passed
ok  	github.com/xiramesh/xira/internal/runtime	4.356s
ok  	github.com/xiramesh/xira/internal/flow	3.148s
ok  	github.com/xiramesh/xira/internal/flow	1.165s  # targeted resume interrupt regression
ok  	github.com/xiramesh/xira/internal/runtime	1.779s  # targeted -race recorder regression
```

说明：本轮收尾没有重跑 `./apps/xira/...` 全量包；重点验证范围是 runtime + flow 以及 recorder race 回归。此前在 managed sandbox 下，API 包全量会因为 `127.0.0.1:0` / `httptest` bind 权限受限失败，需要允许本地 bind 后再跑。

## Review Checklist

Reviewer 建议按下面顺序看：

1. 打开最终主证据目录：

   ```text
   /Users/yinwm/work/flowdeck/.xira/live-tests/full-deepseek-20260616-205804
   ```

2. 检查 file-backed Flow 的最终 workspace artifacts：

   ```text
   testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/01-brief.md
   testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/02-research.md
   testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/03-constraints.md
   testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/04-plan.md
   testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/05-implementation.md
   testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/06-risk.md
   testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/07-test-plan.md
   testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/08-release.md
   testrealdeepseekflowfileartifactsskipreadwithskill/workspace/artifacts/flow-files/09-final-report.md
   ```

3. 检查最终 step run：

   ```text
   testrealdeepseekflowfileartifactsskipreadwithskill/runs/20260616-210111-flow-research/tool_calls.jsonl
   testrealdeepseekflowfileartifactsskipreadwithskill/runs/20260616-210111-flow-research/run.yaml
   testrealdeepseekflowfileartifactsskipreadwithskill/runs/20260616-210111-flow-research/events.jsonl
   testrealdeepseekflowfileartifactsskipreadwithskill/runs/20260616-210111-flow-research/audit.jsonl
   ```

4. 确认最终 step 有 5 个工具调用：

   ```text
   list_dir
   read_file artifacts/flow-files/08-release.md
   read_file artifacts/flow-files/01-brief.md
   read_file artifacts/flow-files/04-plan.md
   write_file artifacts/flow-files/09-final-report.md
   ```

5. 检查前 3 步没有 `read_file` / `search_file` / `list_dir`。

6. 检查 `04-plan.md` 的 tool contract table，确认它不是伪造上游读取，而是明确说前三步无读、后续步骤按 contract 读。

7. 检查 human request 文件，确认 HITL gate 有真实 pending -> approved -> resumed 链路。

8. 检查 `service_adk.go` / `service.go` 的并发记录修复，确认 shared slice append 不再无锁。

9. 检查 `human_requests.go` 的 `kind` 缺省行为，确认真实 DeepSeek 不传 `kind` 时会走 `freeform`。

10. 检查 live test 断言是否保持业务合理性，没有为了通过而放松掉关键 contract。

## 当前剩余风险

- 真实 DeepSeek live test 有模型波动和 API 成本，适合作为 release/review gate，不适合每次普通单测都默认跑。
- 模型有时会额外尝试读取有用文件。现在通过 `executor.tools` 和 `tool_input_allowlist` 控制边界，但 reviewer 仍应检查每个 step 的 allowlist 是否符合业务预期。
- replay 证据链很重要。如果未来新增 runtime 记录字段，要同时看 `run.yaml`、`tool_calls.jsonl`、`events.jsonl` 是否一致。

## 最终判断

这轮真实 DeepSeek 测试是有价值的：它不仅证明 Flow + HITL 可以跑通，还实际挖出了 fake model 不容易发现的问题，包括参数缺省、自然语言否定判断、per-step 工具约束、ADK 并发工具记录丢失。

当前最终状态可以进入 review。review 时请以最终主证据目录和本报告为准，不要只看模型输出文本。
