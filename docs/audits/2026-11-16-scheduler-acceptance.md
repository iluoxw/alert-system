# 调度容量隔离验收记录

日期：2026-11-16。范围：Issue #12 / 迭代 I2。实现目录：`/private/tmp/alert-failure-scheduler`。基线由主 Agent 提供为 `63778bf`，本 Agent 未执行 Git 操作、提交、推送或远程部署。

## 实现结果

- `Run` 启动普通池与 detached 池，每个 worker 同步执行一条任务，结束后才领取下一条。无任务级 goroutine，无已领取任务的内存等待队列。普通池排除 detached 注册表中的全部 kind，detached 池只领取注册表中的 kind；新增普通 kind 无需修改枚举。运行中注册 detached kind 时，普通 worker 在执行前再次检查，避免注册快照变化把 IO 带入普通池。
- 默认普通 worker 为 1，通知 worker 为 2；保留普通事务的默认串行行为。每池可配置 1–32 个 worker。这是工程容量选择，不构成通知或事故响应 SLA。
- 默认执行超时为 120 秒，可配置 1–240 秒；空闲/错误轮询间隔可配置 1 毫秒至 1 分钟。单次数据库领取和 handler 执行共用有界 context；settle 使用独立的 15 秒 context。执行上限 240 秒 + settle 上限 15 秒小于固定 300 秒租约，保留 45 秒余量。没有增加租约续期机制。
- 保留 claim 提交后执行 IO、`claim_count` fencing、过期重领、panic/业务失败收口。执行 context 超时或进程取消且 handler 返回错误时保留 claimed；handler 返回成功后，即使同时取消也继续有界 settle。普通事务被取消时回滚并保持 pending，不额外把取消任务标成 failed。
- 每个 worker 有独立轮询尝试/成功、执行开始、最近完成、当前/最近耗时和 degraded 状态；普通 worker 逐个参与 readiness，通知池不参与 readiness。普通池正常在途任务允许执行到 deadline + settle，避免把合法执行当作轮询死亡。停机等待全部 worker，生产入口取消后最多再等待 20 秒；未遵守 context 的 handler 仍可能触发退出等待超时。
- 同一 Scheduler 实例只允许一个 Run，避免重复调用意外扩容；不同实例仍通过 PG 行锁协调。退出状态可查询，下次 Run 清空历史 worker 状态，不把旧 stopped worker 永久混入健康判定。

## 配置与状态接口

| YAML | 环境变量 | 默认值 | 合法范围 |
|---|---|---|---|
| `scheduler_normal_workers` | `SCHEDULER_NORMAL_WORKERS` | 1 | 1–32 |
| `scheduler_notify_workers` | `SCHEDULER_NOTIFY_WORKERS` | 2 | 1–32 |
| `scheduler_execution_timeout` | `SCHEDULER_EXECUTION_TIMEOUT` | 120s | 1s–240s |
| `scheduler_poll_interval` | `SCHEDULER_POLL_INTERVAL` | 5s | 1ms–1m |

`config.Load` 拒绝配置文件/环境变量显式设置的非法值，包括 worker 数为 0。内部 `Scheduler.Configure` 的零值字段表示沿用默认值，兼容测试和直接构造 `config.Config` 的既有调用方；负值与越界值仍报错。Configure 只允许在 Run 停止时调用。

新增 `GET /api/v1/ops/scheduler`，使用既有 sessionAuth + requireAdmin；未登录返回 401，普通成员返回 403。响应只包含 pool/worker 状态和按 kind 聚合：pending、overdue、claimed、failed、reclaimed、oldest_wait_seconds。overdue 包含到期 pending 和租约过期 claimed；最老等待按各自可领取时刻计算。reclaimed 为留存记录的 `sum(max(claim_count-1,0))`，failed 同样按留存记录计数，均会随 retention 清理，不是永久累计值。空集合返回 `[]`。查询 context 最长 3 秒，查询失败只返回通用错误。

接口不读取或返回 `ref_id`、payload、渠道目标地址、凭据或数据库错误原文。正常慢发送可从在途时长及池占用看见；超过执行与 settle 边界的 worker 标记 degraded。接口需要管理员主动查询，外部告警规则尚未配置。

## 回归证据

