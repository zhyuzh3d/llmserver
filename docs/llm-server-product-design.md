# llmServer 产品与技术总纲 v0.3

> 文档性质：长期产品边界、公共契约与分阶段开发基线
> 目标平台：macOS 本机常驻服务
> 对外形态：面向本机及受信任局域网设备的 OpenAI 兼容 HTTP/SSE API
> 上游类型：标准模型 API、OpenAI Codex、腾讯 WorkBuddy / CodeBuddy Code
> 调研与本机验证日期：2026-08-31
> 当前状态：Stage 1 结算主链、Stage 2/3 首个本机 Adapter、本机管理台和用户级常驻运行已实现；未实现项与真实验证见 [实现状态](./implementation-status.md)

## 1. 产品结论

llmServer 只做一件事：在一台 Mac 上建立统一、可扩展、可计价、可审计的 LLM API 服务，并把不同上游收敛为稳定的 OpenAI 兼容接口。

```text
trusted local / LAN clients
            │  OpenAI-compatible HTTP + SSE
            ▼
┌──────────────────────────────────────────────────────┐
│                      llmServer                       │
│ auth / deployments / routing / events / settlement  │
│ budgets / quota observations / audit / cancellation │
├────────────────┬────────────────┬────────────────────┤
│ Standard API   │ Codex          │ WorkBuddy          │
│ Adapter        │ Adapter        │ Adapter             │
└────────────────┴────────────────┴────────────────────┘
```

调用方只理解 llmServer 的模型 ID、API Key 和响应契约，不理解 Codex App Server、WorkBuddy ACP、上游登录态或上游价格格式。新增上游时增加 Adapter 和 Model Deployment，不改变公共 API。

本仓库不负责 Hominal 或任何具体业务系统的接入、配置迁移和真实运行验收。Hominal 未来只是普通 OpenAI 兼容客户端之一，其适配应在 Hominal 项目中完成。llmServer 的验收使用确定性模拟上游、冻结协议样本和 API 契约测试，不要求 Hominal 发起真实调用。

本机 Web 管理台是稳定管理 API 的薄客户端：负责 Provider/Key、模型发现、Deployment、价格、设备策略和消耗汇总。它不直接调用 Adapter、不持有另一套配置、不参与请求结算，也不是服务恢复所必需的组件。管理端强制监听回环地址。

## 2. 产品边界

### 2.1 必须实现

- `POST /v1/responses`、`POST /v1/chat/completions`、`GET /v1/models`；
- 流式优先的 SSE、非流式聚合、取消、超时与背压；
- 独立客户端 Token、模型授权、并发和请求大小限制；
- 可启停的公开模型配置，以及发现模型与公开模型的严格分离；
- 标准 API、Codex、WorkBuddy 三类可插拔 Adapter；
- 每次调用按公开模型配置价和本次 billable 输入/输出 Token 结算，并返回输入单价、输出单价、输入金额、输出金额和总额；
- 可选的请求价格上限，不设置时按实际用量正常结算；
- 上游额度的调用前、调用后、变化量和重置窗口观测；
- 不依赖提示词的本地执行隔离、凭据隔离和审计；
- 版本化价格表、请求幂等键、持久化结算记录和失败状态解释。

### 2.2 明确不实现

- 任何具体调用方，包括 Hominal 的代码修改或运行编排；
- 公开互联网、多租户商业售卖和支付收款；
- 向调用方、日志、数据库、配置或 Git 导出/下发 Codex/WorkBuddy 的 OAuth、Cookie 或登录文件；只有在官方进程无法直接复用当前用户登录态时，才允许在本机运行时目录建立最小、短生命周期凭据副本，且必须不进入项目目录并在进程结束后删除；
- 默认允许远端请求通过 Codex/WorkBuddy 执行 shell、改文件、操作 GUI 或浏览器；
- 向调用方暴露供应商真实成本、价格来源或 Codex/WorkBuddy 的内部定价方式；
- 完整复刻 Codex、WorkBuddy 的 agent、插件、文件和终端能力；
- 公开互联网管理端、多租户管理和远程下发本机登录凭据。

