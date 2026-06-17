# 统一会话身份契约:让 runtime 记住"这段对话从哪来"

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:test-driven-development. Steps use checkbox (`- [ ]`) syntax. 实现时严格红-绿-重构:先写失败测试,跑一次确认失败原因符合预期,再写最小实现转绿。

分支:`feature/flow-session-history` → PR。

## 这个 PR 真正要解决的问题

**Flow 调 agent 产生的会话记录落错了目录,跟真实触发渠道脱钩。** 但这只是症状。根因不在 flow 一处,而是 runtime 的会话身份契约本身是错的。

### 根因:`TurnRequest` 把一等公民 `InboundContext` 降级成了 `Channel string + Metadata map[string]string`

runtime 真正消费的会话身份形状是 `channel.InboundContext`(`channel/types.go:5`,13 个强类型字段)。但 runtime 对外输入 `TurnRequest`(`runtime/types.go:17`)却把它拍扁成:

```go
type TurnRequest struct {
    ...
    UserID   string            // ← InboundContext.SenderID 的降级
    Channel  string            // ← InboundContext.Channel 的降级
    Metadata map[string]string // ← chat_id/sender_id/space_id… 全塞字符串 map
}
```

后果:`RunAgent` 进门第一件事(`service.go:219`)就得把扁平字段重新拼回 `InboundContext`:

```go
Context: channel.NewInboundContextWithEntrypoint(req.Channel, req.EntrypointID, req.UserID, req.Metadata),
```

`NewInboundContextWithEntrypoint`(`channel/types.go:46`)从 `Metadata` 这个 `map[string]string` 里按 `firstMetadata(metadata, "chat_id", "room_id", "conversation_id")` 这种**字符串键约定**一个个捞——键名拼错编译器不报错,运行时静默 fallback。

### 这个结构缺陷制造了三个"假 channel"

凡是 runtime 内部派生 agent turn 的地方,都得从强类型上下文降级成扁平 `TurnRequest`,降级过程中就有人手滑塞假 channel:

| 位置 | 病灶 |
|---|---|
| `flow/executor.go:177` | `Channel: "flow"` —— flow 把"编排机制"伪装成"触发来源" |
| `runtime/human_request_resume.go:82` | `Channel: "resume"` —— HITL 恢复也伪造来源 |
| `runtime/delegation_resume.go:68` | `Channel: "resume"` —— 委托恢复同病 |
| `runtime/delegation.go:1032` | `Metadata: map[string]string{}` —— 派生子 run 时 chat/sender 全丢 |

**flow 不是孤例,它是三个假 channel 制造者之一。** 只要 `TurnRequest` 还是扁平的,每加一个派生点就会再造一个假 channel。修 flow 一处治标不治本。

### 为什么是现在做,不留"以后"

flow 的 session 错位一旦靠"flow 包自己存 context + bridge 拆解"的 workaround 修到"能跑",修 `TurnRequest` 契约就再也没有 forcing function 了——`it works` 就不会被碰,技术债越滚越大。三个假 channel、一打 `firstMetadata` 字符串键约定、`service.go:263-266` 的"拼好又拍扁"反向降级,会一直留在代码里。

**趁还没有外部用户(仅作者本人在测),一次性把契约理顺。**

## 目标

把 `TurnRequest` 的会话身份从扁平 `Channel + Metadata` 升级为一等公民 `channel.InboundContext`,让三个派生点(flow / HITL-resume / delegation)从结构上**失去伪造 channel 的能力**。flow 的 session 归属错位是这个统一契约下最显眼的验证场景,但不是唯一受益者。

## 已确认的设计决策

