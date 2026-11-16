# 生命周期与映射正确性验收（Issue #13）

日期：2026-11-16。基线：63778bf。独立实现目录：`/private/tmp/alert-failure-correctness`。

## 交付行为

- `FaultRepo.HasConfirmedBusinessImpact` 只认可 lifecycle active 的 business_system。总览既有 SQL 已使用同一过滤，保持不变；Fault 工作区的宣布门槛同步过滤下线应用。Impact 历史明细保留人工裁决，不把下线改写为恢复，不自动关闭历史 Incident。
- `mapping.Projector.Evaluate` 提供只读评估，`Project` 复用同一解析/关联计算。显式策略或标准标签声明的关联维度缺少值（包括纯空白）返回 `unmapped / correlation_label_missing`，不创建或合并 Fault；未声明维度保留旧聚合方式。真实投影持久化治理原因；缺维度时保留该 occurrence 的既有绑定与历史证据，预览没有任何写入。
- 新增管理员 `POST /api/v1/admin/alert-mapping/preview`，批量 1–100 个 Alert UUID；返回 `items[].result`（实体、关联 key、已有 Fault、缺失原因）、`fault_kind` 和 `would_create_fault`。使用 PG READ ONLY 事务从数据库层阻断副作用。它评估已发布规则，现有 `/admin/alert-mappings/preview` 继续只预览草稿策略的匹配告警。
- 新增管理员 `GET /api/v1/admin/alert-mapping/governance`。仅统计当前 firing，按 source、source_integration_id、severity、status、reason_code 分桶；未评估为 unmapped/not_evaluated，ignored 单独保留，空集为 `items: []`。已有 `/alert-mapping/summary` 仍为全历史映射汇总。
- 跨节点 confirmed 文案表达应用退化已确认、Fault 归因未确认，不修改 direct evidence 规则或 ccloud trace 方向。stale 关系即使有直接证据也只得到 potential。
- pipeline 注释修为 dedup→mapping→route→group→silence→inhibit；CLAUDE.md 迁移号遵循最大号 +1，不回填历史空缺，000032 留给通知迭代；本迭代无迁移。

## 管理 API 调用

以下命令使用管理员已登录的 cookie 文件；示例不包含真实会话或通知目标。

```bash
curl -b /tmp/admin-cookie.txt -H 'Content-Type: application/json' \
  -X POST http://localhost:8080/api/v1/admin/alert-mapping/preview \
  -d '{"alert_ids":["00000000-0000-0000-0000-000000000000"]}'
curl -b /tmp/admin-cookie.txt \
  http://localhost:8080/api/v1/admin/alert-mapping/governance
```

preview 不回放，真实回放仍使用既有管理员 `POST /api/v1/admin/alert-mapping/results/:alert_id/reprocess`。提交时重新评估，不信任旧预览；预览与执行之间策略或实体变化可改变结果。新响应只返回映射判断，不返回渠道目标或通知 payload。

## 实测记录

全部 Go 命令显式使用 `GOCACHE=/private/tmp/alert-correctness-gocache`。

1. 修复前 `go test -tags=integration ./internal/incident -run TestService_DecommissionedConfirmedImpact -count=1`：真实 PostgreSQL **失败**，输出“下线应用不能满足准入”“唯一 confirmed 已下线应拒绝宣布: <nil>”，证明旧行为。
2. 修复后 `go test -tags=integration ./internal/incident -run 'TestService_DecommissionedConfirmedImpact|TestMappingEvaluationReadOnlyAndGovernance' -count=1`：退出 0（8.893s）。
3. `go test -tags=integration ./internal/incident -run TestExplicitAppGatewayDependencyLifecycle -count=1`：退出 0（5.754s）。
4. 最终 `go test -tags=integration ./internal/incident -race -count=1`：退出 0（23.333s），包含新增显式策略路径和既有 Incident 全套集成测试。testcontainers 使用隔离 PostgreSQL，不共享固定 E2E DB。
5. `go test ./internal/... ./cmd/... -count=1`：所有包退出 0。覆盖 API RBAC/批量上限/非法 UUID、关联维度纯函数和跨节点影响解释。
6. `go build ./...`、`go vet ./...`：退出 0。
7. 增补 Alert/Fault/Incident 全部时间线表计数后，`go test -tags=integration ./internal/incident -run TestMappingEvaluationReadOnlyAndGovernance -race -count=1`：退出 0（6.343s）。
8. `cd web && npm run build && npx --cache /private/tmp/alert-correctness-npmcache oxlint`：退出 0；构建现有 bundle size 提示。安装使用 `npm ci --ignore-scripts --cache /private/tmp/alert-correctness-npmcache`，未改依赖或 lockfile。

