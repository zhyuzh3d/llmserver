# llmserver 实现状态

> 更新时间：2026-08-31
> 本文只记录已经由代码、自动测试或受控真实调用证明的事实。

## 当前状态

Stage 1 结算主链、Stage 2 Codex Adapter、Stage 3 WorkBuddy Adapter和本机管理台已经可运行。API 已作为 macOS 用户级常驻任务监听 `0.0.0.0:4815`，管理台只监听 `127.0.0.1:4816`。

已实现：

- 服务端运行时只读取 `configs/config.yaml` 与上层 `xconfigs/llmserver/xconfig.yaml`；不读取环境变量、`.env`、旧 Hominal xconfig 或其他配置文件；
- 秘密 xconfig 权限 `0600`，访问 Token 首次生成后持久保存；普通管理状态接口只返回首尾四位的掩码提示，不返回完整秘密值；
- `GET /healthz`、`GET /readyz`、鉴权后的 `GET /v1/models`；
- `POST /v1/responses` 非流式与 SSE、客户端取消、严格兼容模式和请求校验；
- 标准 Responses HTTP/SSE Provider，本机 Codex 最小化 Responses 主链与 App Server 兜底 Provider，本机 WorkBuddy ACP stdio Provider；
- Provider 动态模型发现：标准 `/v1/models`、Codex App Server `model/list`、WorkBuddy CLI 公布的支持列表；
- 本机 Web 管理台：概览、API/系统供应商分组、供应商详细连接配置、统一模型刷新/发布/定价弹窗、访问密钥/白名单、按供应商的平台消耗和按访问密钥的用户消耗；
- 标准 API Provider 除 Base URL 外可独立设置 Models/Responses 完整 URL、Key Header 与 Key Prefix；标准路径和 Bearer 约定仍为默认值；
- 配置保存前完整验证；成功写盘后，使用原子运行快照让新请求热生效；在途请求继续使用旧快照；
- 热更新只替换运行连接参数发生变化的 Provider；改访问密钥、Deployment、价格、显示名称或发现 URL时复用现有 Codex HTTP transport 与 WorkBuddy ACP worker；
- 每个 Deployment 独立公开输入价、输出价与版本，9 位小数定点结算；
- 上游未报告某一维 Token 时使用 `text_estimator_v1` 字符估算，估算量直接成为本次公开计费量；
- 可选 hard/soft budget；hard 只对明确可执行且提供 `max_output_tokens` 的 Deployment 开放；
- SQLite 记录 accepted/running/completed/failed、usage 来源、公开价格快照、金额、幂等和崩溃恢复；
- 供应商结构化 cost/total_cost 作为 `provider_reported` 单独入库；未实报时可用 `actual_price` / `actual_points` 作为 `configured_estimate`；两者均不改变调用方费用；
- `reasoning.effort` 已进入统一请求主链并按 Deployment 能力校验；Codex 和 WorkBuddy 都支持 Provider 默认推理强度，显式调用值优先；
- Codex 主链使用当前登录态的最小化 Responses SSE、空工具集、连接复用和 macOS 系统 HTTPS 代理；明确可安全回退的 HTTP 状态使用隔离的常驻 App Server，结果不明的网络错误不重放；不请求、不记录、不返回账户额度；
- WorkBuddy 默认并行常驻两个 ACP stdio worker，启动时对每个槽位执行最短真实模型预热，worker 失效后后台补齐并重新预热；每次公开请求新建不落盘的独立 session，并通过 session 配置接口显式设置模型和 `thought_level`；worker 关闭 Auto Memory、system reminder、会话摘要/标题、插件市场、热重载、cron、REPL 和遥测；从同一次公开请求的 ACP usage 事件读取 Token 和实际积分，并从本机动态产品目录读取模型积分倍率；不额外请求余额；
- Gateway 为每次 Run 记录不含内容的启动、首段和总耗时，便于定位是本地调度还是模型生成瓶颈；
- 管理端限制回环来源、拒绝跨站修改请求，并设置 CSP、禁止 iframe 与 no-store；
- `launchctl submit` 常驻脚本使用清洁环境启动，不把调用任务中的无关 Token 继承到服务进程；异常退出由 launchd 拉起，系统重启后需手工重新注册。

## 2026-08-31 至 2026-09-01 真实验证

- 标准 API、Codex、WorkBuddy 均成功刷新当前可用模型；分别返回 10、7、15 个模型；
- `luna` 返回 `OK`，公开结算确认，输入 4685 Token、输出 5 Token、总价 `0.000943000 USD`；
- `codex-luna` 最小化直连的短回答首段约 `1.45–2.40` 秒、总耗时约 `1.64–2.68` 秒；最终部署 smoke 为 `2.25/2.40` 秒，同一公开计价仍按上游本次实报 Token 结算；
- 升级后的 WorkBuddy CLI 为 `2.132.x`。关闭自动记忆、系统提醒和会话副任务后，`workbuddy-hy4-preview` 的短回答固定输入从约 `3,100` Token 降至 `121` Token；10 次公开 API 请求首文本中位数/P90 为 `4.02/4.93` 秒，总耗时中位数/P90 为 `4.68/5.52` 秒；
- `hy4-preview` 当前产品配置为 `onlyReasoning=true`、只支持 `high` 且不可关闭思考；短回答仍产生约 `38–127` 个隐藏推理 Token。内置 `minimal` 主 Agent A/B 没有速度收益，因此保持原生 `cli`；长提示若产生大量隐藏推理仍可持续几十秒，不能由本地进程复用消除；
- WorkBuddy 自动预热是明确启用的平台运行开销：启动和失效补池时分别对每个常驻槽位执行一次最短真实生成，消耗少量账号积分但不归属于访问密钥账单；普通配置热更新继续保留健康 worker，避免重复预热；
- 管理页、状态接口、按供应商和按访问密钥的时间窗消耗汇总接口返回 200；普通状态响应与常驻进程环境均未包含完整配置秘密值；伪造跨站修改请求返回 403；
- API 监听所有本机接口，管理台只监听 IPv4 loopback；
- 自动测试覆盖配置分离、密钥掩码、配置热更新、访问密钥改名 Token 迁移、模型发现、自定义端点/鉴权、管理端来源限制、按供应商/访问密钥的公开/实际消耗汇总、鉴权、金额、SSE、取消、幂等和数据库恢复。

## 明确未实现或未证明

- `/v1/chat/completions`、function tools、结构化输出和完整 OpenAI API 覆盖；
- 客户端每分钟速率、并发队列、来源 IP allowlist 和更细输入/输出限制；
- 内建 TLS。当前 LAN 监听只适用于可信隔离网络；不可信网络必须增加 HTTPS/mTLS、VPN 或零信任入口；
- WorkBuddy 剩余积分余额接口；当前能证明每次请求的实际积分消耗，但不能据此构造账户余额；
- soft budget 中途取消和 overshoot 上限；
- Standard API 全部供应商私有 cost 格式。当前仅解析安全、结构化的常见 `cost` / `total_cost` 形态；
- 配置两个 YAML 之间的跨文件崩溃事务。每个文件独立原子替换，运行快照只在两者均保存成功后切换，但操作系统在两次 rename 之间断电仍不是分布式原子事务；
- 系统重启后自动启动。当前按用户要求只保证本次登录会话内常驻；重启后执行 `scripts/llmserver-service start`；
- 完整操作系统沙箱。Codex/WorkBuddy 已禁用工具、使用临时目录和最小环境并对工具事件失败关闭，但仍复用当前 macOS 用户登录态，不应被视为强隔离边界。
