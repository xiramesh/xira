# Xira Completion Outbox、幂等与可靠消费 RFC v0

- 状态：Proposed
- Gate：Managed Execution v0 / Gate 0B
- 关联：#202、#204、#205、#206、#194、#197
- 前置 Accepted RFC：`xira-triggered-turn-mailbox-rfc-v0.zh.md`

## 1. 摘要

本 RFC 冻结 Managed Execution completion 从 Execution terminal 到 Triggered Turn，再到 channel final
的可靠性契约。

v0 对外只承诺：

```text
durable terminal outbox
  + at-least-once dispatch
  + unique ContinuationID
  + idempotent logical Run creation
  + independently retryable final delivery
```

它不承诺跨 SQLite、Agent 工具副作用和外部 channel 的 exactly-once。所谓“幂等 Run 创建”只保证同一
`ContinuationID` 对应一个逻辑 continuation `run_id`；进程在不可判定边界崩溃后，Agent attempt、普通
工具调用或 channel message 仍可能被重试。没有下游 idempotency support 时，用户可见消息最多只能
at-least-once。

本 RFC 同时修正 #202 proposal 中最危险的歧义：`get/log/wait` 读到 terminal result 只是**观察**，绝不
等于 completion 已被消费。若当前 Run 要接管 completion，必须显式 durable claim；只有 Runtime 确认该
Run 已可靠处理后才能 ack。失败、超时或 steering 必须 release/requeue。

## 2. 已核实的当前代码事实

本节是设计输入，不是对未来实现的猜测。

1. `runtime.RunStore` 使用 `run.json` 和多个 JSONL/JSON 文件；锁只在当前进程内，`SaveRun` 不是跨记录
   事务，也没有 CAS。
2. `RunAgent` 先创建 artifacts 目录，Agent loop 结束后才 `SaveRun` 完整 `TurnResponse`。现有
   `InitRun` 不能充当 durable Run reservation。
3. native DeepSeek 路径把 provider `tool_call_id` 写进 `ToolCallRecord.ID`；缺失时
   `executeToolCall` 会生成随机 UUID。ADK 路径同样从 `FunctionCallID()` 读取后进入该 fallback。
4. `humanrequest.Store` 有 pending/running/completed/failed 和启动恢复，可借鉴 sealed transition；但它是
   JSON 文件 + process-local mutex，running claim 没有 lease/CAS，不满足本 Gate。
5. `channel.OutboundEnvelope` 已有 `ID`，`Manager.Emit` 能 exact-entrypoint 路由；但当前 Feishu 等 adapter
   没有用 envelope ID 建立可证明的发送幂等。
6. HITL resume 的 `deliverResumeFinal` 是明确的 best-effort：Run 先持久化，发送失败只写日志。Managed
   Execution continuation 可以复用 outbound mechanics，不能复用这个可靠性语义。
7. 仓库当前没有 SQLite driver。引入事务型 managed-execution store 是后续 implementation 的显式依赖，
   不是现有 RunStore 的隐藏能力。

因此，给现有 JSON 文件再加一个 mutex，或者把“发送成功”塞进 `Run.Status`，都无法解决 crash window。

## 3. 目标、非目标与术语

### 3.1 目标

- Execution terminal 与 completion handling 分离，三条故障链可独立排障。
- terminal 成为 authoritative 后，completion 要么最终 handled/suppressed，要么停在可检查、可重试的
  dead letter；不得 silent loss。
- dispatcher 在任何事务边界 crash，重启后仍能从 durable state 继续。
- 同一 create key 不会启动两个 Execution；同一 ContinuationID 不会创建两个逻辑 Agent Run。
- 观察、接管、处理确认、final 发送各有精确含义。
- user steering 永远不 ack completion，且不与 durable mailbox 混队列。

### 3.2 非目标

- 不实现 Store、Supervisor、worker 或 channel renderer。
- 不在本 Gate 冻结 Go struct、SQL column、index 名或公开 tool schema。
- 不保证外部命令自身副作用 exactly-once。
- 不保证 LLM、任意普通工具调用或外部 channel exactly-once。
- 不把 local-first 单 daemon 扩成 Redis/Kafka/多节点 consensus。
- 不修改 #204 的 Triggered Turn identity、mailbox key、user priority 或 exact-route 语义。

