# PagerDuty 产品分析报告

调研日期：2026-08-20  
调研账号：`cvtesreops.eu.pagerduty.com`，用户 `xiaowei luo`（`luoxiaowei@cvte.com`），角色 Account Owner，License 为 Trial (Full User)  
入口：`https://cvtesreops.eu.pagerduty.com/incidents?status=acknowledged%2Ctriggered%2Cresolved`  
文档：`https://support.pagerduty.com`、`https://developer.pagerduty.com`、`https://response.pagerduty.com`  
开源组织：`https://github.com/PagerDuty`

账号顶部常驻黄条：`Oh no! Your trial has expired.` 核心路由/通知/服务目录仍可点开；AIOps、Automation、Status Pages、Advance 以试用墙或 Contact Sales 呈现。这反而有利于观察「免费内核 vs 加购层」的产品切割。

---

## 1. 产品身份：Operations Cloud，内核仍是「Service → Escalation Policy」

官网自称 *AI-First Operations Platform* / *Operations Cloud*。控制台顶栏却把能力摊成一条工作链：

`Incidents → Services → People → Automation → AIOps → Analytics → Integrations → Status → AI`

十六年来真正没变的内核是：

> 一个技术服务绑定一条升级策略；告警打到服务上，策略决定叫谁，用户通知规则决定怎么叫。

其余都是加在这条链上的层：Event Orchestration（进服务前改写/分流）、Alert Grouping（进服务后降噪）、Automation Actions（到人之后少升级）、Status Page（对外说人话）、Analytics（事后度量）、Advance/AI Agents（摘要与预置机器人）。

PagerDuty **不是监控系统**，也不试图拥有指标存储。它拥有的是：

- 750+ 集成把「已经判定的异常」变成 Event
- 可靠地把 Event 变成 Incident 并叫醒人
- 用服务目录和升级链表达组织责任
- 用分析证明 MTTA/MTTR 在变好

这和 Flashduty 一样站在「告警响应」层，但组织方式完全不同：PagerDuty 的根对象是 **Service**，Flashduty 的根对象是 **协作空间**。

---

## 2. 控制台信息架构（浏览器实测）

### 2.1 顶栏一级导航

| 菜单 | 子项（实测） | 说明 |
| --- | --- | --- |
| **Incidents** | 当前页 | 全局事故工作台，默认按状态过滤 |
| **Services** | Service Directory、Business Services、Service Graph、Service Custom Fields | 技术服务是路由锚点 |
| **People** | Users；Schedules / Escalation Policies / My On-Call Shifts | 人、班、升级链 |
| **Automation** | Automation Actions | 本账号整页销售页 |
| **AIOps** | Operations Console、Noise Reduction、Event Orchestration | 降噪与全局编排，多数要加购 |
| **Analytics** | Dashboard、Insights、Event Analytics、Advance；Operational Maturity、Service Recommendations | 空数据仪表盘仍展示 MTTA/MTTR 卡片 |
| **Integrations** | Extensions、Service Integrations；App Registration、API Access Keys、API Docs、Generic Webhooks v3 | 生态与开发者 |
| **Status** | External Status Page | 对外状态页，试用墙 |
| **AI** | AI Settings、Usage Analytics | Advance，当前套餐不可试 |

顶栏右侧固定：全局搜索、`+ New Incident`、Help、Apps/What's New、用户头像 `XL`。

What's New（2026-08）还在推：去掉 Gravatar、Slack/Web Quick Declare、Enterprise-Scale Status Pages GA。说明 Slack 一键报障和状态页仍是当前增长点。

### 2.2 Incidents 工作台

URL 带状态 query：`?status=acknowledged,triggered,resolved`。

过滤器：

- 状态 Tab：Open / Triggered / Acknowledged / Resolved / Any Status
- 范围：Assigned to me / All
- 时间：Last 30 days
- 搜索：`Go to incident #...`（按编号跳，不是全文检索）

批量动作：Acknowledge / Reassign / Resolve / Snooze

