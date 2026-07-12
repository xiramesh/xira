# Xira Owner 绑定流程 RFC v0

- **状态**: Design
- **日期**: 2026-07-08
- **作者**: ai-daming
- **背景**: 解开 #122 owner 数据模型「静态声明 vs 用户不知 sender_id」的死结
- **关联**: #122（owner 数据模型，已实现）、#124（@ owner 代理）、#125（特权 skill）

---

## 0. TL;DR

#122 定义了 `IsOwner(senderID, entrypointID) bool` 的**查询契约**，owner 来源是
`entrypoints.yaml` 的 `owner:` 字段**静态声明**。但部署者填 `sender_id` 要预先知道它，
而用户**不知道自己的 sender_id**（只有 agent 收到消息时才从 `InboundContext.SenderID` 拿到）。
这是「先有鸡还是先有蛋」的死结。

本 RFC 提议：**纯 IM 的 owner 绑定流程**。部署者启动 agent → agent 打印一个一次性 device code
（高熵、短、好输）→ 用户在 IM 发 `/bind <code>` → agent 验证后把 sender_id 持久化为该 entrypoint
的 owner → code 作废。绑定关系落 `<stateDir>/owner-bindings.json`，重启不丢。

设计要点：
1. **device code 由 xira 生成**（crypto/rand），打印到 stdout（一次性，用完即焚）——不靠部署者
   手设、不进 env、不落日志收集系统。
2. **`/bind` 在 service 层 RunAgent 入口拦截**（session 分配之前），不进 agent turn、不进 session 历史。
3. **`IsOwner` 改为动态优先 + 静态 fallback**：先查运行时绑定，没有再查 #122 的配置 `owner:` 字段。
   向后兼容，老配置零迁移。
4. **基数 0-or-1 硬约束**（#122 继承）：`handleOwnerBind` 整段持锁，check + write + persist +
   revoke-code 原子完成。并发 `/bind` 不会写花。

---

## 1. 动机：为什么需要绑定流程

### 1.1 死结：静态声明 vs 用户不知道 sender_id

```
部署者要在 entrypoints.yaml 填：  owner.sender_id = ???
但 sender_id 是 agent 收消息时才拿到的平台内部 ID（飞书 ou_xxx、ilink wxid_xxx）
用户自己查不到 → 部署者也没法预先知道 → 配置填不了
```

类比：给某人开管理员权限，系统要填「内部身份证号」，而这个人查不到这个号。必须有一个**运行时
握手流程**：用户在 IM 里发起，agent 从消息上下文里拿到 sender_id（对用户透明），写进 ownership。

### 1.2 为什么 device code（xira 生成）而不是 env 变量

曾考虑「部署者通过环境变量 `XIRA_BIND_TOKEN_<ENTRYPOINT_ID>` 预设 claim token」，否决理由：

| 维度 | 环境变量（部署者设） | device code（xira 生成） |
|---|---|---|
| 部署者操作 | 要自己想 `openssl rand` 命令 | **零**，启动即得 |
| token 强度 | 看部署者（**会有人用 `1234`**） | **xira 保证高熵**（crypto/rand） |
| 用户输入体验 | 32 字符痛苦 | 8 字符友好 |
| 爆破风险 | 有（部署者设弱 token） | **天然消除** |
| env 名映射复杂度 | entrypoint ID 含 `-`，shell 变量名不合法，要正向映射 | **不需要** |

device code 是**一次性握手码**（不是长期凭证），打印到终端是业界标准（GitHub device flow、
`kubectl token create`、`docker login`）。它的暴露窗口只有「生成 → 首次 `/bind` 成功」几分钟到
几小时，用完即焚。「API key 不能进日志」的安全模型是针对**长期凭证**的，套到一次性握手码上是错配。

---

## 2. 现状核实（2026-07-08）

### 2.1 #122 的 owner 模型是只读的

- `Definition.OwnerID string`（`entrypoints/registry.go` 的 `Definition` struct，json/yaml tag
  `owner`）——静态配置字段。
- `Service.IsOwner(ctx, senderID, entrypointID) bool`（`runtime/service.go`）——直接读
  `Definition.OwnerID`，注释明写 `read-only after config load`。
