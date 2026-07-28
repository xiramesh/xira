# Xira Managed Execution v0：Worker Ownership、Restart Recovery 与 Capability Boundary RFC

- 状态：Proposed
- Gate：0C（Issue #206）
- 上游：Gate 0A（#204，Accepted）、Gate 0B（#205，Accepted）
- Milestone：#202 Managed Execution v0
- 目标分支：`milestone/managed-execution-v0`

## 1. 摘要

Managed Execution v0 只支持**有界、非交互式命令执行**。它不是把现有
`command_run` / `shell_run` 的 timeout 调大，也不是从 daemon 里 `nohup` 一个子进程。

v0 采用三层 ownership：

1. **daemon** 是 Execution 状态与 completion outbox 的唯一权威写入者；
2. **独立 exec-worker** 持有目标进程组、输出流、max-runtime 计时器与结果候选；
3. **可替换且经过能力认证的 launcher** 决定 worker 是否能跨 daemon crash/restart 存活。

Linux production profile 必须提供跨 daemon 重启存活、可靠身份核验和进程组终止能力。
macOS development profile 可以降级：正常运行时仍支持 yield、日志、等待与取消，但 daemon
重启后无法证明的执行必须诚实进入 `lost`，不能假装恢复成功。Windows 不在 v0 支持范围内。

本 RFC 只冻结 ownership、恢复语义、平台能力边界、失败矩阵和测试门槛；不冻结 Go 接口、SQLite
DDL、artifact 物理编码或某个 init system 的实现。

## 2. 背景与已核实事实

### 2.1 当前实现

当前 `CommandRunTool` 和 `ShellRunTool` 都是 request-bound：

- 分别使用 60 秒和 30 秒默认 timeout；
- 通过 `context.WithTimeout` 与 `exec.CommandContext` 绑定 Agent turn 生命周期；
- stdout/stderr 先写入内存 `bytes.Buffer`，命令结束后才整体返回；
- Unix 上创建独立 process group，取消时向负 PID 发送 `SIGKILL`；
- 仓库当前没有 Execution entity/store、exec-worker、restart harvester 或 daemon singleton
  enforcement；也没有可作为现成契约依赖的 systemd/launchd 部署配置。

因此现有工具适合短命、同步命令，不具备 Managed Execution 所需的持久身份、流式 artifact、
跨 turn 等待或 restart recovery。

### 2.2 Gate 0A 输入

本 RFC 继承 #204 的边界：

- completion 是 durable fact，不是某个 IM 请求仍活着；
- terminal fact 必须可信、有界、可追溯；
- get/list/wait/log/cancel 是 Execution 能力，不把 PTY/stdin/service 混入 v0；
- automatic yield 与 max runtime 是两个不同的时钟：前者只决定何时把控制权还给 Agent，
  后者决定 Execution 最迟何时被终止。

### 2.3 Gate 0B 输入

本 RFC 继承 #205 的可靠性契约：

- `completed` / `failed` / `timed_out` / `lost` 等 terminal transition 必须与匹配的
  completion outbox 在同一 SQLite 事务中提交；
- worker 不得绕过该 terminal API 直接把 Execution 写成 terminal；
- daemon 必须先证明 state-dir singleton，才可迁移、扫描、claim 或恢复；
- 新 daemon 只有在取得 singleton 后，才可接管旧 daemon 的 claim；
- daemon 整体 hang 需要外部 supervisor/watchdog 处理，进程内 goroutine 无法证明自身健康；
- 不满足文件系统锁、原子 rename、fsync 等前提的平台必须 fail closed，不能“尽力而为”。

## 3. 目标与非目标

### 3.1 v0 目标

