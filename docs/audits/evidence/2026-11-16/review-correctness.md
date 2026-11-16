# I3 独立审查

**协调者状态索引（ba0e483）：两项已报告 P1 已复验关闭，未发现剩余阻断项。以下保留原始审查与后续复验记录；最终证据见末节。**

审查提交：`256effc0054d94c52ead1a80fb3bd3907b4c0d1f`。比较基线：`63778bf`。目录：`/private/tmp/alert-failure-correctness`。审查者未参与 I3 实现；仅执行只读 Git 操作、代码阅读、独立测试及外部 Go overlay，功能 worktree 的 `git status --short` 在审查前后均为空。

结论：发现 1 项已用真实 PostgreSQL 复现的 **P1 阻断缺陷**，应修复后复核。现有用例通过不覆盖此升级场景。

## 已证实缺陷

### [P1] 缺失关联维度的新校验截断旧 occurrence 的解除，遗留无证据 active Fault

位置：`internal/mapping/projector.go:91-95`，以及 `:131-135`。新 guard 把缺失关联维度的已映射旧 occurrence 改为 unmapped，随后无条件删除其自动证据并提前返回，跳过 `:143-144` 的 resolved → monitoring 路径。

可达序列：

1. 升级前的合法配置显式声明 `sre.correlation.labels=cluster`（或策略的 CorrelationLabels），但 occurrence 没有 cluster 值。旧实现允许该状态，计算 `cluster=` 空维度 correlation key，并创建 active Fault 与自动 evidence。
2. 升级至审查提交后，同一 occurrence 收到 `source_state=resolved`，维度仍缺失。它不需要在解除时丢 label；从最初触发起就没有该值。
3. 新实现返回 `correlation_label_missing`；删除旧 Fault 的自动 evidence；旧 Fault 仍为 active，也没有保留该 Alert 的证据链。旧版本同样输入会找到原空维度 key，把旧 Fault 推为 monitoring 并保留 evidence。

影响：已有 Fault 无法按正常告警解除链路进入 monitoring，且证据被清除；存在关联 Incident 时 active Fault 还会继续影响恢复门槛，历史工作区失去原自动证据。禁止缺维度的新建/合并本身正确，不能同时破坏升级前已持久化 occurrence 的收口。

**独立复现：**外部测试源码为 `/private/tmp/alert-iteration-issues/correctness_review_test.go`。它使用与旧实现完全一致的 SHA256 空维度 key，构造升级前 active Fault + 自动 evidence，然后调用新 Project 处理同一 resolved occurrence。Go overlay 只增加虚拟测试文件，不改功能 worktree。

```bash
GOCACHE=/private/tmp/alert-review-correctness-gocache go test \
  -overlay /private/tmp/alert-iteration-issues/correctness-review-overlay.json \
  -tags=integration ./internal/incident \
  -run TestReviewResolvedMissingDimensionPreservesLifecycle -count=1 -v
```

结果：退出 1，4.234s；错误为：

```text
resolved旧occurrence后遗留无证据active Fault: status=active links=0
```

日志：`/private/tmp/alert-iteration-issues/correctness-repro.log`。

**基线对照：**同一测试、同一 PG fixture，以另一个外部 overlay 把 projector.go 和既有 incident integration test 文件替换为 `git show 63778bf:...` 的只读内容，运行：

```bash
GOCACHE=/private/tmp/alert-review-correctness-gocache go test \
  -overlay /private/tmp/alert-iteration-issues/correctness-baseline-overlay.json \
  -tags=integration ./internal/incident \
  -run TestReviewResolvedMissingDimensionPreservesLifecycle -count=1 -v
```

结果：退出 0，4.638s。日志：`/private/tmp/alert-iteration-issues/correctness-baseline-repro.log`。证明本问题由本次 guard 与原 unmapped 清理路径组合引入，并非原有 resolved 收口行为。

**建议修复边界：**保留缺维度时禁止创建/合并新 Fault；对已经绑定的 occurrence，解除/重评估应能够沿持久化关联处理已有 Fault 并保留历史证据，不能仅删除关联后返回。补充空维度旧 active Fault 解除、旧 resolved 历史重放、缺维度 firing 不新建、人工 evidence 不删除等回归。具体并发多 evidence 判据应由实现者结合现有恢复语义处理，不能为了通过本用例无条件关闭共享 Fault。

## 已核对且未发现新增阻断项

