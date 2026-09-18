# Requirements Document — era3-fault-workunit

## Introduction

本 spec 覆盖时代三第一段：**把 Fault 从「系统最理解的对象」变成「人行动的工作单元」**，同时接上 DevOps 平台（ccloud）已存在的应用层观测拓扑与健康度，并把 Grafana Alerting 收敛为第二个权威 Alert 源。

- 决策依据（权威）：`docs/adr/0006-fault-work-unit-and-platform-topology.md`（Accepted — 2026-09-17）
- 方向探索与证据：`docs/plans/2026-09-17-era3-direction-analysis.md`（§7 探索题目 E1–E7 与本 spec 的对应关系见该文档 §8）
- 代码基线：main @ v0.2.0，迁移已到 `000025`

**范围之外（明确不做）**：规则工作站 / Ruler、对 Alertmanager 来源重做本地 route/group/silence/inhibit、ccloud 告警接入为告警源、CMDB 自动导入（手工别名表之后的第二步）、平台通知嵌入反向查询（争取项）、时代一代码删除、**Fault 通知接入人员升级链**（v1 为单次通知，不接 `personnel_escalation` timer）。

## Alignment with Product Vision

ADR-0006 的三根支柱就是本 spec 的验收目标：

| 支柱 | 落地要求 |
|---|---|
| Fault 是工作单元 | R3（归属）、R4（认领）、R5（通知）、R10（度量） |
| 拓扑 = 平台 observed + SRE declared，缝合于 `RELY_NODE` | R1（身份对齐）、R2（观测拓扑导入） |
| 影响自动确认 + 业务侧兜底 | R7（健康证据）、R8（自动确认与消歧）、R9（外播与兜底） |

一条边界（不可越）：**Alert 生命周期层只能有一个，且不是本系统**。R6 只把 Grafana 接入为第二个权威源，绝不为任何来源发送 Alert 级通知。

## 硬约束（违反即返工）

以下直接引自 `CLAUDE.md` 核心架构不变量，实施期不得偏离：

| 约束 | 内容 | 涉及 |
|---|---|---|
| 不变量 3 / 23 | 外部 IO 只走独立周期 loop（模式同 `internal/alertsource.Reconciler`：事务外 fetch → 短事务写入），**不新增带 IO 的 timer kind**；timer handler 只在持锁事务内做纯 DB 工作，唯一事务外例外仍只有 `notify_send` | R1 R2 R7 |
| 不变量 4 | 通知意图持有锁事务内**只准 `Schedule` 一条 `notify_send`**，禁止任何同步外部 IO；幂等键含 round 且 round 是意图时刻捕获的时间戳并随 payload 持久化 | R5 R9 |
| 不变量 14 / 24 | Alertmanager 与 Grafana Alerting 各自是 Alert 的唯一权威，本系统**永不发送 Alert 级通知**；generic 来源本地控制面冻结（禁止新增能力，不删代码） | R6 |
| 不变量 15 | 外部来源 ref 只能作为 alias；平台/遥测数据**绝不自动创建实体或改变归属**，未命中一律进待对齐队列 | R1 R2 |
| 不变量 16 | Impact 只有 `potential / confirmed / ruled_out`；人工裁决永不被自动重算覆盖；观测关系过期只能标 stale，不能提升置信度 | R8 R9 |
| 不变量 17 | 自动 Fault 必须显式准入（AlertMappingPolicy 或受信任标准标签），禁止按 alertname 猜测建 Fault | R5 |
| 不变量 20 | 同一 `pgx.Tx` 内二阶段查询必须先完整扫描 `Rows`、检查 `err` 并显式 `Close`，再发子查询；能一次 JOIN 就不嵌套 | R1 R2 R3 |
| 不变量 22 | Fault 进入 active 即通知 owner team；影响外播按 confirmed 另行触发，**两类门槛互相独立**；「没有告警了」只是缺乏证据，不等于恢复 | R5 R9 |

## Requirements

### R1 身份对齐与待对齐队列

**User Story:** 作为平台 SRE，我希望 ccloud 拓扑里的节点能确定性对到系统已有的 canonical 实体，对不上的进队列让我一次性补齐，从而让爆炸半径不被悄悄缩小。

#### Acceptance Criteria

