# Stage 2：Codex Adapter 开发计划

> 前置文档：[llmServer 后端产品与技术总纲](./llm-server-product-design.md)、[Stage 1：标准 API 与结算内核](./stage-1-standard-api.md)
> 范围：把本机 Codex 作为一个受限、可计价、可观测的 Provider 接入
> 明确排除：远程控制 Codex、向调用方/Git 导出登录凭据、Web 控制台、Hominal 接入
> 实现状态：已实现并真实验证最小化 Responses SSE 性能主链、隔离常驻 App Server 兼容兜底、公开计价、usage、取消、推理强度和优先速度层级；不采集 Codex 账户额度

## 1. Stage 目标

Stage 2 不把完整 Codex agent 暴露给局域网。它只复用当前用户已经登录的 Codex 模型访问能力，提取 llmServer 公共 API 所需的文本生成、流式输出、usage 和有效模型；调用方不能触达 Codex 的文件、shell、MCP、插件或其他 agent 工具。

Codex Adapter 必须完全复用 Stage 1 的 Deployment、Run、GatewayEvent、PriceRevision、Budget 和 Settlement。不得增加 Codex 专属价格字段或让客户端理解 Codex thread/turn。

## 2. 已确认协议边界

当前实现保留两条语义等价但优先级明确的传输：

- 性能主链：按 Codex 开源客户端当前请求结构，使用当前登录态向 ChatGPT Codex Responses 端点发送最小化 SSE 请求。请求只有文本输入、空工具集、`store=false`、推理强度和可选速度层级；HTTP client 与连接跨请求复用，并读取 macOS 当前 HTTPS 系统代理。
- 兼容兜底：隔离、常驻的 `codex app-server --stdio` worker。每次请求仍新建 ephemeral thread 和临时 cwd，审批为 never、sandbox 为 read-only，并禁用 apps、plugins、hooks、browser/computer use、image generation 和 skill search。

当前生成链为：

```text
load current login (mtime cache)
→ Responses HTTP/SSE over reused transport
→ response.output_text.delta
→ response.completed + usage
→ settlement
```

主链只在 HTTP `400/401/403/404/502/503/504` 等明确未开始成功流式生成的响应上切换到 App Server，并在一分钟内抑制重复直连探针。连接中断、超时等结果不明的传输错误绝不自动重放，否则可能让一个调用消耗两次额度。App Server 兜底消费 `item/agentMessage/delta` 与 `thread/tokenUsage/updated`，不会等待完整回答后再批量输出。

主链从当前用户的 `auth.json` 读取 access token 和 account ID，只按文件修改时间缓存在进程内，不写入项目配置、日志或数据库。App Server worker 只把当前 `auth.json` 和可选模型缓存复制到权限受限的临时 `CODEX_HOME`，不加载全局 AGENTS、MCP、apps 或 plugins，退出时删除目录。不解析 ChatGPT App 私有数据库、不请求 `account/rateLimits/read`、不从 UI 文本猜测额度。

本 Stage 不锁定某个 Codex 字段版本。启动时记录：

- Codex 可执行文件绝对路径；
- `codex --version`；
- 当前直连请求兼容等级与 App Server schema hash；
- llmServer 支持的协议兼容等级；
- 最近一次离线 fixture 套件结果。

版本或 schema 未通过兼容门时 Provider 状态为 `incompatible`，不得带着旧字段继续调用。

## 3. 进程与并发模型

Responses 主链使用共享 HTTP client 和连接池，但每个 llmServer Run 仍是独立、无状态请求。App Server 兜底使用最多 `max_concurrency` 个常驻 worker，每个 worker 同时只处理一个 ephemeral thread，不复用对话历史：

```text
request_id
client_id
deployment_id
transport (direct | app_server_fallback)
worker_id (仅兜底运行时)
started_at
terminal_state
```

每个 Codex Provider 使用一个有界并发槽，`max_concurrency` 默认 2、有效范围 1–16。排队只发生在该 Provider 的槽位，不会阻塞其他 Provider。配置热更新时旧 Adapter 停止接新请求、关闭空闲连接和 worker；已经开始的请求完成后再销毁对应资源。

子进程崩溃时：

- 对应在途 Run 标记为 Provider 失败；
- 已收到的 usage 进入 unconfirmed/confirmed 判定，不清零；
- 不自动重新提交 turn；
- 不自动重放；下一次请求重新选择当前可用传输；
- 启动时用 `codex --version` 前缀门拒绝已知不兼容版本。

