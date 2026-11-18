# 按角色分发告警分析任务

来源显式启用时，Alertmanager 或 Grafana 的 managed hook 在入库事务中保存脱敏组快照和分发策略。提交后，投影 worker 按角色生成消息。告警 Pipeline 独立处理逐条事件，分发阶段还没有 Team、Fault 或 Incident 的路由上下文。Grafana 主动对账也可为遗漏的状态变化补任务。两条执行路径共享 PostgreSQL 队列。

| 执行路径 | 投递确认 | 执行结果与恢复 |
| --- | --- | --- |
| 独立 Agent Client | HTTPS Poll 后校验 Envelope 和 Handler，ACK 成功后执行本地程序 | 上报固定执行摘要。ACK 后进程崩溃可能丢失执行或报告，服务端不重投 |
| [托管 OMA worker](../../../docs/ops/oma-runtime.md) | 消费同一角色队列，运行账本与 ACK 同事务提交 | 从运行账本续接，最终报告须通过证据校验，由独立的 analysis-runs API 查询 |

不同执行路径需要各自的角色队列时，应配置不同 role。同一 role 的消费者竞争领取，任一任务只交付给一个有效租约。

## 决策记录

1. **任务与 Alert 分别建模。** 逐条 AlertEvent 照常同步 Alert/Fault。分析客户端消费批次快照，任务 ACK 只结束本次投递。
2. **配置在服务启动时加载。** 规则按来源类型、source ID、精确事件类型、等级和标签等值匹配。每个角色取首条命中规则，跨角色分发，整组未命中走 fallback。入队保存策略版本与参数，重启后仍能恢复投影。活跃消息合并保留首次到期时间，已发给客户端的快照冻结。
3. **只在确认 ACK 成功后执行。** 机器凭据绑定 role 和 client_id。每次领取生成独立 lease_id 并增加 attempt，一条确认同时检查主体和代次。客户端在 ACK 响应丢失时重试同一收据。ACK 后崩溃可能漏掉本次分析，适用于允许这种丢失的只读任务。
4. **PostgreSQL 保存状态，通知负责唤醒。** 空闲长轮询不持有数据库连接。服务端在订阅后查库，在通知、退避到期或等待超时后重查。TTL、租约和记录保留期限分别计算。
5. **执行权限由客户端管理员配置。** 客户端用固定 argv 启动受控可执行文件，输入从 stdin 传入。并发槽位限制领取数量。超时或排空宽限期结束时回收进程组。
6. **恢复和触发各自投递。** 同组 firing 与 resolved 可以关联，resolved 不自动取消已经入队的 firing。Grafana 按告警实例和阶段保留接收凭据，Webhook 重试或快照补偿不能为同一阶段重复启动分析。通用冷却窗口、自动取消与执行完成 ACK 属于后续能力。

## 客户端协议

机器接口均使用 HTTPS POST 和机器身份的 Bearer token。每条消息的收据由 `message_id`、`lease_id` 和 `attempt` 组成。客户端在后续请求中原样提交这三个字段。

| 接口 | 请求与响应 |
| --- | --- |
| `/api/v1/agent/poll` | 发送 `client_id`、`version`、`wait`（0–30 秒）和 `max`（1–8 条）。有消息时返回 200 和 `messages` 数组，每条含 `envelope`、收据、`lease_expires_at`、`ack_mode` 和可选 `report_supported`。超时或停机时返回 204。 |
| `/api/v1/agent/ack` | 发送收据。确认成功及同一收据的重复确认均返回 204。 |
| `/api/v1/agent/nack` | ACK 前发送收据、`retryable` 和简短的 `reason`，可选 `retry_after`（秒）。可重试的任务进入退避，达到领取次数上限或不可重试时进入 dead。成功返回 204。 |
| `/api/v1/agent/report` | ACK 后由原客户端发送收据及 `result`。结果包括 `outcome`（`succeeded`、`failed`、`timed_out`、`canceled`）、`duration_ms` 和可选 `exit_code`。首次写入及内容相同的重试返回 204。无有效 ACK、收据不匹配或已有不同结果时返回 409。 |

凭据无效返回 401，poll 声明的客户端与凭据不符返回 403。收据过期、主体不匹配或领取代次已变更时返回 409。领取结果应先验证 Envelope 版本和本地 handler，再发 ACK。具体样例见 [服务端配置](../../../docs/examples/agent-delivery.yaml) 和 [客户端配置](../../../cmd/agent-client/example.yaml)。

单次 poll 响应上限为 16 MiB。服务端在领取事务中按优先级计算消息和收据的大小，放不进本批的任务留在 pending。单条任务超过整个响应预算时进入 dead，审计事件 `poll_size_limit` 记录估算字节数。

### 执行报告与排障

客户端仅在收到 `report_supported: true` 时回传。旧客户端可忽略新增字段，新客户端可继续连旧服务端。客户端给回传及重试最多 5 秒，并记录未获得确认的消息 ID。消息已 ACK 后可以在原投递租约和消息 TTL 到期之后回传，直到该终态记录清理。报告只含结构化执行摘要，不发送命令、stdout、stderr 或异常文本。

例如，Handler 退出码为 17，已 ACK 的客户端可以发送：

```json
{
  "message_id": "00000000-0000-4000-8000-000000000001",
  "lease_id": "00000000-0000-4000-8000-000000000002",
  "attempt": 1,
  "result": { "outcome": "failed", "duration_ms": 217, "exit_code": 17 }
}
```

管理员使用现有登录态请求 `GET /api/v1/admin/agent-deliveries/{message_id}`，可查看 `delivery_status`、`attempts`、`reported_at` 和 `handler_result`。没有 `handler_result` 时，服务端尚未收到执行报告，例如客户端在 ACK 后崩溃或停机宽限期耗尽。无匹配 Handler 会在 ACK 前 NACK，原因 `no_matching_handler` 同时出现在客户端日志和投递审计。通用消息、投递审计及执行报告随终态记录保留 7 天。OMA 结果继续使用 `/api/v1/admin/analysis-runs`。

## 验收路径

共享队列验收覆盖入库与批次事务、路由、并发领取、租约、TTL、重启恢复和 LISTEN 断连兜底。独立 Client 验收覆盖 TLS、ACK 后 exec、退出码、超时、报告重试与管理员只读查询。OMA 验收需记录队列 ACK、运行账本、本轮证据和最终报告，具体步骤见 [OMA 运行文档](../../../docs/ops/oma-runtime.md)。容量 p95、队列监控告警、人工重投及取消仍由 Issue #9 跟踪。
