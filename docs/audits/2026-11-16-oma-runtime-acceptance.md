# OMA 持久分析运行时验收

日期：2026-11-16。范围：Issue #24（Refs #22），运行时、结果 API、迁移 33、既有队列 AckTx 原子接收。部署优先复用现有 SRE API/平台配置，不创建 Kubernetes worker/Secret。本记录不把本地 fake OMA 的通过解释为线上 OMA 或 IAC 故障已验收。

## 实际执行

工作树 `/private/tmp/alert-iac-runtime`；独立缓存 `GOCACHE=/private/tmp/alert-notify-gocache`。

| 命令 | 结果与证据 |
| --- | --- |
| `go test -race ./internal/omaruntime ./internal/api ./internal/agentdelivery -count=1` | 通过，1.563s / 6.524s / 2.167s |
| `go test -overlay /private/tmp/alert-iteration-issues/runtime-review-overlay.json -tags=integration -race ./internal/omaruntime -run 'TestReviewFailedModel\|TestReviewRequiredCoverage' -count=1 -v` | 两项原独立失败复现修复后通过，11.416s；`/private/tmp/omaruntime-review-retest.log` |
| `go test -tags=integration -race ./internal/omaruntime ./internal/agentdelivery -run 'TestRuntime\|TestWorkerReadiness\|TestDeliveryQueue\|TestLostPollAndCrashAfterAck' -p=1 -count=1 -v` | 通过，runtime 23.607s、queue 9.453s；`/private/tmp/omaruntime-acceptance-pg.log` |
| `go test -race ./internal/... ./cmd/... -count=1` | 首轮失败：broker 三项 CLI fixture 的 1 秒超时，runtime/API/queue 等通过；保留 `/private/tmp/omaruntime-final-unit.log`，未放宽断言 |
| `go test -race -p=1 ./internal/... ./cmd/... -count=1` | 全部通过；`/private/tmp/omaruntime-final-unit-serial.log` |
| `go build ./...` / `go vet ./...` | 通过；仅构建/静态检查，不运行 E2E |
| `go test -c -o /private/tmp/alert-oma-e2e.test ./test/e2e` | 编译通过，未运行固定 Compose E2E |

## 失败模式覆盖

| 场景 | 验证 |
| --- | --- |
| 收件后 ACK 前进程崩溃 | 真 PG terminate backend：整笔事务回滚；过期队列租约经 Reap 重领，账本不留 receiving 自旋；新 receipt 原子 ACK 后恢复 |
| ACK 后重启 | 从持久运行账本续接，同 message 唯一；ACK 与报告完成独立 |
| 已提交会话但 checkpoint 丢失 | 从 submitting 动作、唯一 run_id/title 查回；Create 不重复 POST |
| 外部提交结果未知 | session 或 user.message submission_id 对账；查不回保留 submission_unknown，不盲重发 |
| SSE 断连/GET 补漏 | HTTP fixture 覆盖真实 stream 路径、headers、comment/data 帧及 cursor；PG 事件 ID 去重；GET 分页完整补漏 |
| 模型 503/超时/异常终止 | 有界 round 后 partial；session.error/terminated/异常 idle 优先于已观察的 JSON/end_turn，不能 completed/healthy |
| 取证失败/未知 | 证据持久化 error/unknown；禁止引用为成功事实；同会话第二轮回灌 |
| 共享五地/VIP观测缺口 | 必需 coverage 在批准交集内补齐；不在 allowlist 则不执行并持久 coverage_gaps；不能健康 |
| DatasourceError / DatasourceNoData | 只来源/新鲜度观测，独立降 partial/unknown，不能把原 rule_id 当业务退化证明 |
| 并发/背压 | 2 槽只接收 2 条；第三条保持 PG pending；两槽都需真实进度；取消等待模型/订阅 goroutine 退出 |
| lease fencing / 报告幂等 | 旧 token 不能回写；同 run 同内容幂等，不同报告哈希拒绝覆盖 |
| 关联与领域隔离 | 复用 OccurrenceRef；123456789ns Grafana startsAt 经 PG 微秒归一化正确关联 Alert/Fault；Fault active 保持，无领域结果写路径 |
| 结果读取与脱敏 | API session/admin RBAC、READ ONLY、UUID/limit、错误码、数组形状；不返回 receipt/envelope/事件原正文；HTTP 不跟随重定向，防 API key 跨域 |
| readiness | loop 入口不记成功；缺 schema/Claim失败不刷新；成功空轮询、成功 lease renewal、长任务完成刷新；每槽独立有界 stale |

独立审查另在 `/private/tmp/alert-iteration-issues/review-iac-runtime.md` 记录：真 PG/race 双槽不互相掩盖、真实 Renew、DB行锁阻塞至租约失效 33.715s；加强异常终止与 coverage 拒绝持久 gap 9.850s，均通过。该 overlay 在工作树之外，由独立审查 Agent 维护。

## 尚未验收及部署边界

- 本地 PG fixture 按 SQL 顺序建表，不等于 golang-migrate ledger；主 Agent 必须实际验证 32→33→34 以及 down/up，不能先发只有34的迁移镜像再补33。
- 主 API 结果路由与可选 runtime 启动由主 Agent 联合装配。宿主复用 `LoadConfig(get)` / `NewConfiguredWorker(st,cfg)`，管理 Run 的取消与 join；分析 Ready 单独呈现，不能把外部模型故障变成既有通知 API 不可用。
- 未读取/修改线上凭据、未创建基础设施、未发送真实告警。主 Agent 负责专用 canary 的 Grafana→SRE→OMA SSE→IAC 只读取证→同会话结构化报告→只读 API 关联验收。
- 已知 OMA SSE 接口真实存在由前序只读证据支持；Last-Event-ID 历史回放未证明，本实现依赖 PG event ID 去重与 GET 分页补漏，不能宣称服务端可重播或外部 POST 物理恰好一次。
- 固定 Compose E2E 仅编译；由主 Agent 串行执行全链路。真实 IAC 故障归因不由 canary 推导。结果长期保留策略需后续显式制定，不随角色队列 7 天清理级联删除。
