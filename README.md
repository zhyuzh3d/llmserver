# llmserver

llmserver 是运行在本机的统一 OpenAI-compatible LLM API 网关。它把标准 Responses API、本机 Codex 登录额度和本机 WorkBuddy 登录额度收敛为同一套 `/v1/responses` 接口，并统一处理设备鉴权、模型白名单、Token 用量、公开价格、预算、幂等和 SQLite 结算。

当前版本已经适合本机回环地址上的开发和受控调用，但还不是可以直接裸露到局域网的发布版。服务本身尚未提供 TLS、设备速率限制和完整的操作系统级 agent 隔离；局域网使用必须放在 TLS 反向代理或其他受信任的安全入口之后。详细边界见[实现状态](./docs/implementation-status.md)。

## 当前能力

- `GET /healthz`、`GET /readyz`、`GET /v1/models`；
- `POST /v1/responses` 非流式和 SSE；
- 每个设备独立 Bearer Token、Deployment 白名单和可选额度返回策略；
- 标准 OpenAI-compatible Responses 上游；
- 当前 macOS 用户已登录的 Codex 和 WorkBuddy，不复制或下发登录凭据；
- 每个 Deployment 独立输入价、输出价和价格版本；
- 上游缺失 Token 时使用版本化字数估算，估算值直接参与公开结算；
- 可选请求预算、客户端范围幂等键、SQLite 原子结算；
- Codex 多窗口额度观测，默认不向调用方返回。

尚未实现 `/v1/chat/completions`、function tools、Web 控制台和完整管理 CLI。

## 环境要求

- macOS；标准 API 模式本身可移植，但 Codex/WorkBuddy Adapter 当前依赖 macOS 应用路径和用户登录态；
- Go 1.25 或兼容版本；
- 使用 Codex 时，ChatGPT/Codex 已由运行服务的同一个 macOS 用户登录；
- 使用 WorkBuddy 时，WorkBuddy 已由运行服务的同一个 macOS 用户登录；
- 标准 API 模式需要一个支持 Responses API 的上游地址和 API Key。

## 构建

```bash
git clone git@github.com:zhyuzh3d/llmserver.git
cd llmserver
go build -o dist/llmserver ./cmd/llmserver
```

运行测试：

```bash
go test ./...
go test -race ./...
go vet ./...
```

## 配置

仓库提供两种示例配置：

- [`configs/llmserver.example.yaml`](./configs/llmserver.example.yaml)：当前项目环境使用，从上层 `xconfig.yaml` 的 `llm` 节点导入标准 API，再增加本机 Codex/WorkBuddy；
- [`configs/llmserver.standalone.example.yaml`](./configs/llmserver.standalone.example.yaml)：独立部署模板，标准 API Key 从环境变量读取。

先复制为 Git 已忽略的本地配置：

```bash
cp configs/llmserver.standalone.example.yaml llmserver.local.yaml
```

修改 Provider 地址、上游模型、允许的 Deployment 和公开价格。任何真实密钥都不要写入 YAML。配置中的 `api_key_env` 和 `token_env` 填的是环境变量名称，不是密钥值。

每台设备推荐使用独立环境变量，例如：

```yaml
clients:
  - id: ipad-study
    token_env: LLMSERVER_TOKEN_IPAD_STUDY
    allowed_deployments: [api-model, codex-luna]
    include_quota_observations: false
  - id: macbook-dev
    token_env: LLMSERVER_TOKEN_MACBOOK_DEV
    allowed_deployments: [api-model, codex-luna, workbuddy-hy4-preview]
    include_quota_observations: true
```

价格固定绑定到公开 Deployment。调用方不会看到 `manual_override` 等内部价格来源；上游返回的成本也不会改变公开结算。

## 启动

为客户端 Token 和上游 Key 设置环境变量。示例中的命令不会把值写入项目：

```bash
export LLMSERVER_CLIENT_TOKEN="$(openssl rand -hex 32)"
export UPSTREAM_LLM_API_KEY='replace-with-real-upstream-key'
./dist/llmserver -config llmserver.local.yaml
```

使用当前项目的 `xconfig.yaml` 导入模式：

```bash
export LLMSERVER_CLIENT_TOKEN="$(openssl rand -hex 32)"
go run ./cmd/llmserver -config configs/llmserver.example.yaml
```

默认监听 `127.0.0.1:4815`。检查状态：

```bash
curl http://127.0.0.1:4815/healthz
curl http://127.0.0.1:4815/readyz
```

离线模式不会调用真实上游：

```bash
export LLMSERVER_CLIENT_TOKEN='local-offline-test-token'
go run ./cmd/llmserver \
  -config configs/llmserver.example.yaml \
  -mock-response 'offline ok'
```

## 调用

```bash
curl http://127.0.0.1:4815/v1/responses \
  -H "Authorization: Bearer $LLMSERVER_CLIENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"codex-luna",
    "input":"只回复 OK",
    "max_output_tokens":32
  }'
```

成功响应包含标准 `usage` 和扩展的 `llmserver_billing`：输入/输出 Token、单价、输入费用、输出费用、总费用和结算状态。需要严格标准响应时可发送 `x-llmserver-compatibility: strict`，但该模式会省略扩展结算字段。

完整的客户端接入、流式调用、预算、幂等、设备策略和局域网建议见 [`GUIDE.md`](./GUIDE.md)。

## 凭据与 Git 安全

- 独立部署的 API Key 和所有设备 Token 只从环境变量读取；兼容 `xconfig.yaml` 的模式会读取其 `llm` 凭据节点，因此该文件必须位于仓库外并保持 Git 忽略；
- Codex/WorkBuddy 由官方本机进程使用当前 macOS 用户的登录态；
- 不要复制 `.codex`、`.codebuddy`、`auth.json`、`credentials.json` 或 `xconfig.yaml` 到仓库；
- `.env`、本地 YAML、数据库、二进制和常见凭据文件已加入 `.gitignore`；
- 提交前仍应运行组织自己的 secret scanner，不能把 `.gitignore` 当成唯一防线。

## 文档

- [客户端接入指南](./GUIDE.md)
- [后端产品与技术总纲](./docs/llm-server-product-design.md)
- [Stage 1：标准 API 与结算内核](./docs/stage-1-standard-api.md)
- [Stage 2：Codex Adapter](./docs/stage-2-codex.md)
- [Stage 3：WorkBuddy Adapter](./docs/stage-3-workbuddy.md)
- [实现状态](./docs/implementation-status.md)
