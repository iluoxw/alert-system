# SRE Control vs PagerDuty vs Flashduty — 竞争性对照

- 读者：正在做 SRE Control（`iluoxw/alert-system`）的人。目标是决定学什么、抄什么、跳过什么、加倍押注什么。
- 调研日：2026-08-21（Asia/Shanghai）。
- 材料纪律：只使用已读材料，不发明功能、不编数字、不把「材料没打开」写成「产品没有」。
- 主材料：
  - `/workspace/research/alert-system.md`（SRE Control / `iluoxw/alert-system`，tree SHA `c42f7c270da493b1cc2ecb1b7f016722854ba0d7`）
  - `/workspace/research/pagerduty-analysis.md`（文档 + EU 控制台 Pass 1）
  - `/workspace/research/flashduty-analysis.md`（文档 + SREMonitor 控制台 Pass 1）
- 补引（只在需要钉一句时）：`pagerduty-docs.md`、`pagerduty-console-pass1.md`、`flashcat-docs-oss.md`、`flashduty-console-pass1.md`
- 中心论点（SRE Control 自己的，不是对标对象的）：**Alertmanager 是 Alert 生命周期的唯一权威；Fault ≠ Incident；SRE Control 镜像 Alert，拥有 Fault / Impact / Incident。**

---

## 1. 一句话定位对照

| | SRE Control v0.1.0 | PagerDuty Operations Cloud | Flashduty（快猫星云 SaaS） |
|---|---|---|---|
| **一句话** | 自托管、AM 下属的**基础设施故障 OS**：不拥有 Alert 生命周期，产品化 Fault + 确认 Impact + 人声明 Incident | 把监控 / CI / 邮件 / API 收成**必须派给人的 Incident**；Service + 一条 EP +（可选）Schedule + 人身上的通知规则 | 多源告警进入之后的**协作空间 On-call**：Channel 拥有故障 / 降噪 / 分派；同一壳里并上 Monitors / RUM / AI SRE |
| **中心对象** | Fault 工作区 + Incident 响应室；Alert 是证据 | Incident（默认由 Alert 触发；手建没有 Alert） | Channel（协作空间）里的 Incident（默认同 `alert_key` 一条告警一条故障） |
| **Alert 从哪来** | 外部 Prometheus / Thanos Ruler / vmalert 求值；AM webhook + reconcile | 上游 + Events API v2 / email；**不自带规则引擎** | 上游 + 标准 Event API / 夜莺；**也可以**自装 Monitors Alert engine |
| **卖给谁、怎么卖** | 自托管 k8s（api/web/migrate 三镜像）；单部署、本地 bcrypt | 多 SKU SaaS：IR / AIOps（事件价）/ Advance（AI Actions）/ Status Pages / Stakeholders / Process Automation | SaaS 套件 + 开源夜莺 / Categraf 做生成；Free 1 个 Channel，Pro / Standard 门控复盘 / 洞察 / 日历 |
| **明确不是** | 不是 AM 克隆，不是 ITSM / CMDB，不是状态页，不是语音 SaaS | 不是 Prometheus 式 Ruler；Rundeck 是「进来之后怎么自动修」 | 不是夜莺的替代；官方：夜莺生成，Flashduty 收口 |

出处：`alert-system.md` §1、§13；`pagerduty-analysis.md` §1；`flashduty-analysis.md` §1。

**读定位时不要对齐的三句话（先钉死，后面所有表都服从）：**

1. 三个产品都有叫 Incident 的东西，**权不一样**。PD 的 Incident 默认由 Alert 触发，也可以 Web / REST 手建（手建没有 Alert）。Flashduty 的 Incident 可以由告警聚合自动生成，也可以控制台手建 / 外部提报。SRE Control 的 Incident **只能人声明**，是多 Fault 协作容器，带双关闭门（`alert-system.md` §1、§4.3；`pagerduty-analysis.md` §1；`flashduty-analysis.md` §4.4）。
2. PagerDuty 没有 Fault。Flashduty 没有 Fault。SRE Control 的 Fault 是资源 / 共享能力上的技术失败，EvidenceRole + ImpactState 是一等的。把三层对象压成 PD / FD 的 Event→Alert→Incident，就是放弃论点。
3. Flashduty 的 Channel **不是**通知媒介，是服务 / 团队的告警管道与协作容器（`flashduty-analysis.md` 开篇术语）。SRE Control 的 `notify.Channel` 是运输层接口（lark / email / webhook）。PagerDuty 的管道单元叫 Service。三个「管道」词不要混用。

