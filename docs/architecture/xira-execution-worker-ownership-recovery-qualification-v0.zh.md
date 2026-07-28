# Xira Managed Execution v0：Worker Ownership Qualification 与 NFR Matrix

- 状态：Proposed
- Gate：0C（Issue #206）
- 规范主文档：`xira-execution-worker-ownership-recovery-rfc-v0.zh.md`
- 上游：Gate 0A（#204，Accepted）、Gate 0B（#205，Accepted）
- 目标分支：`milestone/managed-execution-v0`

## 1. 角色与权威边界

本文是 Gate 0C 的 normative qualification companion，把主 RFC 的 ownership、restart、artifact、cancel
和 terminal 决策转成可测 NFR 与 failure matrix。两份文档必须一起 review/Accepted。

冲突时按以下顺序处理，不允许自行选一个方便的解释：

1. #204 的 four sealed terminal kinds 与 #205 的 terminal+matching-outbox 原子事务不可被本文修改；
2. 主 RFC 决定 daemon/worker/target ownership 和恢复语义；
3. 本文决定 qualification threshold、quota/GC/admin 要求及必须测试的故障窗口；
4. 发现冲突就阻塞 Gate 0C，在文档层修正，不能留给实现猜。

本文仍不冻结 Go API、SQLite DDL、artifact 物理编码或具体 launcher 技术。

## 2. Admission、配额与 sticky rejection

### 2.1 有限 profile policy

所有上限由 runtime/profile policy 提供，模型参数只能向下收紧，不能放宽。profile 必须声明有限值：

- daemon global active executions；
- workspace、principal 与 conversation active executions；
- 单 Execution output bytes、max runtime 与 termination grace；
- state-dir aggregate artifact bytes 与 terminal record count；
- control API 的 wait window、log page 与 list page 上限。

production 未配置上述值时 fail closed。development 可带保守默认，但启动日志和 capability response
必须打印实际值。qualification 用实际 profile 做边界测试；本文不硬编码未经生产负载验证的魔法数字。

### 2.2 统一 create guard，先验证 origin replay authority

每次 managed create 都必须先证明 authoritative origin Run/tool call 存在且仍允许 replay。origin 已标
non-replayable、已删除或无法核验时，必须 fail closed 为 `origin_run_not_replayable`，不得把请求当首次
create。模型不能提供或覆盖这份 authority。

为使该检查能与 guard decision 原子竞争，managed SQLite 必须保存 origin Run/tool call 的最小
replay-authority projection/version；它不是 conversation/session history。Runtime 只能在 authoritative Run 与
tool call 已 durable 的 managed-tool dispatch boundary 提供建立 projection 的可信事实；首次 create transaction
必须把 replayable projection 与 guard decision 原子落库，不能先提交一个没有 guard 的 replayable projection。
后续状态只允许用 version CAS 从 replayable 单向变为 non-replayable，provider replay 不能刷新或重新打开它。
迁移前没有 projection 的旧 Run/tool call 视为 authority 不可核验，fail closed，不做 lazy 补写。

#206 冻结一个逻辑上的 **managed create guard**，具体表名留给实现：

- guard 以 #205 authoritative create key 唯一，绑定 origin Run/tool call 与 #205 的持久版本化
  request-spec digest/validator version；canonicalizer migration 必须按 guard 保存的旧 validator 比较，不能
  用新 canonicalizer 重算旧 digest；
- decision 是 sealed union：`admitted(execution_id)` 或 `rejected(reason)`；
- admitted branch 的 Execution 仍保存并唯一约束同一个 create key，且 execution_id/digest 必须匹配 guard；
- rejected branch 无 ExecutionID，不是 Execution terminal，不生成 ContinuationID/completion outbox；
- replay 必须先验证 origin authority，再只查 guard：同 key+digest 返回原 Execution/rejection；不同 digest
  返回 `idempotency_conflict`，并带 decision/object kind 便于审计；
- 一个 create key 只有一个 guard winner，因此不存在“receipt 表还是 Execution 表先查”的竞态；
- 有意重试必须使用新 tool call/create key。