1. **先红后绿**：首先新增真实 PostgreSQL 回归 `TestScheduler_Run_IsolatesBlockedNotifications`。用通知 handler 开始屏障确认旧单 Run 已被占住，再插入普通 kind；旧实现在 1 秒内未执行普通 timer，报 `慢通知阻塞了普通 timer`。最终回归用 3 条通知填满默认 2 个通知槽，断言第三条仍为 pending、恰好 2 条 claimed，同时新的 `future_normal_kind` 在 1 秒内执行。时限是此测试的回归判据，不外推为生产 SLA。
2. **S1/S2**：满通知池时普通任务继续执行；PG 中 claimed 数等于配置并发，剩余任务留在 pending。状态聚合与实际在途数一致，普通池 readiness 保持可用。
3. **S3/S4**：单元测试分别覆盖正常空闲、正常慢执行、单 worker 超界、另一 worker 心跳仍正常、停止状态。API 回归覆盖管理员访问、成员拒绝、未登录拒绝、空集合和敏感字段排除；普通池不健康影响 readiness，通知池 degraded 不影响持久化入口。
4. **S5**：既有真实 PG 用例覆盖 claim 提交/行锁释放、普通 handler 行锁保持、发送失败、panic、过期重领、并发不重复领取、活租约不被抢占、旧 fencing token 不覆盖新结果、取消保留 claim、成功后取消仍 settle。新增 Run deadline 回归证明 1 秒超时后仍 claimed，推进测试时钟跨过租约后可重领为 fired，token 从 1 增至 2，重领计数正确。
5. **真实 Sender 联动**：`TestScheduler_RealSender_FailureAndCancellation` 使用真实 Sender、PG notification_log 与 fake channel。最终渠道错误使 timer failed，证明 Sender 不吞发送错误；context 取消后 timer 保持 claimed，过期重领后同一 notification_log 成功变为 sent。
6. **S6**：scheduler 全包 integration race 通过；新增 Run 与真实 Sender focused integration race 通过；scheduler/config/api/app 单元 race 通过。

## 实际验证命令与结果

测试缓存显式置于可写临时目录；首次默认 Go 缓存不可写的问题已记录在 `.learnings/ERRORS.md`。所有通过项均真实退出 0，无 skipped 替代通过。集成测试由 testcontainers 启动真实 `postgres:15-alpine` 临时数据库；未运行固定 Compose E2E。

```bash
# 最初回归：在实现前失败，错误为慢通知阻塞普通 timer
GOCACHE=/private/tmp/alert-failure-gocache go test -tags=integration ./internal/scheduler -run TestScheduler_Run_IsolatesBlockedNotifications -count=1 -v

# 已通过，scheduler 85.529s
GOCACHE=/private/tmp/alert-failure-gocache go test -tags=integration ./internal/scheduler -count=1 -v

# 已通过，scheduler 80.443s；执行时包含满池、deadline 回归，真实 Sender 用下一条补测
GOCACHE=/private/tmp/alert-failure-gocache go test -tags=integration -race ./internal/scheduler -count=1

# 最终 focused 集成 race 已通过，scheduler 13.232s
GOCACHE=/private/tmp/alert-failure-gocache go test -tags=integration -race ./internal/scheduler -run 'TestScheduler_RealSender|TestScheduler_Run_' -count=1 -v

# 已通过，scheduler/config/api/app 均返回 ok
GOCACHE=/private/tmp/alert-failure-gocache go test -race ./internal/scheduler ./internal/config ./internal/api ./internal/app -count=1

# 已通过
GOCACHE=/private/tmp/alert-failure-gocache go build ./...
GOCACHE=/private/tmp/alert-failure-gocache go vet ./internal/scheduler ./internal/config ./internal/api ./internal/app ./cmd/server
```

完整集成日志位于本机会话临时目录 `/private/tmp/alert-scheduler-integration.log`、`/private/tmp/alert-scheduler-race.log`、`/private/tmp/alert-scheduler-focused-final.log`，不应当作永久发布附件。

## 边界与待验收

- 无数据库迁移；未修改 go.mod、`.spec-workflow`、其他 worktree、生产数据或渠道配置。I1、I3 的集成、独立复核、后端全量测试、全仓 vet、固定 Compose E2E、PR 和 CI 由主 Agent 负责，本记录不宣称已完成。
- 现有项目集成基础设施使用 PostgreSQL 15 临时镜像；生产 PostgreSQL 16 的集成验证由总体验收补充。
- 新故障回归使用 fake channel，但 Sender、PG、事务、租约和 fencing 均是真实实现；未验证真实飞书/邮件/公网超时，不宣称线上渠道送达率改善。
- context 超时依赖 handler/底层 IO 遵守取消；Go 无法强制终止忽略 context 的同步 handler。本轮不通过 fire-and-forget 掩盖此限制。网络已发、settle 未提交时仍可能重发，保留 at-least-once 边界。
- 管理聚合查询扫描当前留存 timers；已设置查询超时，尚未基于真实生产队列规模做容量基线或索引评估。最老等待、失败和重领可以查询，但外部监测采集与告警阈值尚未部署。
