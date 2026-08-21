# 快猫星云 / Flashduty 产品分析报告

调研日期：2026-08-20  
调研账号：已登录 `console.flashcat.cloud`，组织展示名为 `AI SRE`  
控制台入口：`https://console.flashcat.cloud/channel`  
文档入口：`https://docs.flashcat.cloud` / `https://docs.flashduty.com`  
文档索引：`https://docs.flashcat.cloud/llms.txt`

本报告同时依据：

1. 浏览器逐页走查控制台（协作空间、故障/告警列表、值班、分析、配置中心、状态页、AI SRE、RUM）。
2. Flashduty 官方文档（On-call、AI SRE、Monitors、RUM、平台、OpenAPI）。
3. 开源仓库（`ccfos/nightingale`、`flashcatcloud/categraf`、`flashduty-cli`、`go-flashduty`、`flashduty-mcp-server` 等）。

---

## 1. 产品身份：不是一个产品，而是一条产品线

快猫星云对外是「一站式智能观测 + On-call」品牌，控制台实际拆成几条可独立售卖、又能互相打通的产品线。

| 产品 | 形态 | 解决什么 | 控制台入口 |
| --- | --- | --- | --- |
| **Flashduty On-call** | SaaS 告警响应 | 把已触发告警变成可分派、可升级、可在 IM 闭环的故障 | `/channel`、`/incident`、`/alert`、`/oncall` |
| **Flashduty AI SRE** | 自治排障 Agent | 对话/作战室/自动化里调查根因、跑诊断、沉淀知识 | `/ai-sre/chat` |
| **Flashduty RUM** | SaaS 真实用户监控 | Web/App/小程序体验、错误、会话重放 | `/rum` |
| **Flashduty Monitors** | 统一告警引擎 | 对接 Prom/ES/Loki/CH/MySQL 等做规则求值 | 文档独立模块 |
| **Flashcat 企业版** | 可私有化全栈观测 | 夜莺内核 + 北极星/灭火图/事件墙/日志分析 | 独立部署文档 |
| **Nightingale + Categraf** | 开源内核 | 告警引擎 + all-in-one 采集器 | GitHub |

官方自我定位非常清楚：Flashduty **不产生告警**，只做统一告警中心。产生告警的是夜莺、Prometheus、Zabbix、云监控或 Monitors。On-call 负责降噪、分派、通知、协同、复盘。

这与「监控平台」和「事故响应操作系统」是两层。Flashduty 的主战场是后者，但用同一账号把 RUM / Monitors / AI SRE 绑在一起，形成「观测 → 告警 → 响应 → 自治排障」的商业套件。

登录页品牌文案是 `Flashduty — 一站式告警`，营销句是 `Noise reduction, No more alert storm`。控制台顶栏则写成 `Flashduty - 一站式运维`。产品叙事已经从「值班电话」扩到「运维工作台 + AI Agent」。

---

## 2. 控制台信息架构（浏览器实测）

登录后默认落在协作空间，而不是故障列表。这是最重要的产品选择。

### 2.1 顶栏

- 产品名：Flashduty
- 一级切换：`On-call`、`协作空间`、`下载桌面版`
- 用户入口：右上角显示 `AI SRE`（组织/账号标识）

### 2.2 左侧导航（图标栏 + 可展开组）

实测可点到的入口：

| 分组 | 子项 | 路由 | 空账号状态 |
| --- | --- | --- | --- |
| On-call 默认工作台 | 协作空间列表 | `/channel` | 仅有系统 `Default` 空间 |
| 故障 | 故障列表 / 告警列表 | `/incident`、`/alert` | 空列表，引导创建 |
| AI SRE | 聊天、自动化、产物、插件、知识库、环境 | `/ai-sre/chat` 等 | 欢迎页 + 绑定 CTA |
| 单聊列表 | 一对一会话 | 侧栏入口 | 空 |
| 收发令箱 | 通知收件箱 | 侧栏入口 | 空 |
| 分析看板 | 用量数据、分析数据 | `/dashboard/usage`、`/dashboard/insights/overview` | 用量有图；洞察要付费 |
| 配置中心 | 值班、模板、映射、表单、字段 | `/oncall`、`/template`、`/mapping`、`/forms`、`/fields` | 多数空态/配额 |
| 快速开始 | 新手引导 | 侧栏入口 | — |
| 底部产品切换 | RUM | `/rum` | 营销落地页 |

