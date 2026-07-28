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

### 2.2 admission receipt 是 #206-owned pre-create fact

v0 不引入隐藏的资源等待调度器。quota 已满时，managed create 在 spawn 前返回
`execution_capacity_exhausted`，不创建一个可能永久 queued 的 Execution/target。

为了防止同一 provider tool call 在容量释放后重放并突然产生副作用，#206 必须持久化一个
**admission rejection receipt**：

- key 为 #205 已冻结的 authoritative create key，并绑定同一 request-spec digest；
- receipt 不含 ExecutionID，不是 Execution terminal，不生成 ContinuationID/completion outbox；
- 同 key+digest replay 返回原 rejection；同 key+不同 digest 返回 `idempotency_conflict`；
- receipt 可在到期后压缩为只保留 create key、digest、decision 的非敏感 tombstone；只要历史 tool call
  仍可能 replay，该 tombstone 就不能删除；
- authoritative Run/tool-call history 已不可用、无法证明该 key 是否执行过时，必须 fail closed 为
  `idempotency_history_expired`，绝不能把旧 key 当首次 create 后 spawn；
- 有意重试由新 tool call/create key 发起。

admission 成功时，quota check、capacity reservation 与 Execution create 在同一 SQLite transaction domain
竞争；拒绝时 receipt 在决定返回前 durable。具体表结构由实现 RFC 决定。这补充的是 pre-create failure，
不修改 #205 的 `create_key → ExecutionID` 成功创建/replay 契约。

已通过 admission 并持久化的 Execution 不因后来 quota 收紧被静默杀掉；管理员只能走审计过的 cancel。

## 3. Latency、RPO 与 RTO

在健康的本地 SQLite/qualified filesystem、未触发 quota 且不含 target 自身耗时的前提下：

- automatic yield 响应在配置 deadline 后 250ms 内返回；
- cancel intent commit 到 verified worker/launcher 收到 graceful terminate 的 p95 不超过 1s；force kill
  最迟在 `termination_grace + 1s` 内发出；
- valid result candidate durable 后，运行中 harvester 的 terminal+outbox commit p95 不超过 2s；
- daemon restart 后在 ready 前完成全量 non-terminal reconciliation；在 profile 最大 non-terminal 数量下
  recovery RTO 不超过 30s；
- worker 至少在“每 1s 或每新增 1MiB 完整 frame，先到者”做 artifact durability checkpoint；host/worker
  crash 的 output RPO 不超过当前 checkpoint window；已 durable candidate 的 terminal fact RPO 为 0；
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

profile 必须声明 terminal metadata TTL、artifact TTL、admission receipt/history TTL、GC interval 和 aggregate
quota。GC 契约：

- active/non-terminal Execution 的 spec、marker、artifact、candidate 不得被 GC；
- terminal metadata 与 artifact 可有不同 TTL，读取必须显式返回 `artifact_expired`，不能静默 dangling；
- GC 先持久化 expiry/tombstone，再删 artifact，最后记录完成审计；crash 后可幂等续做；
- quota pressure 只回收已过期且未被合法 hold 的 artifact；不足时拒绝新 admission，不偷删未过期或
  active evidence；
- result digest、terminal facts、completion correlation、rejection receipt 与 GC audit 的最小保留期服从
  idempotency/audit policy；GC 后不允许历史 create key 重新 spawn；
- secret/owner unbind 立即撤销访问，但物理清理仍按明确 policy 与审计执行。

## 6. Observability 与 admin inspection

至少暴露：active/admission-rejected 数、状态年龄、launch/recovery/harvest latency、timeout/cancel/lost、
artifact observed/durable bytes、truncation/corruption、candidate validation failure、GC backlog、oldest
non-terminal、singleton/launcher capability 和 macOS `orphan_possible`。