- `Registry` 无任何 setter（只有 `Resolve`/`Definitions`/`Definition`）。
- 运行时写 ownership = **改 #122 的契约**（IsOwner 从只读改为「动态优先 + 静态 fallback」）。

### 2.2 RunAgent 入口流程 + `/bind` 拦截点

`Service.RunAgent(ctx, req TurnRequest) (TurnResponse, error)` 的前几步（按顺序）：

1. `NormalizeInboundContext(req.Context)`
2. 构建 `inbound channel.InboundEnvelope`
3. **`entrypointDecision = s.entrypoints.Resolve(...)`** ← 拿到 `entrypointID`、`Definition`
4. `channelConflict` 校验
5. 回填 `req.Context`（Channel / EntrypointID / Account / ChannelAppID / BotID）
6. `s.agents.Get(entrypointDecision.AgentID)` ← 拿 profile
7. `s.instructionTextForRun(...)` ← **会激活 skills**
8. `sessionPolicyForProfile`
9. **`s.sessions.Allocate(...)`** ← session 分配

**拦截点**：步骤 5（回填 req.Context 完成）之后、步骤 6（`agents.Get`）之前。此刻手里的信息：
- `senderID` = `req.Context.SenderID`（入参就带，`NormalizeInboundContext` 后保证非空）
- `entrypointID` = `entrypointDecision.Definition.ID`
- 消息文本 = `req.Message`

三者齐全，且尚未触发 session 分配、skill 激活、instruction 构造——拦截在这里能干净绕过这些重逻辑，
绑定消息不进 session 历史、不占 agent turn。

### 2.3 回复路径：复用现有 SendFinal，零改动

核实（`channelrunner/progress/chatkey_session.go`）：

- `ChatKeySession` 没有 `SendFinal` 方法；`SendFinal` 是 `ChatKeySessionConfig` 的字段
  （类型 `Deliverer = func(ctx, text) error`），各 channel runner 构造时注入发送闭包。
- 实际发送在 `runTurn` 末尾：`s.cfg.SendFinal(turnCtx, resp.FinalResponse)`。
- `runTurn` 调 `RunAgent` 拿 `resp, err` 后只看：`resp.FinalResponse` 非空 → 调 `SendFinal` 发出；
  为空 → 不发。**`runTurn` 不关心 RunAgent 是「正常跑完」还是「提前拦截返回」**。

→ `/bind` 拦截只要填好 `resp.FinalResponse`（如「✅ 绑定成功」）并 `return resp, nil`，现有 SendFinal
路径原样发回 IM。**不改 ChatKeySession**。

**前提**：拦截点必须在 RunAgent 的副作用（`s.sessions.AppendAgentMessages`、`s.usage.AppendCalls`、
`s.runs.SaveRun`、`recordEvent("run.finished")`、evolution candidate）**之前** return。
`/bind` 消息不进 session 历史、不记 usage、不产生 run 记录——它是元操作，不是对话。

### 2.4 stateDir 来源 + 持久化模式

- `Config.StateDir string`（`runtime/service.go`）→ `resolveRuntimeConfig` 解析 → `svc.stateDir`。
- 暴露：`Service.StateDir()` / `Service.StateRoot()`（同一目录）。
- ilink runner 的 `accounts/<id>.json` 是 stateDir 的**子目录** `channels/ilink/<entrypointID>/`——
  ilink 私有，不能放跨 channel 的 ownership。
- owner-bindings.json 应放 **Service.stateDir 根**（与 `usage-ledger.jsonl`、humanrequest store 同层），
  因为 ownership 跨 channel 共享（feishu/ilink 都要读）。
- 现有 .json 持久化模式（ilink `saveAccount`）：`MkdirAll(dir, 0o700)` +
  `json.MarshalIndent` + `WriteFile(data, 0o600)`。**无原子写先例**（直接 WriteFile，非 tmp+rename）。

### 2.5 没有现成 device code 生成机制

- 仓库无 `crypto/rand` 导入；现有随机性都是 `uuid.NewString()`（如 `delegation.go` 取前 8 hex 做短 id）。
- device code 需 `crypto/rand` + base32 编码（去易混字符 0/O/1/I），这套在仓库里要新引入。

### 2.6 没有 slash command 体系

