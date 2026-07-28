# Xira Completion Outbox、幂等与可靠消费 RFC v0

- 状态：Proposed
- Gate：Managed Execution v0 / Gate 0B
- 关联：#202、#204、#205、#206、#194、#197
- 前置 Accepted RFC：`xira-triggered-turn-mailbox-rfc-v0.zh.md`

## 1. 摘要

本 RFC 冻结 Managed Execution completion 从 Execution terminal 到 Triggered Turn，再到 channel final 的可靠性契约。

v0 对外只承诺：

```text
durable terminal outbox
  + at-least-once dispatch
  + unique ContinuationID
  + idempotent logical Run creation
  + independently retryable final delivery
```

它不承诺跨 SQLite、Agent 工具副作用和外部 channel 的 exactly-once。所谓“幂等 Run 创建”只保证同一
`ContinuationID` 对应一个逻辑 continuation `run_id`；不可判定边界 crash 后 attempt/tool/channel message
仍可能重试。没有下游 idempotency support 时，用户可见消息最多只能 at-least-once。

本 RFC 同时修正 #202 proposal 中最危险的歧义：`get/log/wait` 读到 terminal result 只是**观察**，绝不等于 completion 已被消费。当前 Run 要接管必须显式 durable claim；Runtime 确认可靠处理后才 ack。失败、超时或
steering 必须 release/defer/requeue，达到独立上限才 circuit-break。

## 2. 已核实的当前代码事实

本节是设计输入，不是对未来实现的猜测。

1. `runtime.RunStore` 使用 `run.json` 和多个 JSONL/JSON 文件；锁只在当前进程内，`SaveRun` 不是跨记录
   事务，也没有 CAS。
2. `RunAgent` 先创建 artifacts 目录，Agent loop 结束后才 `SaveRun` 完整 `TurnResponse`。现有
   `InitRun` 不能充当 durable Run reservation。
3. native DeepSeek 路径把 provider `tool_call_id` 写进 `ToolCallRecord.ID`；缺失时
   `executeToolCall` 会生成随机 UUID。ADK 路径同样从 `FunctionCallID()` 读取后进入该 fallback。
4. `humanrequest.Store` 顶层 RequestStatus 是 pending/resolved/failed；ResumeStatus 才有
   waiting_response/pending/running/completed/failed 和启动恢复。可借鉴 sealed transition，但它是 JSON 文件
   + process-local mutex，running claim 没有 lease/CAS，不满足本 Gate。
5. `channel.OutboundEnvelope` 有 `ID` 字段，但 constructor/Normalize 不生成 ID；`notify_owner` 会设
   `ID=tool_call_id`，HITL `deliverResumeFinal` 不设，当前 Feishu/iLink adapter 都不消费该字段建立发送幂等。
   本 RFC 需要补齐的是统一 producer 语义和 adapter capability，而不是复用一条已经闭环的幂等链。
6. HITL resume 的 `deliverResumeFinal` 是明确的 best-effort：Run 先持久化，发送失败只写日志。Managed
   Execution continuation 可以复用 outbound mechanics，不能复用这个可靠性语义。
7. 仓库当前没有 SQLite driver。引入事务型 managed-execution store 是后续 implementation 的显式依赖，
   不是现有 RunStore 的隐藏能力。

因此，给现有 JSON 文件再加一个 mutex，或者把“发送成功”塞进 `Run.Status`，都无法解决 crash window。

## 3. 目标、非目标与术语

### 3.1 目标