「服务目录」在侧栏出现过，但实测跳到日历/值班视图，**不是 CMDB**。`/service`、`/route`、`/account/team` 直接 404。团队、成员、SSO、审计更可能藏在账号菜单或独立组织管理页，本账号未完全展开。

### 2.3 协作空间是真正的首页

`/channel` 卡片列表展示：

- 空间名：`Default`
- 统计：`0` 故障、`0` 告警
- 操作：`创建协作空间`、`创建频道`

点进 Default：`/channel/detail/2618595391671`

两个主 Tab：

1. **消息**：按类别看故障统计，聊天/时间线式工作面。
2. **配置**：左侧再拆一套完整控制面。

配置子菜单（控制台原文）：

- 集成目录
- 专属通道
- 分派规则
- 排班规则
- 降噪设定
- 警示升级
- 抑制规则
- 消息通知
- 通知规则
- 消费记录
- 分派记录
- 注释
- 变动记录
- 高级配置

集成目录里已经预置：自定义事件、Prometheus、阿里云监控 CM、阿里云 ARMS、腾讯云监控 CM、Flashmonit、邮件，以及 `+ 新增一个集成`。

**设计含义**：Flashduty 把「一个业务/一个团队的作战单元」做成一等公民。集成、降噪、分派、升级、通知都挂在这个单元上，而不是挂在全局 Service 上。

<img src="/opt/cursor/artifacts/flashduty_channel_logged_in.webp" alt="Flashduty 协作空间列表，Default 频道 0 故障 0 告警" />

<img src="/opt/cursor/artifacts/flashduty_channel_detail.webp" alt="Default 协作空间详情与配置侧栏" />

<img src="/opt/cursor/artifacts/flashduty_channel_integrations.webp" alt="协作空间集成目录：Prometheus、阿里云、腾讯云、邮件" />

---

## 3. 核心对象模型

文档把链路写成：

```
监控系统 → 事件 Event → 告警 Alert → 故障 Incident
```

控制台和文档用词必须对照看，否则会和我们自己的术语撞车。

| Flashduty 对象 | 官方定义 | 控制台表现 | 对应我们更接近的词 |
| --- | --- | --- | --- |
| **事件 Event** | 监控系统的一次触发或恢复推送 | 不单独做一级导航 | Signal / occurrence |
| **告警 Alert** | 同一 `alert_key` 在事件聚合窗口内的合入结果 | `/alert` 独立列表，「了解告警流转」 | Alert（证据） |
| **故障 Incident** | 相似告警的组合，是处理主对象 | `/incident`，可手动创建 | **不是我们的 Fault，也不是我们的 Incident**；更接近「被通知和处置的聚合告警单」 |
| **协作空间 Channel** | 管理故障的最小单元：隔离业务、权限、集成、分派 | `/channel` | 没有直接对应；有点像「团队工作区 + 路由目标」的混合物 |
| **集成 Integration** | 专属（直达空间）或共享（经路由规则分发） | 空间配置 / 集成中心 | AlertSource + hook |
| **分派策略** | 通知谁、怎么通知、超时如何升级 | 空间配置 | Escalation Policy + Notification Policy |
| **值班表 Schedule** | 连接故障与人 | `/oncall`、`/calendar` | Schedule |
| **团队 Team** | 管理值班、模板、映射的权限边界 | 表单里的「管理团队」 | Team |
| **状态页** | 对内/对外服务状态沟通 | `/status-page` | 未做 |
| **作战室 War Room** | 为故障自动建 IM 群 | 故障详情「创建作战室」 | Incident Room 的 IM 投影 |
| **变更事件** | 发布/配置变更，辅助根因 | 用量看板有「变更数量」 | Change Event（预留） |

关键澄清：

1. Flashduty 的「故障」**默认由告警自动创建**。无降噪时，故障 ≈ 告警；有告警聚合时，一条故障含多条告警。
2. 它没有独立的 **Fault（技术故障）** 和 **Impact（业务影响）**。爆炸半径、根因、症状角色都不在 On-call 主模型里。
3. 它也没有强制「人宣布 Incident」。自动触发是主路径，手动创建和外部提报是补充。
4. 协作空间不是聊天工具的附属品，而是配置边界。IM 作战室是故障发生后的协同层，不是配置根。

