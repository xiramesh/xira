# Xira iLink Delegation RCA - 2026-06-21

## 背景

本次问题来自本地运行时 `~/daming-xira` 的 iLink 会话：

- 用户输入：`你用 code agent 帮我看 **#94 W1-CRON**`
- 会话：`.xira/sessions/ilink/ilink-daming/...conversation_1a4c11eeb951c51a`
- 父 run：`20260621-001838-daming-agent`
- 子 run：`20260621-001845-code-agent-057546c3`

iLink 前端实际展示：

1. `我还在处理，会在有结果或需要你确认时继续更新。`
2. `子任务超时，我会继续整理已获得的信息。`
3. `好，以下是 #94 W1-CRON 的完整分析：...`

这些消息不是前端乱序或误渲染，而是 runtime/progress/delegation 链路按当前契约产生的结果。

## 直接结论

`code-agent` 被设计成“长时间 Claude headless 编排器”，但 runtime 把它当普通 LLM delegate worker 执行：

- 默认 delegation policy 把子任务硬性限制在 120 秒。
- `code-agent` 的“只跑 `claude -p`”只是 PROFILE 软约束，不是工具层硬约束。
- delegate worker runtime contract 只强化“最终必须返回 JSON”，没有强化“必须按 code-agent 模板执行”。
- progress 前端只投递 `failed/timeout/waiting_human/silence`，不投递 `allowed/started/completed`，用户看不到真实执行路径。
- timeout 文案固定写成“整理已获得的信息”，但本次 child run 没有返回结构化结果。
- 父 agent 在 child timeout 后静默 fallback 到自己运行 `claude -p`，最终回答没有披露“code-agent 未完成”。

因此，前端看到的三条消息是设计错配的外显结果，不是单纯前端问题。

## 证据

### 1. 子任务实际被 clamp 到 120 秒

父 run 事件：

```text
agent.delegate.allowed requested_max_duration_ms=7200000 effective_max_duration_ms=120000 policy_max_duration_ms=120000
agent.delegate.timeout status=timeout error="context deadline exceeded"
```

对应源码：

- `apps/xira/internal/agents/profile.go`
  - `DefaultMaxDurationMS` 默认 `30000`
  - `MaxDurationMS` 默认 `120000`
- `apps/xira/internal/runtime/delegation.go`
  - `effectiveMaxDurationMS` 超过 policy max 时被 clamp
  - child run 使用 `context.WithTimeout(... effectiveMaxDurationMS ...)`

本地 `daming-agent` profile 只配置：

```yaml
delegation:
  enabled: true
  allow:
    - code-agent
  max_depth: 1
  max_parallel: 1
```

没有配置 `default_max_duration_ms` 或 `max_duration_ms`，所以命中 runtime 默认 120 秒上限。

### 2. 第一条消息来自 silence notice

`progress.Renderer` 对 `run.silence_notice` 的固定文案：

```text
我还在处理，会在有结果或需要你确认时继续更新。
```

这不是 code-agent 已启动/正在执行的状态，只是 request-bound progress forwarder 在一段时间没有可投递事件后发出的等待提示。

### 3. 第二条消息来自 timeout 固定文案

`progress.Renderer` 对 `agent.delegate.timeout` 的固定文案：

```text
子任务超时，我会继续整理已获得的信息。
```

这个渲染不读取 payload，也不知道 child 是否真的返回了 partial/structured result。本次 child run 状态是 failed，未交付 `delegate_result_v1`。

### 4. progress 可见性不足

当前 progress forwarder 只投递：

```go
agent.delegate.failed
agent.delegate.timeout
run.waiting_human
```

`agent.delegate.allowed` / `agent.delegate.started` / `agent.delegate.completed` 不进入 iLink，因此用户看不到：

- 目标 agent 是谁。
- 请求 7200 秒被压成 120 秒。
- code-agent 是否真正启动。
- code-agent 是否完成还是父 agent fallback。

### 5. code-agent 没按自己的 PROFILE 执行

`~/daming-xira/workspace/agents/code-agent/PROFILE.md` 写明：

- 自己不写代码、不直接改文件。
- 唯一职责是把任务转交给 Claude Code headless。
- 固定模板：

```sh
cd "<repo>" && claude -p --permission-mode bypassPermissions --output-format json "<任务>"
```

但本次 child run `20260621-001845-code-agent-057546c3` 实际没有跑 `claude -p`，而是自己执行了大量 `gh issue view`、`grep`、`cat`、`python3 -c` 等探索命令。

子 run 统计：

```text
tool calls: 56
llm calls: 37
prompt tokens: 1,228,957
status: failed
error: context deadline exceeded
```

这说明 PROFILE 约束没有形成执行层硬约束；模型仍按普通 code/research agent 行为展开。

### 6. worker runtime contract 与 code-agent 业务契约不一致

`delegateWorkerPrompt` 主要约束：

- 你是 Xira delegate worker。
- 最终只返回一个 `delegate_result_v1` JSON。
- 不要包含 runtime-owned 字段。

`delegateWorkerRuntimeContract` 也只追加“bounded parent task + JSON-only”约束。

它没有把 `code-agent` 的关键业务契约转成强约束：

- 必须只跑一次固定 `claude -p` 模板。
- 不允许自己展开 repo inspection。
- 不允许使用 `read_file/search_file/list_dir/write_file` 完成分析。

同时 `code-agent` tools 暴露了：

```yaml
shell.run
command.run
tool_output.read
read_file
list_dir
write_file
search_file
```

