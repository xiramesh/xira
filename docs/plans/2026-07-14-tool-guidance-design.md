# Xira per-tool Guidance 设计

## 决策

每个模型可见工具对应零段或一段自包含 Guidance。`Description` 继续说明工具做什么、输入输出和调用格式；可选的 `Guidance` 说明模型何时应主动想到该工具、何时不应使用，以及成功与失败分别意味着什么。Guidance 只描述所属工具，不引用其他可能未开放的工具。

普通 builtin tool 通过可选 `GuidanceProvider` 暴露 Guidance；没有实现该接口就不会产生额外提示。runtime-owned tool 使用同样的一工具一 Guidance 元数据。系统只从本轮经过 profile permissions、runtime native-tools 开关、delegation policy 和 per-run allowlist 过滤后的最终有效工具集中收集 Guidance。未出现在最终工具集中的工具不得在 system instruction 中出现。

## 首批范围

需要 Guidance：`command.run`、`shell.run`、`tool_output.read`、`update_profile`、`update_memory`、`forget_memory`、`human.request`、`notify_owner`、`finish_silent`、`human.interpret`、`emit_status`、`spawn_turn`、`poll_turn`、`answer_child`。

不需要 Guidance：`read_file`、`search_file`、`write_file`、`list_dir`、`edit_file`。这些工具的选择和行为可由局部 description 完整表达。

现有 builtin profile 中针对 `command.run`、`shell.run` 和 `tool_output.read` 的手写提示迁入对应工具 Guidance，避免自定义 Agent 丢失相同操作知识。

## 数据流与验证

runtime 先解析本轮 effective tool names，再按稳定顺序收集非空 Guidance，渲染为独立的 `# Tool Guidance` system-instruction 区块。ADK、child turn 和 HITL resume 共用同一条 instruction 组装路径；native DeepSeek 兼容路径也使用同一编译器。

测试覆盖：零 Guidance 工具不产生区块；任意单工具 allowlist 只注入自身 Guidance；native-tools disabled 时不泄漏 runtime Guidance；delegation disabled 时不出现 child-turn Guidance；Guidance 顺序稳定且去重；每段文本不引用其他 Xira 工具名；effective-name 解析与实际 ADK tool set 保持一致。真实 DeepSeek live test 验证用户明确要求跨会话记住稳定事实时调用 `update_memory`，而临时任务状态不机械写入。