表格列：Status、Urgency、Type、Title、Created、Service、Assigned To

空态插画文案：`No news is good news!`

右侧栏：

- On-call 卡片：`You're always on-call` / 团队 `Default` / `LEVEL 1` / `xiaowei luo` / `on-call all the time`
- Slack Automated Incident Channels 促销
- Getting Started：Setting up PagerDuty、first notification、PD University

这是一个 **「我现在该看什么单」** 的页面，不是拓扑，也不是聊天室。

<img src="/opt/cursor/artifacts/pagerduty_incidents_logged_in.webp" alt="PagerDuty Incidents：试用过期条、空列表、右侧永远值班" />

### 2.3 创建 Incident 表单

`+ New Incident` 抽屉/页字段：

| 字段 | 约束 | 产品含义 |
| --- | --- | --- |
| Title | 必填 | 人话标题 |
| Incident Type | 必填下拉 | 类型化事故（较新能力，和自定义字段绑定） |
| Impacted Service | 必填可搜索 | **事故必须挂在某个 Service 上** |
| Description | 可选多行 | |
| Urgency | 下拉 | 文案写明：决定响应人如何被通知（high/low 两套通知规则） |
| Assignee | 可搜索 | 可绕过策略直接指定 |
| Advanced：additional responders | 可选 | 把别人拉进响应，不改变 Service 绑定 |

没有「业务影响确认」「Fault 选择」「爆炸半径」。创建即一张响应单，锚点是 Service。

<img src="/opt/cursor/artifacts/pagerduty_new_incident.webp" alt="New Incident：Title、Type、Impacted Service、Urgency、Assignee" />

---

## 3. 核心对象模型

```
Integration key
      │
      ▼
   Event
      │  可选：Global Event Orchestration
      ▼
    Alert
      │  可选：Alert Grouping / AIOps
      ▼
  Incident  ──属于──►  Technical Service
                           │
                           └──绑定──► Escalation Policy
                                          │
                                          ├── Level 1: User / Schedule
                                          ├── Level 2: ...
                                          └── User.NotificationRules
```

| 对象 | 控制台定义/文案 | 关键约束 |
| --- | --- | --- |
| **Technical Service** | “A service represents a component, microservice or piece of infrastructure a team operates, manages, and monitors.” | 每个服务 **恰好一条** Escalation Policy |
| **Business Service** | 跨多个技术服务、可能跨多个团队的业务能力 | 本账号空，用于更高一层影响建模 |
| **Service Graph / Dependencies** | `uses` / `is used by` 双向可视化 | 可手拉，也可用 API/Terraform 灌 |
| **Escalation Policy** | 事故创建后，用服务的策略决定联系谁 | 多 Level；可重复整条策略 N 次 |
| **Schedule** | 轮班；新 UI 强调 recurring shifts、multi-responder | 本账号 0 张表，Default EP 写着 Always on call |
| **User** | 联系方式 + 通知规则 + 角色/席位 | Full User vs Stakeholder |
| **Incident** | Triggered / Acknowledged / Resolved | 必须有 Service；有 Urgency、Priority、Type |
| **Alert** | 进入服务后的原始/分组事件 | 服务详情表有 Alerts 列 |
| **Notification Rule** | 用户私有：何时用邮件/短信/电话/推送 | High 与 Low urgency 两套 |
| **Extension** | Slack/Teams/EventBridge 等增强 | 账号级或服务级 |
| **Change Event** | 部署/变更，给 AIOps 做关联 | 服务 Activity 里用文案推销 |
| **Status Page** | 对外/对内状态沟通 | 本账号仅销售页 |
| **Automation Action** | 在自己生产环境跑诊断/修复 | Contact Sales |

没有独立的 Fault。没有 potential/confirmed Impact。Service Graph 只表达「谁用谁」，不计算爆炸半径置信度。

ID 风格：`PATY3M1`（服务）、`PYG7510`（升级策略）、`P7DC0ZA`（用户）。稳定、短、可读，适合运维口播。

---

## 4. Service 是产品的中轴