1. **`TurnRequest` 内嵌 `channel.InboundContext`** —— 删掉 `Channel string`、`UserID string`、`Metadata map[string]string` 三个扁平字段,改 `Context channel.InboundContext`。会话身份只此一处真相源。
2. **HTTP API 出新版 endpoint,无兼容期** —— 旧 endpoint(`/api/v1/agent-runs`、`/api/v1/channels/*/messages`)删掉或重定向;新版 body 用 `{context: {channel, chat_id, ...}, message, agent_id, ...}`。作者本人在测,可以直接 breaking。
3. **`RunAgent` 删掉进门拼装** —— `service.go:218-219` 的 `NewInboundContextWithEntrypoint(...)` 整段消失,`req.Context` 直接就是 `InboundContext`。`service.go:263-266` 的"拼好又拍扁写回 req"也随之消失。
4. **三个派生点从强类型重建 context,不再捏 channel** —— flow 从持久化的 `run.Context` 读;HITL/delegation resume 从 run store 里已存的全量 context 读;delegation 从 `ParentBase` 直接构造(字段一一对应)。
5. **flow 的触发上下文持久化进 `flow_run.yaml`** —— `flow.Run` 加 `Context *channel.InboundContext`,`Advance`/`Resume` 跨进程能读回。
6. **`flow_bridge` 不再需要拆解映射** —— `TurnRequest` 自己就是 `InboundContext`,`flow.AgentTurnRequest.Context` 直接一行赋值过去。

## 关键实现约束(从代码摸到的事实)

- **`Advance`/`Resume` 只收 `flowRunID`,从 Store 重载 `Run`**(`kernel.go:199`、`kernel.go:527`)。触发上下文必须持久化进 `flow.Run`,否则跨进程后续步骤拿不到。
- **`flow.Run` 持久化为 `flow_run.yaml`**(`store.go:322`)。新加字段带 yaml+json tag。
- **`runtimeEventBase`**(`events.go:14-36`)有 13 个强类型上下文字段,跟 `InboundContext` 几乎一一对应——delegation 派生时从它构造 `InboundContext` 是无损的。
- **run store 里存了全量 context** —— HITL/delegation resume 时 `s.runs.Load(runID)` 拿到的 run 带完整 `InboundContext`(当前重建为 `runtimeEventBase` 存着),可以无损重建。
- **entrypoint resolve 本就正确** —— `implicitDefinition`(`entrypoints/registry.go:232`)按触发 channel 造 `cli-default`/`feishu-default`,channel 自动正确,不动。
- **session 路径拼法本就正确** —— `session/file_store.go` 按完整 `InboundContext` 拼,不动。
- **flow 包可 import channel** —— 依赖方向干净(flow 不 import runtime,但可 import channel)。

## 透传链路(B 方案后)

```
入口(CLI / API / feishu / ilink)
  ↓ 构造 channel.InboundContext{Channel, ChatID, SenderID, SpaceID, …}(强类型)
runtime.TurnRequest{Context: inboundCtx, Message, AgentID, …}
  ↓ req.Context 直接就是 InboundContext,进门不拼装
runtime.RunAgent
  ↓ sessions.Allocate(Context: req.Context) → 落 sessions/<真channel>/<entrypoint>/chat_<真chat>__sender_<真sender>/…
flow 链路:
  StartRequest{Context} → flow.Run{Context} 持久化 → buildTurnRequest 用 run.Context → AgentTurnRequest{Context} → bridge 一行赋值 → TurnRequest{Context}
HITL/delegation resume:
  runs.Load(runID) → run 里已有全量 context → 直接构造 TurnRequest{Context}(不再 Channel:"resume")
```

## File Structure

Modify:

