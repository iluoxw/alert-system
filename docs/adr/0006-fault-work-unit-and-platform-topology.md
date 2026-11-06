# ADR-0006：Fault 成为工作单元，DevOps 平台成为应用层观测拓扑来源

## Status

Accepted — 2026-09-17（方向讨论与八项裁决见 `docs/plans/2026-09-17-era3-direction-analysis.md`）

## Context

v0.2.0 已把 Alertmanager 定为 Alert 唯一权威，并建成 Entity / Fault / Impact / Incident 分层模型。uat 首次端到端运行后暴露三组事实：

1. **理解层没有消费者**。系统最理解的对象是 Fault，但 Fault 出现时不通知任何人、没有 owner、没有确认时间；MTTA 仍只统计 Alert 确认。时代一擅长送达却不理解，时代二能理解却不再推送。
2. **拓扑是最薄弱环节，但不是因为缺数据**。告警映射覆盖率 4/28；同时 DevOps 平台（ccloud）已通过 APM trace 维护着 WAF → APISIX → App ↔ App → DB/Redis/Oracle 的多层观测拓扑，并按应用计算 `health` 与 `slaStandard`（分钟级刷新）。缺的是身份对齐，而不是边。
3. **告警源比设想的少**。生产只有 Prometheus → Alertmanager 与 Grafana Alerting（内嵌 Alertmanager，含 ES 日志告警）两条线；ccloud 平台告警是用户订阅制、不进本系统。

用户是两侧：平台 SRE（故障处理方）与业务团队（影响接收方，天使团队愿意在卡片上确认影响，前提是系统确实回答了"是我的问题还是平台的问题"）。

## Decision

### 三根支柱

1. **Fault 是工作单元。** 通知、认领、归属（owner team）、度量全部落在 Fault 上；Alert 只是证据。恢复比触发更难成立："没有告警了"只是缺乏证据，不等于恢复（uat 已实证 Nomad agent 假解除）。
2. **拓扑 = 平台 observed + SRE declared，缝合点是 `RELY_NODE` / 集群这一层。** ccloud 提供 App↔App、App→数据存储、入口→App 的 observed 边与应用健康度；SRE 只声明数据存储、APISIX、集群之下的宿主机与能力，以及 trace 盲区（异步、批处理、DNS）。系统在零拓扑时仍有价值（收敛、归属、通知），每多一条边只多一点影响判定，不存在"拓扑完整"假设。
3. **影响自动确认 + 业务侧兜底。** 平台健康度作为不变量 16 允许的应用 SLO direct evidence，结合变更事件消歧后自动把 potential 推进为 confirmed / ruled_out；飞书卡片负责通知、人工推翻与无 APM 应用。业务团队几乎不需要主动做什么就能得到"是不是平台的问题"的答案。

### 一条边界

- **Alert 生命周期层只能有一个，且不是本系统。** Alertmanager 与 Grafana Alerting 各自负责 Alert 的 firing/resolved、去重、分组、静默与 Alert 级保底通知；本系统只拥有契约（标签 + 状态 + 上游指纹 + 时间）之后的一切，永不发送 Alert 级通知。
- **平台是应用层观测地图的权威，本系统是基础设施故障与响应的权威。** 本系统不重建服务地图 UI，只消费；平台不做 Fault。

### 八项裁决