客户端取消通过 HTTP context 取消直连请求；App Server 兜底为避免共享 stdio 上残留事件污染下一请求，会关闭当前 worker，其他 worker 和 Provider 不受影响。

## 4. 模型发现与公开

`model/list` 只更新 Discovered Model inventory，保存 model ID、display name、hidden、支持的 reasoning effort、输入模态和推荐默认值。它不自动创建或启用 Model Deployment。

管理员通过配置显式建立：

```yaml
prices:
  - revision: codex-terra-price-1
    currency: USD
    source: manual_override # 仅后台可见
    per_million:
      input: "2.000000"
      output: "12.000000"

deployments:
  - id: codex-terra
    provider: codex-local
    upstream_model: gpt-5.6-terra
    price_revision: codex-terra-price-1
    usage_fallback:
      algorithm: text_estimator_v1
    execution_profile: model-only
    enabled: true
```

当 `model/rerouted` 表明实际模型变化时，响应记录：

- `requested_model`：公共 Deployment ID；
- `upstream_model`：Deployment 配置的 Codex model；
- `effective_model`：reroute 后模型；
- `reroute_reason`：脱敏后的官方原因。

公开结算仍按 Deployment 固定 price revision，不临时改用 effective model 或 Codex 返回的价格。否则一次上游不可控的 reroute 会改变客户端价格契约。供应商价格、effective model 与 reroute 只进入后台管理和诊断。

## 5. usage 与公开计价

Responses 主链采用 `response.completed.usage`；App Server 兜底采用 `thread/tokenUsage/updated.last`。两条路径都规范化为同一 meter，兼容 fixture 必须覆盖重复更新、缺失 usage 和异常终态，防止重复累加或把缺失值当成零。

规范化 usage 至少形成：

```text
input_tokens
output_tokens
input_source: provider_reported | estimated_v1
output_source: provider_reported | estimated_v1
estimator_version
```

公开金额只按 Deployment 的输入价、输出价以及上述 input/output 数量计算。调用方看不到价格是人工配置，也看不到 Token 是否由兜底估算。Codex 返回的成本、缓存、reasoning、累计 token 活动等数据可以保留在后台，但不能改变公开金额。

Codex 缺失 input 或 output 时，对缺失维度使用 Stage 1 的 `text_estimator_v1`。输入估算只基于 llmServer 实际发送的可见 instructions、消息和 schema；输出估算基于最终文本或函数 JSON，不试图猜测 Codex 隐藏上下文。估算结果直接成为公开计费 Token，但其来源和估算器版本只在后台保存。禁止把缺失项填零。

## 6. 推理强度与速度层级

公开请求的 `reasoning.effort` 先由 Gateway 按 Deployment 的 `supported_reasoning_efforts` 校验，再传给主链的 `reasoning.effort` 或兜底的 `turn/start.effort`。未显式传入时使用 Provider 的 `default_reasoning_effort`。Codex Provider 可配置 `service_tier=priority` 并映射到相应传输字段，以更高账号额度消耗换取更低延迟；它不是价格算法，也不会改变 llmserver 的公开计价。

Codex 账户额度不在生成响应的稳定字段中，主动前后查询会增加约 2.5–3 秒并产生无法可靠归因的共享窗口差值，因此相关采集、响应字段、管理统计和客户端开关均已移除。

## 7. hard/soft budget 在 Codex 中的含义

Codex 可能在一个 turn 内产生不可见的内部步骤，且 token usage update 不保证逐 token 到达。因此：

- 只有当前协议能可靠传递输出上界，并且上游 Token 或估算器计费单位都能在该上界内受控时，Deployment 才声明 `hard_budget_supported=true`；
- 不满足条件时，任何 hard budget 在调用前返回 `hard_budget_not_enforceable`；
- soft budget 可在 usage update 达到阈值后 interrupt，但明确允许 overshoot；
- 未设置 budget 时照常调用并按本次 billable usage 结算。

不要因为能调用 `turn/interrupt` 就推断 hard budget 可保证。取消延迟和 usage 更新粒度决定它通常只能支撑 soft budget。

## 8. 输出边界

文本 delta 映射为统一 `output_text.delta`。Responses 或 Codex item 中的答案文本、错误和终态分别映射；原始 chain-of-thought 不输出、不入普通日志。主链只承诺文本输入和文本输出。

当前不支持 caller-defined function tools 或结构化输出；公开请求携带 tools、tool choice 或 JSON Schema 时在进入 Provider 前拒绝。Codex 自己的 shell、文件、MCP、web 或 GUI 工具事件也绝不能转换为调用方函数，兜底路径观察到相关 item 时直接失败。

