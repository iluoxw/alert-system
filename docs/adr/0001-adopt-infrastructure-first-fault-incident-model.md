# ADR-0001：采用 Infrastructure-first 的 Fault / Incident 分层模型

## Status

Accepted

## Context

核心用户是维护大量共享基础设施的平台型 SRE。一项 Kubernetes、数据库、网络、存储或消息队列能力会同时支撑多个业务系统；一个业务系统也会依赖多项基础设施能力。现有系统只有扁平 `resource_ref`、按 fingerprint 去重和通知分组，CMDB Lookup 仍为空实现，无法计算依赖传播、受影响业务和爆炸半径。

上一轮以 Incident 为首页主实体的设计跳过了这层供给关系，导致业务影响只能写成静态文案，无法被真实数据支撑。

## Decision

采用 `Signal → Alert → Fault → Incident → Response → Recovery → Learning` 分层：

1. `Alert` 是观测到的异常证据；既不是技术故障，也不是响应事故。
2. `Fault` 是发生在 Resource 或 Infrastructure Capability 上的技术故障，聚合具有因果一致性的告警证据。
3. `Incident` 是围绕一个或多个 Fault 建立的人工响应实体；只有存在显著业务影响、跨团队协作或升级需要时才创建。
4. 用有方向、多对多的依赖图连接 Resource、Infrastructure Capability 和 Business System。外部 CMDB 继续作为 source of truth，本系统只保存服务响应所需的只读投影与同步状态，不提供 CMDB 编辑能力。
5. 告警与 Fault 使用带角色的多对多关联：`trigger`、`cause_candidate`、`symptom`、`supporting`、`excluded`，同时保存来源、置信度、依据与人工裁决。
6. 关联分析以异步旁路运行，不修改既有 `dedup → route → group → silence → inhibit` 管道，不阻塞告警接入与通知。
7. 默认首页从基础设施责任范围、当前 Fault、业务影响与爆炸半径出发；Incident Room 是升级进入协作响应后的下一层。

## Consequences

### Positive

- 符合平台型 SRE 的实际责任模型，能从基础设施异常反查业务影响。
- 明确区分去重、通知分组、故障关联和事故响应，避免每条告警都变成 Incident。
- 支持人工确认根因、拆分错误关联，并为自动化关联保留可信度与审计依据。
- 保持外部 CMDB 为权威来源，避免在告警系统内重复建设资产管理。

### Negative

- 需要新增拓扑只读投影、Fault 生命周期及多张关联表，交付复杂度高于直接新增 Incident 表。
- 拓扑数据可能过期或不完整，爆炸半径只能是“基于当前拓扑的推断”，不能宣称绝对事实。
- 错误的依赖边会扩大误关联，需要提供数据新鲜度、证据解释、人工拆分和排除能力。

### Neutral

- PostgreSQL 邻接表足够支撑当前内部平台规模；暂不引入图数据库。
- AI 只能产生建议性关联或根因假设，人工裁决始终优先。

## Alternatives Considered

**仅使用 Incident，Alert 直接关联 Incident**

- 拒绝：无法表达未达到事故级别的技术故障，也会迫使每次关联都进入协作流程。

**把 Fault 与 Incident 合并成一个实体**

- 拒绝：技术故障的生命周期与团队响应生命周期不同；共享基础设施异常可能先形成 Fault，确认业务影响后才宣布 Incident。

**每次查询时实时调用 CMDB，不保存本地投影**

- 拒绝：CMDB 延迟或不可用会阻塞爆炸半径计算，且无法稳定回放某次故障发生时的依赖证据。

**立即引入图数据库**

- 暂不采用：当前规模与查询深度可由 PostgreSQL 邻接表和递归 CTE 支撑，引入新存储会增加运维复杂度。

## References

- `docs/plans/2026-08-01-infrastructure-first-fault-incident-design.md`
- `.spec-workflow/specs/alert-system-mvp/design.md`
- `internal/enrich/cmdb.go`
