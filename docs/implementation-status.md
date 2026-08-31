# llmserver 实现状态

> 更新时间：2026-08-31
> 本文只记录已经由代码、自动测试或受控真实调用证明的事实。

## 当前状态

Stage 1 结算主链、Stage 2 Codex Adapter、Stage 3 WorkBuddy Adapter和本机管理台已经可运行。API 已作为 macOS 用户级常驻任务监听 `0.0.0.0:4815`，管理台只监听 `127.0.0.1:4816`。

已实现：

- 服务端运行时只读取 `configs/config.yaml` 与上层 `xconfigs/llmserver/xconfig.yaml`；不读取环境变量、`.env`、旧 Hominal xconfig 或其他配置文件；
- 秘密 xconfig 权限 `0600`，设备 Token 首次生成后持久保存；配置对象序列化和普通管理状态接口不会返回秘密值；
- `GET /healthz`、`GET /readyz`、鉴权后的 `GET /v1/models`；
- `POST /v1/responses` 非流式与 SSE、客户端取消、严格兼容模式和请求校验；
- 标准 Responses HTTP/SSE Provider，本机 Codex `exec --json --ephemeral` Provider，本机 WorkBuddy headless `stream-json` Provider；
- Provider 动态模型发现：标准 `/v1/models`、Codex App Server `model/list`、WorkBuddy CLI 公布的支持列表；
- 本机 Web 管理台：Provider/Key、默认公开价格、模型刷新与发布、模型价格、实际成本/积分费率、设备 Token/白名单、额度返回开关和消耗统计；
- 配置保存前完整验证；成功写盘后，使用原子运行快照让新请求热生效；在途请求继续使用旧快照；
- 每个 Deployment 独立公开输入价、输出价与版本，9 位小数定点结算；
- 上游未报告某一维 Token 时使用 `text_estimator_v1` 字符估算，估算量直接成为本次公开计费量；
- 可选 hard/soft budget；hard 只对明确可执行且提供 `max_output_tokens` 的 Deployment 开放；
- SQLite 记录 accepted/running/completed/failed、usage 来源、公开价格快照、金额、幂等和崩溃恢复；
- 供应商结构化 cost/total_cost 作为 `provider_reported` 单独入库；未实报时可用 `actual_price` / `actual_points` 作为 `configured_estimate`；两者均不改变调用方费用；
- Codex 经 App Server 读取多个 rate-limit 窗口并单独入库；只有设备策略开启时才向调用方返回；
- WorkBuddy 记录 CLI 实报 `total_cost_usd`；当前版本没有可靠积分余额接口，不伪造积分实报；
- 管理端限制回环来源、拒绝跨站修改请求，并设置 CSP、禁止 iframe 与 no-store；
- `launchctl submit` 常驻脚本使用清洁环境启动，不把调用任务中的无关 Token 继承到服务进程；异常退出由 launchd 拉起，系统重启后需手工重新注册。

## 2026-08-31 真实验证

- 标准 API、Codex、WorkBuddy 均成功刷新当前可用模型；分别返回 10、7、15 个模型；
- `luna` 返回 `OK`，公开结算确认，输入 4685 Token、输出 5 Token、总价 `0.000943000 USD`；
- `codex-luna` 返回 `OK`，公开结算确认，输入 12777 Token、输出 5 Token、总价 `0.002561400 USD`；额度快照已入库且默认不向调用方返回；
- `workbuddy-hy4-preview` 返回 `OK`，公开结算确认，输入 3096 Token、输出 57 Token、总价 `0.006876000 USD`；CLI 实报美元成本已进入独立实际消耗表；
- 管理页、状态接口和消耗汇总接口返回 200；普通状态响应与常驻进程环境均未包含配置秘密值；伪造跨站修改请求返回 403；
- API 监听所有本机接口，管理台只监听 IPv4 loopback；
- 自动测试覆盖配置分离、密钥脱敏、配置热更新、设备改名 Token 迁移、模型发现、管理端来源限制、公开/实际消耗分离、鉴权、金额、SSE、取消、幂等和数据库恢复。

## 明确未实现或未证明

- `/v1/chat/completions`、function tools、结构化输出和完整 OpenAI API 覆盖；
- 客户端每分钟速率、并发队列、来源 IP allowlist 和更细输入/输出限制；
- 内建 TLS。当前 LAN 监听只适用于可信隔离网络；不可信网络必须增加 HTTPS/mTLS、VPN 或零信任入口；
- WorkBuddy 积分余额或积分变化的供应商实报；后台积分费率只能作为配置估算；
- soft budget 中途取消和 overshoot 上限；
- Standard API 全部供应商私有 cost 格式。当前仅解析安全、结构化的常见 `cost` / `total_cost` 形态；
- 配置两个 YAML 之间的跨文件崩溃事务。每个文件独立原子替换，运行快照只在两者均保存成功后切换，但操作系统在两次 rename 之间断电仍不是分布式原子事务；
- 系统重启后自动启动。当前按用户要求只保证本次登录会话内常驻；重启后执行 `scripts/llmserver-service start`；
- 完整操作系统沙箱。Codex/WorkBuddy 已禁用工具、使用临时目录和最小环境并对工具事件失败关闭，但仍复用当前 macOS 用户登录态，不应被视为强隔离边界。