- Execution 在任何目标进程启动前已持久化；
- 命令可在 Agent turn automatic yield 后继续运行，但受持久化 max runtime 约束；
- stdout/stderr 以有界内存开销持续写入 artifact，并可按 cursor 增量读取；
- 支持 `get`、`list`、`wait`、`log`、`cancel`；
- cancel/timeout 使用 graceful terminate，再在有限 grace 后强制 kill 整个目标进程组；
- daemon crash/restart 后，能够恢复仍活着的 worker、收割结果，或诚实写成 `lost`；
- completion 只由 daemon 通过 Gate 0B 的原子 terminal+outbox API 发布；
- 平台不具备能力时给出明确 capability error，而不是降低正确性却继续宣称支持。

### 3.2 明确非目标

- PTY、交互式 stdin、attach shell、终端 resize；
- 常驻 service、自动重启 service、cron、容器/集群调度；
- 任意通用 executor abstraction 或远程分布式执行；
- 多 host 接管同一个 Execution；
- 冻结 systemd transient unit、launchd、cgroup 或 artifact 编码的具体实现；
- 在三个 Design Gate 全部 Accepted 前创建实现 issue 或提交实现代码。

## 4. 核心模型

```text
Agent tool
   |
   v
daemon / Execution Manager ---- authoritative SQLite
   |                                  |
   | launch/reconcile                 | terminal + matching outbox
   v                                  |
qualified WorkerLauncher              |
   |                                  |
   v                                  |
xira exec-worker ---- output/result candidates
   |
   v
target process group
```

关键区分：

- SQLite 中的 Execution 是**权威状态**；
- worker 写出的 result 是**结果候选证据**，不是 terminal 状态；
- launcher/OS identity 是**活性证据**，PID 本身不是；
- artifact 是**执行证据与读取来源**，不是 completion 发布机制。

## 5. Ownership 契约

### 5.1 daemon ownership

daemon 负责：

- state-dir singleton；
- Execution create、状态迁移、cancel intent、terminal+outbox 原子提交；
- worker launch intent 与 launch generation 的 CAS；
- startup reconciliation、周期 harvest 和 artifact metadata 更新；
- 对外 get/list/wait/log/cancel API；
- capability probe 与 fail-closed。

daemon **不直接持有目标进程**，也不依赖某个 Agent request context 保持 execution 存活。
daemon 退出不应自动解释为用户取消或 timeout。

singleton 必须是 OS-backed exclusive lock，而不是 PID 文件。daemon 在打开数据库、执行迁移、
启动 scanner 或接受请求前取得锁。锁 FD 必须 close-on-exec，且不得被任何 surviving launcher/helper、
worker 或 target 继承；否则旧进程会阻止新 daemon 取得 singleton，形成无法恢复的自锁。

### 5.2 exec-worker ownership

每个 Execution launch generation 最多有一个 exec-worker。worker 负责：

- 持有并监督一个目标进程组；
- 独立执行持久化的 max-runtime deadline；
- 将 stdout/stderr 连续写入 append-only artifact；
- 接收经过验证的 cancel 请求并执行 terminate → grace → kill；
- 观测 target 的真实 exit/signal/timeout/cancel 结果；
- 原子写出 immutable result candidate；
- 在 candidate 与 artifact durable 后退出。

worker 不得：

- 打开 SQLite 并提交 Execution terminal 状态；
- 持有模型、channel、IM、数据库等无关凭据；
- 根据自然语言推断取消者或 recipient；
- 把未经核验的 PID 当成 target identity；
- 在 wrapper 身份丢失后留下不受控 target。

worker 是运行时 containment boundary，不是第二个 daemon。它必须小、无网络依赖、输入 sealed、
行为可独立测试。

### 5.3 target ownership

target 只属于对应 worker 的 containment domain。目标命令及其后代必须处于同一可终止边界，不能
通过 double-fork、setsid 或继承错误逃离。合格的 Linux launcher/worker 组合必须证明：

- worker 活着时，可以终止整个目标树；
- worker 非预期死亡时，target 不会无限期变成无人监管的 orphan；
- daemon supervisor 重启 daemon 时，不会顺手杀掉应继续运行的 worker；
- worker 与 daemon 有不同的 kill domain。