- 现有 IM 消息都是自然语言进 agent turn，无 `/xxx` 前缀识别。
- `hitl_classify.go` 注释明确拒绝自然语言关键词匹配（「No keyword matching」）——但 `/bind` 是
  **显式 slash command（管理操作）**，不是自然语言意图理解，和 hitl_classify 的哲学不冲突。
- 唯一的结构化拦截是 `TryResolveHITL`（`hitl_preflight.go`），只在有 pending HITL 时生效。
  `/bind` 拦截是新的 channel→service 层 slash command 路径。

---

## 3. 设计

### 3.1 整体分层

```
┌─────────────────────────────────────────────────────────┐
│  层 1: 指令拦截（service 层 RunAgent 入口）              │
│  职责：识别 "/bind xxx"，命中走绑定，不命中放行进 agent │
│  产物：parseBindCommand(msg) 纯函数                      │
├─────────────────────────────────────────────────────────┤
│  层 2: 绑定逻辑（handleOwnerBind + ownerBindingStore）   │
│  职责：验 code → 防重复 → 写 ownership → 作废 code      │
│  产物：handleOwnerBind + ownerBindingStore（带 mutex）   │
├─────────────────────────────────────────────────────────┤
│  层 3: 查询改造（IsOwner 动态覆盖静态）                  │
│  职责：先查运行时绑定，没有再 fallback 配置 owner        │
│  产物：改造后的 Service.IsOwner                          │
└─────────────────────────────────────────────────────────┘
```

### 3.2 层 1：指令拦截

#### `parseBindCommand(msg)` 纯函数

```go
// 返回 token, ok。ok=false 表示不是绑定指令，放行进 agent turn。
func parseBindCommand(msg string) (token string, ok bool)
```

匹配规则：

| 规则 | 理由 |
|---|---|
| 去掉首尾空白后，必须以 `/bind ` 开头（含一个空格分隔） | 显式 slash command，不与自然语言「帮我 bind」冲突 |
| 空格分隔取第一个非空 token 作为 code | 容忍粘贴时的换行/多余空格 |
| code 为空（`/bind` 后无参）→ `ok=false`，放行 | 让无参 `/bind` 当普通对话进 agent，agent 可解释绑定用法 |
| 不校验 code 格式（长短/字符集） | 校验是 handleOwnerBind 的事；parse 只负责识别是不是绑定指令 |

例子：

```
"/bind WDJM-LHKD"    → ("WDJM-LHKD", true)
"/bind   WDJM-LHKD"  → ("WDJM-LHKD", true)   // 多空格容忍
"  /bind WDJM-LHKD\n" → ("WDJM-LHKD", true)  // 首尾空白容忍
"/bind"              → ("", false)           // 无参 → 放行
"帮我 /bind 一下"     → ("", false)          // 不以 /bind 开头 → 放行
""                   → ("", false)
```

#### 两层分工：service 层拦截 + runner pre-auth 放行

`/bind` 的处理跨两层，缺一不可（PR #142 review 闭环的 blocker 2 正是证明 service 层 alone 不够）：

- **runner 层（pre-auth 放行）**：三个 runner（feishu/ilink/websocket）的 `isAuthorizedSender` 在检查 `allowed_senders`/owner 之前，先用 `runtime.IsBindCommand(content)` 判定——命中 `/bind <code>` 就跳过 sender 授权（mention gate 仍生效）。**为什么必须**：未绑定前 `IsOwner=false`，配了非空 `allowed_senders` 的入口会把未授权 sender 的 `/bind` 在 runner 层直接丢，到不了 service 层的拦截——「最需要 owner claim 的安全入口」反而绑不上。runner 只判定「是不是 /bind 指令」，不验 token（token 验证在 service 层）。
- **service 层（拦截 + 执行）**：RunAgent 入口 `parseBindCommand` 命中后走 `handleOwnerBind`，集中一处，所有 channel 自动生效；能拿到 `entrypointDecision` 的权威 entrypointID；在 session 分配前，不污染历史。

（曾考虑的替代方案：让 agent 工具 `owner.bind` 由 LLM 调用——code 进 LLM 上下文有泄漏风险、LLM 可能不调、绑定是管理操作不该靠意图理解，否决。）

### 3.3 层 2：绑定逻辑

#### Device code 生成

