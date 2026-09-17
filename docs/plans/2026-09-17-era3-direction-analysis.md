# 时代三方向分析：领域模型、MTTA/MTTR 断层、漏报与风暴、产品定位

> 状态：**探索输入，未裁决**。本文汇总 2026-09-17 三轮讨论（领域对象关系、时代一→二变迁与演进方向、漏报与风暴风险），
> 并合入产品侧的同类产品分层与边界分析，供多个 Agent 并行探索。任何实现前须经主对话或用户裁决，
> 并继续遵守 `CLAUDE.md` 核心架构不变量。
>
> 代码基线：`main@d27e397`（tag `v0.2.0`）。uat 数据快照时点：2026-09-17。

## 阅读约定

- **✅ 已核实**：通过代码、迁移约束或 uat 只读查询确认，附文件位置。
- **🔶 推断**：基于代码阅读或经验的判断，尚未实测，并行探索时应优先证伪。
- **❓ 待裁决**：存在多个合理方案，需要用户决定。
- 引用的 uat 数字只代表快照时点，探索时以实时查询为准。

---

## 0. 一页结论

1. **定位不变**：继续做「Alertmanager 之上的 Infrastructure-first 事故响应操作系统」。不做第二套 Alertmanager，
   不做规则工作站，不做通用 ITSM / 观测平台 / Agent 聊天。
2. **时代二最大的断层是送达，不是理解**：Fault 已经能把多条告警收敛到同一个现实对象，但 Fault 出现时
   **不通知任何人、没有确认人、没有确认时间**（✅）。系统理解得最清楚的对象，恰恰是它不推送的对象。
3. **度量停在时代一**：现有 MTTA 只统计 Alert 确认（✅ `internal/ops/report.go`），而时代二里 Alert 确认只是本地协作；
   没有 Fault MTTA，也没有任何 MTTR。目标是降低 MTTA/MTTR，但目前无法度量。
4. **映射覆盖率是一切的前提**：uat 28 条告警只有 4 条完成映射（✅）。覆盖率不上去，Fault / Impact / Incident
   的价值只作用于少数告警。这与产品侧"映射与实体治理是接入成本和护城河"的判断一致。
5. **两个最危险的静默失效**：上游采集目标注销导致的**假解除**（✅ uat 实证），以及 scheduler / 管道卡死后
   **健康检查仍然通过**（✅）。两者都不会触发任何报警。
6. **现有通知引擎有风暴放大缺陷**：分组只合并首条消息，重复提醒和升级链仍按组内每条告警各起一份（✅）。
   若复用该引擎做 Fault 级通知，必须先修。

---

## 1. 领域模型：五个对象的关系

五个对象分两类：**现实里有什么**（Entity、Service）与**发生了什么事**（Alert、Fault、Incident）。后者都挂在前者上。

```
                    ┌──────────── 现实世界（相对稳定）────────────┐
  EntityAlias ──N:1─┤  Entity（resource_nodes）                    │
  prometheus.instance│   layer: infrastructure / platform / application
                    │   │  ▲ 1:1（仅 application 层）               │
                    │   │  Service（service_registrations）         │
                    │   │  负责团队、tier、runbook 等责任信息        │
                    │   └─ dependency_edges（N:M，consumer → provider）
                    └──────────────────────────────────────────────┘
                              ▲ N:1              ▲ N:M
                              │ 挂在哪个实体      │ 影响了哪些应用
  ┌────────── 发生的事（有生命周期）───────────┐   │
  │  Alert ──N:M 证据──► Fault ────────────────┼───┘ fault_impacts
  │                     (fault_alert_links)    │     potential / confirmed / ruled_out
  │                        │ N:1               │
  │                        ▼                   │
  │                    Incident                │
  │                    primary ×1 + supporting ×N
  └────────────────────────────────────────────┘
```