具体可由 cgroup、parent-death 机制或等价 OS primitive 实现，但实现必须通过能力测试，不能只凭
配置声明。

## 6. Launcher 与平台能力边界

### 6.1 WorkerLauncher 是 capability boundary

核心契约依赖能力，不依赖某个品牌或 init system。`WorkerLauncher` 的实现至少要能表达：

- launch 某个 execution/generation；
- 根据持久身份核验 worker 是否仍为同一实例；
- 向经过核验的 worker 发送 cancel；
- 判断“不存在”“存在且身份匹配”“存在但身份不可信”；
- 声明 restart survival、process-tree containment、durable identity 等 capability。

systemd transient unit 可以成为 Linux 实现方案，但不是 RFC 契约本身。把 systemd 写死在核心模型
会让开发环境、测试和以后可能的 launcher 演进全部被部署细节绑架。

### 6.2 v0 capability matrix

| 能力 | Linux production | macOS development | Windows |
|---|---|---|---|
| bounded non-interactive execution | 必须 | 支持 | 不支持 |
| automatic yield 后继续 | 必须 | 支持 | 不支持 |
| streaming artifact/cursor | 必须 | 支持 | 不支持 |
| verified process-tree cancel | 必须 | 支持本机已验证边界 | 不支持 |
| daemon crash 后 worker 存活 | 必须 | 不保证 | 不支持 |
| daemon restart 后身份恢复 | 必须 | 无法证明则 `lost` | 不支持 |
| external daemon watchdog | 必须部署 | 开发者自行启动 | 不支持 |

Linux production profile 未通过资格测试时，Managed Execution 必须拒绝启动。macOS profile 必须在
capability response 和日志中明确 `restart_survival=false`，不得让调用方误以为与 production 等价。
macOS daemon crash 后，无法重新核验的 worker/target 可能成为 orphan；Execution 必须 `lost` 并产生
`orphan_possible` admin diagnostic，不能拿旧 PID 自动清理。该退化只允许 development profile；正常
运行期的 verified cancel 不受影响，Linux production 必须证明 containment 不留下持久 orphan。

### 6.3 Shutdown profiles

Linux production 默认使用 `leave-managed`：daemon 停止接受新工作，已启动 worker 继续运行，由新
daemon 在取得 singleton 后恢复/harvest。

另外允许：

- `drain`：停止新工作，等待现有 worker 在上限内自然结束；
- `cancel`：先持久化 cancel intent，再请求 worker 有界终止。

不具备 restart survival 的 profile 不能使用 `leave-managed`。它必须显式选择 drain 或 cancel；
强退后剩余不确定执行按恢复矩阵进入 `lost`。

## 7. Persist-before-spawn 协议

### 7.1 创建

在任何 worker/target 启动前，daemon 必须在 SQLite 事务中创建权威 Execution，至少冻结：

- execution ID、create key、调用主体与 scope；
- sealed command spec 的 digest 与安全引用；
- create-time policy snapshot；
- max runtime、automatic yield、termination grace、output quota；
- 初始状态、launch generation 和审计时间。

如果事务失败，不得出现 worker。create key 的并发重试必须返回同一 Execution，不得多起目标进程。
这里的 `queued` 是持久化→launch handshake 状态，不是等待容量的通用任务队列。

### 7.2 安全 materialization

daemon 在 launch 前生成 worker 可读的 sealed spec 与 artifact 目录，并按 temp file → file fsync →
atomic rename → directory fsync 发布。spec 不得落盘明文 secret；只保存 secret reference/digest。
需要的一次性 secret 通过受保护的启动通道或 provider 注入，具体机制留给实现 RFC。

### 7.3 launch intent

daemon 通过 CAS 将一个 generation 标为 launch-intended，再调用 launcher。spawn 返回不等于 target
已经可靠启动；spawn 超时/daemon crash 也不等于没启动。

因此任何 ambiguous launch 都禁止盲目 respawn。恢复者必须先组合核验：

