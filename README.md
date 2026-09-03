# llmserver

llmserver 是运行在 macOS 本机的统一 LLM API 网关。它把标准 OpenAI Responses 兼容供应商、本机 Codex 登录态和本机 WorkBuddy 登录态收敛为同一套 `/v1/responses` 接口，并统一处理访问密钥、模型发布、Token 用量、公开定价、预算、幂等和 SQLite 结算。

调用方只接触公开模型 ID 和平台价格，不会知道模型由哪类供应商执行，也不会获得供应商 API Key、Codex 凭据或 WorkBuddy 凭据。

## 当前能力与边界

已经实现：

- `GET /healthz`、`GET /readyz`、`GET /v1/models`；
- `POST /v1/responses` 非流式响应和 SSE 流式响应；
- OpenAI Responses 自定义函数调用闭环：`tools`、`tool_choice`、单函数 `function_call` 和下一轮 `function_call_output`；
- 每个访问密钥独立的模型白名单和每日平台价美元额度；
- 标准 Responses API、Codex CLI、WorkBuddy CLI 三类 Provider；
- 管理台刷新可用模型、启停模型、设置公开 ID 和输入/输出价格；
- 成功请求返回输入量、输出量、输入费、输出费和总价；
- 上游不返回 Token 时，中文/日文/韩文字符按约 1 Token、其他字符按约 4 字符 1 Token 兜底估算，并直接用估算值公开计费；
- 供应商实报成本、后台配置的实际成本估算、WorkBuddy 本次运行实报积分和平台价消耗分别入库；
- 可选软/硬价格上限、按访问密钥隔离的幂等、SQLite 持久结算和未完成请求恢复；
- 仅本机可访问的管理台和配置热更新；
- macOS 用户登录会话内常驻运行，进程异常退出后由 `launchd` 拉起。

当前公开兼容面不是完整 OpenAI API：没有 `/v1/chat/completions`；不支持 MCP、托管工具和并行工具链；函数参数增量流事件尚未作为稳定契约；`store=true` 与服务端会话续接不支持。标准 API Provider 会透传大部分额外 Responses 字段，但不能据此认为所有 Provider 都具有相同能力。调用方应以 [GUIDE.md](./GUIDE.md) 明确列出的字段为稳定契约。

## 快速启动

运行要求：

- macOS；
- Go 1.25；
- `node` 命令（WorkBuddy Adapter 启动要求）；
- 如需 Codex：ChatGPT/Codex 已在当前 macOS 用户下登录，配置的 Codex 可执行文件可运行；
- 如需 WorkBuddy：WorkBuddy 已在当前 macOS 用户下登录，配置的 WorkBuddy CLI 可运行。

在项目根目录执行：

```bash
go test ./...
scripts/llmserver-service start
scripts/llmserver-service status
```

首次启动会在项目上层创建唯一秘密配置文件，并为公开配置中尚无 Token 的访问密钥生成持久随机值。启动成功后：

- API：`http://127.0.0.1:4815/v1`
- 管理台：`http://127.0.0.1:4816/admin/`
- 健康检查：`http://127.0.0.1:4815/healthz`
- 就绪检查：`http://127.0.0.1:4815/readyz`

管理台只接受本机回环访问；API 默认监听 `0.0.0.0:4815`，可供局域网设备连接。

## 唯一配置来源

服务端不读取环境变量、`.env`、旧 Hominal 配置或其他 YAML。运行时只使用：

- 非秘密配置：[configs/config.yaml](./configs/config.yaml)
- 秘密配置：`../xconfigs/llmserver/xconfig.yaml`

秘密配置只保存访问 Token 和标准 API Provider 的 Key：

```yaml
version: 1
client_tokens:
  local-dev: "replace-with-a-long-random-token"
provider_api_keys:
  api-openai: "replace-with-provider-key"
```

该文件位于 Git 仓库外，程序以 `0600` 权限写入。不要把它复制到仓库、日志、截图或请求响应中。Codex 和 WorkBuddy 直接复用当前 macOS 用户的本机登录态，不需要也不应该把其凭据填入 YAML。

非秘密配置的核心结构：

- `server`：API 地址、管理台地址和 SQLite 路径；
- `clients`：访问密钥 ID、允许模型和可选的 `daily_limit_usd`；
- `providers`：Provider 类型、连接方式、可执行程序和新发现模型的默认公开价格；
- `deployments`：公开模型 ID、上游模型、函数调用能力、公开价格、可选后台实际成本/积分费率及启用状态。