## 3. 三阶段路线

总体建设顺序固定为三步，后两步复用前一步已经冻结的公共契约与结算内核。

| Stage | 交付内容 | 退出条件 |
| --- | --- | --- |
| Stage 1 | 标准 API 核心、统一事件、Model Deployment、客户端鉴权、价格预算与结算、本机管理面、模拟上游 | 不依赖真实供应商即可完成兼容 API、SSE、预算、结算、配置热更新和失败恢复的确定性测试 |
| Stage 2 | Codex App Server stdio Adapter、模型/usage/多窗口额度映射、工具隔离 | 冻结的 Codex 模拟进程覆盖初始化、生成、取消、reroute、usage、配额与崩溃；无本地副作用 |
| Stage 3 | WorkBuddy/CodeBuddy Adapter、模型/usage/credits/额度映射、工具隔离 | 冻结的 WorkBuddy 模拟进程覆盖生成、取消、usage、额度、模型漂移和权限请求；无本地副作用 |

详细开发文档：

- [Stage 1：标准 API 与结算内核](./stage-1-standard-api.md)
- [Stage 2：Codex Adapter](./stage-2-codex.md)
- [Stage 3：WorkBuddy Adapter](./stage-3-workbuddy.md)

管理台只调用稳定的本机管理 API。公开配置与秘密配置仍分别以 `configs/config.yaml` 和 `../xconfigs/llmserver/xconfig.yaml` 为唯一事实源；管理台只是受约束的读写入口。

## 4. 核心概念

### 4.1 Provider、Discovered Model 与 Model Deployment

三者不能混用：

- `Provider`：上游连接与认证实例，例如一个 OpenAI API Key、当前 Codex 登录态或当前 WorkBuddy 登录态；
- `Discovered Model`：上游当前报告可见的模型库存，只供探测和配置参考；
- `Model Deployment`：管理员明确启用、允许客户端调用并绑定计价规则的稳定公共模型。

`GET /v1/models` 只返回启用且可授权访问的 Model Deployment，不直接倾倒上游发现列表。上游新增模型不会自动暴露，上游删除模型也不会悄悄改变公共模型的含义。

```yaml
deployments:
  - id: codex-terra
    provider: codex-local
    upstream_model: gpt-5.6-terra
    enabled: true
    allowed_clients: [lan-agent]
    execution_profile: model-only
    price_revision: codex-terra-price-1
    capabilities:
      streaming: native
      reasoning_effort: native
      function_tools: emulated
```

公共 `model` 字段始终引用 deployment ID。每次响应同时记录 `requested_model`、`upstream_model` 和能够确认时的 `effective_model`，不得因上游自动路由而覆盖用户请求历史。

### 4.2 Run 与统一事件

所有入口先转换为 `RunRequest`，所有 Adapter 只输出 `GatewayEvent`。SSE 是统一事件的实时投影，非流式结果是同一事件流的有界聚合，结算器也消费同一条主链。

```text
request.accepted
budget.evaluated
provider.selected
quota.before_observed
provider.started
output_text.delta / function_call.completed
usage.updated
provider.completed | provider.failed
quota.after_observed
settlement.completed
response.completed
```

`settlement.completed` 是响应完成的必要状态，不是后台可有可无的 ledger 任务。可以把供应商成本和完整审计异步落盘，但不能先向客户端宣称请求完整成功、再尝试计算公开价格。

### 4.3 能力声明

每个 Deployment 对每项能力声明 `native`、`emulated` 或 `unsupported`。使用不支持的能力时明确失败；只有 Deployment 配置允许时才可模拟。模型名称相似不构成跨 Provider 回退依据。

## 5. 公共 API

