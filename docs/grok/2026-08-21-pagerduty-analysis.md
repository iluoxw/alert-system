# PagerDuty 产品分析报告

- 调研日期：2026-08-21（Asia/Shanghai）
- 读者：正在做 SRE Control（`iluoxw/alert-system`）的人。目标不是功能清单，而是把 **产品怎么被设计出来、控制台怎么被实现出来** 钉到可引用的事实。
- 材料纪律：只使用已读材料，不补拍、不编数字、不编截图。
  - 公开文档与开源：`/workspace/research/pagerduty-docs.md`（未登录控制台；developer 若干页 fetch 未渲染，见该文 §9）
  - 已登录 EU 控制台实勘 Pass 1：`/workspace/research/pagerduty-console-pass1.md`（`cvtesreops.eu.pagerduty.com`，用户 xiaowei luo / `P7DCOZA`，Account Owner，Trial Full User）
  - 对标用：`/workspace/research/flashduty-analysis.md`、`flashduty-console-pass1.md`、`alert-system.md`（§11–12；完整对比另文）
- 方法边界：Pass 1 只做导航 / IA / 空租户盘点，未创建、未删除、未保存。**试用已过期**（顶栏蓝条 *"Oh no! Your trial has expired… Select a Plan"*，非阻塞，UI 仍可点）。**New Incident 对话框未能打开**（见 §2.4）。未打开的面板一律用文档补，并标明「控制台未核验」。
- 术语先钉死：
  - **Urgency**（High / Low）= 怎么通知、升不升级。**Priority**（客户自定义 P1 / Sev-1…）= 做事顺序 / 状态页 / 工作流条件。**Severity**（`critical|error|warning|info`）= 事件/告警字段，可驱动 Dynamic urgency。
  - **Service** 是收事件、挂一条 Escalation Policy、长 Incident 的单元。**Schedule 自己不 page**，必须挂到 EP，EP 再挂到 Service。
  - **Event Orchestration** 在 Incident 出现之前改事件；**Incident Workflows** 在 Incident 存在之后跑；**AIOps grouping** 是把多条 Alert 合成一条 Incident。三层不要混称「自动化」。

---

## 1. 产品定位（Operations Cloud / IR / AIOps / Advance）

PagerDuty 对外卖的不是一个「告警箱」，而是一层 **Operations Cloud**：监控 / CI / 邮件 / API 进来之后，用同一套对象模型把人叫起来，再把协同、状态页、自动化和 AI 叠上去。公开材料把它拆成几块可单独计价、但共用对象模型的面，而不是五个互不相关的产品。

