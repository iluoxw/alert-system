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

### 2.2 所有权方向（谁挂谁）

```
SRE Control
  AM / Grafana / generic hook
        | 镜像（权威源只跑安全阶段）
        v
     Alert（证据，三轴）--mapping--> Fault（技术失败）--人声明--> Incident（协作容器）
        |                              |
        |                              +-- Impact potential|confirmed|ruled_out
        +-- 本地非 AM：route / group / silence / inhibit

PagerDuty
  Events API / email / 编排
        v
     Alert --默认触发--> Incident --必须派给--> User
                              ^
                         Service（恰好一条 EP）
                              |
                         EP --> Schedule 和/或 User
                              |
                         人身上的 notification rules

Flashduty
  专属集成直达 / 共享集成+路由 / Event API / 手建
        v
     Event --事件聚合--> Alert --（可选）告警聚合--> Incident
                                                      |
                                                 Channel（协作空间）
                                                      |
                                              分派规则（命中即停）
                                                      |
                                              值班表 / 人 / 团队
```

**意见：** SRE Control 的对象图比另外两家多一层，而且多的那一层（Fault + Impact）才是产品。PD / FD 把「要叫人起来的那一团」直接叫 Incident。SRE Control 把「资源坏了」和「人决定升级成事故」切开——这不是命名洁癖，是关闭门、证据角色、映射治理队列全部挂在这一刀上。对标时不要为了「三列对齐」把 Fault 折叠进 Incident。

### 2.3 同名不同权，再钉四条

| 容易撞车的词 | 不要当成同一物 |
|---|---|
| Incident | PD / FD = 要 page 的那一团（可自动）。SRE Control = 人声明的多 Fault 容器（不可自动） |
| Service | PD = 路由 + EP + 长事故。SRE Control = 实体目录。FD 没有对等物 |
| Channel | FD = 协作空间。SRE Control = 运输层。PD 基本不用这个词当管道 |
| Severity | 三家都有 critical / warning / info 族，但 PD 另有 Urgency（怎么叫）和 Priority（先做谁）。FD 把三档做成 Notify by 矩阵的行。SRE Control 的 severity 在 Alert 上，没有独立 urgency 轴 |

---


## 3. 生命周期与权威来源（谁拥有 Alert 生命周期）

这是整份对照里最不该含糊的一节。

### 3.1 Alert 生命周期：谁说了算

| | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| **Alert 权威** | **Alertmanager**。生命周期、去重、分组、路由、静默、抑制、Alert 级通知全部归 AM。SRE Control **镜像**，AM 源只跑标了 `RunForAuthoritativeSource()` 的阶段（权威安全的 **dedup + mapping**） | Events API 路径。`trigger` 开 Alert；`acknowledge` 把 **Incident** 标 ack（Alert 本身无 ack）；`resolve` 关 Incident。同 `routing_key` + `dedup_key` 在未解决期间并进原 Alert | 标准协议两层接入去重 → 事件聚合（`alert_key` + 窗口）合成 Alert。Alert 进度 Triggered / Closed |
| **人能否手建 Alert** | 不能。人操作的是 Fault / Incident。本地非 AM 可走 generic 信封进管道 | **不能**手建 Alert。手建 / REST Incident **不产生 Alert**，编排 / grouping 都不适用 | 不手建 Alert；手建的是 Incident |
| **人能否手动 Resolve Alert** | AM 权威源：**禁止**，`ErrAuthoritativeSourceManualResolve` → HTTP 409。必须在 AM 里 resolve。本地源按 `ResolvePolicy`（source / ttl / manual） | Resolve Incident → 全部子 Alert resolve。Resolve 全部 Alert → Incident resolve。Events API `resolve` 走监控恢复 | 手动关闭故障会关闭关联告警且不再合入；**不改变**监控状态。监控侧全部 Ok → 故障自动 Closed |
| **Ack 发生在哪一层** | Alert 的 `response_state`：unacknowledged / acknowledged。Ack **不能**改写 `SourceState`。Handoff 改 `AckedBy`、不改 status | **只在 Incident 上**。Alert 无 ack。任何人 ack 都会把 Incident **改派给自己** | **只在 Incident 上**。看见就能认领，不限已分派人。全部取消认领回退 Processing → Triggered |
| **没人 on-call 时** | resolver 空结果写时间线 `nobody_on_call`，**不是错误**，事件留下 | **trigger 事件被丢弃，不可审计** | 无策略 = **接入但不通知**。轮空值班表：单聊不发，**群聊仍发** |
| **解决后再来同一 key** | AM occurrence 幂等键是 `source_id` + `upstream_fingerprint` + `starts_at`（000018）。权威源以 AM 为准 | 同一 `dedup_key` 再 trigger → **新** Alert / Incident，不复活。Reopen 现为 GA（H2 2025 博客） | 同一 `alert_key` 再来，按事件聚合规则开新告警 / 新故障，不复活 |

出处：`alert-system.md` §1、§4.1、§6.1；`pagerduty-analysis.md` §3.1、§4.1、§11.1；`flashduty-analysis.md` §4。

### 3.2 状态机对照

**SRE Control Alert（三轴，CLAUDE inv. 14）**

| 轴 | 值 | 谁写 |
|---|---|---|
| `source_state` | firing / resolved / unknown | AM（或本地源） |
| `response_state` | unacknowledged / acknowledged | 人（REST / 飞书卡片） |
| `suppression_state` | none / silenced / inhibited（silence 优先） | AM 镜像；本地非 AM 才跑本地 silence / inhibit |

遗留投影 `ProjectLegacyStatus`：source resolved → resolved；否则 response ack → acknowledged；否则 firing。允许迁移：firing → acknowledged | resolved；acknowledged → resolved；resolved 无出边。

**PagerDuty**

```
(event) → Triggered ⇄ Acknowledged → Resolved
                ^            |
                +-- ack timeout / unack / snooze expiry
```

Triggered = 人的所有权还没被认领，按 EP 升级。Acknowledged = 停升级。Resolved = 修好了。这是**人的所有权状态**，监控恢复走 Events API `resolve`。

**Flashduty（两轴）**

| 轴 | 值 | 含义 |
|---|---|---|
| 进度 progress | Triggered / Processing / Closed | 人有没有接。Channel 卡片红点只数前两个 |
| 监控 status | 关联告警是否全部 Ok | 人关 ≠ 监控好了 |

**意见：** SRE Control 的三轴比 Flashduty 的两轴更硬，比 PD 的「Alert 无 ack、Incident 三态」更诚实——因为 AM 才是 source 的权威，本地 Ack 本来就不该假装修好了监控。UI 可以学 Flashduty「红点不数 Closed」，但不要把三轴压回 PD 的 triggered/acked/resolved。AM 源禁止手动 Resolve 是正确论点，运营体感会差于「ack-anywhere」的 PD；这是要接受的产品税，不是实现债。

### 3.3 Incident 生命周期（不要和 Alert 混）

| | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 进入 | 人声明。自动 Fault 不能升格成 Incident（CLAUDE inv. 11、17） | Alert 触发（默认）或 Web / REST 声明。自指派手建从 Acknowledged 起、不通知创建者 | 告警聚合（默认关 → 一条告警一条故障）或手建 / 外部提报 |
| 进行中 | declared → mitigating → monitoring。到 monitoring：全部 mitigation 准则过。默认准则：`application_impact_recovered`、`linked_faults_stabilized` | triggered ⇄ acknowledged；snooze 只对已 ack，最大 168h；ack timeout（服务级，默认关）拉回 triggered | Triggered → Processing（有人认领） |
| 关闭门 | **双门**：mitigation + resolution 准则都过；任务全 done / canceled；**没有 included + active 的 Fault**。不能 PATCH 到 resolved | Resolve Incident 或全部 Alert resolve。>100,000 开着 → 24h 强制 resolve。Required Fields on Resolve = EA | 手动关 / 超时关 / 跟随告警关 / 监控全 Ok。Open API 有 resolve / reopen / snooze / wake |
| 关闭之后 | 材料未写 reopen 产品。Learning 只在领域链里有名字，无包无表 | 可 reopen（GA）。同一 key 新 trigger 开新 Incident | 可 reopen。关掉后同 key 开新的 |

SRE Control 默认准则之外，E2B `sandbox_placement_degradation` 有 6 条专用门（`alert-system.md` §4.3）。这是垂直切片，也是风险：种子 runbook 是 E2B 专用，通用页禁止出现 e2b key。