admin inspection 对单个 Execution 至少显示：权威状态/version、deadline、policy digest、worker identity
核验摘要、launcher observation、最后 durable cursor、cancel intent、candidate validity、recovery/lost
reason 和 matching outbox ID。敏感 argv/env/output 只显示 redacted 摘要。

cancel 的同步 control outcome 只表示 intent `accepted` 或 `pending_recovery`，不表示 target 已取消。admin
cancel 也必须走 persist-intent → verified-worker/launcher 协议并记录 actor/audit，不提供裸 PID kill 捷径。

macOS `orphan_possible` 只提供 non-authoritative evidence 与 OS-level inspection 指引；Xira 不根据旧 PID
自动 kill。该状态在 production profile 一律不合格。

## 7. Failure matrix

| 故障点 | 必须结果 |
|---|---|
| quota reject 后同 create key replay | 返回同 admission receipt；容量释放也不 spawn |
| admission history 已 GC 后旧 key replay | `idempotency_history_expired`，fail closed |
| Execution DB create/spec publish 失败 | 无 worker、无 target |
| 100 个相同 create key 并发 | 一个 receipt 或一个 Execution；至多一个 target |
| CAS 后、launcher 调用前 daemon crash | reconcile；v0 不自动新 generation |
| launcher 已启动、返回前 daemon crash | 恢复同一 worker 或诚实 `lost`；不重复 target |
| launcher 证明 target 从未启动 | terminal `failed/launch_failure`；新 tool call 才可重试 |
| marker 发布、无 target evidence/candidate、启动不可证 | `lost`；不假设、不 raw-PID kill、不 respawn |
| target 启动后 worker crash | containment 有界清理；无 valid result 则 `lost` |
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

- create-key/admission receipt/CAS 与 launch-generation 状态机；
- identity matching、PID-reuse rejection 与 startup reconciliation；
- terminal mapping、evidence source 与 terminal+outbox 调用边界；
- cancel/timeout/natural-exit resolution；
- capability/profile fail-closed；
- artifact frame/cursor 与 observed/durable validation。

必须测试：

- 每个 persist-before-spawn durable boundary 的 crash injection；
- 同 key 高并发、quota rejection replay、history GC replay 与 daemon 双实例竞争；
- singleton FD 不被 launcher/helper/worker/target 继承；worker/control FD 不泄漏给 target；
- Linux worker 跨 daemon kill/restart 继续并由新 daemon harvest；macOS 不冒充恢复；
- marker step 3↔6 crash、PID reuse、boot ID 变化、marker/candidate tamper；
- recovery 时 deadline 已过且 worker 响应/不响应的两条 termination 路径；
- 100MiB 以上 output，observed/durable digest、cursor、partial tail、quota、disk full 与内存上限；
- cancel accepted/pending、无 verified worker、TERM→KILL 和自然退出/timeout 的所有竞争顺序；
- model 参数不能放宽 quota；profile 缺值 fail closed；
- recovery RTO、harvest/cancel/yield latency 与 checkpoint RPO；
- retention tombstone/delete 每个 crash boundary，active evidence 与 sticky rejection 不被错误 GC；
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
- fault-injection、large-output、reboot/PID-reuse、admission/GC 测试；
- 实际 quotas、retention、latency/RPO/RTO 测量值。

环境漂移后必须重跑；不通过时 production profile fail closed。

## 10. Acceptance checklist

- [ ] admission receipt 与 #205 successful create/replay 边界无冲突；
- [ ] generation 不自动重试，所有 ambiguous launch 有唯一安全结果；
- [ ] observed/durable output facts 与 artifact corruption 规则可执行；
- [ ] recovery deadline、cancel pending 与 macOS orphan 行为可测试；
- [ ] quota、retention/GC、latency、RPO/RTO、admin inspection 有资格证据；
- [ ] failure matrix 和 contract tests 覆盖主 RFC 的 ownership/terminal 决策；
- [ ] 本文与主 RFC 一起 review/Accepted，并回引 #202/#206。