### 3.1 严重程度、进度、状态是三轴

文档写得很细，控制台创建表单也只暴露其中一部分。

**严重程度**（事件/告警/故障共用）：`Info` / `Warning` / `Critical`。  
告警取关联事件最高级，故障取关联告警最高级。来源枚举由集成映射，可用 Pipeline 改写。

**处理进度**（人怎么走）：

| API | 中文 | 含义 |
| --- | --- | --- |
| `Triggered` | 待处理 | 自动分派并通知 |
| `Processing` | 处理中 | 至少一人已认领；全部取消认领则退回待处理 |
| `Closed` | 已关闭 | 手动关闭或自动恢复 |

**故障状态**（监控事实）：已恢复 / 未恢复，完全由关联告警决定。

两条容易踩的规则：

- 自动恢复会把进度变成已关闭。
- 手动关闭会把关联告警一并关闭，之后不再合入新事件。

这与我们「Alertmanager 写 source_state，用户写 response_state，禁止对 AM 来源人工 Resolve」正好相反：Flashduty **拥有告警生命周期**，可以因为关单而切断上游后续合入。

<img src="/opt/cursor/artifacts/flashduty_create_incident.webp" alt="手动创建故障：标题、严重程度、协作空间、富文本描述" />

<img src="/opt/cursor/artifacts/flashduty_incident_list.webp" alt="故障列表：分派值班、协作空间、未关闭、最近 30 天" />

<img src="/opt/cursor/artifacts/flashduty_alert_list.webp" alt="告警列表与「了解告警流转」入口，说明告警和故障拆开" />

---

## 4. 端到端数据流

文档和工作流可以压成一条链：

```
监控/云/夜莺/邮件/HTTP Pull/DB Pull
        │
        ▼
   集成（专属 or 共享）
        │  共享集成必须有路由规则，否则丢弃
        ▼
 标签增强 / Alert Pipeline（过滤、改写、丢弃）
        │
        ▼
 事件聚合（同 alert_key → 一条告警）
        │
        ▼
 告警聚合 / 抖动 / 静默 / 抑制
        │
        ▼
      故障
        │
        ▼
 分派策略（可多条，按优先级匹配；可多环节升级）
        │
        ▼
 值班表 / 团队 / 个人 + 单聊/群聊渠道
        │
        ▼
 认领 / 暂缓 / 升级 / 作战室 / 行动项
        │
        ▼
 关闭 / 自动恢复 / 复盘 / 状态页
```

两条接入哲学：

- **专属集成**：告警直接进当前协作空间。新手路径，无需路由。
- **共享集成**：公司级 Prometheus 一类入口，按 `service` 等标签路由到多个空间。不配路由就丢，文档明确建议一条无条件兜底规则。

接入限流：每集成 **100 次/秒、1000 次/分钟**，超限 429。文档还警告故障堆积会让检索变慢，系统可能静默关闭历史故障。

---

## 5. 降噪：产品真正的护城河

Flashduty 把降噪做成协作空间级能力，而不是全局「智能开关」。文档分层非常清楚。

### 5.1 事件聚合（Event → Alert）

- 默认开启，窗口 24 小时（1–1440 分钟）。
- 同 `alert_key` 在窗口内合入同一告警。
- 关闭后，每条事件都变成独立告警。

`alert_key` 由上游或集成规则生成，相当于他们的 fingerprint。

### 5.2 告警聚合（Alert → Incident/故障）

- 默认关闭。不开则一告警一故障。
- 两种模式：
  - **智能聚合**：语义相似度，默认可选字段 `title`、`description`、`labels.service`、`labels.resource`，最多 10 个字段。
  - **规则聚合**：指定维度精确相等。可「统一控制」或「细粒度分支」（最多 100 条 case，每组维度 ≤ 5）。
- 聚合窗口可选；计时起点可以是「故障触发」或「每次合入重置」。
- 默认窗口上限 24 小时，可扩到 30 天。
- **风暴预警**：合入数达到阶梯阈值（最多 5 档，2–10000）时写时间线并再通知。
- **严格聚合**：空标签视为不同（智能聚合不支持）。

单故障合入告警上限约 5000，为了控制台渲染。

### 5.3 抖动检测

默认「仅提醒」。可配：

- 状态变化次数 2–100，默认 4
- 观测窗口 1–1440 分钟，默认 60
- 静默时长 30–1440 分钟，默认 120（仅「提醒后静默」）

