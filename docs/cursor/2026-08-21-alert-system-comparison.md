# 快猫星云 / PagerDuty / SRE Control 对比报告

调研日期：2026-08-20  
对照对象：

- 快猫星云 Flashduty：已登录 `console.flashcat.cloud`，文档 `docs.flashcat.cloud`，开源夜莺/Categraf/Flashduty SDK
- PagerDuty：已登录 `cvtesreops.eu.pagerduty.com`（试用过期），文档 support/developer/response.pagerduty.com，开源 IR 手册与 API 生态
- 本仓库 **SRE Control**（alert-system）：`CLAUDE.md`、ADR-0001/0003、`.spec-workflow/specs/alert-system-mvp/`、`web/src/App.tsx` 控制台信息架构

完整单品分析：

- [`2026-08-21-flashcat-flashduty-analysis.md`](./2026-08-21-flashcat-flashduty-analysis.md)
- [`2026-08-21-pagerduty-analysis.md`](./2026-08-21-pagerduty-analysis.md)

---

## 1. 先把三套术语对齐

三家都说「告警 / 故障 / 事故」，但指的不是同一层东西。混用会得出错误结论。

| 概念 | Flashduty | PagerDuty | SRE Control（我们） |
| --- | --- | --- | --- |
| 上游一次触发/恢复 | **事件 Event** | Event | Signal / Alert occurrence（`source_id + upstream_fingerprint + starts_at`） |
| 去重后的异常证据 | **告警 Alert**（按 `alert_key` 合入） | Alert（服务内，常被折叠进 Incident） | **Alert**。AM 来源三轴：`source_state` / `response_state` / `suppression_state` |
| 技术故障（资源/能力坏了） | 不单独建模 | 不单独建模 | **Fault**（candidate/active/monitoring/resolved） |
| 需要人协作的响应容器 | 也叫 **故障 Incident**（自动从告警生成，是处理主对象） | **Incident**（自动或手动，必须挂 Service） | **Incident**（人工宣布，一个可绑多个 Fault，双重关闭门槛） |
| 业务/应用是否真的受损 | 无。最多用标签和状态页组件 | Business Service + 人工判断 | **FaultImpact**：potential / confirmed / ruled_out |
| 配置与路由根 | **协作空间 Channel** | **Technical Service → Escalation Policy** | **AlertSource + AlertMapping + Entity 图**；通知根是 Team |
| 谁拥有 Alert 生命周期 | Flashduty 自己（手动关故障会关告警） | PagerDuty 自己 | **Alertmanager 唯一权威**；本地禁止对 AM 来源人工 Resolve |

一句话：

- Flashduty / PagerDuty 卖的是 **On-call：把告警变成叫醒和协同**。
- 我们要做的是 **Infrastructure-first 的事故响应操作系统**：先看清共享基础设施和爆炸半径，再决定要不要宣布 Incident。Alert 只是证据。

早期 spec（`alert-system-mvp/requirements.md`）曾对标 PagerTree / Squadcast / AlertOps，链路停在 `接入 → 去重 → 路由 → 通知`。ADR-0001 之后产品已经走到 `Signal → Alert → Fault → Incident`。和两家竞品对比时，必须用 ADR 后的模型，不能用 MVP 初稿。

---

## 2. 默认工作台：三家首页回答的问题不同

| | Flashduty | PagerDuty | SRE Control |
| --- | --- | --- | --- |
| 登录后第一屏 | **协作空间列表**（`/channel`） | **Incidents 列表** | **运行总览**（`/`） |
| 首页要回答 | 我的作战单元里有没有单？集成配好了吗？ | 有没有要我 Ack 的单？我现在是否值班？ | 基础设施健不健康？当前 Fault？谁受影响？有没有活跃 Incident？ |
| 一级导航气质 | IM/工作区气质：空间、单聊、收件箱、AI 聊天 | 运营控制台：Incidents / Services / People | 领域分层：总览 / 故障 / 事故 / 告警 / 服务，配置收进控制中心 |
| 空账号最小对象 | 系统 `Default` 协作空间 | `Default Service` + `Default` EP + Owner 永远值班 | 需要登记 AlertSource、实体、映射后才会自动出 Fault |

