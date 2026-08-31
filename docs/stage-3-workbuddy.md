# Stage 3：WorkBuddy Adapter 开发计划

> 前置文档：[llmServer 后端产品与技术总纲](./llm-server-product-design.md)、[Stage 1：标准 API 与结算内核](./stage-1-standard-api.md)、[Stage 2：Codex Adapter](./stage-2-codex.md)
> 范围：把本机 WorkBuddy / CodeBuddy Code 作为受限、可计价、可观测的 Provider 接入
> 明确排除：暴露 WorkBuddy 管理/文件/PTY API、向调用方/Git 导出登录凭据、Web 控制台、Hominal 接入
> 实现状态：已选择并实现逐请求 headless `stream-json` Adapter，真实验证当前登录态、固定模型、usage 与统一公开结算；额度读取和 LAN 操作系统级隔离仍未完成

## 1. Stage 目标

Stage 3 的目标不是复刻 WorkBuddy，而是从其公开或随版本可验证的自动化入口中提取 llmServer 所需的最小模型能力：文本/结构化输出、流式事件、取消、usage、实际模型线索和订阅额度/credits 观测。

WorkBuddy 的难点不是“能否打印回答”，而是以下事实必须同时成立：

- 无头协议在目标版本可稳定解析；
- caller-defined tools 与 WorkBuddy 自身 agent 工具严格分离；
- 本地工具不会因远端提示被执行；
- usage 或确定性兜底估算足够支持公开价格结算；
- `auto` 路由、credits 和多时间窗口不会被伪装成统一真实成本。

Adapter 必须复用 Stage 1 的全部公共契约，WorkBuddy 专属字段只能留在 Provider 内部或脱敏诊断信息中。

## 2. 传输选择门

候选路径包括 ACP stdio、headless `stream-json` 和本机 `--serve` HTTP API。首版已经选择 headless：它能为每个请求建立独立进程和临时 cwd，直接输出结构化流与终态 usage，取消时可杀死单独进程组，也不会把 WorkBuddy 的管理 HTTP 面暴露给 llmserver。

| 维度 | 必须回答的问题 |
| --- | --- |
| 协议稳定性 | 是否公开、有版本或 schema、未知事件能否向前兼容 |
| 流式 | 是否提供真实 delta、终态、usage 和错误，而非只能等完整文本 |
| 取消 | 是否能精确取消当前 run，而不是只能杀死共享进程 |
| 会话隔离 | request/session 是否有稳定关联，是否会隐式复用上下文 |
| 结构化输出 | JSON Schema 是否稳定，失败能否明确识别 |
| usage | input/output 是否完整；缺失维度能否从可见文本稳定估算 |
| 模型事实 | requested model、effective model、auto 路由是否可区分 |
| quota | credits、百分比、窗口和重置时间能否结构化读取 |
| 工具安全 | 能否阻止文件、shell、PTY、MCP、插件和 GUI 副作用 |
| 运维 | 进程生命周期、崩溃恢复、版本升级是否可控 |

当前不实现运行时协议 fallback。ACP/HTTP 只有在未来能显著改善额度读取或隔离，且经过相同语义 fixture 后，才能作为显式的新 Provider 类型接入，不能悄悄改变现有 Deployment 语义。

`--serve` 即使被选作本机后端，也只能监听回环，由 llmServer 独占访问，并只调用经过 allowlist 的 run/result/cancel 端点。`/fs`、`/process`、`/pty`、`/settings`、插件和凭据相关端点永不代理给客户端。

## 3. 进程与会话模型

首版默认单活跃 Run，正式路径为 headless：

- 每 Run 独立子进程和临时空白 cwd，通过 stdin/JSONL stdout 通信；
- `--tools ""`、严格空 MCP 配置、空 setting sources、`dontAsk`、单 turn、禁用后台任务；
- 当前用户的 `HOME` 只交给官方 WorkBuddy 进程完成现有登录态认证，llmserver 不读取或复制凭据；
- 客户端取消终止该 Run 的独立进程组，不自动重放。

无论哪条路径，都必须满足：

```text
one llmserver request_id
↔ one owned upstream session/run
↔ one ordered GatewayEvent stream
↔ one terminal state
↔ one settlement
```

禁止使用“最近一次 session”、当前工作区或客户端 IP 隐式续接。进程崩溃、session 丢失、CLI 升级后不自动重放已开始的请求。

## 4. 模型与 Deployment

WorkBuddy 上游可能支持固定模型和 `auto`。发现结果只进入 inventory，公开模型仍由配置显式建立：

```yaml
prices:
  - revision: workbuddy-auto-price-1
    currency: USD
    source: manual_override # 仅后台可见
    per_million:
      input: "1.000000"
      output: "6.000000"

deployments:
  - id: workbuddy-auto
    provider: workbuddy-local
    upstream_model: auto
    price_revision: workbuddy-auto-price-1
    usage_fallback:
      algorithm: text_estimator_v1
    execution_profile: model-only
    enabled: true
```