### 4.1 Service Directory

`/service-directory`

- Tab：Services / Maintenance Windows
- 过滤：搜索、Business Service、Last Incident、Sort
- 导出、`+ New Service`

Default Service 卡片字段：

- 绿色状态勾
- TEAM：`No team is assigned to the Default escalation policy`
- ON CALL NOW：xiaowei luo
- LAST INCIDENT：No incidents so far
- OPEN INCIDENTS：0 triggered, 0 acknowledged
- **Standards: 1/8 Standards met**（红）

「服务标准分」是隐性治理：一个服务如果没配文档链接、没配集成、没配团队，就会一直红。PagerDuty 用合规分数逼组织把服务目录填完整。

<img src="/opt/cursor/artifacts/pagerduty_service_directory.webp" alt="Service Directory：Default Service，1/8 standards" />

### 4.2 服务详情四 Tab

`/service-directory/PATY3M1`

面包屑：Service Directory > Default Service > Activity

页头：STATUS、ON CALL NOW、ESCALATION POLICY、COMMUNICATION CHANNEL（空，可 Add）、Standards。

**Activity**

- Open Incidents 表：Status / Priority / Urgency / Incident Type / Alerts / Title / Assigned To / Created
- 动作：Acknowledge / Resolve / Snooze / **Merge Incidents**
- Recent Changes：引导接部署工具做变更关联
- Resolved：Last 90 Days Overview

**Integrations**

- 空态 + `+ Add an Integration`
- 热门图标：Nagios、Sumo Logic、Datadog、AWS、Dynatrace、Splunk、Email、GitLab
- Extensions / add-ons 另列

**Settings — 这是内核配置**

Assign and Notify：

- Assign to Escalation Policy: Default
- High-urgency notifications, escalate as needed
- When incidents are not actioned：
  - 不重新触发已 Ack 的事故、不重复通知
  - 不自动 Resolve 打开的事故

Reduce Noise：

- `Currently not grouping alerts on this service`
- 蓝条：账号未开 Alert Grouping，可 Preview Intelligent Alert Grouping（AIOps）

Remediate：

- Documentation Link：无
- Custom Incident Actions：未启用

**Service Dependencies**

中间节点 Default Service，左侧 `uses 0`，右侧 `is used by 0`，两边都能 Add Services。文案区分 Technical / Business Services。

<img src="/opt/cursor/artifacts/pagerduty_service_detail.webp" alt="Default Service Activity：无打开事故" />

<img src="/opt/cursor/artifacts/pagerduty_service_settings.webp" alt="服务设置：升级策略、通知、Alert Grouping 预览" />

<img src="/opt/cursor/artifacts/pagerduty_service_graph.webp" alt="Service Graph 画布：从右侧拖入无依赖服务" />

### 4.3 Business Services

空态：`Create your first Business Service`。  
解释句直接承认技术服务不够：业务能力会跨多个技术服务、跨多个团队。这是 PagerDuty 对「影响面」的回答——用另一层服务对象聚合，而不是用 FaultImpact 三态。

---

## 5. 人、班、升级链、通知规则

### 5.1 Users

`/users`

- Trial (Full User): 1 in use
- Trial (Stakeholder): 0 in use
- 列：Users / License / Role / Actions
- 当前用户：Account Owner

席位模型本身就是产品：能被叫醒的是 Full User；只能看状态的是 Stakeholder。Flashduty 用「订阅状态页免费且不必有账号」打这一点。

### 5.2 用户资料五 Tab

`/users/P7DC0ZA/profile`

- My Profile：姓名、Title、Bio、语言 English (US)、时区 Beijing、近 14 天活动
- Contact Information：Voice / SMS / Email（已有默认邮箱，可 Test/Edit/Delete）/ Mobile App 二维码 / Slack Extension
- Notification Rules
- User Settings
- On-Call Shifts

### 5.3 Notification Rules：可靠性哲学写在警告条里

High urgency：

