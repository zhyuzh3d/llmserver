# llmserver 客户端接入指南

本文面向需要从本机或局域网设备调用 llmserver 的开发者。调用方只需要知道服务地址、设备 Token 和允许使用的公开模型 ID，不需要理解上游是标准 API、Codex 还是 WorkBuddy。

## 1. 接入前准备

服务端管理员需要提供：

- API Base URL，例如 `http://127.0.0.1:4815` 或受 TLS 保护的局域网地址；
- 分配给当前设备的 Bearer Token；
- 当前设备允许使用的模型列表；
- 是否允许返回订阅额度观测。

设备 Token 应一机一个。不要把 Token 放在 URL、客户端日志、Git 仓库或前端公开源码中。

## 2. 健康检查和模型列表

健康检查不需要鉴权：

```bash
curl "$LLMSERVER_BASE_URL/healthz"
curl "$LLMSERVER_BASE_URL/readyz"
```

查询当前设备可用模型需要 Bearer Token：

```bash
curl "$LLMSERVER_BASE_URL/v1/models" \
  -H "Authorization: Bearer $LLMSERVER_API_KEY"
```

`/v1/models` 只返回当前设备策略允许且已启用的 Deployment。客户端应使用这里返回的 `id`，不能直接使用供应商内部模型名。

当前示例配置包括：

- `luna`、`terra`、`sol`：上层 `xconfig.yaml` 导入的标准 API Deployment；
- `codex-luna`、`codex-terra`、`codex-sol`：当前用户 Codex 登录态；
- `workbuddy-hy4-preview`：当前用户 WorkBuddy 登录态。

实际列表以 `/v1/models` 为准。

## 3. 非流式 Responses 请求

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

`input` 可以是字符串，也可以是合法 JSON 结构。当前版本支持文本输出，不支持 function tools；`store=true` 会被拒绝。

典型成功响应：

```json
{
  "id": "resp_...",
  "object": "response",
  "status": "completed",
  "model": "codex-luna",
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "status": "completed",
      "content": [{"type": "output_text", "text": "..."}]
    }
  ],
  "usage": {
    "input_tokens": 100,
    "output_tokens": 20,
    "total_tokens": 120
  },
  "llmserver_billing": {
    "request_id": "req_...",
    "settlement_status": "confirmed",
    "price_version": "price_...",
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

公开费用始终按 Deployment 配置的输入/输出价格和本次公开计费 Token 计算。供应商自己的成本字段不会改变响应费用。供应商没有报告某一维度 Token 时，llmserver 使用字数估算，估算值直接成为该次公开计费量。

## 4. SSE 流式请求

在请求体中设置 `"stream": true`：

```bash
curl -N "$LLMSERVER_BASE_URL/v1/responses" \
  -H "Authorization: Bearer $LLMSERVER_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"workbuddy-hy4-preview",
    "input":"写一句问候语",
    "stream":true
  }'
```

事件顺序通常为：

```text
response.created
response.output_text.delta (零到多次)
llmserver.billing.completed
response.completed
[DONE]
```

客户端只有收到 `llmserver.billing.completed` 或最终 `response.completed` 中的 `settlement_status=confirmed`，才能把价格视为确认结算。连接中断时不要自行假定费用为零；可以用同一幂等键重试查询已完成结果。

## 5. OpenAI SDK 接入

Python：

```python
import os
from openai import OpenAI

client = OpenAI(
    base_url=os.environ["LLMSERVER_BASE_URL"] + "/v1",
    api_key=os.environ["LLMSERVER_API_KEY"],
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
  baseURL: `${process.env.LLMSERVER_BASE_URL}/v1`,
  apiKey: process.env.LLMSERVER_API_KEY,
});