- `apps/xira/internal/runtime/types.go` — `TurnRequest`:删 `Channel/UserID/Metadata` 扁平字段,改 `Context channel.InboundContext`。
- `apps/xira/internal/runtime/service.go` — `RunAgent`(`:216`):删进门 `NewInboundContextWithEntrypoint` 拼装(`:218-219`)与反向拍扁写回(`:263-266`);直接用 `req.Context`。`StartFlow`(`:224`)透传 `req.Context` 给 `flow.StartRequest`。
- `apps/xira/internal/runtime/delegation.go`(`:1026`) — `childReq` 从 `req.ParentBase` 构造 `InboundContext`,删空 `Metadata`。
- `apps/xira/internal/runtime/human_request_resume.go`(`:77`) — 从 `run` 重建 `InboundContext`,删 `Channel:"resume"`。
- `apps/xira/internal/runtime/delegation_resume.go`(`:63`) — 同上,删 `Channel:"resume"`。
- `apps/xira/internal/runtime/flow_bridge.go` — `RunAgent`(`:30`):`TurnRequest.Context = req.Context` 一行赋值,删掉 Channel/UserID/Metadata 逐字段映射。
- `apps/xira/internal/flow/kernel.go` — `StartRequest`(`:46`)加 `Context channel.InboundContext`;`Start` 把它传进 `CreateRun`,nil 时 fallback `{Channel:"cli"}`。
- `apps/xira/internal/flow/types.go` — `Run`(`:283`)加 `Context *channel.InboundContext`(指针,yaml+json tag,`omitempty`)。
- `apps/xira/internal/flow/store.go` — `CreateRunRequest`(`:46`)加 `Context`;`CreateRun` 写入 run。
- `apps/xira/internal/flow/executor.go` — `AgentTurnRequest`(`:56`)加 `Context channel.InboundContext`;`buildTurnRequest`(`:165`)删 `Channel:"flow"`,用 `run.Context`(nil 时 fallback `{Channel:"cli"}`),flow_run_id/flow_id/flow_step_id 仍进 `Metadata`。
- `apps/xira/internal/channelrunner/feishu/runner.go`(`:217`) — 构造 `channel.InboundContext` 替代扁平 `Channel+UserID+Metadata`。
- `apps/xira/internal/channelrunner/ilink/runner.go`(`:613`) — 同上;`buildMetadata` 里强类型字段(chat_id/space_id/account/…)搬进 `InboundContext`,仅 IM 私有字段留 `Raw`。
- `apps/xira/cmd/xira/main.go` — `agent run`(`:167`)与 `flow run`(`:353`)构造 `channel.NewInboundContext("cli", "", nil)`。
- `apps/xira/internal/api/server.go` — `agentRuns`/`channelMessages` handler body 改嵌套 `{context:{...}, message, ...}` 格式;旧扁平字段不兼容(无用户,直接 breaking)。
- `apps/xira/internal/runtime/service_test.go` 及各 `*_test.go` — 所有 `TurnRequest{Channel:..., UserID:..., Metadata:...}` 构造点改为 `TurnRequest{Context: channel.InboundContext{...}}`。
- `docs/guide/xira-flow-v0-usage.zh.md` — 删"Flow 默认 channel 通常是 flow",改说明会话跟触发渠道走。

Create / Test:

- `apps/xira/internal/runtime/types_test.go`(或就近) — `TurnRequest` 序列化为嵌套 JSON 的新契约。
- `apps/xira/internal/flow/executor_test.go` — `buildTurnRequest` 用 `run.Context`。
- `apps/xira/internal/runtime/service_test.go` — flow agent step 落盘到正确 channel/session 路径 + HITL resume 不再造 `resume` channel。

## 边界:不做什么

- 不改 `entrypoints` 解析 / `implicitDefinition` 逻辑 —— 本就正确。
- 不改 `session` 路径拼法 —— 本就按完整 `InboundContext` 拼。
- 不保留旧 HTTP body 兼容 —— 无外部用户,breaking。
- 不做"flow 包存 context + bridge 拆解"的 workaround —— 那是 A 方案的妥协,本 PR 选 B 直接统一契约。
- 不在本 PR 做多 Flow registry(`flows:` 配置)—— 另一份 plan 的事。

---

### Task 1: `TurnRequest` 升级为 `InboundContext` 契约

**Files:** `runtime/types.go`, `runtime/service.go`, 三个内部派生点