- launcher identity；
- per-execution worker lock；
- durable worker marker；
- result candidate；
- generation/spec digest。

v0 不在同一个 Execution 内自动创建新 generation。即使 launcher 能证明 target 从未启动，也提交
terminal `failed`（reason=`launch_failure`）；有意重试必须由新 tool call 创建新 Execution。无法证明
时进入 reconciliation，不靠猜测再起进程。generation 在 v0 仍参与 fencing/evidence，值不用于重试。

`launch_failure` 与其他 terminal 一样必须创建 matching outbox，不能因“target 没启动”按 reason 特判
suppressed。若它在 automatic yield 前返回给仍 active 的 origin Run，该 tool result 仍只是观察；origin Run
要负责本次 completion，必须按 #205 §7.2 执行 `claim_terminal` take-over，暂停 automatic continuation。
模型看不到 claim token，也不能直接 ack；只有 Runtime 在 claimant Run 产生 #205 §7.3 认可的 verified
outcome 后才执行 Runtime-owned ack，把 completion 标为 handled 并不再启动 Triggered Turn。claimant Run
失败、超时或失去 ownership 时必须 release/requeue，outbox/mailbox 继续处理。

若 `launch_failure` 在 automatic yield 后才成为 terminal，origin Run 已不再 active，不能以 origin 身份补
claim；outbox 按 #205 正常 dispatch。此后只有满足 #205 约束的 active successor Run 能与 mailbox
coordinator 竞争同一 `ContinuationID` ownership；无人 take-over 时启动 Triggered Turn。

### 7.4 worker start handshake

worker 顺序必须是：

1. 取得 per-execution/generation exclusive worker lock；
2. 校验 sealed spec、generation 与 digest；
3. 写入并 durable 发布 worker identity marker；
4. 建立 containment 与 output artifact；
5. 启动 target；
6. 更新 marker 中的 target evidence（仍采用原子发布）；
7. 开始监督、计时与输出复制。

若在第 1-5 步失败，worker 写出明确的 launch-failed candidate；若连 candidate 都无法 durable，
恢复者只能基于可验证的 launcher/lock/marker 事实判断，不能伪造 exit code。

## 8. 身份与恢复

### 8.1 身份不是 PID

可恢复 worker identity 至少绑定：

- host boot ID；
- worker PID 与 OS process start identity；
- execution ID 与 launch generation；
- 随机 launch nonce；
- sealed spec digest；
- launcher-specific identity；
- durable worker marker。

PID 单独不具备权威性，因为会复用。daemon 绝不能拿数据库里的裸 PID 直接发 signal。所有 cancel/
adopt 都必须经 launcher 或 worker endpoint 重新核验完整 identity。

worker lock、daemon singleton lock 及控制 FD 不得泄漏给 target；target 继承它们会制造假活性或阻止
恢复。

### 8.2 result candidate

worker 将结果写入同目录临时文件，完成 file fsync 后 atomic no-replace rename，再 directory fsync。candidate
一旦发布不可修改，至少包含：

- execution/generation/spec digest；
- worker identity 与 host boot ID；
- target start/end wall time 与 monotonic duration；
- exit code 或 signal；
- termination source 及 cancel intent reference；
- target-observed stdout/stderr byte count（可带 streaming digest）；
- durable artifact byte count、最后完整 cursor、digest 与 truncation/corruption 标记；
- worker 自身版本与结果格式版本。

observed facts 描述 worker 从 pipe drain 到的字节；durable facts 只覆盖已 checkpoint 的完整 frame。
quota/truncation 时前者可大于后者。harvester 用 durable cursor/digest 核验 artifact；artifact 缺尾或
损坏只降级 artifact，不推翻已通过 identity/result-envelope 校验的真实 exit。candidate 仍不是 SQLite
terminal authority；daemon/harvester 最终调用 #205 的原子 terminal+outbox API。

### 8.3 terminal facts 与分类

#204 已冻结 Triggered Turn 可接受的 terminal status，#206 不得擅自增加第五种：