const response = await client.responses.create({
  model: "codex-luna",
  input: "只回复 OK",
  max_output_tokens: 32,
});
console.log(response.output_text);
```

某些 SDK 会丢弃未知扩展字段。业务若必须读取 `llmserver_billing`，建议直接使用 HTTP/JSON，或确认所用 SDK 能保留原始响应。

## 6. 幂等键

网络超时后重试时，在私有 `llmserver` 字段中携带稳定幂等键：

```json
{
  "model": "codex-luna",
  "input": "只回复 OK",
  "llmserver": {
    "idempotency_key": "device-operation-20260830-0001"
  }
}
```

幂等键在单个客户端设备范围内生效。相同 Key 和相同请求会返回已确认结果而不再次调用上游；相同 Key 用于不同请求会返回 `409 idempotency_key_reused`。未完成或失败的原请求不会被服务器偷偷重放。

## 7. 请求预算

预算可选，不设置时按实际公开计费量正常结算：

```json
{
  "model": "api-model",
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

- `hard`：只在 Deployment 明确声明可执行时支持；调用前按输入估算和最大输出量检查。Codex/WorkBuddy 当前明确不支持 hard budget；
- `soft`：最终结算会标记是否超过预算，但当前版本不保证中途停止，可能 overshoot；
- `max_charge` 不是预付金额，也不是固定收费。最终费用始终按实际公开计费量计算。

## 8. 额度观测

额度返回由服务端针对设备统一配置：

```yaml
include_quota_observations: true
```

调用方不能在单次请求里自行开启。关闭时响应完全省略 `quota_observations`。开启后，Codex 可能返回多个相互独立的百分比窗口，包括调用前、调用后、变化量、窗口长度和重置时间。

这些值是共享账号快照：桌面端或其他请求可能同时消耗额度，所以 delta 不能被解释为本请求的精确成本，也不能换算成美元。WorkBuddy 当前没有可靠的公开额度读取路径，因此不会伪造额度字段。

## 9. 严格兼容模式

发送以下 Header 可获得不含 llmserver 扩展字段的 Responses 结果：

```text
x-llmserver-compatibility: strict
```

严格模式仍会返回 `x-llmserver-request-id`，但会省略 `llmserver_billing` 和对应 SSE 结算事件。需要展示费用的客户端不要启用严格模式。

## 10. 错误和重试

常见状态：

- `401 invalid_api_key`：设备 Token 缺失或错误；
- `403 model_not_allowed`：当前设备无权使用该 Deployment；
- `404 unknown_model_deployment`：模型不存在或被禁用；
- `409 idempotency_key_reused` / `idempotency_in_progress`：幂等冲突；
- `422 hard_budget_not_enforceable`：该模型不能保证 hard budget；
- `502 provider_start_failed` / `provider_stream_failed`：本机应用、登录态、版本或上游服务异常。

只应对确认未完成、且语义允许重试的请求进行重试。使用稳定幂等键；不要因为收到 5xx 就生成新 Key 并重复计费。

## 11. 局域网接入

当前服务没有内建 TLS。最安全的当前部署方式是：

```text
局域网设备 → HTTPS/mTLS 安全入口 → 127.0.0.1:4815 llmserver
```

保持 llmserver 监听回环地址，由同机反向代理、VPN 或零信任入口负责 TLS、来源限制和证书。不要把设备 Bearer Token 通过明文 HTTP 发送到普通 Wi-Fi 网络。

如果临时把 `server.listen` 改为 `0.0.0.0:4815`，这只适合完全隔离且可信的测试网络，不构成正式安全部署。当前 Codex/WorkBuddy Adapter 仍以当前 macOS 用户权限运行，LAN 发布前还需要更强的操作系统级文件、网络、Keychain 和 GUI 隔离。

## 12. 客户端安全清单

- 每台物理设备独立 Token，并只允许所需模型；
- Token 存放在系统 Keychain、受保护环境变量或后端 secret store；
- 不在浏览器公开前端、移动端日志或崩溃报告中打印 Token；
- 校验 HTTPS 证书，不关闭 TLS 验证；
- 保存 `x-llmserver-request-id` 和幂等键用于诊断；
- 金额使用响应中的十进制定点字符串，不转成二进制浮点数后再累计；
- 只把 `settlement_status=confirmed` 视为确认结算。