### 3.4 权威分裂的产品含义

| 决策 | SRE Control 已经选了 | 若改成 PD / FD 会丢掉什么 |
|---|---|---|
| AM 拥有 Alert | 不双通知、不再做一遍 inhibit、不在 AM 源上跑本地 route / group / silence | 抄 FD「每个 Channel 自己降噪」或 PD Intelligent Grouping = 和 AM 抢权威 |
| Fault 由 mapping / 可信标签自动建 | 未映射进治理队列，不静默丢 | 抄 PD「没人 on-call 就丢事件」是同一类不可审计 |
| Incident 只由人声明 | 双关闭门有地方挂；通讯角色有容器 | 抄 FD 默认「一条告警一条故障」= 把 Fault 工作区饿死 |
| 图可达 ≠ Impact | 人可以 ruled_out | 抄 PD Impacted Services（分组副作用、不能手改）= 失去否决 |

---

## 4. 分派、值班、升级

### 4.1 接线：谁 page 谁

| | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 最小可运行图 | Team + `escalation_policies`（目标是 **team ID**，再解析 on-call）+ `notification_policies` + 物化 `shifts`。没有 Schedule 也能跑，空班是洞 | **Service → 恰好一条 EP → Level 1 可直打 User**。本 EU 租户证实：Default Service `PATY3M1` → Default EP `PYG7510` → User `P7DCOZA` Always on-call，**0 条 Schedule、0 个 Team** | **Channel → Default 分派规则 → 人**。值班表是账户级对象，被规则引用，不是 Channel 子资源。也能没有排班表永远 on-call |
| Schedule 自己 page 吗 | 不。resolver 只回答「这个 team 在 T 谁在班」 | **不。** Schedule 必须挂到被 Service 使用的 EP 才 page | 不。值班表被 Channel 内规则引用才触达 |
| 升级对象 | 只有 **team**，再取该 team 的 on-call。没有 PD 式 user-as-target，也没有 notify-previous-acker | User 和 / 或 Schedule。每级多用户：Business 50 / Professional·Free 5 | 值班表 / 团队 / 个人 / 组合；可按角色（主 / 备或自定义，空态写 via support） |
| 两条升级带 | **正交**：`personnel_escalation`（人）× `channel_escalation`（媒介） | 一条 EP 管「叫谁」；「用哪套联系方式」在**人**的 notification rules，按 urgency 分两套 | 一条 Channel 内规则同时管匹配 + 升级路径 + Notify by。内部名 `notificationPolicy` |
| 匹配语义 | 通知策略：matcher 绑定团队政策 → 团队默认（空 matcher）→ 全局默认（`team_id IS NULL`）。**总有落点** | Service 钉死一条 EP。编排可 dynamic EP。没人 on-call：第一级跳到下一级有人的；整条都空则事件丢弃 | **从上到下，命中即停**。建议底部留无过滤兜底。**无策略 = 接入但不通知** |
| 第一下能不能拖 | 可以。`wait_seconds` 在 personnel 级和 channel step 上都有 | **不能。** 第一下 page 不能在 EP 上延迟。Delay 只活在个人规则 | 可以。Delay window 0–3600 秒，挂在**分派策略**上；窗内自动恢复或被关则不通知 |

出处：`alert-system.md` §5.2、§6；`pagerduty-analysis.md` §3.3–3.4、§5.2；`flashduty-analysis.md` §6。

### 4.2 值班模型成熟度

| 能力 | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 排班表达 | 物化 `starts_at` / `ends_at` 行，**不是** RRULE。`GenerateRotation` 纯函数，cap 1000 shifts | 现行 shift-based（named rotations，Hourly/Daily/Weekly/Monthly；Summer 2026 推向全账户）+ 遗留 layers。Flexible Shifts = EA | 多层规则（白班 / 夜班叠加）；周期小时 / 天 / 周 / 月，1–30 |
| Override | **没有**一等 override 窗口 | Custom shifts vs Overrides；回退 layer 不回退 override | Override **完全抢占**；另有日期掩码 / 连续轮换 / 公平轮换 |
| 换班市场 | 无 | Shift Agent（Slack，PTO 冲突 / 要人顶班，0 AI Actions） | 材料未写 marketplace；有换班通知（提前 N 分钟或每天定时） |
| 导出 | 无 | WebCal / `.ics` | API 预览窗口最多 45 天 |
| 空班行为 | 时间线警告 `nobody_on_call` | 事件丢弃（整条 EP 都空时） | 单聊不发、群聊仍发 |
| 注释 | 表注释「一 team 一 schedule」；resolver 允许最多 20 | Free：1 schedule。本租户 0 条，创建表单 Pass 1 没打开 | 账户内名称唯一，1–40 字符 |

**意见：** 三家都允许「没有排班表也永远 on-call」。SRE Control 已经选了可审计的洞（`nobody_on_call`），**不要为了像 PD 改成丢事件**。比抄 Critical Alerts 更早该做的产品决策是：**一等 override**——休假现在没有正规出口。

### 4.3 升级策略细节

| | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 级数 / 重复 | levels JSON：`wait_seconds` + team 目标。`severity_config` 可跳级、可覆盖等待；空对象保持旧行为。耗尽写 `chain_exhausted_or_unconfigured` | 每 EP 最多 20 规则；repeat 最多 9。最小 timeout 1 分钟（单目标）/ 3 分钟（多目标）；默认 30 分钟。**第一级立刻** | 可增删 / 插序。超时最少 1 分钟；默认首环节 30 分钟、不重复。重复间隔最小 0.5 分钟。条件：Open（未 Closed）或 Unacknowledged（无人认领） |
| 停升级 | Ack、Resolve、或 silence（personnel 与 channel 同一谓词） | Ack 停升级和继续通知。Snooze **不停**「EP 作为 responder」的升级 | 认领 / 关闭（视升级条件是 Open 还是 Unacknowledged） |
| 快照 | 材料未写「热改策略是否影响已开火的 timer」 | **触发时 EP 快照**；事后改 EP 不影响，除非把 Incident 重新指派到同一条 EP | 材料未写对等快照句 |
| 动态改对象 | mapping / `sre.*` 标签驱动 Fault，不驱动值班对象 | Event Orchestration：dynamic EP | 标签约定：`layer_person_reset_N_emails` / append；`reset` 优先。**不是**抽屉里的另一组表单 |

**意见：** 若以后允许热改升级策略，必须显式选快照还是跟着改——PD 选了快照。SRE Control 的 timer + JSON levels 现在可以假装「改了就影响下一发」，但 Incident / Fault 一旦开火，运营会问「这条还走旧梯子吗」。先写进 CLAUDE，再动模型。

---


## 5. 通知媒介与 ACK 路径

### 5.1 渠道矩阵

| Kind / 形态 | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 飞书卡片 | **一等**。`lark`：OpenAPI 交互卡片，单告警有认领 / 解决按钮；权限失败降级为文本 + 链接 | 无（西方默认不是飞书） | IM 应用一等；飞书默认模板含「故障详情」「认领」`ack=1` |
| 飞书群机器人 | `lark_webhook`：自定义 bot 文本，18KiB 截断，**无 Ack** | 无 | IM 机器人；群聊 Webhook **域名不校验**，可填自研网关 |
| Email | `net/smtp` PLAIN，可选 STARTTLS。无 Ack。host 空则发送失败、启动不炸 | 一等。本 EU 租户出厂规则：高低 urgency 都是立刻 html email | 一等。发信 `no-reply@notice.flashcat.cloud`。Usage 图三条之一 |
| Webhook 出站 | `webhook`：HTTP POST JSON Message，无 Ack | Generic Webhooks v3（账户 / 服务级，每 scope 10 订阅）+ Extensions | 故障 / 告警 Webhook + Jira / SN / SDP + Custom Action。2 秒超时，最多重试 1 次 |
| 电话 / 语音 | **registry 占位**；政策引用则 `Validate` 拒绝。无 IVR、无按键 ACK、无会议桥 | **一等**。2 分 30 秒切断。Emergency Bypass vCard。日语 / 加拿大法语语音 GA | **一等**。公开中国主叫（010/021/0571 等）+ 境外 `16465861127`。按键 ACK。私有化可 `disablePhone` |
| 短信 | **无 channel 文件** | 一等 | 一等。Usage 图三条之一 |
| Push / App | 无。Web SPA only | 一等。声称 99.99% 可靠、启用时「6× faster MTTA」。iOS Critical Alerts | Flashduty App；仅故障分派场景。iOS Critical Alerts |
| Slack / Teams | 无 | Extensions 卡一等；Shift Agent 门控 Slack 联系方式 | IM Tab 五家：钉钉 / 飞书 / Slack / Teams / 企微。文档另有 Telegram / Zoom 机器人 |
| 钉钉 / 企微 | 无 | 无（材料未列为默认） | 目录一等 |
| 统一网关 | CHANGELOG HOLD：等真正飞书 + `lark_open_id` | 人身上的规则就是网关 | 个人偏好 × 策略矩阵；IM 失败可自动短信 / 邮件补提醒 |

