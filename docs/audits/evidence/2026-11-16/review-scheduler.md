# I2 调度独立审查

**协调者状态索引（cdfe1bd）：已报告 P2 已复验关闭，未发现剩余阻断项。以下保留原始审查与后续复验记录；最终证据见末节。**

审查提交：`b063d0dd4775039294219c13e6de16674183efb9`，比较基线 `63778bf`。目录 `/private/tmp/alert-failure-scheduler`。已读 I2 计划、实现 diff、调度/状态/配置/API/生产停机入口与验收文档。仅只读 Git；未修改源码、未提交、未运行固定 E2E。

## 结论

发现 1 个 P2，未发现可复现 P0/P1。建议修复 readiness 的完成边界再交付，避免合法长任务造成 readiness 短暂误报。

### [P2] 合法长任务完成后成功进度时间戳未刷新，readiness 短暂变红

位置：`internal/scheduler/status.go:128-139`（`workerComplete`），关联 `status.go:167-171` 与 `scheduler.go:464`。

触发条件：普通 worker 执行时长超过 `max(3 * pollInterval, 1s)`、但未超过合法 ExecutionTimeout。`workerStart` 将 `LastPollSuccessAt` 更新为**开始时刻**；在途时 `WorkerStatuses` 使用 ExecutionTimeout+settle 判据，因此 ready 为 true。完成时 `workerComplete` 清空 StartedAt/Kind，只记 LastCompletedAt，没有更新 LastPollSuccessAt。随后立即改走 idle 分支并用早已过期的 LastPollSuccessAt，正常完成的普通 worker 就变 degraded，`NormalWorkersReady` 返回 false，直到下一次成功 PG 查询才恢复。

后果：恰好落在正常完成至下一次取库成功间的 `/readyz` 探针得到 503；较慢数据库下窗口会扩大，多个普通 worker 下任一个落在该状态都会误报。该问题不导致 claim 丢失，但会给健康系统引入不必要的 readiness 抖动。

复现未修改仓库：使用 Go overlay 在临时文件注入 `TestReviewCompletedNormalTaskReadiness`，构造执行 2 秒、pollInterval=10ms 的普通 worker。断言合法在途 ready；调用 workerComplete 后断言 not-ready，命令退出 0 并输出复现信息。

```bash
GOCACHE=/private/tmp/alert-correctness-gocache go test \
  -overlay /private/tmp/alert-iteration-issues/scheduler-overlay.json \
  ./internal/scheduler -run TestReviewCompletedNormalTaskReadiness -count=1 -v
```

结果：`PASS`，`ok ... 0.705s`；日志“合法长任务在途 ready，完成清除 StartedAt 后立即 not-ready，直到下轮成功取库”。探针源文件 `/private/tmp/alert-iteration-issues/scheduler_readiness_probe_test.go`，overlay JSON 同目录。

建议：在**确定成功完成进度**的路径记录完成时心跳，或让健康判断在 idle 时考虑最新成功完成时刻；不要无条件把 aborted DB 事务/失败查询视为成功。补充“合法长任务完成→下一次取库完成前仍 ready”和“持续失败查询最终 not-ready”两类回归。

## 已核对的关键路径

- 两池查询使用 detached 注册表筛选；普通池反集合、通知池正集合，不依赖普通 kind 枚举。claim 发生在 worker 当前槽位内，事务提交后同步执行 detached，无预领取内存排队。
- 运行时新注册 detached kind 在普通 worker 分发前再次核对，避免查询快照过期带入普通 IO；map 读取/注册持相同 RWMutex。未增加注销/切回普通模式能力。
- Configure/Run 用 configMu 与 running 协调，运行时拒绝配置修改；Options 使用锁快照。NormalWorkersReady 使用各 worker 状态而非 LastPollSuccessAt 共享原子心跳。
- Run 执行 context 同时限制 DB 领取和 handler；最大 240s 加独立 settle 15s 小于固定 300s lease。handler 超时返回错误保留 claimed；成功后取消仍 settle；ctx 取消普通事务不改 failed。
- 有错误时 runWorker 按 pollInterval 退避；成功处理立即取下一条。未发现失败查询的零等待忙循环。
- 管理状态端点有 sessionAuth+requireAdmin，3 秒查询上限；返回不含 ref_id/payload/渠道目标。kind 聚合 overdue/oldest_wait 包含到期 pending 和过期 claimed，重领/失败是留存历史口径。
- main 在取消或 HTTP 错误后统一取消 worker，执行 HTTP shutdown 后等待 scheduler，超时明确报错；不会无限等待不遵守 ctx 的 handler。

## 独立执行的验证

