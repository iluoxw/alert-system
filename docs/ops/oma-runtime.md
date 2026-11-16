# OMA 常驻分析 worker

该 worker 是 SRE Control 的可选分析消费者，使用同一 PostgreSQL 中既有角色投递队列，不要求个人电脑常驻。它不影响确定性通知，也没有修改 Alert、Fault、Impact、Incident 生命周期的代码路径。

## 部署前置条件

- 迁移镜像必须同时包含 33 和 34，从账本 32 顺序升级到 33、34。禁止先部署只有 34 的镜像再补较小编号。
- 使用已审查的 Grafana Source scope、稳定公开 rule_id 和角色路由。worker 自身再次限制 source UUID；不把整个 Grafana org 作为默认允许范围。
- 专用 OMA agent、无原生工具的环境及 API key 由运维通过已有平台配置提供，不能复用旧 Demo 会话中的凭据。模型收到的只有公开告警投影与固定 broker 证据，不收到 API key 或 IAC 只读凭据。
- 本次部署优先复用现有 SRE API 进程与平台配置，不新增 Kubernetes worker 或 Secret；API 镜像打包 IAC CLI。独立 cmd 只保留为可选运维入口。
- 镜像构建与 IAC CLI 配置见 [IAC broker 说明](iac-broker-oma.md)、`deploy/iac-worker/Makefile`。默认将只读 CLI 放在 `/app/iacctl-go`，非 root、只读根文件系统和独立 `/tmp`。
- 主 API 装配应调用 `router.RegisterAnalysisResultRoutes(omaruntime.NewRepository())`；这部分由主 Agent 联合装配。

## 复用现有 API 进程

宿主显式启用分析后，调用 `omaruntime.LoadConfig(get)`，其中 `get` 是现有平台配置读取函数；再调用 `omaruntime.NewConfiguredWorker(st,cfg)` 复用已有 Store。构造不启动 goroutine、不创建基础设施、不读取凭据文件、不关闭共享 Store。宿主用受管理的 goroutine 调用 `worker.Run(ctx)`，取消后等待其返回；队列 Reap 沿用现有 API 的周期器。分析进度用 `worker.Ready()` 单独暴露，避免 OMA 服务故障影响既有必达通知 API 的就绪状态。

`cmd/oma-worker` 使用相同组装函数，额外负责独立进程的 DB_DSN、信号、健康监听与有界 Reap。`OMA_HEALTH_ADDR` 仅用于该可选独立入口，嵌入 API 时由宿主选择健康路由。

## 启动配置

| 环境变量 | 默认值 / 约束 |
| --- | --- |
| `DB_DSN` | 必填；同一受控 PG，不打印 |
| `OMA_API_KEY` | 必填；只放 HTTP header，不进 prompt / 报告 |
| `OMA_BASE_URL` | `https://oma.test.seewo.com`；生产要求 HTTPS，不带用户信息或 query，不跟随重定向 |
| `OMA_AGENT_ID` | 必填；专用分析 agent |
| `OMA_ENVIRONMENT_ID` | 必填；专用环境，无原生任意执行工具 |
| `OMA_SOURCE_IDS` | 必填；逗号分隔的明确 UUID，不带空格，不接受通配符 |
| `OMA_ROLE` | 必填；只领取这一角色 |
| `OMA_CLIENT_ID` | `oma-worker`；既有队列审计主体，不需要机器 API bearer token |
| `OMA_WORKERS` | `1`；范围 1–8，每个空闲槽才领取 1 条，无内存预取队列 |
| `OMA_POLL_INTERVAL` | `1s`；100ms–30s |
| `OMA_LEASE` | `60s`；3s–5m；每 1/3 lease 通过 PG 条件更新续租 |
| `OMA_ROUND_TIMEOUT` | `120s`；1s–5m，每轮模型事件观察上限 |
| `OMA_RUN_TIMEOUT` | `10m`；至少一轮上限、最多 15m；进程取消保留 checkpoint |
| `OMA_HEALTH_ADDR` | `:8080`；`/healthz` 与 `/readyz` |
| `IACCTL_BINARY` | `/app/iacctl-go`；必须绝对路径，启动记录可执行文件 SHA-256 |
| `IACCTL_SOURCE_REVISION` | `643ddf7aaec314a438f052a719b3741f2eef71ff`；已打包 CLI 源版本 |

`/readyz` 不是数据库 Ping 的别名：启动后每个槽必须有真正成功的账本/队列轮询；空闲时成功进度超过 `max(3×poll,1s)` 即失效；执行中仅成功续租才能维持进度，超过 lease 即失效。成功长任务完成刷新进度，不能在下一次成功取库前短暂误红。没有仅因进入 loop 而刷新成功时间的逻辑。OMA 503 以分析 partial 状态暴露，不等价于把必达通知 API 摘除。