示例价格只说明格式。`workbuddy-auto` 的结算按这个 Deployment 的固定公开价格，而不是 WorkBuddy 返回的价格或运行后观察到的内部模型价格。若上游报告有效模型，则把它写入后台 `effective_model`；未报告则为 `null`。调用方只看到 `requested_model=workbuddy-auto` 和该 Deployment 的公开价格。

管理员未来可为固定模型建立独立 Deployment 和独立 price revision，但只有探针证实固定选择被上游遵守时，才能声明 `model_selection=native`。控制台手工价格优先级最高；上游悄悄切换或价格变化只能更新后台记录，不能静默改价。

## 5. usage 与公开计价

WorkBuddy 事件中可能同时出现模型 token usage、内部运行轮次、subscription credits 或一个名为 cost 的字段。Adapter 必须先确认每个字段的定义，再分别映射：

- input/output token → 优先形成公开计费 `Usage`；
- llmServer 模型 price revision → `charges`；
- 上游明确的现金金额 → 后台 `upstream_cost_records`；
- credits/额度 → `quota_observations`；
- 不明确的 `total_cost_usd: 0` → unknown，不解释为免费。

一次请求若包含多个内部生成轮，所有可归属该 Run 的 token meter 都要汇总。usage 事件如果是累计值，要用单调快照去重；如果是 delta，则按事件 ID 幂等累加。两者不能靠字段名猜测，必须在版本 fixture 中声明语义。

上游缺失 input 或 output 时，对缺失维度使用 `text_estimator_v1`：输入按 llmServer 实际发送的 instructions、消息和 schema 文本估算，输出按最终文本或函数 JSON 估算。CJK 每字 1 Token，其他 Unicode 字符每 4 字符 1 Token 并向上取整。估算值直接作为公开计费 Token；来源、估算器版本和字符数只在后台记录，不向调用方披露。禁止把缺失值填零。

## 6. quota、credits 与多窗口

WorkBuddy 可能按月、按周、数小时窗口、credits 或模型倍率限制。llmServer 不建立通用“剩余额度”标量，而是保留每一个独立 bucket：

```json
{
  "provider": "workbuddy-local",
  "limit_id": "credits-monthly",
  "unit": "credits_remaining",
  "window": {
    "duration_seconds": null,
    "resets_at": "2026-09-01T00:00:00+08:00"
  },
  "before": "120.000000",
  "after": "118.500000",
  "delta": "-1.500000",
  "source": "provider_snapshot",
  "confidence": "observed",
  "attribution": "shared_account_window"
}
```

如果另有五小时 `percent_used`，它是第二条 observation，不与 monthly credits 相加。对于 remaining 单位，消耗表现为负 delta；对于 used 单位，消耗表现为正 delta。API 字段保留单位语义，不为了让数字方向一致而篡改原值。

调用前后快照只有 bucket ID、单位和同一窗口标识均相等时才计算 delta。窗口重置、来源变化或刷新延迟要显式标记。同一 WorkBuddy 账号可能同时被桌面端或其他 Run 使用，因此差值默认是共享账户观测，不宣称完全归因于当前请求。额度变化只能作为观测，不能用来替代公开价格，也不能从公开金额反推 credits。quota 只向 `include_quota_observations=true` 的客户端 Key 返回，默认完全省略。

## 7. hard/soft budget

WorkBuddy 的 agent 内部轮次和 `auto` 路由可能使最坏成本不可预测。因此默认能力应是：

- `hard_budget_supported=false`，直到固定模型、最大内部轮数、输出上限和全部计费 meter 均可证明受控；
- hard 请求在进入上游前失败；
- soft 请求根据当前累计 usage 触发 cancel/interrupt/kill，但允许 overshoot；
- 无 budget 请求按最终 billable usage 正常结算；
- credits 不作为货币 hard budget 的替代判断。

可以另外为客户端策略配置“额度耗尽前不接单”，但那是 quota admission policy，不是价格上限，错误码和返回字段必须区分。

## 8. 输出、结构化结果与函数模拟

文本和结构化结果转成统一事件。使用 `--json-schema` 或协议等价能力时，Adapter 仍要进行第二次本地 JSON Schema 校验，防止上游声称成功但结构不合法。

caller-defined function 模拟遵循 Stage 2 相同规则：

- caller tool 和 WorkBuddy agent tool 永不共用名称空间；
- 一次最多一个模拟 function call，除非后续契约明确扩展；
- `tool_choice` 约束进入 schema；
- 参数校验失败最多一次修复，额外 usage 计费；
- 能力返回 `function_tools=emulated`；
- WorkBuddy 工具目录、permission request 或 tool call 不是 caller function call。