- [ ] **Step 1: 写失败测试 — `TurnRequest` 内嵌 `InboundContext`**

  在 `runtime` 包测试里构造 `TurnRequest{Context: channel.InboundContext{Channel:"feishu", ChatID:"oc_x", SenderID:"u_y"}, Message:"hi"}`,断言其 JSON 序列化为 `{"context":{"channel":"feishu","chat_id":"oc_x",...},"message":"hi",...}`(嵌套,不再有顶层 `channel`/`user_id`/`metadata`)。

- [ ] **Step 2: 跑测试确认失败**

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run TestTurnRequest -v
  ```

- [ ] **Step 2.5: 改 `TurnRequest` 结构**

  `types.go:17` 删 `UserID/Channel/Metadata`,加 `Context channel.InboundContext`。`Message/AgentID/EntrypointID/SessionID/AllowedTools*` 保留(它们不是会话身份)。

- [ ] **Step 3: 改 `RunAgent` 删进门拼装**

  `service.go:218-225` 的 `channel.InboundEnvelope` 直接用 `Context: req.Context`,删 `NewInboundContextWithEntrypoint`。`service.go:263-266` 的 `req.Channel=...; req.UserID=...` 反向写回删掉(context 已是真相源,不需要同步回扁平字段)。下游所有读 `req.Channel`/`req.UserID` 的地方改读 `req.Context.Channel`/`req.Context.SenderID`。

- [ ] **Step 4: 治三个假 channel 派生点**

  - `delegation.go:1026`:`childReq.Context = channel.NormalizeInboundContext(channel.InboundContext{Channel:req.ParentBase.Channel, SenderID:req.ParentBase.SenderID, ChatID:req.ParentBase.ChatID, ...})`,删空 `Metadata`。
  - `human_request_resume.go:77`:从 `run`(run store 里已有全量 context)重建 `InboundContext`,删 `Channel:"resume"`。
  - `delegation_resume.go:63`:同上,从 `childRun` 重建,删 `Channel:"resume"`。

- [ ] **Step 5: 跑测试转绿,commit**

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -v
  git commit -m "refactor(runtime): unify TurnRequest identity into InboundContext"
  ```

---

### Task 2: flow 包携带并持久化触发上下文

**Files:** `flow/kernel.go`, `flow/types.go`, `flow/store.go`, `flow/executor.go`

- [ ] **Step 1: 写失败测试 — StartRequest 携带 Context 并持久化进 Run**

  在 `flow/executor_test.go`(若不存在则新建)构造 `StartRequest{Context: channel.InboundContext{Channel:"feishu", ChatID:"oc_x", SenderID:"u_y"}, ...}`,`Kernel.Start` 后断言返回的 `run.Context` 非 nil 且 `Channel=="feishu"`。

- [ ] **Step 2: 跑确认失败**

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -run TestStartRequest -v
  ```

- [ ] **Step 3: 加字段 + 透传**

  - `StartRequest`(kernel.go:46)加 `Context channel.InboundContext`。
  - `Run`(types.go:283)加 `Context *channel.InboundContext \`yaml:"context,omitempty" json:"context,omitempty"\``。
  - `CreateRunRequest`(store.go:46)加 `Context *channel.InboundContext`;`CreateRun` 写入 run。
  - `Kernel.Start` 把 `req.Context` 传进 `CreateRun`,nil 时 fallback `channel.NewInboundContext("cli","",nil)`。

- [ ] **Step 4: executor 用 run.Context 取代硬编码**

  - `AgentTurnRequest`(executor.go:56)加 `Context channel.InboundContext`。
  - `buildTurnRequest`(executor.go:165)删 `Channel:"flow"`,改 `Context: runContext(run)`(`run.Context` nil 时 fallback `{Channel:"cli"}`)。flow_run_id/flow_id/flow_step_id 仍进 `Metadata`。

