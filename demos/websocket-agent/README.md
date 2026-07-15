# Xira WebSocket 默认 Agent 演示页

这是一个零依赖静态演示页，用来从浏览器验证 Xira WebSocket channel 与 entrypoint 默认 Agent 的交互。它不需要 `npm install`、构建命令或前端开发服务器。

## 1. 启动 Demo 专用 Xira 实例

在仓库根目录执行：

```bash
export DEEPSEEK_API_KEY="$(cat DEEPSEEK_API_KEY)"
go run ./apps/xira/cmd/xira --config demos/websocket-agent/xira.yaml serve
```

目录里的 `xira.yaml` 和 `entrypoints.yaml` 是隔离的演示配置：它复用仓库的 `workspace` 和
`xira-assistant`，但只启用 `websocket-default`，不会启动现有配置里的飞书或 iLink runner。
演示运行状态写入本目录下被忽略的 `.state/`。

页面发送的 `message` 帧不包含 `agent_id`，实际使用哪个 Agent 完全由 entrypoint 的
`default_agent` 决定。

默认监听地址是 `127.0.0.1:8089`，演示页默认连接：

```text
ws://127.0.0.1:8089/api/v1/channels/websocket/messages
```

## 2. 打开页面

直接在 Finder 中双击 `index.html`，或执行：

```bash
open demos/websocket-agent/index.html
```

点击“连接信道”，状态变为“链路在线”后即可发送消息。页面会分别展示：

- 中间：用户消息、Agent 最终回复、错误和 HumanRequest。
- 右侧：`ready`、`ack`、`event`、`response`、`interrupt`、`error`、`pong` 协议轨迹。
- 左侧：WebSocket URL、entrypoint、chat ID 和 sender ID。

页面自动生成只读的浏览器身份和标签页会话：

- `sender_id` 保存在 `localStorage`，同一浏览器配置会复用，不同浏览器或无痕配置相互独立。
- `chat_id` 保存在 `sessionStorage`，同一标签页刷新后保持不变，新标签页会生成新会话。

因此多个浏览器或标签页可以同时测试，不会再默认争用同一个 ChatKey。它只是演示隔离，不是登录态或可信用户认证；清理站点数据后会生成新身份。

## 3. 运行测试

无需安装依赖：

```bash
node --test demos/websocket-agent/*.test.cjs
```

测试覆盖浏览器身份隔离、协议帧、默认 Agent 路由、request 关联、心跳、坏 JSON、连接生命周期和 UI 帧分流。

## 常见问题

### `503 websocket channel runner is not configured`

当前实例没有启用 `channel: websocket` 的 entrypoint，或启动时没有使用本目录的
`xira.yaml`。

### 页面显示连接异常

确认 Xira 正在监听页面填写的地址。如果网页通过 HTTPS 提供，浏览器会要求使用 `wss://`，不能连接明文 `ws://`。

### `chat_already_has_connection`

相同 ChatKey 已被另一条连接占用。正常情况下每个新标签页都有独立 `chat_id`；如果异常断线后立即重连，等待旧连接释放后再试。

### 公网使用

本页只面向本地或可信内网测试。它不会给 Xira WebSocket endpoint 增加鉴权、Origin 限制或可信用户身份。当前服务端安全边界补齐前，不要把这个入口直接暴露到公网。