| 面 | 它是什么 | 计价线索（文档） | 材料出处 |
| --- | --- | --- | --- |
| **Incident Response（IR）** | 核心：Service + EP + Schedule + 个人通知规则。Incident 必须派给人 | Professional / Business / Enterprise；Free 限制 1 个 EP、1 个 Schedule、2 个 Team | 文档 [incidents](https://support.pagerduty.com/main/docs/incidents)、[escalation-policies](https://support.pagerduty.com/main/docs/escalation-policies)、[teams](https://support.pagerduty.com/main/docs/teams) |
| **Operations Cloud** | 平台伞：工作流执行历史、运营控制台、跨产品对象 | 文档把 Incident Workflows 的执行历史 / step output 写成 Operations Cloud；营销生命周期 Detect → Triage → Diagnose → Remediate → Continuous Learning | 文档 [incident-workflows](https://support.pagerduty.com/main/docs/incident-workflows)；营销 [IR lifecycle](https://www.pagerduty.com/resources/digital-operations/learn/incident-response-lifecycle-for-devops/) |
| **AIOps** | 降噪 + RCA 小组件 + Global Orchestration + Operations Console + 更高 ingest | **按事件消耗**（2023-04 接替按座 Event Intelligence；EI 2023-04-11 起不对新客户开放） | 文档 [aiops](https://support.pagerduty.com/main/docs/aiops) |
| **PagerDuty Advance** | 生成式 / 智能体：Assistant、SRE / Scribe / Shift / Insights Agent、摘要 | Pro / Business / Enterprise 上的附加或一次性 **AI Actions** | 文档 [pagerduty-advance](https://support.pagerduty.com/main/docs/pagerduty-advance) |
| **Process Automation / Runbook Automation** | 2020-10-01 收购 Rundeck（约 $100M）后的商业层；Automation Actions 是产品化接点 | 独立附加；控制台 Automation Actions 对本试用租户整页 Contact Sales | 新闻 [acquisition](https://www.pagerduty.com/newsroom/pagerduty-closes-acquisition-rundeck/)；控制台 `/automation-actions` |
| **Status Pages** | PagerDuty **自己的** Internal / External / Private 状态页，**不是** Atlassian Statuspage | External 每页最多 150,000 订阅；H2 2025 出 Audience-Specific（Business / Enterprise） | 文档 [status-pages-overview](https://support.pagerduty.com/main/docs/status-pages-overview)、[external-status-page](https://support.pagerduty.com/main/docs/external-status-page) |
| **Stakeholders** | 状态更新，不是 page。Full / Limited 两种只读、降价许可 | 工作流触发时可自动订阅最多 25,000 人 | 文档 [communicate-with-stakeholders](https://support.pagerduty.com/main/docs/communicate-with-stakeholders) |
| **Customer Service Ops** | 扩展卡里出现的另一条产品轨（与 IM / EventBridge 并列） | 控制台 Extensions 卡片「PD Customer Service Ops」；文档计划名里的 Digital Ops / EIM 与此相关 | 控制台 `/extensions/`；文档多处计划矩阵写 Digital Ops / EIM |

控制台实勘能钉死的定位信号：顶栏 8 个 mega-menu 已经按 **Incidents / Services / People / Automation / AIOps / Analytics / Integrations / Status** 切开，另有 **AI → `/advance`**。这不是「一个 Incident 列表加设置」，而是平台壳。本租户试用过期后，IR 主路径（Incident / Alert / Service / EP / Schedule / Team / User / Profile）仍可看；AIOps 降噪、Advance、Automation Actions、External Status Page、自定义 Incident Types 变成整页 upsell。Account Settings 的 Products Trials 写明当前计划 **没有资格** 再开 Advance / AIOps 试用。

```
监控 / CI / 邮件 / API
        │
        ▼
 Events API v2  ──或──  Email ingest  ──或──  REST API（手建 / 声明，不产生 Alert）
        │
        ▼
 Event Orchestration（Global 规则 → Routing → Service 规则）
        │
        ▼
 Alert（triggered | resolved | suppressed | suspended）
        │
        ▼
 Incident（triggered | acknowledged | resolved）
        │
        ├── Escalation Policy → Schedule / User → Notification Rules
        ├── Responders、notes、timeline、conference、custom fields
        ├── Incident Workflows（聊天、状态页、自动化）
        └── Stakeholders / Status Pages
```

来源：文档 dump §1 架构图（[incidents](https://support.pagerduty.com/main/docs/incidents)、[alerts](https://support.pagerduty.com/main/docs/alerts)、[event-orchestration](https://support.pagerduty.com/main/docs/event-orchestration)、[incident-workflows](https://support.pagerduty.com/main/docs/incident-workflows)）。

**和 Flashduty / SRE Control 读定位时不要对齐的三句话：**

1. PagerDuty 的 Incident **默认由 Alert 触发**，也可以 Web / REST **手动声明**；手建 / REST 的 Incident **没有 Alert**，因此 Orchestration / grouping 都不适用（[alerts](https://support.pagerduty.com/main/docs/alerts)）。Flashduty 的 Incident 也可以由告警聚合自动生成；SRE Control 的 Incident 是人声明的多 Fault 容器（`alert-system.md` §1）。同名不同权。
2. AI 是 **两个 SKU**：降噪 / 根因小组件 = AIOps（事件价）；聊天 / Agent / 摘要 = Advance（AI Actions）。Operations Console 上的 SRE Agent 两样都要（[aiops](https://support.pagerduty.com/main/docs/aiops)、[sre-agent](https://support.pagerduty.com/main/docs/sre-agent)）。
3. PagerDuty **不自带** Prometheus 式规则引擎。告警生成在上游。Rundeck / Process Automation 是「进来之后怎么自动修」，不是「怎么算出这条告警」。

营销口径还有一份公开的内部重大事故手册 [response.pagerduty.com](https://response.pagerduty.com/)（IC / Deputy / Scribe / SME / Liaison，SEV-1…SEV-5）。那是 **过程**，不是产品里的 Priority / Urgency / Severity（文档 dump §2.4）。H2 2025 发布博客自称 150+ 特性（[product-launch-2025-h2](https://www.pagerduty.com/blog/product/product-launch-2025-h2/)）——营销数字，材料未独立核验。

---

## 2. 控制台 IA（8 个 mega-menu、试用门控、空租户实勘）

本节以 **EU 已登录控制台** 为准（`cvtesreops.eu.pagerduty.com`，Pass 1，2026-08-21）。文档站是 Support KB + Developer portal 的信息架构，和真实 UI 高度同构，但菜单名、门控文案、空态、坏掉的 CTA 必须以控制台为准。

账户身份：xiaowei luo，用户 ID `P7DCOZA`，Account Owner，Trial Full User。公司名 cvtesreops。默认时区 UTC+08:00 Shanghai。邮箱域限制 allow list `cvte.com`，登录邮箱限制开。未出现 Google 存密对话框；OneTrust cookie 对话框 0×0、不挡。

### 2.1 顶栏：8 个下拉 mega-menu

Pass 1 原文：「PagerDuty logo → **8 dropdown menus** → global Search → `+ New Incident` → Support (?) → What's-new gift icon → Account avatar。」菜单是 **click-to-open** 的 mega-menu，列内用 RESPONSE / SETTINGS 分组。

实勘表列出的菜单项（含 8 个产品下拉之外、写在同一张表里的 AI / Support / 头像）：

| 菜单 | 项（href） | 实勘 |
| --- | --- | --- |
| **Incidents** | All Incidents `/incidents`；Alerts `/alerts`；SETTINGS: Incident Types `/incidents/types` | 已访。空表 |
| **Services** | Service Directory `/service-directory`；Business Services `/business-services`；Service Graph `/service-directory/service-graph`；Service Custom Fields `/service-custom-fields` | Directory / Business Services / Default Service 详情已访。Graph / Custom Fields 未点 |
| **People** | My On-Call Shifts `/my-on-call`；Schedules `/schedules-new`；Escalation Policies `/escalation_policies`；Users `/users`（Teams 在 `/teams`） | 已访。0 schedule、0 team、1 user |
| **Automation** | Automation Actions `/automation-actions`；Operations Console `/operations-console` | Actions 已访，整页门控。Operations Console 未点 |
| **AIOps** | Noise Reduction `/reduce-noise`；Event Orchestration `/event-orchestration` | 已访。降噪整页门控；编排可进、0 条 |
| **Analytics** | Dashboard `/analytics/overview-dashboard`；Insights `/analytics/insights`；Event Analytics `/analytics/insights/events`；PagerDuty Advance `/analytics/dashboards/pagerduty-advance`；Operational Maturity `/analytics/dashboards/operational-maturity`；Service Recommendations `/analytics/recommendations` | 只点了 overview-dashboard，全「No data found」 |
| **Integrations** | Extensions `/extensions`；Service Integrations；App Registration `/developer/applications`；API Access Keys `/api_keys`；API Docs（外链）；Generic Webhooks v3 `/integrations/webhooks` | Extensions 已访。其余未点 |
| **Status** | External Status Page `/external-status-dashboard` | 已访，整页 Start a Trial |
| **AI** | AI Settings `/advance`；Usage Analytics | `/advance` 已访，整页门控。Usage Analytics 未点 |
| **Support (?)** | Knowledge Base、Community、PD University、Contact Support（全外链） | 未点 |
| **Account avatar** | My Profile `/users/P7DCOZA`；My On-Call Shifts；Account Settings `/account/settings`；Subscriptions `/account/settings/subscription`；Log Out `/sign_out`（未用） | Profile / Notification Rules / Account Settings 已访 |

**8 与表行数不要强行对齐。** Pass 1 顶栏数的是 8 个下拉；表里把 AI、Support、头像也列进去了。能写死的是：产品面已经按 Incident / Service / People / Automation / AIOps / Analytics / Integrations / Status 切开，AI 另有 `/advance`。

文档 IA（Support KB slug + Developer portal）没有「8 个 mega-menu」这种句子。文档是按对象写的（incidents / alerts / services / escalation-policies / …）。控制台把这些对象收进顶栏下拉，而不是 Flashduty 那种「最左产品轨 + 第二列模块导航」。

### 2.2 空租户里实际有什么

试用过期的空租户不是「什么都没有」，是一套 **出厂默认对象 + 全空列表**。这比文档更能看清最小可运行图。

| 对象 | 控制台实勘 | ID / 备注 |
| --- | --- | --- |
| Service | **Default Service** 一条 | `PATY3M1`。Team 无；On Call Now = xiaowei luo；Last Incident 无；Open 0/0；「1/8 Standards met」环 |
| Escalation Policy | **Default** 一条，挂 Default Service | `PYG7510`，创建 Jan 3 2024。Level 1：立即通知 xiaowei luo / Always on call；repeat **0 times** |
| Schedule | **0** | `/schedules-new` 空态 "Setup An On-call Schedule"；`+ New Schedule` 点击未打开表单（可能门控 / 慢）。顶栏有新 shift-based schedules 横幅（「legacy schedules keep working」） |
| Team | **0** | `/teams/` 空态 "Create a team to organize work" |
| Business Service | **0** | `/business-services` 空态 + `+ New Business Service` |
| User | **1** | Trial (Full User) 1 in use；Trial (Stakeholder) 0；角色 Account Owner |
| Incident | **0** | 空态气球插画 + "No news is good news!" |
| Alert | **0** | "No alerts found" + Clear Filters |
| Integration / Extension / V3 webhook（Default Service 上） | **0 / 0 / 0** | Integrations 页有 Nagios / Sumo / AWS / Dynatrace / Splunk / Datadog logo 的 Add 入口 |
| Orchestration | **0** | `+ New Orchestration` 可见；子 Tab "AI Recommendations" 带锁 |
| 个人值班 | 整月 Always On-Call | `/my-on-call/month`：连续 "Default • Level 1 / Always On-Call" 条；时区 Shanghai |

**最小可运行图（本租户证实）：** Service → 一条 EP → Level 1 直接打到 User，**没有 Schedule、没有 Team**。文档写「Schedule 不挂到 EP 就不 page」（[shift-based-schedules](https://support.pagerduty.com/main/docs/shift-based-schedules)）；控制台把这句话做成了出厂捷径——Default EP 的 Level 1 目标就是人，"Always on call"，右栏卡片写 "You're always on-call"。

Create-Service 向导 `/service-directory/new` 三步（1 Name，2 Assign，3 Integrations）。Step 1：Name*（tip：名字里避免 "PagerDuty" / "Alerts"）、Description；Next 在空名时 disabled。Pass 1 取消，未保存。

### 2.3 试用过期门控

蓝条不挡点击，但整页 / 锁图标 / 行内 upsell 把付费面包起来。Pass 1 未打开定价页，**不写现价**。

| 区域 | 状态 | 出处 |
| --- | --- | --- |
| Incidents / Alerts / Services / EPs / Schedules / Teams / Users / Profile | 可看 | 控制台 |
| Analytics Dashboard `/analytics/overview-dashboard/<hash>` | 能加载；维度 Time / Services / EPs / Teams / Responders；全 "No data found"；日期范围 **Aug 14 2199–Aug 21 2199**（异常默认，原文如此）；有 Share + View report | 控制台。不解释 2199，不编成「产品设计」 |
| AIOps → Noise Reduction | 整页 "Alert Grouping … Start Trial" | 控制台 |
| Event Orchestration | 能进，0 条；AI Recommendations 锁 | 控制台 |
| Automation Actions | 整页 "Runbook Automation … Contact Sales" | 控制台 |
| Status → External Status Pages | 整页 "Start a Trial" | 控制台 |
| AI → `/advance` | 整页 "Take a Tour / Contact Sales" | 控制台 |
| Incident Types → 自定义类型 | 锁 + Contact Sales；内容区无限转圈 | 控制台 |
| Account Settings → Products Trials | 当前计划 **没有资格** 开 Advance / AIOps 试用 | 控制台 |
| Service settings：alert grouping、Custom Incident Actions | 禁用 + 行内 upsell | 控制台 Default Service `/settings` |
| **New Incident 对话框** | **本状态不可用** | 控制台，见下 |

Users 页 license 计数器：「Trial (Full User) 1 in use」「Trial (Stakeholder) 0 in use」。Account Limits：Full User limit 1。Account Settings 可编辑表单（未保存）：Company Name cvtesreops、Default Time Zone UTC+08:00 Shanghai、隐藏联系电话 / 邮箱勾选、Language、Change Account Owner、Email Domain Restriction（allow list `cvte.com`）、Need to cancel。

### 2.4 New Incident 没打开——字段清单不可用

这是 Pass 1 的硬缺口，后面所有「声明 Incident 的表单有哪些字段」都只能走文档，不能假装看见过。

- 列表页 CTA 是 `<a class="btn open-web-trigger-form">`，**没有 href**。点击无 modal。Pass 1 判断：legacy JS trigger form 在过期试用上坏了或被门控。
- `/incidents/new` **重定向回** `/incidents`，红闪 **"Incident Not Found"**。
- 因此：手建 Incident 的字段库存（title、service、urgency、priority、conference、incident type、custom fields…）**本租户拿不到**。文档侧：Web / REST 手建不产生 Alert；自指派的手建 Incident 初始就是 Acknowledged，**不通知创建者**（[incidents](https://support.pagerduty.com/main/docs/incidents)）。

### 2.5 已访 URL（未登出、未创建、未保存）

`/incidents`（带 status 过滤）、`/incidents/new`（→ 重定向）、`/alerts`、`/incidents/types`、`/service-directory`、`/service-directory/PATY3M1`（含 `/integrations`、`/settings`）、`/service-directory/new`（取消）、`/business-services`、`/escalation_policies`、`/escalation_policies/PYG7510`、`/schedules-new`、`/my-on-call/month`、`/teams`、`/users`、`/users/P7DCOZA/profile`、`/users/P7DCOZA/notification-rules`、`/automation-actions`、`/reduce-noise`、`/event-orchestration`、`/analytics/overview-dashboard`、`/extensions`、`/external-status-dashboard`、`/advance`、`/account/settings`。

文档有、Pass 1 **没点** 的：Operations Console、Service Graph、Service Custom Fields、Generic Webhooks v3 管理页、API Keys、App Registration、Insights / Event Analytics / Advance Analytics、Internal Status Page（菜单里只看到 External）。

### 2.6 和文档 IA 的差

- 文档把 Escalation Policies 和 Schedules 写成两篇；控制台 `/escalation_policies` 是共享页 **"Escalation Policies & Schedules"**，顶 Tab = My On-Call Shifts | Escalation Policies | Schedules。People 菜单把这三块和 Users 放在一起，Teams 却走 `/teams`（菜单括号补记）。
- 文档 Mobile 底栏是 Home / Incidents / My Shifts / Services / More（[mobile-app](https://support.pagerduty.com/main/docs/mobile-app)）。Web 顶栏是 8 mega-menu，不是底栏五格。不要把移动 IA 抄到 Web。
- Analytics 在过期试用上仍能打开空看板；Flashduty 把 Usage 留给 Free、Insights 门控 Standard（`flashduty-analysis.md` §2.3）。两边都把「计费用量 / 空看板」和「运营洞察」劈开，只是 PagerDuty 这次空看板的日期默认值是 2199——以控制台原文为准，不做产品解释。

---

## 3. 对象模型：Event → Alert → Incident，Service + EP + Schedule + Team

这是整张产品的脊柱。文档写成（dump §1）：

```
event → alert → incident → 人（经由 service + escalation policy + schedule）
```

前面可以插 Event Orchestration，后面可以挂 Incident Workflows。控制台空租户证实的所有权方向是：**Service 拥有 Incident 和 Integrations；Service 必须挂恰好一条 EP；EP 的 Level 可以打到 User 或 Schedule；Team 是可选的 ACL / 过滤层，本租户为 0。**

### 3.1 Event / Alert / Incident

| 对象 | 定义 | 谁创建 | 状态 | 关键键 |
| --- | --- | --- | --- | --- |
| **Event** | 上游一次 trigger / ack / resolve（或邮件、编排改写后的 CEF） | 监控、Events API、email、编排 | 不是一等列表对象 | `routing_key` + 可选 `dedup_key`（≤255） |
| **Alert** | 合格事件触发的告警，再触发 Incident | **只能** Events API / 基于 Events API 的集成。**不能手建 Alert** | 只有 **triggered / resolved**（另有 suppressed / suspended 在编排语义里） | 同 `dedup_key` 在未解决期间并进原 Alert |
| **Incident** | 「需要被处理和解决的问题」。**必须派给一个用户** | Alert 触发，或 Web / REST 手建 / 声明 | triggered / acknowledged / resolved | 标题初始 = 首条 Alert 标题；之后合入的 Alert **不改** 标题 |

来源：[incidents](https://support.pagerduty.com/main/docs/incidents)、[alerts](https://support.pagerduty.com/main/docs/alerts)、[Events API trigger](https://developer.pagerduty.com/docs/events-api-v2/trigger-events/index.html)。

硬规则（文档，控制台空租户无法打事件来核）：

- 没人 on-call → **trigger 事件被丢弃，不可审计**（[incidents](https://support.pagerduty.com/main/docs/incidents)、[alerts](https://support.pagerduty.com/main/docs/alerts)）。覆盖缺口是静默的。
- 同一 `dedup_key` 超过 **100** 次 trigger 后，Incident 不再收新 trigger；Alerts Log 停更；先 resolve 才能再送。
- 每条 Incident 最多 **1,000** 条 Alert（合并时合计超限则拒）；最多 **100** 次 acknowledgement；最多加 **1,000** 个 responder；最多 **2,000** 条 note，单条 **65,535** 单字节字符（约 16k UTF-8）。
- 服务上 **>100,000** 条未关 Incident → 强制 24 小时自动 resolve。
- 手建 / REST Incident **不生成 Alert**，编排 / 抑制 / 分组都不适用。
- 解决后再用同一 `dedup_key` trigger → **新** Alert / Incident，不复活旧的。Reopen 现为 GA（H2 2025 博客）。
- Account Owner 可 **Redact** Incident（永久；删描述和 incident key；分析指标保留）。

控制台能确认的：`/incidents` 和 `/alerts` 是两个列表，列不同（Incident 有 Urgency / Type / Assigned To；Alert 有 Severity / Integration / Source）。空租户没有详情页可开。Alerts 表承认「有人要直接看告警」，默认工作面仍是 Incident。

### 3.2 Service

Service 是收事件、拥有恰好 **一条** EP、长 Incident 的单元。设置控制指派、urgency、ack timeout、auto-resolution、AIOps、alert grouping、auto-pause、conference bridge、文档链接、workflows（[configurable-service-settings](https://support.pagerduty.com/main/docs/configurable-service-settings)）。

| 设置 | 行为 | 出处 |
| --- | --- | --- |
| Assign to escalation policy | 每服务一条 EP | 同上 |
| Notification urgency | High（升级）/ Low（不升级）/ Dynamic（跟 alert severity）/ Support hours | 同上 |
| Acknowledgement timeout | 默认关。Ack 后 N 时间再 trigger 并再通知 | 同上 |
| Auto-resolution | 可选，N 后关开着的 Incident。Snooze **重置**计时。只作用于开启后新建的 | 同上 |
| Forced auto-resolve | >100,000 开着 → 24h | 同上 |
| Alert grouping | Intelligent / Alert Content / Time only（AIOps） | 同上 |
| Auto-pause transient alerts | 2 / 3 / 5 / 10 / 15 分钟（AIOps / Digital Ops / EIM） | 同上 |
| Conference bridge | 每条 Incident 带拨入 + 会议 URL | 同上 |
| Documentation link | Remediate 下的 runbook URL | 同上 |
| Incident Workflows | 触发且条件匹配时跑 | 同上 |
| AIOps on/off | 关则吞吐掉到 120 ev/min，关 grouping / RCA 小组件 | 同上 |
| Custom incident actions | 在 PagerDuty 外执行自定义逻辑 | 同上 |

**Main Service vs Impacted Services：** Main 是被指派的技术服务（可改派）；Impacted 来自 grouping / merge / reassignment，不能手改。改派之后，grouping 仍用 **起源** 服务的分组设置（[edit-incidents](https://support.pagerduty.com/main/docs/edit-incidents)）。

控制台 Default Service `PATY3M1` 详情（`/service-directory/PATY3M1`）：

- 面包屑 Service Directory › Default Service › Activity。
- 页头：名 + 行内 Edit 铅笔、描述、`+ New Incident`、`More ▾`。
- 摘要条：Status、On Call Now、Escalation Policy（Default）、Communication Channel（none，"Add one"）、Standards 环 1/8。
- Tab：**Activity**（Open Incidents 表 + Merge Incidents；Recent Changes 带 AIOps upsell；Resolved last 90 days；Suppressed Alerts；Maintenance Windows）/ **Integrations**（0 集成、0 extensions、0 V3 webhooks）/ **Settings**（Assign and Notify → EP + urgency 行为 + re-trigger / auto-resolve；Reduce Noise → alert grouping 禁用 + "Preview" upsell；Remediate → documentation link，Custom Incident Actions 禁用）/ **Service Dependencies**。

Service Directory 列表：Tabs Services | Maintenance Windows；过滤 text / Business Service / Last Incident；Sort Service name A–Z；Export；`+ New Service`。一行服务是一张卡，不是纯表——活计数和 Standards 环已经在列表上。

Business Services 在本租户为 0。文档里 Stakeholders 可通过 business-service subscription 订阅（[business-service-subscription](https://support.pagerduty.com/docs/business-service-subscription)）。控制台未打开订阅 UI。

### 3.3 Escalation Policy

EP **自动化指派**，把 Service 连到 user 和 / 或 schedule。设计目标：通知某个目标直到有人 ack。**一个 Service 只能有一条 EP；一条 EP 可以挂很多 Service**（[escalation-policies](https://support.pagerduty.com/main/docs/escalation-policies)）。

工作方式（文档）：

1. Incident 触发 → **立刻**通知第一级目标。**第一下 page 不能在 EP 上延迟。**
2. 响应者有 **escalation timeout** 去 ack / resolve / reassign。
3. 超时 → 下一级。
4. Ack **停**升级和继续通知。Ack 不等于 resolve。
5. 第一级没人 on-call → **跳过**到下一级有人的规则。
6. 整条政策最多 **repeat 9 次**。最后一轮之后，Incident 停在最后那个用户上，循环完联系方式后停通知。
7. 开着的 Incident 用的是触发时的 **EP 快照**；事后改 EP 不影响，除非把 Incident 重新指派到同一条 EP（新快照）。

| 限制 | 值 |
| --- | --- |
| 每条 EP 的规则数 | 20 |
| Repeat | 9 |
| 每级多用户目标 | 50（Business / Digital Ops / EIM）；5（Professional / Free） |
| 最小 timeout，1 个目标 | 1 分钟 |
| 最小 timeout，多目标 | 3 分钟 |
| 默认 timeout | 30 分钟 |
| Free | 1 条 EP |
| Team | 一条 EP 只能属于 **一个** Team |

可选 per-rule round-robin（账户有 Round Robin Scheduling 时）。On-call handoff notifications（OCHON）：最多提前 48 小时；条件：用户有 OCHON 规则、用户是 EP 目标、EP 被未禁用 Service 使用 **或** EP 设为 always 发 OCHON。

控制台 Default EP `PYG7510`：文案 "Send On-Call Handoff Notifications: when in use by a service"；Level 1 卡 "Notify the following user immediately → xiaowei luo / Always on call"；"If no one acknowledges, repeat this policy **0 times**"；右侧 Services (1)。这是文档模型的最小实例：一级、直接打人、不重复。

### 3.4 Schedule

两种调度模型（文档）：

**A. Shift-based schedules（现行；文档写 Summer 2026 推向全部账户）**  
由 **named rotations** 组成。每个 rotation：活跃日、活跃时、handoff、指派类型（轮转成员 **或** 全员同时 on-call）。类型：Hourly / Daily / Weekly / Monthly。模板：Weekly Handoff、Workday/Weekend、Follow the Sun。Custom shifts（一次性改模式）vs Overrides（换人）。未指派 rotation 可被 claim。支持未来生效的 rotation 更新。导出 WebCal 或 `.ics`（我的班或全员）。Free：1 个 schedule。  
来源：[shift-based-schedules](https://support.pagerduty.com/main/docs/shift-based-schedules)。

**B. Legacy schedules**  
由 **layers** 组成。最后创建（最底）层优先。同一时刻只有一人 on-call。Layer 有 daily / weekly / custom、handoff、restrictions。Override 在 Final Schedule 之上另成一层；回退 layer 改动不会回退 override。  
来源：[schedule-basics](https://support.pagerduty.com/main/docs/schedule-basics)、[edit-schedules](https://support.pagerduty.com/main/docs/edit-schedules)。

H2 2025：Flexible Shifts（EA）——无 layer 的重复班、iCal、每班多人、shadow schedules、Google Calendar PTO 冲突、重叠班（[product-launch-2025-h2](https://www.pagerduty.com/blog/product/product-launch-2025-h2/)）。

**关键接线：Schedule 不 page 任何人，直到它挂到一条挂在 Service 上的 EP。**（[shift-based-schedules](https://support.pagerduty.com/main/docs/shift-based-schedules)、[escalation-policies-and-schedules](https://support.pagerduty.com/main/docs/escalation-policies-and-schedules)）

控制台：`/schedules-new` 0 条；新 shift-based 横幅；创建表单未打开。My On-Call 日历却画满了 "Default • Level 1 / Always On-Call"——因为 EP 直接打人，不需要 Schedule 对象。这是文档接线规则的反面例证：没有 Schedule 也能永远 on-call。

### 3.5 Team

Team 用来分组并 ACL PagerDuty 对象（EP、user、schedule、service），并过滤 Web / Mobile UI（[teams](https://support.pagerduty.com/main/docs/teams)）。

| 事实 | 细节 |
| --- | --- |
| 可用性 | Business / Digital Ops / EIM 不限。Professional：**2** 个 Team |
| 上限 | 每 Team 100 条独特 EP；每 Team 500 用户；用户最多 400 个 Team |
| EP 关联 | 把 EP 加进 Team 会自动带上关联用户、schedule、**以及 services**。一条 EP 只能在 **一个** Team |
| 隐私 | Public（默认）或 Private（Advanced Permissions）。Private 对非成员隐藏，Global Admin / Account Owner 除外。**不**作用于 Incident Workflows、Post-Incident Reviews、Analytics |
| Primary Team | 可选，为了计费 |
| Incident 的 Team 归属 | 按 **触发时的 Service**。改派到另一 Team 的 EP / user **不改** Incident 的 Team |
| 过滤语义 | Incidents 列表跟踪该 Incident **曾经**关联过的每个 Team；Insights 按 **当前** Service 的 Team 过滤 |

控制台 `/teams/` 为 0。Users / Service 行上都看得到 Team 列（空）。文档「加 EP 进 Team 会把 Service 一起带上」在空租户上无法核。

### 3.6 控制台看见的嵌套

Pass 1 原话：「Service → Escalation Policy → Level(s) → target (user directly, or schedule)。Team is optional grouping over users/services/EPs。Business Services sit above technical services；Service Graph / Dependencies express relationships。」

和 Flashduty 的差（观察，不是官方对照表）：Flashduty 的 **Channel** 同时拥有集成、降噪、分派和活计数，分派规则挂在 Channel 内、命中即停（`flashduty-analysis.md` §3）。PagerDuty 把「管道」拆成 **可复用的 EP** + **拥有事件的 Service** + **可复用的 Schedule** + **人身上的通知规则**。控制台 People 菜单把 EP / Schedule / User 放在一起，Services 菜单另走，就是这张拆法的 IA。

---

## 4. 生命周期：triggered / acked / resolved，urgency vs priority vs severity

### 4.1 状态机

```
(event) → Triggered ⇄ Acknowledged → Resolved
                ↑            │
                └─ ack timeout / unack / snooze expiry
```

来源：[incidents](https://support.pagerduty.com/main/docs/incidents)、[Events API](https://developer.pagerduty.com/docs/events-api-v2/trigger-events/index.html)、[H2 2025](https://www.pagerduty.com/blog/product/product-launch-2025-h2/)。

| 状态 | 含义（文档） |
| --- | --- |
| **Triggered** | 事件打到一个 **active** 的 Service（有人 on-call；服务未禁用 / 未在 maintenance）。按 EP 升级。默认：只在 trigger 时通知，ack / resolve 不通知（除非个人 notification rules 或 webhooks） |
| **Acknowledged** | 有人认领所有权；升级 **停下**。通知暂停直到 **acknowledgement timeout**。超时 → 回到 triggered，升级恢复 |
| **Resolved** | 修好了。不再通知。可 reopen（GA）。新 trigger + 同一 `dedup_key` 开 **新** Incident，不自动 reopen |
| **Unacknowledge** | 回到 Triggered，通知恢复 |

**Trigger vs declare：** Triggered = 集成 / 事件自动创建。Declared = Web / API 手建。初始状态都是 Triggered——唯一例外：自指派的手建 Incident 从 Acknowledged 起，且不通知创建者。

Alert **没有 ack**：只有 triggered | resolved。Resolve 全部 Alert → Incident resolve。Resolve Incident → 全部子 Alert resolve。Snooze / ack 发生在 Incident 上。

控制台 `/incidents?status=acknowledged,triggered,resolved`：状态是 **URL query，逗号编码**。Tab：Open | Triggered | Acknowledged | Resolved | Any Status。范围："Assigned to me" / "All"。时间 "Last 30 days"。KPI："Your open incidents (Last 30 days)" 和 "All open incidents (Last 30 days)"，链到 `0 triggered / 0 acknowledged`。批量条（无选择时全 disabled）：Acknowledge、Reassign、Resolve、Snooze（分裂下拉）。右："Go to incident #…" 跳转盒。列：[checkbox]、Status、Urgency（漏斗过滤）、Type、Title、Created（可排序）、Service、Assigned To。每页 25 + 前后分页。

Alerts `/alerts` 列：[cb]、Status、Severity、Summary、Created（排序+过滤）、Incident Title、Service、Integration、Source；chip "All Teams"、"[Created At] from Last 30 days"；每页 10。

这些列本身就是对象模型的 UI 投影：Incident 行强调 Urgency / Type / Assignee；Alert 行强调 Severity / Integration / Source。空租户没有详情时间线可核。

### 4.2 Urgency vs Priority vs Severity

| 概念 | 挂在 | 取值 | 作用 |
| --- | --- | --- | --- |
| **Urgency** | Incident | High / Low | **怎么被通知**（用哪一套 notification rules）。High：按需升级。Low：**不自动升级**，也不能手动升级（必须先把 urgency 调高） |
| **Priority** | Incident | 客户自定义（P1、Sev-1…） | 做事顺序 / 状态页 / 工作流条件。可手设、REST、或 Event Orchestration |
| **Severity** | Alert / event | `critical`、`error`、`warning`、`info` | Events API payload。可经服务的 Dynamic notifications + 编排，把 urgency 从 severity 推出来 |

来源：[incidents](https://support.pagerduty.com/main/docs/incidents)、[configurable-service-settings](https://support.pagerduty.com/main/docs/configurable-service-settings)、[Events API](https://developer.pagerduty.com/docs/events-api-v2/trigger-events/index.html)。

**Support hours：** 每服务的日 / 时 / 时区；班内班外不同 urgency；可选「班开始时把未 ack 的抬成 high」。**不阻止**班后创建 Incident。手动 / API 设的 urgency 覆盖服务默认。

**Low → High 的陷阱：** ack 之后把 Low 改成 High，Incident 回到 **unacknowledged**，high-urgency 分页恢复（[edit-incidents](https://support.pagerduty.com/main/docs/edit-incidents)）。

控制台 Incident 表有 Urgency 列和漏斗；Alert 表有 Severity 列。Priority 列 **未出现在 Pass 1 的 Incident 表头摘录里**——不要写成「控制台没有 Priority」，只是空表的默认列没摘到。Incident Types 页能进但自定义类型锁着、内容转圈。

内部手册的 SEV-1…SEV-5（[response.pagerduty.com](https://response.pagerduty.com/)）**不是**产品里这三个字段。

### 4.3 Reassign / Snooze / Merge / Responders / Conference

**Reassign**（文档，控制台批量条有按钮、空表 disabled）：

- 把 Incident 改派给另一 user 或 EP（覆盖该 Incident 上的服务 EP）。
- **Reassign service：** 已 ack 的回到 Triggered；若新服务 EP 不同，assignee 换成新 EP 的 on-call。Roles / tasks / custom fields / workflows 不变。Grouping 仍跟 **起源** 服务。时间线记旧服务 + actor。Jira Cloud/Server 或 Zendesk sync 规则不可用。
- 加 responder：选 user 和 / 或 EP。Snooze **不停**「EP 作为 responder」的升级。

**Snooze：** 只对 **已 ack** 的 Incident。预设 1h / 4h / 8h / 24h / Other（最大 **168 小时 / 1 周**）。到期 → Triggered；若轮转已变，**旧和新** on-call 都被通知。改派或另一人 ack **取消** snooze 并重置 ack timeout。Snooze **重置 auto-resolution 计时**；若 snooze 越过 deadline，会保持 snooze，然后再开计时。Snooze ≠ 编排里的 suppress / pause（那些在 Incident 出现之前）。来源：[edit-incidents](https://support.pagerduty.com/main/docs/edit-incidents)。

**Merge：** 目标必须 **open**。Alert 移到目标；源以原因 **Merged** resolve 并留指针。Dedup key 跟着目标（本身不变）。**Responders 和 subscribers 不合并**——要重加。双向聊天显示源已 resolve。Webhook：`resolve_reason.type = merge_resolve_reason`。之后的 webhook 只打 **目标服务**。上限 1,000 alerts。来源：同上。

**Responders：** 在已开 Incident 上加 user / EP（超出指派 EP）。邀请走被邀请人的 **high-urgency** 规则；Accept / Decline。上限 1,000。非 assignee 的人 ack → Incident **改派给他**；原 assignee 不收状态通知。[incidents](https://support.pagerduty.com/main/docs/incidents)、[notification-rules](https://support.pagerduty.com/main/docs/notification-rules)。

**Conference：** 服务级默认拨入 + URL；声明时可带；工作流动作 Zoom / Meet / Teams / generic；webhook `conference_bridge.updated`；Advance 的 Scribe Agent 把 Zoom 转写到 Slack / Teams。控制台 Default Service 摘要条 Communication Channel = none。

### 4.4 Timeline / notes / types / custom fields

文档（控制台未开详情，以下全是文档）：

- **Timeline：** 每次状态变化、动作、通知；可过滤（[incidents](https://support.pagerduty.com/main/docs/incidents)）。
- **Notes：** 每 Incident 最多 2,000；65,535 单字节字符。Web / mobile / workflows（`Add Note`、Slack prompt）/ Event Orchestration（Advanced；作者 = "An Orchestration Event Rule"）。改 custom field 也进时间线（[edit-incidents](https://support.pagerduty.com/main/docs/edit-incidents)）。
- **Incident Types：** 继承树，最多 **3** 层。默认：Base Incident（全部）、Major Incident、Security Incident（当前 Free / Pro / Business / Enterprise）。自定义：Business **3**；Enterprise **100**。不能删，只能停；必须先停子类型。API Name 不可变（`[a-z0-9_]+`）。最多 5 个 type trigger 工作流（[incident-types](https://support.pagerduty.com/main/docs/incident-types)）。控制台 `/incidents/types`：左栏 + "New Incident Type"（锁）；横幅升级 / Contact Sales；内容无限转圈。
- **Custom Fields：** 挂在 type 上、子类型继承。类型：Text、Single/Multi Select、Tag、URL、Checkbox、Datetime、Decimal、Integer、Paragraph。Business：Base 上 15 / 总共 45；EIM：Base 上 100 / 总共 130；**每 type 最多 15**。停用的仍计数。删除全局且破坏性。默认值不回填旧 Incident。编排只能在 **新** Incident 上设字段（[custom-fields-on-incidents](https://support.pagerduty.com/main/docs/custom-fields-on-incidents)）。
- **Required Fields on Resolve** — EA，IM / CSOps Enterprise（H2 2025）。

Flashduty 把人态（Triggered / Processing / Closed）和监控态拆成两轴（`flashduty-analysis.md` §4）。PagerDuty 的三态是 **人的所有权状态**；监控恢复走 Events API `resolve`（或 resolve 全部 Alert）。没有单独叫 Processing 的进度；ack = 认领所有权。任何人 ack 都会把 Incident 改派到自己（文档）。这和 Flashduty「看见就能认领、全部取消认领回退」不同——材料没有 PagerDuty「取消认领回 Triggered 但仍派给原 EP」的对等句，只有 Unacknowledge / ack timeout。

---

## 5. 通知规则（个人规则、delay、ack timeout、bypass DND、电话 / 短信 / 推送）

通知是 **按人的规则**，不是按服务的（服务只选 **urgency**）。渠道： **push、phone、SMS、email、Slack**。WhatsApp 为 EA（H2 2025）。日语和加拿大法语语音为 GA。

来源：[notification-rules](https://support.pagerduty.com/main/docs/notification-rules)、[configurable-service-settings](https://support.pagerduty.com/main/docs/configurable-service-settings)、[H2 2025](https://www.pagerduty.com/blog/product/product-launch-2025-h2/)。

### 5.1 规则类型

| 规则 | 用途 |
| --- | --- |
| Invited as responder | 走 **high-urgency** 规则；Accept / Decline |
| High-urgency assigned | Delay（分钟，默认 0）+ 联系方式；可重复 |
| High-urgency **changes** | 在已经收过初始 high-urgency 通知的渠道上发 Ack / Resolved / Escalated。自己 ack / resolve 不通知；delay 还没到状态就变了也不通知 |
| Low-urgency assigned | 另一套更安静（或空）的规则 |
| Subscribed status updates | 立刻；无 delay |
| On-call / off-call（OCHON） | 仅 push 或 email；提前小时数；最多 48h |
| Shift Agent | Slack；PTO 冲突 / 要人顶班 |

谁能改：用户改自己的；Admins / Global Admins 改除 Account Owner 外所有人；Owner 改所有人。必须先有 contact method。

**删掉通知规则不会取消指派**——只是你收不到。休假用 **schedule overrides**。

### 5.2 Delay vs escalation timeout vs ack timeout

| 计时器 | 在哪 | 含义 |
| --- | --- | --- |
| Notification **delay** | 用户规则 | 指派后等 N 分钟再打这个渠道。0 = 立刻。文档建议错开：push 0、phone 1、Slack 2、email 3、SMS 4 |
| **Escalation timeout** | EP 规则 | 当前级多久不动作就到下一级。**不延迟第一下 page**。若 delay > timeout 且同一人在下一级，仍可能收到迟到的第一级通知 |
| **Ack timeout** | Service | Ack 之后若仍开着，再 trigger + 再通知。默认关 |
| **Auto-resolve** | Service | N 后关。Snooze 重置 |
| **Snooze** | Incident | 已 ack 上暂停再通知（最大 7 天） |
| **Pause / suppress** | Orchestration | Incident 出现前：挂起 Alert N 秒，或永远不建 Incident |

**没有「延迟 EP 第一下通知」的设置。** 第一级目标在 Incident 触发的那一刻被 page（[escalation-policies](https://support.pagerduty.com/main/docs/escalation-policies)）。

这是和 Flashduty Delay window（0–3600 秒，窗内关闭则不通知，挂在 **分派策略** 上）最硬的设计差。PagerDuty 把「别那么快打电话」放在 **人** 身上；Flashduty 把它放在 **Channel 策略** 上（`flashduty-analysis.md` §6.2）。

### 5.3 渠道可靠性与 bypass DND

文档：push **99.99%** 可靠（Apple / Google），启用 push 时「6× faster MTTA」，建议 push 做 0 分钟规则。Phone / SMS 受运营商和本地监管。电话 **2 分 30 秒切断**。

Bypass 静音 / DND：

- iOS **Critical Alerts** 给 high-urgency push。
- **Emergency Bypass** / PagerDuty 去电号码 vCard 给 phone / SMS。
- Android **Override System Volume** + Sound Looping。
- App **不能**覆盖电话 / 短信音量。

来源：[notification-rules](https://support.pagerduty.com/main/docs/notification-rules)、[push-notification-troubleshooting](https://support.pagerduty.com/main/docs/push-notification-troubleshooting)、[phone-notification-troubleshooting](https://support.pagerduty.com/main/docs/phone-notification-troubleshooting)、[mobile-app-settings](https://support.pagerduty.com/main/docs/mobile-app-settings)。

Mobile：iOS 17+ / Android 12+。FedRAMP：无 root / 越狱 / 模拟器。多账户登录（非 Intune）。High-urgency 预设：Never Miss a Page / Always Text First / Always Call First。App Lock（Business / Digital Ops / EIM）：PIN / 生物识别，管理员超时。能力（文档）：declare / ack / snooze / resolve / merge / reassign / 改 service / urgency / priority / type / notes / status updates / 加 responders / escalate / 跑 workflows / Automation Actions / overrides / 订阅 business services。控制台是 Web，没有核过 App。

### 5.4 控制台实勘：`P7DCOZA` 的通知规则

`/users/P7DCOZA/notification-rules` 分段（Pass 1 原文，6 个 section）：

| 段 | 本用户实况 |
| --- | --- |
| High urgency incident notifications | **1 条：immediately，html email** |
| invite-to-respond note | 有说明段（未摘具体文案） |
| High urgency incident state changes | **无** |
| Low urgency notifications | **1 条 email** |
| On-call / off-call notifications | **2 条：immediately，以及 1 day before** |
| Shift agent notifications | **挡住**："You need to add Slack as a contact method" |

每行有 edit / duplicate / delete 和 `+` 添加。Profile 时区 UTC+08:00 Beijing；Voice & Interface Language English US。Audit Trail Reporting 灰掉。

这是出厂最小集：高低 urgency 都只靠 email、立刻；换班通知有立刻 + 提前 1 天；规则页看见的渠道是 email。Pass 1 没打开 Contact Information 子页，所以不写「没有配手机」——只写规则页看到的。Shift Agent 被 Slack 联系方式门控，和文档「Shift Agent = Slack」一致。

右栏 Incidents 页的 "ON CALL NOW" 卡：Default EP、Level 1、xiaowei luo、on-call all the time。个人规则再弱，EP 仍会指派——文档「删规则 ≠ 取消指派」在空租户上的静态对应。

---

## 6. Event Orchestration vs Incident Workflows vs AIOps grouping

三层自动化，速度和对象都不同。比较「PagerDuty 的自动化」必须先说是哪一层（文档 dump §7 第 7 条）。

| 层 | 何时跑 | 改什么 | 典型动作 | 计划门槛 |
| --- | --- | --- | --- | --- |
| **Event Orchestration** | 事件刚进、Incident 可能还不存在 | 原始 / CEF payload、路由、是否建 Alert | enrich / suppress / pause / **drop**（仅 Global，不可审计）/ 设 priority / dynamic EP / 改 `dedup_key` / 设 severity / 强制 trigger 或 resolve / webhook / Automation Actions | Global Orchestrations = AIOps + Advanced EO；Routing / Service Orchestrations = 全套餐。AI Recommendations = 锁（本租户） |
| **AIOps grouping** | Alert 已建、往 Incident 上合 | 多 Alert → 一 Incident | Intelligent / Content / Unified / Time / Global（跨服务，仅 AIOps）；Auto-pause 2–15 min | 本租户 Noise Reduction 整页 Start Trial；服务设置里 grouping disabled |
| **Incident Workflows** | Incident **已经存在** | 人速协同：聊天、状态页、工单、字段、再开 Incident | 顺序动作；`{{incident.*}}` 引用；Zoom / Slack / Jira / Jeli PIR / Automation Actions / SRE Agent 虚拟响应者（EA） | Professional **1** 条启用；Business / Enterprise 最多 2,000 |

### 6.1 Event Orchestration

现代替代 Global / Service Event Rules。规则 **迭代** 求值：Global Orchestration → Routing → Service Orchestration。最后一条匹配的离散动作赢。条件可匹配原始或 PD-CEF；条件值 **大小写不敏感**（[event-orchestration](https://support.pagerduty.com/main/docs/event-orchestration)）。

| 种类 | 可用性 | 角色 |
| --- | --- | --- |
| Global Orchestrations | AIOps + Advanced Event Orchestration | 路由前的公共动作；可 drop |
| Global Integrations | 全套餐 | 把多服务流先路由，再进 Service Orchestration |
| Service Orchestrations | 全套餐 | 事件落到服务之后的动作 |

规则类型：

- **Global Orchestration rules** — enrich / suppress / pause / drop / 设 priority / dynamic EP / notes / fields。**不能路由。**
- **Routing rules** — 每个 orchestration **每个服务一条** 路由规则。条件、周历、日期范围、或事件频率（Advanced）。Dynamic Routing（AIOps）用 RE2 抽服务名 / ID。Catch-all：suppress（默认）或送到某服务。每 orchestration 最多 **1,000** 个服务。
- **Service Orchestration rules** — 每服务一块画布。嵌套 set，每 ruleset 最多 **25** 条（允许嵌套，不鼓励复杂）。
- **AI Orchestrations** — 手写规则之后的 ML 建议规则（H2 2025 起作为独立段文档化，EA forthcoming）。

其它事实：每 orchestration 最多 **10** 把 integration key；保存 payload 上限 **4 MB**（更大 413）。**打到服务级 email 集成的邮件忽略 Service Orchestration**（只用 email filters / rules）；打到 Global Orchestration 邮箱的邮件走 Global 再走 Service。从 Event Rules 切到 Service Orchestration 可逆。按 orchestration 开关 AIOps；关则该 key 吞吐回到 **120 ev/min**，跳过 Global / Dynamic routing。

控制台 `/event-orchestration`：能进，0 条，`+ New Orchestration`；AI Recommendations 锁。画布、动作目录、integration key **未打开**，属文档。

### 6.2 Incident Workflows

「if-this-then-that」，**不是**事件路由（[incident-workflows](https://support.pagerduty.com/main/docs/incident-workflows)、[actions overview](https://support.pagerduty.com/actions/docs/incident-workflow-actions-overview)）。

| Trigger | 谁 | 备注 |
| --- | --- | --- |
| Incident Type | Admin / Owner | 创建 / 设 type 时自动。最多 **5** 个 type trigger。作用于所有服务 |
| Conditional | Business / EIM | 创建时，或条件从 false→true。可再触发 |
| Manual | Business / EIM | Web、mobile、Slack、MS Teams |
| API | Manager+ | REST |

|  | Professional | Business | Enterprise |
| --- | --- | --- | --- |
| Workflows | **1** 条启用 | 最多 2,000 | 最多 2,000 |
| Triggers | Incident Type | + manual、priority / status / urgency、单选 custom fields | 全部字段含 custom fields |
| Conditional / service | — | 100 | 100 |
| Conditional / all-services | — | 50 | 50 |

动作顺序执行。字段引用 `{{incident.*}}`、`{{incident.custom_fields.NAME}}`、`{{steps['Action Name'].fields['Field']}}`。草稿不跑；发布替换线上。账户级：条件 trigger 共 **5,000 incident events/min**；Web API 动作 **500 MB/min**；单动作响应上限 **5 MB**。HTTP 出口走 webhook IP 列表。支持 Terraform + REST。

控制台 Automation 菜单里 **没有**「Incident Workflows」这一项被 Pass 1 摘到。不要写成「控制台没有工作流」——可能在服务 Settings、More 菜单或未点的入口。本材料只能写：菜单实勘没列它；文档它是一等对象。

### 6.3 AIOps grouping

降噪层（[aiops](https://support.pagerduty.com/main/docs/aiops)）：

- Intelligent Alert Grouping（ML，滚动窗 5 分钟–1 小时；regroup 训练模型）
- Content-Based（所选字段 / custom details 精确匹配）
- Unified（Intelligent + Content + 灵活窗口）
- Time-Based（2 分钟–24 小时或直到 resolve）
- **Global Alert Grouping**（跨服务、基于内容）— 仅 AIOps
- Auto-Pause transient alerts（2–15 分钟）
- Alert Suppression（编排）

另有 RCA 小组件：Probable Origin、Related Incidents、Outlier Incident、Past Incidents、Recent Changes。控制台 Default Service Activity 的 Recent Changes 带 AIOps upsell；Noise Reduction 整页门控。服务 Settings 里 "Reduce Noise → alert grouping disabled, Preview upsell"。

**和 Flashduty 降噪的结构差（观察）：** Flashduty 把事件聚合 / 告警聚合 / 抖动 / 静默 / 抑制挂在 **Channel Configuration**，排除放 Integrations（`flashduty-analysis.md` §5）。PagerDuty 把「多 Alert 合一 Incident」做成服务级（或 Global）AIOps 开关，把「改事件 / 丢掉 / 暂停」做成独立 Event Orchestration 产品，把「人来了之后」做成 Workflows。本租户三层里只有 Orchestration 列表能进去。

---

## 7. 集成：Events API v2、email ingest、webhooks v3、change events

### 7.1 Events API v2（告警事件）

- **Endpoint：** `POST https://events.pagerduty.com/v2/enqueue`
- **Auth：** 32 字符 `routing_key`（服务上的 integration key，或全局 ruleset / orchestration）
- 必填：`routing_key`、`event_action`（`trigger` | `acknowledge` | `resolve`）、`payload.summary`（≤1024）、`payload.source`、`payload.severity`（`critical|error|warning|info`）
- 可选：`dedup_key`（≤255）、`payload.timestamp`、`component`、`group`、`class`、`custom_details`、`images`（HTTPS `src`）、`links`、`client`、`client_url`

**Dedup：**

- 省略 `dedup_key` → PD 生成 UUID → **永远新 Alert**
- 同一 `dedup_key` + 同一 `routing_key` + 开着的 Alert → 打到那条
- Resolve 之后再 trigger 同一 key → **新 Alert**；没有开着的 Alert 时 ack / resolve → **丢弃**
- 用 **另一把** `routing_key` 去 ack / resolve → 丢弃

| Action | 效果 |
| --- | --- |
| trigger | 开 Alert，或在已开 Alert 上追加 trigger 日志 |
| acknowledge | Incident → acknowledged；之后的 trigger 不再 page |
| resolve | Incident → resolved；之后的 trigger 开 **新** Incident |

来源：[trigger-events](https://developer.pagerduty.com/docs/events-api-v2/trigger-events/index.html)。

**吞吐（官方 Support / AIOps）：** 默认每 key **120 events/min**；AIOps 可申请升到每 key **10,000**。第三方目录有写 480/min——**以 120 为官方 Support 数字**，除非客户成功申请上调（[aiops](https://support.pagerduty.com/main/docs/aiops)；dump §9）。Email ingest 每 key **60 events/min**（[email-integration-guide](https://support.pagerduty.com/main/docs/email-integration-guide)）。

REST 创建的 Incident **没有 Alert**，无 grouping / orchestration。高流量走 Events API。

控制台 Default Service Integrations：0 把 key，未生成、未复制。EU REST 基址文档写 `https://api.eu.pagerduty.com`（dump §5.5）；Events 主机在材料里是 `events.pagerduty.com`，**没有**单独的 `events.eu` 被摘到——不要编 EU Events 主机。

### 7.2 Change events

**只提供信息。不创建 Alert、Incident 或通知。** 用于服务时间线上的发布 / 配置，以及 AIOps change correlation。

- **Endpoint：** `POST https://events.pagerduty.com/v2/change/enqueue`
- 同一把 32 字符 **服务** integration key；**不能**用全局 integration key
- 也可经 vendor CI/CD、Custom Change Event Transformer

关联理由（过去 24h，最多展示 3 条）：time-based、related service（要依赖图）、intelligent / ML。表留 **90 天**。

来源：[recent-changes](https://support.pagerduty.com/main/docs/recent-changes)。Developer 的 send-change-events 页本次 fetch 未渲染，URL 仍是官方路径。

控制台 Default Service Activity 有 Recent Changes 区块 + AIOps upsell。空租户无数据。

### 7.3 Webhooks v3

出站 HTTP。账户级 **Generic Webhooks v3** 或服务级。上限：每个独特 service / Team ID 以及账户 scope **10** 个订阅。

V1：2021-11 不支持 / 2022-10 死。V2：停止支持 **2022-10-31**；EOL TBD；无新功能。

V3：自定义头（保存后值打码）、`x-webhook-subscription`、`x-pagerduty-signature`（HMAC）、`user-agent: PagerDuty-Webhook/V3.0`。测试事件 `pagey.ping`。IP 在 developer 站（不是 2022-05 冻结的 `/webhook_ips`）。External Status Page webhook 用 **另一套未公布** IP。OAuth2 webhook H2 2025 全计划 GA。

Incident 事件类型（文档列举）：`triggered`、`acknowledged`、`unacknowledged`、`resolved`、`reopened`、`reassigned`、`delegated`、`escalated`、`annotated`、`priority_updated`、`responder.added`、`responder.replied`、`status_update_published`、`conference_bridge.updated`、`custom_field_values.updated`、`incident_type.changed`、`service_updated`、`workflow.started`、`workflow.completed`。

Service 事件：`created`、`deleted`、`updated`、`custom_field_values.updated`。

来源：[webhooks](https://support.pagerduty.com/main/docs/webhooks)、[H2 2025](https://www.pagerduty.com/blog/product/product-launch-2025-h2/)。developer webhooks-overview 本次未渲染。

控制台：服务详情 0 个 V3 webhooks；菜单有 `/integrations/webhooks`，Pass 1 未点。Extensions `/extensions/` 卡片：PD Customer Service Ops、Microsoft Teams、Google、Slack、Amazon EventBridge、Live Call Routing（+Add）；Tabs Service Extensions | Add-ons；Type 切换 Account Level / Service Level；`+ New Extension`。

### 7.4 Email ingest

两个挂点（[email-integration-guide](https://support.pagerduty.com/main/docs/email-integration-guide)、[email-management-filters-and-rules](https://support.pagerduty.com/main/docs/email-management-filters-and-rules)、[event-orchestration](https://support.pagerduty.com/main/docs/event-orchestration)）：

1. **服务级 Email integration** — 专用地址；**忽略 Service Orchestration**；只用 email filters + management rules。本机部分可定制。
2. **Event Orchestration Email Address** — 先 Global 再 Service。Subject 作 `event.custom_details.subject`。Email 上能改的 Event Fields 只有 **Summary** 和 **Dedup_Key**。

默认：每封邮件开新 Incident。管理模式：按 subject 去重；永远新；自定义 trigger / resolve。过滤：subject / body / from 的 regex 白 / 黑名单；不匹配 **静默丢弃**。

限制：

- 每 key **60 events/min**
- 必须 UTF-8
- TLS opportunistic
- <96 KB：不截断。超过 96 KB：剥非文本附件；文本部分截到 32 KB；仍 >2 MB → 拒。头 `X-PagerDuty-Truncated-Part` / `X-PagerDuty-Removed-Attachments`
- >10 MB：处理前拒（"Message size exceeds fixed limit"）
- Incident key 截到 255
- 出站通知 Details 截到 500 字符

控制台未打开 email 集成向导。

### 7.5 REST API（管理面）

- 基址 US：`https://api.pagerduty.com`；EU：`https://api.eu.pagerduty.com`
- Auth：`Authorization: Token token=<api_key>` 或 `Authorization: Bearer <oauth_access_token>`
- 账户级 key 变更 Incident（ack / resolve）需要 `From: user@email`
- 用户个人 key 继承该用户权限；Global Admin / Owner 建账户 key
- SCIM v2.0 User APIs H2 2025 全计划 GA

官方参考：[api-reference](https://developer.pagerduty.com/api-reference/)。authentication 页本次未渲染；Python 客户端文档同一模型：https://pagerduty.github.io/python-pagerduty/user_guide.html。

REST = 建 / 改对象、手建 Incident（无 Alert 对象）。Events = 机器摄入。

---

## 8. AI：AIOps vs Advance agents

PagerDuty 把 AI 拆成两层付费，可叠加。**不拿客户数据训练。** 第三方 LLM 写在 "PagerDuty Advance AI Disclosure"（Assurance Profile 下载）。输出必须人工核对。

| 层 | 是什么 | 计价 | 来源 |
| --- | --- | --- | --- |
| **AIOps** | 降噪 + RCA 小组件 + Global Orchestration + Operations Console + 更高 ingest | **事件消耗**（2023-04；接替 Event Intelligence） | [aiops](https://support.pagerduty.com/main/docs/aiops) |
| **Advance** | 生成式 + 智能体（Assistant、agents、摘要） | 附加或一次性 **AI Actions** | [pagerduty-advance](https://support.pagerduty.com/main/docs/pagerduty-advance) |

SRE Agent 出现在 Operations Console 需要 **AIOps + Advance**；Slack / Incident 详情只要 Advance（[sre-agent](https://support.pagerduty.com/main/docs/sre-agent)）。

### 8.1 AIOps 能力面（文档）

噪声：Intelligent / Content / Unified / Time / Global grouping；Auto-Pause；编排 Suppression。  
分诊 / RCA：Probable Origin、Related Incidents、Outlier Incident、Past Incidents、Recent Changes。  
编排：Basic + Advanced；Global；Dynamic Routing；Dynamic Escalation Policies。  
控制台：Operations Console（实时观察；SRE Agent 列）+ Visibility Console。  
Ingest：120 → 最多 10,000 ev/min/key。  
按服务 / 按 orchestration 的 AIOps 开关（LGA）：只有打开的对象计入事件档。

本租户：Noise Reduction 整页 Start Trial；服务级 grouping disabled；Account Settings 写明不够格再开 AIOps 试用。Operations Console 菜单有、Pass 1 未点。

### 8.2 Advance 表面（文档）

来源：[pagerduty-advance](https://support.pagerduty.com/main/docs/pagerduty-advance)、[advance user guide](https://support.pagerduty.com/main/docs/pagerduty-advance-user-guide)。

- **Advance Assistant** — Slack / Teams 里的 genAI 聊天；路由到专项 agent。US + EU。**不再直接做 Incident 动作**（ack / 加 responder / 跑工作流 / resolve）——用聊天应用按钮 / 命令。
- **Slack Side Panel** — 与 Assistant 的私聊（付费 Slack）。
- **Post-Incident Reviews** — 从 Scribe + Slack + 生命周期事件起草。US（EU forthcoming）。
- **Status Updates** — 从聊天 + 日志起草内部更新。**不支持** External status page。
- **Periodic Incident Progress（PIP）** — 工作流动作；默认每 15 分钟打到 Slack / Teams。
- **Automation Digest** — 摘要 Automation Action job log（1 AI Action）。
- **AI Generated Runbooks** — EA；关闭开关与 Advance 总开关分开。
- **Amazon Q Business** — data accessor + plugin。
- **Advance Analytics / AI Usage Analytics**
- 按 Team 开 / 关（GA H2 2025）。默认全 Team 开。
- Proactive Incident Insights + Proactive Incident Summarization（Incident 频道里）。

控制台 `/advance`：整页 "Take a Tour / Contact Sales"。Analytics 菜单有 PagerDuty Advance dashboard 入口，未点。

### 8.3 Agents

| Agent | 角色 | 费用 | 访问 | 来源 |
| --- | --- | --- | --- | --- |
| **SRE Agent** | 虚拟响应者：吃 runbook / 日志、可能原因、建议 / 跑已批准修复、playbook、按 **服务** 记记忆 | 每次聊天请求、nudge、**或** 工作流 / EP 触发 = **4 AI Actions** | Console 要 AIOps+Advance；Slack / 详情要 Advance。Teams = EA。工作流虚拟响应者 = EA。EP trigger = EA | [sre-agent](https://support.pagerduty.com/main/docs/sre-agent)；[平台页](https://www.pagerduty.com/platform/ai-agents/sre/)；[2026-03-24 博文](https://www.pagerduty.com/blog/ai/meet-your-virtual-responder-pagerdutys-sre-agent-for-ai-driven-reliability/) |
| **Scribe Agent** | Zoom → Slack / Teams 转写 + 会摘要 | 每 30 分钟桥 6 actions + 摘要 2 | Advance；Slack+Zoom GA；Teams 预计 2025-10 | H2 2025 + Advance FAQ |
| **Shift Agent** | 侦测 Google Calendar PTO / OOO，建议顶班，在聊天里建 override | **0** AI Actions | Advance | H2 2025；notification-rules |
| **Insights Agent** | 历史 Incident、账户配置、可执行的主动建议 | **0** | Advance；预计 GA 2025-10-15 | H2 2025 |

SRE Agent 细节（Support，控制台未核）：

- 表面：Operations Console Tab、Incident Details Tab、Slack（`@pagerduty` / Triage）、MS Teams EA。
- 连接器：Grafana、Datadog、New Relic、AWS CloudWatch；runbook 来自 Confluence、GitHub。
- Nudge：Upload/Update Runbook、Analyze Past/Related Incidents、Generate Playbook、Check Change Events、Search Logs、Update Memory。
- 记忆（按服务）：Incident Playbook（"scratchpad"）、Customer Service Runbook、Incident Summarization、Service Profile。**Memory API** 可看 / 打码。
- 只分析 `custom_details` 和 notes 的 **前 2,000 字符**。今天对 timeline / workflows / alert-grouping 数据访问有限。
- 上传：`.txt/.pdf/.md`、`.jpg/.png`；每对话 25 个文件；每个 100 KB。
- 列出的能力：`get_incident_details`、`list_incidents`、`add_incident_note`、`get_service_details`、`get_related_services`。
- Incident 必须 **resolved** 才持久化记忆（或点 Update Memory）。

### 8.4 AI Actions 官方表

来源：[pagerduty-advance](https://support.pagerduty.com/main/docs/pagerduty-advance)。「Advance credits」已改名为 **AI Actions**。费用可能变。

| 动作 | AI Actions | 声称节省时间 |
| --- | --- | --- |
| Automation Digest | 1 | 5 min |
| Incident Summarization（"catch me up"） | 2 | 10 min |
| Status Update | 3 | 15 min |
| Planned Work（非事故问） | 2 | 10 min |
| Scribe | 6 / 30 min + 摘要 2 | 30–120 min |
| Shift Agent | 0 | 15 min |
| Insights Agent | 0 | 15 min |
| Key Moments | 0 | varies |
| SRE Agent（request / nudge / workflow / EP） | 4 | 20 min |

「声称节省时间」是官方 FAQ 列，不是实测。

### 8.5 H2 2025 / 2026 发布（博客，非控制台）

[product-launch-2025-h2](https://www.pagerduty.com/blog/product/product-launch-2025-h2/)（2025-10-07）：SRE Agent 预计 GA 2025-10-31；Insights Agent 预计 2025-10-15；Scribe Slack+Zoom GA、Teams 2025-10 末；Shift Agent GA；AI Orchestrations forthcoming EA；**MCP Server** GA Professional+（本地 OSS + 托管 `mcp.pagerduty.com`）；Flexible Shifts EA；Reopen GA；Required Fields on Resolve EA；Renotify forthcoming；Service Reassignment 全计划 GA；自定义字段触发工作流；Update Incident Type 动作；Create Incident + Get Status Updates 工作流动作；Audience-Specific Status Pages；PIR US（EU forthcoming）；Automation on Alerts（AIOps + Runbook Automation）；WhatsApp EA；Slack Work Objects；OAuth2 webhooks；ServiceNow OAuth2；SCIM v2；Glean / Logz.io / Backstage enrichment 合作。

后续 2026 SRE 增强博文：EP 触发 agent（EA）、工作流触发（GA）、Team 级 Advance 权限（GA）、Recommended Incident Workflows（GA）。

控制台能对上的只有：菜单里有 AI / AIOps / Automation / Advance Analytics 入口，以及它们在过期试用上的门控。Agent 对话框、Memory API、AI Actions 计数器 **都没看见**。

---

## 9. UI / UX 与实现（Rails 遗留 + React SPA、7 字符 ID）

本节几乎全部来自控制台 Pass 1。文档不写前端栈。

### 9.1 混合应用，不是单一 SPA

Pass 1 原话：部分路由是 **legacy Rails 页**（`/incidents`、`/escalation_policies` 整页刷新；jQuery `open-web-trigger-form` 锚点；404 是旧 Rails "mistyped the address"），另一部分是 **React SPA 壳**（先居中 spinner 再 hydrate：`/service-directory`、`/teams`、`/my-on-call`、`/analytics/*`、`/event-orchestration`）。

过滤 / 状态：

- 遗留列表：**URL query 可寻址**（`?status=acknowledged,triggered,resolved`）。
- SPA：路径段（`/service-directory/PATY3M1/settings`、`/my-on-call/month`）。

New Incident 的坏 CTA（无 href 的 `open-web-trigger-form`）和 `/incidents/new` → "Incident Not Found" 是同一套遗留缝：声明 Incident 仍走旧 trigger form，过期试用上这条缝裂开。不要把「点了没反应」写成产品没有手建 Incident——文档和按钮都在，**本租户这条 JS 没跑起来**。

Flashduty 控制台是单一 React SPA + Tailwind，路由切换不全页刷新（`flashduty-console-pass1.md` §5）。PagerDuty 是 **两代前端并存**。对 SRE Control（已经是单一 React 19 + AntD SPA）的实现启示是：对象 ID 和 URL 方案一旦按 Rails 时代定下来，会在 SPA 迁移后继续活着（见下）。

### 9.2 7 字符大写 ID

对象 ID 是 PagerDuty 经典 **7 字符大写** key：

| 对象 | 本租户 ID |
| --- | --- |
| User | `P7DCOZA` |
| Service | `PATY3M1` |
| Escalation Policy | `PYG7510` |

Analytics dashboard 用另一套 **base64 风格** ID（例 `CEdzCJbjpj0TIskDlDbcBg`）。材料没有「已统一」的声明。

文档 REST 也是这套 7 字符（`P` 前缀按资源类型变）。和 Flashduty 的十进制长整型组织 / 空间 ID + ObjectID 式故障号（`flashduty-analysis.md` §9.2）不同。SRE Control 用 `INC-` + 6 位数字（`alert-system.md` §4.3）。短、可念、出现在 URL 路径里（`/service-directory/PATY3M1`、`/escalation_policies/PYG7510`、`/users/P7DCOZA/profile`）是共同选择。

### 9.3 设计语言（只写看见的）

- 白画布、细灰线、Logo 里的 PagerDuty 绿、主按钮偏蓝（Pass 1 写 `#0f61dd-ish`）、蓝链接、顶栏当前项绿色下划线。
- 表：浅灰表头、可排序 / 过滤图标。
- 空态：线描插画 + 一条主 CTA（Incident 气球 "No news is good news!"；Schedules / Teams / Business Services 各有自己的首次空态）。
- Upsell：淡蓝信息条，或整页营销对开（左文案、右产品截图）。门控不是 Flashduty 那种「猫 + 盾锁 + Upgrade now」统一空态，而是按产品线各做一页营销。
- 持久铬：最顶深蓝试用条、左缘竖「Feedback」页签、右下蓝「?」帮助泡、页脚 Contact / Terms / Privacy / Cookies Settings / Developer Docs / System Status，© 2009–2026。
- 密度中等。Incident 列表右栏同时塞：大 `+ New Incident`、Always on-call 卡、ON CALL NOW 卡（带 carousel 箭头）、Slack 自动频道促销、GETTING STARTED / INFO 链接表——教学型 SaaS，空租户右侧比表身还满。

截图在 Pass 1 会话里拍过、**未落盘**。本文不引用不存在的文件，不编像素级还原。Pass 1 §7 列了 10 组拍摄主题（列表、mega-menu、Service、EP、空态、用户 / 规则、Analytics、门控页、Extensions / Account / 向导），可作为「曾经看见」的索引，不是附件。

### 9.4 实现线索对照

| 层 | 能写死的 |
| --- | --- |
| Web | Rails 遗留页 + React SPA 混部；jQuery 类名 `open-web-trigger-form`；SPA 先 spinner 再 hydrate |
| 路由 | 遗留 snake / 复数（`/escalation_policies`、`/api_keys`）；新壳 kebab（`/service-directory`、`/event-orchestration`、`/schedules-new`、`/reduce-noise`） |
| ID | 7 字符大写；Analytics 另一套 |
| 区域 | 本租户 `*.eu.pagerduty.com`；REST `api.eu.pagerduty.com`；Events 材料写 `events.pagerduty.com` |
| 文档 | Support KB `/main/docs/<slug>`；Developer portal；与控制台分离 |
| 后端 | **未开源**。公开的是 API 客户端、Terraform、MCP、Rundeck OSS、内部过程手册。Scala scheduler / Ruby blender 已归档，且 **不是** 产品值班调度器（见 §10） |
| 遥测 | Pass 1 **没有** 写 Datadog / 自研 RUM。不要把 Flashduty 控制台的 Datadog Session Replay 抄到这里 |

---

## 10. 开源边界（go-pagerduty、Terraform、MCP、Rundeck）

PagerDuty 核心 SaaS（摄入、去重、升级、通知、编排引擎）**闭源**。开源的是客户端、IaC、协议适配、过程文档、以及收购来的 Rundeck。GitHub 主组织 [pagerduty](https://github.com/pagerduty) 创建于 2011-05-04；2026-08-21 抓取时 **125** 个公开仓、约 306 followers。另有 [rundeck](https://github.com/rundeck)、[rundeck-plugins](https://github.com/rundeck-plugins)。**没有** PagerDuty 名下的官方 `statuspage` 组织——Statuspage.io 是 Atlassian。

### 10.1 和产品 / API 相关的仓

组织首页按星标只列了 6 个「top」。其余靠定向搜。**不要把 6 当成全部，也不要假装数过 125。**

| 仓 | Stars（组织页 / 材料） | 它实现什么 |
| --- | --- | --- |
| [PagerDuty/incident-response-docs](https://github.com/PagerDuty/incident-response-docs) | 1049 | [response.pagerduty.com](https://response.pagerduty.com/) 源：角色、SEV、复盘模板、培训。过程，不是产品 |
| [PagerDuty/security-training](https://github.com/PagerDuty/security-training) | 413 | 对员工公开的安全培训课 |
| [PagerDuty/go-pagerduty](https://github.com/PagerDuty/go-pagerduty) | 308 | 官方 **Go 客户端 + `pd` CLI**，REST API v2。`.pd.yml` 或 flag 鉴权。Apache-2.0。`go get github.com/PagerDuty/go-pagerduty@v1.8`。主页仍引用 v2.developer.pagerduty.com |
| [PagerDuty/terraform-provider-pagerduty](https://github.com/PagerDuty/terraform-provider-pagerduty) | 221 | 官方 Terraform。[Registry](https://registry.terraform.io/providers/PagerDuty/pagerduty/latest)。管 user / service / EP / schedule / event orchestrations / incident workflows 等 |
| [PagerDuty/scheduler](https://github.com/PagerDuty/scheduler) | 213 | **已归档。** 内部 Scala 分布式任务调度（Kafka + Cassandra）。生产用过；**不是** 产品 On-call UI |
| [PagerDuty/blender](https://github.com/PagerDuty/blender) | 182 | **已归档**（最后推送 2016）。Ruby 模块化远程命令 / 编排引擎（`pd-blender` gem） |

额外官方仓（不在首页 6 条里）：

| 仓 | 它实现什么 | 来源 |
| --- | --- | --- |
| [PagerDuty/python-pagerduty](https://github.com/PagerDuty/python-pagerduty) | 官方 Python 客户端，REST v2 + Events v2。接替 **pdpyras**。`RestApiV2Client`、`EventsApiV2Client`、OAuth helpers。带 Events API CLI：`pagerduty trigger -k $ROUTING_KEY --description "..."`。[docs](https://pagerduty.github.io/python-pagerduty/user_guide.html) | GitHub + docs |
| [PagerDuty/pdpyras](https://github.com/PagerDuty/pdpyras) | **已弃用** 的前任。同一 Session-wrapper。文档有迁移指南 | GitHub |
| [PagerDuty/pagerduty-mcp-server](https://github.com/PagerDuty/pagerduty-mcp-server) | 官方 **本地 MCP**。72 tools / 17 domains；默认 50 只读；`--enable-write-tools` 开 22 个写。环境变量 `PAGERDUTY_USER_API_KEY`、`PAGERDUTY_API_HOST`。托管：`https://mcp.pagerduty.com/mcp`。Apache-2.0 | GitHub；[support MCP](https://support.pagerduty.com/main/docs/pagerduty-mcp-server) |
| [PagerDuty/pdagent](https://github.com/PagerDuty/pdagent) | 遗留 daemon + CLI，走 Integration API trigger / ack / resolve。正被 **go-pdagent** 替代（README 时未功能对等） | GitHub |
| [rundeck/rundeck](https://github.com/rundeck/rundeck) | 开源 runbook 自动化（Web / CLI / WebAPI）。Apache-2.0。Copyright 2024 PagerDuty, Inc。约 6.2k stars。商业兄弟：**PagerDuty Process Automation / Runbook Automation** | GitHub；[收购](https://www.pagerduty.com/newsroom/pagerduty-closes-acquisition-rundeck/) |
| [rundeck-plugins/pagerduty-notification](https://github.com/rundeck-plugins/pagerduty-notification) | Rundeck job 通知插件 → Events API v1 或 v2（`service_key`） | GitHub |

### 10.2 「pd-cli」

**没有** 名为 `pd-cli` 的单一官方仓。官方 CLI 是：

1. **`pd`** — 随 [go-pagerduty](https://github.com/PagerDuty/go-pagerduty)。REST 入口，按资源分子命令。
2. **`pagerduty`** — 随 [python-pagerduty](https://github.com/PagerDuty/python-pagerduty)。只做 Events API v2（trigger / ack / resolve）。
3. 第三方 TypeScript CLI（PagerDuty SA Martin Stone，oclif）。社区帖明确 **不被 PagerDuty 背书 / 支持**：https://community.pagerduty.com/ask-a-product-question-2/introducing-a-new-pagerduty-cli-227

### 10.3 过程手册 ≠ 产品

[response.pagerduty.com](https://response.pagerduty.com/) 是内部重大事故过程的公开裁切：Before / During / After；角色 IC、Deputy、Scribe、SME、Customer Liaison、Internal Liaison；安全 IR 另路；复盘过程 + 模板；培训。严重度 SEV-1（必须重大 IR）到 SEV-5。**不是** 产品 Priority / Urgency / Severity。

### 10.4 Rundeck 收购（不是 Statuspage）

PagerDuty 于 **2020-10-01** 完成收购 Rundeck（约 $100M）。Rundeck 保持 OSS；PagerDuty 在其上卖 Process Automation / Runbook Automation。Event Orchestration / Incident Workflows 里的 Automation Actions 是产品化接点。

来源：[newsroom](https://www.pagerduty.com/newsroom/pagerduty-closes-acquisition-rundeck/)。

和 Flashduty 的 OSS 边界对照（观察）：Flashduty 核心 On-call 引擎闭源，开源的是夜莺 / Categraf（告警生成）+ CLI / SDK / MCP / Terraform（`flashduty-analysis.md` §8.4）。PagerDuty 同样闭源核心，但 **没有** 等价于夜莺的开源告警生成器；开源重心在 API 客户端和 Rundeck。两边都有官方 Terraform + MCP。Flashduty MCP 材料写 23 tools / 8 模块；PagerDuty MCP 材料写 72 tools / 17 domains——数字来自各自 dump，不要合成「谁更多」的结论以外的实现细节。

---

## 11. 对标 Flashduty：官方说法 vs 控制台实际

Flashduty 官方明确对标 PagerDuty（[comparison.md](https://docs.flashduty.com/zh/on-call/quickstart/comparison.md)）。**该页的逐条对比表没有被摘进本次 Flashduty 材料**，所以本文不编一张「官方对照表」（`flashduty-analysis.md` §11 已声明同一缺口）。能写的是：两边文档各自怎么讲自己、两边控制台实际长成什么样子、以及 **PagerDuty 自己的文档口径和控制台实勘差在哪**。

对照租户：

| | PagerDuty（本文） | Flashduty（已有分析） |
| --- | --- | --- |
| 控制台 | `cvtesreops.eu.pagerduty.com`，Account Owner，Trial Full User，**试用已过期** | `console.flashcat.cloud`，组织 SREMonitor，Owner，Free / 低档 |
| 空对象 | Default Service `PATY3M1` + Default EP `PYG7510` + 1 user；0 incident / schedule / team | Default Channel `2618595391671` + Default 分派规则；0 incident |
| 门控形态 | 顶栏蓝条 + 整页营销对开 / 锁图标 / 行内 upsell | 统一猫 + 盾锁 + "Upgrade now"；配额写在按钮旁（`0/1`、`oper-forbid`） |
| 手建事故 | New Incident modal **没打开**；`/incidents/new` → Incident Not Found | Incidents 页紫色 Create 在空租户上仍在（Pass 1 未走进表单） |

### 11.1 官方已经说出口的（PD 文档，不是对照表）

文档 dump §7 列过 12 条「比较时有用的行为」，全部有 URL。摘到对标 Flashduty 最硬的：

1. **双对象（Alert vs Incident）是一等的。** 手建 / REST Incident 没有 Alert，grouping / orchestration 不适用。Flashduty 也是 Event → Alert → Incident，但默认一条告警一条故障、告警聚合可选（`flashduty-analysis.md` §3、§5.2）。「event=incident」的设计对两边都不能 1:1。
2. **没人 on-call → 事件丢弃，不排队。** Flashduty：无策略 = 接入但不通知；轮空值班表单聊不发、群聊仍发（`flashduty-analysis.md` §4.3、§6.1）。两边覆盖缺口的噪声不同：PD 是静默丢事件，Flashduty 是进空间但不 page。
3. **Schedule 从不 page**；只有挂到 Service 的 EP 才 page。多通知 = 同一 EP 规则上多个目标（或 Workflows），不是「一个 schedule 里 N 个人」。Flashduty 值班表是账户级对象，被 Channel 内分派规则引用。
4. **第一下 page 不能在 EP 上延迟。** Delay 只活在个人 notification rules。Flashduty Delay window 挂在分派策略上，0–3600 秒，窗内关闭则不通知。
5. **Low urgency 不升级。** 班后安静 =（support-hours urgency = low）×（空的 low-urgency 规则）。Flashduty 用严重程度 × 媒介矩阵，没有单独的 High/Low urgency 轴。
6. **Dedup 按 routing_key + dedup_key**，不 reopen 已解决的。Grouping 是第二层（AIOps）。Flashduty 去重至少三层：接入幂等 → 事件聚合（alert_key）→ 告警聚合。
7. **Event Orchestration vs Incident Workflows** 是机器速 vs 人速。Flashduty 没有拆成这两个产品名；共享集成路由接近「事件编排」，入口在集成上，分派抽屉内部名却是 `notificationPolicy`。
8. **Stakeholder 路径是状态更新，不是 page。** Limited Stakeholder 是便宜只读许可。Flashduty 状态页是 On-call 二级菜单，Public 配额 0/1、Internal 0/0（该 Free 租户）。
9. **AI 是两个 SKU。** 分组 / probable origin = AIOps（事件价）。聊天 / agents / 摘要 = Advance（AI Actions）。Flashduty AI SRE 文档写公测不单独收费；控制台黄条写 private beta for Pro + free trial。
10. **硬上限**（容量规划用，文档数字）：100 triggers/dedup、1,000 alerts/incident、100 acks、1,000 responders、2,000 notes、20 EP 规则、9 次 repeat、50/5 多目标、10 webhooks/scope、10 keys/orchestration、1,000 路由目标、2,000 workflows、25,000 工作流订阅者、150k 状态页订阅、10 万开着的 Incident 后 24h 强制 resolve、email 60/min、Events 120/min（AIOps 可到 10k）。Flashduty 标准 Event API 材料写每集成 100 次/秒、1000 次/分钟；单故障合入告警上限约 5000——两套数字不要混用。
11. **OSS 重心：** Terraform + go-pagerduty `pd` + python-pagerduty + MCP + response.pagerduty.com + Rundeck。无官方 Statuspage OSS。
12. **Rundeck** 是自动化后端，不是 paging 产品。Flashduty 没有对等的开源 runbook 引擎被写进本次材料（Ibex 是夜莺侧自愈，约 46 stars）。

### 11.2 控制台实际：结构差比功能清单更重要

| 维度 | PagerDuty 控制台 / 文档 | Flashduty 控制台 / 文档 | 差 |
| --- | --- | --- | --- |
| 壳 | 顶栏 8 mega-menu | 最左产品轨 + 第二列模块导航 + 内容 | PD 是单产品平台壳；FD 是四轨套件（On-call / Monitors / RUM / AI SRE） |
| 主对象 | Service 拥有 Incident；EP 可复用 | Channel 拥有故障、集成、降噪、分派 | FD Pass 1 原话：这是相对 PD「services + notification rules」的关键设计差 |
| 出厂最小图 | Service → EP → User，无 Schedule | Channel → Default 分派规则 → 人 | 两边都能「没有排班表也永远 on-call」 |
| 严重程度 | Urgency High/Low + Priority 自定义 + Severity 四值 | 三档 Critical / Warning / Info，贯穿映射、矩阵、模板 | PD 把「怎么叫」和「多严重」拆开；FD 合成一行矩阵 |
| 分派归属 | EP 被 Service 引用，政策可复用 | 规则挂在 Channel 内，命中即停 | FD 一个对象同时承担「谁匹配」和「怎么升级」 |
| 个人通知 | 账户级规则，按 urgency 两套 + delay | 四场景偏好；策略可选跟随个人或统一矩阵 | PD delay 在人；FD delay 在策略 |
| 语音 / 短信 | 一等；文档 2m30s 切断、vCard / Critical Alerts | 一等；Usage 图只画这三条；公开中国主叫号码 | 本 PD 租户规则页只看到 email |
| IM | Slack / Teams 是西方默认（Extensions 卡） | 钉钉 / 飞书 / 企微与 Slack / Teams 并列 | 目录顺序是市场，不是能力有无 |
| 状态页 | 自己的产品，菜单只摘到 External，整页试用 | On-call 二级；Public 0/1，Internal 0/0 | 两边试用 / Free 都把对外状态页门控或限额 |
| 自己产告警 | 不自带规则引擎，靠 Events API | Monitors 要自装 Alert engine | FD 已扩出 On-call；PD 坚持「生成在上游」 |
| 手建事故 | 本租户 modal 坏了 | Create 按钮在，未走进 | **不要**把 PD 这次坏 CTA 写成「PD 不能手建」 |

控制台还有几处「看起来像 Flashduty、实现不是」：

- PD Incidents 的 "Assigned to me / All" 是人维；FD 是 "Assigned to me / My channel"（人维 + 空间维）。PD 没有 Channel 维。
- PD People 菜单把 EP / Schedule / User 放在一起，Teams 另走 `/teams`。FD 值班表在 Configuration，分派在 Channel 里。
- PD Event Orchestration 是独立 mega-menu 项（AIOps 下）；FD 共享路由挂在集成上，没有同名产品。
- PD 自定义类型是继承树（Base / Major / Security）；FD 自定义表单按生命周期切开：Declare / Acknowledge / Close。
- PD 空 Incident 列表是「先给表和批量条、再给气球空态」；FD 是「先给筛选器、再给 No data」。两边都是检索型列表，不是引导创建型——但 PD 右栏塞满 onboarding，FD 把教学放进每页文档深链。

### 11.3 PagerDuty 控制台实际打薄了官方叙事的地方

这些是 **PD 文档 vs 本 EU 过期试用控制台** 的差，不是 Flashduty 的错：

- 官方文档是完整 IR + AIOps + Advance + Workflows + Status Pages。控制台把后四块收成整页营销。Account Settings 明确 **不够格再开** Advance / AIOps 试用——比「功能灰掉但仍能点进去看表单」更硬。
- 官方手建 Incident 字段、conference、type、custom fields 都有专页。本租户 **New Incident 没打开**，`/incidents/new` 还闪「Incident Not Found」。文档能力 ≠ 本控制台可点。
- 官方 Event Orchestration 有画布、10 key、4 MB、动作目录。控制台只证实：列表能进、0 条、AI Recommendations 锁。
- 官方 Incident Workflows 是一等对象，有计划矩阵。Pass 1 顶栏 **没摘到** 这个菜单项。
- 官方 Mobile 底栏五格、Critical Alerts、Never Miss a Page。本次是 Web EU 控制台，没核 App。
- 官方 shift-based schedules（Summer 2026 推向全部账户）+ Flexible Shifts EA。控制台 `/schedules-new` 有新调度横幅，但创建表单没打开、本租户 0 条。My On-Call 画的是 EP 直打人。
- 官方 Analytics / Insights / Operational Maturity 是产品面。控制台 overview 能开，但日期默认 **2199**、全 No data——能写「空看板能加载」，不能写洞察怎么算。
- 官方 Stakeholders / Internal Status Page / 25,000 自动订阅。菜单只摘到 External Status Page，且整页试用。
- 官方 7 字符 ID 全局一致。控制台 Analytics 另用 base64 风格 ID。两套并存。
- 官方 Rails 从未被写成架构。控制台把 Rails 遗留和 React SPA 的缝暴露出来（类名、整页刷新、坏 CTA、旧 404）。

### 11.4 Flashduty 控制台打薄官方叙事的对位（避免只审一边）

`flashduty-analysis.md` §11.3 已写：官方 100+ 源 vs Pass 1 约 55 张卡；官方 IM 含 Telegram / Zoom vs 控制台五家；官方向导有公开 / 私有 / 代管 / 跟随告警关闭 vs Pass 1 Basic/Advanced 没摘到；AI SRE 文档不单独收费 vs 控制台 Pro beta。对标时两边都要「文档一套、控制台一套」，不要用 PD 文档打 FD 控制台，或反过来。

---

## 12. 对 SRE Control 的可借鉴点（完整对比另文）

完整对比另文（仓库快照见 `/workspace/research/alert-system.md`）。这里只列观察：PagerDuty 哪些设计被文档和控制台同时钉死，值得 SRE Control 对照，而不是抄。SRE Control v0.1.0 的中心论点是 Alertmanager 拥有 Alert 生命周期，自己拥有 Fault / Impact / Incident。PagerDuty 没有 Fault；Incident 默认由 Alert 触发，也可以手建（且手建没有 Alert）。同名对象不要对齐。

### 12.1 对象与所有权

1. **把「谁拥有事件」和「谁被叫起来」拆开。** Service 拥有 Incident 和 integration key；EP 可复用、可挂多个 Service；Schedule 可复用、自己不 page；通知规则在人身上。SRE Control 现在最接近的是 `escalation_policies`（目标是 team，再解析 on-call）+ `notification_policies`（团队级 steps）+ `schedules` / `shifts`（`alert-system.md` §6）。若借鉴，先问「Service 这种路由单元要不要做成一等」，而不是先抄 7 字符 ID。
2. **最小可运行图不需要 Schedule。** 本租户 Default EP 直打 User、Always on-call，My On-Call 日历照样画满。SRE Control 的 resolver 空结果写 timeline `nobody_on_call`，不是错误（`alert-system.md` §6.1）。两边都允许「没有排班表」——但 PD 文档另一句是 **没人 on-call 则事件丢弃、不可审计**。SRE Control 选择留下洞并记警告，比静默丢更可运营。不要为了「像 PD」改成丢。
3. **Alert 和 Incident 不要合成一个表。** 控制台 `/incidents` 与 `/alerts` 列不同（Urgency / Assignee vs Severity / Integration / Source）。SRE Control 已经把 Alert / Fault / Incident 做成三层导航（运行总览 / 故障 / 事故 / 告警证据）。PD 的借鉴是 UI 投影：告警表强调证据来源，事故表强调人和 urgency。不要把 Fault 压回 Alert。
4. **手建 Incident 不走告警管道。** 文档写死 REST / Web 手建不产生 Alert，编排 / grouping 不适用。SRE Control 的 Incident 本来就是人声明 + 双关闭门（CLAUDE inv. 11）。这条和 PD 手建路径同方向，和 Flashduty「聚合可以自动出故障」相反。保持。
5. **EP 快照。** 开着的 Incident 用触发时的 EP；事后改 EP 不影响，除非重新指派。SRE Control 的 personnel_escalation 是 timer + levels JSON（`alert-system.md` §6.2）。若以后允许热改升级策略，要显式选「快照」还是「跟着改」——PD 选了快照。

### 12.2 生命周期与三轴

6. **Urgency ≠ Severity ≠ Priority。** PD 用 urgency 决定叫法，severity 留在事件上，priority 给人和工作流。SRE Control Alert 已有 severity（critical / warning / info）和三轴 source_state / response_state / suppression_state（CLAUDE inv. 14）。不要再引入一套叫法混乱的 SEV——PD 自己的 response.pagerduty.com SEV-1…5 都刻意和产品字段分开。若要「班后别打电话」，做一个 **urgency 或 support-hours** 旋钮，别复用 severity。
7. **Ack 停升级，不等于 resolve。** Ack timeout（服务级，默认关）把「认领了但没修」拉回 Triggered。SRE Control 的 Ack 停 personnel / channel escalation；AM 源禁止手动 Resolve（HTTP 409）。可借鉴的是 **ack timeout 作为服务 / 策略级选项**，不是默认开。
8. **Snooze 只对已 ack，且重置 auto-resolve。** 最大 7 天；到期若轮转变了，新旧 on-call 都叫。SRE Control 材料没有一等 snooze。若做，别和 silence 合成一个开关——PD 把 snooze（人、Incident 后）和 orchestration pause（机器、Incident 前）分开。
9. **Merge 不合并人和订阅者。** 只搬家 Alert 和 dedup key。SRE Control 的 Fault 与 Incident 链接有 primary / supporting、included / excluded（000017）。合并语义要写死「人跟不跟」。
10. **没人 on-call 的行为必须产品化。** PD 丢且不可审计；Flashduty 接入不通知；SRE Control 记 `nobody_on_call`。三选一写进 CLAUDE，不要默默变成第四种。

### 12.3 通知

11. **个人规则 × 服务 urgency，是「怎么叫」的交接点。** 本租户出厂是高低 urgency 各一条立刻 html email、换班立刻 + 提前 1 天、Shift Agent 等 Slack。SRE Control 目前是团队级 policy steps，没有个人 notification rules（`alert-system.md` §11.4）。若借鉴，先做 **按人的 delay 链**（push 0 / 电话 1 / …），再做团队矩阵；不要两套同时上。
12. **第一下 page 不能拖，是 PD 的产品选择，不是物理定律。** Flashduty 和 SRE Control 的 `wait_seconds` 都允许「先等再叫」。SRE Control 已有 flap 表和 channel_escalation wait（`alert-system.md` §5.2、§6）。对齐叙事：「抖动 / 延迟在分派前，升级 timeout 在分派后」，不必抄「EP 零延迟」。
13. **删通知规则 ≠ 取消指派。** 休假走 override。SRE Control 缺一等 override window（`alert-system.md` §6.1）。比抄 Critical Alerts 更早该做的是 override。
14. **Bypass DND 是电话 / 推送产品的核心，不是设置页的一个勾。** iOS Critical Alerts、Emergency Bypass vCard、Android Override Volume。SRE Control 的 phone 是 registry 占位、Validate 拒绝未注册；唯一可 ACK 的是飞书卡片。若永远不做电话，就不要在政策 UI 里露出 phone（`alert-system.md` §12 同类建议）。
15. **Stakeholder ≠ Responder。** 状态更新无 delay、Limited Stakeholder 只读。SRE Control 有 communications_lead / business_liaison 和 incident outbox，没有状态页 / 订阅者产品。不必为对等去补 Status page；若做内部通报，走同一 outbox、单独模板，不要复用 page 规则。

### 12.4 自动化分层

16. **机器速（改事件）和人速（Incident 已在）不要做成一个「自动化」开关。** PD 的 Orchestration / grouping / Workflows 三层，本租户只有 Orchestration 列表能进，正好说明它们是独立产品面。SRE Control 的 pipeline 是 dedup → mapping → route → group → silence → inhibit；AM 源只跑权威安全的阶段（CLAUDE inv. 1、14）。不要为了功能对称在 AM 源上再做一层 PD 式 grouping。
17. **Change events 隔离。** PD：不建 Alert / Incident / 通知，只上时间线和 AIOps 相关。SRE Control 已有独立 `change_events` 表和 ingest，与告警队列隔离（`alert-system.md` §7）。保持。
18. **Events API 和 REST 管理面分开。** 高流量走 enqueue + routing_key；手建走 REST、不产 Alert。SRE Control 只有 `POST /ingest/v1/hook/{hook_uuid}` + 三个 adapter。若加「标准信封」，做成 Events 路径，不要让 REST 声明事故走同一条 hook。
19. **AI 两个 SKU 的边界值得抄，能力不必抄。** 降噪 / 相关变更 = 确定性或事件价；聊天 / 摘要 / agent = 另一层、可关、按 Team。SRE Control 明确 LLM 只留异步 enrichment、路径上禁止（CLAUDE inv. 9；aiops-foundation changelog 永久排除 embedding）。借鉴「开关按对象、不训练客户数据、输出要核」，不要上 SRE Agent。

### 12.5 IA / UX / 实现

20. **运行面和配置面分开。** PD 顶栏 8 mega-menu 把 Incident 和 People / Integrations 切开；SRE Control 全局导航是 SRE 主任务，设置进控制中心（CLAUDE inv. 19）。自托管单产品不必抄四轨或 8 下拉，但「事故列表不是设置页」已经对齐。
21. **URL 从第一天就当 API。** 遗留页 `?status=`，SPA 路径段带 7 字符 ID。SRE Control 已有干净路径（`/faults/:id`、`/incidents/:id`）。深链比再做一个「保存的视图」产品便宜。
22. **空态分检索空 / 首次空 / 套餐空。** PD 气球是检索空；Schedules / Teams 是首次空；Advance / Status / Automation 是套餐空（整页营销）。自托管没有套餐，但仍有「未配 Alertmanager / 未配值班」。值得同一套插画语言，不要三种文案三种布局。
23. **遗留缝会在过期 / 降级时裂开。** `open-web-trigger-form` 无 href、`/incidents/new` 闪 Not Found——功能文档还在，按钮还在，JS 不跑。SRE Control 是单一 SPA，反而该防的是「设置页能开、ingest hook 未配」的假完成。琥珀横幅（Flashduty Monitors「No installation detected」）比静默 404 诚实。
24. **短 ID 给人念，长 ID 给 API。** PD 7 字符进路径；Analytics 另用 base64。SRE Control `INC-` + 6 位。保持短号稳定，不要在 SPA 迁移时换一套。
25. **OSS 边界：客户端和 IaC 开源，引擎闭源。** PD 的 go-pagerduty `pd` + Terraform + MCP 是外部集成面。SRE Control 自托管，引擎就是产品。值得做的是 **稳定的 ingest 信封和 Terraform / 声明式配置**，不是再开一个 MCP 先把 72 个工具铺上。

### 12.6 明确不必借鉴

- 把 Incident 当 Alert 的默认自动产物（和人声明事故 + 双关闭门冲突）。
- 没人 on-call 就丢事件且不审计。
- 为对等去补 Status page / 公开订阅 / 15 万订阅上限 / WhatsApp EA。
- 在 AM 权威源上再做 Event Orchestration 或 Intelligent Grouping。
- 抄 Rails + React 混部。那是 15 年产品的伤疤，不是目标架构。
- 抄 AI Actions 计价表和「声称节省 20 分钟」。
- 把 Channel / Service 一词继续当通知媒介——Flashduty 已经用 Channel 占了协作空间；SRE Control 的 `notify.Channel` 是运输层。若引入服务管道，用 Service / Space，不要再占 Channel。
- 为了「像 PD」去自建 Ruler 或 55 个云监控适配器。PD 自己都不做规则引擎。

---

## 材料缺口（避免被当成「产品没有」）

下列在两份 PagerDuty 材料里明确未核验或未打开。缺的是观察，不是功能不存在：

1. New Incident 表单字段、conference 在声明时的控件、手建是否真的不通知创建者。
2. Incident / Alert 详情时间线、真实数据上的 Urgency / Priority / Severity 配色。
3. Event Orchestration 画布、动作目录、integration key、AI Recommendations 解锁后的样子。
4. Incident Workflows 在控制台的入口和编辑器（菜单未摘到）。
5. Operations Console、Service Graph、Service Custom Fields、Generic Webhooks v3 管理页、API Keys。
6. 创建 Schedule 表单（点了没打开）、shift-based vs legacy 的真实控件。
7. Contact Information 子页、push / phone / SMS 的添加流、测试发送。
8. Mobile App、Critical Alerts、Never Miss a Page 预设。
9. Stakeholder 订阅、Internal Status Page、状态更新发送器。
10. Advance / SRE Agent / Memory API / AI Actions 计数器（本租户整页门控）。
11. 定价页现价；Professional / Business / Enterprise / Digital Ops / EIM 的完整矩阵（文档只散落在各页）。
12. Events API EU 主机是否存在；developer 未渲染页（overview / change-events / webhooks-overview / authentication / api-reference HTML）。
13. 125 个公开仓的全表；go-pdagent 现状。
14. 官方 Flashduty 对比页的逐条表格（FD 材料同样缺失）。
15. 未登录墙、SSO、EU 数据驻留除主机名以外的声明。

---

## 一句话

PagerDuty 的 Operations Cloud 用 **Service + 一条 Escalation Policy +（可选）Schedule + 人身上的通知规则** 把监控事件变成必须派给人的 Incident；Alert 是 Events API 路径上的证据对象，手建 / REST Incident 没有 Alert。Urgency 决定怎么叫，Priority 决定先做谁，Severity 留在事件上。第一下 page 不能在 EP 上延迟；没人 on-call 则事件丢弃。Event Orchestration / AIOps grouping / Incident Workflows 是三层自动化，AI 再拆成事件计价的 AIOps 和 AI Actions 计价的 Advance。EU 过期试用控制台证实了出厂最小图（Default Service `PATY3M1` → Default EP `PYG7510` → User `P7DCOZA` Always on-call）、8 个 mega-menu 的平台壳、Rails 遗留 + React SPA 混部、7 字符 ID，以及 New Incident 对话框在本状态不可用。核心引擎闭源；开源边界是 go-pagerduty `pd`、Terraform、MCP、python-pagerduty 和 Rundeck。

主要来源：`pagerduty-docs.md`（URL 见该文 §8）、`pagerduty-console-pass1.md`（路径见该文 §8）。对标节使用 `flashduty-analysis.md`、`flashduty-console-pass1.md`、`alert-system.md`。调研日 2026-08-21，Asia/Shanghai。
