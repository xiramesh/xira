# 006: 特权 skill(owner-only)

> **GitHub 号**:https://github.com/xiramesh/xira/issues/125（本地编号 006）
> **状态**:open
> **依赖**:003(owner 数据模型)、002(allowlist,owner 是 allowlist 的特权成员)
> **优先级**:中(B 配置的权限隔离)
> **架构来源**:[xira-ownership-isolation-v0.zh.md](../architecture/xira-ownership-isolation-v0.zh.md) §1.4、§4.2

## 问题

B 配置下,群里同事 @ agent 能用通用 skill,但**不能用 owner 的私人 skill**(如 outcome——写 owner vault)。现在所有 skill 对所有授权 sender 平等开放。

## 现状

- skill 在 agent profile 里声明(`profile.Skills`),激活时通过 `activateSkills`(service.go)加载。
- **没有 skill 级权限模型**——skill 激活了,任何授权 sender 都能用。
- owner 概念不存在(#003)。

## 目标

skill 分两类:

- **public skill**:所有授权 sender 可用(allowlist 命中即可)。
- **owner-only skill**:只有 owner 能用。

非 owner 调 owner-only skill → **拒绝**(提示"这个能力只我的主人能用")。

## 拆解

1. **skill 声明 owner-only**:
   - 哪里声明?skill 自己声明(`SKILL.md` 里加字段)还是 profile 声明(`owner_only_skills: [outcome, ...]`)?
   - **倾向 profile 声明**:集中、易审计、不污染 skill 定义(skill 可被多 profile 复用,在 profile 标"对这个 agent 是 owner-only"更灵活)。

2. **运行时拦截**:
   - `activateSkills` 时按当前 sender 过滤——非 owner 调用,把 owner-only skill 从激活列表去掉。
   - 拦截点:核实 activateSkills 的调用链,它拿得到 sender 吗?拿不到要改签名或从 ctx 取。

3. **拒绝消息**:
   - agent 对非 owner 说"这个能力只我的主人能用"(注入 prompt 让 agent 自己说)。
   - 或者直接从工具列表移除,agent 根本不知道这个 skill 存在(更干净)。

## 不做什么

- 不做 skill 级细粒度权限(public / owner-only / friend-only 三态)——先二态。
- 不做动态授权(运行时给某 sender 临时开 owner-only)。

## 验证

- TDD:owner 调用 → skill 激活;非 owner 调用 → owner-only skill 不激活(或被拦截)。
- live 测试:owner 和非 owner 各调用一次 outcome,看行为。
- 覆盖率 ≥85%。

## 风险

- **activateSkills 调用链**:它是 profile 级加载,可能不知道当前 sender。核实它在 RunAgent 流程里的位置——sender 信息要传进来。
- **拒绝方式**:从工具列表移除 vs 注入"拒绝"指令——前者更干净(agent 不会"想要"用),后者更可解释(agent 会说"你不能用")。倾向前者。