### 3.3 术语

| 术语 | 唯一含义 |
|---|---|
| terminal | Execution 已进入 #206 定义的不可逆终态 |
| armed | terminal 与 completion outbox 在同一事务中提交 |
| dispatched | mailbox 已 durable 接受该 ContinuationID；不是 Agent 已处理 |
| claimed | 某 owner 持有未过期 lease；不是成功 |
| run_created | ContinuationID 已绑定唯一逻辑 run_id；不是 Run 已完成 |
| handled | Triggered Run 的结果已可靠持久化，completion 无需重新跑 Agent |
| final_sent | channel adapter 返回 accepted/success 且本地状态已提交；不是用户已读 |
| suppressed | policy/identity/route 等确定性原因决定不运行或不发送 |
| dead_letter | 自动重试耗尽或遇到不可自动修复错误，等待 admin；不是 suppressed/success |

禁止使用无修饰的 `delivered`。代码、日志、指标和文档必须写清 `outbox_dispatched`、`handled` 或
`final_sent`。

## 4. 持久化与事务边界

### 4.1 决策：同一 local SQLite WAL 承载 managed-execution authoritative state

v0 使用 `state_dir` 下单一 managed-execution SQLite WAL 数据库，逻辑上至少容纳：

- Execution authoritative record；
- terminal completion outbox；
- Triggered Turn mailbox/handling record；
- continuation logical Run binding；
- final delivery state 与审计 transition。

具体表名和列名在 Gate 0 全部 Accepted 后由 implementation issue 冻结。这里冻结的是**同一事务域**，不是
DDL。Run/session/artifact 文件仍可保留，但不能作为 completion claim/ack 的事务真相。

SQLite 配置必须至少满足：WAL、`synchronous=FULL`、foreign keys on、busy timeout、有界连接数、schema
migration、私有文件权限和启动 integrity/error reporting。state_dir 所在文件系统若不能提供 SQLite 需要的
locking/durability，启动必须 fail closed，不能降级为内存队列。

### 4.2 terminal 与 outbox 必须同事务提交

唯一合法的 terminal 提交流程是：

```text
BEGIN IMMEDIATE
  CAS Execution non-terminal -> terminal(revision=N)
  INSERT completion_outbox(ContinuationID, terminal facts, state=pending)
COMMIT
```

- CAS 没赢：读取已有 terminal；同一事实是幂等 replay，不同事实是 conflict/corruption。
- outbox insert 没成功：整个 terminal transition 回滚。
- commit 结果对调用者不可判定：按 ExecutionID/terminal revision 重读，不得盲目生成新 ContinuationID。
- terminal 对观察者可见时，outbox 必须已经存在；不允许“先 terminal，稍后 best-effort enqueue”。

#206 必须让 worker/restart harvester 通过这个统一操作提交 completed/failed/timed_out/lost。任何旁路直接写
terminal 都违反本 RFC。

### 4.3 outbox 到 mailbox 也在同一数据库事务中交接

dispatcher 对一条 outbox record：

```text
Transaction A:
  CAS outbox pending/expired_claim -> dispatch_claimed(owner, token, lease_until)

Transaction B:
  verify outbox is dispatch_claimed by the same unexpired token
  INSERT mailbox(ContinuationID, mailbox_key, sequence, state=queued)
    ON UNIQUE ContinuationID: verify canonical digest is identical
  CAS outbox dispatch_claimed(token) -> dispatched
```

逻辑上 outbox 与 mailbox 仍分离：前者证明 terminal producer 没丢事件，后者执行 #204 的 per-chat-key
串行策略。物理上同库使“mailbox 已存在”与“outbox dispatched”可原子提交。

Transaction A 的 claim 单独提交，让多个 dispatcher 不会长时间占有 SQLite write transaction；若在 A 与 B
之间 crash，lease 到期后回到 pending。Transaction B 中 mailbox insert 与 outbox dispatched 必须原子，
因此不存在“mailbox 已入队但 outbox 仍永久 pending”的半状态。

重复 dispatch 遇到相同 `ContinuationID + digest` 返回原 mailbox record；相同 ID 不同 digest 必须报
conflict 并进入可见的 integrity dead letter，不能覆盖旧 payload。