如果 headless 初始化事件会报告工具目录，这只能证明工具存在，不能证明工具被禁用。安全结论必须来自权限处理和实际副作用检查。

## 9. model-only 安全门

WorkBuddy/CodeBuddy 可能具备文件、shell、PTY、进程、MCP、插件和 IDE 控制。默认 LAN Deployment 必须采用多层隔离：

- 每 Run 临时空白 cwd，不放置项目说明和真实代码；
- 使用当前版本最严格的 MCP、tool、permission 和 approval 配置；
- 不加载用户插件、项目 rules、skills、自定义 agent 和持久记忆；
- 进程最小环境，不继承其他 Provider secrets；
- 协议层拒绝全部 permission request 和 agent tool call；
- 操作系统层监测文件、子进程、网络、Keychain、剪贴板和 GUI 副作用；
- `--serve` 路径实施端点 allowlist，永不转发管理能力。

无法可靠关闭工具时，不允许用 system prompt 代替隔离。Provider 应标记 `unsafe_for_remote` 并拒绝 LAN 客户端请求；本机开发探针可以保留，但不能算 Stage 3 完成。

## 10. 模拟 WorkBuddy

自动验收使用可脚本化 fake CLI/ACP/HTTP server，并为最终选定传输保留黄金 fixture。至少覆盖：

- 初始化、认证成功/失效、模型列表；
- 文本 delta、结构化成功/失败、内部多轮；
- requested model 与 effective model 相同、不同、未知；
- usage 累计、delta、重复、乱序、仅 total、完全缺失；
- credits、percent、数小时/周/月多 bucket、窗口重置；
- permission request、agent tool call、工具目录 init；
- cancel 成功、延迟、无响应；
- stderr 噪声、畸形 JSON、进程崩溃、HTTP 半关闭；
- 同一 session 事件串到另一个 Run 的攻击性 fixture。

不同传输路径若都保留，必须跑同一语义 fixture 并明确能力差异。不能让运行时 fallback 在客户端不知情时改变函数、usage 或结算状态。

## 11. 实现切片

### Slice 1：协议探针与选择记录

为 ACP、headless、HTTP 运行相同只读探针，生成能力/安全/usage 评分和正式传输决策记录。退出条件是选择有证据，不以代码量最少作为唯一理由。

### Slice 2：进程、会话与事件

实现选定传输的生命周期、run/session 映射、文本流和终态。退出条件是两个并发模拟 Run 不串事件，崩溃不重放。

### Slice 3：结构化输出与取消

实现 JSON Schema、函数模拟、客户端断连和上游取消。退出条件是结构校验、修复轮计费、取消延迟均可确定测试。

### Slice 4：usage、价格与模型事实

实现 usage 语义、逐维度字数兜底、price revision、auto/effective model 后台记录。退出条件是内部多轮可归属 usage 完整汇总，缺失维度稳定估算，未知 effective model 不影响 Deployment 价格。

### Slice 5：quota/credits

实现调用前后快照、多单位、多窗口和重置检测。退出条件是任何 fixture 都不会把百分比、credits 与美元互换。

### Slice 6：安全与恢复

完成拒绝 permission/tool、操作系统副作用探针、进程恢复和版本兼容门。退出条件是副作用为零，升级不兼容时 Provider 拒绝接单。

## 12. Stage 3 退出门

必须全部满足：

- 正式传输选择有可复查的协议、安全、usage 和取消证据；
- WorkBuddy 管理、文件、PTY、进程、插件和设置端点不对外暴露；
- 每个 Run 有独立 session/run 归属，事件不串流；
- Deployment 与 discovered model 分离，auto 与 effective model 分开记录；
- 每个 Deployment 都有输入/输出公开价，手工修改优先且不被上游覆盖；
- 调用方看不到价格来源、上游成本或 Token 估算来源，零成本字段不被解释为免费；
- input/output 缺失维度使用版本化字数估算，不填零；
- credits、percent 和不同时间窗口保留为独立 quota bucket，按客户端 Key 控制且默认不返回；
- 不可证明时拒绝 hard budget，soft overshoot 可解释；
- permission 或 agent tool 事件被明确拒绝；
- 模拟安全套件确认无文件、进程、网络、Keychain、MCP、插件或 GUI 副作用；
- 全套测试离线通过；
- 发布说明明确：未调用 Hominal；真实 smoke 只证明测试时的当前登录态、实时模型、token usage 和本次 `rawUsage.credit` 可用。当前仍没有可验证的剩余积分余额快照，因此 WorkBuddy quota 暂不返回；模型积分倍率只作目录信息展示，不能拿倍率、`total_cost_usd` 或公开价格反推余额。