- [ ] **Step 5: 跑测试,commit**

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/flow -v
  git commit -m "feat(flow): carry and persist trigger context through Run"
  ```

---

### Task 3: flow_bridge 一行赋值

**Files:** `runtime/flow_bridge.go`

- [ ] **Step 1: 写失败测试 — bridge 直接透传 context**

  构造 `flow.AgentTurnRequest{Context: channel.InboundContext{Channel:"feishu", ChatID:"oc_x", SenderID:"u_y"}}`,断言 bridge 产出的 `TurnRequest.Context.Channel=="feishu"`、`.ChatID=="oc_x"`、`.SenderID=="u_y"`(不再需要拆 chat_id/sender_id 进 Metadata)。

- [ ] **Step 2: 跑确认失败**

- [ ] **Step 3: 实现**

  `flowBridge.RunAgent`(flow_bridge.go:30)构造 `TurnRequest` 时 `Context: req.Context`,删掉 `Channel/UserID/Metadata` 逐字段映射。

- [ ] **Step 4: 跑测试,commit**

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run TestFlowBridge -v
  git commit -m "refactor(runtime): flow bridge passes InboundContext directly"
  ```

---

### Task 4: 外部入口(CLI / API / IM runners)构造 InboundContext

**Files:** `cmd/xira/main.go`, `api/server.go`, `channelrunner/feishu/runner.go`, `channelrunner/ilink/runner.go`

- [ ] **Step 1: 写失败测试 — CLI flow run 默认走 cli channel**

  在 `cmd/xira/main_flow_test.go` 加测试:`xira flow run <flow> --entrypoint ad_hoc --input request=hi`,断言产生的 agent run 其 session 路径落在 `sessions/cli/...`(而非 `sessions/flow/...`)。

- [ ] **Step 2: 跑确认失败**

- [ ] **Step 3: 实现 CLI**

  `agent run`(main.go:167)与 `flow run`(main.go:353)构造 `channel.NewInboundContext("cli","",nil)`。`Service.StartFlow` 把 `req.Context` 透传给 `flow.StartRequest`。

- [ ] **Step 4: 实现 IM runners**

  `feishu/runner.go:217` 与 `ilink/runner.go:613`:把 `Channel+UserID+Metadata` 改为构造 `channel.InboundContext`(Channel/ChatID/ChatType/SenderID/SpaceID/SpaceType/Account/MessageID 等强类型字段)。`ilink` 的 `buildMetadata` 里 IM 私有字段(message_type/seq/context_token/…)搬进 `InboundContext.Raw`,强类型字段搬进结构体字段。

- [ ] **Step 5: 实现 API 新版 body**

  `server.go` 的 `agentRuns`/`channelMessages` handler:body 改嵌套 `{context:{channel, chat_id, sender_id, ...}, message, agent_id, entrypoint_id}`,`json.Decode` 进 `TurnRequest`(其 `Context` 字段自动吃嵌套)。删 `channelMessages` 里 `req.Channel != channelName` 的扁平校验,改校验 `req.Context.Channel`。

