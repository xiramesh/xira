# 002: 授权 allowlist(allowed_senders)

> **GitHub 号**:https://github.com/xiramesh/xira/issues/121（本地编号 002）
> **状态**:open
> **依赖**:无
> **优先级**:高(A 配置基础访问控制,B 配置也需要)
> **架构来源**:[xira-ownership-isolation-v0.zh.md](../architecture/xira-ownership-isolation-v0.zh.md) §1.2(授权关系)

## 问题

没有 sender 级的访问控制。现在任何人 @ agent 都会响应(受 `respond_to_unmentioned_group_messages` 控制 mention 过滤,但没有"这个 sender 被授权了吗"的检查)。

## 现状(已核实 2026-07-07)

- 仓库里的 allowlist 全是 **tool 级**:`runtimeToolAllowlist` / `runtimeToolInputAllowlist`(`runtime/service.go:671-744`)。控制"哪些工具能调",不是"哪些 sender 能用"。
- entrypoint `Definition`(`entrypoints/registry.go:11`)有 `AllowedAgents`(哪些 agent 可用),**没有 `AllowedSenders`**。
- mention 过滤(`feishu/runner.go:374 shouldHandleMessage`)只判断"@ 没@",不判断"@ 的人是不是被授权"。

## 目标

entrypoint / profile 级配置 `allowed_senders`(sender id 列表),入站消息若 sender 不在 allowlist 且非 owner(owner 是 #003)→ **忽略,不进 RunAgent**。

## 拆解

1. **配置字段**:entrypoint `Definition` 加 `AllowedSenders []string`(空 = 允许所有,兼容现有行为)。
2. **校验逻辑**:runner 层(或 Router 层)在 mention 过滤之后、RunAgent 之前,加 sender 授权检查。
3. **owner 例外**:这个 issue 不实现 owner(owner 是 #003),但设计上要留接口——"allowlist 命中 OR 是 owner"。实现时 owner 检查留 TODO 钩子,#003 接上。
4. **拒绝行为**:非授权 sender 发的消息**静默忽略**(不回复"你没权限",那会泄露 bot 存在)。和 `respond_to_unmentioned_group_messages=false` 的行为一致。

## 不做什么

- 不做动态 allowlist 管理(pairing 流程拉黑名单 = #004)。这里是**静态配置**。
- 不做 owner(owner = #003)。
- 不做 skill 级权限(skill 权限 = #006)。

## 验证

- TDD:非授权 sender 的消息被过滤掉,授权 sender 的消息进 RunAgent。
- 覆盖率 ≥85%。
- live 测试:飞书用授权账号和未授权账号各发一条,看行为差异。

## 风险

- **校验放哪一层**:放 channel runner(每个 channel 各写一遍)还是放 Router(集中)?Router 层更干净,但核实 Router 是否拿到 sender id——拿到 InboundContext 才行。**核实 Router → RunAgent 的调用链**。
- **空 allowlist 语义**:空 = 全允许 vs 空 = 全禁止?倾向**空 = 全允许**(向后兼容,现有 entrypoints.yaml 不动就不会 break)。