```go
// generateBindCode 生成 8 字符 base32 码（去易混字符 0/O/1/I），分两组用 - 连，好念好输：
//   WDJM-LHKD
// 5 字节随机 → base32（去易混）→ 8 字符。熵 ≈ 40 bit，配合「一次性 + 已绑即废」，爆破不现实。
func generateBindCode() string
```

用 `crypto/rand`（仓库首次引入），base32 字母表去掉 `0/O/1/I`。短码 + 易读，方便用户在 IM 里输入。

#### 启动时生成 + 打印（一次性）

`NewService` 里，对每个**未绑定 owner**的 entrypoint 生成一个 code，存入 `s.bindCodes map[entrypointID]string`，
并打印到 **stdout**（不是 slog）：

```
$ ./xira serve
xira runtime listening on :8080 ...

owner 绑定（未绑定 owner 的 entrypoint）：
  feishu-default  → 在 IM 发: /bind WDJM-LHKD
  feishu-leave    → 在 IM 发: /bind KPQN-RXMT

绑定成功后此码作废。重启会生成新码（已绑定的不再生成）。
```

- 用 `fmt.Println` 打印到 **stdout**，**不进 slog** —— 不被结构化日志收集系统归档。
  这就是 GitHub device flow 的做法（显示在终端给人看，用完即焚，不是写日志）。

#### `ownerBindingStore`（带 mutex 的持久化存储）

```go
type ownerBindingStore struct {
    mu       sync.RWMutex
    dir      string  // = s.stateDir（Service 级，跨 channel 共享）
    bindings map[string]ownerBinding  // entrypointID → binding（启动时从 owner-bindings.json 加载）
}

type ownerBinding struct {
    EntrypointID      string    `json:"entrypoint_id"`
    OwnerSenderID     string    `json:"owner_sender_id"`
    OwnerSenderIDType string    `json:"owner_sender_id_type,omitempty"`
    BoundAt           time.Time `json:"bound_at"`
}
```

持久化文件 `<stateDir>/owner-bindings.json`（单文件，不是每 binding 一文件——binding 总量小）：

```json
{
  "bindings": [
    {
      "entrypoint_id": "feishu-default",
      "owner_sender_id": "ou_xxxxxx",
      "owner_sender_id_type": "open_id",
      "bound_at": "2026-07-08T10:30:00Z"
    }
  ]
}
```

- `json.MarshalIndent` + 末尾换行；目录 `MkdirAll(dir, 0o700)`；文件 `0o600`。
- 复用 ilink `saveAccount` 的模式（perm/缩进一致）。

#### `handleOwnerBind`（核心，必须原子）

```go
func (s *Service) handleOwnerBindWithIdentity(entrypointID, senderID, senderIDType, code string) (resultMsg string) {
    // ★ 整个 check + write 持同一把锁，原子完成
    s.ownerBindings.mu.Lock()
    defer s.ownerBindings.mu.Unlock()

    expected, configured := s.bindCodes[entrypointID]
    if !configured {
        return "❌ 这个入口未启用绑定。"
    }
    if subtle.ConstantTimeCompare([]byte(code), []byte(expected)) != 1 {
        return "❌ 绑定码无效。"   // 不区分「已绑定」和「码错」，防探测
    }
    if existing, bound := s.ownerBindings.bindings[entrypointID]; bound {
        if existing.OwnerSenderID == senderID {
            return "✅ 你已经是主人了。"  // 幂等：同一人重复绑，友好提示
        }
        return "❌ 该入口已有主人，无法重新绑定。"  // 防冒领
    }
    s.ownerBindings.bindings[entrypointID] = ownerBinding{
        EntrypointID:      entrypointID,
        OwnerSenderID:     senderID,
        OwnerSenderIDType: senderIDType,
        BoundAt:           time.Now().UTC(),
    }
    s.ownerBindings.persistLocked()    // 持锁持久化（写文件）
    delete(s.bindCodes, entrypointID)  // 作废 code
    return "✅ 绑定成功。你现在是 " + entrypointID + " 的主人。"
}
```

`senderIDType` 必须来自 channel 的结构化 sender identity（飞书沿用
`user_id > open_id > union_id` 的 canonical 选择），不能由显示名、普通文本或模型参数提供。
旧版 `owner-bindings.json` 没有 `owner_sender_id_type` 时仍可用于 `IsOwner` 授权，但不能用于私有投递；
同一已绑定 owner 再发送一次 `/bind ...` 时，runtime 可用当前可信事件中的 ID type 原子补齐并持久化。
补齐失败必须回滚，不能把内存态当成已迁移。

