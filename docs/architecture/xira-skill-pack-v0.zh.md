# Xira Skill Pack v0

> 状态：Phase 1 runtime 设计与实现格式。
>
> 结论：Xira Skill Pack 是 agent / flow 内部复用的能力单元，不是客户交付边界，也不直接等同于 ADK Skill。

## 定位

Xira Skill Pack 负责把可复用的工作方法、约束、参考资料和能力依赖声明出来。它可以被 `agent profile` 或未来的 `flow pack` 引用，然后由 runtime 编译进最终 agent instructions。

它不负责：

- 启动 MCP server。
- 注入 secret。
- 覆盖 agent 身份。
- 作为客户交付验收边界。

客户交付边界仍然是 flow pack。Skill 只是 flow / agent 内部的复用单元。

## 目录结构

```text
workspace/skills/<skill-id>/
  SKILL.md
  references/        # 可选，长说明、规则、案例
  templates/         # 可选，输出模板
  scripts/           # 可选，供 command.run 调用的本地脚本
  assets/            # 可选，静态资料
```

`SKILL.md` 文件名大小写不敏感，`skill.md`、`Skill.md` 可以被加载。规范写法仍然是 `SKILL.md`。如果同一目录下同时存在多个大小写变体，runtime 必须报错，避免 macOS 与 Linux 行为不一致。

同样规则适用于 agent 的 `PROFILE.md` 和 `SOUL.md`。

## SKILL.md 格式

`SKILL.md` 使用 YAML frontmatter + Markdown body：

```markdown
---
schema_version: xira.skill.v0
id: local-research
name: Local Research
version: 0.1.0
description: Use local workspace files and command evidence to produce source-backed answers.

activation:
  mode: explicit

requires:
  tools:
    - search_file
    - read_file
  optional_tools:
    - command.run
    - tool_output.read
  secrets: []
  mcp_servers: []

context:
  includes:
    - references/
  forbidden:
    - secrets/

verification:
  default_checks:
    - final_response_non_empty

artifacts:
  output_dir: artifacts/skills/local-research
  retention: local
---

# Instructions

Use this skill when the task requires source-backed local research.

Rules:

- Prefer `search_file` before `read_file` when the target file is unknown.
- Cite workspace paths when making claims from local files.
- If command output is truncated, use `tool_output.read` before relying on missing output.
- Do not claim evidence exists unless it came from a tool result or an explicitly loaded reference.
```

## 字段语义

`schema_version`：必须为 `xira.skill.v0`。

`id`：必须和目录名一致。

`activation.mode`：v0 只支持 `explicit`。Skill 必须被 agent profile 默认引用、future flow pack 默认引用，或后续 runtime `skill.activate` 显式激活，不做关键词自动触发。

`requires.tools`：skill 必需工具。runtime 激活 skill 时会校验这些工具是否存在，且是否在当前 agent profile `tools` / `permissions.tools` 内。

`requires.optional_tools`：有则可用，没有也不阻止启动。

`requires.secrets`：skill 需要的 secret 名称。runtime 激活 skill 时必须被当前 agent profile `permissions.secrets` 覆盖；skill 不会自动获得 secret。

`requires.mcp_servers`：skill 需要的 MCP server 名称。runtime 激活 skill 时必须被当前 agent profile `mcp_servers` 覆盖；skill 不会自动启动 MCP server。

`context.includes`：skill 自带参考资料路径。路径必须留在 skill 目录内，且 v0 要求可读。

`context.forbidden`：skill 明确不应访问的相对路径。路径必须留在 skill 目录内。

`verification.default_checks`：skill 推荐检查项。v0 不覆盖 agent profile，只作为后续 verification 合并基础。

`artifacts`：skill 推荐产物目录和保留策略。

Markdown body：真正编译进 agent instructions 的可复用工作规则。

## Agent 默认激活方式

Agent profile 使用现有 `skills` 字段声明默认激活列表：

```yaml
skills:
  - local-research
```

`profile.skills` 不是 allow-list，也不表示“这个 agent 只能使用这些 skill”。它只表示该 profile 的 run 默认尝试激活哪些 skill。

runtime 编译顺序：

```text
PROFILE.md body
SOUL.md
default active skill instructions, in profile.skills order
Runtime Identity
Runtime Capabilities
```

每个 skill 编译时会包一层边界：

```text
# Loaded Skill: local-research v0.1.0

This skill is subordinate to the current agent profile. Use it only when relevant to the user task.

<SKILL.md Markdown body>
```

这样 skill 不会变成另一个 agent，也不能覆盖当前 agent 身份。

## Runtime 生成 Skill

runtime 生成的 skill 和 workspace 静态 skill 进入同一个 Skill Registry。是否能使用，不由 skill 自己决定，也不由第二套 `skill_policy` 决定，而是在激活时使用当前 agent profile 的同一套权限边界判断：

```text
SkillRegistry = runtime 知道有哪些 skill
ActiveSkills  = 当前 run / session 已激活哪些 skill
Profile       = 当前 agent 的唯一权限上限
```

统一激活流程：

```text
skill.activate(id)
  -> skill 是否存在
  -> SKILL.md / metadata 是否合法
  -> context 路径是否合法
  -> requires.tools 是否被 profile.permissions.tools 覆盖
  -> requires.secrets 是否被 profile.permissions.secrets 覆盖
  -> requires.mcp_servers 是否被 profile.mcp_servers 覆盖
  -> 通过后注入当前 run / session context
```

Phase 1 已实现 `profile.skills` 的默认激活路径；后续显式 `skill.search` / `skill.activate` runtime tool 必须复用同一条激活校验逻辑。

## v0 校验规则

- `workspace/skills/<id>/SKILL.md` 必须存在，文件名大小写不敏感。
- `schema_version`、`id`、`name`、`version`、`description` 必填。
- `id` 必须等于目录名。
- Markdown body 必须非空。
- `activation.mode` 必须是 `explicit`。
- `context.includes` 和 `context.forbidden` 必须是 skill 目录内的相对路径。
- `context.includes` 必须可读。
- `profile.skills` 默认激活不存在的 skill 时，当前 run 失败。
- skill `requires.tools` 中的工具必须存在，且激活时必须被当前 profile 允许。
- skill `requires.secrets` 和 `requires.mcp_servers` 必须在激活时被当前 profile 覆盖。
- 多个 skill 重复激活同一个 id 时去重，并保持第一次出现的顺序。

## 后续扩展

MCP 不在 Skill v0 中直接启动。MCP 就是一等 MCP 能力。Skill 只声明 `requires.mcp_servers`，runtime 根据当前 profile 的 MCP 配置决定是否可以激活。

CLI / shell 能力也不封装成新的统一类型：临时探索走 `command.run` / `shell.run`，稳定流程沉淀到 skill instructions、flow step、command recipe 或具体 tool。