建议通过管理台修改供应商、模型和访问密钥。保存时会先验证完整配置，再原子写入上述两个 YAML，并让新请求立即使用新快照。只改访问密钥、Deployment、价格、供应商显示名称或模型发现 URL 时，会复用连接参数未变化的 Provider，不会丢掉 Codex HTTP 连接和 WorkBuddy ACP 热身；只有执行程序、上游生成 URL/鉴权、并发、默认推理强度或速度层级等运行参数变化时才替换对应 Provider。手工修改 YAML 后不会自动重载，需要重启服务；`server.listen`、`server.admin_listen` 和 `server.state_path` 即使通过管理接口写入，也必须重启后才真正改变监听或数据库。

## 供应商配置

### 标准 API Provider

管理台可以新增 `openai_responses` Provider。若供应商遵循标准路径和 Bearer 鉴权，填写 Base URL 即可：

- Base URL 以 `/v1` 结尾时，请求地址为 `{base}/responses`；
- Base URL 已以 `/v1/responses` 结尾时直接使用；
- 其他 Base URL 默认追加 `/v1/responses`。

不符合默认路径或鉴权时，可在编辑弹窗填写完整 Models URL、完整 Responses URL、Key Header 和 Key Prefix；无前缀时将 Prefix 设为 `none`。额外签名、动态 Header、私有请求体或仅支持 Chat Completions 的供应商不能靠 Base URL 兼容，需要独立 Adapter。

### Codex 与 WorkBuddy

这两类是系统 Provider，只能配置现有项，不能从管理台新增。Codex 可在已验证的模型上通过登录态 Responses 直连执行调用方自定义函数协议；WorkBuddy 继续只开放无工具文本生成。Codex App Server 兜底和 WorkBuddy ACP session 使用隔离的临时目录，请求方看不到本机账号和真实上游模型身份。

Codex 生成主链使用当前登录态的最小化 Responses SSE 请求，HTTP 连接常驻复用，并遵循 macOS 当前 HTTPS 系统代理。纯文本请求继续发送空工具集并保持原性能路径；函数请求只发送调用方本轮声明的函数，并收集 `response.output_item.done` 重建标准输出。函数链路要求直连 Responses 可用，不会降级到无法表达动态自定义函数的 App Server；普通文本请求在直连接口明确不可用时仍可切换到隔离、常驻的 App Server worker。状态不明的网络错误不自动重放，避免一次调用产生两次上游消耗。

WorkBuddy 使用与 IDE/Web 集成一致的 ACP stdio 长连接。默认在后台并行建立并常驻两个 worker，启动时每个槽位并行执行一次最短真实生成，预热 Node、登录认证、TLS 和模型路由；API 与管理监听不会等待桌面软件冷启动或登录恢复，WorkBuddy 请求本身会等待首个 worker 就绪。ACP 初始化有超时保护，worker 因取消、超时或协议错误失效后，后台自动补齐并重新预热。进程跨请求复用，但每次调用新建独立 session 和临时 cwd，通过 `session/set_model` 与 `session/set_config_option` 显式设置模型和推理强度，并直接转发 `agent_message_chunk`。worker 使用 `--no-session-persistence`，同时关闭 Auto Memory、system reminder、会话摘要/标题、插件市场、热重载、cron、REPL 和遥测；不会加载用户/项目设置、MCP 或工具。积分直接取自同一次公开请求的 ACP usage 事件，不发起额外额度、余额或积分请求。模型列表中的积分倍率只是产品目录元数据，不等于本次实际积分，也不是账号剩余积分。

系统 Provider 可配置最大并发和默认推理强度。Codex 还可启用 `priority` 速度层级；它能降低等待时间，但会增加登录账号的额度消耗。默认值当前为 Codex `low + priority`、WorkBuddy `high`。当前产品目录中的 `hy4-preview` 标记为只支持 `high` 且不可关闭思考，因此公开 Deployment 也只接受 `high`；不再接受实际无效的 `minimal/low` 并制造错误预期。

## 计价与结算

调用方费用永远只按 Deployment 的公开输入/输出价格计算：

