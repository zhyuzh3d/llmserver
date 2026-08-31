# Stage 3：WorkBuddy Adapter 开发计划

> 前置文档：[llmServer 后端产品与技术总纲](./llm-server-product-design.md)、[Stage 1：标准 API 与结算内核](./stage-1-standard-api.md)、[Stage 2：Codex Adapter](./stage-2-codex.md)
> 范围：把本机 WorkBuddy / CodeBuddy Code 作为受限、可计价、可观测的 Provider 接入
> 明确排除：暴露 WorkBuddy 管理/文件/PTY API、向调用方/Git 导出登录凭据、Web 控制台、Hominal 接入
> 实现状态：已选择并实现常驻 ACP stdio Adapter，真实验证当前登录态、固定模型、答案 delta、usage、本次运行积分与统一公开结算；不发起额外额度、积分或余额请求

## 1. Stage 目标

Stage 3 的目标不是复刻 WorkBuddy，而是从其公开或随版本可验证的自动化入口中提取 llmServer 所需的最小模型能力：文本/结构化输出、流式事件、取消、usage、实际模型线索和本次运行已经产生的 credits。

WorkBuddy 的难点不是“能否打印回答”，而是以下事实必须同时成立：

- 无头协议在目标版本可稳定解析；
- caller-defined tools 与 WorkBuddy 自身 agent 工具严格分离；
- 本地工具不会因远端提示被执行；
- usage 或确定性兜底估算足够支持公开价格结算；
- `auto` 路由和 credits 不会被伪装成公开价格或账户余额。

Adapter 必须复用 Stage 1 的全部公共契约，WorkBuddy 专属字段只能留在 Provider 内部或脱敏诊断信息中。

## 2. 传输选择门

候选路径包括 ACP stdio、headless `stream-json` 和本机 `--serve` HTTP API。受控探针确认 ACP 是 WorkBuddy IDE/Web 集成使用的长连接路径：同一进程可建立多个独立 session，并以结构化事件报告答案 delta、阶段、usage 和积分。正式实现因此选择 ACP，消除逐请求 CLI/Node 冷启动；不会把 WorkBuddy 的管理 HTTP 面暴露给 llmserver。

| 维度 | 必须回答的问题 |
| --- | --- |
| 协议稳定性 | 是否公开、有版本或 schema、未知事件能否向前兼容 |
| 流式 | 是否提供真实 delta、终态、usage 和错误，而非只能等完整文本 |
| 取消 | 是否能精确取消当前 run，而不是只能杀死共享进程 |
| 会话隔离 | request/session 是否有稳定关联，是否会隐式复用上下文 |
| 结构化输出 | JSON Schema 是否稳定，失败能否明确识别 |
| usage | input/output 是否完整；缺失维度能否从可见文本稳定估算 |
| 模型事实 | requested model、effective model、auto 路由是否可区分 |
| 本次积分 | 生成结果是否已经提供可归属于当前 session 的 credits |
| 工具安全 | 能否阻止文件、shell、PTY、MCP、插件和 GUI 副作用 |
| 运维 | 进程生命周期、崩溃恢复、版本升级是否可控 |

当前不做 headless/HTTP 运行时 fallback。ACP 不可用时请求明确失败，不自动切换另一条可能具有不同 usage、取消或安全语义的传输。

`--serve` 即使被选作本机后端，也只能监听回环，由 llmServer 独占访问，并只调用经过 allowlist 的 run/result/cancel 端点。`/fs`、`/process`、`/pty`、`/settings`、插件和凭据相关端点永不代理给客户端。

## 3. 进程与会话模型

正式路径为 ACP stdio worker pool，并使用 `max_concurrency` 控制有界并发：

- worker 跨请求复用，常用的两个并发槽在 Provider 构建时预热；
- 每 Run 新建 ACP session 和临时空白 cwd，通过 `session/set_model` 显式选定上游模型，并通过 `session/set_config_option(thought_level)` 显式设置本次推理强度，不复用对话历史；
- CLI 使用 `--tools ""`、严格空 MCP 配置、空 setting sources、`dontAsk`、单 turn、禁用后台任务；
- 当前用户的 `HOME` 只交给官方 WorkBuddy 进程完成现有登录态认证，llmserver 不读取或复制凭据；
- 客户端取消或协议污染会关闭当前 worker，不自动重放；其他 worker 和 Provider 不受影响。

ACP 在答案和 usage 完成后还可能等待约 5 秒生成 UI 对话标题。Adapter 只在 `model_done` 且同一次调用的 usage 已到达时发送完成事件，随后对该 session 发 `session/cancel`，使 worker 尽快回池；这不会截断答案或结算。

不能只把 `--effort` 传给 ACP 进程：当前版本的 `session/new` 探针显示其 `thought_level` 仍可能是默认 `enabled`。每次 session 都必须显式设置，否则管理配置中的 `minimal/low` 只是表面值，实际隐藏推理时长可能完全没有降低。

