# 交接说明 — entity-lifecycle

> **状态（2026-09-21）**：10/10 已交付并合入 main。v0.3.1 发布本 spec；v0.3.2 补了控制中心列表的行内下线入口（仅 Web）。不要按下文「尚未开工」再实现一遍。
>
> 交接时点：2026-09-20。下文是开工前交接，保留作背景。
> 接手 Agent 负责实现，主对话负责 review。**开工前完整读完本文件，再读 `requirements.md`、`design.md`、`tasks.md`。**
> 三份规格是权威；本文件补的是规格文本之外的来龙去脉、环境现状与已知的坑。

---

## 1. 这个 spec 从哪来

不是设计推演，是 uat 上一次真实事故卡住之后倒逼出来的。

**事实链**（2026-09-20 实测，数据来自 uat 库）：

1. 节点 `10.61.17.11`（`p-ai-sandbox-001`）**已从 E2B 集群剔除**——用户确认的现实，不是系统推断。
2. 采集目标随之消失，三条证据告警先后"自动解除"：`E2BNodeExporterDown`（09-14）、`E2BNodeExporterCoverageGap`、`E2BNomadAgentsCoverageGap`（均 09-19）。
3. INC-000001 因此卡在 monitoring **195h56m**，`incident_recovery_nudge` 每 6 小时提醒一次，已 fired 12 次。
4. 提醒本身是对的（不变量 11 的设计意图：假恢复必须人工确认），但**人没有可执行的出口**——系统根本没有"已下线"这个状态。
5. 实体至今以受管身份留在拓扑里，`last_observed_at` 停在 09-12；受管实体 API 只有 `POST /entities` 与 `PUT /entities/:id`，**无删除、无状态字段**。

一句话概括问题：**只要实体没有终态，「告警消失」就永远是歧义信号**。这正是 nudge 必须存在的原因，也是它无法闭环的原因。

## 2. 边界：什么不是这个 spec 要解决的

接手时最容易跑偏的方向，逐条挡住：

| 不做 | 为什么 |
|---|---|
| 资产管理 / CMDB 台账 | 产品边界明确不是通用 ITSM（见 `CLAUDE.md` 产品定位） |
| 自动发现下线 | 遥测消失**永远**不能推断为下线——这是整个 spec 的前提，做了就自相矛盾 |
| 实体删除 | `faults.entity_ref`、别名、历史边都指向它，删除会让历史悬空 |
| 自动关闭 Incident | 不变量 11 的双重门槛与人工确认不可绕过；下线只能收口 Fault |
| 新的 timer kind / 外部 IO | 不变量 23；本 spec 全程无外部 IO |
| 新增一级导航 | 不变量 19；入口收在控制中心的实体 / 服务目录下 |

## 3. 环境现状（开工时可能已变，先自行核对）

- **main / era3 均在 `v0.3.0`**（tag 指向含 VERSION 提升的提交）。uat 已部署 0.3.0，schema 到 `27`。
- **迁移编号**：`000026 / 000027` 已用，**`000028` 预留给 era3-fault-workunit 任务 28**。本 spec 从 **`000029`** 起。
- uat 数据量很小（5 个实体、3 个 Fault、1 个 Incident、77 条告警全 resolved），适合直接拿来验证本 spec 的场景。
- uat 上**所有实体都有 `owner_team_id`**，但 `team_ops_settings` 里**配了渠道的团队 = 0**，`FAULT_NOTIFY_FALLBACK_TARGET` 也没配——所以 Fault 通知目前发不出去。这不影响本 spec，但若你的 E2E 涉及通知，别指望 uat 能收到。

## 4. 与 era3-fault-workunit 的关系

两个 spec 会碰同一处代码，**必须约定顺序**：

| 冲突点 | 情况 |
|---|---|
| `internal/impact/projector.go` `RebuildFault` | era3 的任务 31（健康判定分支）与本 spec 任务 4 都要改。era3 的 P4 目前**阻塞**于平台团队的 `CCLOUD_HEALTH_WINDOW` 与真实接口路径，短期不会动，所以本 spec 可以先做；若 P4 解锁，先合的一方在 HANDOFF 里留言 |
| 迁移编号 | 见上，`000028` 不可占用 |
| `internal/app/incident_nudge.go` | era3 没有待办任务改它，本 spec 任务 8 可独占 |

## 5. 已知的坑（都是这个项目真实踩过的）

| 坑 | 怎么避 |
|---|---|
| **新增字段只在领域对象里赋值，没真正落库** | era3 任务 14 的 `owner_team_id` 就是这样：projector 填了、`Upsert` 没写、列清单没扩展，而单测只测纯函数所以全绿。**凡新增持久化字段，必须有带 `//go:build integration` 的「写入 → 读回」测试** |
| **在 Go 侧过滤代替 SQL 侧断链** | 任务 3 的核心陷阱：遍历是递归 CTE，Go 侧过滤只会让下线节点自己消失，它的下游照样被遍历出来。必须进递归条件 |
| **测试把"当前实现"当成预期写死** | era3 出现过两次：对账测试断言了错误行为；元告警测试把 fallback 目标全设成与原目标不同，恰好绕开唯一会出事的路径。**边界值要专门挑与生产配置一致的那个** |
| **"不做某事"的分支静默返回** | 所有 early return 都要留可追溯记录（时间线或审计），不能静默 |
| **pgx 结果集未释放就发子查询** | 不变量 20；`infrastructure.go` 里已有二阶段查询，加条件时别引入新的嵌套 |
| `.git/index.lock` 反复残留（已 4 次） | 多 worktree 并行时会撞上；确认无 git 进程后可删 |

## 6. 工作协议

1. 任务状态：派发前标 `[-]`，验收通过标 `[x]`；**只由主对话 / 接手 Agent 改 `.spec-workflow/`，子代理禁改**。
2. 实现分支从 **main（`v0.3.0`）** 拉；子代理禁 git 操作、禁改 `go.mod`（除非授权）。
3. 每个任务完成后 `log-implementation`：artifacts 写可 grep 的接口签名，summary 写跨任务警告。
4. 每阶段收尾必跑：`go build ./... && go vet ./... && go vet -tags=integration ./...`、`go test -tags=integration ./... -race -count=1`、`cd web && npm run build && npx oxlint`。
5. `test/e2e` 固定端口，**任何时刻只能跑一轮**；跑前确认无残留容器。
6. 新增迁移必须在全新 PG 上实测 up → down → up。

## 7. 交回 review 时请提供

1. `git log --oneline` 与对应任务号
2. 每个任务的 `log-implementation` 已落账、`tasks.md` 已标 `[x]`
3. 全量测试与 E2E 的真实输出（不是"应该能过"）
4. 迁移往返验证结果
5. 偏离规格之处及理由

## 8. 建议的第一步

任务 1 → 2 → 3 一批做完再验收：迁移 + 落库读回 + 遍历断链，这三件事是后面所有任务的地基，且能用 uat 上那个真实的 `host:10.61.17.11` 做验证样本。