静态 owner 可通过 entrypoint 的 `owner_id_type` 声明可投递类型；只配置 `owner` 而没有 type 时同样保持
authorization-only，`notify_owner` fail closed。

#### 并发双绑防御（silent data corruption 重灾区）

场景：用户两台设备同时发 `/bind`，或 code 泄漏后冒领者和真主人同时发。两个 goroutine 同时进
`handleOwnerBind`。

不锁的后果：两个都通过 `IsBound? → false` → 两个都写 → 后写覆盖先写 → **silent data corruption**
（AGENTS.md §2 点名「不崩、只随机失效」是最贵的 bug）。

**设计要求**：`handleOwnerBind` 的 check + write + persist + revoke-code 必须**同一把锁内原子完成**。
TDD 必须有并发测试（`/bind` × N goroutine 并发 → 最终恰好 1 个成功 + N-1 个失败 + bindings 文件
状态正确）。

#### code 爆破防御

`subtle.ConstantTimeCompare` 用于 code 比较（防 timing 侧信道，即使本地 code 也别用 `==`，
习惯）。更现实的爆破面是「IM 里高频试不同 code」——这个靠 code 高熵（xira 生成保证）+ 平台限频
兜底，**代码层不内置 rate limit**（YAGNI，限频策略是 channel 的事）。

### 3.4 层 3：查询改造（IsOwner 动态覆盖静态）

#122 的 `IsOwner`（已实现，只读静态配置）：

```go
func (s *Service) IsOwner(ctx, senderID, entrypointID string) bool {
    def := s.entrypoints.Definition(entrypointID)
    return def.OwnerID == senderID
}
```

改造后（动态优先 + 静态 fallback）：

```go
func (s *Service) IsOwner(ctx, senderID, entrypointID string) bool {
    // ① 先查运行时绑定（/bind 建立的）
    if binding, ok := s.ownerBindings.Get(entrypointID); ok {
        return binding.OwnerSenderID == senderID
    }
    // ② fallback 静态配置（#122 保留，向后兼容）
    def := s.entrypoints.Definition(entrypointID)
    return def.OwnerID == senderID
}
```

为什么「动态覆盖静态」而不是「动态取代静态」：

- **动态覆盖静态**（本设计）：yaml 配了 owner 的老用户零迁移继续用；新用户用 `/bind`；两者都不改配置。
- 动态取代静态（删 yaml owner）：破坏 #122 已合并的行为，老配置失效，回归风险。

**这是契约变更，但向后兼容**。#122 的「配置静态声明 owner」继续有效（fallback 路径），`/bind` 只是
新增了一条「运行时建立」的优先路径。

### 3.5 数据流（一次成功绑定）

```
用户发 "/bind WDJM-LHKD"
    │
    ▼
RunAgent 入口（service.go）
    │
    ├─ entrypointDecision = Resolve(...)   ← entrypointID = "feishu-default"
    │                                        senderID = "ou_xxx"（req.Context.SenderID）
    ├─ 回填 req.Context
    │
    ├─ ★★ 拦截点（agents.Get 之前、session.Allocate 之前）★★
    │     parseBindCommand(req.Message) → 命中
    │     ↓
    │     handleOwnerBind("feishu-default", "ou_xxx", "WDJM-LHKD")
    │       ├─ 对比 s.bindCodes["feishu-default"]  → 匹配
    │       ├─ s.ownerBindings.IsBound?  → 否
    │       ├─ 持锁：Set + persist + revoke code
    │       └─ 返回 "✅ 绑定成功"
    │     ↓
    │     return TurnResponse{FinalResponse:"✅ 绑定成功", Status:"completed"}, nil
    │
    ▼
ChatKeySession.runTurn 拿到 FinalResponse 非空
    → s.cfg.SendFinal(ctx, "✅ 绑定成功")   ← 现有发送路径，零改动，发回飞书
```

### 3.6 owner 私有投递（#154）

owner 授权判断与私有投递解析是两份契约：