- 橙条：强烈建议加电话和推送，否则容易漏通知
- 橙条：多条规则是避免漏通知的最好办法
- 现有规则：`Immediately after it's assigned to me, html email me at luoxiaowei@cvte.com`

另外三组：

- 被邀请做 responder 时：复用 high urgency 规则
- High urgency 状态变化
- Low urgency 通知（同样是立刻邮件）

这是 PagerDuty 最老、也最硬的设计：**通知策略属于人，不属于服务。**  
服务只决定叫谁、叫得有多急（urgency）；每个人自己决定 0 分钟邮件、5 分钟短信、10 分钟电话。

Flashduty 把更多通知方式收进分派策略和模板，管理员色彩更重。PagerDuty 把「别漏叫我」交给个人，用产品文案恐吓你去加电话。

<img src="/opt/cursor/artifacts/pagerduty_notification_rules.webp" alt="用户通知规则：立刻邮件，警告缺少电话和推送" />

### 5.4 Escalation Policies & Schedules

`/schedules-new` 会落到「Escalation Policies & Schedules」新壳，并宣传 shift-based schedules。

本账号 **0 张 Schedule**。Default Escalation Policy（`/escalation_policies/PYG7510`，2024-01-03 创建）：

- Level 1：立刻通知 xiaowei luo，`Always on call`
- `If no one acknowledges, repeat this policy 0 times`
- 右侧：该策略被 Default Service 使用
- `Send On-Call Handoff Notifications: when in use by a service`

蓝条教学文案几乎是产品说明书：

> When an incident is created, PagerDuty uses the service's escalation policy to determine who to contact. You'll want to create dedicated escalation policies for each type of service... To get you started, we've already created your first escalation policy called 'Default.'

也就是说：新账号的最小闭环是 **Default Service + Default EP + 账号所有者永远值班 + 邮件通知**。不建协作空间，不建频道。

<img src="/opt/cursor/artifacts/pagerduty_escalation_policies.webp" alt="Escalation Policies 列表：Default 绑着 Default Service" />

People 菜单在本试用账号里 **没有独立 Teams 列表**。团队更像服务/策略上的可选归属，而不是 Flashduty 那种管理边界。空账号看到的组织模型是扁平的：一个 Owner，一条策略，一个服务。

---

## 6. 生命周期

```
监控工具 --Integration--> Event
                │
                ├─ Event Orchestration（全局 key，可改写/抑制/改道）
                ▼
              Alert @ Service
                │
                ├─ Alert Grouping（服务级，AIOps）
                ▼
            Incident Triggered
                │
                ├─ 按 Escalation Policy Level 1..N 找人
                ├─ 按该人 High/Low Notification Rules 触达
                ├─ Ack 停止升级
                ├─ Snooze 推迟
                ├─ Reassign / 加 responder
                ├─ Merge
                ▼
            Resolved
                │
                └─ 可选 Postmortem / Status update / Analytics
```

和 Flashduty 的差异：

- PagerDuty 的 Incident **默认就是响应对象**，不先经过「告警列表是一等公民、故障是聚合」的教学。服务详情里 Alerts 只是 Incident 的子计数。
- Ack 的语义非常强：停止升级。这是 On-call 产品的共同语言。
- Urgency 是通知强度，不是业务严重度。Priority / Incident Type 才开始承载管理维度。
- 服务设置里可以明确「不要自动 resolve」「不要对已 Ack 的再打扰」。权威在 PagerDuty，不在上游。

试用过期后，列表动作按钮仍在，但没有真实事故可点进详情。详情解剖（conference bridge、status update、past incidents、runbook、timeline）本次只能结合文档与空表列推断，不能假装已经点进过真实事故页。

---

## 7. 加购层：Automation / AIOps / Status / AI

这些页面在过期试用账号里几乎都是销售页，但信息架构本身说明产品怎么赚钱。

### Automation Actions

`/automation-actions`  
卖点：接到你自己的生产自动化；少 40% 升级；MTTR 少约 25 分钟。按钮 `Contact Sales`。预览卡片是「拉 CloudWatch 日志 / 重启应用」。

