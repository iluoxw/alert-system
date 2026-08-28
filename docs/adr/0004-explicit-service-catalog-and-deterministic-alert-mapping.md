# ADR-0004：显式服务目录与确定性 Alert Mapping

## Status

Accepted — 2026-08-02

## Context

Alertmanager 权威同步已经能可靠镜像 Alert，但当前 `resource_nodes` 仍以 `(source, external_ref)` 为身份，早期业务关联器也曾把证据分类写在 Go 代码中。继续为其他业务复制关联器，会让核心逐渐依赖业务私有标签和页面分支。

第一阶段已经确认只做到“应用服务及其负责团队”，并要求未登记、歧义身份和无策略 Alert 保持可见但不能被猜测成 Fault。

## Decision

1. 在现有 `resource_nodes` 上 expand canonical `entity_key / layer / managed`，保留旧拓扑 API 兼容；本里程碑不进行一次性全表替换。
2. `service_registrations` 是应用服务 ownership 的显式权威，必须引用 canonical application entity 与既有 team；canonical entity key 创建后不可修改，遥测不得自动创建团队或修改归属。
3. `entity_aliases` 表达外部身份。相同 `(namespace, value)` 可以被多个实体声明，以便真实记录治理冲突；运行时只有恰好一个 enabled owner 时才解析成功。
4. `alert_mapping_policies` 以按优先级的 Matcher、可选 AlertSource、固定实体或 label→alias、Fault kind/scope、证据角色和 correlation labels 组成。策略无业务专属字段。
5. 只有具备已登记 Alertmanager source、upstream fingerprint 与 occurrence 身份的权威 Alert 才能进入自动 Mapping。其受信标准标签 `sre.entity.key / sre.fault.kind / sre.evidence.role` 可作为无需策略的显式契约；兼容来源只保留 Alert，除此以外禁止按 `alertname` 猜测实体和 Fault。
6. `alert_mapping_results` 保存每条 Alert 最近一次 `mapped / unmapped / conflict / ignored` 结果及解释。未映射与冲突是治理结果，不是管道错误。
7. Webhook 管道与 Alertmanager API 对账共用一个幂等 Projector；它可写 Fault/证据/应用 Impact，但不执行 Alert 级路由、静默或通知。
8. direct impact firing 证据可建立 active Fault，并在应用实体上建立 confirmed Impact；其他证据最多建立 candidate/potential。Incident 仍必须由人宣布。

## Consequences

### Positive

- 新增业务只需要登记服务、Alias 与策略，无需修改核心代码。
- 身份冲突和缺失能被查询、解释和重新评估，不会静默误归属。
- Alertmanager 快路径和慢路径最终落到相同 Fault 结果。
- 复用现有 Fault、证据、Impact 和 Incident 生命周期，避免平行领域模型。

### Negative

- 服务接入增加显式治理成本；Alias 和策略发布前需要维护人员理解标签契约。
- 当前 expand 模型仍保留 `resource_nodes.source/external_ref` 兼容字段，完整 EntityRelation 迁移尚未完成。
- 第一阶段每条 Alert 同时只保存最近一次 Mapping 结果；完整策略版本审计/回滚历史需要后续追加不可变发布记录。

### Neutral

- 历史业务专用关联器已移除；新链路统一走 AlertMappingPolicy。

## Alternatives Considered

### 为每个业务实现 System Pack/Connector

拒绝。接入速度快，但会把业务证据枚举、Fault kind 与 UI 分支固化进核心，CCH 之后无法线性扩展。

### 自动从 service/namespace 标签创建服务和团队

拒绝。遥测标签缺少组织权威，拼写错误或租户污染会改变责任边界。

### Alias 表强制 `(namespace, value)` 唯一

拒绝。数据库拒绝第二个声明会隐藏实际冲突，也无法形成可治理的隔离队列。

### 本里程碑直接替换全部 resource_nodes/dependency_edges

拒绝。影响既有拓扑与 Fault 工作台过大；采用 expand-and-contract 可独立验收服务接入主线。

## References

- `docs/adr/0003-alertmanager-authoritative-layered-entity-model.md`