环境与操作异常：首次 Go 默认缓存权限被拒，改临时缓存；PG 新测试 fixture 缺 ResolvePolicy/Explanation 触发约束，补完整后重跑通过。一次 `go test ./...` 误进入无 tag 隔离的固定 E2E TestMain，发现后中断 exit 1，**不计为通过**；只读 PID/comm 确认没有 e2e.test 子进程，残留 firebreak-e2e-postgres-1 已交主 Agent 清理。该失误与后续范围收敛已记录在 `.learnings/2026-11-16-correctness.md`。

## 验收矩阵与限制

| 项 | 已执行证据 | 限制 |
|---|---|---|
| C1-C3 生命周期 | 真 PG：唯一 confirmed 下线拒绝、另有有效 confirmed 允许、历史 Incident 仍 declared | 用 SQL 构造生命周期状态，HTTP 下线链路沿用既有 E2E，由主 Agent 串行验证 |
| C4 关联维度 | 标准契约与策略路径缺值均治理；完整维度拆分、空维度兼容单测 | 不对未声明维度推测业务含义 |
| C5 只读预览 | READ ONLY 真 PG；预览前后 faults/mapping results/fault_alert_links/timers/timeline_entries/fault_timeline_entries/incident_timeline_entries 计数相同；投影 entity/key 一致、已有 Fault 正确 | 成功 HTTP 全栈调用由集成验收补；数据库只读同时禁止未逐表计数的其他写 |
| C6 firing 治理 | legacy 空 fingerprint、权威 firing/unknown/resolved、ignored 与 mapped、空集真 PG | 同 kind 双 source_id 分桶由 SQL 明确实现，未另建双来源 fixture；状态为最近持久化映射判断，不自动补跑历史评估 |
| C7 拓扑 | 真 PG 显式 App→Gateway 反向可达、过期边 stale、下线 Gateway 阻断；纯函数 stale+direct 不 confirmed | 不宣称真实平台采样或因果归因已验证 |
| C8 权限/输出 | API 单测未登录 401、member 403、空批/超100/非法UUID 400；返回无通知目标字段 | 不涉及对外发送，不部署、不合并 |


## 独立审查 P1 修复：升级前空维度 occurrence 的解除

独立审查 `review-correctness.md` 发现旧版本已绑定的空维度 occurrence，在新 guard 下解除时会被改为 unmapped 并删除证据，导致旧 Fault 遗留 active。已按持久化绑定修复，未放宽新准入：

- Evaluate 缺维度时只读查询已有 `fault_alert_links`，包括后续被人工裁决过的证据，不按空维度 key 猜测新绑定。单一既有绑定在预览返回原 Fault ID/entity/key，`would_create_fault=false`；reason 仍为 `correlation_label_missing`。
- Project 对缺维度 occurrence 不创建、合并或重开 Fault，不删除或覆盖自动/人工历史证据。重复 firing 保持原状态，不产生激活转移。
- 收到明确 resolved 时，按持久绑定 Fault ID 条件推进 active/candidate→monitoring；其他关联 occurrence 仍 firing 或来源 unknown 时保留状态。monitoring/resolved 历史重放不重开。这里只确认开始观察，不自动 resolved 或关闭 Incident。
- `TestMappingUpgradeMissingDimensionPreservesBoundLifecycle` 用旧版精确 SHA256 空维度 key、active Fault 和历史 evidence 构造真实 PG 升级 fixture，覆盖重复 firing、共享 Fault 另一条 firing、最后解除、自动/人工证据保留、人工裁决过的当前绑定、resolved 历史及缺维度 firing 重放。