出处：`alert-system.md` §5、§11.2；`pagerduty-analysis.md` §5；`flashduty-analysis.md` §7。

### 5.2 「怎么叫」的交接点

| 设计点 | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 政策挂在哪 | **团队级** `notification_policies`：每严重程度 steps（`wait_seconds` / channels / mode）。mode `spread` 并上一级渠道，其他 = replace | **人身上**，按 High / Low urgency 两套 + delay（建议 push 0 / phone 1 / Slack 2 / email 3 / SMS 4） | **Channel 内规则**：Follow personal **或** Follow default。统一设置 = Critical / Warning / Info × Voice / SMS / Email 矩阵 + Group chat |
| 个人规则 | **没有** | 六段：high assigned / high changes / low / invited / OCHON / Shift Agent。本租户实况：high 立刻 email、low 一条 email、换班立刻 + 提前 1 天 | 四场景：故障分派（含语音 / App）/ 协作与跟进（无 App 无语音）/ 值班轮换 / 内部状态页（仅 IM） |
| 删规则的含义 | 不适用 | **≠ 取消指派**。休假走 override | 选统一矩阵则忽略偏好 |
| 团队群目标 | **坏的**：`NotifyGroup` / Repeat / channel-escalation 的 Lark 目标是 `alert.TeamID`（UUID），不是 `chat_id`。没有 team channel contact 表 | Service 摘要条 Communication Channel；工作流动作拉 Slack / Zoom / Meet / Teams | 分派抽屉 Group chat 目标；作战室专业版，同时只允许一个 IM 集成开 |

**意见：** SRE Control 现在不是「渠道少」，是「团队触达身份不存在」。飞书卡片能 ACK 单告警，群卡片故意没按钮（哪条告警会歧义）——这是对的。但 group / repeat / channel-escalation 可能根本打不到飞书群。在补语音 / Slack 之前，先决定：**团队是否有一等联系人（chat_id / webhook / email alias）**。没有这个对象，Channel-as-service 和矩阵都是空中楼阁。

### 5.3 ACK 路径

| 入口 | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| Web | REST Ack / Resolve。重复 Ack / Resolve 返回 200 | 列表批量条 + 详情。Mobile 文档能力面很宽（本次未核 App） | 认领 / 取消认领 / 关闭 / 升级 / snooze / wake |
| IM 卡片 | 飞书：`POST /callbacks/lark/card`，session-exempt，HMAC-SHA1。actor OpenID 只进时间线，`acked_by` UUID **保持 NULL**（无飞书→用户映射） | Slack / Teams 按钮 / 命令。Advance Assistant **不再直接**做 Incident 动作 | IM 应用卡片：认领 / 关闭 / 升级 |
| 深链 | 卡片 value 带 `action` + `alert_id` | 材料未写 `?ack=1` 对等句 | 机器人 `{{.DetailUrl}}?ack=1`，零权限闭环 |
| 语音按键 | 无 | 有 | 有 |
| Open API | 同源 REST（走同一 domain.Ack） | Events API `acknowledge` + REST（账户 key 改 Incident 要 `From:`） | `/incident/ack` `/unack` `/resolve` `/reopen` `/snooze` `/wake` |
| 谁能 ACK | 能打到 REST / 卡片的人。卡片写不进 `acked_by` | 任何人 ack → Incident **改派给他**；原 assignee 不收状态通知 | **看见就能认领**，不限已分派人。空间是可见性边界，分派是触达边界 |

**意见：** 「看见即可」是协作空间模型的推论，SRE Control 还没到——不是因为哲学不同，是因为身份映射不存在。P0 产品决策是：飞书 OpenID 是否必须绑到本地 user，才允许卡片 Ack 成为法律意义上的认领。重复 Ack 返回 200 已经对了。

---

## 6. 降噪（聚合 / 静默 / 抑制 / 编排）

### 6.1 管道位置

```
SRE Control（非 AM）
  ingest hook → dedup → mapping → route → group → silence → inhibit
SRE Control（AM 权威源）
  ingest / reconcile → 只跑 RunForAuthoritativeSource() 的阶段（dedup + mapping）
  本地 silence / inhibit / route / group 全部跳过

PagerDuty
  Events / email
    → Event Orchestration（Global → Routing → Service；可 enrich / suppress / pause / drop）
    → Alert
    → AIOps grouping（多 Alert → 一 Incident）
    → Incident Workflows（人速：聊天 / 状态页 / 工单）

Flashduty
  Event API / 专属或共享集成
    → 两层接入去重
    → Pipeline / 标签增强
    → 排除 Drop（丢，无记录）
    → 事件聚合（alert_key → Alert）
    → 告警聚合（可选；规则 / 智能 → Incident）
    → 抖动 / 静默 / 抑制（现行语义：拦故障生成，可选留痕）
    → 分派
```

出处：`alert-system.md` §1、CLAUDE inv. 1 / 10；`pagerduty-analysis.md` §6；`flashduty-analysis.md` §5。

### 6.2 能力对照（只写材料有的）

| 层 | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 接入幂等 | AM：`source_id` + `upstream_fingerprint` + `starts_at`。hook UUID 一次一密，存 `hex(sha256)` | `routing_key` + `dedup_key`。省略 key → PD 生成 UUID → 永远新 Alert。超 100 次 trigger 停收 | 完全相同 event 丢；状态 / 标题 / 描述与上一条一致则丢并更新属性 |
| 事件 / Alert 聚合 | 依赖 **AM grouping**。本地 group 是兼容路径；空 `group_by` = singleton。另有 template group / `storm_summary` | 同 dedup_key 并进原 Alert。再往上是 AIOps grouping（Intelligent / Content / Unified / Time / Global） | 事件聚合默认开，窗口默认 24h（1–1440 分）。告警聚合**默认关** |
| 抖动 | `flap` 表 + 时间线 `flapping_start` / `end`（000009） | Auto-pause transient 2–15 分（AIOps） | `max_changes` 默认 4（2–100），窗 60 分，静默 120 分。模式：关 / 仅提醒 / 提醒后静默。新建默认仅提醒 |
| 静默 | 本地 `silences` 表；**AM 源跳过**。停升级谓词含 silence | Orchestration suppress / pause（Incident 前）。Snooze 是 Incident 后、只对已 ack | 单次或周期（星期 / 服务日历）。现行：拦故障生成，丢弃或保留打标。快速静默从故障详情生成 |
| 抑制 | 本地 inhibit 阶段；**AM 源跳过** | 编排 Suppression | 新告警条件 + **10 分钟内**活跃告警 + 相同项。10 分钟窗口写死不可调 |
| 排除 / drop | 无独立 Drop 产品。禁用 hook → 401（不泄露存在） | Global Orchestration 可 **drop，不可审计** | Drop rules 挂在 Integrations 组，事件入口丢、任何列表都不出现 |
| 编排产品 | pipeline 固定六段 + hook registry（before/after stage，失败隔离）。无独立 Orchestration UI | Event Orchestration 是独立 mega-menu。每 orchestration 10 key、payload 4MB、最多 1000 服务 | 共享集成路由：名称映射、继续 / 停止、一条规则打多空间、预览最多 20 条。入口在集成上，不是独立产品名 |
| 噪声开关哲学 | **opt-in**。零或默认 = 关（CLAUDE inv. 10）。LLM 不在路径上 | grouping / AIOps 是付费开关；关则吞吐回 120 ev/min | 每 Channel 一套。默认：事件聚合开、告警聚合关、抖动仅提醒——先保证同 key 不刷屏，默认不把不同告警捏成一团 |

**意见：** SRE Control 和 Flashduty 的权力分配是反的。FD 让每个协作空间自己降噪；SRE Control 把 Alert 级降噪还给 AM，自己只做 mapping。为「功能对称」在 AM 源上再做一层 PD Intelligent Grouping 或 FD 静默 / 抑制，就是背叛论点。本地非 AM（Grafana / generic）的 silence / inhibit 是兼容路径，不要把它做成主 UX。

