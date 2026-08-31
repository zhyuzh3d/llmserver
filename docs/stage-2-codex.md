# Stage 2：Codex Adapter 开发计划

> 前置文档：[llmServer 后端产品与技术总纲](./llm-server-product-design.md)、[Stage 1：标准 API 与结算内核](./stage-1-standard-api.md)
> 范围：把本机 Codex 作为一个受限、可计价、可观测的 Provider 接入
> 明确排除：远程控制 Codex、向调用方/Git 导出登录凭据、Web 控制台、Hominal 接入
> 实现状态：已实现并真实验证逐请求 `codex exec --json --ephemeral` Adapter、公开计价、usage、取消和 App Server 多窗口额度观测；仍是回环受控实验状态，LAN 发布所需的操作系统级读取/网络隔离尚未完成

## 1. Stage 目标

Stage 2 不把 Codex 伪装成普通 HTTP 模型，也不把完整 Codex agent 暴露给局域网。它只从 Codex App Server 中提取 llmServer 公共 API 所需的模型生成、流式输出、结构化结果、usage、有效模型和额度观测，并把本地工具能力隔离在默认 `model-only` 执行配置之外。

Codex Adapter 必须完全复用 Stage 1 的 Deployment、Run、GatewayEvent、PriceRevision、Budget、Settlement 和 QuotaObservation。不得增加 Codex 专属价格字段或让客户端理解 Codex thread/turn。

## 2. 已确认协议边界

当前实现把职责分成两条官方本机协议，不使用面向远程的 WebSocket：

- 生成链使用每 Run 独立的 `codex exec --json --ephemeral`，从 stdin 接收请求，从 JSONL stdout 接收文本终态和 usage；
- 额度链使用短生命周期 `codex app-server --stdio`，调用 `account/rateLimits/read` 获取生成前后快照。

当前生成链为：

```text
rateLimits/read (before)
→ codex exec process start
→ thread.started / turn.started
→ item.started / item.completed
→ turn.completed + usage
→ rateLimits/read (after)
→ settlement
```

官方 App Server 允许模型列表、模型 reroute、token usage 更新和 ChatGPT 多 bucket rate limits。当前 Adapter 不读取 `~/.codex/auth.json`，而是把当前用户的 `HOME/CODEX_HOME` 交给官方 Codex 进程自行认证；不解析 ChatGPT App 私有数据库，也不从 UI 文本猜测额度。当前路径无需复制凭据，将来若官方运行方式迫使建立副本，副本只能位于项目外的临时目录并在 Run 后清除。

本 Stage 不锁定某个 Codex 字段版本。启动时记录：

- Codex 可执行文件绝对路径；
- `codex --version`；
- App Server schema hash；
- llmServer 支持的协议兼容等级；
- 最近一次离线 fixture 套件结果。

版本或 schema 未通过兼容门时 Provider 状态为 `incompatible`，不得带着旧字段继续调用。

## 3. 进程与并发模型

首版采用逐请求子进程而非长期共享 App Server。这样每个 llmServer Run 只拥有一个临时 cwd 和一个 Codex 进程，不复用 thread，不保存 session：

```text
request_id
client_id
deployment_id
codex_process_id (仅运行时)
started_at
terminal_state
```

当前每个 Codex Provider 只允许一个活跃 Run，Adapter 通过单槽背压等待。未来应把排队期限和公平性收敛进 Stage 1 调度器，再开放可配置并发。

子进程崩溃时：

- 所有在途 Run 标记 `provider_process_exited`；
- 已收到的 usage 进入 unconfirmed/confirmed 判定，不清零；
- 不自动重新提交 turn；
- 不自动重放；下一次请求创建全新进程；
- 启动时用 `codex --version` 前缀门拒绝已知不兼容版本。

客户端取消会终止该 Run 的独立进程组，不影响其他 Provider。因为当前没有共享生成进程，不需要用 thread interrupt 区分多个在途 turn。

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

Adapter 将 `thread/tokenUsage/updated` 和 turn 终态中的 usage 规范化为累计 meter。必须确认上游字段是累计值还是 delta；兼容 fixture 要包含重复更新、回退值和缺失终态等情况，防止重复累加。

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

## 6. 配额观测

调用前和调用后分别调用 `account/rateLimits/read`。官方协议可能同时返回多个 `limitId`，每个 bucket 又可能有 primary/secondary window、used percent、窗口长度、重置时间和 credits。因此映射规则为：

```text
limit_id   = upstream limitId + window role
unit       = percent_used or provider-reported credit unit
before     = pre-call snapshot
after      = post-call snapshot
delta      = after - before, only when comparable
window     = duration + resets_at
source     = codex.account/rateLimits/read
confidence = observed
attribution = shared_account_window unless exclusive ownership is proven
```

同一个 `limitId` 的 primary 与 secondary 是两个独立 bucket，不合并。调用前后如果窗口已重置、`resetsAt` 改变或 bucket 消失，则两次值不可直接相减，`delta=null` 并标记 `window_changed`。由于 ChatGPT/Codex 桌面端或其他 Run 可能同时消耗相同账号，before/after 差值默认只能标记为共享账户窗口变化，不能声称完全由当前请求造成。

