# Feishu 原始 WebSocket 事件诊断（0.8.x）

本文是 issue #210 的短时线上采集与回滚手册。原始事件包含聊天正文、用户标识和可能的事件 token；不得提交 Git、粘贴到公开 issue，或复制进普通应用日志。

## 采集契约

- 只在 Feishu entrypoint 显式配置 `raw_event_diagnostics.enabled: true` 时启用；默认不创建目录或文件。
- `raw_event_diagnostics` 当前只支持 Feishu；在其他 channel 上启用会导致配置加载失败，不会静默忽略。
- 对该 entrypoint 收到的所有 `im.message.receive_v1` 采集完整 `EventReq.Body`，发生在 mention、sender allowlist、dedupe 和 typed event 校验之前，不支持按 `chat_id` 过滤。
- 一个部署有多个 Feishu entrypoint 时，需要在每个待诊断的 entrypoint 上显式开启；每个 entrypoint 的容量和文件互相隔离。
- JSONL 位于 `<state_dir>/channels/feishu/<entrypoint-id>/raw-event-diagnostics/`。目录权限强制为 `0700`，文件权限强制为 `0600`。
- `max_bytes` 是该目录下所有诊断 JSONL 的总容量。进程重启不会重置计数；到限后跳过新记录并写 WARN，不影响原消息 handler。
- `retention_hours` 按文件采集起始时间计算；运行期间由后台清理器（最长约一分钟调度延迟）以及每次新事件采集共同清除到期文件，同时写 WARN。关闭诊断后不会再运行清理，因此验证结束必须人工清理遗留文件。
- 每行包含 `captured_at`、`entrypoint_id`、`chat_id` 和原始 JSON 对象 `payload`。普通日志只记录启用、容量/保留到限和写入失败，不记录 payload。

## 启用配置

在实际 `entrypoints.yaml` 的 Feishu entrypoint 下加入：

```yaml
entrypoints:
  - id: feishu-main
    channel: feishu
    # 其余现有字段保持不变
    raw_event_diagnostics:
      enabled: true
      max_bytes: 104857600 # 示例：100 MiB
      retention_hours: 24
```

启用时 `max_bytes` 和 `retention_hours` 必须都大于零；缺任意一个会拒绝启动。这是故意的安全边界，不提供无上限默认值。

## daming-ubuntu 升级与验证

以下路径和服务名必须替换为机器上的真实值；先从现有进程或 service unit 核实，不能凭猜测覆盖。

1. 在已通过全仓验证的 commit 上构建，并记录版本：

   ```bash
   git rev-parse HEAD
   task build:prod
   ./bin/xira version
   ```

2. 在目标机备份当前 0.7.0 二进制，记录当前启动命令、环境、配置路径及 binary 校验和。只替换二进制；不覆盖 `xira.yaml`、workspace、entrypoints 配置或 `state_dir`。

3. 先把上面的诊断配置加入真实 Feishu entrypoint，再短暂停止旧进程、替换为已验证的 0.8.x 二进制，并沿用原环境和参数启动。启动前的 `xira version` 输出和启动日志必须共同证明：

   - `xira 0.8.0 commit=<目标 SHA>`；
   - Feishu runner 已启动且长连接正常；
   - `feishu raw event diagnostics enabled` WARN，字段中的 entrypoint、目录、容量和保留期正确。

4. 发送一条受控消息后核权限：

   ```bash
   stat -c '%a %n' <diagnostic-directory>
   stat -c '%a %n' <diagnostic-directory>/im-message-receive-v1-*.jsonl
   ```

   目录必须是 `700`，所有 JSONL 必须是 `600`。同时确认普通收发仍正常，且 `xira.log`/journal 中没有 payload 或聊天正文副本。

5. 在不同群和单聊发送可识别但不含真实敏感业务内容的测试消息。至少覆盖：

   - 两个不同 `chat_id` 的群；
   - 单聊；
   - 群内 @ bot 与不 @ bot；
   - 调查需要的不同 sender / 消息形态。

   不 @、未授权或后续被 dedupe 的 receive 事件也应有原始记录，因为采集发生在业务 gate 之前。

6. 只在目标机本地检查 wire shape。不要 `cat` 整个文件到共享终端记录；可先用 `jq` 只列字段结构：

   ```bash
   jq -c '{captured_at,entrypoint_id,sender_keys:(.payload.event.sender|keys),sender_has_name:(.payload.event.sender|has("name")),message_keys:(.payload.event.message|keys)}' <diagnostic-file>
   ```

   最终回填 #133 时只写脱敏结论，例如 `event.sender.name present/absent`、字段出现条件和消息形态；不得附原始行、token、正文、chat_id 或 sender_id。

## 关闭、无新增确认与清理

1. 把 `raw_event_diagnostics.enabled` 改为 `false`（或删除整个区块）并重启同一 0.8.x 二进制。
2. 记录全部诊断文件的大小和修改时间，发送一条新的受控 Feishu 消息，再次核对；文件不得新增或增长，日志不得再出现 enabled WARN。
3. 完成脱敏结论后，删除明确核实过的 `<state_dir>/channels/feishu/<entrypoint-id>/raw-event-diagnostics/`。不要对未解析变量、workspace 根目录或 `state_dir` 根目录执行递归删除。

## 回滚

若 0.8.x 的版本、长连接或普通收发验证失败：停止新进程，恢复已备份的 0.7.0 二进制及原启动参数，然后确认 Feishu 长连接和收发恢复。配置、workspace 和 state 全程不应被二进制替换；诊断文件按上节单独清理。