若借鉴聚合：**放在 Alert / Fault 层，不要自动升格成 Incident。** FD 自己都把告警聚合做成可选，KPI 在未开启时写明「Visible after alert grouping is enabled」。

Delay window（FD）和 flap（SRE Control 已有）是两层：抖动按变化次数按住；延迟窗是分派前最后 N 秒。叙事对齐「两个旋钮、两个阶段」，不必合成一个开关。

---

## 7. 控制台 IA 与交互（inbox vs 工作台 vs 协作空间）

### 7.1 壳：三种完全不同的家

| | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 壳 | 单一 React 19 + AntD 5 SPA。全局侧栏 = **SRE 主任务**；设置进控制中心（CLAUDE inv. 19） | 顶栏 **8 个 mega-menu**：Incidents / Services / People / Automation / AIOps / Analytics / Integrations / Status；另有 AI → `/advance`。平台壳，不是「一个列表加设置」 | **三级**：最左 ~50px 产品轨（AI SRE / On-call / RUM / Monitors）+ 第二列模块导航 + 内容。On-call 默认落地 `/channel` |
| 打开产品后的家 | **运行总览** `/` → 故障工作区 / 事故响应室。告警在 `/alerts`，标题「告警 / 告警证据」——inbox 不是家 | **`/incidents` 列表**。空态气球 "No news is good news!"。右栏塞满 Always on-call / ON CALL NOW / Slack 促销 / GETTING STARTED——教学型 inbox | **Channel 卡片网格**。Default 卡绿点 + `0 Triggered / 0 Processing`。空间是「被盯着的服务」，不是设置里的一行 |
| 人维过滤 | 材料未写 "Assigned to me" 作为主分段 | "Assigned to me" / "All"（人维） | "Assigned to me" / "My channel"（人维 + **空间维**） |
| 运行 vs 配置 | Run：总览 / 故障 / 事故 / 告警 / 服务。Response：静默。Admin：`/settings/*` 控制中心 | People 菜单把 EP / Schedule / User 放一起；Services 另走；Teams 走 `/teams` | 分派在 Channel Configuration；值班表在账户 Configuration；头像里才是 Org / Billing / SSO |

SRE Control 侧栏实勘（`web/src/App.tsx`，`alert-system.md` §8）：

| 组 | 路径 | 页 |
|---|---|---|
| 运行 | `/` | 运行总览 |
| 运行 | `/faults`、`/faults/:id` | 故障列表、**故障工作区** |
| 运行 | `/incidents`、`/incidents/:id` | 事故列表、**事故响应室** |
| 运行 | `/alerts`、`/alerts/:id` | 告警证据、详情 |
| 运行 | `/services`、`/infrastructure/:id` | 服务目录、基础设施详情 |
| 响应 | `/silences` | 静默策略 |
| 管理 | `/settings/*` | 控制中心（integrations / mappings / services / teams / users / schedules / escalation-policies / notification-policies / ops-settings / routes） |

出处：`alert-system.md` §8；`pagerduty-analysis.md` §2；`flashduty-analysis.md` §2。

### 7.2 交互表面

| 表面 | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 对象详情 | FaultWorkspace / IncidentRoom = **整页工作区 / 协作空间**，不是抽屉里的表 | Service 详情整页 + Tab（Activity / Integrations / Settings / Dependencies）。Incident 详情本次空租户未开 | Channel 详情 = 整页 + `?tab=`（`integration` / `notificationPolicy` / `baseSettings` / `advancedSettings`） |
| 嵌套配置 | 控制中心里的独立页 | 遗留整页 + SPA 路径段 | **右侧 ~60% 编号抽屉**，粘性 Save / Cancel（分派规则）。几乎不用 modal |
| 列表性格 | 工作台式（总览 / 工作区） | 检索型列表 + 批量条（Ack / Reassign / Resolve / Snooze），空态气球是检索空 | 检索型：先给筛选器再给 "No data"。首次功能用猫插画 + CTA。套餐空复用猫 + 盾锁 + Upgrade |
| 实时 | SSE `GET /api/v1/events` + 15s poll 回退。一连接挂在 AppLayout | 材料未写 websocket。遗留列表 URL query 可寻址 | Pass 1 **未观察到 websocket**；Channel 活计数更像轮询 |
| 空态分类 | 材料未写统一空态语言。有「未配 AM / 未配值班」这类首次态的位置，但没有套餐 | 检索空 / 首次空 / 套餐空（整页营销对开）分得清。过期试用上 New Incident CTA 裂开（无 href 的 `open-web-trigger-form`） | 检索空 / 首次空 / 套餐空。配额写在按钮旁（`0/1`、`0/20`、`oper-forbid`） |
| 教学 | 无每页文档深链（材料未列） | 右栏 INFO / GETTING STARTED；Support mega-menu 外链 KB | **每页标题旁文档深链** + 控件 helper（Owning team 的 MTTA 归属、Delay window 的窗内关闭不通知） |

**意见：** 三家的「家」就是产品论点的 UI 投影。

- PD = **inbox**：打开就是要我处理的 Incident。
- FD = **协作空间**：打开就是我的 Channel 活计数。
- SRE Control = **基础设施工作台**：打开就是总览 / 故障 / 事故。告警被降级为证据，这是对的，**加倍押注，不要改成 inbox**。

自托管单产品不必抄 8 mega-menu 或四条产品轨。要抄的是：「事故列表不是设置页」——已经对齐——以及空态三分法（检索空 / 未配 AM / 未配值班），用同一套插画，不要三种文案三种布局。琥珀横幅（FD Monitors「No installation detected」）比静默 404 诚实；PD 过期试用上坏掉的 New Incident 是反面教材。

PD 的可抄 UI 投影：告警表强调 Severity / Integration / Source，事故表强调人和 urgency。SRE Control 已经三层导航，列设计应对齐「证据 vs 人」，不要把 Fault 列做成第三张告警表。

---


## 8. 集成与 API 表面

### 8.1 入站

| | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 主入口 | **只有** `POST /ingest/v1/hook/{hook_uuid}`。UUID v4，hash 查 `hook_id_hash`，禁用 / 不存在一律 401。body ≤1MB，成功 202，全有或全无入队 | `POST https://events.pagerduty.com/v2/enqueue`。32 字符 `routing_key`。另：email ingest、REST 手建（不产 Alert） | `POST https://api.flashcat.cloud/event/push/alert/standard?integration_key=`。夜莺专用 `/event/push/alert/n9e` |
| 适配器 | `alertmanager`、`grafana`、`generic`。另：`alertmanager_api` reconcile（`/api/v2/alerts` + watermark）；`change-events` 独立 Bearer，与告警队列隔离 | Events API 是通用信封。控制台 Default Service Integrations 有 Nagios / Sumo / AWS / Dynatrace / Splunk / Datadog logo。Email 每 key 60/min | 文档 100+ 源；Pass 1 目录约 **55** 张卡，中国云厂商长尾重。专属集成直达 Channel；共享必须配路由，不配则拒收 |
| 标准信封 | generic：snake_case，object 或 array，`ResolvePolicy` 调用方必填 | CEF：`event_action` trigger/ack/resolve；必填 summary / source / severity（critical / error / warning / info） | 必填 `title_rule`（≤512）、`event_status`（Critical / Warning / Info / Ok） |
| 吞吐（文档数字，勿混用） | 材料未写 ev/min。`notify_send` 持锁打 HTTP——已接受的 MVP 吞吐上限 | 默认每 key **120/min**；AIOps 可申请 **10,000**。Email 60/min | 每集成 **100/s、1000/min**，超限 429。数组最多 100 条 |
| 没有的 | Datadog、CloudWatch、Zabbix、PD Events API v2、email inbound、SNS、Grafana OnCall、Flashduty pull | 不自带 Ruler | SaaS 控制台不替你跑 Monitors 引擎——必须先装 Alert engine |

出处：`alert-system.md` §7；`pagerduty-analysis.md` §7；`flashduty-analysis.md` §8。

### 8.2 出站 / 管理面 / IaC