```text
输入费 = 输入 Token × 输入单价 / 1,000,000
输出费 = 输出 Token × 输出单价 / 1,000,000
总价   = 输入费 + 输出费
```

供应商实报费用和 WorkBuddy 本次运行积分只进入管理统计，不改变调用方账单。价格和用量在请求完成主链中计算并写入 SQLite；只有 `settlement_status=confirmed` 的结果才是已确认结算。

上游报告的输出 Token 可能包含不可见推理 Token，因此输出 Token 数量不一定与最终文本长度成比例。平台仍以本次上游实报量为优先计费量；只有缺失的维度才使用字符估算，不会用可见文本长度覆盖供应商实报。

硬预算只适用于标记为支持硬预算的标准 API Deployment，并要求请求提供 `max_output_tokens`。软预算不阻止执行，只在完成后报告是否超额。预算不设置时，按本次最终用量正常结算。

## 管理台

管理台包含五个主要页面：

1. “概览”：查看已启用模型和近期访问密钥用量；
2. “供应商”：管理 API Provider，刷新三类 Provider 的模型并配置发布状态和价格；
3. “平台消耗”：按供应商和时间窗查看平台价、供应商实际消耗及模型明细；
4. “访问密钥”：生成/复制持久 Token，设置模型白名单和每日额度；
5. “用户消耗”：按访问密钥查看平台模拟价总量，以及不同模型随时间变化的消耗折线图。

推荐的管理员初始化顺序：

1. 先在“供应商”确认系统 Provider 状态，并按需添加标准 API Provider；
2. 点击“刷新/设置模型”，选择对外发布的模型，设置公开模型 ID、平台输入价和平台输出价；
3. 在“访问密钥”为每台设备分别建立一个持久 Token，并只勾选该设备需要的模型；
4. 完成测试调用后，在“平台消耗”核对供应商实际消耗，在“用户消耗”核对访问密钥的平台价趋势。

### 供应商管理

供应商分为两类：

- API 供应商：可以新增、编辑和删除，秘密 Key 写入仓库外的 xconfig；
- 系统供应商：Codex 和 WorkBuddy，只能使用当前项目已有配置，不能从页面新增。

供应商卡片只展示基础状态。API Key 默认显示首尾提示，不在普通状态接口返回完整秘密；“复制”动作通过只允许回环访问的管理接口取得完整值。API 供应商的 Base URL、Models URL、Responses URL、Key Header 和 Key Prefix 在编辑弹窗中维护。

WorkBuddy 系统供应商还可设置自动预热、预热模型和超时。预热会在服务启动及 worker 自动恢复时产生最短真实模型调用，消耗少量登录账号积分，但不会形成任何访问密钥的调用方账单；服务日志记录预热耗时和 Token。关闭自动预热只取消真实模型预热，不取消 ACP worker 常驻。

### 模型发布和价格

“刷新/设置模型”弹窗是模型发布和定价的统一入口。每个模型可配置：

- 是否启用；
- 对外公开模型 ID；
- 平台输入价和平台输出价，单位为每百万 Token 的 USD；
- API 模型可选的实际消耗记录方式及对应费率；
- WorkBuddy 模型目录提供的积分倍率，只作为供应商产品信息展示；
- 函数调用能力：`native`、`emulated` 或 `unsupported`。

模型发现结果中的 `supported_reasoning_efforts` 用于校验公开请求的 `reasoning.effort`；调用值不在模型允许列表时会在启动上游前拒绝。Codex 的 `model/list` 当前会漏报 GPT-5.6 Luna、Terra、Sol 已验证可用的 `none`，服务端会只对这三个精确模型 ID 补齐该能力。`hard_budget_supported` 用于硬预算准入判断。这两项当前没有管理台编辑入口，需要修改时应直接编辑 `configs/config.yaml` 并重启服务。

`function_calling` 是 Deployment 的明确能力门：省略等价于 `unsupported`；只有 `native` 会接受非空 `tools`；`emulated` 只诚实标记上游可能用提示词生成 JSON，不会被公开接口当作可执行的原生身体能力。管理台模型弹窗可以修改该项，但管理员只应在完成强制函数选择、Schema 参数、工具结果续接和无副作用测试后启用 `native`。WorkBuddy 当前固定为 `unsupported`。