### 5.1 首版端点

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/v1/responses` | 第一等接口 |
| `POST` | `/v1/chat/completions` | 兼容入口，共用同一运行主链 |
| `GET` | `/v1/models` | 当前客户端可调用的 Deployment |
| `GET` | `/healthz` | 进程存活 |
| `GET` | `/readyz` | 至少一个授权路由可接单 |
| `GET` | `/llmserver/v1/capabilities` | Deployment 能力与模拟状态 |
| `GET` | `/llmserver/v1/requests/{id}` | 授权范围内的脱敏状态与结算结果 |

首版不实现云端对象存储语义。`store` 仅接受 `false` 或缺省。Chat Completions 只作为协议转换层，不拥有另一套调用、usage 或计价逻辑。

### 5.2 llmServer 扩展

为了不污染 OpenAI 标准字段，扩展请求放在 `llmserver` 对象中：

```json
{
  "model": "codex-terra",
  "input": "...",
  "max_output_tokens": 4096,
  "llmserver": {
    "idempotency_key": "01J...",
    "budget": {
      "max_charge": "0.050000",
      "currency": "USD",
      "mode": "hard"
    }
  }
}
```

`budget` 整体可省略。省略表示不设单请求价格上限，绝不表示免费；请求仍按实际发生的用量结算。

非流式响应在标准对象顶层增加 `llmserver_billing`。流式响应在 `[DONE]` 前增加 `llmserver.billing.completed` SSE 事件。每个响应头都包含 `x-llmserver-request-id`。对于不能容忍扩展字段或自定义 SSE 事件的严格客户端，可显式发送 `x-llmserver-compatibility: strict`：此模式隐藏响应扩展，但结算仍完成，客户端再凭 request ID 查询。

### 5.3 错误

错误使用 OpenAI 风格外壳，并给出稳定 `code`：

- `unknown_model_deployment`
- `model_not_allowed`
- `unsupported_feature`
- `budget_exceeded_before_start`
- `hard_budget_not_enforceable`
- `provider_auth_required`
- `provider_quota_exhausted`
- `provider_protocol_incompatible`
- `usage_unavailable`
- `settlement_failed`
- `provider_process_exited`

任何已产生上游调用、但最终用量无法确认的请求都必须保留 `settlement_status=unconfirmed`，不得伪造零价格。

## 6. 统一公开价格与预算机制

### 6.1 调用方只看到 llmServer 的模型价格

所有启用的 Deployment 都必须绑定一份不可变的 `PriceRevision`。调用方只看到该模型当前公开的输入单价、输出单价和据此计算的金额，不知道也不需要知道价格是人工输入、目录默认值还是供应商建议值。

后台可以记录三类互相独立的数据：

- `public_price`：llmServer 向调用方公开并用于结算的模型价格；
- `upstream_reported_price/cost`：供应商返回或后台同步的价格、成本，只用于控制台、统计和对账；
- `quota_observations`：订阅额度或 credits 快照，与公开价格互不换算。

供应商返回的价格永远不能直接改变本次或未来请求的公开价格。控制台对某个 Deployment 的手工修改具有最高优先级，并进入“手工锁定”状态；后续目录同步只能更新后台参考值，不能覆盖它。没有手工价时可以使用 llmServer 预置的模型默认价；供应商建议价只有被管理员或配置流程明确采纳并生成新的 PriceRevision 后才会生效。任何启用的 Deployment 最终都必须解析出完整的输入价和输出价。

价格的确定性对象是 Deployment，而不是不可控的内部自动路由。对 `workbuddy-auto` 这类 Deployment，即使 WorkBuddy 没有可靠报告内部模型，仍按 `workbuddy-auto` 自己的公开价格结算；`effective_model` 只供后台诊断，不影响调用方价格。

### 6.2 首版只按输入、输出 Token 计价

首版公开算法固定为：

```text
input_charge  = billable_input_tokens  × configured_input_rate
output_charge = billable_output_tokens × configured_output_rate
total_charge  = input_charge + output_charge
```

`billable_input_tokens` 和 `billable_output_tokens` 优先使用 Adapter 从上游 usage 规范化得到的总输入、总输出数量；任一维度缺失时，只对缺失维度使用 llmServer 的确定性字数估算器。缓存输入属于总输入，reasoning token 属于总输出时均不单独加价；相关细分值可以留给后台统计，但不进入调用方价格。供应商自身的缓存折扣、服务层级价格、credits、工具费或返回的 `cost_usd` 也不参与公开结算。

首版兜底算法 `text_estimator_v1` 保持简单：输入侧对 llmServer 实际发送的 instructions、消息文本、函数/schema 文本做规范化拼接；输出侧对最终文本和函数参数 JSON 做规范化拼接。CJK 字符按每字 1 Token，其他 Unicode 字符按每 4 字符 1 Token 向上取整，非空结果最低为 1。图片、音频等非文本输入只有在 Deployment 配置固定等价 Token 后才能进入兜底估算。

上游只返回其中一个维度时，另一个维度单独估算，不丢弃已返回的值。估算出的数量直接成为本次请求的公开计费 Token，与供应商返回值使用同一价格公式。调用方不接收 `reported/estimated/mixed` 标记；后台必须保存每个维度的来源、估算器版本和规范化字符数，以保证排错和历史复算。估算器升级必须使用新版本，只影响升级后开始的 Run。

这套规则对当前文本模型和 agent 调用足够，而且最容易解释。未来若增加必须按图片、音频或工具调用计费的新 API，应增加新的公开价格算法版本，并让对应 Deployment 明确选择；不能在 `input/output` 名义下暗加费用。

费率统一表达为“每一百万 Token 的金额”。所有金额使用十进制定点字符串或整数微单位，不使用二进制浮点数。每个 Run 在接单时固定 `price_revision`，运行中控制台改价不影响已开始请求；历史请求永远按当时的价格版本复算。单个 Deployment/Run 只使用一种结算货币；请求 budget 的 currency 必须与公开价格相同，首版不做汇率换算。

不能把缺失 Token 填零，也不能使用不可复现的临时猜测。兜底估算虽然不追求供应商级精度，但同一文本和同一估算器版本必须得到完全相同的公开计费 Token。

### 6.3 可选价格上限

用户的“价格上限”是可选保护，不是预扣费，也不改变公开计费公式。最终价格始终使用本次确定的 billable input/output Token；上限只决定是否允许开始或是否需要提前停止。

支持两种模式：

- `hard`：必须证明上限可执行。服务根据上游值或输入估算值、`max_output_tokens` 和公开费率计算最坏上界；若上界超过限制则调用前拒绝。若某 Adapter 不能把输出限制映射到公开计费 Token，也在调用前以 `hard_budget_not_enforceable` 拒绝；
- `soft`：调用前给出估算，运行中尽力监测，超过时尽力取消，但允许因 usage 延迟、分词误差或取消延迟产生超额。最终返回本次公开结算价格和 `budget.exceeded=true`。

不允许把“估计费用小于上限”当成最终收费，也不允许为了凑到上限而收取未发生的费用。调用前可以做内部额度保留以防并发超卖，但请求结束后必须按实际结算并释放差额。

### 6.4 返回结构

```json
{
  "llmserver_billing": {
    "request_id": "req_01J...",
    "settlement_status": "confirmed",
    "price_version": "price_01J...",
    "currency": "USD",
    "usage": {
      "input_tokens": 2000,
      "output_tokens": 300
    },
    "unit_prices": {
      "input_per_million": "2.000000",
      "output_per_million": "12.000000"
    },
    "charges": {
      "input": "0.004000",
      "output": "0.003600",
      "total": "0.007600"
    },
    "budget": {
      "mode": "hard",
      "max_charge": "0.050000",
      "exceeded": false
    }
  }
}
```

调用方响应不包含 `pricing_basis`、`upstream_cost`、手工修改标记、Token 来源或供应商价格。若输入或输出 usage 缺失，以版本化字数估算值填补，不能写成 `0`。

对外 OpenAI `usage.input_tokens/output_tokens` 与 `llmserver_billing.usage` 必须使用同一组 billable Token，避免一个响应出现两套数字。上游原始 usage 和更细的 cached/reasoning 字段只保存在后台；需要保留标准细分字段时，也不得改变上述公开计费总量。

## 7. 配额与订阅额度

配额不是价格。不同软件可能同时存在数小时、每日、每周、每月、credits、请求数或 token 等多个窗口，所以 llmServer 不定义“统一剩余额度百分比”，也不把百分比兑换成美元。

配额属于敏感的账号运行信息，默认不向调用方返回。是否返回由客户端 API Key 的服务端策略统一控制，而不是由单次请求参数控制：

```yaml
client_credentials:
  - id: bedroom-device
    response_policy:
      include_quota_observations: false