这扩展的是 #206 pre-create admission routing，不改变 #205 successful `create_key → ExecutionID` replay。

### 2.3 admission 与 capacity 是单一事务

v0 不引入隐藏的资源等待调度器。首次 decision 在 managed-execution SQLite 的一个 writer transaction 内
完成：取得与 GC 相同的写序列化点后，验证 origin authority projection/version、争用 guard、检查 quota；
admitted 时同时 reserve capacity + 创建 queued Execution + 写 admitted guard，quota 已满时只写 rejected
guard（reason=`execution_capacity_exhausted`）。capacity ledger 必须位于同一 SQLite/事务域，不能以内存
计数或外部 store 作为权威。

- transaction 未 commit：guard、reservation、Execution 全部回滚，无 worker/target；
- commit 后：non-terminal Execution 持有 capacity，直到 terminal+matching-outbox transaction 同时释放；
- spec/materialization 在 commit 后失败：提交 `failed` terminal+outbox 并释放 capacity，不留下永久 reservation；
- `execution_capacity_exhausted` 是该 create key 的 sticky decision；容量后来释放也只 replay 原 rejection，
  有意重试必须创建新 tool call/create key；
- 已 admitted 的 Execution 不因后来 quota 收紧被静默杀掉；管理员只能走审计过的 cancel。

### 2.4 GC 不变量：guard 或 non-replayable origin 必须至少存在一个

GC 不能靠两个独立 TTL 猜 provider 最晚何时 replay。一次 decision 返回后必须始终满足：

```text
origin is replayable  => matching create guard exists
create guard removed  => origin was durably marked non-replayable first
```

guard 可压缩为只保留 key、digest、decision、object ref/reason 的非敏感 tombstone，但只要 origin replayable
就不能删除。admission 与 GC 必须使用同一 managed SQLite writer serialization point：authority 校验只能在
取得该序列化点后的 create transaction 内完成；GC transaction 必须等待更早的 create commit/rollback，再以
version CAS 将 origin 单向 fence 为 non-replayable，之后才可删除 guard。若 GC 先 commit，后来的 create 必须
在 fresh authority check 失败；禁止把 authority check 放到事务外，也禁止把 projection/guard GC 拆到不能
共同序列化的 store。两者最终都物理删除后，任何引用该缺失 origin 的迟到请求仍 fail closed。这样无需另设
in-memory drain，也不会让旧 token 在 guard 删除后作为“首次 create”commit。

只有 terminal Execution 可被压缩。admitted guard 存活时，匹配 Execution 的最小 identity/status tombstone
也必须可解析，且 tombstone 不得早于 guard 删除；artifact/body 可以按 TTL 过期，但 replay 至少返回原
execution_id + `execution_expired` snapshot，绝不能因完整 record 已清理而重新 create。对该 tombstone 的
cancel 返回已经存在的 terminal status，不创建新 cancel intent。rejected guard 同理保留原 rejection decision。

## 3. Latency、RPO 与 RTO

在健康的本地 SQLite/qualified filesystem、未触发 quota 且不含 target 自身耗时的前提下：

- automatic yield 响应在配置 deadline 后 250ms 内返回；
- cancel intent commit 到 verified worker/launcher 收到 graceful terminate 的 p95 不超过 1s；force kill
  最迟在 `termination_grace + 1s` 内发出；
- valid result candidate durable 后，运行中 harvester 的 terminal+outbox commit p95 不超过 2s；
- daemon restart 后在 ready 前完成全量 non-terminal reconciliation；在 profile 最大 non-terminal 数量下
  recovery RTO 不超过 30s；
- worker 至少在“每 1s 或每新增 1MiB 完整 frame，先到者”做 artifact durability checkpoint；host/worker
  crash 的 output RPO 不超过当前 checkpoint window；已 durable candidate 的 result evidence RPO 为 0，
  terminal authority 仍要等 harvester 原子提交 terminal+matching-outbox；