| terminal status | 可验证含义 |
|---|---|
| `completed` | target 正常退出且满足成功契约 |
| `failed` | target 非零退出、启动失败、显式用户取消或可归因的执行错误 |
| `timed_out` | worker 因持久化 max-runtime 到期终止 target |
| `lost` | 系统无法再可信证明 target 如何结束 |

`cancelled` 是 `failed` 下的 `termination_reason`，不是 terminal status；同理 signal、launch failure、
artifact failure 是 reason/evidence。completion outbox 的 sealed kind 仍只对应上表四种状态。

权威 terminal facts 至少包含 execution ID、status、occurred time、bounded result/artifact references、
原 `parent_run_id + tool_call_id` correlation、termination reason/evidence type+digest。通常 evidence 指向
worker candidate；确定性 pre-worker failure 没有 candidate 时，digest 必须覆盖 create guard、durable
materialization/launch intent，以及 daemon/launcher 对失败点的证明。全量 stdout/stderr 不得进入
terminal/outbox/RuntimeEvent/IM。

### 8.4 startup reconciliation matrix

新 daemon 取得 singleton 后逐项分类：

| 可验证事实 | 处理 |
|---|---|
| valid immutable result candidate | harvest，原子提交 terminal+outbox |
| identity-matching worker alive，deadline 未过 | 恢复为受管理 running，继续 harvest/cancel |
| identity-matching worker alive，deadline 已过 | 立即通过 verified launcher/containment 执行 timeout termination；不只等 worker 自己醒来 |
| launcher 能证明 target 从未启动 | terminal `failed/launch_failure`；v0 不创建新 generation |
| marker 存在、无 target evidence/candidate，launcher 也无法证明未启动 | `lost`；不得假设 running/launch failure 或 respawn |
| worker/target 不存在且无 valid result | `lost` + matching outbox |
| PID 存在但 identity 不匹配 | 绝不 adopt/kill；原 Execution 为 `lost` |
| host boot ID 改变，有 valid result | 允许校验并 harvest 已 durable 的结果 |
| host boot ID 改变，无 valid result | 绝不 adopt；`lost` |
| candidate 无效/部分写/identity 不符 | 隔离证据，`lost`，不得伪造成 failed |
| result 有效但 output artifact 尾部损坏 | 保留真实 terminal；artifact 单独标 corruption/truncated |

`lost` 的含义是“系统不能再可信证明它如何结束”，不是普通 non-zero exit，也不是一个方便的兜底
失败码。它必须携带 reason category 与可审计 evidence summary。

v0 不把 launcher/system service 的普通 exit notification 提升为 result authority。worker 在发布 valid
candidate 前死亡时，除非 launcher 能证明 target 从未启动，否则即使 OS 显示进程“干净退出”也只能
`lost`；以后若要支持可信 OS-native result adapter，必须另行扩展 evidence contract。

### 8.5 恢复不得回拨 terminal

Execution terminal state 是单向的。迟到 candidate、迟到 cancel 或旧 daemon claim 都不能覆盖已提交
terminal。重复 harvest 必须幂等；匹配 outbox 由同一 terminal transaction 保证只有一个逻辑事实。

## 9. Cancel、timeout 与竞争

### 9.1 cancel intent 先持久化

cancel API 先把 intent、actor、requested_at 与 reason category 持久化，再向核验通过的 worker 发请求。
“signal 发成功”不是取消完成；最终状态取决于 worker 实际观测结果。
同步返回只表达 intent `accepted`；若尚无 verified worker，则表达 `pending_recovery`。这两个都是 control
outcome，不是 Execution terminal status；调用方通过 get/wait 观察最终 `failed`（reason=`cancelled`）或其他结果。

worker 收到 cancel 或 max-runtime 到期后：

1. 向目标 containment domain 发送 graceful terminate；
2. 等待冻结策略中的 grace；
3. 仍存活则强制 kill 整个目标树；
4. drain/flush output；
5. 发布包含实际 evidence 的 result candidate。