| 面 | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 出站 | Lark callback；notify webhook；Incident 通讯走同一 `notify_send` outbox（`incident_outbox.go`）。API 打码原始 target（CLAUDE 13） | Webhooks v3 事件面很宽（triggered / acked / resolved / reopened / workflow.* …）。OAuth2 webhook H2 2025 GA | 故障 Webhook 缩写枚举（`i_new` `i_ack` `i_rslv` …）；告警 Webhook 标准版+。出口 IP 文档写四个；2s 超时 |
| REST 管理 | 自有 `/api/v1/*` + `/ingest`。Vite 代理这两条 | `api.pagerduty.com` / `api.eu.pagerduty.com`。Token 或 Bearer。SCIM v2 GA H2 2025 | **仅** `api.flashcat.cloud`；多数 POST；query `app_key=`。文档 337 操作 / 39 服务。控制台同源 `/api/…` 是另一条 |
| Terraform / CLI / MCP | 材料未列官方 provider / MCP | 官方 Terraform；`pd`（go-pagerduty REST）+ `pagerduty`（python Events）；MCP 72 tools / 17 domains | Terraform 12 资源（channel / escalate_rule / schedule / silence / inhibit / route / template / …）；CLI；Go SDK；MCP 23 tools / 8 模块 |
| Change events | 已有独立表 + ingest，与告警队列隔离 | 不建 Alert / Incident / 通知；90 天；关联过去 24h 最多展 3 条 | `/settings/source/change` 仅 Standard Change Event（本租户） |

**意见：** 集成广度不是 v0.1 的战场。PD 自己都不做 Ruler；FD 的 55 张卡是中国市场目录，不是 SRE Control 的作业。若加「标准信封」，做成 **Events 路径**（机器摄入），不要让 REST 声明事故走同一条 hook——PD 这条拆法是对的，SRE Control 的 Incident 本来就不该进 ingest。Change events 已经隔离，保持。

值得做的外部表面是 **稳定的 ingest 信封 + 声明式配置（Terraform / 文件）**，不是先铺 72 个 MCP 工具。引擎就是产品（自托管），和 PD / FD「引擎闭源、客户端开源」的边界相反。

---

## 9. AI 能力

| | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 立场 | CLAUDE inv. 9：**AI/LLM 留给异步 enrichment，路径上未实现**。`annotations` 表在，无 pipeline 调用。Embedding / Event Intelligence **永久排除**（aiops-foundation changelog） | **两个 SKU**。不拿客户数据训练。输出必须人核 | 控制台最左轨就有 AI SRE。文档：公测、不单独收费。控制台黄条：private beta for Pro + free trial |
| 降噪 / RCA | 确定性 mapping + flap / storm。禁止 LLM 在路径上 | **AIOps**（事件价，2023-04 接替 EI）：Intelligent grouping、Auto-pause、Probable Origin / Related / Outlier / Past / Recent Changes、Global Orchestration、Operations Console | 智能告警聚合（ML 语义；默认 title / description / labels.service / labels.resource，最多 10 字段；**材料没给阈值**）。新奇故障、历史相似（专业版） |
| 聊天 / Agent | 无 | **Advance**（AI Actions）：Assistant、SRE Agent（虚拟响应者，4 actions / 次）、Scribe、Shift（0）、Insights（0）。SRE Agent 在 Operations Console 要 AIOps+Advance | chat-first：`/` 命令、`@` 引用 incident、附件、scope、模型 Auto。二级：Automations / Artifacts / Plugins / Knowledges / Environments。作战室创建后可自动跑一轮诊断回贴 |
| 本租户能看见 | 无 UI | `/advance`、Noise Reduction、Automation Actions = 整页门控。Account Settings：**没资格**再开 Advance / AIOps 试用 | `/ai-sre/chat` 能进问候页；Automations 等子页 Pass 1 未打开。每页右上角 AI SRE pill |
| 和 On-call 的接法 | 无。enrich 包存在、未接线 | SRE Agent 可被工作流 / EP 触发（EA）。Automation Actions = Rundeck 产品化接点 | `@` incident；故障触发自动化；MCP / A2A / Sandbox / BYOC Runner |

出处：`alert-system.md` §3、CLAUDE inv. 9；`pagerduty-analysis.md` §8；`flashduty-analysis.md` §10.1。

**意见：** 抄边界，不抄能力。

- 该抄的：降噪 / 相关变更 = 确定性（或以后单独计价）；聊天 / 摘要 / agent = 另一层、可关、按对象 / 按 Team；不训练客户数据；输出要核。
- 不该抄：SRE Agent、AI Actions 计价表、「声称节省 20 分钟」、在 AM 源上做 Intelligent Grouping、把 embedding 从永久排除名单里捞回来。
- SRE Control 的异步 enrichment 位置是对的。在 mapping 治理队列（人还在看 unmapped）上做建议，比在 notify 路径上生成摘要更贴论点。

---

## 10. 实现架构（SaaS vs 自建、ID、SPA、队列）

| 层 | SRE Control | PagerDuty | Flashduty |
|---|---|---|---|
| 交付 | **自托管**。Buildx 三镜像 api / web / migrate；k8s Web+API 同 Pod。本地 PG :5433、API :8080、Vite :5173 | **闭源 SaaS**。本租户 `*.eu.pagerduty.com`；REST `api.eu.pagerduty.com`；Events 材料写 `events.pagerduty.com`（未摘到 `events.eu`） | **闭源 SaaS** On-call 引擎 + 开源夜莺 / Categraf 做生成。SLA（2024-04-28）：核心 99.9%；99.9% 告警 5 分钟内投递。不含公测 / 免费 / 私有化 |
| 后端 | Go 1.25、gin v1.12、pgx v5.7、PG 16 | 未开源。归档的 Scala scheduler / Ruby blender **不是**产品值班器 | 未开源。从 API / SDK / html/template 推断偏 Go。企业版存储 VM + Doris ≠ SaaS 证明 |
| 队列 | **无外部 MQ**。PG `timers` + `alert_events` `SKIP LOCKED`。11 种 timer（含 notify_send / personnel_escalation / channel_escalation / 三条 ops cron）。handler 禁止自开事务 | 不可见。OSS scheduler 是内部任务调度（Kafka + Cassandra），已归档 | 不可见。上报后异步、出错重试（SLA 文案） |
| 通知出箱 | intent 接口只 Schedule `notify_send`。幂等键含 round 时间戳。重试 3 次、200ms–5s full jitter。at-least-once。**handler 持 timer 行锁打 HTTP**（已接受 MVP） | 人规则 + 电话网 + webhook IP 列表 | 多渠道冗余；IM 失败可降级短信 / 邮件 |
| 前端 | **单一** React 19 + AntD 5.29.3 + RQ5 + RRD7。侧栏折叠持久化；≤900px 自动收 | **两代并存**：遗留 Rails 整页刷新（`/incidents`、`/escalation_policies`，jQuery `open-web-trigger-form`）+ React SPA 先 spinner 再 hydrate（`/service-directory`、`/teams`、`/analytics/*`） | **单一** React SPA + Tailwind + dark mode。路由切内容不全页刷新。具体 React 大版本 / 组件库名称材料没写，不要写成 AntD |
| ID | Incident：`INC-` + 6 位数字。其余多为 UUID。alerts.team_id / acked_by **仍缺 FK** | 经典 **7 字符大写**（User `P7DCOZA`、Service `PATY3M1`、EP `PYG7510`）。Analytics 另用 base64 风格。短号进 URL 路径 | 组织 / 空间：**十进制长整型**（Org `2618591231671`、Channel `2618595391671`）。故障文档示例：Mongo 风格 ObjectID + 6 位 `num`。两套并存 |
| 鉴权 | 无自助注册；`cmd/seed-admin` 写 bcrypt。sessions 表。SSO / OIDC = users 表注释「未来」 | 账户级 / 用户级 API key；邮箱域限制（本租户 allow list `cvte.com`）。SCIM v2 GA H2 2025 | 个人 APP Key 每账号最多 5。头像三栏：Personal / Org / Switch Identity。RBAC + 按团队的数据权限 |
| 实时 | SSE + 15s poll | 材料未写 | Pass 1 无 websocket |
| 遥测 | 材料未写 RUM | Pass 1 **没有** Datadog / 自研 RUM 记录 | 控制台用 **Datadog** RUM + Session Replay（`service: fd-console-prd`，`version: 202608202010`）。自己卖 RUM，自己的控制台先用竞品盯生产 |
| OSS 边界 | 私有仓 `iluoxw/alert-system`；引擎就是产品 | 核心闭源。开：go-pagerduty、python-pagerduty、Terraform、MCP、response.pagerduty.com、Rundeck（2020-10 收购） | 核心闭源。开：夜莺、Categraf、Ibex、CLI / SDK / MCP / Terraform / sprig fork。**没有**名为 flashduty 的后端仓 |
| 多租户 | 单部署；RBAC = 任一 team 的 admin | 账户 / Team / 计划门控。Private Team 不作用于 Workflows / PIR / Analytics | 多主体完全隔离；空间可私有。本租户只有一个身份 |