1. ccloud 节点 SHALL 以 `node.key`（恒等于 `metadata.appId`）作为唯一外部标识解析到 canonical Entity：`CCLOUD_APP` → alias 命名空间 `ccloud.app_id`；`RELY_NODE` → **以 `node.key` 为准**锚定，合法 `host:port` 时额外写 `ccloud.rely_node` alias 便于人识别。**禁止**用 `serviceName` 或 appId 的长度/格式做身份判断（`serviceName` 不唯一且可含全角字符；appId 同时存在 24 与 40 字符两种长度；证据见 `internal/ccloud/testdata/README.md` §6/§7/§11）。
2. WHEN 某 `RELY_NODE` 节点的 `node.key` 在 `entity_aliases` 中查不到匹配 THEN 系统 SHALL 写入 `unresolved_identities`，且 **`value` 取 `node.key`**（不得用 `host:port`——多个实例都报 `":0"` 时会塌进同一主键行互相覆盖），并附 `host_port`、`service_type`、`first_seen`、`last_seen`、`sample_app` 供人识别；**且不得创建任何实体**。
3. WHEN 同一未对齐值在后续同步轮次再次出现 THEN 系统 SHALL 更新 `last_seen` 与样本应用，而不是插入重复行（幂等）。
4. 系统 SHALL 提供 `GET /api/v1/admin/topology/unresolved`（admin only），按 `last_seen` 倒序分页返回；集合字段为空时返回 `[]` 而非 `null`。
5. 未对齐 `RELY_NODE` 数量 SHALL 作为先行指标暴露给 R10。
6. IF 未对齐值被 SRE 手工创建别名 THEN 系统 SHALL 在下一同步轮次自动把它并入拓扑（无需重启或手工触发）。
7. IF 该 ccloud 应用已有 `service_registrations` 登记 THEN 系统 SHALL 把 `ccloud.app_id` alias 合并挂到**已有** application 实体上，不新建实体；IF 未登记 THEN 该应用 SHALL 以 `unmanaged` 状态存在并可被认领，**不得自动创建团队或改变归属**（沿用不变量 15）。

### R2 平台观测拓扑导入

**User Story:** 作为平台 SRE，我希望系统自动消费 ccloud 已维护的拓扑，而不是要求业务团队手填依赖，从而冷启动时爆炸半径就有内容。

#### Acceptance Criteria

1. 系统 SHALL 以独立周期 loop（非 scheduler timer）拉取 ccloud 拓扑：全链路视图 + 按应用的服务拓扑视图；默认间隔 `CCLOUD_SYNC_INTERVAL`（10m），配置为 0 时完全关闭且不产生任何 IO。
2. 导入的边 SHALL 写入 `dependency_edges`，`source='ccloud'`、`origin='observed'`，方向统一为 **consumer → provider**；`sync_run_id` 关联 `topology_sync_runs`。
3. 适配器 SHALL 只信 `edge.from` / `edge.to`，**忽略 `edge.key`**（两个视图的 key 方向相反：全链路 77/77 条为 `from->to`，服务拓扑 12/12 条为 `to->from`，因此不可用），并忽略 `sourceInfo`/`targetInfo` 中的名字（服务拓扑视图里 `serviceName` 退化成 key）。`from` = 调用发起方、`to` = 被调方，**与 `dependency_edges`（consumer→provider）同向，直接照抄不翻转**。
4. WHEN `totalCount = 0` THEN 该边 SHALL 导入为 observed 且标记 stale。
5. 自环边（from == to）SHALL 被丢弃。
6. 导入 SHALL 按 source 隔离：只替换 `source='ccloud'` 的既有边，**不得触碰 `entity_registry` 或其他 source 写入的边**。
7. 重放同一份快照 SHALL 不改变边数量（幂等）。
8. 实体 SHALL 携带 environment（项目/环境）信息。
9. IF ccloud 接口不可用 THEN 系统 SHALL 只降级拓扑与健康证据（既有 observed 边按 stale 规则过期），**Fault 通知与认领功能必须 100% 不受影响**。
10. 节点分类 SHALL 以 `nodeType` 为判据，`nodeType` 为空时回落 `serviceType`；比较 `serviceType` 前 SHALL `TrimSpace`（真实取值含 `" POSTGRESQL"`，带前导空格）。`coreMetrics` SHALL NOT 参与任何判定（两视图的量纲、大小写与顺序均不一致）。
11. 系统 SHALL 显式统计被忽略的 `EXTERNAL_HTTP_SERVER` 节点数；IF 该计数非零 THEN SHALL 在同步日志中提示重新评估忽略规则（样例中该类型无任何边，但它是 `host:port` 形状，一旦带边就意味着在丢真实边）。

### R3 Fault 归属（owner team）

**User Story:** 作为平台 SRE，我希望每一个 active Fault 都有明确的负责团队，从而知道该找谁。

#### Acceptance Criteria