判定对象是相同 Alert Key 的故障。

### 5.4 静默

- 单次 / 周期（星期） / 周期（服务日历工作日/休息日）
- 条件：严重程度、标题、描述、集成来源、标签；AND/OR
- 行为：直接丢弃 **或** 保留并打标
- **快速静默**：从故障详情一键生成，自动排除 JSON/纯数字/过长/时间格式标签，以及账户级排除规则

免费档也有静默；聚合、风暴、抑制要标准版及以上。

### 5.5 抑制

当 10 分钟内存在满足条件的活跃告警（未认领且未恢复），且与新告警有相同项时抑制。  
可配在协作空间或集成 Pipeline。行为同样是丢弃或打标。

这是典型的 Alertmanager inhibit，但做成了产品化表单，并且挂了「宿主机抑制 Pod」这类宣传场景。

---

## 6. 分派、升级、值班、通知

### 6.1 分派策略挂在协作空间，不挂在 Service

创建空间时就要配一条默认分派策略：

- 通知对象：值班表 / 团队 / 个人
- 通知模板：必填
- 延迟窗口：0–3600 秒，窗口内自动或手动关闭则不发
- 可选 IM 群聊渠道

空间内可有多条策略，**按优先级从上到下匹配，命中即停**。  
一条策略里可以有多个升级环节：当前环节超时未确认，自动进入下一环节。

对照 PagerDuty：一个 Service 只绑一条 Escalation Policy。Flashduty 用「多策略 + 条件匹配 + 动态分派」换灵活性。

动态分派可以根据标签 reset/append 人员、团队、群聊，并能结合外部映射（CMDB/CSV）。文档把它当作打通自研系统的主能力。

没命中任何分派策略：**不通知任何人**。这和我们「必须落到默认团队、禁止静默丢弃」不同，更危险，也更「配置责任制」。

值班表轮空：不给个人发单聊，但仍可推群聊。

认领没有「只有被分派的人才能点」的限制，任何人看到都能认领。

### 6.2 值班表能力很深

创建表单实测字段：

- 值班名称（1–40，必填）
- 管理团队（可现场创建团队）
- 值班描述（富文本）
- 换班通知：直接通知 / 定时通知

文档里的完整模型更重：

- 一个值班表多层规则（白班/夜班叠加）
- 轮换周期：小时/天/周/月，1–30
- 日期掩码、连续轮换、公平轮换
- 账户级值班角色（不只主备，可自定义「SRE 响应官」）
- 临时调班完全抢占窗口内普通规则
- 交接提前通知 + 每日定时播报
- 个人值班计划视图（1 天到 1 月）

侧栏「服务日历」用于定义工作日/休息日，给分派和静默做日期类型差异（证券交易日是典型例子）。

<img src="/opt/cursor/artifacts/flashduty_oncall_form.webp" alt="创建值班表：名称、管理团队、描述、换班通知" />

### 6.3 通知渠道是中国市场的产品核心

文档和支持项：

- App 推送（iOS Critical Alerts 破勿扰；Android 国内厂商通道）
- 语音、短信、邮件
- 飞书 / 钉钉 / 企微（含私有化版本）
- Slack / Microsoft Teams / Zoom / Telegram 机器人

通知模板可全渠道自定义，含语音模板，支持用历史故障预览。  
个人中心绑定手机、邮箱、IM 账号后，才能真正接到电话/短信。

用量看板按 SMS / Email / Voice 分开计数，说明计费和成本意识直接做进产品。

<img src="/opt/cursor/artifacts/flashduty_usage_dashboard.webp" alt="分析看板用量：通知数量、故障数量、变更数量" />

---

## 7. 响应层：作战室、行动项、复盘、状态页

Flashduty 把「找到人」和「找到人之后怎么闭环」拆成 On-call 与 Response。

### 7.1 作战室

专业版功能。围绕一条故障在飞书/钉钉/企微/Slack 建专属群：

- 自动拉处理人
- 状态实时同步
- 群内认领/关闭/暂缓回写平台
- 行动项/跟进项以纯文本回帖
- 关闭后可解散
- 同一时间只允许一个 IM 集成开作战室
- 企微要求至少 2 名已关联成员