| 关系 | 基数 | 约束 / 写入方（✅ 迁移已核实） | 含义 |
|---|---|---|---|
| Service → Entity | 1:1 | `service_registrations.entity_id UNIQUE`，只对应 application 层 | Service 是应用实体的**责任信息**，不是并列的实体表 |
| Alias → Entity | N:1 | `UNIQUE(namespace, alias_value, entity_id)` | 同一现实对象在各来源里的名字；同一 alias 指向多个实体时映射结果为 `conflict` |
| Entity ↔ Entity | N:M | `dependency_edges`，`from`=消费方、`to`=提供方；`source` 区分写入方，`origin` 区分 declared / observed | 依赖图不是树 |
| Alert → Entity | N:1（间接） | `alert_mapping_results`（每条告警一行：策略、实体、Fault） | 必须显式映射（不变量 17），不按 alertname 猜 |
| Alert ↔ Fault | N:M | `fault_alert_links UNIQUE(fault_id, alert_id)`，带 `role` 与 `decision_origin` | 自动映射下一条告警只保留一个 Fault（`RemoveAutomatedEvidenceForAlertExcept` 只删自动挂载），人工可额外挂载 |
| Fault → Entity | N:1 | `faults.resource_node_id`；未 resolved 时 `correlation_key` 唯一 | 同一实体 + 同一 fault_kind = 同一故障，收敛来源于此 |
| Fault → Entity（影响） | N:M | `fault_impacts UNIQUE(fault_id, node_id)`，三态，人工裁决不被自动重算覆盖 | 沿依赖边反向遍历，只记录 application 层 |
| Fault → Fault | N:M | `fault_relations`，`contributes_to` | 跨层因果候选 |
| Fault → Incident | N:1 | `incident_fault_links UNIQUE(fault_id)`，每个事故一个 primary | 一个故障最多进一个事故 |

**易混淆点**

1. Alert 从不直接挂 Incident，链路永远是 Alert → Fault → Incident。
2. 依赖边写在实体之间。依赖图建不全，影响就算不出来（uat 上 capacity_gap 一度只连了一台主机）。
3. 状态归属不同：Alert 的 `source_state` 以 Alertmanager 为准；Fault / Incident 由 SRE Control 和人推进。
   告警解除不会自动关闭 Fault，这正是防假解除的底线。
4. Impact 只落在 application 层，平台与基础设施实体只在遍历路径上。

**uat 实例**（2026-09-17）

```
E2BNodeExporterDown  ─► alias :9100 ─┐
                                     ├─► host(sandbox-001) ─┬─► host_unreachable   monitoring
E2BNomadAgentDown    ─► alias :4646 ─┘                      └─► nomad_agent_down   monitoring（假解除）
E2BNomadAgentsCoverageGap  ─┐ 固定目标
E2BNodeExporterCoverageGap ─┴──────────► capability:e2b-nomad ─► capacity_gap      candidate

platform:e2b ─► capability:e2b-nomad ─► host(sandbox-001/002/003)
platform:e2b ─► host(sandbox-001)

INC-000001（monitoring）── primary: capacity_gap ── supporting: host_unreachable
```

---

## 2. 时代一 → 时代二：本质变迁与 MTTA/MTTR 拆解

### 2.1 本质变迁

| 维度 | 时代一：自建告警控制面（迁移 001–011） | 时代二：Infrastructure-first（迁移 012–025） |
|---|---|---|
| 核心对象 | Alert | Fault / Incident |
| 回答的问题 | 这条告警推给谁、推几次 | 哪个东西坏了、影响了谁、谁在处理 |
| 告警状态权威 | 本系统 | Alertmanager |
| 降噪方式 | 分组、模板指纹、风暴汇总、抖动抑制（基于**相似度**） | 实体解析后按 correlation_key 收敛（基于**同一个现实对象**） |
| MTTA 抓手 | 升级链、值班、重复提醒 | **交给了 Alertmanager** |
| MTTR 抓手 | 几乎没有 | 爆炸半径、多 Fault 事故、恢复门槛 |

一句话：**时代一擅长送达但不理解，时代二能理解却不再主动推送。**

注意：时代一的机制没有被删除，对 generic / Grafana 来源仍完整运行；只有 Alertmanager 来源绕过它
（✅ 只有 `DedupStage` 与 `MappingStage` 的 `RunForAuthoritativeSource()` 返回 true）。

### 2.2 按阶段对照现状

```
MTTA = 检测 → 聚合成可处理的单元 → 送到对的人 → 确认
MTTR = 确认 → 定位（什么坏了 / 影响谁 / 最近改了啥）→ 缓解 → 验证恢复 → 收尾
```

