# Stage 1：标准 API 与结算内核开发计划

> 前置文档：[llmServer 后端产品与技术总纲](./llm-server-product-design.md)
> 范围：标准 API Provider、公共 OpenAI 兼容接口、统一运行主链、价格与配额基础设施
> 明确排除：Codex、WorkBuddy、Web 控制台、Hominal 接入、真实付费模型调用
> 实现状态：核心 Responses 链路已通过离线与受控真实测试；本 Stage 尚未达到全部退出门，详见 [实现状态](./implementation-status.md)

## 1. Stage 目标

Stage 1 要交付一个即使完全断网、没有任何真实模型账号，也能通过确定性模拟完成端到端验收的后端。它不是“先做一个反向代理”，而是先冻结后续所有 Adapter 都必须遵守的四个核心契约：

1. OpenAI 兼容请求与响应；
2. 流式运行与取消状态机；
3. Model Deployment 与客户端授权；
4. usage、可选预算、主链结算和 quota bucket。

Stage 1 完成后，Codex 与 WorkBuddy 只能作为新的 Provider Adapter 接入，不能各自发明模型 ID、用量字段、价格字段或错误语义。

## 2. 交付范围

实现以下接口：

- `POST /v1/responses`：文本、SSE、`instructions`、`max_output_tokens`、`reasoning.effort`、单函数工具、结构化输出；
- `POST /v1/chat/completions`：文本和单函数工具的兼容投影；
- `GET /v1/models`：只返回当前客户端获权的 Deployment；
- `GET /healthz`、`GET /readyz`；
- `GET /llmserver/v1/capabilities`；
- `GET /llmserver/v1/requests/{id}`：只允许请求所属客户端读取；
- CLI：校验配置、生成/吊销客户端 Token、列出 Deployment、探测 Provider、查看脱敏请求结算。

首个 Standard API Adapter 支持 OpenAI Responses 风格上游。其他“OpenAI 兼容”供应商只有通过契约套件后才能配置为该协议，不能仅凭 URL 中存在 `/v1` 就假定兼容。

## 3. 固定数据模型

### 3.1 RunRequest

```go
type RunRequest struct {
    ID                 string
    IdempotencyKey     string
    ClientID           string
    DeploymentID       string
    UpstreamModel      string
    Instructions       string
    Input              []InputItem
    ReasoningEffort    string
    MaxOutputTokens    *int
    Tools              []FunctionTool
    ToolChoice         ToolChoice
    OutputSchema       json.RawMessage
    Budget             *BudgetPolicy
    ConfigRevision     string
    PriceRevision      string
    UsageEstimator     string
    Deadline           time.Time
}
```

入口层必须在创建 Run 前完成 body 大小、字段、客户端、Deployment、能力和预算参数校验。Adapter 看不到客户端 Token 和原始 HTTP 对象。

### 3.2 GatewayEvent

最小事件集合：

```text
request.accepted
budget.evaluated
provider.selected
quota.before_observed
provider.started
output_text.delta
output_text.completed
function_call.delta
function_call.completed
usage.updated
provider.warning
provider.completed
provider.failed
provider.cancelled
quota.after_observed
settlement.completed
response.completed
```

事件包含 request ID、单调 `seq`、时间、Deployment、Provider、requested/upstream/effective model。事件数据结构不可直接嵌入供应商私有对象；原始对象只可作为受限诊断 fixture 保存。

### 3.3 持久化表

Stage 1 最小 SQLite 表：

```text
config_revisions
providers
model_deployments
price_revisions
upstream_cost_records
client_credentials
client_policies
runs
run_usage
run_charges
run_quota_observations
idempotency_keys
compatibility_runs
```

`runs` 先写入 accepted 状态再调用上游。usage 与 charge 在同一数据库事务中确定结算终态。SSE 已发送但数据库暂时失败时，服务不能伪造 `confirmed`；在可恢复重试前保持 `settling`，超过截止时间后标记 `unconfirmed`。

## 4. Model Deployment 与配置

配置文件使用单一声明式格式，建议 YAML。敏感值只引用 Keychain 条目，不直接写在文件中。

```yaml
providers:
  - id: openai-main
    type: openai_responses
    base_url: https://api.openai.com/v1
    secret_ref: keychain://llmserver/openai-main
    connect_timeout: 10s
    total_timeout: 180s

prices:
  - revision: api-terra-price-1
    currency: USD
    source: manual_override # 仅后台可见，不进入调用方响应
    per_million:
      input: "2.000000"
      output: "12.000000"

deployments:
  - id: api-terra
    provider: openai-main
    upstream_model: gpt-5.6-terra
    price_revision: api-terra-price-1
    usage_fallback:
      algorithm: text_estimator_v1
      image_equivalent_tokens: null
    enabled: true
    capabilities:
      streaming: native
      function_tools: native
      structured_output: native
```