如果找不到身份匹配的 worker，daemon 不得向裸 PID 发 signal。它保留 cancel intent，并由 recovery
判断 `lost` 或 harvest 已存在结果，cancel API 绝不提前宣称 target 已取消。

### 9.2 自然退出、取消和 timeout 竞争

自然退出可能与 cancel/timeout 同时发生。状态不能按“最后一个 API 调用”编故事：

- target 已自然退出且 worker 先观测到 exit，迟到 cancel 不把它改写为 cancelled；
- worker 先执行 max-runtime termination，并有相应 evidence，结果是 `timed_out`；
- cancel termination 有匹配 intent/evidence，结果是 `failed`，reason 为 `cancelled`；
- 无法证明谁终止了 target 时，terminal 只能是 `lost`；`unknown_termination` 是其 sealed
  termination reason/evidence category，不是第五种 status。

最终提交仍受 SQLite terminal CAS 保护。

## 10. Output artifact 与资源上限

### 10.1 逻辑契约

artifact 对外提供 append-only、单调 cursor 的 stdout/stderr chunk 序列。每个 frame 至少有 stream、
sequence、length 和完整性校验。物理上使用单文件、双文件或分段文件由实现决定，但不得改变 cursor
语义。

读取端只返回完整 frame。crash 留下的 partial tail 在恢复时截断或隔离到最后一个有效 frame，不能
把乱码当作命令输出。

### 10.2 有界性

- worker 复制输出时使用固定上限 buffer，不随总输出量增长；
- 每个 Execution 有持久化 output quota；
- 超过 quota 后继续 drain target pipe，避免 target 因 pipe backpressure 卡死，但不再无限落盘；
- result 标记 `output_truncated=true` 并保留最后 durable cursor；
- get/wait/completion 只携带有界、redacted preview 与 artifact reference，不携带全量输出；
- artifact 与目录默认 owner-only 权限，禁止进入普通 Info log 或 IM event payload。

terminal candidate 发布前必须 flush 已接受的完整 output frame。性能参数与 flush cadence 可配置，但
资格测试必须证明 crash 后丢失范围有明确上限，且文档不得宣称零丢失。

### 10.3 磁盘与 I/O 失败

artifact 写失败、quota exhausted、result candidate 无法 durable、SQLite full 是不同故障：

- artifact 超 quota：继续监督，结果可 terminal，但标 truncated；
- artifact 非 quota I/O failure：尝试有界终止 target，若仍能 durable 写 valid candidate，可提交带
  artifact failure 的 terminal；
- candidate 无法 durable：不能凭内存结果发布可信 terminal；恢复后通常为 lost；
- SQLite 无法提交 terminal+outbox：candidate 保留，harvester 重试，不能只发通知后丢事实。

## 11. Security boundary

- persisted spec、marker、result、日志、preview 不得包含明文 secret；
- worker 只获得执行所需的最小环境和文件权限，不继承 daemon 全量环境；
- command spec 必须 sealed，worker 不接受任意后续参数注入；
- artifact path 根据 execution ID 派生并防目录穿越、跨 Execution 读取；`O_NOFOLLOW`/等价能力用于
  防意外或陈旧 symlink，并作为对抗性 symlink swap 的 defense-in-depth，不宣称构成完整隔离；
- cancel 必须经 daemon 权限判定，worker endpoint 还要核验 launch nonce/peer identity；
- result/candidate 解析按不可信输入处理：长度有界、版本校验、未知字段策略明确；
- worker executable、launcher profile 和 artifact root 的权限是 production qualification 的一部分。

v0 不宣称能抵御与 Xira 同 OS 用户、可主动读取/篡改 state-dir 的恶意 target；那需要 #148 的 OS/
container 隔离。production 必须把 state-dir 放在 command write roots 之外，且不向 target 暴露内部路径，
以阻止误写并缩小攻击面；评审 terminal “可信”时必须同时写明这一 threat-model 前提。

## 12. Qualification companion