| 阶段 | 现状 | 证据 |
|---|---|---|
| 聚合 | ✅ 有效 | 4 条字段毫不相同的告警经别名解析收敛成 1 个 Fault |
| 聚合的前提 | 🔴 映射覆盖率 14% | ✅ uat 4/28；策略在 09-12 之前为 0；新进入的 iowait 告警全部未映射 |
| 送到对的人 | 🔴 Fault 出现时零通知 | ✅ `internal/mapping`、`internal/impact`、`internal/store/faults.go` 无任何通知依赖；`faults` 表无 owner / acked_by / acked_at |
| 确认 | 🔴 确认停留在 Alert | ✅ 不变量 14：Alert Ack 只是本地响应协作 |
| 度量 | 🔴 停在时代一 | ✅ `ops/report.go` `DailyStats.MTTAAvg` 基于告警确认；无 Fault MTTA、无 MTTR |
| 定位：影响谁 | ✅ 可用 | 爆炸半径 + 人工确认 confirmed |
| 定位：最近改了啥 | 🟡 数据有，未接到故障 | ✅ `change_events` 已接入，仅在告警详情按 `resource_ref` 展示（`internal/api/alerts.go`） |
| 缓解 | 🟡 默认关闭 | ✅ Runbook 默认 `DisabledExecutor`（不变量 12） |
| 验证恢复 | 🟡 依赖建模完整度 | 见 §3 M1 |
| 收尾 | ✅ v0.2.0 已补 | `incident_recovery_nudge` |

**关于假解除的一个修正**：代码里存在 `source_resolution_reason = 'source_reconciled_absence'`（✅
`internal/store/alerts_authority.go` `MarkMissingAfterSnapshot`），但 uat 上 `E2BNomadAgentDown` 的解除原因是
`source_webhook`，即 Alertmanager 按正常流程发出 resolved。根因在更上游：采集目标注销 → 序列消失 →
规则不再触发。**SRE Control 从自身数据无法区分这种假解除。** 真正起作用的是上游 `CoverageGap` 这类数量规则，
加上 capacity_gap 被纳入同一事故，恢复提醒判断"证据未全部解除"。

### 2.3 可免费获得的度量（✅ 字段已存在）

- 事故漏斗：`faults.first_detected_at → incidents.declared_at → mitigated_at → monitoring_at → resolved_at`
- Fault 级：`first_detected_at`、`resolved_at` 已有；**缺确认时间**（需迁移）

---

## 3. 漏报与风暴风险

### 3.1 漏报

| # | 路径 | 触发 | 后果 | 依据 |
|---|---|---|---|---|
| **M1** | 上游规则 | 采集目标注销 → 序列消失 → 规则不再触发 → Alertmanager 正常 resolved | 故障仍在但告警"恢复"，SRE Control 分辨不出 | ✅ uat 实证 |
| **M2** | 系统自身 | scheduler 或管道协程卡死 | **所有定时器静默停止**（发送、升级、重复、恢复提醒） | ✅ `/healthz` 恒 200；`/readyz` 只 `Pool.Ping`（`cmd/server/main.go` `RegisterHealthRoutes`）；uat 单副本、liveness 初始延迟 490s；scheduler 历史上卡死过 |
| **M3** | 通知发送 | 渠道故障超过 3 次重试（最坏约 25s） | 永久失败不重试，只留时间线 `notify_failed` | ✅ `internal/notify/sender.go` `defaultMaxAttempts=3` → `markFailed` |
| **M4** | generic / Grafana 接入 | 管道 stage 报错 | 事件标 `failed` 后不再消费，需人工 | ✅ `internal/store/events.go` `MarkFailed` 注释；**Alertmanager 来源可由对账自愈**（`internal/alertsource/reconciler.go` 重新 upsert 并 `Project`） |
| **M5** | Fault 视图 | 无映射策略 | 不进入任何 Fault / 事故 | ✅ 覆盖率 4/28 |
| **M6** | 通知送达 | Alertmanager 接收方配错 | SRE Control 不对 Fault 发通知，无法兜底 | ✅ |
| M7 | generic 配置 | 静默 / 抑制过宽；持续性故障配成 `resolve_policy=ttl` | 告警被吞或被 TTL 解除 | 🔶 取决于配置 |

### 3.2 风暴

| # | 路径 | 机制 | 放大 | 依据 |
|---|---|---|---|---|
| **S1** | generic 通知引擎 | 分组只合并首条消息；渠道升级、重复提醒、人员升级链**按组内每条告警各启动一份** | 组内 N 条：首次 1 条，之后每个周期 **N 条** | ✅ `internal/app/outbox.go` `NotifyGroup` 中 `for _, alert := range active` 分别调用 `engine.Start`、注册 repeat、`chain.Start`；`NotifyRepeat` 按 `AlertID` 单发 |
| S2 | generic 接入 | 降噪默认关闭 | 无保护 | ✅ 不变量 10 |
| S3 | Alertmanager / 自建探针 | 接收方或探针自身 | E2B 探针连发 4 条 | 在 SRE Control 之外 |
| S4 | 发送重复投递 | 已发出未提交即崩溃 | 重复 1 次 | ✅ 已接受的 at-least-once。租约与发送耗时：一次 `notify_send` 领取只处理**一个** `SendRequest`（一个渠道 + 一个目标），扇出在意图侧完成（`outbox.go` `DispatchStep` 按渠道各注册一个定时器；`notifySendPayloadHandler` 只调一次 `sender.Send`），单次最多 3 次尝试、渠道超时 5s，最坏约 25s，远小于 `scheduler.go` `defaultLeaseTTL=5min`，慢发送导致重复领取的概率很低 |
| S5 | 恢复提醒 | 每个未关闭事故每 6h 一条 | 有上限 | ✅ |