1. `faults` SHALL 增加 `owner_team_id`（UUID，可空）。
2. WHEN 创建或更新 Fault THEN 系统 SHALL 以该 Fault 关联 canonical 实体的 `owner_team_id` 作为 owner（`resource_nodes.owner_team_id`），解析不到时置 NULL。
3. 系统 SHALL NOT 依据 Alert 标签、ccloud 数据或任何不可信输入推断或改变 owner（沿用不变量 15）。
4. IF Fault 被认领（R4）THEN `owner_team_id` SHALL 保持不变，认领只记录动作人，不改变归属。

### R4 Fault 认领与时间线

**User Story:** 作为值班 SRE，我希望在飞书卡片或 Web 上一键认领 Fault，从而让 MTTA 在 Fault 级可度量。

#### Acceptance Criteria

1. `faults` SHALL 增加 `acked_by`（UUID，可空）、`acked_by_name`（TEXT）、`acked_at`（时间戳）。
2. 系统 SHALL 提供 `domain.AckFault(fault, actorID, actorName, now)`：actorID 写入 UUID 列，actorName 写入时间线与 TEXT 列（沿用不变量 7 的双参数口径）。
3. WHEN 对已认领的 Fault 重复认领 THEN 系统 SHALL 按当前状态短路返回成功（HTTP 200 + 当前状态），**不返回 409、不重写时间线**（沿用不变量 6）。
4. 认领动作 SHALL 追加 `fault_timeline_entries` 记录（actor、action、detail、at）。
5. 系统 SHALL 提供 `POST /api/v1/faults/:id/ack`，登录即可（member 可处置）。
6. 飞书卡片回调路径 SHALL 与 REST 端点保持同一语义。**（延后，不在 P2 范围）** 依赖公网可回调的入口，见任务 45；
   P2 期间认领只经 REST 与 Web 完成。
7. Fault 列表与详情响应 SHALL 返回 owner / acked 相关字段。

### R5 Fault 级通知

**User Story:** 作为值班 SRE，我希望 Fault 一进入 active 就收到飞书卡片，并能直接在卡片上认领，从而不必再自己盯着告警列表。

#### Acceptance Criteria

1. WHEN Fault 进入 active THEN 系统 SHALL 在同一事务内产生通知意图，并且**只 `Schedule` 一条 `notify_send`**，不得执行任何同步外部 IO。
2. 通知目标 SHALL 取该 Fault owner team 的 `team_ops_settings.channels`（kind + target 成对）；缺失时回落全局 `FAULT_NOTIFY_FALLBACK_TARGET`。
   飞书有**两条能力不同**的通道，选型直接决定 R5.7 能否成立：
   - `lark`（自建应用 OpenAPI，target 为 `chat_id:` / `open_id:`）：发交互卡片，按钮可回调，是完整形态；
   - `lark_webhook`（群自定义机器人 hook URL）：**只发纯文本，按钮与回调均不支持**，认领退回 Web 端完成。
3. WHEN 同一 `correlation_key` 在冷却窗口（`FAULT_NOTIFY_COOLDOWN`）内刚被 resolved THEN 系统 SHALL 跳过通知。
4. WHEN 该 Fault 的上游 provider 已存在「已通知」的 active Fault THEN 系统 SHALL 不单独通知下游（拓扑抑制），并在下游 Fault 时间线上写明抑制原因。
5. 幂等键 SHALL 为 `fault:{id}|{step}|{channel}|{target}|{round}`（round 随 payload 持久化）。
6. 通知成功发出后系统 SHALL 记录 `owner_notified_at`。
7. **（延后，不在 P2 范围）** 通过 `lark` 渠道发送时，卡片 SHALL 带「认领」按钮（回调 `resourceType="fault"`、`action="ack"`）。
   本条依赖 uat 具备公网可回调入口，实现与验收随任务 45 一并进行。
   P2 期间通知走 `lark_webhook` 纯文本，SHALL 在正文给出 Fault 详情链接，认领在 Web 端完成——这是既定取舍，不视为未满足验收。
8. 同一个故障周期内 SHALL 只通知一次（不重复轰炸）；影响外播不受本条约束（见 R9）。
9. **（延后，不在 P2 范围）** 飞书卡片回调中的「推进状态」类动作 SHALL 限 admin，并 SHALL 校验 actor；`ack` 动作登录即可。
   与 R4.6、R5.7 同属卡片交互，随任务 19 / 45 一并实现与验收。
