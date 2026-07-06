# 008: `user.md` 用户档案

> **GitHub 号**:https://github.com/xiramesh/xira/issues/127（本地编号 008）
> **状态**:open
> **依赖**:007(per-sender 数据隔离,要先把目录结构建出来)
> **优先级**:中(有了 #007 才有意义)
> **架构来源**:[xira-ownership-isolation-v0.zh.md](../architecture/xira-ownership-isolation-v0.zh.md) §3.3、§7.4

## 问题

没有 per-sender 的用户档案文件。agent 记不住"这个用户是谁、偏好什么",每次对话从零开始。

## 现状

- 无 user.md / USER.md / PROFILE.md 任何形式的用户档案。
- 聊天历史 per-sender 隔离(session.dimensions),但那是会话历史,不是档案。

## 目标

每个 sender 一个 `user.md`,记录这个人的档案:

- 身份信息(从 #001 身份注入来:sender_id、name、channel)。
- 偏好(交互过程中 agent 整理出的)。
- **agent 自己读写**(运行时更新),不是手写。

## 命名定案:叫 `user.md`

- 内容是"人"的档案(user 是主体),不是"发消息者"(sender 是角色)。
- 未来跨 channel 同一 user 会来(deferred):一个 user 可能对应多个 sender_id。`user.md` 扛得住这次升级,`sender.md` 不行。
- **隔离钥匙仍是 `sender_id`**——目录按 sender 隔离(`users/{sender_id}/user.md`),文件内容描述 user。两者不矛盾。

## 安全条件(必须记死)

`user.md` 安全的前提是**物理在 per-sender 目录里**,和 agent 人设(`agents/{agent}/`)隔开。OpenClaw / Hermes 的 USER.md 撞车(agent 自我和对话方档案语义混淆)是因为它俩把这两样塞进同一个全局 `memories/`。**安全性是目录结构给的,不是名字给的**——挪目录前先想清楚。

## 拆解

1. **目录结构**(依赖 #007):`workspace/users/{sender_id}/user.md`。
2. **user.md 格式**:定义 frontmatter 或 markdown 结构(身份 + 偏好)。
3. **agent 读写机制**:
   - 读:对话开始时把 user.md 注入 prompt(类似 skill 注入)。
   - 写:agent 通过某个工具(update_user_profile?)更新,或者每次对话结束由 runtime 整理。
4. **首次创建**:sender 第一次对话时,user.md 不存在 → 创建空模板。

## 不做什么

- 不做 memory(#009,那是交互记忆,user.md 是档案)。
- 不做跨 channel user 合并(deferred)。

## 验证

- TDD:user.md 创建、读写、注入 prompt。
- live 测试:多次对话后 user.md 有内容,agent 行为体现"记得这个用户"。
- 覆盖率 ≥85%。

## 风险

- **agent 自写的可靠性**:让 LLM 自己更新 user.md 会写偏、写漏、写错。第一版可以**只读注入,agent 不写**——需要更新时人工或后续机制。
- **prompt 长度**:user.md 内容多了会撑爆 prompt。控制大小 + 淘汰策略。