运维 NFR、unified create guard、retention/GC、failure matrix、contract tests 与 production capability
qualification 由同 Gate companion
`xira-execution-worker-ownership-recovery-qualification-v0.zh.md` 冻结。两份文档必须一起 review/Accepted；
companion 不得改变本文 ownership/terminal 决策，只把它们转成可测门槛。

## 13. 备选方案与取舍

| 方案 | 结论 | 原因 |
|---|---|---|
| daemon 直接持有命令 | 拒绝 | daemon crash 后 pipe、计时器、结果与进程 ownership 同时丢失 |
| `nohup`/double-fork target | 拒绝 | 只解决“还活着”，没有身份、结果、取消和 containment |
| worker 直接写 SQLite terminal | 拒绝 | 破坏唯一写入者与 #205 terminal+outbox 原子边界 |
| 仅用 PID/PID 文件恢复 | 拒绝 | PID 复用与 stale file 会误 adopt/误杀无关进程 |
| 同一 Execution 自动重试 launch | 拒绝 | 即使 target 未被观测到，也不该在旧 tool call 下悄悄引入后发副作用 |
| 把 quota rejection 伪造成 failed Execution | 拒绝 | 从未 admission 的请求不应生成 terminal/outbox；使用 create guard rejected branch |
| receipt/Execution 分表各自查 key | 拒绝 | 无跨表唯一 winner，replay 与 admission 会竞态；统一由 create guard 路由 |
| 用两个独立 TTL 管 guard/history | 拒绝 | 两者最终都删除会失忆；删除 guard 前必须先 fence origin replay authority |
| 把 systemd 写死进核心 | 拒绝 | 将产品契约与部署实现耦合，开发/测试/演进成本过高 |
| portable worker + qualified launcher | 采用 | ownership 稳定，同时允许 Linux 严格、macOS 显式降级 |
| 宣称 macOS 与 Linux recovery 等价 | 拒绝 | 没有对应 OS 证据就是虚假能力，silent data loss 风险更高 |
| artifact/result 只写 JSON 后覆盖 | 拒绝 | crash partial write 与并发读无法可靠分辨 |

## 14. 后续实现切分约束

本 Gate Accepted 后，才允许从 #202 创建实现 issue。实现切分应按契约边界而不是按“文件数量”：

1. Execution schema/create policy 与 state machine；
2. artifact/result format 与 portable exec-worker；
3. Linux qualified launcher、daemon singleton 与 recovery harvester；
4. get/list/wait/log/cancel tool/API；
5. #205 completion outbox integration 与 platform qualification。

这些只是推荐切分，不在本 RFC 中提前锁定 issue 数量或 PR 顺序。每个实现 PR 都以
`milestone/managed-execution-v0` 为 base，不直接进入 release `main`。

## 15. Acceptance checklist

- [ ] daemon / worker / target ownership 无重叠或无人负责区；
- [ ] persist-before-spawn 和 ambiguous launch 恢复规则可执行；
- [ ] PID reuse、reboot、worker/daemon crash 都有诚实结果；
- [ ] worker 不绕过 #205 terminal+outbox 原子 API；
- [ ] Linux production 与 macOS development capability 差异明确；
- [ ] cancel/timeout/natural exit 竞争不靠 wall-clock 猜测；
- [ ] output artifact、quota、partial write、disk failure 有界；
- [ ] active quota、retention/GC、latency、RPO/RTO 与 admin inspection 可测试；
- [ ] qualification companion 与本文同时完成 review，未产生第二套语义；
- [ ] security、secret、path 与 worker 权限边界明确；
- [ ] failure/test/qualification matrix 足以阻止 silent data loss；
- [ ] PTY/stdin/service/distributed execution 未偷渡进 v0；
- [ ] #202/#204/#205/#206 的引用与状态同步完成。

在 checklist 全部核实、review 结论闭环并完成 issue 回引前，本 RFC 保持 **Proposed**，不得作为
“设计已定”启动实现。