10. **通知触发 SHALL 覆盖 `AlertProjector.Project` 的全部生产调用入口**，而不仅是 pipeline：
    - `internal/pipeline/stage_mapping.go`（webhook 实时入队路径）SHALL 触发；
    - `internal/alertsource.Reconciler`（周期对账路径，含 `unknown→firing` 恢复与快照缺失两处调用）SHALL 触发——
      这正是「Alertmanager 曾不可用、恢复后重新出现告警」的场景，漏触发等同于漏报；
    - `POST /api/v1/admin/alert-mapping/results/:alert_id/reprocess`（人工重新映射）SHALL NOT 触发，
      因其为治理操作而非新发生的故障；该路径 SHALL 在 `log-implementation` 中记录此结论。
11. 同一次 `candidate→active` 或 `created(active)` 转移 SHALL 只产生一条通知意图，无论由哪条入口触发
    （多入口在同一时刻并发时，由 R5.6 的 `owner_notified_at` 与 R5.5 的幂等键共同收敛）。

### R6 Grafana 作为第二权威 Alert 源

**User Story:** 作为平台 SRE，我希望 Grafana Alerting（含 ES 日志告警）与 Alertmanager 一视同仁地进入系统，从而不必再维护第二条特殊路径。

#### Acceptance Criteria

1. 系统 SHALL 在 domain 层提供权威来源常量集 `AuthoritativeSourceKinds = {alertmanager, grafana}` 与判定函数，**禁止在业务代码中散落字面量**。
2. 现有硬编码字面量 SHALL 全部改为使用该常量集：`internal/pipeline/stage_dedup.go`、`internal/mapping/projector.go`、`internal/store/alert_sources.go`（改为 `kind IN`）、`internal/api/config.go`。
3. Grafana 接入 SHALL 作为独立权威 `AlertSource`，复用 Alertmanager 适配器契约与 v2 对账端点；对账路径 SHALL 按 kind 选择（Alertmanager `/api/v2/alerts`，Grafana `/api/alertmanager/grafana/api/v2/alerts`）。
4. Grafana Alert 的 `UpstreamFingerprint` SHALL 取 `payload.alerts[].fingerprint`；occurrence 幂等键 SHALL 为 `source_id + upstream_fingerprint + starts_at`。
5. 权威来源 SHALL 只执行 authoritative-safe 的 dedup + mapping，跳过本地 route / group / silence / inhibit，并且**不发送 Alert 级通知**。
6. IF 来源不可用或数据过期 THEN 系统 SHALL 标记该来源 stale，**禁止把「暂时看不到」推断为 resolved**。
7. `ValidateConfig` SHALL 接受 `grafana` 作为合法 kind，并保持 Bearer token 鉴权方式不变。
8. 时代一（generic 来源）本地控制面 SHALL 保持可运行且冻结：不新增能力、不删除代码；相关页面 SHALL 标注「仅 generic 来源」。

### R7 健康度证据采集与时间约束

**User Story:** 作为业务团队，我希望系统直接告诉我平台侧今天是否真的破线，从而不必自己去查 APM。

#### Acceptance Criteria

1. `fault_health_evidence` SHALL 记录 `(fault_id, node_id, observed_at, health, sla, window_seconds, source, baseline)`。
2. 采集 SHALL 走独立周期 loop（`CCLOUD_HEALTH_PROBE_INTERVAL`，默认 1m）：列出 open 的 active Fault → 取其爆炸半径内的 application 节点（有 `ccloud.app_id` alias）→ **事务外** `FetchAppHealth` → 短事务写证据 → 触发 `impact.RebuildFault`。
3. 证据 SHALL 带故障前基线（`baseline`）；IF 取不到故障前基线 THEN 结论不得自动 confirmed（只能人工）。
4. 判定 SHALL 满足时间约束：只有 `observed_at ≥ first_detected_at + W`（W = `CCLOUD_HEALTH_WINDOW`）的样本才可用于自动结论；`T + W` 之前的样本只算「观察中」。
5. `CCLOUD_HEALTH_WINDOW` 未配置时采集 SHALL 拒绝启动并显式报错（不允许猜测默认值）。
6. 每条证据 SHALL 保留 `(value, sla, window, observed_at)` 以便人工复核。
7. 健康度采集 SHALL 只作 Impact 证据，**不得生成应用层 Fault**。

### R8 影响自动确认与自伤消歧

**User Story:** 作为业务团队，我希望「是我的问题还是平台的问题」这个问题被自动回答；如果是我自己刚发布导致的，系统不要误判成平台故障。

#### Acceptance Criteria