Provider worker 的热态必须跨普通配置热更新保留。价格、Deployment、访问密钥、供应商显示名称和模型发现 URL 不改变 ACP 生成连接，不能因此销毁 worker；只有 executable、版本门、extra args、并发、默认推理强度或预热设置变化时替换该 Provider。启用自动预热后，服务启动和 worker 失效补池时对每个常驻槽位执行一次最短真实生成；这是管理员明确接受的平台运行开销，不归属于任何访问密钥账单，必须在日志中记录耗时和 Token，且预热失败不得触发 launchd 重启—预热—再失败的消耗循环。

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
- 同一次 ACP usage 的 `credit` → 后台 `upstream_cost_records`，单位 `POINTS`；
- 不明确的 `total_cost_usd: 0` → unknown，不解释为免费。

一次请求若包含多个内部生成轮，所有可归属该 Run 的 token meter 都要汇总。usage 事件如果是累计值，要用单调快照去重；如果是 delta，则按事件 ID 幂等累加。两者不能靠字段名猜测，必须在版本 fixture 中声明语义。

上游缺失 input 或 output 时，对缺失维度使用 `text_estimator_v1`：输入按 llmServer 实际发送的 instructions、消息和 schema 文本估算，输出按最终文本或函数 JSON 估算。CJK 每字 1 Token，其他 Unicode 字符每 4 字符 1 Token 并向上取整。估算值直接作为公开计费 Token；来源、估算器版本和字符数只在后台记录，不向调用方披露。禁止把缺失值填零。

## 6. 本次积分记录

Adapter 不调用 WorkBuddy 的账户额度、积分余额或订阅窗口接口。生成过程中只读取同一 ACP session 的 usage 事件；其中 credit 存在且为非负数时以 `POINTS` 进入后台实际消耗，读取不到就保持未知，不扫描本地历史会话、不发起补查，也不阻塞公开价格结算。

模型目录中的积分倍率只作为目录信息展示，不能乘 Token 反推本次积分，更不能推导账户余额。调用方费用始终按 Deployment 的公开输入/输出价格计算。

## 7. hard/soft budget

WorkBuddy 的 agent 内部轮次和 `auto` 路由可能使最坏成本不可预测。因此默认能力应是：

- `hard_budget_supported=false`，直到固定模型、最大内部轮数、输出上限和全部计费 meter 均可证明受控；
- hard 请求在进入上游前失败；
- soft 请求根据当前累计 usage 触发 cancel/interrupt/kill，但允许 overshoot；
- 无 budget 请求按最终 billable usage 正常结算；
- credits 不作为货币 hard budget 的替代判断。

## 8. 输出边界

答案文本转成统一事件；思考片段、阶段状态、标题和命令目录不对外输出。当前公共契约只支持文本输入和文本输出，不支持 caller-defined function tools 或结构化输出。

公开请求携带 tools、tool choice 或 JSON Schema 时在进入 Provider 前拒绝。WorkBuddy 的工具目录、permission request 或 tool call 不是 caller function call；ACP 观察到 tool/interruption 事件时失败关闭。

如果 ACP 初始化或 session 事件报告工具目录，这只能证明工具存在，不能证明工具被禁用。安全结论必须来自权限处理和实际副作用检查。

## 9. model-only 安全门

WorkBuddy/CodeBuddy 可能具备文件、shell、PTY、进程、MCP、插件和 IDE 控制。默认 LAN Deployment 必须采用多层隔离：

- 每 Run 临时空白 cwd，不放置项目说明和真实代码；
- 使用当前版本最严格的 MCP、tool、permission 和 approval 配置；
- session 不落盘；关闭 Auto Memory、system reminder、会话摘要/标题、热重载、插件市场、cron、REPL 和遥测；
- 不加载用户插件、项目 rules、skills、自定义 agent 和持久记忆，固定使用原生 `cli` 主 Agent；
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
- 同一次 ACP usage 的 credit 存在、缺失、重复和格式错误；
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

### Slice 3：文本输出与取消

实现答案 delta、客户端断连和上游取消。退出条件是思考/状态不会混入答案、取消延迟可确定测试、标题生成不拖延公开完成。

### Slice 4：usage、价格与模型事实

实现 usage 语义、逐维度字数兜底、price revision、auto/effective model 后台记录。退出条件是内部多轮可归属 usage 完整汇总，缺失维度稳定估算，未知 effective model 不影响 Deployment 价格。

### Slice 5：本次积分

实现同一次 ACP usage 的 credit 读取。退出条件是读取不增加外部请求或本地扫描，缺失积分不阻塞结算，POINTS 不与美元互换。

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
- 不请求账户额度或余额；仅把同一次 ACP usage 已返回的积分作为后台实际消耗；
- 不可证明时拒绝 hard budget，soft overshoot 可解释；
- permission 或 agent tool 事件被明确拒绝；
- 模拟安全套件确认无文件、进程、网络、Keychain、MCP、插件或 GUI 副作用；
- 全套测试离线通过；
- 发布说明明确：未调用 Hominal；真实 smoke 只证明测试时的当前登录态、实时模型、ACP usage 和本次 credit 可用。模型积分倍率只作目录信息展示，不能拿倍率、`total_cost_usd` 或公开价格反推余额。