Flashduty 顶栏甚至把「协作空间」和 On-call 并列。人被训练成：先进自己的空间，再看空间里的故障。

PagerDuty 顶栏第一项永远是 Incidents。右侧 On-call 卡片回答「现在该谁接电话」。Service 是配置中轴，但不是首页。

我们的侧栏已经按不变量 19 收过一遍：

```
运行：运行总览 / 故障 / 事故 / 告警 / 服务
响应：静默策略
管理：控制中心
```

控制中心再按任务分组：数据与模型、组织与责任、响应策略、自动化、兼容接入。这比两家把「所有配置平铺进顶栏」更接近平台型 SRE 的任务切分，也刻意避免 Flashduty 那种「AI 和空间占满一级导航、分析/配置折叠在图标里」的认知负担。

**结论**：不要把首页改成 Flashduty 频道墙，也不要改成纯 Incident 列表。首页继续回答基础设施健康和爆炸半径。Incident 是升级后的下一层，不是默认世界观。

---

## 3. 领域模型对照

### 3.1 对象怎么长

```text
Flashduty
  协作空间
    ├ 专属/共享集成
    ├ 降噪（事件聚合 / 告警聚合 / 抖动 / 静默 / 抑制）
    ├ 分派策略（多条、可条件匹配、多环节升级）
    └ 故障 ← 告警 ← 事件

PagerDuty
  Technical Service ──1:1── Escalation Policy ── N Level ── User/Schedule
       │
       └ Incident ← Alert ← Event
  Business Service（可选聚合）
  Service Graph（uses / used by）

SRE Control
  Entity 图（infrastructure / platform / application，多对多）
       │
  Alert ──(mapping/证据角色)── Fault ──(Impact 三态)── 应用服务/团队
       │                         │
       │                         └── 人工宣布 ── Incident（多 Fault，primary_fault 可换）
  Alertmanager = Alert 权威
```

### 3.2 自动建单 vs 人工宣布

| | 默认是否自动产生「要人处理的单」 | 关闭门槛 |
| --- | --- | --- |
| Flashduty | 是。事件→告警→故障，无聚合时几乎 1:1 | 手动关或告警全恢复；手动关会连带关告警 |
| PagerDuty | 是。Event 打到 Service 就建 Incident | Ack/Resolve；服务可关自动 resolve |
| SRE Control | **否。** 映射命中最多自动建/更新 Fault；Incident 必须人宣布，且需要 confirmed 应用影响（不变量 11、16、17） |

这是战略分歧，不是功能多少的问题。

Flashduty/PagerDuty 优化 MTTA：单子来了就要有人认。  
我们优化的是「共享基础设施上，什么时候值得拉跨团队响应」。E2B 黄金场景写得很死：placement 失败高但用户 create 仍成功，只形成 Fault 和风险；SLO 击穿才建议宣布 Incident。

如果照抄两家的自动 Incident，会把平台 SRE 重新淹进告警单，ADR-0001 作废。

### 3.3 图与影响

| | Flashduty | PagerDuty | SRE Control |
| --- | --- | --- | --- |
| 拓扑 | 基本没有。侧栏「服务目录」实测跳到日历。用标签和 Link 集成跳外部 CMDB | Service Graph + Business Service。手拉或 API/Terraform 灌依赖 | Canonical Entity / Alias / Relation，消费者→提供者，反向遍历深度 8 |
| 影响 | 不建模 | 用 Business Service 近似「业务能力」 | FaultImpact 三态；图遍历只能 potential；SLO/人工才能 confirmed |
| 证据角色 | 无 | 无（Alerts 计在 Incident 下） | trigger / supporting / cause_candidate / symptom / amplifier |
| 自动 Fault 准入 | 无此对象 | 无此对象 | 必须 MappingPolicy 或受信标准标签，禁止按 alertname 猜 |

PagerDuty 的 Service Graph 是「服务目录可视化」，不是故障传播引擎。Standards 1/8 这种红分，是在治理目录完整度，不计算爆炸半径。

