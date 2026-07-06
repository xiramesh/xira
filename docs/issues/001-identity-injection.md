# 001: 身份注入 — sender/chat → prompt

> **GitHub 号**:https://github.com/xiramesh/xira/issues/120（本地编号 001）
> **状态**:open
> **依赖**:无
> **优先级**:高(最低悬果实,A/B 都受益,不依赖 owner / Docker)
> **架构来源**:[xira-ownership-isolation-v0.zh.md](../architecture/xira-ownership-isolation-v0.zh.md) §1、§3.3、§7

## 问题

agent 不知道"跟谁说话、在哪说话"。飞书场景实测:`composeInstructionText`(service.go:1632)注入了 agent 身份 + 日期 + 工具列表,但**没有 sender / chat / channel**。agent 说"我不知道你是谁"是真的。

这是后续所有身份相关能力(owner 识别、个性化、跨 channel)的基础——必须先打通"sender/chat → InboundContext → prompt"这条路。

## 现状(已核实 2026-07-07)

- `InboundContext`(`channel/types.go:5`)字段:`Channel`、`ChannelAppID`、`ChatID`、`ChatType`、`SenderID`、`ReplyToSenderID`——**全是 ID,没有 name**。
- `composeInstructionText`(`runtime/service.go:1632`)注入:`profile.ID`、`profile.Name`、`time.Now()`、工具列表。**无 sender/chat**。
- 飞书事件带 `message.Mentions`(已用),但 sender/chat 的 name 字段**没提取**。

## 目标

入站消息到达时,InboundContext 携带可读身份;composeInstructionText 注入"对话场景"块,让 agent 知道:

- 正在与谁对话(sender name + id)
- 在哪个场景(chat name + id + type:群聊/私聊)
- 在哪个 channel(feishu / ilink / cli / tui)

## 拆解

1. **InboundContext 加 name 字段**(代码层):
   - 加 `ChatName string`、`SenderName string`(只加 name,id 类字段已有)。
   - 确认序列化/持久化兼容(InboundContext 有 `json`/`yaml` tag)。

2. **各 channel runner 提取 name**:
   - feishu:从飞书事件取 sender name + chat name(飞书 API 是否在消息事件里直接给 name?核实——可能要额外调 `contact` API。先看事件 payload,有就用,没有就标 TODO)。
   - ilink:从 ilink payload 取(可能没有 name,只有 id;空就是空)。
   - cli/tui:本地用户名(os.user)或 "local-user"。

3. **composeInstructionText 注入对话场景**:
   - 新增 `# Conversation Context` 块,放 sender/chat/channel 信息。
   - 这一步**只注入、不改 agent 行为**——agent 自己决定怎么用(个性化回复 vs 不用)。
   - 注意:composeInstructionText 当前签名是 `(profile, skillBlocks)`,要改成能拿到 InboundContext(或单独传一段 conversation context 文本)。**核实调用链**——RunAgent 那一层有 InboundContext 吗?

## 不做什么(out of scope)

- 不做 owner 识别(owner 是 #003)——这里只注入"发言人是谁",不判断"是不是主人"。
- 不做个性化回复逻辑——agent 决定,不强制。
- 不做跨 channel user 合并(deferred)。

## 验证

- TDD:先写测试——`composeInstructionText` 注入的 prompt 里包含 sender/chat 信息。
- 飞书 live 测试:实测发一条消息,看 agent 回复是否体现"知道跟谁说话"(用真 DeepSeek key,§5.3 双门控)。
- 覆盖率 ≥85%。

## 风险

- **飞书 name 可能要额外 API 调用**:消息事件可能只有 open_id 没 name。如果调 `contact` API 拉 name,会引入网络依赖 + 鉴权。先核实事件 payload,有就用,没有就空(SenderName 为空),不强行加 API 调用(那会变成独立工作量)。
- **签名扩散**:composeInstructionText 改签名,所有调用方都要改。核实有几处调用,别漏。