- `OwnerResolver.IsOwner` 只回答某 sender 是否为该 entrypoint 的 owner；
- `OwnerTargetResolver.ResolveOwnerTarget` 返回 entrypoint/channel/account/app/bot 路由信息，以及 typed recipient。

`notify_owner` 是 runtime-native tool。模型只能提供 `message`，不能提供 recipient。runtime 从当前
`runExecutionContext` 的权威 entrypoint 解析 owner，构造 `OutboundProactiveMessage`，并通过
`Manager.Emit` 按 `EntrypointID` 精确选择 runner。相同 channel 存在多个 entrypoint 时，禁止退化成
“取第一个 runner”；缺少 entrypoint 的 channel-only fallback 只有在候选唯一时才合法。

typed recipient 私信还要求最终 runner 声明 `typed_recipient_outbound`。这个检查发生在精确选出
runner 之后，不能用 Manager 的 fleet capability 并集代替。Feishu/iLink 支持；WebSocket 只保留
无 recipient 的 proactive resume final，客户端自报 `sender_id_type` 不进入 owner binding，因此
WebSocket owner binding 是 authorization-only，`notify_owner` fail closed。

通知成功后允许 final 为空，表示群里有意静默；这个例外只对 `notify_owner` 返回 `status=sent` 的 run
成立。同一 run 最多一次成功通知；投递失败可以重试。聚合时只要有一次 `notify_owner` 成功即允许
intentional silence：同工具之前或之后的失败/拒绝不撤销已经发生的投递；任何其他工具失败仍不允许。
只有失败、从未成功，或普通空 final 仍按失败处理。
`notify_owner` 只在生产 ADK 路径注册，未被生产 dispatch 的 legacy native generator 不广告该工具。
owner 私聊回复与跨 chatKey HITL resume 不在本阶段，见 #155。

---

## 4. 生命周期

### 启动时（NewService）

```
1. 加载 owner-bindings.json → ownerBindings.bindings（内存缓存）
2. 对每个 entrypoint：
     - 若已绑定（在 ownerBindings 里）→ 不生成 code
     - 若未绑定 → generateBindCode() → bindCodes[entrypointID] = code
3. 打印未绑定 entrypoint 的 code 到 stdout
```

### 重启后

| 东西 | 重启后状态 | 理由 |
|---|---|---|
| `ownerBindings`（绑定关系） | **保留**（从 owner-bindings.json 加载） | 绑定是长期事实，IsOwner 必须持续 true |
| `bindCodes`（待用 code） | **重新生成**（旧的失效） | code 本就不持久化（一次性消费）。重启前没绑完的，重新复制新 code |

→ 重启**不丢绑定**。已绑定的实例重启后，该 entrypoint 不再生成 code（不会出现在绑定码列表里），
`/bind` 也进不了绑定分支（`IsBound? → true` 直接拦）。重启只影响「code 已给用户但用户还没 `/bind`
就重启了」——后果是重新复制一次新 code，和 GitHub device code 过期重新申请一样。

### 解绑（不做）

owner 绑定锁死。要换 owner：停 agent → 删 `owner-bindings.json` 里对应条目 → 重启 → 重新 `/bind`。
动态解绑/换主是 follow-up，不在本 RFC。

---

## 5. 踩坑与约束

### 5.1 `Status` 复用 `"completed"`（拦截点保证不触发 `assistant.final`）

`/bind` 拦截返回的 `TurnResponse` 复用 `Status: "completed"` + 非空 `FinalResponse`。

`assistant.final` 的发布点在 service.go 的 RunAgent **末尾**（副作用区，条件
`final != "" && resp.Status == "completed"`，AGENTS.md §1.2）。`/bind` 拦截在 RunAgent
**开头**（entrypointDecision 之后、agents.Get 之前）提前 `return`，**根本到不了**
`assistant.final` 发布点——所以即使 Status 是 `"completed"` 也不会误触发。

`chatkey_session.runTurn` 的 `SendFinal` 只看 `FinalResponse != ""`，不看 Status（核实
`chatkey_session.go`）——所以「绑定成功」提示会被正常发回 IM。

（曾考虑用非 `"completed"` 的 Status 值，但那会给未来新增的 Status 消费者埋雷——event_mapping 的
mapRunFinished 虽有 default 分支，复用现值更安全、更简单。）

### 5.2 拦截点必须在所有副作用之前 return

