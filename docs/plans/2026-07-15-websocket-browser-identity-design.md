# WebSocket Demo 浏览器身份隔离设计

## 目标与选择

公网静态演示页目前把 `chat_id=browser-demo`、`sender_id=browser-user` 写死，所有浏览器会形成同一个 ChatKey，既会触发单连接冲突，也会复用同一会话历史。本次把身份边界收缩为“浏览器演示身份”，不把它包装成真实用户认证。

采用客户端存储方案：`sender_id` 首次访问时生成 UUID，写入 `localStorage`，同一浏览器 profile 后续访问保持稳定；`chat_id` 首次打开标签页时生成 UUID，写入 `sessionStorage`，同一标签页刷新保持稳定，新标签页形成独立会话。两项在页面上只读展示。WebSocket URL 与 entrypoint 仍是所有用户共享的接入配置。

未采用两个替代方案：每次刷新都随机会让刷新后的同一会话丢失；从 Basic Auth 用户名派生身份需要 Nginx 把已认证身份可信传给 Xira，而当前 WebSocket 协议没有该服务端契约。后者才是正式多用户认证的正确方向，但超出本次静态演示页范围。

## 数据流与失败处理

页面挂载时先解析两个存储：

1. 从 `localStorage` 读取 `xira.websocket-demo.sender-id.v1`；不存在时生成 `browser-<uuid>` 并保存。
2. 从 `sessionStorage` 读取 `xira.websocket-demo.chat-id.v1`；不存在时生成 `chat-<uuid>` 并保存。
3. 将结果写入只读的 Sender ID 与 Chat ID 输入框。发送协议保持不变，消息帧仍不包含 `agent_id`。

浏览器禁用存储、隐私模式抛异常或存储写满时，页面不能因此失效：本次页面生命周期内仍生成并使用随机 ID，但无法承诺刷新后稳定。UUID 优先使用 `crypto.randomUUID()`；不可用时退化为时间戳加随机串。共享 Basic Auth 仅保护入口，不参与 Sender 身份计算。

## 测试与验收

身份生成属于会话隔离契约，测试必须覆盖：已有值复用、首次生成并持久化、空白旧值重建、读失败、写失败、无存储对象、UUID 主路径与 fallback。DOM 行为测试验证两个字段自动填充且只读，连接时使用生成后的值。

部署后使用两个独立浏览器上下文验收：二者 Sender ID、Chat ID 均不同；各自能同时建立 WSS，不再出现 `chat_already_has_connection`；刷新同一上下文后 Sender ID 保持稳定。最终仍以 Xira 日志中的不同 ChatKey 与两个成功连接为准。