公开函数契约以 [OpenAI Responses API](https://developers.openai.com/api/reference/cli/resources/responses/methods/create) 为基线，不另造私有工具协议。llmserver 只传递并校验函数意图，不执行调用方函数，也不会把 Codex 自身的文件、Shell、MCP 或其他本机工具暴露给调用方。

手工保存的模型价格优先于刷新模型时发现的默认价格。取消启用并保存后，新请求立即不能再使用该模型；已经开始的请求继续使用启动时的配置快照。平台公开价格与供应商实际消耗始终分离：前者用于调用方账单，后者只用于管理统计。

Codex 不采集也不展示账户额度。WorkBuddy 的实际消耗固定优先记录本次会话已经产生的积分；模型积分倍率不代替本次实际积分。

### 访问密钥

每台调用设备应使用独立访问密钥，以便单独控制模型权限、撤销访问和统计用量。新 Token 使用安全随机数生成，并持久保存于 `xconfig.yaml`；服务重启不会改变 Token。重新生成并保存后旧 Token 立即失效，删除访问密钥会同时删除其秘密配置，但已有历史用量仍保留在 SQLite 中。

权限设置中可以为每个密钥填写每日额度，单位为 USD，留空表示不限。额度只累计成功确认结算的“平台模拟价”，不使用供应商实际成本、Codex 额度或 WorkBuddy 积分；按服务器本地时区每天 `00:00` 自动开始新窗口。后台同时显示当天已用金额，“立即重置”只把该密钥的额度窗口推进到当前时刻，不删除历史 Run 和账单。

新请求在启动供应商前检查当天已确认消耗；达到上限后返回 `402 daily_quota_exceeded`。由于最终输出量只有请求结束时才确定，最后一个获准请求可能把已用金额推到上限之上，之后的新请求才会被阻止；并发中的未结算请求也无法预先精确计价。已完成请求使用同一幂等键重试时仍返回原结果，不重复调用供应商，也不受随后达到额度上限影响。

### 消耗统计

“平台消耗”按供应商和自定义时间窗汇总平台价、实际消耗及模型明细。“用户消耗”只展示平台模拟价，提供最近 1 小时每 5 分钟、最近 3 小时和 6 小时每 10 分钟、最近 1 天每 30 分钟以及最近 3 天每小时的折线图；每个非零模型使用独立颜色，并显示该时间段的总额。统计中严格区分：

- 平台价消耗：按公开模型价格和计费 Token 计算，单位 USD；
- API 供应商实际消耗：优先使用供应商实报，否则使用模型配置的实际费率估算；
- Codex 实际消耗：不采集；
- WorkBuddy 实际消耗：本次调用积分。

WorkBuddy 当前没有可靠的剩余积分查询接口，因此管理台展示的是本次生成记录中已经发生的积分消耗，不是账户余额。

## 性能链路与诊断

网关不会跨 Provider 重试，也不会为了统计额外请求 Codex/WorkBuddy。纯文本请求不注入任何函数 Schema；函数请求在 API 入口只做一次本地结构与 Schema 编译校验，并且只把调用方实际提交的工具传给上游。调用方应按任务筛选少量工具，因为 Schema 本身计入输入上下文；函数执行后生成最终回答还需要第二次模型调用。Codex 普通请求只有在上游明确返回可安全回退的 HTTP 状态且尚未开始生成时才使用 App Server 兜底，函数请求不做不兼容降级；连接中断等结果不明的错误不会重放。

每次完成请求只执行一次本地结算事务。函数参数属于本轮输出 Token，`function_call_output` 属于下一轮输入 Token；调用方本地函数本身不由 llmserver 计价。服务日志会为每个 Run 输出 `provider_start_ms`、`first_delta_ms` 和 `total_ms`，不记录提示词、回答或凭据。`provider_start_ms` 表示 Adapter 建立本次事件流所需时间，不同传输下可能包含排队、HTTP 响应头或 session 初始化；判断用户体验应以 `first_delta_ms` 和 `total_ms` 为主。

2026-09-02 的本机真实函数调用验收中，Codex Luna、Terra、Sol 均通过登录态直连返回声明函数和符合 Schema 的参数，单次工具决策约 `1–3` 秒；Luna 的 `function_call_output` 第二轮成功生成最终文本。标准 API Luna、Sol 通过统一接口完成函数调用，Terra 对同一供应商的直连协议测试成功但统一接口测试遇到过上游瞬时启动失败，因此能力成立不等于供应商稳定性保证。

2026-08-31 的本机短回答受控实测中，Codex 最小化直连首段约 `1.45–2.40` 秒、总耗时约 `1.64–2.68` 秒；最终部署 smoke 为 `2.25/2.40` 秒。同一任务在旧 App Server 重链中通常为 `5–20+` 秒。WorkBuddy 旧链路曾出现约 `3,100` 个固定输入 Token 和数十秒等待。2026-09-01 升级到 CLI `2.132.x` 并启用无状态轻量链路后，10 次公开 API 短回答输入稳定为 `121` Token，SSE 创建耗时 `1.9–8` 毫秒，首个可见文本中位数 `4.02` 秒、P90 `4.93` 秒，总耗时中位数 `4.68` 秒、P90 `5.52` 秒；唯一一次冷态/上游波动为 `8.50/8.78` 秒。配置热更新会保留 ACP worker，不因普通管理操作反复冷启动。

这些数字不是任意提示的服务等级保证。上述约 `3–5` 秒数据使用的是“只返回一个字符”的微型 smoke，而不是普通长回答；Codex 的约 `1.5–2.4` 秒数据同样来自只返回三个字符的 Luna 请求。WorkBuddy 界面可先显示思考/阶段状态，而 llmserver 为避免泄漏隐藏推理，只把最终答案文本作为首段。短 smoke 中约 `38–127` 个输出 Token 是隐藏推理；约 180 个英文词的近期测试则触发 `5,349–10,944` 个上游输出 Token，首文本为 `72.6–148.4` 秒。自动预热和常驻池只能消除本地启动、认证和连接冷态，不能消除 HY4 强制高推理。严格比较必须固定完全相同的模型、提示、目标输出和计时定义。

## 常驻服务与维护

```bash
scripts/llmserver-service start
scripts/llmserver-service restart
scripts/llmserver-service status
scripts/llmserver-service stop
scripts/llmserver-service uninstall
```

`start`/`restart` 会重新构建 `dist/llmserver`，安装用户级 `~/Library/LaunchAgents/cc.hominal.llmserver.plist`，再通过 `launchctl bootstrap` 启动。服务脱离终端运行、异常退出后自动拉起，并在 macOS 重启后的用户登录阶段自动启动。`stop` 只停止当前登录会话，保留下一次登录自动启动；`uninstall` 停止服务并把 plist 改名为 `.disabled`，用于明确取消自动启动且保留可恢复副本。

### Codex / WorkBuddy 升级检查

桌面软件升级可能同时更换其内置 CLI 和协议。不要只修改 `expected_version` 后直接恢复局域网流量；应依次执行：

1. 读取新 CLI 的 `--version`、`--help` 和模型发现结果；
2. 确认现有无工具、空 MCP、无会话落盘和最小环境参数仍被支持；
3. 更新 `configs/config.yaml` 中对应 Provider 的 `expected_version` 前缀；
4. 运行 `go test ./...` 并重启服务；
5. 通过公开 `/v1/responses` 做一次流式 smoke，确认有文本、usage、`llmserver.billing.completed` 和 `response.completed`；
6. 检查新进程没有额外模型调用、工具请求、记忆注入或敏感配置继承。

当前已验证的 WorkBuddy 兼容基线是 Desktop `5.4.5` 内置 CLI `2.132.x`。版本前缀是兼容门，不是对未来版本的兼容承诺。

运行数据位于 `var/`：

- `var/llmserver.db`：请求、结算和统计；
- `var/llmserver.stdout.log`：标准输出；
- `var/llmserver.stderr.log`：错误日志。

`dist/`、`var/` 和秘密配置均不提交到 Git。开发验证命令：

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o dist/llmserver ./cmd/llmserver
```

## 网络安全边界

API 没有内建 TLS，也没有按来源 IP 的防火墙；Bearer Token 在明文 HTTP 上传输。只应在可信、隔离的局域网使用。跨不可信 Wi-Fi、跨网段或公网使用时，必须在前方增加 HTTPS/mTLS、VPN 或零信任入口。管理台必须继续绑定回环地址。

调用示例、SSE、预算、幂等和错误处理见 [GUIDE.md](./GUIDE.md)，实现状态与已知限制见 [docs/implementation-status.md](./docs/implementation-status.md)。