- Execution terminal 与 completion handling 分离，三条故障链可独立排障。
- terminal 成为 authoritative 后，completion 必须处于可解释的 queued/deferred/handled/suppressed/dead-letter
  状态；用户持续活跃允许它等待，但等待必须可观测且不得无限重跑 Agent 产生费用/副作用。
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
| attempt_running | 一个有界 Agent attempt 持有 fenced execution ownership；不是 handled |
| user_activity_deferred | 因 user steering 暂停自动重跑，等待 quiet gate；不是成功或丢失 |
| handled | Triggered Run 的结果已可靠持久化，completion 无需重新跑 Agent |
| final_sent | channel adapter 返回 accepted/success 且本地状态已提交；不是用户已读 |
| suppressed | policy/identity/route 等确定性原因决定不运行或不发送 |
| dead_letter | 自动重试耗尽或遇到不可自动修复错误，等待 admin；不是 suppressed/success |
| infrastructure_blocked | Runtime 级存储健康覆盖态；record 保持原 durable state、暂停调度，不是 record state |

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
  INSERT completion_outbox(ContinuationID, ExecutionID, terminal facts, state=arming)
  CAS Execution non-terminal -> terminal(completion_id=ContinuationID, state_version++)
    -- database trigger rejects terminal when its matching outbox is absent/mismatched
  CAS completion_outbox arming -> pending
COMMIT
```

上面三条写操作必须在**同一个**数据库事务中；trigger 必须匹配同一 ExecutionID、ContinuationID 和 `state=arming` 的 outbox，不能只检查“任意 outbox 存在”。

- CAS 没赢：读取已有 terminal；同一事实是幂等 replay，不同事实是 conflict/corruption。
- outbox insert 没成功：整个 terminal transition 回滚。
- commit 结果对调用者不可判定：按 ExecutionID/ContinuationID 重读，不得盲目生成新 ID。
- terminal 对观察者可见时，outbox 必须已经存在；不允许“先 terminal，稍后 best-effort enqueue”。

#206 必须让 worker/restart harvester 通过这个统一操作提交 completed/failed/timed_out/lost。外键 + trigger
等数据库约束必须让“terminal 没有 matching outbox”无法 commit，不能只靠调用约定阻止旁路写入。

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
之间 crash，recovery scan 用数据库 UTC 时间判断 expiry + grace，并 CAS 回 pending；操作行清空 owner/token/
lease 字段，旧值写 audit。Transaction B 中 mailbox insert 与 outbox dispatched 必须原子，因此不存在
“mailbox 已入队但 outbox 仍永久 pending”的半状态。

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

数据库保存 `create_key`、canonical request spec、`request_spec_digest`、首次 create 的 immutable effective
policy snapshot/digest 和 ExecutionID，并对 create key 建唯一约束。

`request_spec_digest` 必须带持久版本：v0 为 `SHA-256("xira.execution.request-spec.v0\0" + canonical_spec_bytes)`；同版本 replay 才按该 validator 比较，migration 保留旧 validator，禁止用新 canonicalizer 重算旧 digest。

request spec 包含 caller 请求的 executor kind、program/argv 或 shell command、cwd、environment refs、requested
max runtime 等语义输入。首次 create 再解析 canonical cwd、effective max runtime、termination/sandbox policy
并冻结 policy snapshot；后续 replay 不用“今天的 policy”重算旧 digest。managed execution 不得把隐式
process env 当未记录输入；secret value 不落明文，但保存可比较的 version/content digest。yield/preview/cursor
等观察字段不进入 request digest。

这里冻结的是外部 Execution 的 create-time policy；Triggered Turn 是否仍可运行/发送，继续按 #204 做 `current policy ∩ original snapshot` 的只收紧校验。两者发生在不同 phase，不得互相替代。

行为冻结为：

| 场景 | 行为 |
|---|---|
| 首次 create | 事务内保存 spec/digest，persist-before-spawn 后返回唯一 ExecutionID |
| 相同 key + 相同 request digest | 重新做当前 visibility/control authorization 后返回原 Execution；policy 演进不制造 conflict |
| 相同 key + 不同 request digest | 无论原 Execution 是否 terminal 都返回 `idempotency_conflict`，不覆盖/重启 |
| 并发相同 create | 唯一约束决定一个 winner；其他读取 winner |
| provider replay | 因 run_id + tool_call_id 相同而命中原 Execution |
| tool_call_id 缺失 | managed create fail closed；不得使用随机 fallback 后 spawn |

现有 `executeToolCall` 对普通工具可继续随机补 ID；managed `command.run/shell.run` 迁移后必须在 spawn 前
验证 authoritative ID。若未来存在非模型内部 caller，必须由可信调用边界提供 durable request ID，并使用
带不同 namespace 的 create-key 规则；禁止把自由输入的 idempotency key 暴露给模型。

### 5.2 ContinuationID

每个 v0 Execution 只有一个 ContinuationID：

```text
ContinuationID = SHA-256(
  "xira.execution.completion.v0\0" + ExecutionID
)
```

v0 只允许一次不可逆 terminal transition；普通 `state_version` 仅服务 CAS，不进入 ContinuationID。schema
migration 不得因为重写版本号改变已存在的 ContinuationID；terminal facts 的 digest 用于检测 replay/corruption。

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
必须有单独 `attempt_id`，并审计本 attempt 调过的 tool name/ID/outcome/idempotency class；Runtime-managed
Execution create 因 `(run_id, tool_call_id)` 可幂等，其他工具在 5 次 handling attempt 下最坏可产生 5 倍
副作用，不能宣传成最多一次。

## 6. 三个独立状态机

Execution terminal state 由 #206 冻结。本 RFC 只要求它不可被 completion failure 反向修改。

### 6.1 Outbox dispatch state

```text
pending
  -> dispatch_claimed
  -> dispatched

