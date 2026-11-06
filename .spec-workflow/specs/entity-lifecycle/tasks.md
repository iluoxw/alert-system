# Tasks Document — entity-lifecycle

> **依赖顺序：1（迁移）→ 2–4（store / impact）→ 5–6（API）→ 7（前端）→ 8（nudge 文案）→ 9（E2E）→ 10（文档）。**
> 任务 2 与 3 可并行；5 依赖 2–4；7 依赖 5–6。
> 迁移占用 **`000029`**，不得占用 026/027/028（归 era3-fault-workunit）。
> 每阶段收尾必跑：`go build ./... && go vet ./... && go vet -tags=integration ./...`、`go test -tags=integration ./... -race -count=1`、`cd web && npm run build && npx oxlint`。
> 动手前先读 `design.md` 的「代码锚点」表与 `requirements.md` 的「硬约束」表。

## 数据与领域

- [x] 1. 迁移 000029：实体生命周期列
  - File: migrations/000029_entity_lifecycle.up.sql, migrations/000029_entity_lifecycle.down.sql
  - `resource_nodes` 加 `lifecycle_state`（CHECK active/decommissioned，默认 active）、`decommissioned_at`、`decommissioned_by`、`decommission_reason`，加 `lifecycle_state` 索引
  - Purpose: 让「已下线」成为可表达的终态，与 stale 区分开
  - _Leverage: migrations/000026_fault_workunit.up.sql 的加列风格_
  - _Requirements: R1.1, R1.2_
  - _Prompt: Implement the task for spec entity-lifecycle, first run spec-workflow-guide to get the workflow guide then implement the task: Role: 数据库工程师 | Task: 按 design.md 数据模型加列与索引 | Restrictions: 纯加列，默认值必须让既有行为逐字节不变；不得新建审计表；不得使用 026/027/028 编号 | Success: 全新 PG 上 up → down → up 实测通过，既有行迁移后全部 active | 完成后按工作流标记状态并 log-implementation_

- [x] 2. store：生命周期读写与列表筛选
  - File: internal/store/infrastructure.go, internal/store/infrastructure_lifecycle_test.go
  - 新增专用方法设置 / 恢复生命周期（写审计三列），实体列表支持按状态筛选、默认只返回 active；`faultColumns` 式的列清单要同步扩展，保证读回
  - Purpose: 让状态真正落库并能读回，而不是只活在领域对象里
  - _Leverage: internal/store/infrastructure.go:296 UpdateManagedEntity 的事务与列清单写法_
  - _Requirements: R1.1, R1.2, R1.5, R1.6, R2.5_
  - _Prompt: Implement the task for spec entity-lifecycle, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 实现生命周期读写与筛选 | Restrictions: **不得**把状态变更塞进 UpdateManagedEntity（见 D2）；不得删除实体、别名或依赖边；遵守不变量 20 的结果集释放顺序 | Success: 带 //go:build integration 的「写入 → 读回」测试通过，覆盖下线、恢复、默认筛选 | 完成后按工作流标记状态并 log-implementation_

- [x] 3. 遍历排除下线实体
  - File: internal/store/infrastructure.go, internal/store/infrastructure_faults_test.go
  - `listCanonicalTraversal`（infrastructure.go:673）的递归 CTE 加排除条件，使下线节点**及其下游**都不出现在结果里
  - Purpose: 爆炸半径不再包含已不存在的资源
  - _Leverage: 既有递归 CTE 与 staleSeconds 参数的处理方式_
  - _Requirements: R2.1, R2.4_
  - _Prompt: Implement the task for spec entity-lifecycle, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go + SQL 工程师 | Task: 在递归 CTE 内排除 decommissioned 实体 | Restrictions: 必须在 SQL 递归里断链，**不得**在 Go 侧过滤返回结果（那样下游仍会被遍历出来，见 D1）；依赖边必须保留在库中；不得引入新的嵌套查询（不变量 20） | Success: 集成测试断言「下线节点的下游也不出现」，且 active 路径不受影响 | 完成后按工作流标记状态并 log-implementation_

- [x] 4. Impact：下线实体的自动结论降级
  - File: internal/impact/projector.go, internal/impact/projector_test.go
  - 实体下线后，由它产生的 `potential` Impact 转 `ruled_out`，理由记为实体已下线
  - Purpose: 影响面与现实一致，同时不侵犯人工裁决
  - _Leverage: internal/impact/projector.go:39 RebuildFault 的三态处理_
  - _Requirements: R2.2, R2.3_
  - _Prompt: Implement the task for spec entity-lifecycle, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 增加下线导致的自动降级分支 | Restrictions: 人工 confirmed / 人工 ruled_out **永不**被覆盖（不变量 16）；按既有的人工裁决标记判断，不得用时间戳新旧来猜；不引入 likely 等新状态 | Success: 单测覆盖自动降级与人工裁决不被覆盖两条路径 | 完成后按工作流标记状态并 log-implementation_

## API

- [x] 5. 下线前置检查端点
  - File: internal/api/infrastructure.go, internal/api/infrastructure_lifecycle_test.go
  - `GET /api/v1/admin/entities/:id/lifecycle-preview`：返回该实体的 active/monitoring Fault、未关闭 Incident、未解除告警证据
  - Purpose: 让操作人在下线前看见影响面
  - _Leverage: internal/api/infrastructure.go:39-40 的 admin 分组注册方式_
  - _Requirements: R3.1, R4.1_
  - _Prompt: Implement the task for spec entity-lifecycle, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 实现前置检查端点 | Restrictions: admin only；集合字段为空必须返回 [] 而非 null（不变量 13）；一次查询取回，避免 N+1 | Success: 测试覆盖空集合、非 admin 403、实体不存在 404 | 完成后按工作流标记状态并 log-implementation_