- SQLite busy 可有界重试，但不得跨 max-runtime、cancel deadline 或 readiness 无限等待。

恢复 identity-matching worker 时必须重算持久 max-runtime deadline。deadline 已过就立即走 verified
launcher/containment timeout termination；不能因为 worker 进程仍 alive 就继续 running，也不能只依赖
可能已经 hang 的 worker 自己触发 timer。

平台达不到阈值时不标 qualified；修改产品 SLO 必须回到 Gate 文档 review，不能只放宽测试。

## 4. Artifact/result qualification

candidate 必须把两组 output facts 分开：

| facts | 含义 | harvester 用法 |
|---|---|---|
| target-observed | worker 从 stdout/stderr pipe drain 到的 byte count；可带 streaming digest | 解释 target 实际产出与 quota 丢弃量 |
| durable-artifact | 已 checkpoint 的完整 frame byte count、cursor、digest | 核验可恢复 artifact，作为 log(cursor) 边界 |

quota/truncation 时 observed 可以大于 durable。candidate identity/result envelope 校验与 artifact 校验分开：

- candidate envelope/exit evidence 有效、artifact 尾损坏：保留真实 terminal，artifact 标 corrupt/truncated；
- durable cursor/digest 不匹配：不得把 candidate 声称的 observed bytes 伪装成可读取 artifact；
- candidate envelope/identity 自身无效：不能相信 exit evidence，按主 RFC 进入 `lost`；
- output 超 quota 后继续 drain pipe，内存有界，candidate 标 observed/durable 差值与 truncated；
- terminal/outbox/RuntimeEvent/IM 只携带 bounded refs/preview，不携带全量 output。

## 5. Retention 与 GC

profile 必须声明 terminal metadata TTL、artifact TTL、create-guard compaction、origin replay-authority retention、
GC interval 和 aggregate quota。GC 契约：

- active/non-terminal Execution 的 spec、marker、artifact、candidate 不得被 GC；
- terminal metadata 与 artifact 可有不同 TTL，读取必须显式返回 `artifact_expired`，不能静默 dangling；
- GC 先持久化 expiry/tombstone，再删 artifact，最后记录完成审计；crash 后可幂等续做；
- quota pressure 只回收已过期且未被合法 hold 的 artifact；不足时拒绝新 admission，不偷删未过期或
  active evidence；
- result digest、terminal facts、completion correlation、create guard 与 GC audit 服从 §2.4；Execution
  identity/status tombstone 必须至少与匹配 admitted guard 同寿，guard 删除后才服从普通 terminal metadata
  retention；这些是 profile-independent 顺序不变量，配置值不能覆盖；
- secret/owner unbind 立即撤销访问，但物理清理仍按明确 policy 与审计执行。

## 6. Observability 与 admin inspection

至少暴露：active/admission-rejected 数及按 reason 分类的拒绝率（至少单列
`execution_capacity_exhausted`）、状态年龄、launch/recovery/harvest latency、timeout/cancel/lost、artifact
observed/durable bytes、truncation/corruption、candidate validation failure、GC backlog、oldest non-terminal、
singleton/launcher capability 和 macOS `orphan_possible`。

admin inspection 对单个 Execution 至少显示：权威状态/version、deadline、policy digest、worker identity
核验摘要、launcher observation、最后 durable cursor、cancel intent、candidate validity、recovery/lost
reason 和 matching outbox ID。敏感 argv/env/output 只显示 redacted 摘要。

cancel 的同步 control outcome 只表示 intent `accepted` 或 `pending_recovery`，不表示 target 已取消。admin
cancel 也必须走 persist-intent → verified-worker/launcher 协议并记录 actor/audit，不提供裸 PID kill 捷径。

macOS `orphan_possible` 只提供 non-authoritative evidence 与 OS-level inspection 指引；Xira 不根据旧 PID
自动 kill。该状态在 production profile 一律不合格。

## 7. Failure matrix

本表的处理结果是主 RFC §8.4/§9 的可测试投影，不创造第二套恢复语义；若发现偏离，必须修本文并阻塞
qualification，不能让实现任选。

