# Xira Completion Reliability 平台资格验证 v0

- 状态：Accepted（2026-07-28）
- 关联：#205、#206、PR #208
- 核心契约：`xira-completion-outbox-reliability-rfc-v0.zh.md`

## 1. 目的与边界

本文只负责 SQLite WAL、文件系统、断电/进程级故障的 release/manual/nightly 资格验证。核心 RFC §11 的
自动化 contract matrix 负责状态机与 CAS 分支 100% 覆盖；这里的机器验证不计入包级覆盖率，也不能用一次
开发机测试替代持续的 contract tests。

资格验证失败表示该平台不得被标为 supported，不能静默降级到内存队列或放宽 durability。

## 2. Evidence 格式

每次运行至少保存：

- git commit、schema version、Go/SQLite 版本；
- OS/kernel、文件系统与 mount options；
- case ID、fault 注入点、开始/结束时间、通过/失败；
- integrity check、reconciliation 后 record counts/digests；
- 完整日志与 artifact 路径，secret/env value 必须脱敏。

## 3. 资格矩阵

| Case | 平台/故障 | 必须证明 | 频率 |
|---|---|---|---|
| PQ-01 | macOS APFS | migration、WAL restart、claim recovery、integrity check 通过 | release |
| PQ-02 | Linux ext4 | 同 PQ-01；service restart 后 ready 前 reconciliation 完成 | release |
| PQ-03 | Linux overlayfs | 容器目标环境若声明支持，完成同 PQ-02；否则明确 unsupported | release |
| PQ-04 | NFS/不支持 locking FS | startup fail closed，稳定健康错误，不创建内存 fallback | release |
| PQ-05 | commit/power-loss fault suite | 每个 terminal/outbox/handling/delivery commit 边界 crash 后无半状态 | nightly + release |
| PQ-06 | WAL/DB `FULL/IOERR/READONLY` | Runtime fail-stop/health failure；原 record 不被伪造为 dead letter | nightly |
| PQ-07 | database `CORRUPT` | startup/运行期给出稳定诊断，禁止调度；恢复操作需 admin | nightly |
| PQ-08 | attempt goroutine hang | deadline+grace 后同 daemon scanner fence/cancel/requeue，旧 token 失效 | nightly |
| PQ-09 | daemon-wide hang | #206/deployment supervisor 终止并重启；新 daemon 持 singleton 后恢复 | release |
| PQ-10 | host clock forward/backward jump | grace/fencing 防双 commit，延迟与 recovery metric 可见 | nightly |

## 4. 支持声明

release evidence 必须列出 `supported`、`unsupported`、`not-qualified` 三类，不能把“没测”写成支持。新增 OS、
文件系统或容器存储驱动时先补本矩阵并保存 evidence，再改支持声明。

PQ-09 只验证 #205/#206 的交界：whole-daemon freeze 不可能靠 daemon 内 scanner 自救，必须由进程外
supervisor/watchdog 终止旧进程；singleton 证明新 daemon 接管前不存在两个可继续 commit 的实例。