- [x] 6. 生命周期变更端点与联动收口
  - File: internal/api/infrastructure.go, internal/api/infrastructure_lifecycle_test.go
  - `PATCH /api/v1/admin/entities/:id/lifecycle`：下线 / 恢复；`cascade_faults=true` 时同事务把关联 Fault 推到 resolved 并写时间线；存在 active Fault 且未选 cascade 时 409
  - Purpose: 把下线做成一次可审计、可联动的显式动作
  - _Leverage: 任务 2 的 store 方法；internal/api/faults.go 的同事务「状态推进 + 时间线」配对写法_
  - _Requirements: R1.3, R3.2, R3.3, R3.4, R3.5, R4.1_
  - _Prompt: Implement the task for spec entity-lifecycle, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 实现生命周期变更与 Fault 联动收口 | Restrictions: 下线原因必填；**不得自动关闭 Incident**（不变量 11），只能在其时间线追加说明；状态变更与时间线必须同事务；若需通报只准 Schedule 一条 notify_send（不变量 4）；重复下线按当前状态短路 200 | Success: 测试覆盖 409、cascade 收口、重复下线幂等、Incident 未被自动关闭 | 完成后按工作流标记状态并 log-implementation_

## 前端

- [x] 7. 实体详情下线入口与列表筛选
  - File: web/src/pages/InfrastructureDetail.tsx, web/src/pages/Settings/ServiceCatalog.tsx, web/src/api/client.ts
  - 详情页展示生命周期状态 / 下线时间 / 操作人 / 原因，提供下线与恢复操作；提交前先调 preview 并把影响面呈现给操作人确认；列表支持按状态筛选，默认只看 active
  - Purpose: 让下线成为页面上可完成的任务
  - _Leverage: ServiceCatalog.tsx 既有的受管实体编辑弹窗与目录驱动选择器_
  - _Requirements: R4.2, R4.3, R4.4, R4.5_
  - _Prompt: Implement the task for spec entity-lifecycle, first run spec-workflow-guide to get the workflow guide then implement the task: Role: React + TS 工程师 | Task: 实现下线入口、确认弹窗与筛选 | Restrictions: 不新增一级导航（不变量 19）；不得在用户看不到影响面时直接提交；下线原因必填；AntD 锁 5 不升 6 | Success: npm run build 与 oxlint 通过；空影响面与非空影响面两种文案都明确 | 完成后按工作流标记状态并 log-implementation_

## 收敛与验收

- [x] 8. nudge 文案给出下线出口
  - File: internal/app/incident_nudge.go, internal/app/incident_nudge_test.go
  - 在既有「可能源于采集目标下线或服务发现注销」提示后补可执行出口；全部证据实体已下线时在文案中明示
  - Purpose: 把假恢复的判断直接引向下线收口
  - _Leverage: 既有 nudge 文案构造_
  - _Requirements: R5.1, R5.2, R5.3, R5.4_
  - _Prompt: Implement the task for spec entity-lifecycle, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 调整 nudge 文案 | Restrictions: 继续用「解除」而非「恢复」（不变量 11）；**不得**推进 Incident 状态；不得改触发条件与间隔语义 | Success: 单测断言文案包含下线出口，且全部证据实体已下线时有明示 | 完成后按工作流标记状态并 log-implementation_

- [x] 9. E2E：下线收口全链路
  - File: test/e2e/entity_lifecycle_test.go
  - 造一个挂着 active Fault 与未关闭 Incident 的实体 → 下线并 cascade → 断言 Fault resolved、Incident 仍开、时间线写明原因是下线、爆炸半径不再包含该实体及其下游
  - Purpose: 验证跨 store / impact / api 的联动
  - _Leverage: test/e2e/harness.go 与既有 Fault / Incident 场景的 seed 方式_
  - _Requirements: R2.1, R3.3, R3.4_
  - _Prompt: Implement the task for spec entity-lifecycle, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 测试工程师 | Task: 编写端到端场景 | Restrictions: test/e2e 固定端口，任何时刻只能跑一轮；不得修改既有场景的断言 | Success: go test -tags=integration ./test/e2e/... 全绿 | 完成后按工作流标记状态并 log-implementation_

- [x] 10. 文档与不变量收口
  - File: CLAUDE.md, CHANGELOG.md
  - CLAUDE.md 增补「实体生命周期」口径（下线是人工终态、遥测不得自动置位、下线不删除历史）；CHANGELOG 记录本次交付
  - Purpose: 让后续实现受同一约束
  - _Leverage: CLAUDE.md 既有不变量的写法_
  - _Requirements: R1.4, R1.6_
  - _Prompt: Implement the task for spec entity-lifecycle, first run spec-workflow-guide to get the workflow guide then implement the task: Role: 技术写作者 | Task: 增补不变量与发布记录 | Restrictions: 不改写既有不变量编号；条目与实际变更一致 | Success: 说明与实现一致 | 完成后按工作流标记状态并 log-implementation_

## 顺延（本 spec 不做）

- 实体生命周期变更的完整历史表（`entity_lifecycle_events`）：v1 用四列表达「谁、何时、为什么」，需要审计全量历史时再起。
- 自动发现下线（如平台 API 报告节点已移除后建议下线）：需要可信来源，且必须保持人工确认，等 ccloud 接口就绪后再评估。
- 实体删除与 IP 复用场景的别名回收。