出处：`alert-system.md` §1、§9、§11；`pagerduty-analysis.md` §9–10；`flashduty-analysis.md` §8.4、§9。

**意见：**

- 单一 SPA + PG timer 是优势，不是寒酸。PD 的 Rails 缝在过期试用上裂开（New Incident 打不开、`/incidents/new` 闪 Not Found）——15 年产品的伤疤，不是目标架构。
- 短号给人念（`INC-000123`），长 ID 给 API。PD 7 字符、FD `num`、SRE Control `INC-` 是同一选择。**保持短号稳定**，不要在以后迁 API 时换一套。
- 无外部 MQ 是论点的一部分：权威在 AM 和 PG，不在 Kafka。`notify_send` 持锁是吞吐上限，不是正确性 bug；产品决策是「接受 at-least-once + 低 QPS」，直到团队触达对象先做对。
- 不要抄 FD 用 Datadog 收自己的控制台——那是 SaaS 实现选择。

---


## 11. SRE Control 相对领先的地方（不要谦虚）

对照的是 v0.1.0 已经在 schema / domain / UI 里落地的东西，不是路线图。CHANGELOG 叙事停在 2026-08-02，但 000012–000022 和代码已经把下面这些做出来了（`alert-system.md` §3）。

1. **Fault ≠ Alert ≠ Incident。** PD 把 Alert 默认塌缩成 Incident；FD 仍是告警中心、聚合可选地自动出故障。SRE Control 是三家唯一把「资源坏了」做成一等对象、并用人声明把事故从故障里提纯出来的产品。领域链 `Signal → Alert → Fault → Incident → Response → Recovery → Learning` 不是文案，是表和状态机（000012 / 000013 / 000017 / 000018）。
2. **Impact 是人可否决的状态，不是图遍历的副作用。** potential / confirmed / ruled_out；blast-radius 反向走、深 8，**图可达明确不是 Impact**（CLAUDE 16）。PD 的 Impacted Services 来自分组 / 合并，不能手改。这一刀让「误伤半径」可以被 SRE 纠正，而不是被 ML 写死。
3. **AM 是 Alert 权威，产品拒绝双管。** 不双通知、不再 inhibit、不在 AM 源上跑本地 route / group / silence。三轴镜像 + reconcile + occurrence 幂等，是「下属」而不是「克隆」。PD / FD 都必须在自己的管道里重做 Alert 生命周期，因为它们没有一个已经正确的 AM。
4. **Incident 不能 PATCH 到 resolved。** 双关闭门：缓解准则 + 解决准则 + 任务收口 + 无 included+active Fault。PD 的 resolve 是所有权动作；FD 的 Closed 可以是人止噪。SRE Control 把「宣称结束」做成恢复门，这是事故产品，不是告警箱。
5. **Guarded runbooks 是真的门，不是链接。** dry-run SHA256、审批人 ≠ 执行人、生产默认不执行除非接上支持的 API、UAT-only `lifecycle_verify`。PD 的 documentation link 是 URL；Automation Actions 对本试用租户整页 Contact Sales。Rundeck 是收购来的另一条产品。SRE Control 把「能跑的动作」限制在 allow-list 类型上（CLAUDE 12），比「自定义 action 目录」更安全，也更像基础设施 OS。
6. **家是工作台，不是 inbox。** 运行总览 / 故障工作区 / 事故响应室是一级导航；告警显式叫「证据」。PD 打开就是 Incident 表；FD 打开就是 Channel 卡。论点已经写在 IA 里，不要退回去。
7. **映射治理队列 + 三层实体图。** 未映射不静默丢，进 `AlertMappingUnmapped`。自动 Fault 只走政策或可信 `sre.*` 标签。这比 PD「没人 on-call 就丢、不可审计」和 FD「排除无记录」更可运营。边 origin 分 observed / declared（000020），人声明的依赖和观测到的依赖不是一种东西。
8. **没人 on-call 留下洞并记警告。** 三家都能「没有排班表」。只有 SRE Control 把覆盖缺口写成时间线事件而不是丢包或纯安静。选这条，并写进 CLAUDE，不要默默变成第四种。
9. **Change events 一开始就隔离。** 与告警队列分开、独立 token。PD 也是这么做的（不建 Alert / Incident / 通知）；SRE Control 没有把变更当告警喂进 pipeline。保持。
10. **确定性降噪，LLM 不在热路径。** flap / storm / template group / retention 是表和 timer，不是「Event Intelligence」SKU。PD 把 grouping 做成按事件计价的 AIOps；SRE Control 把它做成 opt-in 且默认关。这是正确的自负。
11. **自托管 + 单一 SPA + PG 就是控制面。** 没有套餐把门控钉在论点功能上。Fault / Impact / 双关闭门不会在「Start a Trial」整页后面。PD / FD 的试用租户把 AIOps / 复盘 / 状态页收成营销页——那些不是核心，但也说明 SaaS 会把能力劈成 SKU。SRE Control 的核心没有被自己的商业模式阉割。
12. **E2B 黄金路径是垂直切片，不是 demo 脚本。** 专用恢复准则 + 6 个 allow-list action key + 事故响应室。风险是种子太特化；优势是证明「Fault → Incident → 有门的 runbook」这条链能在一个真实基础设施上闭环。PD / FD 材料里没有对等的、绑在具体基础设施上的垂直闭环。

---

## 12. SRE Control 明确缺口（P0 / P1 / P2）

只列**产品决策**，不写代码任务、不写「补一张表」。每条都是「选 A 还是 B」，以及为什么现在必须选。实现裂缝（TeamID 当 chat_id、卡片写不进 `acked_by`）被收成决策的约束，不是工单。

### P0 — 不选就会让现有论点在生产里说不清

| # | 决策 | 选项 | 建议 | 依据 |
|---|---|---|---|---|
| P0-1 | **团队触达身份是不是一等对象** | A. 做 team contact（chat_id / webhook / email alias），让 group / repeat / channel-escalation 有真实目标。B. 放弃团队群，只打人（email / `lark_open_id`） | **选 A**。没有它，双升级带的 channel 一侧是空转；工作台「通知了团队」是假完成 | `alert-system.md` §5.3、§11.7 |
| P0-2 | **卡片 ACK 算不算法律认领** | A. 飞书 OpenID 必须映射到本地 user，才能写 `acked_by`。B. 承认卡片 ACK 只是时间线事件，认领以 Web 为准。C. 学 FD「看见即可」，不要求本地 user | **先选 A 或 B，不要默认为 C**。C 需要协作空间可见性模型，SRE Control 还没有 Channel。现在的实现是「看起来像 A、落成谁都不是」 | `alert-system.md` §5.4；`flashduty-analysis.md` §7.5 |
| P0-3 | **要不要引入「管道」一等对象** | A. 引入 Space / Service Room（**不要叫 Channel**），拥有集成 + 活计数 + 分派引用。B. 继续用 Team + notification_policies + 服务目录，管道不做成对象 | **v0.1 选 B，但把 P0-1 做进 Team。** 先补触达身份，再决定空间是否值得升格。FD 的 Channel 是整张产品的脊柱；SRE Control 的脊柱是 Fault，不要为对等换脊柱 | `alert-system.md` §11.1；`flashduty-analysis.md` §12.1、§12.5 |
| P0-4 | **没人 on-call 的行为写死哪一种** | A. 维持 `nobody_on_call` 警告、事件留下（现状）。B. 学 PD 丢且不可审计。C. 学 FD 接入不通知、群聊仍发 | **写死 A。** 三选一进 CLAUDE，禁止默成第四种。群聊仍发可以是 A 的附加，前提是 P0-1 存在 | `alert-system.md` §6.1；`pagerduty-analysis.md` §12.2 #10 |
| P0-5 | **值班缺口的正规出口：override 还是「改物化 shift」** | A. 一等 override 窗口（临时换人，不改底表）。B. 只允许重生成 rotation / 手改 shift 行 | **选 A。** PD 明确：删通知规则 ≠ 取消指派，休假走 override。没有 override，排班产品是假的 | `alert-system.md` §6.1；`pagerduty-analysis.md` §5.1、§12.3 #13 |