```

推荐一个物理设备使用一个独立 Key，使控制台能够针对设备启停。未来 Web 控制台修改的也是这项客户端策略；在控制台实现前由配置文件和 CLI 管理。关闭时，响应中完全省略 `quota_status` 和 `quota_observations`，而不是返回空数组暗示额度为零。打开后，所有使用该 Key 的调用都按同一规则返回，客户端不能自行越权开启。

后台是否采集和保存 quota 是独立策略。即使不向调用方公开，Provider 仍可为控制台健康状态和内部记录采集；如果主动读取 before/after 会显著增加延迟，可配置为只使用 Provider 已随调用返回的值或缓存快照。

统一的是返回结构，不是单位：

```json
{
  "provider": "codex-local",
  "limit_id": "codex",
  "unit": "percent_used",
  "window": {
    "duration_seconds": 900,
    "resets_at": "2026-08-30T12:00:00Z"
  },
  "before": "25.000000",
  "after": "31.000000",
  "delta": "6.000000",
  "source": "provider_snapshot",
  "confidence": "observed",
  "attribution": "shared_account_window"
}
```

规则如下：

- 一个请求可返回零个、一个或多个 quota bucket；
- `unit` 可为 `percent_used`、`percent_remaining`、`credits`、`tokens`、`requests` 或 Provider 扩展单位；
- 尽可能在调用前和调用后各读取一次；`delta=after-before`，含义随单位保留；
- 只有调用后值时，`before` 和 `delta` 为 `null`；
- 上游刷新粒度较粗时，单次调用的 delta 可能为零，这不等于该调用没有消耗；
- Codex/WorkBuddy 桌面端或其他 Run 可能共用同一账号，调用前后差值此时只是共享账户窗口变化，不能归因成本次请求；
- 不合并不同窗口，不比较不同 Provider 的百分比，不根据价格反推额度；
- quota 快照失败不改写货币结算，但必须返回 `quota_status=unavailable`。

上面最后一条只适用于允许返回 quota 的客户端；默认关闭时不暴露任何 quota 状态。

Codex 官方协议已经存在按 `limitId` 区分的多 bucket、`usedPercent`、窗口长度和重置时间，因此内部数据模型必须从第一天支持数组和多窗口，而不是先做一个单百分比字段再迁移。

## 8. Adapter 契约

```go
type ProviderAdapter interface {
    ID() string
    Probe(context.Context) ProviderStatus
    DiscoverModels(context.Context) ([]DiscoveredModel, error)
    ObserveQuota(context.Context) ([]QuotaObservation, error)
    Start(context.Context, RunRequest) (EventStream, error)
    Cancel(context.Context, RunID) error
    Close(context.Context) error
}
```

所有 Adapter 必须：

- 只接收已完成鉴权、授权、参数校验和预算判定的内部请求；
- 输出单调序号的统一事件，不绕过主链另交最终结果；
- 明确报告请求模型与有效模型，未知时使用 `null`；
- 区分认证、配额、限流、协议、进程和取消错误；
- 不把缺失 usage、价格或 quota 当作零；
- 客户端断开后传播取消；
- 每个 Run 最多产生一次终态和一次结算；
- 不读取或返回上游原始登录凭据。

标准 API Adapter 优先透传原生 Responses/Chat Completions 和 SSE，只做必要规范化。Codex Adapter 使用同机 stdio App Server。WorkBuddy Adapter 的正式传输要在 Stage 3 原型中由“可隔离性、协议稳定性、usage 完整性”决定，不在总纲中过早锁死 ACP 或 headless 单一路径。

## 9. 函数、会话与 agent 边界

标准 API 的原生 function calls 保留原生语义。Codex/WorkBuddy 若不能原生提供 caller-defined function call，只能在 Deployment 标记 `emulated` 后，用严格输出 schema 模拟；参数必须再次通过原 JSON Schema 校验，且模拟产生的额外用量计入同一结算。

Provider 自己的本地工具和调用方定义的函数是两个命名空间。默认 `model-only` 执行配置不得批准或代理文件、shell、PTY、浏览器、GUI、Keychain、MCP 或插件副作用。仅靠 system prompt、空白工作目录或“未观察到调用”都不构成安全隔离证据。

默认会话模式为 `client_managed`：客户端发送所需上下文，网关不根据 IP、客户端或最近请求隐式续接。未来需要上游粘性会话时必须使用显式 session ID，并把会话归属、Provider、Deployment、配置 revision 和过期时间持久化。

## 10. 安全与局域网访问

默认监听 `127.0.0.1`。允许局域网访问时必须显式配置监听地址，同时启用：

- 独立的高熵客户端 Token，数据库只保存校验摘要；
- 每个客户端的 Deployment allowlist、并发、速率、输入/输出大小和到期时间；
- 每个客户端 Key 的响应字段策略，至少包含 `include_quota_observations`，默认 `false`；
- TLS；内网无法可靠部署证书时，优先 SSH 隧道或受控反向代理，不以“在家里”替代认证；
- 来源 IP allowlist 作为附加限制，而不是唯一认证；
- 默认拒绝 CORS；
- 管理 API 继续仅监听回环或独立 Unix socket。

上游 API Key 存放 macOS Keychain。Codex/WorkBuddy 凭据由各自程序维护，Adapter 默认通过当前用户的 `HOME/CODEX_HOME` 让官方进程自行认证，不解析凭据文件。日志默认不记录 prompt、response、reasoning、工具参数、Authorization、Cookie、账号 ID 和本机敏感路径。

## 11. 配置、持久化与恢复

首版使用声明式 YAML/TOML 加 SQLite 状态库：

- 配置文件：Provider、Deployment、Price Revision、客户端响应策略的期望状态；
- SQLite：配置 revision、请求状态、usage、结算、quota 快照、幂等键、协议兼容结果；
- Keychain：上游 secrets；
- 版本化 fixture/schema 目录：Codex/WorkBuddy 协议样本和兼容基线。

配置加载采用“解析 → 完整校验 → 生成 revision → 原子激活”。错误配置不替换当前活动 revision。每个 Run 固定 config、deployment、price revision 和 estimator version，以便进程崩溃后复算。

相同客户端与相同 `idempotency_key` 的重试必须返回已有 Run，或明确告诉客户端原 Run 仍在进行；不得再次调用上游。若客户端没有幂等键，服务可以生成 request ID，但不能保证断线重试不重复消费。

## 12. 测试与证据边界

三个 Stage 的自动验收均不调用 Hominal，也不要求消耗真实模型额度。测试由四层组成：

1. 纯单元测试：金额定点运算、价格 revision、预算、usage 合并、quota delta；
2. 协议契约测试：OpenAI 请求/响应、SSE、错误、严格兼容模式；
3. 确定性模拟 Provider：脚本化生成、延迟、usage、配额变化、崩溃和取消；
4. 冻结上游 fixture：由已知 Codex/WorkBuddy 协议样本回放，验证版本字段映射。

模拟测试只能证明 llmServer 的协议与状态机正确，不能证明当前真实账号、登录态、供应商服务或实时额度可用。真实 Codex/WorkBuddy 最小生成 smoke test 保留为人工、显式、非发布门动作；未执行时文档和版本说明必须写明“未做真实上游生成验证”。

关键不变量：

- 同一输入/输出 usage + 同一 price revision 必须得到逐位一致的金额；
- 每个成功响应都有结算终态，缺失 usage 不得伪造已确认；
- hard budget 不可证明时调用前失败；
- 不同 quota bucket 永不被合并或换算，默认不向客户端 Key 返回；
- 输出后不跨 Provider 自动重试；
- 客户端取消最终到达 Adapter；
- 两个客户端之间无内容、会话、价格上限和结算串扰；
- 模拟恶意请求无法产生 Mac 文件、进程、网络或 GUI 副作用。

## 13. 技术实现建议

核心服务使用 Go：适合 HTTP/SSE、子进程管理、定点金额、并发调度和单二进制部署。服务通过当前用户域的 `launchctl submit` 常驻，而不是 root LaunchDaemon，因为本机软件登录态属于用户会话。该方式不增加第三个配置文件，系统重启后由管理员重新注册。

首版模块边界：

```text
cmd/llmserver          服务入口
internal/api           OpenAI 兼容解析与编码
internal/run           状态机、事件、取消、幂等
internal/deployment    模型公开与能力策略
internal/pricing       price revision、预算、结算
internal/quota         多窗口额度观测
internal/provider      Adapter SPI
internal/providers/api
internal/providers/codex
internal/providers/workbuddy
internal/auth          客户端 Token 与授权
internal/store         SQLite 与 revision
internal/config        声明式配置与校验
internal/runtimecfg     配置验证、保存与运行快照切换
internal/discovery      三类 Provider 的模型发现
internal/admin          仅回环管理 API 与静态 Web 页面
```

不要建立通用插件运行时、规则 DSL 或复杂路由语言。管理页面保持薄层；新增 Adapter 所需的最小稳定面仍是 `ProviderAdapter + GatewayEvent + ModelDeployment + Settlement + ModelDiscovery`。

## 14. 主要风险与控制

| 风险 | 立场与控制 |
| --- | --- |
| “OpenAI 兼容”被理解为 100% 等价 | 公布支持矩阵，未知参数和不支持能力明确失败 |
| 价格或估算算法变化造成历史账单漂移 | Run 固定不可变 price revision 和 estimator version，金额可离线复算 |
| hard budget 实际无法保证 | 无可靠上界或取消能力时调用前拒绝，不降级成假 hard |
| 供应商价格或模拟方式泄漏给调用方 | 公共响应只包含模型公开价格；来源、手工锁定和上游成本仅后台可见 |
| 多种订阅额度无法统一 | 统一 bucket 数据结构，不统一单位、不换算货币 |
| Codex/WorkBuddy 自动切模型 | 结算绑定 Deployment；有效模型作为独立可空事实 |
| 本地 agent 获得 Mac 权限 | 进程级隔离、拒绝审批、黑盒副作用探针，提示词不算隔离 |
| 断线重试造成重复消费 | 幂等键、持久化 Run、输出后禁止自动重试 |
| 事件发出后结算失败 | 结算属于主链；严格区分 completed、unconfirmed、failed |
| Web UI 提前绑死模型 | 当前移除，待后端管理契约稳定后单独立项 |

## 15. 当前明确决策

1. llmServer 是独立后端服务，不承诺修改或真实运行 Hominal。
2. 所有调用方只看到 Deployment 的公开模型价格，不看到价格来源、供应商成本或模拟方式。
3. 公开价格按总输入、总输出 billable Token 和控制台配置单价计算；上游缺失时用版本化字数估算，手工改价优先级最高。
4. hard 上限只有在可证明时才接受，无法保证时不得假装已保证。
5. quota 与 price 互不换算；quota 是否返回由客户端 Key 策略控制，默认关闭。
6. 公开模型以显式 Model Deployment 为准，发现模型不自动公开。
7. Web 控制台从三个开发 Stage 中移除，配置先用文件和 CLI。
8. 自动验收只要求模拟与 fixture 跑通；真实上游 smoke test 非发布门，且必须如实标注。

## 16. 参考资料

- [OpenAI Codex App Server](https://learn.chatgpt.com/docs/app-server)
- [OpenAI Responses API](https://developers.openai.com/api/reference/resources/responses)
- [OpenAI 模型与价格比较](https://developers.openai.com/api/docs/models/compare)
- [CodeBuddy Code HTTP API Beta](https://www.codebuddy.ai/docs/cli/http-api)
- [腾讯云 WorkBuddy Enterprise CLI 命令参考](https://cloud.tencent.com/document/product/1831/137026)
- [腾讯云 WorkBuddy Enterprise IDE / ACP 集成](https://cloud.tencent.com/document/product/1831/137019)
