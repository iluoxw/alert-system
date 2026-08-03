# ADR-0005：统一 EntityRelation 与保守爆炸半径

## Status

Accepted — 2026-08-02

## Context

应用服务目录与 Alert Mapping 已能把权威 Alert 稳定投影为 canonical entity 上的 Fault，但应用服务尚不能显式声明自己依赖哪些 Kubernetes、数据库、Kafka、网络或其他共享能力。现有 `dependency_edges` 只由完整拓扑快照写入，并要求 `sync_run_id`；因此无法承载服务负责人维护的长期依赖契约。现有 E2B 关联器可以按单一 topology source 计算爆炸半径，但通用 Mapping 还不能跨来源把基础设施 Fault 投影为应用 Impact。

关键安全约束是：图可达只能说明“可能受影响”，不能证明业务已经退化；拓扑过期、循环或缺失不得触发自动 Incident。

## Decision

1. 继续使用 PostgreSQL `dependency_edges` 作为唯一 EntityRelation 存储，不增加平行关系表或图数据库。关系方向固定为 `from consumer depends_on to provider`。
2. 关系区分两种来源：拓扑适配器写入 `observed` 关系并绑定 `sync_run_id`；服务目录写入 `declared` 关系，`sync_run_id` 为空且由服务登记事务原子替换。
3. 新增显式受管实体登记，只允许创建 `infrastructure / platform` 实体并要求 owning team；canonical entity key 与 layer 创建后不可修改。自动发现实体仍保持 unmanaged，不能借编辑接口改变身份或归属。
4. 应用服务可以声明多个依赖目标与 criticality。第一阶段只治理 `depends_on`；不解释副本、主备、ISR、failover 或剩余容量。
5. 通用爆炸半径按 canonical node ID 跨 relation source 反向遍历，最大深度为 8，使用 path 去环，同一实体保留最短且优先非 stale 的路径。
6. 仅把可达的 application entity 持久化为 FaultImpact，路径可以包含 infrastructure/platform 中间实体。新关系或 Alert 重评估会重建自动 Impact；人工 confirmed/ruled_out 永远优先，不被自动结果覆盖。
7. 基础设施/平台 Fault 对可达应用默认只能生成 `potential`。只有同一应用存在时间窗重叠的 active `direct_impact` Alert 证据且路径非 stale，才可自动升级为 `confirmed`；人工仍可确认或排除。
8. 应用直接 Fault 继续确认自身 Impact。应用直接证据变化或服务依赖变化时，同事务重算相关上游 open Fault，避免依赖用户手工重新投递 Alert。
9. observed 关系超过 24 小时视为 stale：仍保留在解释路径中，但降低置信度并禁止基于它自动 confirmed。declared 关系作为有效配置，直到被服务负责人修改或删除。
10. Incident 门槛不变：active Fault 加至少一个 confirmed application Impact，且仍由人宣布。

## Consequences

### Positive

- E2B、CCH 及新业务使用同一服务依赖契约，无需业务专属传播代码。
- 复用现有 PostgreSQL 事务、递归 CTE、FaultImpact 与人工裁决优先级，实施和运维成本低。
- 关系来源、传播路径、stale 状态和自动确认依据可解释。
- 应用 SLO Alert 与基础设施 Fault 可以在通用模型中双向汇合。

### Negative

- 服务负责人需要维护依赖关系；缺失关系会导致爆炸半径不完整。
- 同步重算受最大深度和 open Fault 数量限制，不适用于无限规模图。
- 时间窗重叠只能证明相关业务退化，不能证明唯一根因；仍需要 SRE 判断。

### Neutral

- `dependency_edges` 表名暂不迁移为 `entity_relations`，API/领域语言使用 EntityRelation，数据库在 contract 阶段再改名。
- observed 拓扑适配器仍可保留来源私有 external ref，但跨来源传播必须依赖已经收敛到同一 canonical node 的关系。

## Alternatives Considered

### 新建 `entity_relations` 并与 `dependency_edges` 并行

拒绝。会导致导入、查询、冲突和 freshness 语义重复，爆炸半径需要 UNION 两套权威。

### 把依赖保存为 `service_registrations.dependencies` JSON

拒绝。无法建立 FK、反向查询、去环、共享实体和路径解释，也无法与观测拓扑汇合。

### 立即引入 Neo4j/JanusGraph

拒绝。第一阶段深度与规模有限，PostgreSQL 递归 CTE 已有测试基础；新存储会引入同步一致性和额外值班负担。

### 图可达直接确认业务影响

拒绝。共享基础设施通常具备副本和降级能力，自动确认会制造错误 Incident。

## References

- `docs/adr/0003-alertmanager-authoritative-layered-entity-model.md`
- `docs/adr/0004-explicit-service-catalog-and-deterministic-alert-mapping.md`
- `docs/plans/2026-08-02-entity-relations-blast-radius-implementation.md`
