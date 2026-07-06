# 009: memory 系统

> **GitHub 号**:https://github.com/xiramesh/xira/issues/128（本地编号 009）
> **状态**:open
> **依赖**:007(per-sender 数据隔离)
> **优先级**:低(最不紧急,依赖链最深)
> **架构来源**:[xira-ownership-isolation-v0.zh.md](../architecture/xira-ownership-isolation-v0.zh.md) §3.1、§6.1

## 问题

没有 memory。agent 跨会话记不住"我们之前聊过什么"(会话历史是 per-sender 的,但那是原始记录,不是整理后的记忆)。

## 现状

- 无 memory 系统。
- 会话历史 per-sender 隔离(session.dimensions)。
- OpenClaw MEMORY.md / Hermes MEMORY.md 都是全局单文件(它们的多租户都炸在这),我们 per-sender(架构文档 §3.1)。

## 目标

per-sender 的 memory:

- agent 在对话中整理出"关于这个用户的事实 / 偏好 / 上下文"。
- 跨会话保留,下次对话注入 prompt。
- per-sender 隔离(`users/{sender_id}/memory/`)。

## 和 #008 user.md 的区别

| | user.md(#008) | memory(#009) |
|---|---|---|
| **内容** | 档案(身份 + 偏好) | 交互记忆(事实 + 上下文) |
| **大小** | 小,慢增长 | 较大,持续增长 |
| **来源** | agent 整理 + 手动 | agent 自动整理 |
| **检索** | 全量注入 prompt | 可能需要检索(向量?) |

user.md = "这个人是张三,喜欢简洁回复";memory = "上周张三问过报销流程,我说要找 HR"。

## 拆解(最不确定的 issue,设计优先)

1. **存储**:per-sender markdown 文件?还是数据库?第一版倾向**文件**(简单,和 user.md 一致)。
2. **写入时机**:每轮对话结束?还是 agent 主动写?第一版**轮次结束 runtime 整理**(不让 LLM 主动写,避免乱写)。
3. **检索**:全量注入 prompt(简单,撑爆)还是按需检索(复杂,要向量)?第一版**全量注入 + 大小上限**,超出再考虑检索。
4. **淘汰**:memory 太多了怎么办?第一版**无淘汰**(让它涨,观察)。

## 不做什么

- 不做向量检索(第一版全量注入)。
- 不做自动提升到 per-agent KB(§6.1 deferred,policy 问题)。
- 不做跨 user 的 memory 共享。

## 验证

- TDD:memory 写入、读取、注入。
- live 测试:跨多次对话验证记忆保留。
- 覆盖率 ≥85%。

## 风险

- **这个 issue 最不确定**:存储 / 写入 / 检索 / 淘汰每个都有多种选择。**建议先开 RFC 讨论设计,再落 issue 实施**——直接写代码会返工。
- **agent 自写提升陷阱**(§6.1):agent 学到"对所有用户有用的事实"想写 per-agent KB,有泄露 sender 私有上下文的风险。安全默认:一律写 per-sender,绝不自动提升。