我们已有、两家都没有的，正是产品差异化，不要为了「看起来更像 PagerDuty」而删掉。

---

## 4. 路由与责任：三种完全不同的根

### Flashduty：协作空间根

- 告警先进入空间（专属）或按路由规则进入空间（共享）。
- 空间内多条分派策略，优先级匹配，命中即停。
- 策略对象可以是值班表、团队、个人，并可同时选通知渠道和 IM 群。
- 动态分派可按标签改人。
- **没命中策略：不通知。**

适合：按业务线/组件切开的中国运维团队，一个空间就是一个 IM 作战单元。

风险：空间同时承担隔离、权限、集成、降噪、分派、聊天，概念过载。没有空间就没有世界。

### PagerDuty：Service → Escalation Policy 根

- 事故必须挂 Service。
- Service 只能绑 **一条** EP。
- EP 的 Level 决定叫谁；User Notification Rules 决定怎么叫。
- Urgency 选择走 High 还是 Low 那套个人规则。
- 新账号 Default Service + Default EP + Always on call，十分钟能接到邮件。

适合：微服务目录已经存在、责任按服务切、国际 IM（Slack/Teams）协同的组织。

风险：一个服务一条策略，复杂条件路由要靠 Event Orchestration（加购）补。没有服务目录就建不出干净的世界。

### SRE Control：权威同步 + 实体绑定 + 团队响应

第一阶段（不变量 14、15、17）：

- Alertmanager 做完 route/group/silence/inhibit/Alert 级通知。
- 我们同步 Alert，做实体绑定和 Fault。
- 本地 route/group/silence/inhibit **对 AM 来源显式跳过**，避免双权威。
- 应用服务和 `owner_team_id` 必须登记，遥测不能自动建团队。
- 人员升级 / 渠道升级 / 飞书卡片走我们的响应层，对象是 Fault/Incident，不是再广播一遍 Alert。

控制中心里仍保留「本地告警路由」，但文案已经写成：仅非 AM 兼容来源使用。

**不要**把 Flashduty 协作空间或 PagerDuty Service 改造成我们的导航根。我们的根是 Entity。Service 目录页是应用服务登记和依赖，不是 On-call 路由锚点。

可以学的具体机制：

- PagerDuty：每个可响应对象都要能回答「on call now 是谁」。运行总览和 Fault 工作区应继续把负责团队/当前值班放到第一屏。
- Flashduty：共享集成必须有兜底路由，否则丢事件。我们对 AM 对账失败只标 stale、禁止推断 resolved，已经更严；对非 AM 来源仍应坚持「命中默认团队，禁止静默丢弃」。
- 两边都把「升级策略」和「个人通知偏好」分开。我们已有 Escalation Policy 与 Notification Policy，方向正确；缺的是 PagerDuty 那种「强烈建议加电话」的个人触达完整性提示。

---

## 5. 降噪

| 能力 | Flashduty | PagerDuty | SRE Control |
| --- | --- | --- | --- |
| 事件级去重 | 事件聚合，`alert_key`，默认 24h | fingerprint / incident key（集成侧） | fingerprint；AM 来源用上游 fingerprint + starts_at |
| 相似告警合成处理单 | 告警聚合：智能相似度或规则维度 | Intelligent Alert Grouping（AIOps，服务级，本账号未开） | **不把相似 Alert 合成 Incident**。Fault 按 entity + kind + 关联标签 |
| 分组通知 | 聚合进故障后少通知 | group 在 PD 侧；也依赖上游 | AM 权威来源不再本地 group；非 AM 保留 group_wait/group_interval，空 group_by 走单例组 |
| 静默 | 单次/星期/服务日历；可丢弃或打标；快速静默 | Maintenance Window + Event Orchestration | Silence matcher；AM 来源以 AM 为准 |
| 抑制 | 10 分钟窗口，按条件+相同项 | 多在 AIOps / Orchestration | 管道有 inhibit；AM 来源跳过 |
| 抖动 | 一等配置（次数/窗口/静默） | 部分在 AIOps | `FLAP_WINDOW_SECONDS`，默认 0，opt-in |
| 风暴 | 阶梯阈值再通知 | 弱于 Flashduty 宣传点 | `storm_threshold_per_min`，默认 0，opt-in；`storm_summary` timer |
| 降噪默认 | 事件聚合开，告警聚合关 | 本账号 Alert Grouping 关 | **一律 opt-in**（不变量 10） |

