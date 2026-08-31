# llmserver 接入指南

调用方只需要三个信息：服务器地址、分配给本设备的 Bearer Token，以及 `/v1/models` 返回的公开模型 ID。调用方不需要知道模型实际来自标准 API、Codex 还是 WorkBuddy。

## 获取地址和 Token

本机地址为 `http://127.0.0.1:4815`。局域网设备应把 `127.0.0.1` 替换为运行 llmserver 的 Mac 局域网 IP，端口保持 `4815`。

管理员在本机打开 `http://127.0.0.1:4816/admin/`，进入“访问密钥”，为每台设备创建独立 Token、勾选允许模型并保存。Token 是持久值，除非管理员点击“重新生成”或手工修改，否则服务重启不会改变它。

不要把 Token 放进 URL、Git 仓库、浏览器公开前端、客户端日志或崩溃报告。

## 健康检查和模型列表

```bash
export LLMSERVER_BASE_URL='http://192.168.1.20:4815'
export LLMSERVER_API_KEY='管理员分配的设备 Token'

curl "$LLMSERVER_BASE_URL/healthz"
curl "$LLMSERVER_BASE_URL/readyz"
curl "$LLMSERVER_BASE_URL/v1/models" \
  -H "Authorization: Bearer $LLMSERVER_API_KEY"
```

这里的环境变量只是客户端命令示例；llmserver 服务端不读取环境变量配置。`/v1/models` 只返回当前设备获准且已启用的公开模型。

## 非流式 Responses 请求

```bash
curl "$LLMSERVER_BASE_URL/v1/responses" \
  -H "Authorization: Bearer $LLMSERVER_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"codex-luna",
    "instructions":"回答简洁准确",
    "input":"解释什么是幂等请求",
    "max_output_tokens":256
  }'
```

成功响应包含 OpenAI Responses 风格结果和 `llmserver_billing`：

```json
{
  "id": "resp_...",
  "object": "response",
  "status": "completed",
  "model": "codex-luna",
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
    "price_version": "...",
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

公开费用只按管理台配置的模型输入/输出单价计算。供应商实报成本不进入这个字段。若供应商缺少输入或输出 Token，llmserver 会用字符数近似估算，该估算量就是本次公开计费量。

## SSE 流式请求

```bash
curl -N "$LLMSERVER_BASE_URL/v1/responses" \
  -H "Authorization: Bearer $LLMSERVER_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"workbuddy-hy4-preview","input":"写一句问候语","stream":true}'
```

主要事件顺序：

```text
response.created
response.output_text.delta
llmserver.billing.completed
response.completed
[DONE]
```

只有收到 `llmserver.billing.completed`，或最终响应中的 `settlement_status=confirmed`，才能把金额视为确认结算。连接中断后应使用同一幂等键重试，不要自行假定费用为零。

## OpenAI SDK

Python：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://192.168.1.20:4815/v1",
    api_key="设备 Token",
)

response = client.responses.create(
    model="codex-luna",
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
  apiKey: "设备 Token",
});

const response = await client.responses.create({
  model: "codex-luna",
  input: "只回复 OK",
  max_output_tokens: 32,
});
console.log(response.output_text);
```

部分 SDK 会丢弃未知扩展字段。业务若必须读取 `llmserver_billing`，应确认 SDK 保留原始响应，或直接使用 HTTP/JSON。

## 可选预算

不设置预算时，服务照常按本次实际公开计费量结算。预算不是预付金额：

```json
{
  "model": "luna",
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

`hard` 只用于能够在调用前约束最大输出量的标准 API Deployment，并要求 `max_output_tokens`。Codex/WorkBuddy 不保证硬上限。`soft` 只在结算后标记是否超额，不能保证中途停止。

## 幂等重试

```json
{
  "model": "codex-luna",
  "input": "只回复 OK",
  "llmserver": {
    "idempotency_key": "device-operation-20260831-0001"
  }
}
```

幂等键按设备隔离。相同 Key 和相同请求会返回已确认结果，不再次调用上游；相同 Key 用于不同请求返回 `409`。

## 额度观测

额度信息由管理员按设备统一开关，调用方不能单次请求自行开启。开启后，Codex 响应可能附带多个共享账号限额窗口的调用前百分比、调用后百分比和变化量。它们不是本请求的精确美元成本，也不能跨不同窗口直接相加。

WorkBuddy 当前实报总美元成本但不提供积分余额接口。后台手工设置的积分费率属于“配置估算”，不会作为供应商实报额度返回。

## 管理与用量查询

“概览”和“供应商”的模型设置弹窗是模型发布的唯一管理入口。取消某个模型的“启用”并保存后，新请求会立即停止接受该公开模型；在途请求继续使用开始时的配置快照。供应商卡片显示的 Key 只有首尾各四位可见，复制动作通过本机受保护的管理接口取得完整值，普通状态接口不会返回完整秘密。

“平台消耗”和“用户消耗”共享 1 小时、1 天、7 天及自定义小时/天时间窗。前者按供应商汇总，后者按访问密钥汇总；两者都列出有消耗模型的平台价金额、实际供应商实报或配置估算，以及可用的额度变化。平台价与实际供应商消耗始终分栏展示，不能相互替代。

## 严格兼容模式

发送 `x-llmserver-compatibility: strict` 会省略 `llmserver_billing` 和自定义 SSE 结算事件，但仍返回 `x-llmserver-request-id`。需要显示费用的客户端不要启用该模式。

## 常见错误

- `401 invalid_api_key`：设备 Token 缺失或错误；
- `403 model_not_allowed`：设备未获准使用该公开模型；
- `404 unknown_model_deployment`：模型不存在或被禁用；
- `409 idempotency_key_reused` / `idempotency_in_progress`：幂等冲突；
- `422 hard_budget_not_enforceable`：该模型无法执行 hard budget；
- `502 provider_start_failed` / `provider_stream_failed`：供应商、本机登录态或程序版本异常。

局域网 HTTP 没有加密能力，只适合可信隔离网络。跨不可信网络必须增加 HTTPS/mTLS、VPN 或零信任入口。
