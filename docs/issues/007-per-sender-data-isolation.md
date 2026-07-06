# 007: per-sender 工具数据隔离

> **GitHub 号**:https://github.com/xiramesh/xira/issues/126（本地编号 007）
> **状态**:open
> **依赖**:无(独立于 owner 链)
> **优先级**:中(A 配置多用户必需,B 配置也受益)
> **架构来源**:[xira-ownership-isolation-v0.zh.md](../architecture/xira-ownership-isolation-v0.zh.md) §2.2、§3.1

## 问题

工具的文件读写(write_file / edit_file / outcome 的 decision log)落**共享 workspace**,没有 per-sender 隔离。多用户场景下,张三写的 decision 和李四的混。

## 现状

- `pathWithinRoots`(`tools/sandbox.go`)做沙盒边界检查,加了 symlink 安全(#W2)。
- `SandboxRoots` 来自 profile(`agents.Permissions.AllowRoots`),是 **per-profile 共享**的,不是 per-sender。
- 工具调用拿不到 sender id(核实——ToolContext 里有 sender 吗?)。

## 目标

agent 调 `write_file("decision.md")` 时,实际落到 `workspace/users/{sender_id}/data/decision.md`。**透明 rewrite**:agent 看到的还是 `decision.md`,物理隔离 per-sender。

## 拆解

1. **per-sender root 注入**:
   - 工具调用前,SandboxRoots 加一个 `users/{sender_id}/data/` 作为 root。
   - 这个 root 要传给 pathWithinRoots,让 `write_file("decision.md")` 解析到这个目录。
   - 透明 rewrite 的关键:工具看到的相对路径不变,sandbox 层做映射。

2. **ToolContext 拿到 sender**:
   - 核实 ToolContext 的字段——有没有 sender_id?
   - 没有 → 要从 InboundContext 传进来(#001 加了 sender,这里要用)。

3. **per-agent 层保留**:
   - skills 定义、agent persona、KB 仍在 per-agent 共享层(不动)。
   - 只 scope 工具的**数据读写**,不 scope 技能定义。

## 不做什么

- 不做 user.md(#008)。
- 不做 memory(#009)。
- 不做跨 channel user 合并(deferred)。

## 验证

- TDD:sender A 写文件 → 落 A 目录;sender B 写同名文件 → 落 B 目录;互相不可见。
- symlink 逃逸防护要继续有效(不能因为加 per-sender root 绕过 #W2 的检查)。
- 覆盖率 ≥85%。

## 风险

- **透明 rewrite 的复杂度**:agent 可能用绝对路径(不是相对),或者 `../` 跨目录。pathWithinRoots 要覆盖所有边界。
- **outcome skill 的特殊路径**:outcome 的 decision log 可能有固定路径(`vault/decision-log.md` 之类),核实它的写入路径,不能简单 rewrite 了事。
- **per-agent KB 的读写**:KB 是共享的,不能被 per-sender rewrite 误伤。区分"用户数据写"和"KB 读"。