| 故障点 | 必须结果 |
|---|---|
| quota reject 后同 create key replay | guard 返回同 sticky rejection；容量释放也不 spawn；新 tool call 才可重试 |
| admitted/rejected 同 key 并发 | 一个 guard winner；admitted branch 至多一个匹配 Execution |
| origin replayable 时 GC 尝试删 guard | 拒绝/跳过 GC；guard 保留 |
| GC fence 与已开始的 admission 并发 | 共享 writer serialization point；按 commit 顺序二选一，不允许 stale authority commit |
| origin 先 durable non-replayable、guard 后删除 | 迟到请求在 authority check fail closed |
| origin 与 guard 最终均物理删除后迟到请求 | origin 缺失即 fail closed；不得首次 create/spawn |
| admitted guard 存活、Execution 完整记录已 compact | 返回原 execution_id 的 expired snapshot；不 spawn |
| 对 compacted terminal tombstone cancel | 返回现有 terminal status；不创建 cancel intent、不改 terminal |
| Execution DB create/spec publish 失败 | 无 worker、无 target |
| 100 个相同 create key 并发 | 一个 guard decision；admitted 时至多一个 Execution/target |
| admission transaction 任一步失败 | guard/reservation/Execution 全回滚，无 capacity 泄漏 |
| materialization 在 admission commit 后失败 | terminal `failed` + matching outbox，并释放 capacity |
| terminal/outbox/capacity release transaction 结果未知 | 同库重读；三者全有或全无，不靠外部计数修补 |
| CAS 后、launcher 调用前 daemon crash | reconcile；v0 不自动新 generation |
| launcher 已启动、返回前 daemon crash | 恢复同一 worker 或诚实 `lost`；不重复 target |
| launcher 证明 target 从未启动 | terminal `failed/launch_failure` + matching outbox；origin Run 可 claim，否则照常 Triggered Turn；新 tool call 才可重试 |
| marker 发布、无 target evidence/candidate、启动不可证 | `lost`；不假设、不 raw-PID kill、不 respawn |
| target 启动后 worker crash | containment 有界清理；无 valid result 则 `lost` |
| worker 在 candidate 前死亡、OS 仅报告 exit | `lost`；v0 不把普通 OS notification 当 result authority |
| worker alive 但 max-runtime 已过 | verified timeout termination，不继续 running |
| result rename 后、SQLite terminal 前 daemon crash | 新 daemon harvest 同一 candidate |
| terminal+matching-outbox 原子 commit 后、dispatch 前 crash | #205 dispatcher 重放 |
| daemon 与 worker 同时 crash | 按 durable evidence 分类，不使用内存事实 |
| PID 复用/伪造 marker | 不 adopt、不 signal 无关进程 |
| host reboot | valid result 可 harvest；其余 running 不可假恢复 |
| cancel 无 verified worker | intent `pending_recovery`；最终 harvest 或 `lost`，不报告 cancelled |
| cancel/natural exit/timeout 同时发生 | 由 evidence 决定；terminal CAS 单向 |
| output 远超 quota | pipe 继续 drain、内存有界；observed/durable facts 可解释 |
| candidate 有效、artifact partial/corrupt | terminal 可保留；artifact 单独降级 |
| candidate envelope/identity 无效 | 隔离 evidence，`lost` |
| disk full / SQLite busy / SQLite full | 有界重试或 fail closed，completion 不静默丢失 |
| unsupported filesystem/launcher capability | 启动时拒绝 Managed Execution |
| 第二 daemon 争 singleton | 在 DB/migration/scanner 前失败 |
| surviving launcher/helper/worker/target 继承 singleton FD | qualification 失败；新 daemon 不得 ready |
| macOS crash 后 identity 不可证、进程可能仍活 | `lost + orphan_possible`；不自动 raw-PID cleanup |

## 8. Contract code 与必须测试

以下落地时按 AGENTS.md §5.2 标记为 contract code，所有分支/case 100%：

