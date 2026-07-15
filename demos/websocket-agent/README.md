# Xira WebSocket 默认 Agent 演示页

这是一个零依赖静态演示页，用来从浏览器验证 Xira WebSocket channel 与 entrypoint 默认 Agent 的交互。它不需要 `npm install`、构建命令或前端开发服务器。

## 1. 启用 WebSocket entrypoint

运行实例必须有一个启用的 WebSocket entrypoint。示例：

```yaml
entrypoints:
  - id: websocket-default
    enabled: true
    channel: websocket
    default_agent: xira-assistant
    allowed_agents:
      - xira-assistant
    session:
      dimensions:
        - chat
        - sender
```

页面发送的 `message` 帧不包含 `agent_id`，因此实际使用哪个 Agent 完全由这里的 `default_agent` 决定。

## 2. 启动 Xira

在仓库根目录执行：

```bash
go run ./apps/xira/cmd/xira --config xira.yaml serve
```

默认监听地址是 `127.0.0.1:8089`，演示页默认连接：

```text
ws://127.0.0.1:8089/api/v1/channels/websocket/messages
```

## 3. 打开页面

直接在 Finder 中双击 `index.html`，或执行：

```bash
open demos/websocket-agent/index.html
```

点击“连接信道”，状态变为“链路在线”后即可发送消息。页面会分别展示：

- 中间：用户消息、Agent 最终回复、错误和 HumanRequest。
- 右侧：`ready`、`ack`、`event`、`response`、`interrupt`、`error`、`pong` 协议轨迹。
- 左侧：WebSocket URL、entrypoint、chat ID 和 sender ID。

同一 `chat_id + sender_id` 同时只能有一条活跃 WebSocket 连接。请避免重复打开多个页面；如果异常断线后立即重连收到 `chat_already_has_connection`，等待旧连接释放后再手工连接。

## 4. 运行测试

无需安装依赖：

```bash
node --test demos/websocket-agent/*.test.cjs
```

测试覆盖协议帧、默认 Agent 路由、request 关联、心跳、坏 JSON、连接生命周期和 UI 帧分流。

## 常见问题

### `503 websocket channel runner is not configured`

当前实例没有启用 `channel: websocket` 的 entrypoint，或修改配置后尚未重启 `xira serve`。

### 页面显示连接异常

确认 Xira 正在监听页面填写的地址。如果网页通过 HTTPS 提供，浏览器会要求使用 `wss://`，不能连接明文 `ws://`。

### `chat_already_has_connection`

相同 ChatKey 已被另一个浏览器页占用。关闭另一个页面，或更换 chat ID / sender ID。

### 公网使用

本页只面向本地或可信内网测试。它不会给 Xira WebSocket endpoint 增加鉴权、Origin 限制或可信用户身份。当前服务端安全边界补齐前，不要把这个入口直接暴露到公网。