- [ ] **Step 6: 跑测试,commit**

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/cmd/xira ./apps/xira/internal/api ./apps/xira/internal/channelrunner/... -run TestFlow -v
  git commit -m "feat: entrypoints construct InboundContext for unified identity"
  ```

---

### Task 5: 非实时测试断言会话落盘到正确路径

**Files:** `runtime/service_test.go`

- [ ] **Step 1: 写失败测试**

  `TestFlowAgentStepPersistsSessionMessagesInTriggerChannel`:启动 flow(显式传 `Context{Channel:"feishu",ChatID:"oc_x",SenderID:"u_y"}`),Advance 后找 agent step,断言 `messages.jsonl` 落在 `sessions/feishu/<entrypoint>/chat_*oc_x*__sender_u_y*/agents/<agent>/messages.jsonl`,而非 `sessions/flow/...`。

  `TestHumanRequestResumeDoesNotForgeResumeChannel`:触发 HITL,resume 后断言 session 路径 channel 不是 `resume`(应为原始触发 channel)。

- [ ] **Step 2: 跑确认失败/通过**

  若 Task 1-4 已正确,应直接转绿;否则定位透传断点。

- [ ] **Step 3: commit**

  ```bash
  GOCACHE=$(pwd)/.cache/go-build go test -count=1 ./apps/xira/internal/runtime -run "TestFlowAgentStepPersistsSessionMessages|TestHumanRequestResume" -v
  git commit -m "test: assert flow and resume sessions land in trigger channel"
  ```

---

### Task 6: DeepSeek 实时测试断言 + 文档

**Files:** `runtime/deepseek_hitl_live_test.go`, `docs/guide/xira-flow-v0-usage.zh.md`

- [ ] **Step 1: 实时测试加断言**

  `assertPersistedSessionMessagesForRun`:对完成的 `agent_run_id`,load run 后断言 `AgentHistory` 非空,且 messages 落盘路径 `Context.Channel != "flow"`(应为 live test 设的触发 channel)。

- [ ] **Step 2: 跑实时测试(有 key 时)**

  ```bash
  DEEPSEEK_API_KEY="$(tr -d '\r\n' < DEEPSEEK_API_KEY)" \
  XIRA_DEEPSEEK_LIVE=1 \
  XIRA_LIVE_ARTIFACT_ROOT=/Users/yinwm/work/flowdeck/.xira/live-tests/flow-session-history-$(date +%Y%m%d-%H%M%S) \
  GOCACHE=$(pwd)/.cache/go-build \
  go test -count=1 ./apps/xira/internal/runtime -run 'TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill$' -v
  ```

  预期:evidence 含 `sessions/**/messages.jsonl`,且不在 `flow/` 子树下。

- [ ] **Step 3: 文档修正**

  `xira-flow-v0-usage.zh.md`:删/改"Flow 默认 channel 通常是 flow",改为说明 Flow 会话跟触发渠道走,CLI 触发落 `sessions/cli/...`,IM 触发落对应 channel。补充 HTTP API 新版 body 格式说明。

- [ ] **Step 4: commit**

  ```bash
  git commit -m "test/docs: assert and document unified trigger-channel session ownership"
  ```

---

## Acceptance Criteria

- `TurnRequest` 不再有 `Channel/UserID/Metadata` 扁平字段;会话身份唯一来源是 `Context channel.InboundContext`。
- `executor.go` 不再出现 `Channel: "flow"`;`human_request_resume.go`/`delegation_resume.go` 不再出现 `Channel: "resume"`。
- `service.go` 不再有 `NewInboundContextWithEntrypoint(req.Channel, ...)` 进门拼装,也不再有反向拍扁写回 `req.Channel/UserID`。
- Flow 经 CLI 触发,agent 会话落 `sessions/cli/...`(与 `xira agent` 一致)。
- Flow/API/IM 携带真实 Context,agent 会话落对应 channel 目录(含真实 chat/sender)。
- HITL/delegation resume 不再造 `resume` channel,会话归属跟原始触发渠道一致。
- `Advance`/`Resume` 跨进程仍能读到触发上下文(Context 持久化进 flow_run.yaml)。
- 非实时测试断言路径正确;DeepSeek 实时测试断言 messages.jsonl 落盘且非 flow channel。
- HTTP API body 为嵌套 `{context:{...}}` 格式,文档已更新。

## 与大 plan 的关系

本 PR 直接统一 runtime 会话身份契约(`TurnRequest` → `InboundContext`),根治"假 channel 制造机"。`docs/superpowers/plans/2026-06-17-xira-flow-registry-session-history.md` 的 Flow registry(多 Flow 配置)部分仍由那份 plan 独立推进,二者可并行;那份 plan 里 Task 5 的 session history 措辞被本 PR 取代。