PagerDuty 不在 SaaS 里替你执行 kubectl。它要你把已有 Runbook 挂到事故上。这和我们「白名单类型化动作 + 生产默认 DisabledExecutor」是同一类边界，只是他们做成了售卖模块。

### AIOps

- Operations Console：实时多视图分诊，要 Start Trial
- Event Orchestration：`/event-orchestration`，用全局 integration key 做事件路由。空态可 New Orchestration，另有 AI Recommendations Tab
- Noise Reduction / Intelligent Alert Grouping：挂在服务 Settings 里 Preview

AIOps 是「规模化事件进来之后怎么少叫人」，和基础 Escalation 分开卖（Flashduty 对比文声称 \$699+/月起、按事件量）。

### Analytics

`/analytics/overview-dashboard/...`  
时间窗、Share、Last updated。卡片：Total Incidents、MTTA、MTTR、Interruptions、Acknowledgment Rate。全部 No data。  
产品把 **MTTA/MTTR/打断次数** 当作默认运营语言。

<img src="/opt/cursor/artifacts/pagerduty_analytics.webp" alt="Analytics Overview：MTTA/MTTR 卡片无数据" />

### Integrations / Extensions

`/extensions/` 卡片：Customer Service Ops（Salesforce/Zendesk/ServiceNow）、Teams、Google、Slack、EventBridge、Live Call Routing。  
开发者工具：API keys、Generic Webhooks v3、App Registration。  
生态叙事是 750+，控制台用「热门 logo 墙」降低选择成本。

<img src="/opt/cursor/artifacts/pagerduty_extensions.webp" alt="Extensions：Slack、Teams、EventBridge、Live Call Routing" />

### External Status Pages

整页销售：公开/私有页、品牌定制、Human-in-the-Loop 自动更新、订阅通知。`Start a Trial`。

<img src="/opt/cursor/artifacts/pagerduty_status_pages.webp" alt="External Status Pages 销售页" />

### AI / Advance

账号设置写明：现有 plan 不能试用 Advance 和 AIOps，要 Business/Professional。  
公开资料里的拆法：Advance（问答/摘要）、SRE Agent、Scribe、Shift、Insights。修复动作依赖预先配好的 Automation Actions，不是开放 Agent loop。

---

## 8. 账号与治理细节

`/account/settings`：

- Company Name：`cvtesreops`
- 默认时区：UTC+08:00 Shanghai
- 对非管理员隐藏电话/邮箱
- 默认 UI/语音语言 English (US)；选其它语言时「只有部分页面或语音」，其余仍英语
- 更换 Account Owner（唯一能改套餐/账单/取消的人）
- 登录邮箱域名限制

Footer：Contact / Terms / Privacy / Developer Docs / System Status。  
System Status 链到 PagerDuty 自己的状态页——他们吃自己的狗粮。

---

## 9. 官方文档与开源

### 9.1 文档分成三套，职责不同