### 3.3 若引入 Fault 级通知会新增的风险

| 风险 | 场景 | 必须内置的保护 |
|---|---|---|
| 一个底层故障牵出一片 Fault | 网络分区致多台主机同时 `host_unreachable` | **拓扑抑制**：上游提供方已有已通知的 active Fault 时，下游只挂到该通知下 |
| Fault 状态抖动 | 告警反复 firing / resolved | **一个故障周期只通知一次** + resolved 后冷却期 |
| 对账误判 | Alertmanager API 结果不完整 | 同上冷却期（对账已有最少 2 次确认） |
| 与 Alertmanager 通知叠加 | 同一问题两边各发一遍 | 已映射告警在 Alertmanager 侧降级为低优先级通道 |
| 覆盖不足被当成没事 | 未映射告警无 Fault 通知 | 未映射告警必须继续走 Alertmanager 原通知 |

---

## 4. 同类产品分层与定位（产品侧分析）

| 层 | 代表 | 管什么 | 与 SRE Control 的关系 |
|---|---|---|---|
| 规则 / 求值 | Prometheus Ruler、vmalert、Nightingale | 何时产生告警 | 明确不做；ADR-0003 已拒绝规则工作站 |
| Alert 控制面 | Alertmanager | 去重、分组、路由、静默、抑制、Alert 级通知 | 已定为 Alert 唯一权威，本地管道对 Alertmanager 来源旁路 |
| On-call / 最后一公里 | PagerDuty、Flashduty、PageFire、IncidentRelay、旧 Grafana OnCall | 排班、电话 / 短信升级、认领 | 重叠在"叫醒人"；当前主通道是飞书卡片，不是电话 paging |
| 事故协作 / IR | incident.io、Rootly、FireHydrant | 宣布事故、聊天室指挥、时间线、复盘 | 重叠在 Incident Room；西方产品偏 Slack，很少把 Fault + 保守爆炸半径做成一等公民 |

另有 Keep 这类"告警瑞士军刀"（多源汇聚、降噪、工作流）：争夺的是 Alert 汇聚层，不是基础设施 Fault 图。

**稀缺的缝**：在已有 Alertmanager 之上，为「共享基础设施 → 多业务影响」建立**可解释的 Fault / Impact**，再决定是否升级为 Incident。
PagerDuty / Flashduty 强在叫醒，incident.io 强在聊天协作；几乎没有产品把「图可达 ≠ 真实影响」「人工宣布 + 双重关闭门槛」
「假解除不能自动关单」写成硬不变量——而这些已写进 `CLAUDE.md`，且 uat 已踩过 Nomad agent 假解除的坑。

---

## 5. 未来边界（产品侧分析）

**做（核心）**

- **实体 / 别名 / 映射治理**：接入成本与产品护城河；映射质量直接决定 Fault 是否可信
- **Impact 证据链**：potential → confirmed / ruled_out 要更好用，不能只靠人手点
- **Incident 指挥与飞书原生协作**：卡片动作、状态通报、恢复提醒已起步，向"飞书里的 incident.io"演进，而非网页版 PagerDuty 克隆
- **Recovery / Learning**：假解除提示、关闭门槛、复盘入口；Learning 先做可检索的时间线与证据，再谈 AI 总结
- **运维可靠性**：claim/lease、retention、对账这类"线上才暴露"的问题继续硬化

**明确不做（至少下一阶段）**

- 规则工作站 / Ruler 替代
- 对 Alertmanager 来源再跑一套 route / group / silence
- 通用 CMDB 编辑、工单资产 ITSM
- Service Observability Phase 3「生产观测 attestation」主线（已不在 main；观测仍归 Prometheus / OTel）
- 高风险自动关单、自动宣布 Incident、任意 shell Runbook