`NotifyTriggerAvailable` 只是 commit 后的低延迟 wake；wake 丢失不影响正确性，startup/periodic scan
仍以数据库为真相。

## 5. 稳定身份与幂等创建

### 5.1 Execution create key

模型不可传入、覆盖或猜测 create key。Runtime 在真正执行 tool 前注入：

```text
create_key = SHA-256(
  "xira.execution.create.v0\0" + authoritative_run_id + "\0" + authoritative_tool_call_id
)
```

数据库保存 `create_key`、canonical execution spec、`spec_digest` 和 ExecutionID，并对 create key 建唯一
约束。

canonical spec 必须包含会改变外部执行语义的字段：executor kind、program/argv 或 shell command、canonical
cwd、显式 environment snapshot/policy references、max runtime、termination policy、sandbox/profile version。
managed execution 不得把“启动时碰巧继承的整个 process env”当作未记录输入；secret value 不落明文，但要
保存可比较的 secret version/content digest。只影响本次调用观察体验的字段（如 yield window、preview
limit、log cursor）不进入 digest。

行为冻结为：

| 场景 | 行为 |
|---|---|
| 首次 create | 事务内保存 spec/digest，persist-before-spawn 后返回唯一 ExecutionID |
| 相同 key + 相同 digest | 返回原 Execution，不 spawn 第二个进程 |
| 相同 key + 不同 digest | `idempotency_conflict`，返回两个 digest 的安全摘要，不执行 |
| 并发相同 create | 唯一约束决定一个 winner；其他读取 winner |
| provider replay | 因 run_id + tool_call_id 相同而命中原 Execution |
| tool_call_id 缺失 | managed create fail closed；不得使用随机 fallback 后 spawn |

现有 `executeToolCall` 对普通工具可继续随机补 ID；managed `command.run/shell.run` 迁移后必须在 spawn 前
验证 authoritative ID。若未来存在非模型内部 caller，必须由可信调用边界提供 durable request ID，并使用
带不同 namespace 的 create-key 规则；禁止把自由输入的 idempotency key 暴露给模型。

### 5.2 ContinuationID

每个 Execution terminal revision 只有一个 ContinuationID：

```text
ContinuationID = SHA-256(
  "xira.execution.completion.v0\0" + ExecutionID + "\0" + terminal_revision
)
```

terminal revision 由 authoritative store 分配。v0 Execution 只允许一个不可逆终态，因此正常值只有一个；
revision 仍用于检测 corruption/migration replay，而不是允许 terminal 来回改写。

outbox 和 mailbox 都保存 canonical trigger digest。ContinuationID 相同但 kind/status/result/artifact/route digest
不同是 integrity conflict，不是普通 replay。

### 5.3 idempotent logical Run creation

mailbox claim 后，coordinator 必须先在数据库事务中完成：

```text
ContinuationID UNIQUE -> continuation_run_id
handling_state: claimed -> run_created
```

再调用 #194 的 `RunTriggeredAgent` core。`continuation_run_id` 由 Kernel 生成并稳定复用，不能让每次 retry
调用当前 `NewRunID` 得到新 ID。

crash 后重试遵循：

- binding 不存在：创建一次；
- binding 已存在、Run 尚无可靠结果：复用同一 logical run_id，创建新的 attempt 记录；
- Run 结果已经可靠持久化：只推进 handled/delivery，不再调用 Agent；
- 同一 ContinuationID 绑定不同 run_id：integrity error，立即 dead letter。

这提供 effectively-once 的**逻辑 Run identity**，不提供 exactly-once Agent execution。每次实际调用 Agent
必须有单独 `attempt_id` 供排障；Runtime-managed Execution create 因 `(run_id, tool_call_id)` 可幂等，其他
无幂等能力的工具仍可能在 crash/retry 后重复副作用，必须在产品文案和审计中诚实呈现。

## 6. 三个独立状态机

Execution terminal state 由 #206 冻结。本 RFC 只要求它不可被 completion failure 反向修改。

### 6.1 Outbox dispatch state

```text
pending
  -> dispatch_claimed
  -> dispatched

dispatch_claimed --lease expired--> pending
pending/dispatch_claimed --deterministic integrity error--> dead_letter
```