Flashduty 的降噪文档是三家最完整的产品化说明，尤其是：

- 两层聚合不要混（事件→告警 vs 告警→故障）
- 快速静默自动排除无意义标签，并解释排除原因
- 抖动「仅提醒 / 提醒后静默」

这些可以借鉴到 **Fault 证据噪声** 和静默 UX，而不是搬到 Alert 再广播。

PagerDuty 把智能聚合做成加购，内核保持可解释的服务级开关。我们把 LLM 放异步 enrichment、禁止进管道（不变量 9），与 PD 的「智能层可摘掉」一致，与 Flashduty「智能聚合写进降噪主路径」不同。

---

## 6. 通知与协同

| | Flashduty | PagerDuty | SRE Control |
| --- | --- | --- | --- |
| 中国 IM | 飞书/钉钉/企微一等，含私有化 | 弱 | 飞书卡片 + 验签，REST 与回调同语义 |
| 国际 IM | Slack/Teams/Telegram/Zoom | Slack/Teams 一等 | 无 |
| 电话/短信 | 国内触达是卖点，用量看板按渠道计费 | 核心能力，Live Call Routing 加购 | 邮件已通；电话是插件位；飞书真实发送仍 HOLD |
| 通知所有权 | 分派策略 + 模板 + 个人偏好 | **个人 Notification Rules 为主** | 团队 Notification Policy + 人员/渠道升级 |
| 协同 | 协作空间消息 + 作战室自动建群 + 群内回写 | Slack 频道 / Teams；产品自己不做频道首页 | Incident Room 是产品内响应室 |
| Ack 入口 | 控制台、语音、IM、App | 控制台、电话、短信、App、Slack | REST、飞书卡片；重复 Ack 短路 200 |

Flashduty 的作战室和 AI 回帖，是「IM 原生」做到底的结果。我们的 Incident Room 在产品内，通信目标对外只许脱敏审计引用（不变量 13）。不要为了学作战室，把原始 webhook/群地址重新暴露到详情页。

PagerDuty 不把聊天当首页，是因为它认准自己是叫醒系统。Slack 集成再深，也只是 extension。

对我们：

- 继续飞书，不补钉钉/企微除非有明确用户。
- Incident 通报走 `notify_send` outbox，禁止在持锁事务里打外部 IO（不变量 4）。
- 学 PD 把「当前值班人」做成到处可见的小部件；学 Flashduty 把通知失败/消耗记进时间线和用量，而不是学它把空间做成首页。

---

## 7. 排班与升级

三家都有 Schedule + Escalation，但完成度差距大。

Flashduty 最深：多层规则、日期掩码、公平轮换、连续轮换、账户级角色、临时调班抢占、交接/定时播报、服务日历。这是中国「白班研发 + 夜班 SRE + 周末监控组」的真实班表。

PagerDuty 最经典：EP Level + Schedule + Override。新 UI 在推 shift-based、multi-responder。本账号连一张班表都没有，靠 Always on call 撑着，说明 **排班不是最小闭环的前置**。

我们已有：团队时区、人工班次、轮换、升级策略、severity_config、ops 日报/悬置/值班播报。缺的是 Flashduty 那种公平轮换/日期掩码的表达力，以及 PD 那种「先 Default 永远值班，再逐步加班表」的空账号体验。

优先级建议：空账号体验和「现在谁值班」可视化，高于再做一个公平轮换算法。

---

## 8. AI