**可选外挂（按需，不当作身份）**

- 需要电话 / 短信硬升级时：对接 Flashduty / PagerDuty 作为 paging 出口，SRE Control 仍拥有 Fault / Incident 语义
- AI：只做异步建议与复盘草稿，永远不进持锁事务（不变量 9）

**产品侧优先级**：映射与实体治理体验 → Impact 确认工作流 → 飞书 Incident 指挥闭环 → 复盘 / 学习。

---

## 6. 两份分析的合流

### 6.1 一致

| 主题 | 工程侧（§2–3） | 产品侧（§4–5） |
|---|---|---|
| 映射治理是前提 | 覆盖率 14%，P0 | 护城河，优先级第一 |
| 不重做 Alertmanager | 不在 SRE Control 重发 Alert 级通知 | 明确不做 |
| 假解除 | uat 实证，靠聚合证据 + 建模完整度防御 | 市场缺口：几乎没有同类产品把「假解除不能自动关单」写成硬不变量；在做什么中落为 Recovery 的假解除提示 |
| AI 边界 | 只做确定性 | 只做异步建议与草稿 |
| 可靠性 | M2 元监控、M3 发送失败兜底 | 运维可靠性继续硬化 |

### 6.2 张力与缺口（需要在探索中澄清）

1. **Fault 级送达不在产品侧优先级里**。工程侧认为这是时代二 MTTA 的最大断层；产品侧把"叫醒人"定位为可选外挂。
   两者可以兼容：**SRE Control 拥有 Fault 通知的语义**（谁、何时、是否抑制、谁确认），**送达通道可外挂**
   （飞书卡片为主，电话 / 短信经 Flashduty / PagerDuty）。❓ 需裁决 Fault 级通知是否进入 v0.3 范围，以及排在第几。
2. **度量不在产品侧优先级里**。目标是降低 MTTA/MTTR，但当前无法度量。事故漏斗零迁移即可计算，成本极低。
   ❓ 建议作为所有工作流的公共验收口径前置。
3. **"复用时代一引擎"与"不做第二套 Alertmanager"不矛盾**：复用的是送达与升级引擎，作用对象换成 Fault，
   不重新对 Alert 做 route / group / silence。但复用前必须修 S1。
4. **Impact 确认门槛影响通知时机**。confirmed 依赖人工或 SLO 探针；以"active + confirmed"作为通知门槛可能太晚，
   以"active"又可能太吵。Impact 确认工作流（产品侧第二优先级）与 Fault 通知门槛是同一问题的两面。

### 6.3 待裁决问题清单

| # | 问题 | 选项 | 影响 |
|---|---|---|---|
| D1 | Fault 级通知是否进入下一阶段 | 进入 / 仅做度量先观察 / 不做 | MTTA 能否下降 |
| D2 | Fault 通知门槛 | active / active + 严重度 / active + confirmed | 噪音与及时性 |
| D3 | 通知意图产生位置 | mapping projector 事务内 / 异步扫描已提交 Fault | projector 纯净性与延迟 |
| D4 | 送达通道 | 仅飞书卡片 / 飞书 + 外挂 paging | 叫醒能力与集成成本 |
| D5 | 时代一本地控制面去留 | 冻结不删 / 自建探针迁往 Alertmanager 后删除 | 维护成本与接入路径统一 |
| D6 | 度量是否作为公共前置 | 是 / 否 | 所有工作流能否验收 |

---

## 7. 建议的并行探索题目

每个题目独立可探索，互不阻塞。探索产出建议为：**事实核实 + 方案对比 + 推荐 + 风险**，不直接改代码。

### E1 映射治理体验（产品优先级 1，工程 P0）

- 问题：如何把覆盖率从 14% 提到 ≥80%，且治理成本可持续
- 已知：待治理结果在 `alert_mapping_results`（`status/reason_code`）；策略在 `alert_mapping_policies`；
  预览端点 `POST /api/v1/admin/alert-mappings/preview`；v0.2.0 起受管实体可通过 API 挂别名与依赖
- 方向：按标签特征（`instance` / `job` / `consul_node`）确定性聚类给出策略建议；覆盖率按来源与团队统计；策略质量反馈
- 起点：`internal/mapping/projector.go`、`internal/api/service_catalog.go`、`web/src/pages/Settings/AlertMappings.tsx`

### E2 Impact 确认工作流（产品优先级 2）