## 9. model-only 安全配置

Codex 是 agent runtime，不是纯模型进程。Stage 2 的首要安全门是证明远端输入不能借道操作 Mac。至少实施：

- llmServer 管理的临时空白 cwd，每个 Run 独立；
- 当前协议支持的最严格 sandbox 和拒绝审批策略；
- 不加载项目级说明、MCP、skills、plugins、hooks 和用户工具配置，无法关闭的项目必须列入风险；
- 子进程使用最小环境变量，不传客户端秘密和其他 Provider Key；
- 对文件、进程、网络、Keychain、剪贴板、GUI 进行操作系统层监测或隔离；
- 收到 `approval.required` 或本地工具 item 时立即拒绝并使 Run 失败，不静默忽略后继续。

安全 fixture 必须包含诱导读取文件、执行 shell、访问网络、调用已安装 MCP、修改 cwd 外文件的恶意提示。仅验证回答中“我不能”不算通过，必须检查实际副作用为零。

如果当前 Codex 版本无法达到这个门，Provider 状态应为 `unsafe_for_remote`，Stage 2 不得把它开放给 LAN 客户端。允许保留本机实验探针，但这不构成 Stage 退出。

## 10. 模拟传输

自动验收使用一个可脚本化的 stdio JSONL fake process，而不是调用真实 Codex。Fake 必须支持：

- initialize 成功、超时、版本不兼容；
- account 已登录、未登录、认证过期；
- model/list 分页、hidden、模型删除；
- Responses SSE 与 thread/turn 正常文本流；
- token usage 累计更新、重复、缺失、乱序；
- model reroute；
- interrupt 成功、延迟和无响应；
- approval/tool 事件；
- 半行 JSON、未知通知、stderr 噪声、进程崩溃。

fixture 应分别覆盖 Responses SSE 与 App Server，固定 Codex 版本和 schema hash，并保留“输入协议 → 输出事件 → 期望 GatewayEvent”的黄金文件。真实版本升级时先生成新 fixture 和兼容报告，再改变 Adapter。

## 11. 实现切片

### Slice 1：传输协议与兼容门

完成登录态加载、HTTP/SSE、stdio 生命周期、initialize、schema/version 记录、fake server/process。退出条件是异常 SSE/JSON、超时和崩溃均能稳定收敛到 Provider 状态，未知结果不自动重放。

### Slice 2：模型与版本状态

完成版本探测、App Server `model/list`、发现库存和显式 Deployment 验证。退出条件是模型新增/删除不自动改变 `/v1/models`，认证过期只产生脱敏错误。

### Slice 3：turn 与流式输出

完成 Responses 与 thread/turn 的文本流映射和取消。退出条件是两条传输投影一致，断连能终止本地消费且不触发重放。

### Slice 4：usage 与结算

完成 token usage 去重、逐维度字数兜底和公开价格。退出条件是每个 fixture 的金额可复算，缺失 usage 不会被误填为零。

### Slice 5：文本边界与安全门

完成非文本能力拒绝、审批拒绝和工具副作用套件。退出条件是所有模拟恶意场景副作用为零，任何工具/审批事件都明确失败。

### Slice 6：恢复与发布状态

完成进程重启、Provider 状态、用户级 `launchctl` 常驻和脱敏诊断。退出条件是崩溃不重放请求，恢复后新请求可用，历史结算不丢失。

## 12. Stage 2 退出门

必须全部满足：

- Codex 主链只读取当前用户已存在的登录文件并在内存中使用，不写入项目配置、日志、数据库或响应；兜底副本只存在于权限受限的临时目录；
- 直连失败只有在可证明尚未开始成功生成时才能回退，结果不明时禁止重放；
- discovered models 与公开 Deployment 严格分离；
- requested/upstream/effective model 和 reroute 可解释；
- 每个 Codex Deployment 都有输入/输出公开价，手工修改优先且不被上游覆盖；
- 调用方看不到价格来源、上游成本或 Token 估算来源；
- usage 更新不会重复计数，缺失维度使用版本化字数估算而不是零；
- 不发起 Codex 账户额度查询，公开响应与管理统计均不包含 Codex quota；
- 不可证明时拒绝 hard budget，soft overshoot 可解释；
- 取消、进程崩溃和版本不兼容不会触发自动重放；
- 模拟安全套件确认无文件、进程、网络、Keychain、MCP 或 GUI 副作用；
- 全套测试离线通过；
- 发布说明明确：未调用 Hominal；真实 smoke 只证明测试时的当前登录态和模型服务可用，不构成长期可用性或 LAN 安全证明。