### P1 — 决定产品形状，但不阻挡 Fault / Incident 主路径

| # | 决策 | 选项 | 建议 | 依据 |
|---|---|---|---|---|
| P1-1 | **「怎么叫」要不要从 severity 拆出来** | A. 引入 urgency 或 support-hours（班后低打扰），severity 留在 Alert。B. 继续只用 critical / warning / info 驱动渠道 step | **倾向 A，但做成策略旋钮，不要引入 PD 的 Priority / SEV-1 第三套。** PD 自己的 response.pagerduty.com SEV 都刻意和产品字段分开 | `pagerduty-analysis.md` §4.2、§12.2 #6 |
| P1-2 | **个人通知规则做不做** | A. 先做人身上的 delay 链（飞书 0 / email N）。B. 只做团队矩阵（现状）。C. 两套一起上 | **选 A 或 B，禁止 C。** 若做 A，交接点学 FD「跟随个人 / 跟随统一」，不要学 PD 六段规则一次做完 | `alert-system.md` §11.4；`flashduty-analysis.md` §12.2 #8 |
| P1-3 | **Ack timeout / Snooze 做不做、和 silence 是否分开** | A. ack timeout 作策略级选项（默认关）；snooze 只对已 ack，且 ≠ silence。B. 不做，Ack 停升级即足够 | **选 A 的 ack timeout（默认关）。Snooze 可晚于 override。** 禁止和 AM silence 合成一个开关——一个在人后，一个在 AM | `pagerduty-analysis.md` §4.3、§12.2 #7–8 |
| P1-4 | **电话 / 短信：永不做，还是占位露在政策 UI 里** | A. 政策 UI **不露出** phone；Validate 拒绝保持。B. 做中国语音 / 短信（FD 路径）。C. 继续占位并在 UI 里显示 | **选 A，直到有明确的电话产品。** Bypass DND 是电话产品的核心，不是设置页一个勾。Email / 飞书不足以「永不 miss」时再开 B | `alert-system.md` §11.2；`pagerduty-analysis.md` §12.3 #14 |
| P1-5 | **升级策略热改：快照还是跟随** | A. 开火时快照（PD）。B. timer 永远读最新 JSON | **选 A。** 开着的 Fault / Incident 不该在有人改梯子时换人 | `pagerduty-analysis.md` §3.3、§12.1 #5 |
| P1-6 | **标准摄入信封 vs 继续三条 adapter** | A. 加 Events 风格信封（机器 trigger / resolve），REST 声明事故走另一条。B. 维持 AM / Grafana / generic 三条 | **要加就选 A，且 Incident 声明不准走 hook。** 不要为对等去接 55 个云源 | `alert-system.md` §7；`pagerduty-analysis.md` §12.4 #18 |
| P1-7 | **本地非 AM 的 silence / inhibit 是主 UX 还是兼容** | A. 主 UX 只服务 Grafana / generic；AM 源继续跳过。B. 为对称在 AM 源上再做一层 | **写死 A。** B 背叛 AM 权威 | `alert-system.md` §1；`flashduty-analysis.md` §12.5 |
| P1-8 | **Stakeholder / 状态页做不做** | A. 内部通报走现有 incident outbox + 单独模板，角色继续用 comms_lead / liaison。B. 做公开状态页 / 订阅者 | **选 A。** 不必为对等补 Status page。公开订阅是另一条产品 | `alert-system.md` §11.3；`pagerduty-analysis.md` §12.3 #15 |

### P2 — 承认是缺口，但不是现在的产品主线

| # | 决策 | 建议 |
|---|---|---|
| P2-1 | Learning / 复盘产品 | 领域链有名字、无包无表。保持「事故关上门之前不做复盘产品」。不要抄 FD Post Incident 的 Pro 门控形态 |
| P2-2 | Analytics（MTTA / MTTR 看板） | 现有：指纹 30d 统计、ops daily report timer。不要先做 PD 式 Insights SKU。若做，KPI 挂 Fault / Incident，不要挂 Alert inbox |
| P2-3 | SSO / OIDC / SCIM | users 表已留注释。自托管会要，但不改变论点 |
| P2-4 | Mobile / Push | Web SPA only。没有电话产品就不要做「Never Miss a Page」叙事 |
| P2-5 | 值班多层 / iCal / 公平轮换 / 换班市场 | override（P0-5）之后再谈。物化 shift 够用到第一个真实轮转痛点 |
| P2-6 | 异步 LLM enrichment 何时接线 | 位置已经对：`annotations` + 不在热路径。优先挂 mapping 治理队列的建议，而不是通知摘要。Embedding 保持永久排除 |
| P2-7 | 合并语义：人跟不跟 | PD Merge 不合并 responders / subscribers。SRE Control 已有 primary / supporting、included / excluded（000017）。若做 Fault / Incident 合并，先写死「人跟不跟」，不要默默带上 |
| P2-8 | 钉钉 / 企微 / Slack | 飞书卡片已经是可 ACK 的一等通道。再加 IM 是市场决策，不是论点决策。加之前先有身份映射（P0-2） |

---

## 13. 建议：该抄 PD 什么、该抄 Flashduty 什么、不该抄什么

### 13.1 该抄 PagerDuty 的

这些是文档和控制台同时钉死、且不和「AM 权威 / Fault ≠ Incident」冲突的设计。

| # | 抄什么 | 落到 SRE Control 哪一句 | 不要连皮带骨一起搬 |
|---|---|---|---|
| 1 | **把「谁拥有事件」和「谁被叫起来」拆开** | Service 拥有 Incident + key；EP 可复用；Schedule 可复用、自己不 page；通知规则在人身上。SRE Control 最接近的是 EP（team 目标）+ notification_policies + shifts。先问「路由单元要不要做成一等」，不要先抄 7 字符 ID | 不要把 Service 做成「又一个 Alert 桶」 |
| 2 | **手建 Incident 不走告警管道** | 与人声明 + 双关闭门同方向。REST / Web 声明不产 Alert，编排 / grouping 不适用 | 不要因此允许 Alert 默认自动出 Incident |
| 3 | **Alert 表和 Incident 表列不同** | 告警表强调证据来源（Severity / Integration / Source）；事故表强调人和打扰度。Fault 表强调实体 / Impact / 证据角色——第三张表，不是第三张告警表 | 不要把 Fault 压回 Alert |
| 4 | **Urgency ≠ Severity ≠ Priority** | 班后别打电话用 urgency / support-hours，别复用 severity。Priority / SEV-1 不必引入 | 不要把 response.pagerduty.com 的 SEV-1…5 当成产品字段 |
| 5 | **Ack 停升级 ≠ resolve；ack timeout 默认关** | 已有 Ack 停双升级带。可加策略级 timeout，把「认领了但没修」拉回未 ack | 不要默认开 timeout |
| 6 | **Snooze ≠ silence ≠ orchestration pause** | 人后 / 机器前 / AM 权威，三层分开。若做 snooze，只对已 ack | 不要和 `/silences` 合成一个开关 |
| 7 | **EP 快照** | 开火后的 personnel_escalation 用触发时的 levels。事后改政策不改这条，除非重指派 | 不要让 timer 默默读最新 JSON 还不声明 |
| 8 | **Events 路径和 REST 管理面分开** | 高流量走 hook / 信封；声明事故走 REST。Change events 继续隔离 | 不要让声明事故走 `/ingest/v1/hook` |
| 9 | **AI 两个 SKU 的边界** | 降噪 = 确定性；聊天 / 摘要 = 另一层、可关。不训练客户数据，输出要核 | 不要上 SRE Agent，不要抄 AI Actions 价目表 |
| 10 | **最小可运行图不需要 Schedule；但没人 on-call 必须产品化** | 已经可以没有排班表。覆盖缺口选留下并警告，**不要抄「丢且不可审计」** | 空班行为是产品，不是实现细节 |
| 11 | **URL 从第一天当 API** | 已有 `/faults/:id`、`/incidents/:id`。遗留页 `?status=` 可寻址。深链比「保存的视图」产品便宜 | 不要在 SPA 迁移时换短号 |
| 12 | **空态三分法** | 检索空 / 首次空（未配 AM、未配值班）/ 无套餐。自托管仍有首次空 | 不要抄整页营销对开；抄分类 |
| 13 | **删通知规则 ≠ 取消指派** | 休假走 override（P0-5） | 不要用「把人从政策里删掉」当休假 |
| 14 | **Stakeholder ≠ Responder** | 内部通报走同一 outbox、单独模板，不复用 page 规则 | 不要为对等补 15 万订阅的状态页 |
| 15 | **Merge 不默认合并人** | 已有 included / excluded。写死人跟不跟 | 不要默默把 responders 并过去 |