- origin replay authority/create-guard/CAS 与 launch-generation 状态机；
- identity matching、PID-reuse rejection 与 startup reconciliation；
- terminal mapping、evidence source 与 terminal+outbox 调用边界；
- cancel/timeout/natural-exit resolution；
- capability/profile fail-closed；
- artifact frame/cursor 与 observed/durable validation。

必须测试：

- 每个 persist-before-spawn durable boundary 的 crash injection；
- 同 key 跨 decision 高并发、sticky quota rejection replay、GC fence 与 in-flight admission 的两种 commit
  顺序、guard/origin 全部 GC 顺序与 daemon 双实例竞争；
- origin projection bootstrap/fence、旧 Run migration fail-closed，以及旧 digest validator replay；
- admitted terminal Execution compaction 后 replay 仍返回原 identity/expired snapshot，cancel 不改 terminal；
- admission rollback、post-commit materialization failure 与 terminal capacity release；
- singleton FD 不被 launcher/helper/worker/target 继承；worker/control FD 不泄漏给 target；
- Linux worker 跨 daemon kill/restart 继续并由新 daemon harvest；macOS 不冒充恢复；
- marker step 3↔6 crash、PID reuse、boot ID 变化、marker/candidate tamper；
- recovery 时 deadline 已过且 worker 响应/不响应的两条 termination 路径；
- launch_failure 在 yield 前 claim + Runtime-owned ack、claimant failure/release，以及 yield 后无人 claim 的
  Triggered Turn 路径；
- worker 在 candidate 前死亡但 OS 有 exit notification 时仍诚实 `lost`；
- 100MiB 以上 output，observed/durable digest、cursor、partial tail、quota、disk full 与内存上限；
- cancel accepted/pending、无 verified worker、TERM→KILL 和自然退出/timeout 的所有竞争顺序；
- model 参数不能放宽 quota；profile 缺值 fail closed；
- recovery RTO、harvest/cancel/yield latency 与 checkpoint RPO；
- retention tombstone/delete 每个 crash boundary，active evidence 与 create guard 不被错误 GC；
- worker 绕过 terminal API 被数据库约束拒绝；#205 outbox 重放端到端；
- 外部 watchdog 重启 daemon 时 worker kill domain 不受影响。

全量验证至少包括 `go build ./...`、`go test ./...`、race/fault-injection 与平台资格套件。不能用“进程还在”
替代 identity，也不能用手搓干净 marker/candidate 绕过真实序列化恢复路径。

## 9. Production qualification artifact

每个发布环境必须生成可审计结果，至少记录：

- OS/kernel/filesystem/launcher 版本；
- singleton、atomic no-replace rename、file+directory fsync；
- daemon/worker kill-domain 与 FD inheritance 隔离；
- worker restart survival、identity proof、process-tree terminate/kill；
- fault-injection、large-output、reboot/PID-reuse、create-guard/admission/GC 测试；
- 实际 quotas、retention、latency/RPO/RTO 测量值。

环境漂移后必须重跑；不通过时 production profile fail closed。

## 10. Acceptance checklist

- [ ] unified create guard 与 #205 successful create/replay 边界无冲突；
- [ ] origin replay authority、guard tombstone 与 in-flight fencing 在 GC 后仍 fail closed；
- [ ] admission 单事务 rollback 与 terminal capacity release 无泄漏；
- [ ] generation 不自动重试，所有 ambiguous launch 有唯一安全结果；
- [ ] observed/durable output facts 与 artifact corruption 规则可执行；
- [ ] launch_failure matching outbox 可由 origin Run claim，未 claim 时仍会 continuation；
- [ ] recovery deadline、cancel pending 与 macOS orphan 行为可测试；
- [ ] quota、retention/GC、latency、RPO/RTO、admin inspection 有资格证据；
- [ ] failure matrix 和 contract tests 覆盖主 RFC 的 ownership/terminal 决策；
- [ ] 本文与主 RFC 一起 review/Accepted，并回引 #202/#206。