配额读取失败不能改变已完成的公开结算。只有客户端 Key 的 `include_quota_observations=true` 时才返回 quota；默认 Key 完全不接收 quota 字段。百分比 delta 只能说明快照变化，不能证明本次 Run 的精确消耗，更不能换算成美元。

## 7. hard/soft budget 在 Codex 中的含义

Codex 可能在一个 turn 内产生不可见的内部步骤，且 token usage update 不保证逐 token 到达。因此：

- 只有当前协议能可靠传递输出上界，并且上游 Token 或估算器计费单位都能在该上界内受控时，Deployment 才声明 `hard_budget_supported=true`；
- 不满足条件时，任何 hard budget 在调用前返回 `hard_budget_not_enforceable`；
- soft budget 可在 usage update 达到阈值后 interrupt，但明确允许 overshoot；
- 未设置 budget 时照常调用并按本次 billable usage 结算。

不要因为能调用 `turn/interrupt` 就推断 hard budget 可保证。取消延迟和 usage 更新粒度决定它通常只能支撑 soft budget。

## 8. 输出与函数映射

文本 delta 映射为统一 `output_text.delta`。Codex item 中的 agent 消息、公开 reasoning summary、错误和 turn 终态分别映射；原始 chain-of-thought 不输出、不入普通日志。

caller-defined function tools 首版采用受限模拟：

1. llmServer 根据允许的单个或有限候选函数构造严格 JSON Schema；
2. 通过当前 App Server 的结构化输出能力要求最终结果；
3. 对结果再次做 JSON Schema 校验；
4. 转换为 Responses `function_call` 或 Chat Completions `tool_calls`；
5. 修复轮最多一次且 usage 计入同一 Run。

能力标记为 `function_tools=emulated`。Codex 自己的 shell、文件、MCP、web 或 GUI 工具事件绝不能转换为调用方函数。

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

## 10. 模拟 App Server

自动验收使用一个可脚本化的 stdio JSONL fake process，而不是调用真实 Codex。Fake 必须支持：

- initialize 成功、超时、版本不兼容；
- account 已登录、未登录、认证过期；
- model/list 分页、hidden、模型删除；
- thread/turn 正常流式和结构化结果；
- token usage 累计更新、重复、缺失、乱序；
- model reroute；
- rate limits 单 bucket、多 bucket、窗口重置、credits；
- interrupt 成功、延迟和无响应；
- approval/tool 事件；
- 半行 JSON、未知通知、stderr 噪声、进程崩溃。

fixture 应固定 Codex 版本和 schema hash，并保留“输入协议 → 输出事件 → 期望 GatewayEvent”的黄金文件。真实版本升级时先生成新 fixture 和兼容报告，再改变 Adapter。

## 11. 实现切片

### Slice 1：进程协议与兼容门

完成二进制探测、stdio 生命周期、initialize、schema/version 记录、fake process。退出条件是异常 JSON、超时和崩溃均能稳定收敛到 Provider 状态。

### Slice 2：模型与账号状态

完成 account/read、model/list、发现库存和显式 Deployment 验证。退出条件是模型新增/删除不自动改变 `/v1/models`。

### Slice 3：turn 与流式输出

完成 thread/turn、item 映射、文本流、结构化结果、取消。退出条件是流式/非流式投影一致，断连最终 interrupt。

### Slice 4：usage、结算与 quota

完成 token usage 去重、逐维度字数兜底、公开价格和调用前后多窗口额度观测。退出条件是每个 fixture 的金额可复算，窗口变化不会生成错误 delta。

### Slice 5：函数模拟与安全门

完成 output schema、函数转换、拒绝审批、工具副作用套件。退出条件是所有模拟恶意场景副作用为零，任何工具/审批事件都明确失败。

### Slice 6：恢复与发布状态

完成进程重启、Provider 状态、用户级 `launchctl` 常驻和脱敏诊断。退出条件是崩溃不重放请求，恢复后新请求可用，历史结算不丢失。

## 12. Stage 2 退出门

必须全部满足：

- Codex 只通过公开 App Server stdio 协议接入，不读取登录文件；
- discovered models 与公开 Deployment 严格分离；
- requested/upstream/effective model 和 reroute 可解释；
- 每个 Codex Deployment 都有输入/输出公开价，手工修改优先且不被上游覆盖；
- 调用方看不到价格来源、上游成本或 Token 估算来源；
- usage 更新不会重复计数，缺失维度使用版本化字数估算而不是零；
- quota 支持多 limit、多窗口、窗口重置和不可比较 delta，并按客户端 Key 控制、默认不返回；
- 不可证明时拒绝 hard budget，soft overshoot 可解释；
- 取消、进程崩溃和版本不兼容不会触发自动重放；
- 模拟安全套件确认无文件、进程、网络、Keychain、MCP 或 GUI 副作用；
- 全套测试离线通过；
- 发布说明明确：未调用 Hominal；真实 smoke 只证明测试时的当前登录态、模型服务和额度快照可用，不构成长期可用性或 LAN 安全证明。