`dispatched` 表示 mailbox row 已 durable 存在。它不是 handled，更不是 final_sent。

### 6.2 Mailbox handling state

```text
queued
  -> handling_claimed
  -> run_created
  -> handled

handling_claimed/run_created --steered or retryable failure--> queued
queued/handling_claimed --policy/identity/route invalid--> suppressed
queued/handling_claimed/run_created --automatic retry exhausted--> dead_letter
```

约束：

- 同一 `(EntrypointID, Channel, ChatID, SenderID)` 最多一个有效 handling claim/active continuation。
- user message 可以 steering active Triggered Run；steering release/requeue，不计失败次数，且 user turn 先跑。
- `run_created` 不是 ack 点。只有 Triggered Run outcome 已可靠保存后才能进入 `handled`。
- verified completed + non-empty final：`handled`，并在同一事务中保存 immutable final body/ref + digest、
  创建/推进 delivery pending。
- verified intentional silence：`handled`，delivery=`not_required`；这不是 suppressed。
- durable `waiting_human`：原 completion 已可靠转换成 HumanRequest，可 `handled`，delivery=`not_required`；后续
  HITL 由自己的状态机负责，不能重跑原 completion 生成第二个请求。
- Run failed、timeout、ErrSteered、Run outcome 持久化失败：不得 handled；release/requeue 或最终 dead letter。
- `suppressed` 只用于确定性 policy/identity/route/unsupported-trigger 决策，并保存 sealed reason。

### 6.3 Final delivery state

```text
not_ready
  -> pending
  -> send_claimed
  -> final_sent

send_claimed --error/lease expired--> retryable_failed -> pending
pending/send_claimed/retryable_failed --route/policy invalid--> suppressed
retryable_failed --automatic retry exhausted--> dead_letter
not_ready -> not_required
```

delivery record 保存稳定 `envelope.ID = ContinuationID`（必要时加固定 final suffix），每次 retry 使用同一个
ID。adapter 若支持官方 idempotency key，必须映射它；不支持时，send 成功后、本地 commit 前 crash 可能导致
重复消息，这是无法伪装消失的 at-least-once 边界。

`final_sent` 的精确定义是：adapter 返回 success/accepted，且本地 CAS 已提交。它不表示用户已读，也不表示
第三方平台一定只创建一条消息。

delivery failure 只改变 delivery state：

- 不改 Execution terminal；
- 不把 handling 从 handled 退回 queued；
- 不重跑 Agent；
- 不重新执行外部命令。

每次 send claim 前都重新执行 #204 的 exact-entrypoint route、identity 和 current-policy 校验；校验失败走
sealed suppression，不 fallback 到同 channel 的其他 runner/owner。

## 7. `get/log/wait`、claim 与 ack

### 7.1 默认观察不消费

`execution.control(get|log|wait)` 全部是 read-only observation。即使 `wait` 返回完整 terminal result，也不
设置 consumed/suppressed/handled，不取消 outbox，不删除 mailbox。

理由很简单：模型读到结果后，当前 Run 仍可能失败、超时、被 steering，甚至没生成 final。把 read 当 ack
会制造 silent loss。

因此，如果当前 Run 只调用 `wait`，automatic continuation 仍可被创建。这不是重复 bug，而是调用者没有
取得 durable ownership 的安全结果。

### 7.2 显式 take-over claim

v0 需要一个显式“当前 Run 接管 terminal completion”的 control action；implementation 可在 Gate 0 后确定
wire 名，本文称 `claim_terminal`。它必须：

- 只允许当前 active Run，且该 Run 必须通过 Execution visibility/policy 校验并绑定同一
  `(EntrypointID, Channel, ChatID, SenderID)`；它可以是 origin Run，也可以是同一 conversation 中用户后来
  发起的 successor Run；
- Runtime 注入 claimant run_id，模型不能指定其他 claimant；
- 对已 terminal Execution 建立 durable claim token、owner、version、lease_until；
- 返回 terminal facts；
- outbox/mailbox 保留，只在有效 claim 期间暂停 automatic continuation；
- claim 冲突返回当前安全状态，不偷取未过期 lease。