- 问题：potential → confirmed / ruled_out 如何减少纯手点，同时不违反不变量 16
- 已知：confirmed 只能来自应用 SLO / 合成探针 direct evidence 或人工；人工裁决不被重算覆盖（uat 已验证）
- 方向：哪些 direct evidence 可接入；批量裁决；在飞书卡片中裁决
- 起点：`internal/impact/projector.go`、`PUT /api/v1/faults/:id/impacts/:node_id`

### E3 飞书 Incident 指挥闭环（产品优先级 3）

- 问题：从 Fault 出现到事故关闭，哪些动作可以在飞书内完成
- 已知：卡片动作网关与幂等表已存在（迁移 000023、`internal/api/lark_callback.go`）；事故通报与恢复提醒经 `notify_send`
- 方向：宣布、认领、切换 primary、推进状态、恢复确认的卡片化；与 D1 / D4 的关系
- 起点：`docs/plans/2026-08-13-feishu-card-action-design.md`

### E4 Fault 级通知与确认（工程 P1，对应 D1–D4）

- 问题：Fault 通知的最小可行设计
- **为什么不违反不变量 14**：14 禁止对 Alertmanager 来源重复发送 **Alert 级**通知；Fault 是另一类对象
  （N 条告警收敛到同一实体 + 故障类型），Fault 级通知不是重复的 Alert 通知。已有先例：`IncidentOutbox`
  作为非 Alert 对象经同一套 `notify_send` 发通知
- 必须包含：拓扑抑制、一个故障周期只通知一次、冷却期、未映射告警保底、`faults` 增加 owner / acked_by / acked_at
  （**迁移仅作为方案提出，探索阶段不要编写**：迁移编号在并行分支间会冲突，落地时由主对话统一分配）
- 前置：修复 S1（§3.2）
- 验收用例建议：组或 Fault 内 N 条告警时，每个周期只发 1 条
- 起点：`internal/app/outbox.go`、`internal/oncall`、`internal/app/incident_outbox.go`（非 Alert 对象走 `notify_send` 的先例）

### E5 度量（对应 D6）

- 问题：零迁移可得的事故漏斗 + 需迁移的 Fault MTTA，如何进入现有日报或独立页面
- 起点：`internal/ops/report.go`、`internal/app/app.go` 日报装配

### E6 可靠性：元监控与发送失败兜底（M2 / M3 / M4）

- 问题：scheduler / 管道存活如何暴露；发送最终失败如何被人感知；failed 事件如何重放
- 方向：`/readyz` 增加最近一次成功轮询距今时长；外部心跳（Alertmanager 定时 canary，uat 已有 `E2BAlertPipelineCanary` 历史数据）；
  发送失败切备用渠道或发元告警
- 起点：`internal/scheduler/scheduler.go`、`internal/api/directory.go`、`internal/notify/sender.go`

### E7 变更关联（工程 P3，MTTR 主要抓手）

- 问题：如何在 Fault 页面展示 `first_detected_at` 之前 N 分钟内该实体及其依赖方的变更
- 已知：`change_events` 已接入（`internal/ingest/changeevent.go`），当前只按 `resource_ref` 关联到告警
- 起点：`internal/api/alerts.go` 近期变更查询

---

## 附录 A：uat 现场快照（2026-09-17）

- 部署：单副本，镜像 `sre-control-api:0.1.0`（`0.2.0` 已推送至内网仓库，未部署）
- Alertmanager 接入健康：webhook 与对账持续成功，连续失败 0
- 数据：告警 28 条（映射 4 条）、Fault 3 个、Incident 1 个、映射策略 3 条、实体 5 个、别名 6 条
- 解除原因分布：`source_webhook` 21、`source_reconciled_absence` 5（均为测试或 canary 告警）、空 2
- E2B：sandbox-001 / 002 主机在线但 Nomad agent（4646）不可达；sandbox-003 整机不可达；Nomad 仅 15 个 ready 节点；
  CoverageGap 两条告警持续 firing
- INC-000001：monitoring；primary 为 capacity_gap（用户手动设置）；`nomad_agent_down` 未关联

## 附录 B：已知待办（未纳入上述题目）

- uat 部署 v0.2.0（API 与 Web 镜像；部署后 retention 清理首次运行）
- `DB_DSN` 位于名为 `secret` 的 ConfigMap（明文），应迁至 Secret 并轮换；飞书 webhook token 曾在排查中暴露，应轮换
- liveness `initialDelaySeconds=490`、单副本、内存 request 50Mi
- `sslmode=disable`
- SMTP 未配置且无启动期告警
- 前端单 chunk 约 1.5MB；npm audit 1 个 high
