# 004: owner 绑定流程(pairing-based)

> **GitHub 号**:https://github.com/xiramesh/xira/issues/123（本地编号 004）
> **状态**:open
> **依赖**:003(owner 数据模型)
> **优先级**:中(B 配置可用后,部署体验才好)
> **架构来源**:[xira-ownership-isolation-v0.zh.md](../architecture/xira-ownership-isolation-v0.zh.md) §1.2、§1.3

## 问题

#003 的 owner 来自配置文件静态声明——部署者要预先知道自己的 sender_id。但用户**不知道自己的 sender_id**(只有 agent 收到消息时才能拿到)。所以需要一个**绑定流程**:用户在 IM 里发起,agent 验证,建立 ownership 关系。

## 现状

- pairing 基建存在(ilink channel,`channelrunner/ilink/runner.go`),但只用于 channel 接入,不建立 ownership。
- pairing code 机制:8 字符码、`secrets.choice()`、文件存储。可复用思路。
- OpenClaw / Hermes 的 pairing 都是 CLI approve(`hermes pairing approve <platform> <code>`),**不适合纯 IM 用户**——我们要的是 IM 内完成。

## 目标

用户纯 IM 操作完成 owner 绑定:

1. 部署者启动 agent 时拿到一个**claim token**(部署时生成,打印一次)。
2. 用户在 IM 里发"绑定" + claim token 给 agent。
3. agent 验证 token,把 sender_id 写入 ownership(首个绑定的 = owner,后续 token 作废)。
4. 防冒领:claim token 一次性,先到先得,部署者控制发放。

## 拆解

1. **claim token 生成**:部署/启动时生成,打印到日志,部署者复制给用户。
2. **绑定指令**:agent 识别 IM 消息里的"绑定 <token>"模式(和现有 human_interpret 信号识别类似)。
3. **写入 ownership**:复用 #003 的数据模型,但这是**运行时写**(#003 是静态读)。
4. **幂等 + 防冒领**:已绑定 owner 的实例,新 token 作废;token 用一次就失效。

## 不做什么

- 不做"解绑" / "换 owner"(owner 已绑定就锁死,要换重启 + 改配置)。
- 不做多 owner(N=1 硬约束,#003)。
- 不做 CLI approve 流程(纯 IM)。

## 验证

- TDD:绑定流程的 happy path + 冒领场景(用过的 token 失效)+ 重复绑定(已绑定拒绝)。
- live 测试:飞书实测绑定一遍。
- 覆盖率 ≥85%。

## 风险

- **claim token 载体**:打印到日志,部署者要去看日志。要不要支持配置文件预设 claim token?倾向日志生成(避免配置文件里明文 token 留痕)。
- **绑定指令的识别**:在 IM 里识别"绑定 X"——会和正常对话竞争吗?建议加固定前缀(`/bind <token>` 之类),核实 channel 是否支持前缀消息。