| | Flashduty | PagerDuty | SRE Control |
| --- | --- | --- | --- |
| 控制台位置 | 一级产品 `/ai-sre` | 顶栏 AI + AIOps 加购 | 无一级入口（正确） |
| 形态 | 开放 Agent loop：Sandbox/BYOC、Skill、MCP、A2A、知识库、记忆 | 预置 Agent + Advance 问答 + AIOps 关联 | 异步 enrichment 预留；管道/handler/通知意图禁止模型调用 |
| 和故障的关系 | 作战室自动第一轮诊断 | 依赖 Automation Actions 预配脚本 | AI 不得宣布 Incident、不得改写 Fault 裁决 |
| 不可用时 | 核心 On-call 仍可用 | 内核 Incidents 仍可用 | 必须 100% 不受影响 |

Flashduty 把 AI 做成差异化叙事是商业选择。我们若在 Fault 图和 AM 权威还要被人理解的阶段，把「AI SRE」做成一级菜单，会重复 Flashduty 空账号里「侧栏比内容还复杂」的问题，也违反不变量 9 和 19（未来能力不占一级导航）。

可以学的只有交互壳：Fault/Incident 详情里放「建议」区块，明确标 AI，失败则不渲染。不要学控制台里的 Kubernetes/GitHub 插件市场。

---

## 9. 状态页、复盘、分析、自动化

| 能力 | Flashduty | PagerDuty | SRE Control |
| --- | --- | --- | --- |
| 对外状态页 | On-call 随赠，公开+内部，RSS/Atom | 按套餐，Stakeholder 席位看内部页 | 明确非本期 |
| 复盘 | 协作编辑 + AI 草稿 | 有（原 Jeli），套餐限额 | Incident 关单后 Learning，未做成独立产品 |
| 分析 | 用量（通知/故障/变更）+ 付费洞察 | MTTA/MTTR/Interruptions/Ack Rate | 告警噪声画像（30 天次数、平均持续）；无全局 MTTA 看板 |
| Runbook/自愈 | 自定义操作 Webhook | Automation Actions 加购 | 白名单类型化动作；生产默认 Disabled；cleanup_apply 审批分离 |
| 变更事件 | 有接入和用量 | Change Events + AIOps 关联 | 通用 webhook 预留 |

PagerDuty Analytics 的指标语言（MTTA/MTTR/打断次数）可以直接借到运行总览，但口径要改：

- MTTA 应对 **Fault 或 Incident**，不要对 AM Alert 再算一遍（Alert 级叫醒不是我们的权威工作）。
- 打断次数对平台 SRE 有意义，但要按团队/基础设施能力聚合。

自动化：三家都不在核心里裸跑 shell。Flashduty 自定义操作要二次确认；PD 整页 Contact Sales；我们比两家更严（UAT 授权、hash 审批、脱离 request cancel 的清理）。保持，不要为了「功能对等」放松不变量 12。

---

## 10. 信息架构与配置治理

| | Flashduty | PagerDuty | SRE Control |
| --- | --- | --- | --- |
| 配置放哪 | 大量挂在协作空间内部；全局只有值班/模板/映射/表单/字段 | 挂在 Service / People / Account；Extensions 单独 | 控制中心按用户任务分组，不按数据库表平铺 |
| 空态 | 紫猫插画 + 创建第一个 X | 插画 + Default 对象已经在 | 仍偏后台，空账号不会「自动有一个可响应单元」 |
| 付费墙 | 额度（0/1、0/20）和「暂无权限」 | 黄条试用过期 + Start Trial/Contact Sales | 无（私有化） |
| 选择器 | 团队/空间下拉 | 服务/用户搜索下拉 | 不变量 19：目录驱动、可搜索、禁止手输 UUID |
| 机制说明 | 控制台较少讲原理，文档很全，「了解告警流转」是少数教学链 | 蓝条/橙条直接教模型（EP 是什么、为什么要电话） | 禁止页面常驻「唯一权威、系统如何工作」；只留真实风险 |

PD 的产品教学写在设置页蓝条里，比 Flashduty 更「把模型教给用户」。我们不该在页面堆机制说明，但 **创建映射 / 宣布 Incident / 关单门槛** 这类高后果操作，应该用和 PD 橙条同级的后果提示（不变量 11 已要求前后端提示门槛）。