claim lease 需要 heartbeat。模型看不到 raw token，也不能直接调用 ack。

origin/successor Run 与 mailbox coordinator 争用的不是两把独立锁，而是以 `ContinuationID` 为键的同一份
durable completion ownership。terminal transaction 创建 available ownership；当前 Run 的 take-over 与
coordinator 的 handling claim 都通过它 CAS。dispatcher 可以继续把 outbox durable 交接到 mailbox，但有效
Run ownership 存在时 coordinator 不得启动 Triggered Run。

### 7.3 Runtime-owned ack

只有 Runtime 根据 claiming Run outcome 自动 ack：

| claiming Run outcome | completion 行为 |
|---|---|
| verified completed + final 持久化 | handled；进入独立 delivery |
| verified intentional silence | handled；delivery not_required |
| durable waiting_human | handled；交给 HITL 状态机 |
| failed / timeout / persistence error | release，outbox/mailbox 可继续 |
| ErrSteered / process crash / lease expired | release/requeue，不计业务失败 |

ack 必须 CAS 匹配 `(ContinuationID, claimant_run_id, claim_token, version)`。旧 owner 在 lease 过期后返回，不能
ack 新 owner 的工作。

如果 automatic mailbox continuation 已先 claim，当前 Run 不能再 take-over；反之当前 Run 持有有效 claim
时 coordinator 不启动 Triggered Run。两者竞争由数据库唯一 active-claim invariant 决定，不靠内存 Router
时序猜测。

## 8. Lease、CAS、重试与 poison

### 8.1 Lease 与恢复默认值

v0 默认：

- claim lease：60 秒；
- heartbeat：每 20 秒，且不得晚于剩余 lease 的 1/3；
- startup recovery scan：服务 ready 前执行一次；之后每 10 秒扫描到期 claim/pending work；
- 所有时间判断使用数据库记录的 UTC timestamp；CAS/version 决定 owner，不用 wall-clock 单独决定正确性。

lease duration 可由本地配置调大，但 heartbeat 必须保持不超过 lease/3。调小不得破坏正常 GC pause/IO stall
容忍；v0 implementation 应给出安全下限。

lease acquire/renew/release/ack 都必须用 monotonic `version` + opaque claim token 做 CAS。仅按
`state='claimed'` 更新会让过期 worker 覆盖新 owner，禁止。

### 8.2 自动重试

各 phase 独立计数：`dispatch_attempts`、`handling_attempts`、`delivery_attempts`，不能混成一个 attempts。

默认 backoff：

```text
delay = full-jitter(min(5m, 1s * 2^(attempt-1)))
```

- outbox dispatch 的 transient store/busy failure 持续重试；第 12 次起触发告警，但不因次数丢进 dead
  letter。只有 digest/schema/state corruption 这类 deterministic failure 才立即 dead letter。
- handling 的真实 Agent/verification 失败最多 5 次；继续自动跑会重复费用和普通工具副作用，第 5 次后
  dead letter。
- final delivery 至少重试 24 小时且至少 12 次；两项都满足仍失败才 dead letter。其间始终只重发已持久化
  final，不重跑 Agent。
- admin `retry` 在校验状态后把对应 phase 重置为可调度，并新开 audit record；不得删除历史 attempts。
- ErrSteered、正常 user priority 延迟和 lease-expiry recovery 不消耗各 phase 的业务失败额度；另记
  recovery metric。
- SQLite busy/短暂 channel/network/LLM 错误为 retryable。
- digest mismatch、非法状态转换、未知 schema/kind 为 deterministic integrity failure，可立即 dead letter。
- policy/identity/route 明确失效为 suppressed，不拿重试假装有希望；若 admin 修复配置，必须显式 unsuppress/retry。

这些阈值都不是删除阈值。dead letter 永久可检查，直到 admin retry/suppress 或 retention policy 在明确
审计后清理。

### 8.3 suppression reason

至少使用 sealed reason：

```text
entrypoint_unavailable
route_mismatch
sender_unauthorized
conversation_unavailable
unsupported_trigger
policy_denied
admin_suppressed
```

自由文本 error 只能作补充，不能代替 machine-readable reason。`dead_letter` 不能伪装成
`admin_suppressed`。