- 生命周期：HasConfirmedBusinessImpact 在同一服务事务内增加 active lifecycle 过滤；总览既有 SQL 与前端 gateReady 使用同样 kind/state/lifecycle 条件。下线实体的历史人工 Impact 保留。已存在 Incident 的 Declare 幂等分支位于新门槛之前，不会因为后续下线被拒绝或自动关闭。
- 并发宣布：原有每 Fault 全局唯一 link 约束和 ErrIncidentForFaultExists 后读既有 Incident 的路径保留。本次没有修改其事务/锁协议。未把并发下线与 Declare 的交错误报为新引入的缺陷；并发专门故障注入未执行，见后面的覆盖建议。
- Evaluate 与 Project 共用合同解析和 correlation key；新函数本身只读。拟创建结果查询与真实 Upsert 仍有正常并发变化窗口，执行时再次评估，不信任预览。
- HTTP preview 使用单独 READ ONLY 事务，循环中没有 Project、证据/时间线/timer 写入；返回结果结构不含通知目标、payload 或凭据。管理员中间件、批量 1–100、UUID 校验均存在；全 API race 测试已独立通过。现有 API 测试覆盖拒绝路径，成功 HTTP+真实 PG 的全栈调用尚未实测。
- governance SQL 分别按 source、source_integration_id、severity、status、reason 分桶；权威 unknown/resolved 排除；ignored 未合入 unmapped；结果空集显式初始化为 []。
- 跨节点文案只调整因果解释，不翻转拓扑或提高 confirmed 判定；stale + direct 不自动 confirmed 的回归存在且已独立执行。

## 独立验证命令

```bash
# 全包 race，退出 0：mapping 2.259s、impact 2.569s、api 6.615s
GOCACHE=/private/tmp/alert-review-correctness-gocache go test -race \
  ./internal/mapping ./internal/impact ./internal/api -count=1

# 真实 PostgreSQL focused race，退出 0：incident 18.520s
GOCACHE=/private/tmp/alert-review-correctness-gocache go test -tags=integration -race \
  ./internal/incident \
  -run 'TestService_DecommissionedConfirmedImpact|TestMappingEvaluationReadOnlyAndGovernance|TestExplicitAppGatewayDependencyLifecycle|TestService_Declare' \
  -count=1

git diff --check 63778bf 256effc
# 退出 0，无输出
```

日志：`/private/tmp/alert-review-correctness-unit.log`、`/private/tmp/alert-review-correctness.log`。真实数据库使用既有 testcontainers PostgreSQL 15 临时镜像。没有执行固定 `test/e2e`、npm 安装、生产访问或破坏性线上操作。前端一行 gateReady 仅做代码口径核对，不以实现者的构建记录替代本次独立验证。

首次 regex focused 命令未匹配 impact 测试，输出 `[no tests to run]`，因此随后独立重跑完整 mapping/impact/api 包；未把无匹配计为覆盖。新增故障复现的退出 1 是已确认产品失败证据，未改测试阈值或功能代码。

## 覆盖建议（不是已证实缺陷）

1. 补成功 HTTP preview + 真 PG 的端到端只读验证，尤其是策略变化、已存在/拟创建 Fault、404/空集合路径。
2. 给 governance 增加同 kind、不同 source_integration_id 的双来源 fixture；当前 SQL 具有正确分桶键，但既有用例没有验证双来源隔离。
3. 给下线提交与 Declare 交错、同 Fault 并发双击、下线后既有 Incident 再次 Declare 补同步屏障回归，明确操作的线性化期望；目前没有实际复现新增并发错误。
4. 新 preview/governance 沿用文件中把 `*gin.Context` 传给数据库的旧模式。Gin 未启用 ContextWithFallback 时不会透传 Request 的取消；后续可统一换为 `c.Request.Context()` 并设置有界查询 context。本审查未做断连/PG锁等待注入，因此仅列为可改进项。

## 2026-11-16 修复提交复核：2bc9b1d

复核提交：`2bc9b1df8d994837a945d31a9fd04ea6babed297`。原 P1 的独立 overlay 在该提交 **已通过**，`TestReviewResolvedMissingDimensionPreservesLifecycle` 退出 0，5.918s。新实现保留已绑定自动/人工证据，不按缺失维度猜新 Fault；resolved 历史的状态不在 UPDATE 范围，不会被重开。实现者新增串行覆盖也已独立运行，结果见后续回执。

但复核新增收口函数后发现 **新的 P1：行锁等待后继续使用旧语句快照，可能把新 firing 共享 Fault 推到 monitoring**。因此原问题已关闭，当前提交仍有阻断，不应仅凭原复现转绿通过验收。

### [P1] MonitorBoundFaultAfterResolvedOccurrence 不能在同一 UPDATE 中兼做等待与证据一致性判断

位置：`internal/store/faults.go:MonitorBoundFaultAfterResolvedOccurrence`（该修复新增）。

新的 UPDATE 通过 NOT EXISTS 检查其他 occurrence，意图正确；但是 PostgreSQL READ COMMITTED 的语句快照在等待 Fault 行锁之前已经创建。若另一事务持有 Fault 行锁并正在写入新 firing evidence，等待结束后 UPDATE 对目标行执行并发重检查，子查询仍无法看见快照之后新提交的 evidence 行。

独立复现：

1. 构造已有缺维度 active Fault 与旧 occurrence，准备旧 occurrence 解除收口。
2. Tx1 依照 Project 的锁协议先锁 Fault，再通过真实仓储新增一条 firing Alert 和它的 direct_impact evidence，暂不提交。
3. Tx2 调用真实 MonitorBoundFaultAfterResolvedOccurrence；通过第三条连接查询 pg_stat_activity，确定该 UPDATE 已在等 Fault 行锁。
4. Tx1 提交，Tx2 继续并提交。数据库已有另一条 firing direct evidence，但 Fault 状态为 monitoring。