1. `impact.RebuildFault` SHALL 在既有 direct-evidence 分支之后新增 health 判定分支，产出 `confirmed` / `ruled_out` / 保持 `potential`。
2. `confirmed` 条件 SHALL 同时满足：最新样本 `observed_at ≥ T + W` 且 `health < sla` 且故障前基线 `≥ sla` 且无「疑似自身变更」标记。
3. `ruled_out` 条件 SHALL 为：`T + W` 之后连续 K 个刷新周期（`IMPACT_RULED_OUT_STREAK`，默认 3）均达标；单次达标不算。
4. WHEN 该应用的 `change_events` 在 `[T − CHANGE_SUSPECT_WINDOW, now]` 内存在发布/变更 THEN 系统 SHALL 置 `fault_impacts.flags.suspected_self_change = true`，状态**留在 potential**，交人工裁决。
5. 变更匹配 SHALL 使用已有 `change_events.resource_ref`，并新增 `ccloud.app_id` 键参与匹配。
6. 人工裁决（confirm / rule_out）SHALL 永不被后续自动重算覆盖（沿用不变量 16）。
7. IF 平台发布事件 webhook 不可用 THEN 消歧 SHALL 退化为不生效，并 SHALL 在页面与文档中明示该限制（不得静默生效或不生效）。

### R9 影响外播与业务侧兜底

**User Story:** 作为业务团队负责人，我希望应用被 confirmed 影响时收到飞书卡片，并能在卡片上确认或推翻，从而不必登录系统。

#### Acceptance Criteria

1. WHEN Impact 变为 confirmed THEN 系统 SHALL 通知受影响应用的 owner team 渠道；通知目标与 R5 的 Fault 通知门槛**互相独立**。
2. 外播通知 SHALL 沿用不变量 4 口径：只 `Schedule notify_send`，幂等键含 round。
3. 卡片动作 SHALL 使用 `resourceType="impact"`（value 含 `fault_id` + `node_id`），动作 `confirm` / `rule_out` 落到既有 `PUT /api/v1/faults/:id/impacts/:node_id` 的人工裁决语义。
4. WHEN Fault 变为 resolved THEN 系统 SHALL 向所有 confirmed 影响的团队外播恢复通知。
5. 所有外播 SHALL 落 `fault_timeline_entries` 与通知日志，且通信目标在对外响应中只暴露脱敏引用（沿用不变量 13）。

### R10 先行指标与可靠性

**User Story:** 作为平台 SRE，我希望在 MTTR 数据够用之前就有可看的先行指标，并且系统静默失败时能被我感知。

#### Acceptance Criteria

1. 系统 SHALL 提供 `GET /api/v1/ops/indicators`，返回：firing 告警映射率、无 owner 的 active Fault 数、Fault 认领时长中位数（`acked_at − first_detected`）、恢复提醒拦截次数（nudge 发出时仍有 active Fault）、未对齐身份数（来自 R1）。
2. 指标 SHALL 追加到既有运维日报；运行总览 SHALL 有对应卡片。
3. `/readyz` SHALL 增加 scheduler / pipeline 最近一次成功轮询距今天数，超过 3× 轮询间隔时返回 503；`/healthz` 保持恒 200。
4. WHEN `notify_send` 最终失败 THEN 系统 SHALL 向 fallback target 发送一次元告警（不重试风暴）。
5. k8s 探针改用 `/readyz` 属于平台侧配置变更，SHALL 在本文档与部署说明中标注为需要人工确认的动作，不由本 spec 自动执行。

## Non-Functional Requirements

### Code Architecture and Modularity

- 一切依赖指向 `internal/domain`；domain 保持零外部依赖（纯函数）。
- 所有 store 方法带 `tx pgx.Tx` 参数，事务由调用方开启。
- 新增外部 IO 一律落在 `internal/ccloud/` 包内的独立 loop，不得侵入 `internal/pipeline` 或 scheduler handler。

### Compatibility

- 迁移编号由主对话统一分配（本 spec 使用 000026 / 000027 / 000028），并行分支不得自行占号。
- 现有 API 语义不得破坏：Ack/Resolve 幂等短路、空集合返回 `[]`、401 口径不变。
- 降噪与影响判定相关的新增能力必须 opt-in（零值/缺省短路）。

### Performance

- 拓扑同步与健康探测为周期任务，单轮耗时不影响 HTTP 关键路径；外部调用必须有超时。
- 爆炸半径遍历沿用既有最大深度 8 与递归 CTE。

### Security

- ccloud token 只从环境变量读取，凭据不落日志；沿用 `hex(sha256(token))` 存储口径。
- 新增 admin 端点（R1.4、R10.1）必须 admin only；Fault ack 登录即可。

### Reliability

- ccloud 不可用不得影响既有全部功能（R2.9）。
- 所有 loop 的失败必须写入 `topology_sync_runs` / 日志并退避，不得形成重试风暴。
- 外部 IO 组件不可用时，系统既有功能必须 100% 可用。
