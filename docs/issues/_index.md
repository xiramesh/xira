# Xira Issues 索引（身份与隔离 Epic #119）

> 本目录是 [Epic #119](https://github.com/xiramesh/xira/issues/119) 的子 issue 路线图索引，与 GitHub 双向映射。每个文件是 issue 创建时的完整描述快照（GitHub issue 是 source of truth，本目录供离线浏览 + 保留设计上下文）。
> 状态约定：`[open]` / `[in-progress]` / `[done]` / `[deferred]` / `[wontfix]`。

## 架构来源

所有 issue 源自 [`docs/architecture/xira-ownership-isolation-v0.zh.md`](../architecture/xira-ownership-isolation-v0.zh.md)（统一身份与隔离架构 v0）。拆解原则：每个 issue 一个最小可交付，独立 merge、独立验证。

## Issues 列表

### 第一批：无依赖，A/B 都受益（优先做）

| 本地 | GitHub | 标题 | 状态 | 依赖 |
|---|---|---|---|---|
| [001](001-identity-injection.md) | [#120](https://github.com/xiramesh/xira/issues/120) | 身份注入：sender/chat → prompt | [open] | 无 |
| [002](002-allowlist.md) | [#121](https://github.com/xiramesh/xira/issues/121) | 授权 allowlist（allowed_senders） | [open] | 无 |

### 第二批：owner 绑定链（依赖链，谨慎）

| 本地 | GitHub | 标题 | 状态 | 依赖 |
|---|---|---|---|---|
| [003](003-owner-data-model.md) | [#122](https://github.com/xiramesh/xira/issues/122) | owner 绑定数据模型 | [open] | 无 |
| [004](004-owner-binding-flow.md) | [#123](https://github.com/xiramesh/xira/issues/123) | owner 绑定流程（pairing-based） | [open] | #122 |
| [005](005-owner-mention-proxy.md) | [#124](https://github.com/xiramesh/xira/issues/124) | @ owner 代理 | [open] | #122 |
| [006](006-owner-only-skills.md) | [#125](https://github.com/xiramesh/xira/issues/125) | 特权 skill（owner-only） | [open] | #122 |

### 第三批：per-sender 数据隔离（独立于 owner）

| 本地 | GitHub | 标题 | 状态 | 依赖 |
|---|---|---|---|---|
| [007](007-per-sender-data-isolation.md) | [#126](https://github.com/xiramesh/xira/issues/126) | per-sender 工具数据隔离 | [open] | 无 |
| [008](008-user-md-profile.md) | [#127](https://github.com/xiramesh/xira/issues/127) | `user.md` 用户档案 | [open] | #126 |
| [009](009-memory-system.md) | [#128](https://github.com/xiramesh/xira/issues/128) / [#159](https://github.com/xiramesh/xira/issues/159) | sender / agent memory 系统 | [in-progress] | #126 |

### 已从清单移除

- **Docker per-instance 部署** —— [wontfix]，用户明确决定不做。单实例跑 B 配置是中间态允许，见架构文档 §5.3。

### deferred(不开 issue,架构文档 §6 跟踪)

- 跨 channel 同一 user(sender_id→user_id 映射)
- runtime 自动提升(per-sender→per-agent)仍禁止；显式 Agent memory scope 由 #159 跟踪
- 访问控制细化(owner admin 能力)

## 依赖图

```
001 身份注入        002 allowlist      007 per-sender 数据隔离
   │                   │                   │
   │                   │                   ├── 008 user.md
   │                   │                   └── 009 memory
   │                   │
   └───┐               │
       ↓               ↓
   003 owner 数据模型 ←──┘
       │
       ├── 004 owner 绑定流程
       ├── 005 @ owner 代理
       └── 006 特权 skill
```

两条主线:**左线 owner**(003→004/005/006),**右线数据隔离**(007→008/009)。互相独立,可并行。

## 工作纪律

- 每个 issue 独立 PR,不合并(尤其 owner 链——容易触发"修一个破一个"循环)。
- 实现前先核实代码现状(AGENTS.md §2),issue 描述里的"现状"已核实,但实现时再核一遍——代码会变。
- TDD + ≥85% 覆盖率(AGENTS.md §5.1/5.2),涉及 LLM 的用真 key(§5.3)。