示例金额只说明格式，不是程序内置价格。每个启用的 Deployment 必须解析出输入价和输出价。价格来源的内部优先级固定为 `manual_override > catalog_default`；控制台一旦手工修改就建立新的不可变 revision 并锁定该 Deployment，后台同步不得覆盖。供应商建议价只能进入后台参考记录，被明确采纳并生成 PriceRevision 后才可能生效。配置激活前必须验证 ID 唯一、引用存在、currency 一致、费率非负、客户端 allowlist 有效、Deployment 能力不超过 Provider 已验证能力。

供应商自己返回的价格或成本保存到 `upstream_cost_records`，只供后台统计和控制台展示。它不参与 `run_charges`，也不进入调用方响应。

`text_estimator_v1` 在上游缺失输入或输出 Token 时逐维度兜底：CJK 字符按每字 1 Token，其他 Unicode 字符按每 4 字符 1 Token 向上取整，非空最低为 1。输入统计规范化后的 instructions、消息、函数和 schema 文本；输出统计最终文本和函数参数 JSON。图片等非文本输入只有配置固定等价 Token 后才能估算。

`/v1/models` 的每条 `id` 是 Deployment ID。Provider 的发现列表通过 CLI 管理命令查看，不自动转成 Deployment。

## 5. 预算与结算主链

### 5.1 接单顺序

```text
authenticate client
→ resolve deployment and fixed revisions
→ validate capability
→ count/estimate input meters
→ evaluate optional budget
→ persist accepted run and idempotency key
→ observe quota before
→ invoke provider and consume events
→ aggregate final usage
→ observe quota after
→ calculate and persist settlement
→ attach billing and complete response
```

没有 budget 的请求跳过限制判断，但不跳过 usage 和 settlement。

### 5.2 hard budget

只有下列条件全部成立才接受 `mode=hard`：

- 当前价格 revision 同时包含输入价和输出价；
- 输入计量可确定，或使用一个不会低估的上界；
- 客户端提供有效 `max_output_tokens`；
- Adapter 能把输出上限传给上游并确保所有计费 Token 都进入最终 input/output usage；
- 不存在无法计数或无法限制的内部生成。

计算 `maximum_possible_charge` 后，若大于 `max_charge`，返回 `budget_exceeded_before_start`，不得调用 Provider。任一条件不成立则返回 `hard_budget_not_enforceable`。Stage 1 不实现“先开始再看看能否守住”的伪 hard。

### 5.3 soft budget

soft 模式记录 `estimated_charge`，在收到可用 usage update 时重算当前价格，达到阈值即发起取消。由于上游 usage 和取消可能滞后，最终总价允许超过上限，但必须返回：

```json
{
  "mode": "soft",
  "max_charge": "0.050000",
  "estimated_before_start": "0.031200",
  "exceeded": true,
  "overshoot": "0.002100",
  "enforcement": "best_effort"
}
```

### 5.4 结算状态

结算状态固定为：

- `confirmed`：输入、输出公开计费 Token 均已确定且价格 revision 有效；Token 可以来自上游，也可以来自固定版本估算器；
- `unconfirmed`：调用已发生，但上游值和兜底估算都无法形成某个计费维度；该状态只能进入后台记录，不能作为成功响应返回；
- `not_chargeable`：调用在接触 Provider 前失败；
- `failed`：内部定价或持久化不变量被破坏，响应不得作为成功完成。

失败请求可能仍有已发生的上游用量，因此请求状态与结算状态分开保存。不能使用 HTTP 状态码推断价格为零。

## 6. 兼容响应

默认 llmServer 模式在非流式响应附加：

```json
{
  "llmserver_billing": {
    "request_id": "req_...",
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
    "budget": null
  }
}
```

调用方看不到 `source`、手工锁定状态、供应商成本、Token 估算来源或任何“模拟定价”标记。`price_version` 是不透露来源的审计标识。后台 `run_usage` 仍要按 input/output 保存 `provider_reported|estimated_v1`、估算器版本和原始字符数。标准 OpenAI `usage` 与 `llmserver_billing.usage` 必须使用同一组 billable input/output Token。

额度字段由客户端 Key 策略控制，默认完全省略。启用后才在 `llmserver_billing` 中增加 `quota_status` 和 `quota_observations`。

SSE 在标准结束事件之后、`[DONE]` 之前输出一个命名明确的 `llmserver.billing.completed` 事件。严格兼容模式不发送扩展对象和自定义事件，但必须返回 request ID 响应头，结算可通过查询端点读取。

Stage 1 必须用官方 SDK 的至少两个版本做解析契约测试；测试目标是“标准字段可正常消费”，不是声称所有 OpenAI SDK 功能都已实现。

## 7. Standard API Adapter

Adapter 职责：

- 构造最小上游请求，过滤 hop-by-hop 和未允许头部；
- 原生透传或增量解析 SSE，不等待完整生成；
- 保留上游 request ID、effective model、usage 与 error category；
- 客户端取消时关闭上游 body 并取消 context；
- 遵守连接、首字节、空闲事件和总执行四类超时；
- 对 Base URL 做 scheme、host 和内网地址策略校验，防止配置型 SSRF；
- 不自动跨 Provider 或跨 Deployment 重试。

