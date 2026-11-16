# Fault 通知可靠性验收

日期：2026-11-16。范围：Issue #11 / I1，基于 `63778bf`。本记录由通知实现 Agent 编写；未执行 Git、发布、真实团队通知或固定 Compose E2E。

## 最终行为

- `fault_notification_watches` 只在真实激活回调或管理员显式提交时登记。默认 reprocess 继续静默；补映射后管理员需要预览并提交，不能等待后台全库扫描偷偷补发。
- `fault_notification_deliveries` 按 Fault 激活 epoch、渠道和目标哈希唯一。持久 UUID + generation 是发送器和 timer 的稳定幂等身份，扫描时不生成新 round；重新进入 active 建立新 epoch，旧 epoch 的 receipt/result 不更新当前送达状态。
- 状态包含 queued、sending、sent、failed、covered、cancelled。`sent` 只表示渠道接口接受，不能推断人工已读或 ACK。多目标分别记录，至少一个 sent 才更新当前 `owner_notified_at`，该汇总字段从不参与覆盖判断，历史字段不回填为送达。
- 上游只有当前 epoch、active、相同渠道及相同目标的 sent 才覆盖下游的这个目标。排除自身；不同目标不互相抑制。上游 queued/sending 不造成暂缓，缺省没有等待 SLA 或无限等待；因此无须为 pending 配置一个未经确认的释放时限。
- 冷却与无目标持久记录 reason/next_check_at。每分钟一次 DB 批次扫描、每批最多 100 个 watch；只处理仍 active 且未 ACK 的 Fault。进程重新装配后继续原有 watch。相同阻塞原因不逐分钟重复写时间线。
- 每个 watch 使用保存点隔离，单项失败记录 `reconcile_error` 并延后，其他项和下一轮仍可提交。主进程与 E2E harness 都调用启动对账，且注册第 13 个普通 timer kind `fault_notification_reconcile`。
- 丢失 pending/claimed timer 的 queued/sending 收敛为带 `orphaned_timer` 审计的 failed；claimed 即使租约过期仍可恢复，不误判成孤儿。不自动重试终态 failed；管理员显式 `retry_failed=true` 最多增加 2 个 generation，每个 generation 仍使用 Sender 既有有限重试。
- 锁顺序为 Fault → watch → delivery → timer；候选批次不预锁 watch。外部 IO 仍只在事务外 Sender 执行。HTTP 已发送而结果事务尚未提交的崩溃窗口仍是 at-least-once，不能声称物理恰好一次。

## 管理入口