---

## 2. 对象模型对照：Alert / Incident / Fault / Service / Channel / Impact

### 2.1 六对象谁有、谁拥有、谁创建

| 对象 | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| **Signal / Event** | 入站事件，**不是**持久化实体。适配器产出 `AlertEvent`，dedup 之后禁止再读 `SignalContext` | Event：上游一次 trigger / ack / resolve（或编排改写后的 CEF）。不是一等列表对象。键：`routing_key` + 可选 `dedup_key`（≤255） | Event：上游每一次触发 / 恢复。属于集成（专属或共享） |
| **Alert** | **证据**。AM occurrence（`SourceIntegrationID` + `UpstreamFingerprint` + `starts_at` 幂等）或本地生命周期行。三轴：`source_state` / `response_state` / `suppression_state` | 只能 Events API / 基于它的集成创建，**不能手建**。状态 triggered / resolved（编排语义另有 suppressed / suspended）。**没有 ack** | 同 `alert_key`（≤255）在事件聚合窗口内合并。进度只有 Triggered / Closed，**没有认领态** |
| **Fault** | **一等**。范围 instance / capability。状态 candidate → active → monitoring → resolved（只向前）。自动创建仅 mapping policy 或可信标签 `sre.entity.key` / `sre.fault.kind` / `sre.evidence.role`。未映射进治理队列 `AlertMappingUnmapped` | **没有这个对象** | **没有这个对象** |
| **Incident** | **人声明**的多 Fault 协作容器。显示 ID `INC-` + 6 位数字。状态 declared → mitigating → monitoring → resolved。角色 IC / TL / comms / liaison。Fault 链接 primary / supporting、included / excluded；一 Fault 最多绑一 Incident；一 Incident 一个 primary | 「需要被处理和解决的问题」，**必须派给一个用户**。Alert 触发或 Web / REST 手建。状态 triggered / acknowledged / resolved。标题 = 首条 Alert 标题，之后合入不改标题 | On-call **主对象**。相似告警的组合，**归协作空间**。可自动（聚合）或手建 / 外部提报。短标识 `num` = ObjectID 后 6 位十六进制大写 |
| **Service** | 服务目录 = 实体 / 层 / 负责人，**不是** PD Service。三层拓扑：infrastructure / platform / application。节点：resource / infrastructure_capability / business_system | **收事件、恰好挂一条 EP、长 Incident 的单元**。拥有 integration key、urgency、ack timeout、auto-resolution、grouping、conference | 没有与 PD Service 对等的一等对象。最接近的是 Channel。账户级有服务日历（Pro，给分派 / 静默做日期过滤） |
| **Channel** | **没有 Channel-as-service**。`notify.Channel` = 运输层 Kind。政策 step 里写字符串 kind。团队通知目标目前是 `TeamID` UUID（`outbox.go` 已知缺口）。最接近的产品对象是 `notification_policies` + `team_ops_settings.channels` JSON | 没有「协作空间」。Extensions 里有 Slack / Teams；通信通道挂在 Service 摘要条 | **协作空间**。拥有集成、降噪、分派、活计数。Terraform `flashduty_channel`。通知运输层分别住在分派 Notify by / IM 集成 / 个人偏好 |
| **Impact** | **一等状态**：potential / confirmed / ruled_out。图可达 ≠ Impact（CLAUDE inv. 16）。人可否决。Fault 证据角色：direct_impact / cause_candidate / trigger / amplifier / symptom / supporting / excluded | **Impacted Services**：来自 grouping / merge / reassignment，不能手改。Main Service 是被指派的技术服务。没有 potential / confirmed / ruled_out | 材料没有与 SRE Control 对等的 Impact 状态机。Channel 高级有「新奇故障 / 外部提报」，不是 blast-radius 否决 |

出处：`alert-system.md` §1、§4、§11.1；`pagerduty-analysis.md` §3；`flashduty-analysis.md` §3。