## 9. Admin inspection、指标与 retention

admin 至少能按 ExecutionID、ContinuationID、origin run_id、continuation run_id、mailbox key 查询：

- 三个状态机的当前 state/version/lease/next_attempt；
- canonical digests 和安全 spec 摘要；
- 每 phase attempts、last_error、dead-letter reason；
- terminal/result/artifact refs；
- delivery envelope ID、target 摘要和 final_sent 时间；
- 完整 transition audit，不含 secret/env value/无界 stdout。

至少暴露：

- pending/claimed/dead-letter count（按 phase）；
- oldest pending age；
- lease expiry/recovery count；
- idempotency hit/conflict；
- handling retry/suppressed reason；
- final delivery retry/final_sent；
- completion 从 terminal 到 handled、从 handled 到 final_sent 的 latency。

未到 `dispatched` 的 outbox、未到 handled/suppressed 的 mailbox、未到 final_sent/not_required/suppressed 的
delivery 永不自动 GC。终态 metadata 默认至少保留 30 天；artifact retention 由 #206 冻结，但清 artifact 后
record 必须保留 tombstone/digest，不能让 replay 变成“从没发生”。

## 10. Crash-window 证明

| crash 点 | 重启后的权威判断与动作 |
|---|---|
| process spawn 前 | persist-before-spawn record 存在；#206 决定 spawn/recovery |
| terminal CAS 前 | Execution 仍 non-terminal；#206 worker/harvester 继续 |
| terminal + outbox commit 结果未知 | 按 ExecutionID/revision 重读；存在即复用同一 ContinuationID |
| outbox claim 后、mailbox insert 前 | Transaction A 已 commit、B 未 commit；lease 到期后重试同一 ID |
| mailbox insert 后、outbox state 前 | 同事务保证二者同时 commit；不存在半状态 |
| mailbox claim 后、Run binding 前 | lease 到期回 queued |
| Run binding commit 后、Agent 前 | 复用同一 logical run_id，创建新 attempt |
| Agent 返回后、Run outcome commit 前 | 仍未 handled；可能重跑 attempt，不创建第二个 logical run_id |
| handled 后、delivery pending 前 | 同事务推进；不存在 handled 却无 delivery record |
| channel send 前 | retry 同一 envelope ID |
| channel accepted 后、本地 final_sent 前 | retry可能重复；这是公开的 at-least-once channel 边界 |
| final_sent commit 后 | 不再自动发送 |

这里没有“恰好一次”魔法。可靠性来自 durable truth、unique identity、CAS、lease recovery 和把不可判定边界
明确暴露出来。

## 11. Race / fault-injection contract test matrix

以下状态转换属于 contract code，所有 case/branch 100% 覆盖；包级覆盖率仍需满足 AGENTS.md §5。

| 测试 | 必须证明 |
|---|---|
| terminal transaction fault at every statement/commit | terminal 可见必有 outbox；不存在半提交 |
| 100 concurrent same create key/same spec | 一个 ExecutionID、一次 spawn |
| same create key/different canonical spec | 明确 conflict，旧 Execution 不被覆盖 |
| missing provider tool_call_id | spawn 前 fail closed，无随机 key |
| provider tool-call replay after restart | 返回原 Execution |
| 100 concurrent same ContinuationID dispatch | 一个 mailbox row、digest 一致 |
| outbox dequeue/insert/commit crash injection | restart 后不丢、不双 mailbox |
| claim lease expires while stale owner returns | stale token 的 renew/release/ack 全部 CAS 失败 |
| same mailbox key user/trigger race | user 优先；最多一个 active；trigger 仍 durable |
| user steers Triggered Run | 不 ack、不计业务失败、User Turn 后 requeue |
| `wait` observes terminal then Run fails | automatic continuation 仍可处理 |
| current origin/successor Run claim then fails/times out/crashes | lease release/expiry 后 requeue |
| current origin/successor Run claim then verified complete | completion handled；不创建 Triggered Run |
| 100 replayed Run-create requests | 一个 continuation run_id；attempt 可审计 |
| crash at Run binding/Agent/outcome boundaries | 不创建第二个 logical run_id；未持久化 outcome 不误 handled |
| waiting_human outcome | completion handled 一次；HumanRequest 不重复创建 |
| handling fails 5 times | dead letter 可检查；Execution terminal 不变 |
| final send fails for 24h and at least 12 attempts | delivery dead letter；不重跑 Agent/Execution |
| channel accepted then local commit crash | 使用同 envelope ID 重试；测试明确允许 adapter 不幂等时重复 |
| invalid entrypoint/sender/session/route | sealed suppression，fail closed，无 fallback |
| admin retry dead letter | phase 重启、旧 attempts/audit 保留 |
| startup with expired claims | ready 前恢复；10 秒 scan 覆盖运行期 expiry |
| retention after artifact cleanup | identity/digest/tombstone 仍可防 replay |