`/bind` 拦截必须在 RunAgent 的 `s.sessions.AppendAgentMessages`、`s.usage.AppendCalls`、
`s.runs.SaveRun`、`recordEvent("run.finished")`、evolution candidate 之前 return。绑定消息**不能**
进 session 历史、不能记 usage、不能产生 run 记录——它是元操作。

### 5.3 并发原子性（silent data corruption 重灾区）

`handleOwnerBind` 的 check + write + persist + revoke-code 必须同一把锁内原子完成。TDD 必须有
并发测试（`/bind` × N goroutine → 恰好 1 成功 + N-1 失败 + 文件状态正确）。

### 5.4 code 比较用 `subtle.ConstantTimeCompare`

防 timing 侧信道。即使本地 code 也别用 `==`。

---

## 6. 验证

### TDD（先写失败测试）

| 测试 | 验证什么 |
|---|---|
| `parseBindCommand`：正常/多空格/首尾空白/无参/非 `/bind` 开头/空串 | 指令识别边界 |
| `generateBindCode`：长度 8、字符集（去易混）、两次调用不同 | code 生成正确 |
| `ownerBindingStore`：Set/Get round-trip + persist 后 reload 状态一致 | 持久化 round-trip |
| `handleOwnerBind` happy path：对 code + 未绑 → 成功 + code 作废 | 核心路径 |
| `handleOwnerBind` 错误 code：失败，不写、code 不作废（可重试） | code 失败不消耗 code |
| `handleOwnerBind` 重复绑定（同一 sender）：幂等成功提示 | 幂等 |
| `handleOwnerBind` 冒领（已绑别人）：拒绝 | 防冒领 |
| `handleOwnerBind` **并发**：N goroutine 同时 `/bind` → 恰好 1 成功 + N-1 失败 + 文件正确 | silent corruption 防御 |
| `RunAgent` 拦截：发 `/bind xxx` → 不进 agent turn（无 session/usage/run 记录）+ FinalResponse 非空 | 拦截点正确 + 无副作用 |
| `IsOwner` 动态覆盖：动态命中用动态；无动态 fallback 静态 | 契约变更正确 |

### 覆盖率（AGENTS.md §5.2 分级）

| 函数 | 档 | 目标 |
|---|---|---|
| `IsOwner`、`handleOwnerBind`、`parseBindCommand`、`ownerBindingStore.Get/Set`、`generateBindCode` | **契约代码** | **100%**（标 `// coverage: contract (100% required)`） |
| `ownerBindingStore.persistLocked/load`、绑定码打印 | 核心逻辑 | 85% |
| getter 类 | helper | 不强制 |

### live 测试（AGENTS.md §5.3 双门控）

飞书实测：启动看 code → `/bind` → 看回复「绑定成功」→ 再发普通消息验证 agent 行为体现「认识主人」。
合并前 `task live-test` 跑，确认无 SKIP 行。

---

## 7. 不做什么（防 scope 蔓延）

- ❌ 不做「解绑 / 换 owner」（锁死，要换：停 + 删文件 + 重启 + 重新 `/bind`）
- ❌ 不做多 owner（N=1 硬约束，#122）
- ❌ 不做 CLI approve（纯 IM）
- ❌ 不做 code 爆破 rate limit（YAGNI，靠 code 高熵 + 平台限频）
- ❌ 不改 ChatKeySession（复用现有 SendFinal 出口；runner 的 pre-auth 放行见 §3.2，是配合而非 ChatKeySession 改动）
- ❌ 绑定消息不进 session 历史（元操作，不是对话）
- ❌ 不持久化 device code（一次性，启动生成，用完即焚）

---

## 8. 对 #122 契约的影响

| #122 的契约 | 本 RFC 后 |
|---|---|
| `Definition.OwnerID` 静态配置字段 | **保留**（fallback 路径） |
| `IsOwner` 读 `Definition.OwnerID` | **改为**：先查动态绑定，fallback 静态 |
| 基数 0-or-1 | **保留**（`handleOwnerBind` 持锁强制） |
| Registry 无 setter | **不变**（动态绑定另立 `ownerBindingStore`，不走 Registry） |

向后兼容：yaml 配了 owner 的老用户零迁移继续用；新用户用 `/bind`。

---

Refs #122 #123 #124 #125