Flashduty 把「了解告警流转」放在告警列表，是很好的按需帮助。我们对应的应是「Alert / Fault / Incident 有何不同」，做成 Tooltip 或文档，不要做成首页 Banner。

---

## 11. 开源与集成策略

| | Flashduty | PagerDuty | SRE Control |
| --- | --- | --- | --- |
| 开源什么 | 夜莺、Categraf、RUM SDK、CLI/SDK/MCP/Terraform | IR 手册、安全培训、API SDK、Terraform、MCP、部分内部库 | 本仓库即产品；管道借鉴 AM 算法但不 import |
| 不开源什么 | On-call 内核、AI SRE 内核 | Operations Cloud 内核 | — |
| 集成数量 | 60+，中国云/监控更全；兼容 PD Events API；HTTP/DB Pull | 750+，海外生态；Events API 是行业插头 | AM / Grafana / generic hook；来源是逻辑集群+hook UUID |
| 新业务怎么接 | 新集成适配器 + 空间路由 | 新 Integration + Service | **适配我们的契约**：Entity/Relation/Alert/Fault/Impact/团队。禁止核心为业务加私有实体 |

Flashduty 开源夜莺是为了占领告警引擎；再把 On-call 收成 SaaS。文档反复说：夜莺不要当 PagerDuty 用。

PagerDuty 开源手册是为了占领「事故响应该怎么做」的话语权。

我们的立场更接近「私有化、Alertmanager 已存在的平台 SRE」。集成策略应对齐 ADR-0003：标准资源标识 + 服务登记 + 映射，而不是追 750 个 logo。E2B 只是黄金场景，页面禁止 `e2b.*` 私有键。

---

## 12. 实现架构对照（只谈已证实的）

| 主题 | Flashduty（从文档/SDK 推断） | PagerDuty（从文档/开源推断） | SRE Control（代码事实） |
| --- | --- | --- | --- |
| 形态 | 多产品 SaaS | 多区域 SaaS（`*.eu.pagerduty.com`） | Go 单体 + PG，无外置 MQ |
| API | 337 接口 / 39 service，Go SDK 1:1 | REST + Events API + Webhooks v3 | 自有 REST；ingest hook |
| 通知发送 | 渠道多，用量计费 | 个人规则 + 升级重复 | timers + outbox，`notify_send`，幂等键含 round |
| 权威 | 自己 | 自己 | AM 权威；本地响应轴分离 |
| AI 位置 | 独立运行环境（Sandbox/Runner） | 独立产品线 | 管道外异步，可摘 |
| 前端 | SPA，中文为主，AntD 气质 | SPA，英语为主 | Vite React 19 + AntD 5.29.3 |

我们和两家最大的实现差异不是语言栈，而是 **认不认上游 Alertmanager 为 Alert 权威**。照抄任何一家的「在自己系统里 Resolve 告警」，都会破坏不变量 14。

---

## 13. 能力矩阵（第一阶段该不该追）

图例：● 已有或明确做；◐ 部分；○ 刻意不做/后期；■ 竞品核心；□ 竞品加购或弱

| 能力 | FD | PD | 我们现在 | 第一阶段建议 |
| --- | --- | --- | --- | --- |
| Alert 接入与对账 | ■ | ■ | ● AM hook + `/api/v2/alerts` | 保持，把它做硬 |
| Alert 再路由/再通知 | ■ | ■ | ○ AM 来源跳过 | 保持跳过 |
| Fault 自动准入 | □ | □ | ● Mapping | **保持，这是我们的核** |
| Impact 三态 + 爆炸半径 | □ | □ 服务图近似 | ● | **保持，这是我们的核** |
| 人工宣布 Incident + 关单门槛 | □ 自动建单 | □ 自动建单 | ● | **保持** |
| 协作空间/频道首页 | ■ | □ Slack | ○ | 不抄 |
| Service→EP 1:1 | □ 空间多策略 | ■ | ◐ 团队+EP | 学「现在谁值班」，不改成 Service 根 |
| 个人通知规则完整性 | ◐ | ■ | ◐ | 学提示，补联系方式健康度 |
| 中国 IM 作战室 | ■ | □ | ◐ 飞书卡片 | 飞书闭环优先于建群 |
| 智能聚合 | ■ | □ 加购 | ○ 管道禁止 LLM | 保持旁路 |
| 开放 AI Agent | ■ | □ 预置 | ○ | 不占一级导航 |
| 状态页 | ■ | ■ 加购 | ○ | 不阻塞 |
| MTTA/MTTR 看板 | ◐ 付费 | ■ | ◐ 噪声画像 | 可加在总览，口径对 Fault/Incident |
| Runbook 白名单 | ◐ Webhook | ■ 加购 | ● 更严 | 保持严 |
| 750+ 集成 logo | ◐ 60+ | ■ | ○ 标准契约 | 不追数量 |