错误输出：

```text
新firing已提交但共享Fault变成monitoring
```

复现源码继续保存在原外部文件 `correctness_review_test.go`，新增 `TestReviewConcurrentFiringDoesNotGetMonitored`，不修改功能 worktree。

```bash
GOCACHE=/private/tmp/alert-review-correctness-gocache go test \
  -overlay /private/tmp/alert-iteration-issues/correctness-review-overlay.json \
  -tags=integration -race ./internal/incident \
  -run 'TestReviewConcurrentFiring|TestReviewResolvedMissing' -count=1 -v
```

结果：退出 1，9.092s；原解除回归通过，新共享 Fault 并发回归失败。日志：`/private/tmp/alert-iteration-issues/correctness-fixed-concurrency.log`。

建议：先用独立语句按 Fault ID 获取 FOR UPDATE 锁，拿到锁之后再以**新语句快照**检查其他证据并更新。保留现有对 active/candidate、其他 firing/unknown、历史 resolved 与人工证据的约束；新增同步屏障测试验证另一事务的新证据能被等待后的检查看见。

修复者新增的串行边界测试已独立运行：`go test -tags=integration -race ./internal/incident -run TestMappingUpgradeMissingDimensionPreservesBoundLifecycle -count=1 -v`，退出 0，4.518s。覆盖共享已有 firing 阻断、历史不重开、人工/自动证据保留；它尚未覆盖上述新增 evidence 与行锁等待交错，因此不抵消新 P1。

## 第二轮修复独立复核：ba0e483

复核目录 `/private/tmp/alert-failure-correctness`，主 Agent 指定 SHA `ba0e483`，相对 `2bc9b1d`。本次未参与 I3 实现、未修改功能源码、未运行 Git、未运行固定 E2E。

**结论：两项已报告 P1 均已修复，当前复核未发现新的阻断项，可继续集成验收。**

- `MonitorBoundFaultAfterResolvedOccurrence` 先调用独立 `SELECT ... FOR UPDATE` 锁定父 Fault，完成等待后才执行带其他 evidence 判据的 UPDATE。因此 READ COMMITTED 下的证据检查使用等待之后的新语句快照，能看见持锁事务刚提交的新 firing occurrence。
- `UpsertAutomatedEvidence`、`ReplaceAutomatedEvidence`、`UpsertHumanEvidence` 都先走同一单目标父锁，再插入/替换子 evidence；因此上述一致性屏障没有被正常自动/人工写入路径绕开。原升级缺维度路径保留绑定和人工证据、历史 resolved 不重开等限制保持不变。
- `RemoveAutomatedEvidenceForAlertExcept` 没有扩展成跨旧/新 Fault 的父锁，不引入“先锁新目标、再锁旧目标”的反向映射顺序。删除本身不新增 firing 证据；本次没有据此假设它提供跨多个 Fault 的原子状态视图。

### 独立验证

原审查 overlay 的并发屏障原先写死 `query LIKE 'UPDATE faults f SET status=%'`。修复把等待移至 `SELECT id::text ... FOR UPDATE`，直接沿用旧匹配会让 fixture 报“未观察到行锁屏障”，并非产品回归。因此保留原文件不动，复制为 `correctness_review_ba0e483_test.go`，仅将等待语句匹配扩为旧 UPDATE **或**新 SELECT。fixture 的数据、提交时序与两个最终领域断言均保持不变。新版 overlay 为 `/private/tmp/alert-iteration-issues/correctness-review-ba0e483-overlay.json`。

```sh
GOCACHE=/private/tmp/alert-notify-gocache go test \
  -overlay /private/tmp/alert-iteration-issues/correctness-review-ba0e483-overlay.json \
  -tags=integration -race ./internal/incident \
  -run 'TestReviewConcurrentFiring|TestReviewResolvedMissing' -count=1 -v
# 退出0：16.663s，原升级解除与并发firing两项均实际通过

GOCACHE=/private/tmp/alert-notify-gocache go test -tags=integration -race \
  ./internal/incident ./internal/store \
  -run 'TestMappingUpgrade|TestFaultRepoPreservesHumanEvidenceAndMonotonicActivation' -count=1 -v
# 全部退出0：incident与store（store 9.283s），新fixture按backend PID确认锁等待

GOCACHE=/private/tmp/alert-notify-gocache go test -race ./internal/mapping ./internal/api -count=1
# 全部退出0：mapping 1.461s / api 6.729s
```

真实 PostgreSQL 15 由现有 testcontainers 启动并清理。完整日志：`correctness-ba0e483-overlay.log`、`correctness-ba0e483-focused.log`，均位于 `/private/tmp/alert-iteration-issues`。本次覆盖了原两项独立故障复现、新增父锁等待 fixture、串行升级/人工 evidence 保护；未执行全仓集成、前端构建、固定 Compose E2E 或线上操作。