原独立复现 overlay 已从退出 1 转为退出 0（4.077s）。新增强断言 fixture 与原只读评估测试一起运行 integration race 退出 0（7.457s；补人工当前绑定分支后最终同命令退出 0，9.683s）。mapping/impact/api 全包 race 分别退出 0（1.496s/2.303s/6.553s）；`go build ./...` 与 focused store/mapping/incident vet 退出 0。

### Grafana CI 夹具修正

PR #18 backend CI 的 `TestGrafanaAuthority_FullChain` 失败根因：通用固定目标策略帮助函数声明了 `cluster` 关联维度，而 Grafana webhook 和对账 mock 都未提供值。此为新 guard 正确拒绝，不放宽产品约束。Grafana webhook 与 mock 现在显式携带相同 `cluster=prod-a`，并新增对账/解除后仍引用初始 Fault ID 的断言。其他 mock 不凭空增加维度。

已执行 `go test -c -o /private/tmp/alert-iteration-issues/correctness-e2e-compile.test ./test/e2e` 仅编译，退出 0；未执行 TestMain、未启动固定 Compose。真实 E2E 由主 Agent 串行执行，不能把本次编译当成 Grafana E2E 通过。

## 并发收口 P1 修复：等待行锁后的新快照

独立复核在 `2bc9b1d` 发现：单条 UPDATE 在等待 Fault 行锁期间，NOT EXISTS 仍使用语句开始时的 READ COMMITTED 快照，漏看等待期间新提交的 firing 证据，错误推进 monitoring。

修复先通过独立 `SELECT id FROM faults WHERE id=$1 FOR UPDATE` 取得父行锁，获得锁后再发下一条 SQL 检查证据并更新。自动/人工 evidence 新增及替换先取其单个 Fault 锁。删除旧自动证据保持原路径，不追加多个旧 Fault 父锁：Project 已持新 Fault 锁，额外锁旧 Fault 会让反向改映射产生跨 Fault 锁倒序；删除本身不会新增被漏看的 firing 证据。没有按空维度猜测新关联，也没有新增自动关闭 Incident。

新增 `TestMappingUpgradeConcurrentFiringDoesNotGetMonitored`：Tx1 锁 Fault 并新增 firing occurrence/evidence；Tx2 发起解除，按其独立 `pg_backend_pid()` 在 `pg_stat_activity` 观察 `wait_event_type='Lock'` 同步屏障；Tx1 提交后 Tx2 重新检查必须保持 active。屏障不依赖 SQL 文本或固定睡眠，整个场景带 10 秒 context 上界。

实际命令（均使用临时 GOCACHE）：

```bash
go test -tags=integration -race ./internal/incident \
  -run 'TestMappingUpgrade|TestMappingEvaluation' -count=1
# 退出0，13.901s，含新增并发屏障、旧升级生命周期及只读预览
```

本轮没有执行固定 E2E 或 Git操作。原始独立并发复现日志由审查者保留在 `/private/tmp/alert-iteration-issues/correctness-fixed-concurrency.log`；新实现对应回归已进入仓库，不依赖外部 overlay。

最终范围：只对新增/替换 evidence 的单个目标 Fault 与解除判定建立父锁屏障；不扩大 Remove 到跨 Fault 锁协议。`go test -tags=integration -race ./internal/store -run TestFaultRepoPreservesHumanEvidenceAndMonotonicActivation -count=1` 退出 0（6.840s）；mapping/impact/api race 再次退出 0（1.457s/1.892s/6.593s）；全包 build 与 store/mapping/incident vet 退出 0。

收敛锁范围后，最终 `go test -tags=integration -race ./internal/incident -run TestMappingUpgradeConcurrentFiringDoesNotGetMonitored -count=1` 退出 0（5.581s）。