---

## 14. 我们应该坚持、应该借鉴、应该拒绝

### 坚持（两家都没有或做反了）

1. `Alert ≠ Fault ≠ Incident`。自动层停在 Fault；Incident 人工宣布。
2. Alertmanager 是 Alert 唯一权威。不同步「暂时看不到 = resolved」。
3. 图可达只产生 potential Impact；人工裁决不被自动覆盖。
4. 映射准入，禁止按 alertname 猜 Fault。
5. AI 旁路、降噪 opt-in、Runbook 白名单、通知 outbox。
6. 全局导航按 SRE 一级任务，不把 AI/机制说明/未来功能堆到侧栏。
7. 核心不随业务长私有实体。E2B 是验收剧本，不是对象模型。

### 借鉴（改 UX 和完整性，不改世界观）

1. **把模型教在高后果操作上**：宣布/关 Incident、创建 Mapping、跳过 AM 本地路由时，用一句后果提示，类似 PD 橙条。
2. **到处展示 on-call now**。PD 右侧栏、服务卡片都有；我们的总览/Fault/服务页应对齐。
3. **空账号最小闭环**：PD 的 Default Service+EP、FD 的 Default 空间。我们需要「登记一个 AM Source + 一条 Mapping + 一个默认团队」的引导，而不是空白控制中心。
4. **告警流转说明做成按需帮助**，对标 FD「了解告警流转」，解释三层对象。
5. **快速静默从当前 Fault/Alert 生成 matcher**，并列出被丢弃的标签及原因。
6. **通知渠道健康度**：没电话/没飞书绑定就警告，不要等漏叫。
7. **运营指标**：MTTA/MTTR/打断次数挂到 Fault/Incident，用量（飞书失败、重复提醒）进时间线。
8. **服务标准分的思路**可移植到实体登记：无 owner、无依赖、无 mapping 的应用服务给「未治理」而不是假装健康。

### 拒绝

1. 用协作空间或 Service 替换 Entity 图作为产品根。
2. 告警一进门就自动建 Incident。
3. 手动关单去改 AM 告警状态。
4. 未匹配策略就丢通知。
5. 把 AI SRE 做成一级导航或管道决策者。
6. 为了功能对等去追状态页、750 集成、智能聚合主路径。
7. 在页面常驻「我们如何对标 PagerDuty/Flashduty」。对比只留在 `docs/cursor/`。

---

## 15. 一句话定位差

**PagerDuty**：叫醒对的人。根是 Service，信仰是通知可靠，智能和自动化加购。

**Flashduty**：在中国 IM 里把告警合成故障并闭环。根是协作空间，信仰是降噪 + 飞书/钉钉/企微 + AI Agent。

**SRE Control**：让平台 SRE 先看见共享基础设施上的 Fault 和真实影响，再决定是否宣布 Incident。根是 Entity 图，信仰是分层对象和单一 Alert 权威。

两家都是优秀的 On-call 产品。都不是我们要成为的东西。调研价值在于：把 On-call 层（叫醒、升级、静默、值班）做成够用且可解释，把差异化留在 Fault / Impact / 人工 Incident，而不是在频道和 AI 聊天上和他们比密度。
