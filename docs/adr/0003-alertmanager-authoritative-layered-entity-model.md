# ADR-0003：以 Alertmanager 为 Alert 权威并采用分层实体图驱动 Fault/Impact

## Status

Accepted

## Context

alert-system 当前已经具备 Alert 入站、去重、路由、分组、静默、抑制、通知和 Incident-first 纵向切片，但同时存在三个结构性问题：

1. 本地管道复刻了 Alertmanager 的部分语义，来源事实、本地 Ack 和人工 Resolve 又混在单一 Alert 状态中，可能产生双权威和状态冲突；
2. 现有拓扑以 source/外部引用及业务样例为中心，E2B 相关器、页面和 Runbook 出现硬编码，新增 CCH 等业务时容易演变为每业务一套插件；
3. 依赖图可推导爆炸半径，但无法单独证明应用已经受影响；把 potential 直接当 confirmed 会制造错误 Incident。

同时，SRE Control 的定位不是遥测存储系统。Prometheus/Thanos/VictoriaMetrics/OTel 后端继续保存原始 metrics/logs/traces；外部 rule evaluator 已经负责告警规则；Alertmanager 已经负责 Alert 生命周期、分组、路由、静默和抑制。

需要一个能兼容 IaaS、PaaS 和应用服务，又不要求每个业务拟合专用代码的公共模型。

## Decision

1. **第一阶段以 Alertmanager 为唯一 Alert 权威。** Prometheus、Thanos Ruler 或 vmalert 继续评估规则；SRE Control 不实现规则工作站。
2. **使用 webhook 快路径与 `/api/v2/alerts` 对账慢路径。** webhook 先持久入队，API 周期修复因通知语义产生的缺口。一个 HA Alertmanager 集群对应一个逻辑 `AlertSource`。
3. **Alert occurrence 身份为 `source_id + upstream_fingerprint + starts_at`。** 重复写幂等，乱序事件不得回退状态；API 失败只标记 source stale，成功快照连续缺失后才关闭 Alert。
4. **Alert 使用三轴状态。** `source_state` 和 `suppression_state` 仅由 Alertmanager 同步器写入；`response_state` 由 SRE Control 写入。Alertmanager 来源禁用本地 Manual Resolve。
5. **Alertmanager 来源绕过本地 route/group/silence/inhibit 与重复 Alert 通知。** SRE Control 不成为现有 Alertmanager 通知链的关键依赖。
6. **使用 canonical `Entity`、`EntityAlias`、`EntityRelation`。** 层级为 infrastructure/platform/application；关系第一阶段为 runs_on、depends_on、part_of，方向从消费者/从属方指向提供者/宿主方。
7. **应用服务与 owning team 必须显式登记。** 遥测发现只能创建 unmanaged 占位实体，不能创建团队或推断 ownership。
8. **只有显式 `AlertMappingPolicy` 或受信标准标签能自动创建 Fault 证据。** 未映射 Alert 进入治理队列；不根据 alertname 猜测实体和故障类型。
9. **Impact 只有 potential、confirmed、ruled_out。** 图遍历只能产生 potential；应用 SLO/synthetic 或人工判断才能确认或排除。第一阶段不实现完整冗余与容量推理。
10. **Incident 可以关联多个 Fault。** 维护一个可审计变更的 primary fault；关闭要求所有关联 active Fault 均已处理或被人工排除。
11. **E2B 和 CCH 是验收场景而不是领域分支。** 两者通过相同的服务登记、实体别名、关系和 Alert Mapping 接入。

详细契约、非功能目标、失败模式与 E2E 门禁见 `docs/plans/2026-08-02-alertmanager-authoritative-sre-control-design.md`。

## Consequences

### Positive

- Alert 生命周期只有一个权威，避免 SRE Control 与 Alertmanager 对 firing/resolved、silence 和 inhibit 产生冲突。
- webhook 提供实时性，API 对账提供最终收敛，可覆盖 Alertmanager 通知分组和抑制导致的事件缺口。
- 新业务主要增加登记数据、别名、关系和映射策略，不需要修改核心领域代码。
- Alert、Fault、Impact 和 Incident 的事实边界明确，依赖图不会被误当成用户影响证据。
- 分层实体图能用同一模型表达物理机、Kubernetes、数据库和应用服务，并保留渐进扩展空间。

### Negative

- SRE Control 第一阶段不能统一编辑和发布 Prometheus 告警规则。
- 同时维护 webhook 和 API 对账增加同步器、状态收敛和测试复杂度。
- 显式服务登记、alias 治理和 mapping 发布带来接入成本。
- 不实现冗余推理意味着部分 Impact 需要业务探针或人工判断才能排除。
- 从当前单状态 Alert、source-scoped topology 和 E2B 硬编码迁移需要兼容期。

### Neutral

- Alertmanager 的现有通知渠道继续工作；SRE Control 的 Incident 通报属于另一层协作事件。
- 原始遥测仍由现有观测平台保留，SRE Control 只保存证据引用和领域状态。
- Event Recorder、规则工作站、业务旅程和自动冗余策略都保留为后续可选演进，而不是隐含承诺。

## Alternatives Considered

### 现在实现类似 Thanos Ruler/vmalert 的规则工作站

拒绝。它会同时引入规则存储、调度、评估、HA、租户隔离、发布回滚和 Alertmanager 兼容问题，且不能直接解决当前 Fault/Impact/Incident 的领域缺口。只有外部规则治理成为主要瓶颈时才重新评估。

### 只消费 Alertmanager notification webhook

拒绝。notification webhook 受 route、group、repeat interval、silence 和 inhibit 语义影响，不能可靠表达 Alertmanager 当前完整 Alert 集合。必须配合 API 对账。

### 保留本地 route/group/silence/inhibit 作为第二套控制面

拒绝。对 Alertmanager 来源会形成双权威、重复通知和不可解释的状态差异。兼容来源可以在迁移期暂时保留，但不是目标架构。

### 每个业务实现 Connector/System Pack

拒绝作为默认接入模型。业务专用采集适配器可以存在于边缘，但核心接入应依靠通用服务登记、实体别名、关系和映射策略。否则 E2B、CCH 之后会持续复制领域与 UI 分支。

### 以 ManagedSystem 作为所有拓扑的强制根节点

拒绝。基础设施和平台实体往往被多个业务共享，强制归属单一系统会扭曲依赖关系。应用服务及其 owning team 是第一阶段明确的业务终点，不是全图根。

### 第一阶段构建完整的冗余、主备、ISR 和容量推理引擎

拒绝。依赖配置质量和平台特定语义过多，错误自动确认的风险高。第一阶段只产出 potential，再由 SLO、synthetic 或人确认/排除；稳定重复模式成熟后再策略化。

## References

- `docs/adr/0001-adopt-infrastructure-first-fault-incident-model.md`
- `docs/adr/0002-use-e2b-failed-to-place-as-golden-scenario.md`
- `docs/plans/2026-08-02-alertmanager-authoritative-sre-control-design.md`
- [Prometheus Alertmanager overview](https://prometheus.io/docs/alerting/latest/alertmanager/)
- [Alertmanager configuration](https://prometheus.io/docs/alerting/latest/configuration/)
- [Alertmanager API](https://github.com/prometheus/alertmanager#api)
- [Thanos Rule](https://thanos.io/tip/components/rule.md/)
- [VictoriaMetrics vmalert](https://docs.victoriametrics.com/victoriametrics/vmalert/)