## 持久接收与恢复

领取后在同一个短事务内依次执行：保存 `oma_analysis_runs`（message_id 唯一）→复用 `Queue.AckTx` 校验并 ACK 原 receipt→标为 received→提交。事务中途崩溃全部回滚，队列租约到期后可以重新领取；提交后进程崩溃则从 received 运行账本续接。ACK 仍只代表接收，不代表报告完成。旧 receipt 不会触发分析。

每条分析运行有独立 lease_token，续租、事件、动作和报告 checkpoint 都做 fencing 条件检查。单次执行失败后有界退避；非接收阶段最多 5 次恢复执行，超出保留 partial 原因。会话创建/第二轮 user.message 在外部 POST 前持久记录 submission_id 与请求内容；提交结果不确定时按 metadata.run_id/唯一标题或 user.message 内 submission_id 查回。查不回时写 submission_unknown，禁止盲目重发 POST 或新开会话掩盖旧请求。OMA 没有已验证的服务端幂等保证，这个边界不能宣称物理恰好一次。

证据查询是固定只读命令，因此崩溃在取数后、证据落库前可以重新采样；新的 observed_at 明确区分采样时刻。证据落库后不重复取数。提交证据后失去响应通过同会话事件对账；报告同 run 唯一、相同内容哈希幂等、不同内容拒绝覆盖。

## 真实事件订阅

使用已核验的 `GET /v1/sessions/{id}/events/stream` SSE；持久记录 event ID/type/正文/时间，`UNIQUE(session_id,event_id)` 去重，保存 cursor 和连接次数。断连后重新订阅，并使用 `GET /v1/sessions/{id}/events?order=asc&limit=100&page=...` 重叠补漏，最多 20 页。Last-Event-ID 只作为 best effort，不依赖未证明的服务端历史回放。

只有本轮正常 `session.status_idle/stop_reason=end_turn` 加合法、可追证据的 JSON 才能完成。已观察到的 session.error、session.status_terminated 或异常 stop_reason 优先于本轮早先的合法 JSON/end_turn，记录 partial/unknown。报告是完成 checkpoint 时的观察；不预测该 checkpoint 之后的会话事件，也不静默重写不可变历史报告。

第一轮输出 `sre.analysis-intent/v1`，只能请求最多 6 个固定 query_id，无参数；模型不能提供 URL、SQL、shell、环境或 appId。第二轮同会话回灌 `sre.analysis-evidence/v1`，模型输出 `sre.analysis-result/v1`。事实必须引用本次成功证据 ID；错误/未知证据不能当事实证明健康。

程序按固定 broker Profile 限制候选查询。共享五地/入口规则必须保留 entry_coverage，VIP 规则必须保留 vip_coverage；模型省略时由程序在已批准 query allowlist 内补齐。若覆盖 ID 未在执行 allowlist 中，不越权执行，持久 coverage_gaps 并强制 partial/unknown。DatasourceError / DatasourceNoData 仅允许来源/新鲜度观测，不能因为原 rule_id 是系统错误率就得出业务健康或退化结论。截断组同样不能得到完整健康结论。

## 管理员只读结果 API

- `GET /api/v1/admin/analysis-runs?message_id=...&alert_id=...&fault_id=...&source_id=...&role=...&limit=50`
- `GET /api/v1/admin/analysis-runs/{run_uuid}`

现有 sessionAuth + requireAdmin 认证；未登录 401、普通成员 403、错误 UUID/limit 400、记录不存在 404。请求使用有界 READ ONLY 事务，集合恒为数组。返回阶段、session、stream连接/事件摘要、schema化报告、脱敏证据以及原 Alert/Fault ID，不返回 receipt、队列原 envelope、原始 OMA 事件正文或 API key。

关联使用共享 `agentprotocol.OccurrenceRef`，Grafana 时间按 PG 微秒精度归一化，Alertmanager 保留原纳秒契约。无精确匹配时保留 partial binding 状态，不按标题猜关联。分析报告不是领域状态机写入口。新结果表不随队列 7 天清理级联删除；其保留策略由运维后续显式制定。

## 运行核验

先核对迁移、worker ready、来源/角色白名单和 broker hash；用专用 canary 走 Grafana→SRE→OMA→受控 IAC 只读取证→同会话第二轮→结果 API。分别保存队列 ACK、运行阶段、OMA session、持久事件、证据 observed_at 与最终结果；只看 ACK、idle 或 worker 退出码都不能作为通过。真实故障/真实 IAC 故障归因不能由 synthetic canary 推导。