dispatch_claimed --lease expired + grace/recovery CAS--> pending
pending/dispatch_claimed --deterministic integrity error--> dead_letter
```

`dispatched` 表示 mailbox row 已 durable 存在。它不是 handled，更不是 final_sent。

### 6.2 Mailbox handling state

```text
queued
  -> handling_claimed
  -> run_created
  -> attempt_running
  -> handled

handling_claimed/run_created/attempt_running --retryable failure--> queued
attempt_running --steered #1/#2--> user_activity_deferred
attempt_running --steered #3--> dead_letter(steering_starvation)
user_activity_deferred --new user activity--> user_activity_deferred(reset quiet watermark)
user_activity_deferred --quiet eligible--> queued
queued/handling_claimed/user_activity_deferred --policy/identity/route invalid--> suppressed
queued/handling_claimed/run_created/attempt_running --retry exhausted--> dead_letter
```

约束：

- 同一 `(EntrypointID, Channel, ChatID, SenderID)` 最多一个有效 handling claim/active continuation。
- Runtime 为每个 mailbox key durable 保存 `last_user_activity_at`，每次接受 user message 时用数据库 UTC 更新；
  deferred record 只有在该 key 无 active/pending user turn，且 scanner 读到
  `db_now_utc >= last_user_activity_at + 30s` 时才 eligible。后续 user message 只重置 quiet watermark，不增加
  steering 次数；periodic scanner 让资格随数据库当前时间推进，不依赖新的业务写入。
- mailbox handling record 的 `auto_steer_streak` 只在一个**已启动** Triggered attempt 以 ErrSteered 结束时增加：第 1/2 次 deferred，
  第 3 次进入 `steering_starvation`。成功、suppressed、admin retry 或 authorized successor 显式 take-over 会
  清零 active streak；`total_steered` 与 transition audit 永久保留。steering 不计普通 handling failure。
- `run_created` 不是 ack 点。只有 Triggered Run outcome 已可靠保存后才能进入 `handled`。
- verified completed + non-empty final：`handled`，并在同一事务中保存 immutable final body/ref + digest、
  创建/推进 delivery pending。
- verified intentional silence：`handled`，delivery=`not_required`；这不是 suppressed。
- durable `waiting_human`：原 completion 已可靠转换成 HumanRequest，可 `handled`，delivery=`not_required`；后续
  HITL 由自己的状态机负责，不能重跑原 completion 生成第二个请求。
- Run failed、timeout、ErrSteered、Run outcome 持久化失败：不得 handled；按上述 circuit breaker release、
  deferred/requeue 或最终 dead letter。
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
ID。现有 Feishu/iLink adapter 不消费 envelope ID，也没有可证明的平台 idempotency mapping；v0 因而必须把
send 成功、本地 commit 前 crash 后的重复消息列为硬边界。未来 adapter 若支持官方 key，才可显式声明并映射。

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

当前 Run take-over 后也不能一直持有 short claim：claim transaction 必须把 ownership 绑定到该 Run 已有的
fenced attempt token 和 effective deadline，使用 §8.1 的 attempt 规则续到 outcome/ack。

### 7.3 Runtime-owned ack

只有 Runtime 根据 claiming Run outcome 自动 ack：

| claiming Run outcome | completion 行为 |
|---|---|
| verified completed + final | 与 §6.2 同事务保存 immutable final body/ref+digest、handled、delivery pending |
| verified intentional silence | 同事务保存 handled subtype、delivery not_required |
| durable waiting_human | 同事务引用已持久 HumanRequest、保存 handled subtype；交给 HITL 状态机 |
| failed / timeout / persistence error | release，outbox/mailbox 可继续 |
| ErrSteered / process crash / lease expired | steering 走 circuit breaker；其余 release/requeue，不误 ack |

ack 必须 CAS 匹配 `(ContinuationID, claimant_run_id, claim_token, version)`。旧 owner 在 lease 过期后返回，不能
ack 新 owner 的工作。

如果 automatic mailbox continuation 已先 claim，当前 Run 不能再 take-over；反之当前 Run 持有有效 claim
时 coordinator 不启动 Triggered Run。两者竞争由数据库唯一 active-claim invariant 决定，不靠内存 Router
时序猜测。

## 8. Lease、CAS、重试与 poison

### 8.1 Lease 与恢复默认值

v0 区分短 claim 和 Agent attempt ownership，不能拿同一个 60 秒 lease 包住整段 LLM Run：

- dispatch/queued handling 短 claim：60 秒 lease，15 秒 heartbeat；运行期 recovery scan 每 30 秒处理
  `lease_until + 30s grace < db_now_utc` 的 claim。
- `run_created -> attempt_running` 时原子换成 attempt fencing token、daemon instance ID 和
  `attempt_deadline = db_now_utc + effective Agent max duration`。同一 live daemon 不在 deadline + 60 秒 grace
  前偷取 attempt；heartbeat 每 15 秒用于可观测/卡死诊断，但不能单凭一次 heartbeat 延迟启动重跑。
- Agent context 必须受 attempt_deadline 约束；每个 tool call/checkpoint/outcome commit 前重新验证 fencing token。
  失去 ownership 的旧 attempt 立即取消，不能再调下一个工具或 ack；已在外部系统进行中的调用不可撤销，
  仍属于公开的 at-least-once 副作用边界。
- attempt fence 是数据库单调递增 version/token。scanner 在 deadline + grace 后可由**同一 live daemon** CAS
  fence overdue attempt、触发本地 attempt registry cancel 并重新排队；旧 attempt 此后所有 tool boundary/outcome
  校验都失败。已经发出的外部调用与 fence CAS 不能原子化，仍属于上述公开边界。
- 单 daemon 必须由 #206 的 state-dir singleton ownership 证明。新 daemon 取得 singleton 后，可在 startup
  reconciliation 立即回收旧 daemon instance 的 claim/attempt，不必等旧 deadline。

startup reconciliation 在 service ready 和 periodic scanner 启动前串行、分批、幂等完成；中途 crash 时下次
启动从 durable state 重做。expired claim reset 的组合 CAS 必须同时匹配 state/version/token 且使用数据库 UTC
判断 expiry；winner 由 CAS 决定，时间只决定“是否有资格竞争”。reset 清空操作行 owner/token/lease，旧值留
audit。

host clock jump/休眠是 local SQLite 的已知限制：grace 吸收小抖动，向前跳仍可能触发竞争，向后跳会延迟恢复；
fencing token 保证至多一个 owner 能继续 commit/调用下一工具。测试使用可控 DB clock 覆盖两种跳变。

### 8.2 自动重试

各 phase 独立计数：`dispatch_attempts`、`handling_attempts`、`delivery_attempts`，不能混成一个 attempts。

默认 backoff：

```text
delay = full-jitter(min(5m, 1s * 2^(attempt-1)))
```

- `SQLITE_BUSY/LOCKED` 按 transient 重试；第 12 次告警，持续 24 小时或 1000 次后停止 hot retry、进入 Runtime
  级 `infrastructure_blocked`：所有 record 保持进入前的 durable state，暂停新调度，health/admin 暴露
  reason、started_at、last_success_at 与各 phase oldest age；存储 health probe 恢复后继续原 record。
  `FULL/IOERR/READONLY/CORRUPT` 立即 fail-stop/健康检查失败并给出稳定诊断；数据库不可写时绝不能伪造
  per-record dead letter。进程级 watchdog/restart 属于 #206/deployment；只有可读写数据库内的单 record
  digest/schema/state corruption 才进入该 completion 的 deterministic dead letter。
- handling 的真实 Agent/verification 失败最多 5 次；继续自动跑会重复费用和普通工具副作用，第 5 次后
  dead letter。
- final delivery 至少重试 24 小时且至少 12 次；两项都满足仍失败才 dead letter。其间始终只重发已持久化
  final，不重跑 Agent。
- admin `retry` 在校验状态后把对应 phase 重置为可调度，并新开 audit record；不得删除历史 attempts。
- ErrSteered 走独立 steering circuit breaker；正常 user priority 延迟和 lease-expiry recovery 不消耗普通
  handling failure 额度，另记 recovery metric。
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
`admin_suppressed`。dead-letter reason 另含 `steering_starvation`、`retry_exhausted`、
`persistent_record_error`，不得混入 suppression 枚举。

## 9. Admin inspection、指标与 retention

admin 至少能按 ExecutionID、ContinuationID、origin run_id、continuation run_id、mailbox key 查询：

- 三个状态机的当前 state/version/lease/next_attempt；
- canonical digests 和安全 spec 摘要；
- 每 phase attempts、auto_steer_streak/total_steered、last_error、dead-letter reason；
- terminal/result/artifact refs；
- delivery envelope ID、target 摘要和 final_sent 时间；
- Runtime infrastructure health overlay 与 reason/started_at/last_success_at；
- 完整 transition audit，不含 secret/env value/无界 stdout。

至少暴露：

- pending/claimed/dead-letter count（按 phase）；
- oldest pending age、oldest dispatch_claimed age；
- lease expiry/recovery count；
- idempotency hit/conflict；
- handling retry/suppressed reason；
- final delivery retry/final_sent；
- completion 从 terminal 到 handled、从 handled 到 final_sent 的 latency。

`handling=handled && delivery=dead_letter` 可在 admin/UI 派生显示为 `handled_but_undeliverable`，但不得新增第四个 authoritative 组合状态；两个正交状态机仍各自推进和重试。

未到 `dispatched` 的 outbox、未到 handled/suppressed 的 mailbox、未到 final_sent/not_required/suppressed 的
delivery 永不自动 GC。终态 metadata 默认至少保留 30 天；artifact retention 由 #206 冻结，但清 artifact 后
record 必须保留 tombstone/digest，不能让 replay 变成“从没发生”。

## 10. Crash-window 证明

| crash 点 | 重启后的权威判断与动作 |
|---|---|
| process spawn 前 | persist-before-spawn record 存在；#206 决定 spawn/recovery |
| terminal CAS 前 | Execution 仍 non-terminal；#206 worker/harvester 继续 |
| terminal + outbox commit 结果未知 | 按 ExecutionID/ContinuationID 重读；存在即复用同一 ID |
| outbox claim 后、mailbox insert 前 | A 已 commit、B 未 commit；expiry+grace 组合 CAS 清 owner/token 后重试 |
| mailbox insert 后、outbox state 前 | 同事务保证二者同时 commit；不存在半状态 |
| mailbox claim 后、Run binding 前 | lease 到期回 queued |
| Run binding commit 后、Agent 前 | 复用 logical run_id，CAS 创建 fenced attempt |
| attempt 运行超过短 lease | attempt_deadline ownership 仍有效，不被 short scanner 双跑 |
| Agent 返回后、Run outcome commit 前 | 仍未 handled；可能重跑 attempt，不创建第二个 logical run_id |
| handled 后、delivery pending 前 | 同事务推进；不存在 handled 却无 delivery record |
| channel send 前 | retry 同一 envelope ID |
| channel accepted 后、本地 final_sent 前 | retry可能重复；这是公开的 at-least-once channel 边界 |
| final_sent commit 后 | 不再自动发送 |
| startup reconciliation 中途 crash | periodic scanner 尚未启动；下次 startup 幂等重扫 |

这里没有“恰好一次”魔法。可靠性来自 durable truth、unique identity、CAS、lease recovery 和把不可判定边界
明确暴露出来。

## 11. Automated race / fault-injection contract test matrix

以下状态转换属于 contract code，所有 case/branch 100% 覆盖；包级覆盖率仍需满足 AGENTS.md §5。

| 测试 | 必须证明 |
|---|---|
| terminal transaction fault at every statement/commit | terminal 可见必有 outbox；不存在半提交 |
| 100 concurrent same create key/same spec | 一个 ExecutionID、一次 spawn |
| same create key/different canonical spec | 明确 conflict，旧 Execution 不被覆盖 |
| same request replay after policy change | authorization 通过后返回原 Execution；不因重算 policy digest conflict |
| missing provider tool_call_id | spawn 前 fail closed，无随机 key |
| provider tool-call replay after restart | 返回原 Execution |
| 100 concurrent same ContinuationID dispatch | 一个 mailbox row、digest 一致 |
| outbox dequeue/insert/commit crash injection | restart 后不丢、不双 mailbox |
| claim lease expires while stale owner returns | stale token 的 renew/release/ack 全部 CAS 失败 |
| same mailbox key user/trigger race | user 优先；最多一个 active；trigger 仍 durable |
| user steers Triggered Run 1/2/3 次且持续发消息 | activity 只重置 quiet；第三个 started attempt 被 steer 才 dead letter |
| `wait` observes terminal then Run fails | automatic continuation 仍可处理 |
| current origin/successor Run claim then fails/times out/crashes | lease release/expiry 后 requeue |
| current origin/successor Run claim then verified complete | completion handled；不创建 Triggered Run |
| 100 replayed Run-create requests | 一个 continuation run_id；attempt 可审计 |
| crash at Run binding/Agent/outcome boundaries | 不创建第二个 logical run_id；未持久化 outcome 不误 handled |
| long Agent attempt exceeds short claim lease | scanner 不启动第二 attempt；stale token 不能继续 tool/ack |
| same-daemon attempt hangs past deadline+grace | scanner fence/cancel/requeue；旧 attempt 后续 tool/outcome 全拒绝 |
| waiting_human outcome | completion handled 一次；HumanRequest 不重复创建 |
| handling fails 5 times | dead letter 可检查；Execution terminal 不变 |
| final send fails for 24h and at least 12 attempts | delivery dead letter；不重跑 Agent/Execution |
| channel accepted then local commit crash | 使用同 envelope ID 重试；测试明确允许 adapter 不幂等时重复 |
| Feishu/iLink capability contract | 当前明确 non-idempotent；不得因 envelope.ID 存在就宣称去重 |
| persistent SQLite BUSY/FULL/CORRUPT | global infrastructure_blocked/fail-stop 可见；pending 不 silent drop |
| invalid entrypoint/sender/session/route | sealed suppression，fail closed，无 fallback |
| admin retry dead letter | phase 重启、旧 attempts/audit 保留 |
| startup with expired claims | ready 前恢复；运行期 30 秒 scan + 30 秒 grace 覆盖 expiry |
| startup scan crashes / overlaps periodic | 下次幂等恢复；periodic 不会在 startup 完成前启动 |
| DB clock forward/backward jump | grace + fencing 防双 commit；恢复延迟/竞争可观测 |
| worker bypasses terminal API | database constraint 拒绝无 matching outbox 的 terminal commit |
| retention after artifact cleanup | identity/digest/tombstone 仍可防 replay |

恢复链测试必须使用真实 `BuildScope -> persisted SessionScope -> inboundContextFromScope -> Manager.Emit` 数据，
不能手搓无 prefix 的干净 sender/chat ID。

平台/文件系统/断电不是包级覆盖率分母，按 companion qualification 文档
`xira-completion-reliability-platform-qualification-v0.zh.md` 做 release/manual/nightly 取证；二者不能拿“单测全绿”互相冒充。

## 12. 被拒绝的方案

| 方案 | 拒绝原因 |
|---|---|
| JSON + process mutex | 不能原子提交四类记录，也不能 fence stale owner |
| terminal 后 best-effort enqueue | terminal commit 后 crash 会永久 silent loss |
| `wait` 返回即 consumed | 观察后 Run 仍可能失败/steer，读取不是 ack |
| Run 创建即删 mailbox | `run_created` 尚无 durable outcome，不是 handled |
| delivery 失败重跑 Agent | 会重复 LLM/tool 副作用；只能重发 frozen final |
| Redis/Kafka | local-first 单 daemon v0 不需要分布式基础设施 |
| exactly-once 宣称 | 外部 channel/tool 与本地 commit 没有原子边界 |

## 13. 与 #204/#206/#194/#197 的边界

- #204：继续拥有 Triggered Turn typed input、mailbox identity、coordinator/user priority、exact route 和
  fail-closed policy。本 RFC不改变这些 invariant。
- #206：定义 Execution worker ownership、persist-before-spawn、terminal facts、artifact/result refs 和 restart
  harvest、state-dir singleton daemon ownership；所有 terminal transition 必须满足本 RFC 的 atomic
  terminal+outbox operation，且数据库约束拒绝旁路。#205 可先 Accepted 成为 #206 的规范输入；#206 必须在
  自身 Accepted checklist 回引本契约；daemon-wide hang 的外部 supervisor/watchdog 也由 #206/deployment 负责。
  #205 先定义交界契约，#206 再接受并实现；两者都 Accepted 前不实施，不存在循环前置。
- #194：提供 typed Triggered Turn 共用的 Agent Loop Core。本 RFC 在 core 外做 Run binding、attempt、claim、
  handling ack 和 delivery。
- #197：复用 `OutboundEnvelope`/exact-entrypoint `Manager.Emit` mechanics，不假设已有 envelope-ID 幂等闭环。
  delivery target 仍是原 conversation，不改成 Cron Principal；adapter capability 必须显式声明。

Gate 0 全部 Accepted 前，不创建 implementation issue，不合入 Store/Supervisor/tool migration 代码。

## 14. 本 Gate Accepted 条件

- [ ] 评审接受同一 local SQLite 事务域与 terminal + outbox 原子提交。
- [ ] 评审接受 outbox、mailbox handling、final delivery 三状态机及无歧义术语。
- [ ] 评审接受 `get/log/wait` 只观察；显式 claim + Runtime-owned ack。
- [ ] 评审接受 run_id + authoritative tool_call_id create key、request digest/policy snapshot 分离和 missing-ID fail closed。
- [ ] 评审接受 ContinuationID 唯一与 idempotent logical Run binding，不宣称 Agent exactly-once。
- [ ] 评审接受 short claim 与 fenced Agent attempt 分离、15s heartbeat、30s scan/grace、DB-time expiry CAS。
- [ ] 评审接受分 phase retry、full-jitter backoff、phase-specific poison threshold 与 sealed suppression。
- [ ] 评审接受 steering quiet gate/三次 circuit breaker，不因用户活跃无限重跑 Agent。
- [ ] 评审接受数据库层拒绝 terminal-without-outbox，以及 #206 的 singleton/terminal 对接义务。
- [ ] 评审接受 final delivery 独立重试，`final_sent` 不等于用户已读/全局 exactly-once。
- [ ] automated matrix 覆盖 #205 六项验收；platform qualification 留有可审计 evidence。
- [ ] Accepted 后将最终结论回灌 #202、更新 #205 checklist 并关闭 #205。

当前文档是 Proposed。以上条件经评审完成前，不得把它当成 implementation contract。