| # | 问题 | 裁决 |
|---|---|---|
| 1 | Fault 通知门槛 | Fault 进入 active 即通知 owner team；影响外播按 Impact confirmed 另行触发，两类通知门槛独立 |
| 2 | Grafana 权威化方式 | 作为独立权威 AlertSource 接入（复用 Alertmanager 适配器与 v2 对账），不改 Grafana 团队配置 |
| 3 | 时代一本地控制面 | 冻结：禁止新增能力，不删除代码；删除待 generic 来源确认为零之后 |
| 4 | 平台拓扑同步方式 | 周期快照保证图存在；Fault 出现后对半径内应用按需刷新健康度保证证据新鲜 |
| 5 | `RELY_NODE` 身份对齐责任方 | 先由 SRE 手工维护别名表（共享 DB 实例数量有限）；CMDB 导入作为第二步 |
| 6 | 自伤消歧 | 应用在故障窗口内有近期发布 → 不自动 confirmed，标记"疑似自身变更"交人裁决 |
| 7 | 业务侧第一入口 | 飞书卡片先行（不依赖平台团队）；平台通知中嵌入反向查询作为争取项 |
| 8 | 度量口径 | 先量先行指标：firing 告警映射率、无 owner 的 active Fault 数、Fault 认领时长、假解除拦截次数、未对齐身份数；MTTR 等 n 足够后再量 |

### 健康度证据的时间约束

健康度是窗口聚合值，不是瞬时状态。设窗口为 W、Fault `first_detected_at` 为 T：

- 只有 `observed_at ≥ T + W` 的样本才主要反映故障期间，之前的样本只能算"观察中"。
- 需要故障前基线：基线本身低于 SLA 时不能归因于本次故障，confirmed 只能人工。
- ruled_out 需要 `T + W` 之后连续 K 个刷新周期达标，单次达标不算。
- 每条证据记录 `(value, sla, window, observed_at)`；人工裁决永不被自动重算覆盖（沿用不变量 16）。

### ccloud 数据契约的已知坑（适配器必须编码）

> 以下每条都有真实响应作为证据，fixture 与逐条论证见 `internal/ccloud/testdata/README.md`。

- **`edge.key` 不可信**：两个视图的方向相反——全链路 77/77 条为 `from->to`，服务拓扑 12/12 条为 `to->from`。
  只信 `from`/`to`，忽略 `key` 与 `sourceInfo`/`targetInfo` 中的名字（`serviceName` 在服务拓扑视图里退化成 key）。
- **`from`/`to` 语义 = 调用发起方 → 被调方**，与 `dependency_edges`（consumer→provider）同向，直接照抄不翻转
  （判据：`App → REDIS` 且 `requestRate≈30.7/s`）。由此产生的缺口：入口链 `WAF → APISIX → App` 的入口在
  `from` 侧，**APISIX/WAF 故障不会经反向遍历传播到应用**——保守可接受，记为已知缺口。
- 节点分类判据是 **`nodeType`，为空时回落 `serviceType`**（`EXTERNAL_HTTP_SERVER` 的 `nodeType` 为空串）；
  `serviceType` 含前导空格（`" POSTGRESQL"`），比较前需 TrimSpace。
- 身份只能取 `node.key`（= `metadata.appId`）：`serviceName` 不唯一（同名不同 key）、可含全角字符；
  appId 存在 24 与 40 两种长度，禁止按格式校验。
- `health` 可空且 **`0.0` 是合法测量值**（APISIX 当前就是 `0.0` 而 `slaStandard` 为 1.0），Go 侧必须 `*float64`；
  `coreMetrics` 的量纲 / 大小写 / 顺序在两个视图间不一致，**不得用于任何判定**。
- `totalCount = 0` 的边表示历史有、窗口内无流量：导入为 observed 且标 stale（样例中占 40/77，是常态）。
- 全链路视图不含 `RELY_NODE`，只有单应用服务拓扑视图才带数据库边：导入基础设施边需按应用逐个拉取。
- 拓扑按项目 / 环境组织：实体必须带 environment。
- 存在自环边（App→自身）：丢弃（注意自环也可能有流量，不能靠 `totalCount` 判断）。
- 身份可能不可解析（Redis 节点 `serviceName` 为 `":0"`）：写入待对齐队列，绝不自动建实体（沿用不变量 15）。
  **队列的锚点用 `node.key` 而非 `host:port`**，否则多个报 `":0"` 的实例会塌进同一行互相覆盖。
