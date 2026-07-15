# WebSocket 默认 Agent 静态演示页设计

## 目标

在 `demos/websocket-agent/` 提供一个无需安装依赖、无需启动前端开发服务器、可由浏览器直接打开的静态页面，用来验证 Xira WebSocket channel 与 entrypoint 默认 Agent 的完整交互。页面只负责演示和诊断，不作为公网产品入口，也不改变 Xira API server 的路由或鉴权边界。

## 交互与视觉

页面采用深色“信号台 / 运行记录仪”风格：左侧是连接参数与连接状态，中间是用户和 Agent 的对话记录，右侧是按时间滚动的协议事件轨迹。视觉重点是连接状态、当前 request、run 进度和最终结果，不模仿通用聊天产品。

连接区允许编辑 WebSocket URL、entrypoint ID、chat ID 和 sender ID。默认 URL 指向 `ws://127.0.0.1:8089/api/v1/channels/websocket/messages`，默认 entrypoint 是 `websocket-default`。页面不提供 Agent 选择器，发送 `message` 帧时也不写 `agent_id`，从而明确验证 entrypoint 的默认 Agent 路由。

## 协议与状态

页面处理 `ready`、`ack`、`event`、`response`、`interrupt`、`error` 和 `pong`。发送消息后先展示本地用户消息；`ack` 更新请求状态；`event` 进入右侧轨迹；`response.data.final_response` 作为 Agent 最终回复；`interrupt` 展示待人工处理内容和结构化操作；`error` 保留服务端错误码与可重试信息。

一个浏览器页只维护一条连接。页面不自动重连，避免旧连接未释放时撞上 WebSocket 单 ChatKey owner 契约；断线后由用户显式重新连接。连接期间每 20 秒发送一次 `ping`。所有服务端文本都通过 `textContent` 渲染，不把模型输出作为 HTML 注入。

## 文件结构与测试

- `index.html`：静态页面语义结构。
- `styles.css`、`console.css`：无外部资源的基础视觉、交互组件与响应式布局。
- `protocol.js`：UMD 风格的纯协议/client 模块，浏览器和 Node 测试共用。
- `app.js`：DOM 绑定和交互控制。
- `protocol.test.cjs`：Node 内置 `node:test` 单元测试。
- `README.md`：配置、启动、打开和排障说明。

测试从协议行为开始：URL 规范化、hello/message/ping 帧、默认 Agent 字段缺席、request 关联、坏 JSON、WebSocket 生命周期和心跳。实现后用浏览器直接打开 `index.html`，检查桌面与窄屏布局、连接失败状态和交互可读性。