作战室创建后，**AI SRE 会自动跑一轮初步诊断并回帖**。这是它和 PagerDuty Slack 频道最不一样的地方：群不只是聊天，还是 Agent 的默认工作面。

### 7.2 故障上的其它响应对象

- 行动项 → 可转为跟进项 → 可关联复盘
- 自定义操作（Webhook，执行前二次确认）
- 自定义字段、创建/认领/关闭三段自定义表单
- 相似故障、新奇故障
- 手动创建 / 外部免登录提报链接
- 时间线精确到秒，悬停变相对时间

### 7.3 状态页

`/status-page` 分公开（本账号 0/1）和内部（0/0）。  
文档能力包括：组件与分组、故障/维护事件、模板、邮件/RSS/Atom 订阅、Widget、自定义域名、从 Atlassian Statuspage 迁移。  
免费档就带公开状态页，和 PagerDuty 按套餐砍功能形成对比。

<img src="/opt/cursor/artifacts/flashduty_status_page.webp" alt="状态页：公开 0/1，内部 0/0" />

---

## 8. AI SRE：已经是一级产品，不是开关

`/ai-sre/chat` 欢迎语：「下午好，SREMonitor」。能力标签：值班轮换、智能定责、快速收敛。底部有「立即绑定 AI-SRE」，并声明仍在专业用户开发阶段。

侧栏：

- 聊天线 / 会话
- 自动化（从零编写、事件监听、延迟判断、On-call 自动分析、复盘自动分析）
- 产物（会话产出的报告/网页/PDF）
- 插件 Apps：Kubernetes、GitHub、GitLab
- Skills / MCP / Agents（A2A）
- 知识库（DUTY.md + runbook + 服务清单）
- 环境：自托管 Runner vs 云端 Sandbox

文档把差异说得很满：Flashduty 做一个开放 Agent loop（沙箱/BYOC Runner、Skill、MCP、A2A），PagerDuty 则是 AIOps + Advance + 四个预置 Agent。宣传口径需要打折，但控制台信息架构已经把 AI 放到和 On-call 平级。

<img src="/opt/cursor/artifacts/flashduty_ai_sre.webp" alt="AI SRE 对话欢迎页与绑定入口" />

---

## 9. 平台底座与商业包装

从文档和控制台付费墙可以看到清晰的商业分层：

- 洞察分析、自定义字段、自定义模板、日历高级视图：升级后可用
- 通知模板有系统预置且不可删
- 元数据映射 `/mapping` 限额 0/20
- 状态页公开 0/1
- 分析数据页直接「暂无权限」

平台能力还包括：组织信息、RBAC（功能角色 + 团队数据权限）、SSO（Authing/Keycloak/LDAP）、审计日志、自定义侧栏菜单、337 个 OpenAPI、CLI、MCP、Terraform、Go SDK。

告警还能做：

- 标签增强（正则、映射表、外部 API）
- HTTP Pull / DB Pull（不会推 Webhook 的老系统）
- 兼容 PagerDuty Events API
- 出站 Alert/Incident Webhook，可查完整调用历史
- Jira / ServiceNow / SDP 双向同步

---

## 10. 开源项目：内核开源，On-call 闭源 SaaS

