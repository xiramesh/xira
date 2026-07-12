# 009: memory 系统

> **GitHub 号**:https://github.com/xiramesh/xira/issues/128（本地编号 009）
> **状态**:done（#128）；agent scope follow-up：#159
> **依赖**:007(per-sender 数据隔离)
> **优先级**:低(最不紧急,依赖链最深)
> **架构来源**:[xira-ownership-isolation-v0.zh.md](../architecture/xira-ownership-isolation-v0.zh.md) §3.1、§6.1

## 问题

没有 memory。agent 跨会话记不住"我们之前聊过什么"(会话历史是 per-sender 的,但那是原始记录,不是整理后的记忆)。

## 当前实现

- #128 已实现 per-sender `memory.jsonl`、`update_memory`、`forget_memory`、expiry、active memory prompt 注入和真 DeepSeek live test。
- 现有文件按真实 SenderID 隔离，省略 scope 的工具调用继续使用这一行为。
- #159 在同一 memory 系统中增加 `agent` scope，让 Agent 显式记录属于自己的事项、经验和上下文；不新增 Notes / owner inbox / 第二套 Agent Loop。

## 目标

sender/agent 双 scope memory:

- agent 在对话中整理出"关于这个用户的事实 / 偏好 / 上下文"。
- agent 也能显式记录"我自己以后要记住的事项 / 经验 / 上下文"。
- 跨会话保留,下次对话注入 prompt。
- sender 与 agent 地址空间物理隔离；模型只选择 sealed scope，不能指定任意身份 ID。

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
- 不做 runtime 自动提升（不把 sender memory 静默复制成 agent memory）。
- 不做跨 user 的 sender memory 共享；agent memory 是 Agent 自己的状态，不是合并用户档案。
- 不做 cron；未来 cron 只触发同一个 Agent Loop 读取 agent memory。

## 验证

- TDD:memory 写入、读取、注入。
- live 测试:跨多次对话验证记忆保留。
- 覆盖率 ≥85%。

## 风险

- **这个 issue 最不确定**:存储 / 写入 / 检索 / 淘汰每个都有多种选择。**建议先开 RFC 讨论设计,再落 issue 实施**——直接写代码会返工。
- **agent 自写提升陷阱**(§6.1):agent 学到"对所有用户有用的事实"想写 per-agent KB,有泄露 sender 私有上下文的风险。安全默认:一律写 per-sender,绝不自动提升。