工具面过宽，软约束不足，导致模型偏离预期 workflow。

### 7. file tools 与 shell tools 的路径语义不一致

本次 child run 里，`read_file` 读取 `~/work/...` 被解析为：

```text
/Users/yinwm/daming-xira/workspace/~/work/...
```

原因：file tools 的 `resolveWithin` 对用户传入 path 不展开 `~`；相对路径一律拼到 workspace root 下。

而 `shell.run` 执行 `cd ~/work/...` 会交给 shell 展开 `~`，因此同一仓库在 file tools 和 shell tools 中表现不同。

这不是本次 timeout 的主因，但它增加了 tool-call 重试和模型困惑。

### 8. 父 agent 静默 fallback

父 run 在 `delegate_agent` timeout 后继续执行：

```sh
cd ~/work/wanghuan/ai-agent-platform && claude -p --permission-mode bypassPermissions --output-format json "..."
```

最终 iLink 展示的完整分析来自父 `daming-agent` 的 fallback，不是 `code-agent` 的成功结果。

当前 runtime 没有 `agent.delegate.fallback_started` / `fallback_used` 事件，父 agent 最终回答也没有被强制要求披露 fallback 来源，导致用户误以为“code-agent 最终成功完成”。

## 根因分层

### P0：语义根因

`code-agent` 的真实职责是 long-running external worker wrapper，但 runtime delegation 把它当 bounded LLM subagent。

这两个模型冲突：

- wrapper 模型需要几分钟到几十分钟、主要等待 `claude -p`。
- bounded LLM delegate 模型默认 30 秒/120 秒、需要快速 JSON 汇报。

### P1：策略根因

delegation policy 默认上限 120 秒，对代码审查/深度 repo 分析类任务不匹配。用户传入 `7200000ms` 也会被 clamp，但这个 clamp 不对用户可见。

### P1：执行约束根因

`code-agent` 的固定 workflow 没有被工具层 enforced。PROFILE 是软提示，工具暴露仍允许它自己做完整探索。

### P1：可观测性根因

iLink 只看到 timeout 和 silence，不看到 allowed/started/effective timeout/fallback。执行路径被隐藏，用户只能从最终文本猜测。

### P2：文案根因

`agent.delegate.timeout` 文案声称“整理已获得的信息”，但事件本身不保证有可整理的 child result。该文案在 timeout 场景下容易误导。

### P2：路径根因

file tools 不展开用户 path 的 `~`，shell tools 会由 shell 展开 `~`。这导致工具行为不一致，放大模型探索成本。

## 建议修复

### 1. 明确区分 delegate 类型

把 `code-agent` 这类“外部 headless worker wrapper”从普通 delegate worker 中拆出来，至少在 profile/policy 上支持 long-running wrapper：

- 更长默认 timeout，例如 30-120 分钟。
- 独立 worker mode，例如 `external_command_worker`。
- 明确只允许一类 command template。

### 2. 给 `daming-agent` 配显式 delegation timeout

在本地 profile 中显式配置：

```yaml
delegation:
  enabled: true
  allow:
    - code-agent
  max_depth: 1
  max_parallel: 1
  default_max_duration_ms: 7200000
  max_duration_ms: 7200000
```

如果不希望全局放大，应为 `code-agent` 单独引入 per-target policy。

### 3. 收紧 code-agent 工具面

如果它的职责只是调用 Claude Code headless，应考虑只保留：

- `shell.run`
- `tool_output.read`

并禁止 `read_file/search_file/list_dir/write_file/command.run`，减少模型自己做分析的空间。

更强的做法是实现专用 `claude_code.run` tool，由 runtime 负责拼模板、设置 cwd、读取 stdout JSON，而不是让模型手写 shell。

### 4. 强化 delegate worker contract

当 target agent 是 `code-agent` 时，runtime prompt 应明确：

- 必须按 profile 固定模板执行。
- 不要自己 inspect repo。
- 如果没有执行 `claude -p`，最终必须标记失败。

更好的是把这些规则变成工具层验证，而不是 prompt 文本。

### 5. 修正 progress 投递与文案

建议让 iLink 可见：

- `agent.delegate.allowed`：显示目标 agent、effective timeout。
- `agent.delegate.started`：显示子任务已启动。
- `agent.delegate.completed`：显示子任务完成。
- `agent.delegate.timeout`：文案改成“code-agent 超时，未返回结构化结果；将改用 fallback/当前上下文继续处理”。

同时新增 fallback 事件：

- `agent.delegate.fallback_started`
- `agent.delegate.fallback_completed`

### 6. 父 agent fallback 必须披露来源

父 agent 最终回答应强制包含执行路径，例如：

```text
code-agent 在 120 秒上限内超时，未返回结构化结果。以下是我改用 claude -p 重新分析后的结果。
```

### 7. 统一 `~` 路径语义

可选修复：

- file tools 对用户传入 path 支持 leading `~` 展开。
- 或文档/工具 schema 明确禁止 `~`，要求绝对路径。

推荐前者，因为配置 roots 已经支持 `~` 展开，用户 path 保持一致更符合直觉。

## 最小可落地修复顺序

1. 修改本地 `daming-agent` delegation timeout，先避免 code-agent 两分钟必超时。
2. 收紧 `code-agent` tools，只保留 `shell.run` 和 `tool_output.read`。
3. 修改 timeout progress 文案，避免“整理已获得的信息”的错误暗示。
4. 最终回答中披露 fallback 来源。
5. 后续再做 per-target delegation policy、专用 `claude_code.run` tool、`~` path 统一等结构性修复。
