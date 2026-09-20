# Design Document — entity-lifecycle

## Overview

给受管实体加一个**人工维护的终态**，让「已下线」与「暂时看不到」彻底分开。改动集中在三处：`resource_nodes` 加生命周期列、遍历与影响投影排除下线实体、一个 admin 端点加实体详情页的操作入口。

设计原则：**加列不加表，加分支不加机制**。不新增 timer kind、不新增外部 IO、不新增一级导航，全部复用既有的仓储 / 投影 / 页面结构。

## 代码锚点（动手前先读）

| 位置 | 作用 | 本 spec 的改动 |
|---|---|---|
| `migrations/000029_*.up.sql`（新建） | 迁移，**编号从 029 起**（026/027/028 归 era3-fault-workunit） | 加列 |
| `internal/store/infrastructure.go:673` `listCanonicalTraversal` | 爆炸半径与依赖遍历的**唯一**递归实现，`ListBlastRadiusByNodeID`(660) 与 `ListDependencyProvidersByNodeID`(668) 都走它 | 加排除条件 |
| `internal/store/infrastructure.go:296` `UpdateManagedEntity` | 受管实体更新（别名 / 依赖按 source 隔离替换） | **不要**在这里塞生命周期，另开专用方法 |
| `internal/impact/projector.go:39` `RebuildFault` | Impact 重算，三态口径 | 下线实体产出的 potential 转 ruled_out |
| `internal/api/infrastructure.go:39-40` | `POST /entities`、`PUT /entities/:id`（admin） | 新增生命周期端点 |
| `web/src/pages/InfrastructureDetail.tsx` | 实体详情（路由 `infrastructure/:id`） | 状态展示 + 下线入口 |
| `web/src/pages/Settings/ServiceCatalog.tsx` | 受管实体的增改与待对齐 Tab | 列表筛选 |
| `internal/app/incident_nudge.go` | nudge handler 与文案 | R5 文案补出口 |

## 数据模型

`resource_nodes` 加四列（迁移 `000029`）：

```sql
ALTER TABLE resource_nodes
    ADD COLUMN lifecycle_state    TEXT NOT NULL DEFAULT 'active'
        CHECK (lifecycle_state IN ('active', 'decommissioned')),
    ADD COLUMN decommissioned_at  TIMESTAMPTZ,
    ADD COLUMN decommissioned_by  UUID REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN decommission_reason TEXT;

-- 默认列表只看 active，遍历同样按该列过滤
CREATE INDEX resource_nodes_lifecycle_idx ON resource_nodes (lifecycle_state);
```

**为什么不建审计表**：v1 只需回答「谁、何时、为什么下线」，四列即可；下线可逆但不是高频操作。若后续需要完整的状态变更历史，再补 `entity_lifecycle_events`——现在建表属于 YAGNI。恢复为 `active` 时清空三个审计列，并在关联 Fault / Incident 时间线留痕（见下）。

**为什么不加 `deleted_at` 语义**：下线不是软删除。实体必须继续可解析（`faults.entity_ref`、别名、历史边都指向它），只是退出实时计算。

## 关键决策

### D1 遍历排除放在 SQL 递归里，不在 Go 里过滤

`listCanonicalTraversal` 是递归 CTE。若在 Go 侧过滤返回结果，下线节点的**下游**仍会被遍历出来（它只是自己不出现，路径没断）。R2.1 要求「不经由它向下游传播」，所以排除条件必须进递归的 JOIN 条件。

注意不变量 20：该函数已有二阶段查询，加条件时不要引入新的嵌套查询。

### D2 下线是独立端点，不混进 `PUT /entities/:id`

`UpdateManagedEntity` 的语义是「替换属性 / 别名 / 依赖」，其中 `aliases`/`dependencies` 缺失表示保持不变（不变量 15）。把状态变更混进去会让「顺手改个名字」意外触发下线联动收口。用独立端点表达独立意图，也便于 admin 审计。

### D3 联动收口只处理 Fault，不碰 Incident

Incident 关闭受不变量 11 的双重门槛约束，且必须人工确认。下线只做两件事：把关联 Fault 按「实体已下线」推到 `resolved`，并在相关 Incident 时间线追加说明。Incident 本身仍由人在页面上按门槛关闭。

这样 INC-000001 那类案例的正确流程是：确认节点已剔除 → 下线实体（关联 Fault 自动收口）→ 人工确认后关闭 Incident。

### D4 Impact 只降级自动结论

下线导致的 `potential → ruled_out` 只能覆盖自动产生的结论。人工 `confirmed` / 人工 `ruled_out` 永不被覆盖（不变量 16）。实现时按既有的「人工裁决标记」判断，不要用时间戳新旧来猜。

### D5 事务边界

下线 + 联动收口 + 时间线必须在**同一事务**内提交。通知意图（若 Fault 收口需要通报）沿用不变量 4：只 `Schedule` 一条 `notify_send`，禁止同步 IO。

## 接口

```
PATCH /api/v1/admin/entities/:id/lifecycle      # admin only
  body: { state: "decommissioned"|"active",
          reason: string,                        # 下线必填
          cascade_faults: bool }                 # 是否一并收口关联 Fault
  200 → 实体最新状态 + 本次收口的 Fault 列表（空时 []）
  409 → 存在 active Fault 且 cascade_faults=false

GET  /api/v1/admin/entities/:id/lifecycle-preview  # admin only
  200 → { active_faults: [], open_incidents: [], firing_alerts: [] }   # 空集合返 []
```

前端在提交前先调 preview，把影响面摆给操作人看（R4.4），不允许盲提交。

## 测试策略

| 层 | 覆盖 |
|---|---|
| store 集成测试（`//go:build integration`） | 下线实体不出现在遍历结果里，**且其下游也不出现**；依赖边仍在库中；`active` 实体不受影响 |
| impact 单测 | 自动 potential → ruled_out；人工 confirmed / ruled_out 不被覆盖 |
| api 测试 | admin 门禁；preview 空集合返 `[]`；存在 active Fault 且未选 cascade 时 409；重复下线 200 短路 |
| 迁移往返 | 全新 PG 上 up → down → up；既有行迁移后全部 `active` |
| E2E | 下线一个挂着 active Fault 的实体 → Fault 收口、Incident 未被自动关闭、时间线写明原因是下线 |

## 迁移与兼容

- 纯加列，默认值使既有行为逐字节不变（对齐不变量 10 的 opt-in 口径）。
- 回滚安全：旧版本代码不读新列，可直接回退镜像而不回退数据库。
- 与 era3-fault-workunit 的 P4（健康证据）无冲突，但两者都会改 `RebuildFault`，**并行时必须约定谁先合**。
