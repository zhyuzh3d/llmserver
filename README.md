# llmserver

llmserver 是一套本机统一 OpenAI Responses API 网关。它把标准 API、本机 Codex 登录额度和本机 WorkBuddy 登录额度收敛到同一个 `/v1/responses` 主链，并统一处理设备 Token、模型白名单、Token 用量、公开价格、预算、幂等与 SQLite 结算。

调用方只会看到公开模型 ID 和后台配置的价格，不会知道某个模型来自标准 API、Codex 还是 WorkBuddy，也不会收到供应商凭据或本机登录凭据。

## 已实现

- `GET /healthz`、`GET /readyz`、`GET /v1/models`；
- `POST /v1/responses` 非流式与 SSE；
- 每个设备独立持久 Token、模型白名单和额度观测返回开关；
- 标准 Responses API、本机 Codex、本机 WorkBuddy 三类 Provider；
- 管理台内刷新 Provider 可用模型、勾选发布模型、修改公开价格与实际成本/积分估算；
- 每次响应返回输入量、输出量、输入费、输出费和总价；
- 上游缺少 Token 用量时，按字符数近似估算并直接作为本次公开计费量；
- 供应商实报成本、配置成本估算、Codex 额度窗口、公开定价消耗分别入库；
- 请求预算、客户端范围幂等、SQLite 结算与崩溃恢复；
- 本机管理页面和热更新配置；
- macOS 用户级常驻运行，进程异常退出后由 `launchd` 拉起，系统重启后不自动注册。

当前没有实现 `/v1/chat/completions`、function tools 和结构化输出。需要这些能力的客户端应先使用 Responses API，不能把本文档当成完整 OpenAI API 覆盖声明。

## 唯一配置来源

服务端不从环境变量、`.env`、旧 Hominal xconfig 或其他 YAML 读取配置。运行时只读取两个文件：

- 非秘密配置：[configs/config.yaml](./configs/config.yaml)
- 秘密配置：`../xconfigs/llmserver/xconfig.yaml`

秘密文件结构如下，真实值不要复制进仓库：

```yaml
version: 1
client_tokens:
  local-dev: "replace-with-a-long-random-token"
provider_api_keys:
  api-openai: "replace-with-provider-key"
```

首次启动时，如果秘密文件不存在，服务会创建权限为 `0600` 的文件，并为已有设备生成持久随机 Token。生成后的 Token 不会因服务重启而变化。标准 API Key 可通过本机管理页面录入。

公开配置包含：

- `server.listen`：API 地址；当前为 `0.0.0.0:4815`，允许局域网访问；
- `server.admin_listen`：管理页地址，强制要求回环地址；
- `clients`：设备 ID、模型白名单和额度返回开关；
- `providers`：Provider 类型、地址、可执行程序与新模型默认公开价格；
- `deployments`：公开模型 ID、上游模型、公开价格，以及可选实际成本/积分费率。

公开价格与实际消耗严格分离：调用方费用始终按 `deployment.price × 本次计费 Token` 计算。供应商返回的 cost 只进入管理统计；供应商没有返回成本时，可用 `actual_price` 或 `actual_points` 做后台估算，不改变调用方账单。

## 构建和测试

要求 macOS、Go 1.25，以及已登录的 ChatGPT/Codex 和 WorkBuddy 桌面应用。WorkBuddy Adapter 还需要当前 `node` 命令可用。

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o dist/llmserver ./cmd/llmserver
```

## 常驻启动

项目提供的脚本不会创建 plist，也不会增加第三个配置文件：

```bash
scripts/llmserver-service start
scripts/llmserver-service status
scripts/llmserver-service restart
scripts/llmserver-service stop
```

`start` 会构建二进制并通过 `launchctl submit` 注册 `cc.hominal.llmserver`。服务脱离终端持续运行，异常退出后自动拉起；系统重启后需再次执行 `start`。运行数据库与日志位于 `var/`，该目录不会提交到 Git。

启动后访问：

- API：`http://127.0.0.1:4815/v1`
- 本机管理台：`http://127.0.0.1:4816/admin/`

管理台只接受来自回环地址的访问，配置变更会先验证、写入两个指定 YAML，再原子切换新请求使用的运行快照。服务监听地址本身不在页面内修改；修改 `server` 段后需要重启服务。

## 管理台工作流

1. 在“供应商”添加标准 API 地址和 Key，或选择现有 Codex/WorkBuddy Provider。
2. 设置“新模型默认输入价/输出价”，点击“刷新可用模型”。有未保存修改时，管理台会先保存再拉取。
3. 勾选模型并设置公开 ID 与价格。
4. 在“模型与定价”启停模型，并按需配置后台货币成本或积分估算。
5. 在“访问设备”生成持久 Token、设置模型白名单和额度返回开关。
6. 在“消耗统计”分别查看公开结算、供应商实报/配置估算和 Codex 额度快照。

Codex 能读取当前账号的多个限额窗口。WorkBuddy 当前 CLI 会实报 `total_cost_usd`，但没有提供积分余额接口；因此管理台会记录实报美元成本，积分只能明确标记为配置估算，不能伪装成供应商实报。

## 局域网边界

API 当前监听所有本机接口，并用每设备 Bearer Token 限制模型范围。它没有内建 TLS，也没有按来源 IP 的防火墙。只应在可信、隔离的局域网使用；跨不可信 Wi-Fi 或公网时，应在前方增加 HTTPS/mTLS、VPN 或零信任入口。管理台始终只能从本机访问。

客户端请求、流式事件、预算与幂等说明见 [GUIDE.md](./GUIDE.md)，实现边界见 [docs/implementation-status.md](./docs/implementation-status.md)。