恢复链测试必须使用真实 `BuildScope -> persisted SessionScope -> inboundContextFromScope -> Manager.Emit` 数据，
不能手搓无 prefix 的干净 sender/chat ID。

## 12. 被拒绝的方案

### A. JSON 文件 + process-local mutex

拒绝。无法原子提交 terminal/outbox/mailbox/Run binding，也无法用 lease token 阻止 stale owner。

### B. terminal 后 best-effort enqueue goroutine

拒绝。terminal commit 后 crash 会永久丢 completion，是本 Gate 要消灭的 silent-loss window。

### C. `wait` 返回就标 consumed

拒绝。读取之后 Run 仍可能失败/steer；把观察当 ack 是倒果为因。

### D. Run 创建成功就删 mailbox

拒绝。Run 可能尚未执行、尚未持久化 outcome，甚至只创建了目录。`run_created` 不是 handled。

### E. delivery 失败就重跑 Triggered Agent

拒绝。会重复 LLM/tool 副作用。delivery 只重发已持久化 final。

### F. Redis/Kafka

拒绝。单 daemon/local-first v0 不需要分布式基础设施；同一 SQLite 事务域更容易证明正确。

### G. 宣称 exactly-once

拒绝。channel accepted 与本地 commit 不能原子；外部命令、LLM 和普通工具也不都支持 idempotency。

## 13. 与 #204/#206/#194/#197 的边界

- #204：继续拥有 Triggered Turn typed input、mailbox identity、coordinator/user priority、exact route 和
  fail-closed policy。本 RFC不改变这些 invariant。
- #206：定义 Execution worker ownership、persist-before-spawn、terminal facts、artifact/result refs 和 restart
  harvest；但所有 terminal transition 必须调用本 RFC 的 atomic terminal+outbox operation。
- #194：提供 typed Triggered Turn 共用的 Agent Loop Core。本 RFC 在 core 外做 Run binding、attempt、claim、
  handling ack 和 delivery。
- #197：复用 `OutboundEnvelope`/exact-entrypoint `Manager.Emit` mechanics。delivery target 仍是原 conversation，
  不改成 Cron Principal；adapter 对 envelope ID 的幂等能力必须显式声明。

Gate 0 全部 Accepted 前，不创建 implementation issue，不合入 Store/Supervisor/tool migration 代码。

## 14. 本 Gate Accepted 条件

- [ ] 评审接受同一 local SQLite 事务域与 terminal + outbox 原子提交。
- [ ] 评审接受 outbox、mailbox handling、final delivery 三状态机及无歧义术语。
- [ ] 评审接受 `get/log/wait` 只观察；显式 claim + Runtime-owned ack。
- [ ] 评审接受 run_id + authoritative tool_call_id create key、canonical spec conflict 和 missing-ID fail closed。
- [ ] 评审接受 ContinuationID 唯一与 idempotent logical Run binding，不宣称 Agent exactly-once。
- [ ] 评审接受 60s lease、20s heartbeat、10s recovery scan、CAS token/version。
- [ ] 评审接受分 phase retry、full-jitter backoff、phase-specific poison threshold 与 sealed suppression。
- [ ] 评审接受 final delivery 独立重试，`final_sent` 不等于用户已读/全局 exactly-once。
- [ ] race/fault-injection matrix 覆盖 #205 issue 的六项验收。
- [ ] Accepted 后将最终结论回灌 #202、更新 #205 checklist 并关闭 #205。

当前文档是 Proposed。以上条件经评审完成前，不得把它当成 implementation contract。