```bash
GOCACHE=/private/tmp/alert-correctness-gocache go test -race \
  ./internal/scheduler ./internal/config ./internal/api ./internal/app -count=1
# 全部退出 0：scheduler 1.364s / config 1.839s / api 14.013s / app 1.465s

GOCACHE=/private/tmp/alert-correctness-gocache go test -tags=integration -race \
  ./internal/scheduler -run 'TestScheduler_RealSender|TestScheduler_Run_' -count=1 -v
# 退出 0，22.439s，真实 testcontainers PostgreSQL 15
```

真实 PG 覆盖：通知两槽占满且第三条留 pending，普通未来 kind 继续运行；执行 deadline 保留 claim、过期重领 claim_count 增长；真实 Sender 最终失败与取消后重领恢复。所有命令均真实结束，未把后台状态当成通过。

## 限制

- 独立重跑 focused 集成及四包 race，没有重跑 scheduler 所有旧集成、全仓/固定 Compose E2E、生产 HTTP 停机端到端测试。
- 新增 overlay 复现是确定性的状态转换测试，不宣称测得真实生产 readiness 误报频率；未修改该提交的测试文件。
- 最大超时与租约关系按代码边界审查，没有实际等待 300 秒租约；ctx 合作取消、外部 IO 已发生但 settle 未提交的 at-least-once 窗口仍是既有明确限制。
- 状态聚合未使用生产队列规模压测，无法由本轮测试推导真实容量上限。

## 修复独立复核：cdfe1bd

复核日期：2026-11-16。主 Agent 指定修复提交 `cdfe1bd`，相对 `b063d0d`；复核目录 `/private/tmp/alert-failure-scheduler`。本次未运行 Git、未修改工作区源码、未运行 E2E；仅阅读实现并运行 focused 单测/隔离真实 PG race，以及位于 `/private/tmp/alert-iteration-issues` 的只读 Go overlay 探针。

**结论：原 P2 已修复，未发现新增阻断项，可继续集成验收。**

- `workerComplete(w, persisted)` 仅在确认业务事务成功提交或 detached 终态成功 settle 时更新 `LastProgressAt`。idle 健康判断采用 `max(LastPollSuccessAt, LastProgressAt)`，合法长任务完成后即使下次取库尚未完成，也不会立即掉入过期的开始时间。
- 普通事务提交失败、失败查询均不产生新的完成进度；开始一次取库只更新 `LastPollAt`，不能无限延长旧成功完成的有效期。真实 PG 用 `SELECT 1/0` 使事务 aborted，再以成功的独立失败收口事务证明其确实未被算成成功进度；重命名 timers 的查询错误路径也未伪造进度。
- detached 结果仅以成功提交且 fencing token 仍有效的 settle 为完成进度：成功 fired 和已持久化的业务 failed 都代表调度取得持久进展，业务 failed 仍在状态接口可见；取消保持 claimed、settle 数据库失败、过时 fencing token 都不更新完成进度。成功后取消仍用有界 WithoutCancel settle fired，未丢失原恢复语义。

### 独立执行命令及结果

```sh
GOCACHE=/private/tmp/alert-notify-gocache go test -race ./internal/scheduler ./internal/api -count=1
# 退出0：scheduler 1.288s / api 6.705s

GOCACHE=/private/tmp/alert-notify-gocache go test -tags=integration -race ./internal/scheduler \
  -run 'TestScheduler_CompletionProgressRequiresDatabaseSuccess|TestScheduler_DetachedHandler_(CancelDuringHandler|SuccessThenCancel|HandlerError)|TestScheduler_SettleClaim_StaleToken|TestScheduler_RealSender' \
  -count=1 -v
# 退出0：26.769s；日志 scheduler-cdfe1bd-review-tests.log

GOCACHE=/private/tmp/alert-notify-gocache go test -tags=integration -race \
  -overlay /private/tmp/alert-iteration-issues/scheduler-progress-overlay.json \
  ./internal/scheduler -run TestReviewDetachedCompletionProgress -count=1 -v
# 退出0：11.845s；五个子场景全部实际执行
```

overlay 源文件 `/private/tmp/alert-iteration-issues/scheduler_progress_probe_test.go`。它确定性构造 handler 已运行 2 秒且开始心跳过期的完成边界，直接执行单次 `processOneDue`，避免下一次成功查询掩盖问题。五个真实 PG 子场景为：sent、business_failed、cancelled、settle_db_failed（在 handler 返回后让 settle 查不到 timers）、stale_fence（发送期间增加 claim_count）。断言成功 settle 后 LastProgressAt 非零且不降级，其余三个场景 LastProgressAt 保持零且在旧轮询时间过期后降级。所有故障注入仅作用于 testcontainers PostgreSQL 15；每个容器正常清理。

本次仅复核完成边界修复及相关取消/数据库/fencing 路径，没有重新执行全仓 integration、固定 Compose E2E 或生产停机演练；这些仍属于主 Agent 集成验收范围。