- 负责人不在拓扑接口中，需另一接口。

### 外部 IO 的执行位置

ccloud 拉取、健康探测等外部 IO 一律走独立周期 loop（模式同 `internal/alertsource.Reconciler`：事务外 fetch → 短事务写入），**不新增带 IO 的 scheduler timer kind**。不变量 3 的唯一事务外例外仍只有 `notify_send`。

## Consequences

### Positive

- Fault 通知、认领、owner 让"系统知道"变成"人行动"，MTTA 在 Fault 级可度量。
- 拓扑冷启动主要来源从业务团队手填换成平台导入，业务团队只需补平台看不到的少数边。
- 影响确认对已接 APM 的应用自动化，卡片退化为兜底与推翻。
- 告警源模型只剩一种（Alertmanager 契约），Grafana 权威化只是把字面量抽成常量集。

### Negative

- 引入对 ccloud 接口的运行时依赖；接口不可用时拓扑与健康证据停止刷新（observed 边按既有 stale 规则降级，Fault 通知与认领不受影响）。
- 身份对齐表需要 SRE 持续维护；未对齐 `RELY_NODE` 数量必须作为先行指标盯住，否则爆炸半径会悄悄变小。
- 健康度 W 由平台决定，W 越大自动 confirmed 越慢。
- "平台说你不健康"与"你确认你受影响"在组织里的分量可能不同，需天使团队首次真实事故验证。

### Neutral

- 时代一（generic / Grafana 非权威接入的本地路由、分组、升级链）继续运行但冻结。
- Incident 门槛不变：active Fault + 至少一个 confirmed 应用 Impact，且由人宣布。

## Alternatives Considered

### 把 ccloud 平台告警接入为第三个告警源

暂不做。平台告警是事件型、用户订阅制，生命周期语义未知；接入会把本系统拖回 Alert 级生命周期与保底通知。平台的价值在登记、拓扑、健康度与变更事件，不在告警。

### 把所有来源汇入一个 Alertmanager

不必要。Grafana Alerting 内嵌 Alertmanager 且暴露兼容 v2 API，作为独立权威源即可；汇入会引入跨团队配置变更。

### 等业务团队声明依赖后再算影响

拒绝。要求不掌握信息的人维护信息长期必败；平台已有 observed 边，业务团队只补盲区。

### 健康度直接生成应用层 Fault

拒绝。业务团队已从平台告警得知自身 SLO 破线；本系统不做应用层 Ruler。健康度只作影响证据。

### 用 scheduler timer 执行 ccloud 拉取

拒绝。timer handler 在持锁事务内执行，外部 IO 会重新引入不变量 3 已经用 `notify_send` 独立模式解决的问题。

## 实施路径

本 ADR 只固化决策，执行计划见 `.spec-workflow/specs/era3-fault-workunit/`（`requirements.md` R1–R10 / `design.md` / `tasks.md`）。

| 阶段 | 内容 | 版本 |
|---|---|---|
| P0 | 文档固化（本 ADR / 方向文档状态同步 / CLAUDE.md 不变量 22–24） | v0.3 |
| P1 | 身份对齐 + ccloud 拓扑导入 | v0.3 |
| P2 | Fault owner / 认领 / 通知 | v0.3 |
| P3 | Grafana 权威源 | v0.4 |
| P4 | 健康证据 + 自伤消歧 + 影响外播 | v0.4 |
| P5 | 先行指标 + 可靠性 | v0.5 |

依赖关系：`P1 ∥ P2 ∥ P3` 可并行（P2 的 owner 先取 `resource_nodes.owner_team_id`）；`P4` 依赖 `P1 + P2`；`P5` 依赖 `P2`。

方向文档 §7 的探索题目 E1–E7 与上述阶段的对应关系（含 E1 / E3 未纳入的理由）见
`docs/plans/2026-09-17-era3-direction-analysis.md` §8。
