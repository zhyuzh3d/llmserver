# llmserver 接入指南

本文只面向调用 llmserver API 的应用或局域网设备。开始接入前，请取得服务地址、Bearer Token 和可用的公开模型 ID。

## 1. 确定服务地址

本机调用使用：

```text
http://127.0.0.1:4815
```

局域网设备需要把 `127.0.0.1` 替换为服务器 Mac 的局域网 IP，例如 `http://192.168.1.20:4815`。先在调用设备上检查：

```bash
curl -fsS http://192.168.1.20:4815/healthz
curl -fsS http://192.168.1.20:4815/readyz
```

预期分别返回 `{"status":"ok"}` 和 `{"status":"ready"}`。`healthz` 表示服务可访问，`readyz` 表示服务已可接收模型请求；具体模型能否调用仍应以实际请求结果为准。

以下环境变量仅用于简化调用示例：

```bash
export LLMSERVER_BASE_URL='http://192.168.1.20:4815'
export LLMSERVER_API_KEY='你的访问 Token'
```

不要把 Token 放进 URL、Git 仓库、网页前端、共享脚本、访问日志或崩溃报告。

## 2. 获取可用模型

```bash
curl "$LLMSERVER_BASE_URL/v1/models" \
  -H "Authorization: Bearer $LLMSERVER_API_KEY"
```

示例响应：

```json
{
  "object": "list",
  "data": [
    {"id": "example-model", "object": "model", "created": 0, "owned_by": "llmserver"}
  ]
}
```

列表只包含当前 Token 可以使用的公开模型。调用时必须使用这里返回的 `id`。

## 3. 非流式 Responses 请求

```bash
curl "$LLMSERVER_BASE_URL/v1/responses" \
  -H "Authorization: Bearer $LLMSERVER_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "example-model",
    "instructions": "回答简洁准确",
    "input": "解释什么是幂等请求",
    "max_output_tokens": 256
  }'
```

稳定支持的请求字段是：

- `model`：必填，公开模型 ID；
- `input`：必填，推荐使用字符串；
- `instructions`：可选，推荐使用字符串；
- `stream`：可选，默认 `false`；
- `reasoning.effort`：可选推理强度；是否支持、允许值及是否能真正降低思考由模型决定。`codex-luna`、`codex-terra`、`codex-sol` 支持显式传入 `none`；省略时使用服务端配置的默认档位。调用方不掌握其他模型能力时应省略，不要默认填写 `low`；
- `max_output_tokens`：可选正整数；部分模型不保证严格限制，要求价格硬上限时应同时使用 `hard` 预算；
- `store`：只能省略或设为 `false`，`true` 会被拒绝；
- `llmserver`：可选的预算与幂等扩展。

工具调用尚不支持。未在本指南列出的 Responses 字段不能作为稳定契约。

每个请求都是独立、无状态的。统一接口不承诺 `session_id`、`previous_response_id` 或服务端对话历史续接；即使个别上游偶然接受额外字段，也不能作为跨模型契约。多轮应用必须在自己的存储中保留必要历史，并在下一次 `input` 中重新提交。不要假设相同访问 Token、相同模型或短时间内连续调用会自动进入同一对话。

成功响应包含 OpenAI Responses 风格结果、标准 `usage` 和 `llmserver_billing`：

```json
{
  "id": "resp_...",
  "object": "response",
  "status": "completed",
  "model": "example-model",
  "output": [{
    "type": "message",
    "role": "assistant",
    "status": "completed",
    "content": [{"type": "output_text", "text": "..."}]
  }],
  "usage": {
    "input_tokens": 100,
    "output_tokens": 20,
    "total_tokens": 120
  },
  "llmserver_billing": {
    "request_id": "req_...",
    "settlement_status": "confirmed",
    "price_version": "example-model-public-v1",
    "currency": "USD",
    "usage": {"input_tokens": 100, "output_tokens": 20},
    "unit_prices": {
      "input_per_million": "0.200000000",
      "output_per_million": "1.200000000"
    },
    "charges": {
      "input": "0.000020000",
      "output": "0.000024000",
      "total": "0.000044000"
    }
  }
}
```

调用费用以响应中的 `usage`、`unit_prices` 和 `charges` 为准。金额字段是十进制字符串，调用方应使用十进制定点数或 Decimal 类型，不要转成二进制浮点数后参与账务累计。若服务无法取得输入或输出 Token 数量，会使用近似值完成计费，因此调用方不应自行重新计算 Token 后替换服务返回的账单。

部分推理模型会把不可见推理计入 `output_tokens`，所以输出 Token 数量可能明显大于可见文本对应的 Token 数量。这不表示响应重复或计费错误；仍应以确认账单为准。`max_output_tokens` 对不支持硬预算的模型只是请求参数，不保证限制隐藏推理、可见输出或实际费用。

同时保存响应头 `x-llmserver-request-id`，它与 `llmserver_billing.request_id` 对应，适合故障排查和账单对照。

## 4. SSE 流式请求

```bash
curl -N "$LLMSERVER_BASE_URL/v1/responses" \
  -H "Authorization: Bearer $LLMSERVER_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"example-model","input":"写一句问候语","stream":true}'
```

主要事件顺序：

```text
response.created
response.output_text.delta
llmserver.billing.completed
response.completed
[DONE]
```

客户端应按 SSE 帧解析 `event:` 和 `data:`，不要把网络分块当作事件边界。只有收到 `llmserver.billing.completed`，或最终 `response.completed` 内 `response.llmserver_billing.settlement_status=confirmed`，才能把金额视为确认结算。

`response.created` 只表示请求已进入流式处理，不表示模型已经产生可见答案。流式请求建立后 HTTP 状态通常已经是 `200`；后续模型失败会通过 `event: error` 报告，因此客户端必须同时处理 `response.completed`、`error`、连接中断和 `[DONE]`，不能只检查 HTTP 状态。

