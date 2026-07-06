# 003: owner 绑定数据模型

> **GitHub 号**:https://github.com/xiramesh/xira/issues/122（本地编号 003）
> **状态**:open
> **依赖**:无(可独立,但建议在 #001 之后做——身份注入先打通)
> **优先级**:中(owner 链的根,后续 4/5/6 都依赖它)
> **架构来源**:[xira-ownership-isolation-v0.zh.md](../architecture/xira-ownership-isolation-v0.zh.md) §1.1、§1.2、§1.3

## 问题

没有 owner 概念。运行时无法回答"这个 sender 是这个 agent 的主人吗"。这是统一架构的核心原语——owner 绑定不存在,所有 B 配置能力(@ owner 代理、特权 skill)都无从谈起。

## 现状

- `agents/profile.go` 的 `Permissions` 结构有 `AllowRoots`,**没有 owner 字段**。
- runtime 不注入 sender/owner 身份(这是 #001 的事,先做)。
- pairing 基建(ilink channel)存在,但只用于"授权 ilink 账号接入",**不建立 ownership 关系**。

## 目标

定义并持久化 **per-instance 的 ownership 关系**:一个 agent 实例绑定 0 或 1 个 owner。运行时提供查询:`IsOwner(senderID) bool`。

### 基数约束(硬约束,架构文档 §1.3)

- per-instance owner 数量:0 或 1。
- 单实例跑 B 配置时:仅逻辑保证(代码 + 信任部署环境)。
- 多实例(Docker,wontfix):物理保证(目前不做)。
- **绝不允许 N 个 owner**——退化成"又一堆授权用户",B 语义崩塌。

## 拆解

1. **数据结构**:
   - `Ownership` struct:`{ AgentID, OwnerSenderID, OwnerChannel, BoundAt }`。
   - 存哪?核实——per-instance state(`.xira/state/`?)还是 entrypoint config?倾向**配置文件优先**(部署者显式声明 owner),运行时不支持动态改(动态改 = #004)。
   
2. **配置形态**:
   - entrypoint / profile 加 `owner` 字段:`{ sender_id: "wxid_xxx", channel: "ilink", display_name: "大明" }`。
   - 空 = 无 owner(A 配置)。

3. **查询接口**:
   - `func (s *Service) IsOwner(ctx context.Context, senderID, channel string) bool`。
   - 放 Service 层,RunAgent 和后续 #005/#006 都用。

4. **持久化 + 加载**:
   - 启动时加载 owner 声明,运行时只读。
   - 动态绑定(#004)再考虑运行时写。

## 不做什么

- 不做 owner 绑定**流程**(怎么把一个 sender 绑成 owner = #004)——这里只做**数据模型 + 查询**,owner 来源是配置文件静态声明。
- 不做 @ owner 代理(#005)。
- 不做特权 skill(#006)。
- 不做 Docker(wontfix)——单实例靠逻辑保证。

## 验证

- TDD:配置 owner → `IsOwner` 返回正确;配置空 → 所有人返回 false(A 语义);配置两个 owner → **应该拒绝**(基数约束)。
- 覆盖率 ≥85%(关键契约代码追求 100%,AGENTS.md §5.2)。

## 风险

- **滚雪球高危**:这是 owner 链的根,#004/#005/#006 都长在它上面。数据结构定错,后面全要返工。**先把数据模型定稳,别急着上 #004**。
- **per-instance 概念在单进程下不清晰**:现在一个进程跑多 agent,每个 agent 一个 owner 声明——per-instance 在这里退化为 per-agent。文档里 per-instance = per-agent-profile,核实术语一致性。