三个端点均使用现有 `sessionAuth + requireAdmin`：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/api/v1/admin/faults/:id/notifications` | 状态、watch_reason、next_check_at、历史目标级结果 |
| POST | `/api/v1/admin/faults/:id/notifications/preview` | 只读预览；使用数据库 READ ONLY 事务 |
| POST | `/api/v1/admin/faults/:id/notifications/submit` | 锁定 Fault 后重新加载状态/渠道，显式补投 |

预览与提交使用同一只读 evaluate 计算渠道、冷却与上游；预览目标 action 可为 queue、covered、wait_cooldown、retry 或既有状态。补投失败目标需显式查询参数 `?retry_failed=true`。已认领/非 active 返回对应原因，不新增投递。每个请求只处理 1 个 Fault，不能提交无限批次。重复无变化提交不重复追加 notify_backfill 审计；首次登记、结果变化或实际重投保留操作人审计。

目标原文只用于既有发送 payload / 发送器内部存储，管理结果和新时间线一律显示 `[redacted]` 或目标哈希，不输出 webhook URL。输入 UUID 校验失败返回 400；未登录 401，非管理员 403，不存在 404。

## 已执行验证

初始回归在修改产品代码前真实失败：

```sh
GOCACHE=/private/tmp/alert-notify-gocache go test ./internal/app -run 'TestFaultOutboxQueueFaultActivated_(UpstreamSuppressed|OwnerChannelFallback)' -count=1
```

旧实现分别报「历史 owner_notified_at 不能作为送达覆盖证明，下游必须入队」与「入队不能回写 owner_notified_at」。默认 GOCACHE 首次被沙箱拒绝后使用独立临时缓存；错误已记入本 worktree `.learnings`。

最终命令：

```sh
GOCACHE=/private/tmp/alert-notify-gocache go test ./internal/app ./internal/api ./internal/notify -race -count=1
GOCACHE=/private/tmp/alert-notify-gocache go test -tags=integration ./internal/app -run 'TestFaultNotification' -race -count=1 -v
GOCACHE=/private/tmp/alert-notify-gocache go build ./...
GOCACHE=/private/tmp/alert-notify-gocache go vet ./internal/app ./internal/api ./internal/notify ./internal/store
```

Focused PG 测试使用现有 testcontainers harness 的 PostgreSQL 15。实际三个测试组均执行、未跳过，最终退出 0。详细本地输出：`/private/tmp/alert-notify-focused-test.log`。

| 验收项 | 测试/证据 | 结果与边界 |
| --- | --- | --- |
| N1 上游入队/失败不构成送达 | 旧实现红灯；CoverageAndCooldown 的上游 queued + 下游 queued；Reliability 的实际 Sender 失败不写 owner_notified_at | 通过；缺省不等待 pending，所以失败释放不依赖 fallback |
| N2 不同目标、有效上游、排除自身 | CoverageAndCooldown 使用含 root 的拓扑查询，真实 PG Fault 查询、相同目标 covered、不同目标 queued | 通过 |
| N3 部分成功 | CoverageAndCooldown 两目标实际 fake Channel 一成功一失败，状态计数 sent=1 / failed=1，JSON 不含目标原文 | 通过 |
| N4 预览/补投/静默 | Reliability 在 READ ONLY 真事务预览后 watch 仍为空；8 个并发 Submit 只创建 1 个 delivery、1 条补投审计；reprocess 原入口无通知调用保持不变 | 预览/补投通过；真实映射回放组合 E2E 未在本子任务运行 |
| N5 无目标/冷却/重启/ACK | Reliability 更换 Outbox 实例后补渠道对账恢复，ACK 对象不补投；CoverageAndCooldown 冷却到期恢复 | 通过；重建实例验证持久恢复，未做 OS kill 注入 |
| N6 幂等/并发/有界失败 | 8 路补投、已 sent 重发短路、最多两轮显式重试、孤儿 sending 回收、24 路结果/补投/对账竞争，10 秒 deadline；坏 watch 不阻断好 watch | 通过，并带 race；保留外部 IO 的 at-least-once 边界 |
| epoch 隔离 | CoverageAndCooldown 激活新 epoch 后旧 receipt/result 不更新当前 owner_notified_at；Covered SQL 与 watch 当前 epoch JOIN | 通过 |
| RBAC/目标脱敏 | API TestFaultNotificationsRBAC；真实 PG preview JSON、时间线固定脱敏字段 | 通过 |
| N7 迁移 | Reliability 空库全迁移、新迁移 SQL down/up 保留 faults；主 Agent 的真 ledger 结果见下文 | 通过，ledger 由主 Agent 执行 |

主 Agent 已执行 PostgreSQL 16 + 缓存 `sre-control-migrate:issue9-63778bf-offline`（Dockerfile 固定 v4.18.3；该镜像 `-version` 显示 `dev`，未验证运行二进制版本）的真实 ledger 验证，回执文件 `/private/tmp/alert-iteration-issues/migration-results.json`：空库 → 32、29 → 32、31 → 32 全通过；每个场景 down 1 到 31 再 up 到 32 通过，29/31 已有 teams 哨兵行保留。实现 Agent 已读取该结果文件；执行脚本和完整迁移证据由主 Agent归档。迁移 32 在此验证后未修改；没有新增或回填 28。

## 未执行与限制

- 未运行 `test/e2e` 固定 Compose 全套、全仓 integration/race、前端构建及 CI；按分工由主 Agent 集成验收。
- 未对真实飞书/邮件、线上 UAT/PRO 或真实 SLA 做验证。所有出站调用仅使用进程内 fake Channel。
- 未做真实进程 kill 或多主机外部 IO 故障注入；恢复测试通过重建服务对象与持久状态故障注入实现。
- 新 watch 只覆盖已登记对象，历史静默回放结果需要管理员显式补投。旧 owner_notified_at 不能用于推算历史送达率。
- 回滚 32 会删除新的 watch/delivery 元数据，因此 down/up 只验证结构及原有领域数据保留，不承诺删除后的投递元数据仍可恢复。实际回滚须停止新版本发送器并由运维评估在途通知。

## 独立审查后的激活版本修复

独立审查发现原实现把 Mapper 的 EnteredActive + 当前时间当成新激活依据，可被两个并发事务读到同一旧 candidate 的时序重复触发；同时静默 Project 真正重激活时会错误复用旧通知结果。此处不再依赖 Transition 判断激活身份：迁移 32 增加 `faults.activation_version` 和数据库 BEFORE INSERT/UPDATE 触发器，只有实际 non-active → active 才递增并清除当前 owner_notified_at。通知 epoch 直接使用该领域版本；watch 只在真实通知入口或显式提交登记，静默新版本不会被旧 watch 自动扫描。Due 取候选之后还会在 Fault 行锁内重新比对版本，以覆盖候选读取与静默重激活之间的竞态。

新增真实 PG 回归 `fault_notification_activation_integration_test.go`：使用 Scan 后的同步屏障让一个 Mapper 固定持有旧 candidate 观测；另一个 Mapper 投影、提交并实际发送后，才释放旧事务。严格断言实际发送一次、delivery 总数一、领域与 watch 都保持同一个版本。另测 monitoring → 静默 Project 重激活后，旧 due watch 和过期扫描候选均不能自动补投；Preview 显示新意图，显式 Submit 后才实际发送第二轮。

迁移 32 已因此修改，前文主 Agent 的旧 ledger 结果不能作为此最终结构的验证结论；需要主 Agent 重新执行空库/29/31升级与 down/up 后更新证据。普通单测、focused PG race 与 build/vet 将随本修复重跑。

N4 HTTP E2E 已补 `TestFaultNotify_SilentReprocessPreviewAndBackfill`：真实 webhook 无规则→补规则→HTTP静默reprocess→HTTP GET/POST预览无数据库副作用→显式提交→等待fake渠道sent→重复提交保持相同delivery/timer/审计且目标脱敏。旧E2E timer计数改为持久payload GroupKey，拓扑覆盖改为等待真实delivery sent后再输入下游。已用 `go test -c -o /private/tmp/alert-notify-e2e.test ./test/e2e` 编译通过；未运行固定Compose，由主 Agent 串行执行。

激活版本修复后的本地最终回归已实际完成：上述 `TestFaultNotification*` 五组真实 PG + race 全部通过，包耗时 18.712s，输出 `/private/tmp/alert-notify-version-test.log`；app/api/notify 的普通 race 全部退出 0（1.477s / 6.682s / 2.100s）。E2E 仅编译，未启动 Compose。最终 `go test -c`、`go build ./...` 与 focused `go vet` 均已实际结束并退出 0。


主 Agent 已完成新版迁移 32 的真实 ledger 复验并返回全通过，结果 `/private/tmp/alert-iteration-issues/migration-results-final.json`：空库/29/31 → 32 及 down/up；旧 Fault 与历史 owner_notified_at 保留、不误回填 watch/delivery；普通更新不增加激活版本；monitoring → active 恰好加一且清 owner；down 移除新增列/触发器，再 up 保留 Fault。此最终回执取代前文旧迁移结果，最终迁移结构已冻结。