连接在结算前中断时，服务可能已经调用上游但尚未确认本地结算。不要自行认定费用为零；使用预先设置的同一幂等键重试并保存请求 ID。

模型首个可见文本的等待时间可能远大于建立 SSE 连接的时间，尤其是推理模型。客户端应分别设置合理的连接超时和完整请求超时，不要把“数秒内没有文本 delta”直接当成网络断线；需要取消时应主动关闭请求连接。

## 5. OpenAI SDK

Python：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://192.168.1.20:4815/v1",
    api_key="你的访问 Token",
)

response = client.responses.create(
    model="example-model",
    input="只回复 OK",
    max_output_tokens=32,
)
print(response.output_text)
```

JavaScript：

```javascript
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://192.168.1.20:4815/v1",
  apiKey: "你的访问 Token",
});

const response = await client.responses.create({
  model: "example-model",
  input: "只回复 OK",
  max_output_tokens: 32,
});
console.log(response.output_text);
```

不同 SDK 版本对未知扩展字段的保留方式不同。业务若必须读取 `llmserver_billing`，应验证所用 SDK 能访问原始响应；不能确认时直接使用 HTTP/JSON。

SDK 的自动重试必须与业务幂等键一起使用。没有 `llmserver.idempotency_key` 时，超时后由 SDK 自动重发可能产生第二次真实模型调用和第二笔费用。

## 6. 可选价格上限

预算不是预付金额，也不是每次请求的必填项。不设置预算时，服务仍按最终计费 Token 正常结算。

软预算示例：

```json
{
  "model": "example-model",
  "input": "...",
  "llmserver": {
    "budget": {
      "mode": "soft",
      "currency": "USD",
      "max_charge": "0.010000000"
    }
  }
}
```

`soft` 不会提前阻止或中断请求，只会在结算结果的 `budget.exceeded` 中标记是否超额。

硬预算示例：

```json
{
  "model": "example-model",
  "input": "...",
  "max_output_tokens": 256,
  "llmserver": {
    "budget": {
      "mode": "hard",
      "currency": "USD",
      "max_charge": "0.010000000"
    }
  }
}
```

`hard` 必须同时给出 `max_output_tokens`，并且只能用于支持硬预算的模型。调用前检查超过上限时返回 HTTP `402`；模型不支持硬预算时返回 `422 hard_budget_not_enforceable`。

预算应使用模型公开价格的币种，金额应为非负十进制字符串；硬预算会拒绝币种不一致的请求。

## 7. 幂等重试

```json
{
  "model": "example-model",
  "input": "只回复 OK",
  "llmserver": {
    "idempotency_key": "device-operation-20260831-0001"
  }
}
```

幂等键最长 256 个字符，并按访问密钥隔离：

- 相同访问密钥、相同幂等键和完全相同请求：已确认完成时返回原结果，不再次调用上游；
- 相同幂等键用于不同请求：返回 `409 idempotency_key_reused`；
- 原请求尚未确认完成：返回 `409 idempotency_in_progress`。

调用方应在首次发送前生成业务级幂等键，并在超时、断线和不确定结果重试时保持请求体不变。

## 8. 严格兼容模式

如果客户端不能接受自定义响应字段，可发送：

```text
x-llmserver-compatibility: strict
```

严格模式会省略 `llmserver_billing` 和自定义 SSE 结算事件，但仍返回 `x-llmserver-request-id`。需要展示费用或判断结算状态的客户端不应启用严格模式。

## 9. 常见错误

| HTTP | code | 含义 |
| --- | --- | --- |
| 400 | `invalid_json` / `missing_model` / `missing_input` | 请求格式或必填字段错误 |
| 400 | `invalid_reasoning` / `invalid_budget` / `invalid_max_output_tokens` | 推理、预算或输出上限格式错误 |
| 400 | `unsupported_feature` | 使用了工具或 `store=true` 等未支持能力 |
| 400 | `unsupported_reasoning_effort` | 当前模型不支持请求的推理强度 |
| 401 | `invalid_api_key` | Token 缺失、错误或已被替换 |
| 402 | `budget_exceeded_before_start` | 硬预算调用前检查未通过 |
| 402 | `daily_quota_exceeded` | 访问 Token 当日可用的平台价额度已经用完 |
| 403 | `model_not_allowed` | 此 Token 未获准使用该模型 |
| 404 | `unknown_model_deployment` | 公开模型不存在或已停用 |
| 409 | `idempotency_key_reused` / `idempotency_in_progress` | 幂等冲突或原请求未完成 |
| 413 | `request_too_large` | 请求体超过服务限制 |
| 422 | `hard_budget_not_enforceable` | 当前模型无法执行硬预算 |
| 502 | `provider_start_failed` / `provider_stream_failed` | 模型执行端启动失败或流式响应异常 |
| 503 | `not_ready` | 服务运行时尚未就绪 |

流式请求中，模型开始执行后的错误通常通过 SSE `event: error` 返回，而不是改写已经发送的 HTTP 200 状态。收到 `error` 或连接提前结束且没有 `response.completed` 时，应把结果视为未确认，并按幂等重试规则处理。

在 SSE 开始前被拒绝的 HTTP 请求使用 `{"error": {...}}` 结构；SSE 开始后的错误使用 `event: error`。若响应头带有 `x-llmserver-request-id`，排障时应一并提供，但不要提供访问 Token。

## 10. 网络安全

如果服务地址使用 HTTP，Bearer Token 会以明文方式传输，只能在可信、隔离的网络中使用。跨不可信网络时，应使用服务方提供的 HTTPS、VPN 或其他安全入口。