出处：`pagerduty-analysis.md` §12。

### 13.2 该抄 Flashduty 的

这些是控制台和文档同时钉死、且能加强 Fault 工作台而不是把它变成协作空间克隆的设计。

| # | 抄什么 | 落到 SRE Control 哪一句 | 不要连皮带骨一起搬 |
|---|---|---|---|
| 1 | **进度轴的 UI 用词：红点不数 Closed** | 三轴已经比 FD 两轴硬。Channel 卡只数 Triggered / Processing。总览 / 故障列表的活计数应对齐「还要人看的」，不要把 source resolved 的证据算进红点 | 不要把三轴压回 Triggered / Processing / Closed |
| 2 | **Delay window 和 Flapping 分层** | 已有 flap 表和 `wait_seconds`。叙事写成「抖动在分派前按次数按住，延迟窗是最后 N 秒」 | 不要合成一个「防抖」开关 |
| 3 | **Notify by = 严重程度 × 媒介矩阵，并允许跟随个人 / 跟随统一** | 这是个人偏好和团队政策的显式交接点。比「渠道列表」教得会。P1-2 若做人规则，用这套交接点 | 不要一次上 PD 六段 + FD 四场景 |
| 4 | **分派命中即停 + 无策略不通知 是一种合法选择** | SRE Control 现状是 matcher → team default → global default，**总有落点**。两种都自洽，要显式选，不要隐式全局 | 不要默默改成「没配就不响」还不写进 CLAUDE |
| 5 | **排除放接入组，静默 / 抑制可留痕** | Drop = 防火墙，无记录。Silence / inhibit = 策略，可留痕。SRE Control 对 AM 源跳过后两者是对的；对非 AM，保持这个分组 | 不要在 AM 源上再做 FD 式静默 |
| 6 | **ACK 深链 `?ack=1`** | 机器人没有卡片按钮时，一条带 ack 的 URL 就能闭环。跨渠道同一语义（重复 Ack → 200 已对） | 不要在身份映射（P0-2）之前把它当成法律认领 |
| 7 | **模板失败有默认兜底，渠道超限行为写死** | Telegram 超限不发送、Slack 截断、Teams 报错——比「尽量发出去」可预测。引擎 Go html/template + 阉割 sprig，与 Go 栈同族 | 不要为每个渠道发明不同的「尽量」 |
| 8 | **媒介按场景裁剪** | 故障分派才有语音 / App；协作、换班、状态页逐步降级。SRE Control 只有飞书卡片可 ACK、email / webhook fire-and-forget——和「协作场景故意去掉语音」是同一类克制 | 不要做一张万能偏好表 |
| 9 | **详情整页 + `?tab=`，嵌套配置用抽屉** | FaultWorkspace / IncidentRoom 已经是整页。策略编辑学 60% 宽编号抽屉、粘性 Save / Cancel，避免再开一堆 modal | 不要把 Fault 工作区改成抽屉 |
| 10 | **每页文档深链 + 控件旁 helper** | Owning team 的 MTTA 归属、Delay window 的「窗内关闭不通知」，写在控件旁边。比单独一篇 CLAUDE 更接近操作者 | 自托管没有套餐页，但「未配 AM」需要同样诚实的琥珀横幅 |
| 11 | **动态分派用标签约定，不新增编排产品** | `sre.*` 标签已经在走标签驱动自动化。若要动态改值班对象，`layer_person_reset_N_…` 这种约定比再做一个 Orchestration 轻 | 不要为此上 Event Orchestration UI |
| 12 | **OSS 边界叙事：生成开源 / 响应自建** | 夜莺生成、Flashduty 收口。SRE Control：AM 生成并拥有 Alert，自己拥有 Fault / Incident。切开的层不同，边界同类 | 不要为对等去自建 Ruler 或抄 55 个适配器 |
| 13 | **给管道一个工作台外观（如果以后做 Space）** | 卡片活计数 + 星标 + 团队域 + grid/list，比设置页一行更像日常入口。问「谁拥有集成 + 降噪 + 分派 + 活计数」，不要先问「通知渠道表怎么建」 | **现在不要做。** 脊柱是 Fault。先 P0-1 |

出处：`flashduty-analysis.md` §12。

### 13.3 不该抄的（两边一起写死）

**不该抄 PagerDuty**

- 把 Incident 当 Alert 的默认自动产物（和人声明 + 双关闭门冲突）。
- 没人 on-call 就丢事件且不审计。
- 为对等去补 Status page / 公开订阅 / 15 万上限 / WhatsApp EA。
- 在 AM 权威源上再做 Event Orchestration 或 Intelligent Grouping。
- 抄 Rails + React 混部。那是 15 年产品的伤疤。
- 抄 AI Actions 计价表和「声称节省 20 分钟」。
- 把 `notify.Channel` 继续当「服务管道」——FD 已经用 Channel 占了协作空间。若引入管道，用 Service / Space，不要再占 Channel。
- 为了「像 PD」去自建 Ruler 或 55 个云监控适配器。PD 自己都不做规则引擎。
- 低 urgency 不能手动升级这条物理定律。SRE Control 的 `wait_seconds` 允许先等再叫，对齐「抖动 / 延迟在分派前，升级 timeout 在分派后」即可，不必抄「EP 零延迟」。

**不该抄 Flashduty**

- 把 Incident 当告警聚合的默认产出（和人声明事故冲突）。若借鉴聚合，停在 Alert / Fault。
- 为对等去补 Status page / 公开订阅 / 作战室多 IM。
- 在 AM 权威源上再做一层 FD 式静默 / 抑制。
- 把 Channel 一词继续当通知媒介，或反过来用 Channel 当协作空间——两个概念会在自己的代码里撞车。
- 抄 Datadog RUM 收自己的控制台。
- 抄四条产品轨（On-call / Monitors / RUM / AI SRE）。自托管单产品，设置进控制中心已经对了。
- 抄「排除无记录」到 mapping 失败上。未映射必须进治理队列。
- 抄 Free 1 Channel / 猫+盾锁门控。没有套餐就不要装套餐空态。

**两边材料都缺、禁止假装看见的（避免把缺口写成「产品没有」）**

来自 `pagerduty-analysis.md` 材料缺口、`flashduty-analysis.md` §13：

- PD New Incident 表单字段、conference 控件、手建是否真的不通知创建者（本租户 modal 没打开）。
- PD / FD 的 Incident / 故障详情时间线、真实数据上的配色。
- PD Event Orchestration 画布 / 动作目录；Incident Workflows 控制台入口（菜单未摘到）。
- PD / FD 创建 Schedule 的真实表单（两边 Pass 1 都没走进去）。
- FD 降噪五块 pane 的真实控件、智能聚合是否有相似度滑条。
- 官方 Flashduty vs PagerDuty 对比页的逐条表格（两边材料都声明没摘到）。
- 定价现价。本文不写价。

---

## 一句话结论

SRE Control v0.1.0 已经在对象模型和权威分裂上领先：AM 拥有 Alert，自己拥有 Fault / 可否决的 Impact / 人声明且有双关闭门的 Incident，家是基础设施工作台而不是告警 inbox。PagerDuty 该抄的是所有权拆分、三轴打扰度、手建不走告警管道、EP 快照、Events 与 REST 分家、以及「没人 on-call 必须产品化」（但不要抄成丢事件）。Flashduty 该抄的是红点不数已关闭、防抖两层旋钮、严重程度×媒介矩阵、ACK 深链、抽屉+`?tab=`、以及「聚合停在告警层」。两边都不该抄的是：自动 Incident、在 AM 源上重做降噪、状态页 / 语音 SKU 对等、以及用 Channel / Service 一词继续制造命名事故。

下一步不是功能清单，是 §12 的 P0 五条决策：团队触达身份、卡片 ACK 的法律含义、管道对象先不做、空班写死留下并警告、一等 override。这五条选完，论点才能在生产里自圆其说。

主要来源：`/workspace/research/alert-system.md`、`/workspace/research/pagerduty-analysis.md`、`/workspace/research/flashduty-analysis.md`。补引：`pagerduty-docs.md`、`pagerduty-console-pass1.md`、`flashcat-docs-oss.md`、`flashduty-console-pass1.md`。调研日 2026-08-21，Asia/Shanghai。
