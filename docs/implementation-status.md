# llmServer 实现状态

> 更新时间：2026-08-30
> 原则：这里只记录已经由代码和测试证明的事实，不把设计计划写成已完成功能。

## 当前里程碑

Stage 1 核心链路与 Stage 2/3 的首个真实 Adapter 已经可运行，但完整安全和运维退出门尚未满足，因此当前版本只适合本机回环开发和受控测试，不应直接作为 LAN 发布版。

已实现：

- 仅解析上层 `xconfig.yaml` 的 `llm` 节点，密钥使用不透明类型并在格式化、JSON、YAML 中脱敏；
- 独立配置的标准 API Provider 支持通过 `api_key_env` 从进程环境解析密钥，无需把真实值写入 YAML；
- 手工公开价格覆盖目录默认价，输入价与输出价使用 9 位小数定点运算；
- `text_estimator_v1` 按维度填补上游缺失的输入或输出 Token，估算结果直接作为公开 billable usage；
- Bearer Token 鉴权、按客户端 Deployment allowlist、`GET /v1/models`；
- `POST /v1/responses` 非流式和 SSE、客户端取消、严格模式、请求体和基础能力校验；
- 标准 Responses HTTP/SSE Adapter，私有 `llmserver` 请求扩展不会传给上游；
- 上游响应字段白名单，避免兼容供应商注入的 instructions、tools、路由键或账号元数据泄漏给调用方；
- 可选 hard/soft budget 的当前基础语义；hard 在缺失 `max_output_tokens` 时调用前拒绝；
- SQLite accepted/running/completed/failed 状态、usage 来源、价格快照、金额、崩溃恢复和客户端范围内幂等键；
- 结算成功写盘后才发出 billing/completed，持久化失败不会伪造 confirmed；
- Codex/WorkBuddy 直接复用当前 macOS 用户在官方程序中的登录态；llmserver 不读取、复制、记录或返回登录凭据；
- Codex 逐 Run `exec --json --ephemeral` Adapter，版本前缀门、临时 cwd、单并发、最小环境、进程组取消和本地工具事件失败关闭；
- Codex 调用前后经 App Server 读取多 bucket rate limits，独立保存百分比窗口；只有客户端策略开启时才返回，默认省略；
- WorkBuddy 逐 Run headless `stream-json` Adapter，临时 cwd、单并发、禁用 tools/MCP/settings/后台任务、最小环境、进程组取消和工具内容失败关闭；
- Codex/WorkBuddy 上游报告的 token usage 进入同一公开结算内核；上游 cost 字段不参与调用方价格；
- Mock、伪 OpenAI 上游、金额、鉴权、取消、SSE、错误脱敏、重开恢复和幂等测试。

受控真实测试使用用户授权的上层 `xconfig.yaml`，未调用 Hominal：

- `luna` 非流式返回“正常”，上游报告 4688 输入、5 输出 Token，公开结算 `0.000943600 USD`；
- `luna` SSE 返回 `OK`，上游报告 4685 输入、5 输出 Token，公开结算 `0.000943000 USD`；
- 完成后的相同幂等请求约 0.00085 秒从 SQLite 返回，没有再次调用上游；
- 首次真实响应暴露出兼容供应商回显内部 instructions/tools 的问题，之后已改成公开字段白名单并通过第二次真实测试。

Stage 2/3 真实受控测试直接使用当前已登录用户额度，未调用 Hominal：

- `codex-luna` 返回 `CODEX_QUOTA_RETRY_OK`，上游报告 12832 输入、11 输出 Token，按配置价结算 `0.002579600 USD`；
- 同一 Codex Run 前后观测到 `codex` 周窗口，以及 Spark 的 5 小时和周窗口。本次供应商百分比粒度未变化，三个 delta 均为 0；这些共享账号快照已落库且默认客户端响应未返回；
- `workbuddy-hy4-preview` 返回 `WORKBUDDY_STAGE3_FINAL_OK`，上游报告 3175 输入、150 输出 Token，按配置价结算 `0.008150000 USD`；
- 两个 Adapter 都用诱导读取项目 README 的提示做了只读安全 smoke，均拒绝读取且未向响应返回文件内容。这个结果不能替代操作系统级副作用隔离证明。

## 尚未达到的 Stage 1 退出门

- `/v1/chat/completions`、单函数工具和结构化输出；
- `/llmserver/v1/capabilities`、请求查询和完整管理 CLI；
- 客户端每分钟速率、并发、队列、期限、来源 IP 与更细输入/输出限制；
- LAN 非回环监听时的 TLS 强制启动门；
- WorkBuddy credits/百分比额度尚无可靠公开读取路径；Codex 已实现，统一 bucket 结构仍需扩展 string/decimal credits 单位；
- soft budget 的中途 usage 观测、取消和 overshoot 解释；
- Standard API 更完整的错误分类、慢消费者、超时和首字节前安全重试；
- 配置 revision、Provider/Deployment/Price revision 的完整 SQLite 快照；
- LaunchAgent、滚动脱敏日志和安装/升级/卸载流程。

## Stage 2/3 剩余门槛

本机验证版本是 `codex-cli 0.151.0-alpha.7.1` 和 WorkBuddy `2.115.0`。版本发生变化时，当前前缀门会拒绝启动，必须先更新 fixture 和兼容判断。

两个 Adapter 的提示词、空目录、禁用工具、拒绝审批和事件失败关闭是纵深防御，不是完整安全边界。Codex 的 `read-only` sandbox 仍可能允许读取本机其他文件；WorkBuddy 的工具禁用也还没有用操作系统审计证明所有版本均无网络、Keychain、剪贴板或 GUI 副作用。因此配置仍只监听回环，不能直接开放到 LAN。

还需补齐冻结 fake CLI/App Server fixture、真实取消与崩溃恢复测试、慢消费者和排队期限、结构化输出/function tools、WorkBuddy quota、Provider 健康状态、LaunchAgent，以及 LAN TLS/mTLS 或反向代理强制门。Codex 额度观测依赖独立 App Server 刷新，失败时只记录 `unavailable`，不会推翻已完成的价格结算。