只有在未收到任何输出、未产生可计费生成且错误明确可重试时，才允许对同一上游做一次连接级重试。重试事实写入 Run，usage 汇总不能重复。

## 8. 客户端鉴权与局域网

客户端 Token 至少 256 bit 随机熵，只在创建时显示一次，数据库保存带盐摘要。每个 Token 策略包括：

- Deployment allowlist；
- 每分钟请求数、最大并发、队列上限；
- 单请求 body、输入、输出、工具数、总时长上限；
- 是否允许图片、工具、strict 模式；
- `include_quota_observations`，默认 `false`，单次请求不能覆盖；
- 可选来源 IP 和到期时间。

一个物理设备应使用一个独立客户端 Token。未来控制台按 Token/设备修改返回策略；当前由配置文件和 CLI 使用同一策略接口管理。

API 可在可信隔离 LAN 监听，但必须启用客户端 Token；当前版本没有内建 TLS，跨不可信网络应由前置 HTTPS/mTLS、VPN 或零信任入口保护。管理 API 始终强制回环监听，不开放 LAN 管理端点。

## 9. 实现切片

### Slice 1：运行骨架

建立 Go module、配置校验、SQLite migration、request ID、状态机、Mock Adapter、health/readiness。退出条件是 Mock Adapter 能完成非流式文本及失败状态持久化。

### Slice 2：Responses 与 SSE

实现 `/v1/responses`、统一事件、SSE、非流式聚合、断连取消和慢消费者限制。退出条件是相同 fixture 的流式/非流式文本与 usage 一致。

### Slice 3：Deployment 与鉴权

实现客户端 Token、策略、Deployment、能力矩阵和 `/v1/models`。退出条件是两个客户端看到不同模型集合，越权调用被拒绝且无上游事件。

### Slice 4：定价、预算与幂等

实现定点金额、price revision、手工改价锁定、hard/soft budget、结算扩展、幂等键和崩溃恢复。退出条件是金额 property tests、价格优先级、并发幂等和预算边界全部通过。

### Slice 5：Standard API Adapter

实现上游 HTTP/SSE、Keychain 引用、错误映射、取消和 fixture server。退出条件是本地假 OpenAI Server 的全部协议场景通过，不要求真实 API Key。

### Slice 6：Chat Completions 与发布骨架

实现 Chat Completions 投影、管理 CLI、日志轮转与脱敏。常驻骨架当前已使用 `launchctl submit` 脚本实现，不创建第三个配置文件；Chat Completions 仍未实现。最终退出条件是安装、启动、重启、数据库恢复和卸载流程可重复。

## 10. 自动测试矩阵

必须覆盖：

- Responses 与 Chat Completions 的文本、SSE、单函数、结构化输出；
- 上游 cached/reasoning 细分不会重复加价、缺失 usage 的逐维度估算、重复 usage update；
- `text_estimator_v1` 的中文、英文、混合文本、空文本、函数 JSON 和版本切换固定向量；
- 供应商价格变化不影响公开价格，手工改价优先且不会被同步覆盖；
- 十进制定点舍入、极大 token、零费率、price revision 切换；
- 无预算、hard 刚好等于上限、超过一个最小金额单位、不可执行 hard；
- soft 取消及时、取消滞后和实际超额；
- quota 默认不返回、按不同客户端 Key 开关、零/单/多 bucket、单位不同、before 缺失；
- 相同幂等键并发到达、完成后重试、运行中重试、不同客户端同键；
- Provider 在首字节前失败、输出后失败、usage 后崩溃、结算写盘失败；
- 客户端断连、慢消费者、超大事件、超时；
- Token 吊销、模型越权、LAN 无 TLS、SSRF Base URL、日志 secret 扫描；
- 两个客户端并发时上下文、预算、usage、billing 完全隔离。

测试使用本地脚本化 Provider，可按 fixture 精确指定事件和虚拟时钟。禁止让单元测试依赖公网、真实账号或供应商当前价格。

## 11. Stage 1 退出门

满足全部条件才进入 Stage 2：

- 所有公共请求经同一 Run/事件/结算主链；
- `/v1/models` 只公开显式 Deployment；
- 每个成功请求都有 confirmed 结算对象；usage 缺失时使用版本化估算，只有两者都无法确定才失败；
- 无预算时按本次 billable usage 结算，hard/soft 行为与文档一致；
- 金额不使用浮点数，历史请求可凭 price revision 完整复算；
- 每个启用模型都有输入/输出公开价，手工修改优先级最高；
- 调用方看不到价格来源、模拟方式或供应商实际成本；
- 配额支持数组、多窗口和多单位，按客户端 Key 控制且默认不返回；
- SSE 断连可取消 Mock 与 Standard API Adapter；
- 幂等重试不会重复启动 Provider Run；
- LAN 模式没有认证或 TLS 时拒绝启动；
- 全套测试完全离线运行；
- 文档明确写明未进行 Hominal 和真实供应商生成验收。