| 仓库 | 作用 | 和 Flashduty 的关系 |
| --- | --- | --- |
| [ccfos/nightingale](https://github.com/ccfos/nightingale) | 开源告警引擎，对标「Grafana 可视化 + Prometheus 规则」里的告警半边 | Flashcat 企业版内核；文档明确「只做值班请用 Flashduty/PagerDuty」 |
| [flashcatcloud/categraf](https://github.com/flashcatcloud/categraf) | All-in-one 采集器，1.3k star | 指标 Remote Write 进夜莺；README 直接导流 Flashduty |
| [flashcatcloud/ibex](https://github.com/flashcatcloud/ibex) | 夜莺自愈任务执行 | 监控侧自动动作，不是 On-call |
| [flashcatcloud/n9e-helm](https://github.com/flashcatcloud/n9e-helm) | 夜莺 Helm | 私有化交付 |
| [flashcatcloud/flashduty-cli](https://github.com/flashcatcloud/flashduty-cli) | 官方 CLI | 故障/值班/状态页/模板 |
| [flashcatcloud/go-flashduty](https://github.com/flashcatcloud/go-flashduty) | 官方 Go SDK，号称覆盖 337 API / 39 service | SaaS API 的类型化外壳 |
| [flashcatcloud/flashduty-mcp-server](https://github.com/flashcatcloud/flashduty-mcp-server) | 官方 MCP | 把 On-call API 接到 Cursor/Claude |
| [flashcatcloud/terraform-provider-flashduty](https://github.com/flashcatcloud/terraform-provider-flashduty) | IaC | 配置即代码 |
| [flashcatcloud/flashduty-tools](https://github.com/flashcatcloud/flashduty-tools) | 辅助脚本 | 迁移/运维 |
| [flashcatcloud/knowledge-base](https://github.com/flashcatcloud/knowledge-base) | 文档源 | 对应 docs.flashcat.cloud |
| 大量 `fc-sdk-*` | RUM SDK（Web/iOS/Android/Harmony/Flutter/Electron/小程序） | 从 Datadog SDK fork 痕迹明显 |

**结论**：开源护城河在采集和告警引擎（夜莺/Categraf），不在 On-call 内核。Flashduty 本体是闭源 SaaS，只开放 API/CLI/MCP/Terraform 这圈生态。RUM 明显走「兼容 Datadog SDK 协议/实现」的路线。

夜莺自己的边界声明值得原文记下：如果你要的是跨云、跨监控系统的统一值班、降噪、分派，请用 Flashduty 或 PagerDuty，不要用夜莺硬做 On-call。

---

## 11. 实现与交互线索

- 前端是现代 SPA，路由全部客户端切换：`/channel/detail/{id}?tab=integration`、`/incident`、`/ai-sre/automations/new`。
- ID 是长数字（`2618595391671`），不像 PagerDuty 的 `Pxxxxxx`。
- UI 语言中英混用：On-call、AI SRE、MCP、RUM 不翻译；配置项全中文。
- 组件风格接近 Ant Design：表格、抽屉、空态插画（紫猫）、必填星号、卡片列表。
- 空态文案普遍带下一步 CTA（「创建第一个值班」「尝试创建第一个故障」）。
- 付费墙不是整页封死，而是按钮/创建额度/「暂无权限」。
- 集成采用「复制推送地址到监控平台」的 webhook 模式，和我们的 hook UUID 同一类。
- 桌面客户端、移动 App、IM 应用是一等触达面，Web 控制台更像配置和检索后台。

---

## 12. 产品设计原则（从细节反推）

1. **协作空间是配置根，故障是工作根。** 人先进入自己的空间，再看空间里的故障。
2. **降噪发生在通知之前的两层聚合。** 先按 key 合成告警，再按规则/相似度合成故障。
3. **IM 是执行面，不是装饰。** 认领、升级、作战室、AI 回帖都在群里完成。
4. **中国云 + 中国 IM 是默认世界。** 阿里/腾讯/华为/火山、飞书/钉钉/企微、国内短信语音，比 750+ 海外集成更重要。
5. **自动建单是默认，人只做认领和关闭。** 没有「先证明业务影响再宣布事故」。
6. **AI 被做成产品线，用来讲差异化。** 控制台一级入口已经超过 On-call 本身的复杂度。
7. **免费/标准/专业用配额和权限墙切割。** 空账号能看懂模型，但不能用完能力。

---

## 13. 对本项目有用的观察，以及不能照抄的点

值得学：

- 事件 / 告警 / 故障 三层对象在 UI 上拆开，并有「了解告警流转」教学入口。
- 专属集成 vs 共享集成 + 兜底路由，把「一个团队试水」和「公司级 Prometheus」拆开。
- 快速静默从已有故障生成 matcher，并解释哪些标签被丢掉。
- 用量看板把 SMS/Voice 成本暴露给管理员。
- 空态和创建表单字段少，先让人建出第一条故障。

不能照抄：

- 把自动聚合告警叫做「故障」，再把需要跨团队响应的东西也叫同一词。我们已用 Fault / Incident 切开。
- 未匹配分派策略就静默不通知。
- 手动关单连带关闭告警（会和 Alertmanager 权威冲突）。
- 用协作空间同时充当拓扑、权限、路由、聊天室，最后会变成「什么都是 Channel」。
- 把 AI 做成一级导航但核心 Fault 图还没稳。Flashduty 可以，因为它已经是成熟 On-call SaaS。

更完整的三方对照见同目录 `2026-08-21-alert-system-comparison.md`。
