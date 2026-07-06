# 005: @ owner 代理

> **GitHub 号**:https://github.com/xiramesh/xira/issues/124（本地编号 005）
> **状态**:open
> **依赖**:003(owner 数据模型,要知道 owner 是谁)、001(身份注入,要拿到 mention)
> **优先级**:中(B 配置的核心创新,业界没做)
> **架构来源**:[xira-ownership-isolation-v0.zh.md](../architecture/xira-ownership-isolation-v0.zh.md) §1.4、§4.2

## 问题

B 配置下,群里有人 @ 主人(不是 @ agent),agent 要识别"这是叫我主人"并**默默处理**。这是 A 形态没有的、B 形态的核心价值——但业界(OpenClaw / Hermes)都没做。

## 现状

- 飞书 `message.Mentions` 已解析(mentioned list 有 user_id)。
- 但 agent 不知道"主人是谁",无法匹配 mention → owner。
- 没有"默默处理"的行为定义。

## 目标

owner 绑定存在(#003)+ config 开启时:

- 群里有人 @ owner(mention 列表含 owner sender_id)→ agent 进入"代理模式"。
- **默默处理**:不公开回复。具体处理什么(建 task?记 vault?只记 session?)见"待设计"。
- @ agent 仍公开回复(不变)。
- @ owner + @ agent 同时:优先级待定。

## 待设计(这个 issue 会吃很多轮 review,先别急着写代码)

这些是**产品决策**,不是技术决策,需要团队讨论定案:

1. **"默默处理"具体做什么?**
   - 选项 a:只记 session(下次主人问起能答),不主动动作。
   - 选项 b:建 task(需要主人有 task 系统)。
   - 选项 c:记 vault(需要 vault skill)。
   - 选项 d:私聊通知主人("群里 X @ 你说了 Y")。
   - **倾向先做 a + d**:最简,不依赖额外系统。

2. **@ owner + @ agent 同时?**
   - 优先级:公开回复优先 vs 默默处理优先?
   - 倾向:**公开回复优先**(避免主人漏看)。

3. **默默处理的范围**:
   - 哪些消息触发?所有 @ owner 还是只有"看起来需要处理的"?
   - 避免 agent 过度解读——闲聊里 @ 主人也要触发吗?

## 拆解(待设计定案后才完整)

1. mention 匹配 owner id → 判定"@ owner"。
2. config 开关:`owner_mention_proxy: true/false`(绑了 owner 但不想用 = 关)。
3. "默默处理"实现(取决于待设计结论)。
4. 通知机制(如果要通知 owner)。

## 不做什么

- 不做 @ owner 的"主动回复"(那就是普通 @ agent 行为)。
- 不做跨 channel @ owner 追踪(deferred)。

## 验证

- TDD:mention 含 owner → 触发代理;mention 不含 owner → 不触发;config 关 → 不触发。
- live 测试:飞书群里 @ 主人,看 agent 行为。
- 覆盖率 ≥85%。

## 风险(高危)

- **这个 issue 是"修一个破一个"的高发区**(AGENTS.md §2)。处理逻辑一旦做复杂(建 task + 记 vault + 通知 + 判断范围),会连续触发新边界。**建议第一版只做"记 session + 通知",把建 task / vault 留后续**。
- **"默默处理"的不可靠性**:agent 判断"这条 @ 主人需不需要处理"很容易误判。第一版可以**一律触发**(所有 @ owner 都记 session),不做语义判断。