| 站点 | 给谁 | 内容 |
| --- | --- | --- |
| [support.pagerduty.com](https://support.pagerduty.com) | 使用者 | 产品操作：服务、策略、通知、集成 |
| [developer.pagerduty.com](https://developer.pagerduty.com) | 集成方 | REST API、Events API、Webhooks、App |
| [response.pagerduty.com](https://response.pagerduty.com) | 组织/IC | **事故响应过程**，不是软件说明书 |

第三套特别重要。GitHub [`PagerDuty/incident-response-docs`](https://github.com/PagerDuty/incident-response-docs)（1k+ star）公开的是 PagerDuty 公司自己的 IR 手册：准备、值班、指挥、沟通、复盘。产品只实现手册里「叫人、升级、记录」那一段；**指挥、沟通、学习是组织过程**。

另有 [`PagerDuty/postmortem-docs`](https://github.com/PagerDuty/postmortem-docs)、[`PagerDuty/security-training`](https://github.com/PagerDuty/security-training)。他们把方法论开源，把引擎闭源。

### 9.2 开源仓库画像

PagerDuty 组织约 125 个公开库。和产品相关、星数较高的：

| 仓库 | Star 量级 | 作用 |
| --- | --- | --- |
| incident-response-docs | 1049 | IR 过程圣经 |
| security-training | 413 | 安全培训公开课 |
| go-pagerduty | 308 | 官方 Go REST 客户端 |
| terraform-provider-pagerduty | 221 | IaC |
| scheduler | 213 | Scala 定时库（基础设施，不是 On-call 产品） |
| blender | 182 | 编排引擎（同样偏内部基建） |
| pdpyras / pagerduty-api-python-client | 133 / 81 | Python API |
| pd-oncall-chat-topic | 94 | 用当前值班人改 Slack topic |
| pagerduty-mcp-server | 76 | 官方 MCP，让外部 Agent 调 PagerDuty |
| api-schema | 34 | OpenAPI |
| pdagent | 34 | 老式监控侧 agent |

**和 Flashduty 对照**：两边都不开源 On-call 内核。两边都开放 API SDK、Terraform、MCP。PagerDuty 多开源的是「怎么做事故响应」的文档和一些内部基础设施库；Flashduty 多开源的是夜莺/Categraf 这条监控内核。

Events API（v2）是事实标准：很多监控工具「兼容 PagerDuty」只是会打这条协议。Flashduty 甚至提供 PagerDuty 兼容接入，说明 PD Events API 已经是行业插头。

---

## 10. 实现线索

- 子域租户：`{account}.eu.pagerduty.com`，登录走 `identity.pagerduty.com` 全球认证。
- 前端 SPA，资源 URL 带短 ID。
- 实时性：Operations Console 文案强调 live updates；普通列表更像轮询+手动刷新。
- 移动 App、电话、短信、Slack/Teams 是触达面；Web 是配置和分诊。
- Generic Webhooks v3、REST、Events API、Terraform 构成集成面。
- 服务依赖明确说可以用 API/Terraform 从代码灌入——他们不打算自建 CMDB，只收投影。
- 语言策略很诚实：产品灵魂是英语，本地化不完整。对中国团队不友好，对跨国 SRE 标准。

---

## 11. 设计原则（从细节反推）

1. **Service 是责任边界，不是聊天室。** 事故必须挂服务，服务必须挂升级链。
2. **叫醒可靠高于协同热闹。** 通知规则页的两条橙警告，比任何 AI 文案都更能代表产品。
3. **Urgency ≠ Severity。** 前者是叫人方式，后者/Priority 才是管理标签。
4. **个人拥有自己的触达偏好，组织拥有升级链。** 两层分开，避免管理员替所有人决定「先电话还是先邮件」。
5. **先给 Default Service + Default EP，让十分钟能接到第一封信。** 复杂排班、业务服务、编排都是后来的。
6. **用 Standards 分数治理目录质量。** 空服务会一直红。
7. **内核免费可用，智能和自动化加购。** 过期试用账号把这条线画得很清楚。
8. **方法论开源，引擎闭源。** IR 手册是品牌，也是实施顾问话术。
9. **不建模 Fault。** 技术故障和响应事故在产品里是同一张 Incident，顶多再用 Business Service 表达影响面。
10. **Slack 是协同层，产品自己不做频道首页。** 顶栏第一项永远是 Incidents。

---

## 12. 本账号没能点到、因此不能装成已证实的部分

- 真实事故详情页（时间线、conference、status update、postmortem、related alerts）——列表为空。
- Teams 完整管理页——People 下未出现。
- 创建 Schedule 向导的每一层字段——只看到空态和 New Schedule 按钮，未保存。
- Event Orchestration 规则编辑器内部。
- Live Call Routing、Customer Service Ops 的真实配置。
- AI Agents 运行中的界面。

这些以官方文档和销售页为辅证，不当作控制台实测。

与快猫星云、以及本仓库 SRE Control 的对照，见 `2026-08-20-flashduty-pagerduty-sre-control-comparison.md`。
