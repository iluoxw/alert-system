# 快猫星云 Flashduty 产品分析报告

- 调研日期：2026-08-21（Asia/Shanghai）
- 读者：正在做 SRE Control（`iluoxw/alert-system`）的人。目标不是写一篇功能清单，而是把 **产品怎么被设计出来、控制台怎么被实现出来** 钉死到可引用的事实。
- 材料纪律：只使用两份已读材料，不补拍、不编数字、不编截图。
  - 公开文档与开源仓库：`/workspace/research/flashcat-docs-oss.md`（未登录控制台；未 clone 仓库）
  - 已登录控制台实勘 Pass 1：`/workspace/research/flashduty-console-pass1.md`（组织 **SREMonitor**，Owner，Org ID `2618591231671`，UI 语言 English，夹杂残留中文）
- 方法边界：Pass 1 只做导航 / IA 盘点 + Channel 深潜；未创建、未删除、未保存任何对象。分派规则抽屉打开过一次做字段采集后取消。未打开的面板（事件聚合、告警聚合、抖动、静默、抑制、排除、集成向导、故障详情、RUM 子页、AI SRE 子页等）一律用文档补，并标明「控制台未核验」。
- 术语先钉死：**Channel / 协作空间 ≠ 通知媒介**。控制台路径 `/channel`、Terraform 资源 `flashduty_channel`、文档「协作空间」是同一对象：服务 / 团队的告警管道与协作容器。真正的通知渠道是 App / 语音 / 短信 / 邮件 / IM 应用或机器人，挂在分派策略的 Notify by、个人偏好、以及 Integrations → Instant messages 上。

---

## 1. 产品定位与族谱

快猫星云（Flashcat）对外自称「智能时代的全栈可观测性解决方案提供商」，覆盖指标、日志、APM、RUM、监控告警和 On-call，并把 AI 放在故障发现、根因分析、事件响应这条故事线上（[flashcat.cloud](https://flashcat.cloud/)）。真正卖货时，它拆成三条商业线和两条开源线，而不是一个产品五个皮肤。

| 产品 | 交付形态 | 官方一句话 | 在族谱里干什么 |
| --- | --- | --- | --- |
| **Flashcat 企业版** | 可私有化 | 全栈智能观测平台 | 以夜莺为内核，在数据层 / 平台层之上加场景层（北极星 / 灭火图 / 事件墙）和智能层（FlashAI） |
| **Flashduty** | SaaS | 智能告警响应平台：降噪、值班、分派、升级、触达、分析 | 多源告警进入之后的统一事件处理；文档里已扩成 SaaS 可观测套件 |
| **RUM** | SaaS | 真实用户监控：会话录制与重放 | 在 Flashduty 控制台里是独立产品轨 |
| **Nightingale（夜莺 / n9e）** | 开源 | 云原生监控告警系统 | 采集对接、存储、可视化、告警规则、告警事件**生成** |
| **Categraf** | 开源 | All-in-one 数据采集器 | 夜莺推荐的采集器；指标 + 日志，PUSH / Prometheus remote write |

来源：[flashcat.cloud](https://flashcat.cloud/)、[flashcat.cloud/docs](https://flashcat.cloud/docs/)、[夜莺 vs Flashduty](https://flashcat.cloud/blog/nightingale-vs-flashduty/)、[企业版 vs 夜莺](https://flashcat.cloud/blog/flashcat-vs-n9e/)。

文档镜像关系：`docs.flashcat.cloud` 与 `docs.flashduty.com` 内容一致，后者提供完整 `llms.txt`。`help.flashcat.cloud` 未作为独立文档站出现。

### 1.1 公司、品牌、产品三个名字不要混

日常口语里「快猫 / Flashcat / Flashduty」经常被当成同一个东西。公开材料里的分层是：

```
                    快猫星云 Flashcat（公司）
                    /          |           \
           开源内核层        私有化企业版         SaaS 套件
        Nightingale        Flashcat 企业版      Flashduty
        Categraf           北极星 / 灭火图 /      ├ On-call
        Ibex（自愈）        事件墙 / 日志分析      ├ Monitors
        Huatuo（eBPF）     中心-边缘              ├ RUM
        Open-Falcon 血统                         └ AI SRE
                    \___________________________/
                         夜莺通知媒介 → Flashduty 协作空间
```

来源：`flashcat-docs-oss.md` §2。

Flashduty 文档首页把 SaaS 再拆成四个产品面：AI SRE（公测）、On-call、RUM、Monitors（[docs.flashduty.com/zh/home.md](https://docs.flashduty.com/zh/home.md)）。这与官网「Flashduty = On-call SaaS」的简写并存：文档口径里 Flashduty 已经是 **SaaS 可观测性套件品牌**，On-call 只是其中核心。控制台实勘完全印证了这一点——最左侧产品轨就是 AI SRE / On-call / RUM / Monitors 四条，On-call 是默认落地产品（控制台 `/channel`）。

Flashduty 也可以自带告警引擎（Monitors），并不只是「只收别人告警」（[visualization-via-grafana-and-monitoring-via-flashduty](https://flashcat.cloud/blog/visualization-via-grafana-and-monitoring-via-flashduty/)）。这对 SRE Control 很重要：Flashduty 的产品边界已经从「PagerDuty 中国替身」扩到「On-call + 自己也能产生告警」。

### 1.2 夜莺 vs Flashduty：官方不互相替代

官方边界写得很硬（[nightingale-vs-flashduty](https://flashcat.cloud/blog/nightingale-vs-flashduty/)）：

- 夜莺：采集、存储、可视化、告警规则、告警事件**生成**。
- Flashduty：告警生成**之后**的统一事件处理（降噪、排班、认领、升级、协同、闭环）。
- 多监控源或响应流程复杂时，夜莺 / Prometheus / Zabbix / 云监控在前，Flashduty 在后。

夜莺出身：滴滴开源，2022-05-11 捐赠 CCF ODC，是该委员会接受的第一个项目。主仓 [ccfos/nightingale](https://github.com/ccfos/nightingale)（2026-08-20 约 13253 stars，Go，Apache-2.0）；前端 [n9e/fe](https://github.com/n9e/fe)（约 301 stars）。自身不采集数据，推荐 Categraf。进程是 `n9e`（中心，默认 17000）+ `n9e-edge`（边缘告警引擎，默认 19000）。测试用 SQLite（`n9e.db`），生产用 MySQL（库名仍叫 `n9e_v6`）+ Redis。通知内置约 20 种媒介，v8 起抽象「通知规则」。

Categraf：[flashcatcloud/categraf](https://github.com/flashcatcloud/categraf)（约 1300 stars，Go，MIT）。TOML 配置、`[[instances]]` 多实例，指标命名 follow Telegraf。README 推荐用 Flashduty 做 On-call。

企业版四层（[flashcat-intro](https://flashcat.cloud/blog/flashcat-intro/)、[产品页](https://flashcat.cloud/product/flashcat-enterprise/)）：

| 层 | 内容 |
| --- | --- |
| 数据层 | Categraf 采集 + 30+ 数据源（Prometheus / Zabbix / 云监控 / SLS 等） |
| 平台层 | 查询、仪表盘、告警、权限、组织、On-call 协同；指标存 VictoriaMetrics，日志 / 链路存 Apache Doris |
| 场景层 | 北极星（业务 VIP 指标 / SLO / 大屏）、灭火图（IT 对象健康全景 + 历史回放 + 下钻）、事件墙（变更 / 告警 / 发布时间轴） |
| 智能层 | FlashAI Agent：自然语言分析、巡检、规则建设 |

架构要点：水平扩展、无单点、**中心-边缘**；中心与边缘断网时边缘可独立闭环（[企业版 overview](https://flashcat.cloud/docs/content/flashcat/overview/)）。

### 1.3 典型串法：夜莺生成，Flashduty 收口

```
Categraf ──remote write──► n9e / n9e-edge ──► TSDB（VM / Prometheus）
                                │ 告警规则求值
                                ▼
                         AlertCurEvent JSON
                                │ POST /event/push/alert/n9e?integration_key=
                                ▼
              Flashduty 集成（专属 Channel 或共享 + 路由）
                                │ Event → Alert → Incident
                                ▼
              协作空间：聚合 / 静默 / 抑制 / 分派 / 升级 / IM / 作战室
```

夜莺的 FlashDuty 媒介**不走模板**：原样 POST `AlertCurEvent` 数组；恢复靠 `is_recovered: true`。推送地址形如 `https://api.flashcat.cloud/event/push/alert/n9e?integration_key=...`，通知规则可选 `?channel_id=` 打到多个协作空间（[夜莺 Flashduty 媒介文档](https://flashcat.cloud/docs/content/flashcat-monitor/nightingale-v9/usage/alert-notify/notify-channel/flashduty/)）。

周边开源：Ibex（告警自愈，`flashcatcloud/ibex` 约 46 stars，Categraf 连 RPC `:20090`）；Huatuo（`ccfos/huatuo`，eBPF，约 1087 stars，与夜莺同属 CCF，**不是** Flashduty 组件）；Open-Falcon 血统还在——Flashduty 仍有 Open-Falcon webhook 集成，兼容的是协议，不是现行主产品。

Flashduty 对标 PagerDuty（[comparison.md](https://docs.flashduty.com/zh/on-call/quickstart/comparison.md)）。On-call 文档自称已服务数千家企业、处理超过 10 亿条告警事件（[on-call.md](https://docs.flashduty.com/zh/on-call.md)）——营销数字，材料未独立核验。

---

## 2. 控制台信息架构：三级导航、产品切换、套餐门控

本节以 **SREMonitor 已登录控制台** 为准（[console.flashcat.cloud](https://console.flashcat.cloud)，Pass 1）。文档侧栏是 `llms.txt` 的信息架构，和真实 UI 高度同构，但菜单名、顺序、灰度和门控文案必须以控制台为准。

组织身份：Owner 角色，Org ID `2618591231671`，用户菜单显示「SREMonitor · Owner」。UI 语言为 English，预置数据与部分 placeholder 残留中文（`默认模板`、`输入模板名称检索`、`自定义事件`、`邮件 Email`）。i18n 在数据 / 预置层不完整。

### 2.1 三级壳

控制台是一个固定壳 + 内容区的 SaaS SPA，不是文档那种「一个产品一个站点」。

**第一列：最左侧约 50px 图标轨 = 产品切换器（persistent，icon-only）**

- Flashduty logo → 营销站 `www.flashduty.com`
- **AI SRE** → `/ai-sre/chat`（带 beta 徽标，chat-first）
- **On-call** → `/channel`（默认落地产品）
- **RUM** → `/rum/dashboard/overview`
- **Monitors** → `/monit/rule/list`
- 底部簇：客服耳机、语言切换（文A）、主题（太阳 = 亮 / 暗）、帮助（?）、历史 / 通知（红徽标 `"3"`）、反馈（屏幕图标）、设备 / APP 图标、用户头像 `"SR"` → `/profile`

**第二列：约 165px 带标签的模块导航**（顶部写模块名 + 折叠开关）

| 产品轨 | 二级菜单（控制台实勘） |
| --- | --- |
| On-call | Channels · Incidents · Status Pages · Post Incident · Analytics（Usage, Insights）· Integrations · Configuration（Schedules, Calendars, Templates, Mappings, Custom Forms, Custom Fields）· Quick start（带动画 spinner / avatar） |
| AI SRE | New chat · Automations · Artifacts · Plugins · Knowledges · Environments · Chats list（搜索 + 过滤） |
| RUM | Analytics · Applications · Performance · Error Tracking · Session Explorer · Session Replay |
| Monitors | Overview · Entity Tree · Alerting rules · Targets · Datasources · Alerting engines · Event Monitoring |
| Platform（走头像，不在第二列） | Personal（Profile, Notification, APP Key, Flashduty APP）· Billing（Plan, Billing overview, Billing）· Organization（Organization, Members, Teams, Roles, SSO）· Audit（Audit logs） |

**第三列：内容区。** 页标题右侧几乎每页都有文档深链（"Learn about channels"、"What is an incident" …），右上角固定一颗紫色 **AI SRE** pill，打开侧栏助手。这是「教学型 SaaS」：空态、文档链、Quick start 动画同时存在。

头像菜单是 **三栏 popover**：Personal Center（姓名 / 邮箱、Profile、Notification、APP Key、Flashduty APP、Sign out）| Organization（Plan、Billing overview、Billing、Organization、Members、More）| **Switch Identity**（组织切换；当前只有 "SREMonitor · Owner" ✓）。文档口径与此对齐：个人中心管通知偏好 / APP Key（每账号最多 5 个）/ App 扫码绑定；组织管团队、角色、SSO、审计；主体账号的头像 / 名称即组织身份（[personal-settings.md](https://docs.flashduty.com/zh/on-call/configuration/personal-settings.md)）。

### 2.2 文档 IA 与控制台 IA 的对齐

文档顶层（[llms.txt](https://docs.flashduty.com/llms.txt)）是：平台 / 组织、On-call、Monitors、RUM、AI SRE、开发者、合规。控制台把「开发者 / 合规」收进文档站和头像里的 Plan / Audit，不占产品轨。On-call 文档树（协作空间 / 故障 / 复盘 / 状态页 / 分析 / 集成 / 配置）和二级菜单一一对应，只是英文标签化了：Channel = 协作空间，Post Incident = 复盘，Mappings = 过滤条件 / 标签映射。

文档点名、Pass 1 核过的 URL：

| URL | 含义 | 控制台状态 |
| --- | --- | --- |
| `/channel` | 协作空间列表 | 已访。1 张 Default 卡片 |
| `/channel/detail/2618595391671` | Default 空间详情 | 已访。`?tab=` 切配置 |
| `/incident` | 全局故障列表 | 已访。空表 |
| `/status-page` | 状态页 | 已访。Public 0/1，Internal 0/0 |
| `/post-incident` | 复盘 | Pro 门控 |
| `/dashboard/usage` | 用量 | 已访。Notifications / Incidents 两张图 |
| `/dashboard/insights/overview` | Insights | Standard 门控 |
| `/settings/source/alert` | 告警集成 | 已访 |
| `/settings/source/change` | 变更事件 | 已访。仅 Standard Change Event |
| `/settings/source/im` | IM | 已访。钉钉 / 飞书 / Slack / Teams / 企微 |
| `/settings/source/webhook` | 出站 | 已访。Alert / Incident / Custom Action / Jira / SDP / ServiceNow |
| `/oncall` | 值班表 | 已访。首次创建空态 |
| `/calendar` | 服务日历 | Pro 门控 |
| `/template` | 通知模板 | 已访。一条不可改的系统预置 |
| `/mapping` | 映射 | 已访。配额 0/20，创建禁用 |
| `/forms` | 自定义表单 | 已访。Declare / Acknowledge / Close 三个生命周期表单，均 Not configured |
| `/fields` | 自定义字段 | Standard 门控 |
| `/profile` | 个人资料 | 已访 |
| `/ai-sre/chat` | AI SRE | 已访。问候 + composer |
| `/rum/dashboard/overview` | RUM | 已访。仅模块导航，内容仍在 Loading |
| `/monit/rule/list` | Monitors 规则 | 已访。要求先装 Alert engine |

文档还写了 `https://api.flashcat.cloud`（唯一 Open API / Event API）和 `https://mcp.flashcat.cloud/mcp`（远程 MCP）。控制台前端自己打的是同源 `/api/…`，例如 `GET /api/team/list`（Pass 1）。

### 2.3 套餐门控：这套租户是 Free / 低档

Pass 1 没有打开定价页，所以**不写现价**。能写死的是门控表现和文档版本要求。

控制台统一空态：猫咪吉祥物 + 盾锁，文案 *"This feature is only available for Pro/Standard and above users"*，紫色 **"Upgrade now"**。命中页面：

| 页面 | 路径 | 门控档 | 实勘 |
| --- | --- | --- | --- |
| Post Incident | `/post-incident` | Pro | No permission |
| Analytics → Insights | `/dashboard/insights/overview` | Standard | No permission |
| Configuration → Calendars | `/calendar` | Pro | No permission |
| Configuration → Custom Fields | `/fields` | Standard | No permission |

文档补充的功能门控（各页「版本要求」，未在本租户逐一点开核验）：聚合 / 风暴 / 抑制需标准版+；模板、作战室、新奇故障、外部提报、行动项、AI 产物公开链需专业版+（[permission-design.md](https://docs.flashduty.com/zh/platform/permission-design.md) 及各功能页）。

配额和禁用按钮也是门控的一部分，而且做得很外显：

- Channels：`Create channel` **disabled**，带 `oper-forbid` class。文档：免费版只能建 **1** 个协作空间（[create-edit.md](https://docs.flashduty.com/zh/on-call/channel/create-edit.md)）。本租户恰好 1 个 Default，创建按钮灭掉，和文档一致。
- Status Pages：Public Status Page **0/1**（可点 Create）；Internal Status Page **0/0**（创建按钮 disabled）。
- Templates：`Create custom template` disabled；表里一条 `默认模板 / 系统预置模板，不可更改`。
- Mappings：标题旁 **「0 / 20 Created」**，`Create mapping schema` disabled。
- Custom Forms：三个生命周期表单都在，只是「Not configured」——表单框架免费可见，字段定制才 Standard。

结论：SREMonitor 是一条被故意做瘦的 Free / 低档租户。On-call 主路径（Channel、Incident、Integrations、Schedules、Usage、默认分派）能看；分析、复盘、日历、自定义字段、再建空间被挡。Pass 1 据此判断「租户在 Free/low tier」，材料里没有打开 Plan 页做二次确认。

### 2.4 各页实勘摘要（不编列、不编空态）

**Channels `/channel`。** 标题 "Channel" + 文档链。工具栏：Teams 下拉、"My starred" 开关、搜索 "Enter channel name or description"、帮助、**Create channel**（禁用）、按 "Create time" 升降序、grid / list 视图切换。卡片网格；仅一张 **Default**，绿色状态点 + 「0 Triggered / 0 Processing」。

**Incidents `/incident`。** 分段页签 "Assigned to me / My channel"；状态下拉 "Open"；时间 "Last 30 days"；搜索图标；warning badge；"View alerts" 链；紫色 **Create**；导出；列设置齿轮；"+ Filters" chip。表头全选 checkbox。空态 *"No data found, try other filters"*。这是「先给筛选器、再给空态」的检索型列表，不是引导创建型。

**Usage `/dashboard/usage`。** 两张图：Notifications、Incidents。维度下拉（Channel）、Day / Week / Month、范围 "Last 2 weeks"。折线系列 SMS / Email / Voice（粉 / 紫 / 青）。即使是 Free 租户，用量看板也在——计费和「你已经用了多少触达」是同一产品面。

**Integrations `/settings/source/alert`。** 标题 "Multiple ways to receive alerts"。顶 Tab：**Alert events / Change events / Instant messages / Webhook / Other**，右侧还可按 integration key 搜。双栏：左目录（可搜索 logo 卡片，链到 `/settings/source/<kind>/add/<slug>`），右实例表（Integration, Team, Latest event time, Status, Actions），带 Status / Type / Teams 过滤，分页 "Total 0 items"、10/page。Change events 只有 Standard Change Event。IM：DingTalk、Feishu/Lark、Slack、Microsoft Teams、WeCom（表列无 Latest event time）。Webhook：Alert Webhook、Custom Action、Incident Webhook、Jira、ServiceDesk Plus、ServiceNow。

告警源目录 Pass 1 看到约 **55** 项，中国云厂商长尾很重，字母序排在一组 curated「popular」头之后。点名见到的包括 Prometheus、Grafana、Zabbix、Nightingale/Flashcat、Uptime Kuma、Sentry、Dynatrace、Splunk、Nagios、PagerDuty、Cloudflare、Keep、ElastAlert2、Skywalking、Graylog、Harbor、HTTP Pull、OceanBase、zilliz、ZStack，以及阿里 / 腾讯 / 华为 / 百度 / 火山 / 京东 / UCloud / AWS / Azure / GCP 云监控家族。文档 `llms.txt` 写的是 **100+**。两个数字不要合并：目录卡片数是 Pass 1 点数，文档是全量清单口径（还含邮件、蓝鲸、观测云、监控宝、Meraki、Monit 等，控制台 Other / 搜索里可能还有，Pass 1 未穷举）。

**Schedules `/oncall`。** 富空态：吉祥物 + "Create first schedule"，解释文案提到 daily rules、holiday overrides、roles via support，CTA "Start schedule planning"。角色库要找支持开通——这和文档「账户级角色库（主 / 备或自定义如 SRE 响应官）」一致。

**Quick start** 在 On-call 二级菜单最底部，带旋转头像，Pass 1 未点开。它和每页文档链、空态 CTA 是同一套 onboarding 系统。

---

## 3. 核心对象模型：Event → Alert → Incident，Channel = 协作空间

这是整张产品的脊柱。文档把它写成三层对象（[noise-reduction.md](https://docs.flashduty.com/zh/on-call/channel/noise-reduction.md)、[what-is-incident.md](https://docs.flashduty.com/zh/on-call/incident/what-is-incident.md)）：

```
监控系统 → 事件 Event → 告警 Alert → 故障 Incident
```

| 对象 | 定义 | 关键键 | 谁拥有它 |
| --- | --- | --- | --- |
| 事件 Event | 上游每一次触发 / 恢复 | 原始 payload | 集成（专属或共享） |
| 告警 Alert | 同 `alert_key` 在事件聚合窗口内合并 | `alert_key`（≤255，可自动生成） | 先属于集成，再被合入故障 |
| 故障 Incident | 相似告警的组合，On-call **主对象** | `incident_id`；短标识 `num`（ObjectID 后 6 位十六进制大写） | **协作空间 Channel** |

Channel 不是通知渠道，是「服务 / 团队的告警管道」。Pass 1 原话：它拥有集成（告警源）、降噪、分派 / 升级和指标；通知运输层分别住在 (a) 分派规则 Notify by、(b) Integrations → Instant messages、(c) 个人通知偏好。这是相对 PagerDuty「Service + 个人 notification rules」的关键设计差。

### 3.1 Channel 作为最小管理单元

文档：协作空间是故障管理最小单元。创建向导三步，后两步可跳过（[create-edit.md](https://docs.flashduty.com/zh/on-call/channel/create-edit.md)）：

1. **空间信息**：名称、所属团队、可选代管团队（最多 3，需开通）、公开 / 私有、超时自动关闭、跟随告警关闭、新奇故障、外部提报。
2. **默认分派策略**。
3. **挂接告警集成**，生成 Webhook。

本租户 Create channel 被禁用，向导本身未打开。能看到的是已经存在的 Default 空间，以及它的详情页如何把上述字段摊开。

空间详情是 **整页**（不是抽屉），面包屑 `Channel > Channel details`。页头：状态点、名称、复制 ID 图标、**Enabled** 状态下拉。ID 为 `2618595391671`，与 Org ID `2618591231671` 同一数量级前缀，像同一套 snowflake / 序列空间（见 §9）。

两个顶 Tab：

- **Incident**（默认）：4 张 KPI——过去一周 MTTA、过去一周 MTTR、过去一周故障数、降噪比（文案 *"Visible after alert grouping is enabled"*，各带 ? tooltip）。下面复用全局 Incidents 同一套表和工具栏（状态、时间、搜索 "incident title, id, labels…"、View alerts、Create、导出、列齿轮、+ Filters）。文档还写过「近 7 天同比」；控制台英文 KPI 卡写的是 "in past week"，未在 Pass 1 看到同比数字（本空间 0 故障）。
- **Configuration**：左侧内导航，按组，全部映射到 `?tab=`。

配置内导航（控制台标签 / `?tab=` / 文档分组）：

| 控制台分组 | 控制台项 | 已知 `?tab=` | 文档对应 |
| --- | --- | --- | --- |
| Integrations | Private integration | `integration` | 集成数据 · 专属集成 |
| Integrations | Drop rules | （Pass 1 未记 tab 名） | 排除规则 |
| Noise reduction | Event aggregation | 未点开 | 事件聚合 |
| Noise reduction | Alert grouping | 未点开 | 告警聚合 |
| Noise reduction | Flapping detection | 未点开 | 抖动 |
| Noise reduction | Silence rules | 未点开 | 静默 |
| Noise reduction | Inhibit rules | 未点开 | 抑制 |
| Dispatching | Escalation rules | `notificationPolicy` | 分派策略 |
| Settings | Basic | `baseSettings` | 设置 |
| Settings | Advanced | `advancedSettings` | 高级：超时关闭 / 新奇故障 / 外部提报 |

内部名 `notificationPolicy` 泄露 API 模型：UI 叫 Escalation rules，后端更接近「通知策略」。文档配置侧栏分组「集成数据 / 降噪处理 / 通知分派 / 设置」与控制台四组一一对应。

**Private integration** 文案："Create integrations in the current channel. Alerts will enter this channel directly." 已有卡片带中文遗留标签：自定义事件、Prometheus、腾讯云监控 CM、阿里云监控 CM、阿里云 ARMS、Flashmonit、邮件 Email，状态都是 "No alert events received yet"。另有虚线 "+ Add an integration"。这些是**专属集成**：不经共享路由，直达本空间。共享集成在全局 `/settings/source/alert`，要配路由才会进空间；不配路由会拒收新推送（[routing-rules.md](https://docs.flashduty.com/zh/on-call/integration/alert-integration/routing-rules.md)）。

**Basic settings**（已打开）：Name、Owning team（select + "Quick create team"，helper：「所属团队有管理权限……MTTA/MTTR 属于该团队」）、Description、Save（未 dirty 则 disabled）。

**Advanced settings**（已打开，三张 toggle 卡，全部 off）：

- Auto close on timeout
- Outlier incident detection（外链 Learn more）——文档「新奇故障」
- External reporting（"Disabled, only internal members can create incidents"）——文档「外部提报」

文档创建向导里还有「跟随告警关闭、公开 / 私有、代管团队」。这三项未在 Pass 1 的 Basic / Advanced 摘录里出现。可能在未展开的控件、未开通的代管、或未记录的字段里。**不要把文档向导字段当成控制台已看见的字段。**

Terraform 资源 `flashduty_channel` 含聚合、抖动、自动关闭，说明 Channel 的降噪和自动关闭是资源级字段，不是纯 UI 开关。

### 3.2 Incident 作为 On-call 主对象

文档级字段：id、num、title、description、severity、status、progress、labels（自动故障 = 首条告警标签；手动 = 空）、fields、responders、channel_id、group_method、时间戳（start / last / end / ack / close / snooze）。

严重程度首字母大写：`Info` / `Warning` / `Critical`。事件按集成映射；告警 = 关联事件最高级；故障 = 关联告警最高级。控制台 Notify by 矩阵和 Usage 图用的也是这三档，颜色线索为粉红 / 红、橙 / 琥珀、蓝 / 青。

标签不可手改，只能上报或 enrichment。`__name__` 风格 Meta Label 可按个人偏好隐藏。

告警对象：alert_id、alert_key、severity、status、progress（Triggered / Closed）、labels、event_cnt、所属 incident。注意告警进度只有 Triggered / Closed，没有 Processing——Processing 是故障被认领后的人态。

全局 Incidents 和 Channel 内 Incidents 共用同一套表。入口还提供 "View alerts"，说明产品承认「有人要直接看告警」，但默认工作面是故障。手动 Create 在空租户上仍是紫色主按钮：故障可以不是监控推上来的。

### 3.3 权限与租户，叠在对象之上

文档（[permission-design.md](https://docs.flashduty.com/zh/platform/permission-design.md)）：

- **功能权限**：RBAC。预置 Admin / Responder / Viewer，可自定义。权限点按 Platform / On-call / RUM / Monitors / 自定义菜单。Read + Manage，Manage 含 Read。
- **数据权限**：按团队。空间 / 值班 / 模板 / 日历 / 集成 / 映射规则的改删看所属团队、创建者、主体账号。
- 必须同时有功能和数据权限。
- 隔离实践：多主体完全隔离；空间设私有做资源隔离。
- 2026-01-30 废弃 `Account.Admin` / `Fin.Admin` / `Tech.Admin`，映射到 Admin / Responder，兼容期至 **2026-06-30**。

License：固定 / 临时 On-call 许可。主体账号 = 组织。本租户头像菜单只有一个身份，看不到多主体切换的实况。

---

## 4. 生命周期与状态机

Flashduty 把「人有没有接」和「监控有没有恢复」拆成两根轴。文档用词是 **处理进度 progress** 与 **故障状态 status（监控侧是否恢复）**。不要把 Closed 理解成「监控好了」，也不要把 Triggered 理解成「监控还在响」。

### 4.1 进度：Triggered / Processing / Closed

API 大写（[what-is-incident.md](https://docs.flashduty.com/zh/on-call/incident/what-is-incident.md)）：

| 进度 | 含义 | 谁推进 |
| --- | --- | --- |
| `Triggered` | 待处理，自动分派通知 | 系统：故障生成后进入 |
| `Processing` | 至少一人已认领；**全部取消认领则回退** | 人：认领 / 取消认领 |
| `Closed` | 手动关闭或自动恢复 | 人或系统 |

任何看见故障的人都能认领，**不限已分派人**。这是协作空间模型的推论：空间是可见性边界，分派是触达边界，ACK 是看见即可。

Channel 卡片上的活计数是 **Triggered / Processing**，不把 Closed 算进红点。Default 卡显示 `0 Triggered / 0 Processing`。

### 4.2 监控状态：恢复轴独立

关联告警全部 Ok → 故障视为已恢复，进度自动 Closed。

手动关闭**不改变**监控状态，但会关闭关联告警，且不再合入新事件。

这两句要一起读：

1. 人可以在监控还没恢复时把故障关掉（止住通知和合入）。
2. 监控恢复会把还开着的故障自动关掉。
3. 关掉之后，同一 `alert_key` 再来，按事件聚合规则会开新告警 / 新故障，而不是复活这条。

文档高级设置里的「跟随告警关闭」和「超时自动关闭」是自动 Closed 的另外两条路。控制台 Advanced 只看到 **Auto close on timeout** 开关（off），没摘到「跟随告警关闭」控件。Open API 提供 `/incident/resolve`、`/incident/reopen`、`/incident/snooze`、`/incident/wake`，说明关闭不是单向死亡：可 reopen，可 snooze / wake。

告警进度只有 Triggered / Closed。告警没有「认领」态——认领发生在故障上。一条故障里可以有多条告警，部分恢复、部分还在，故障监控状态要等全部 Ok。

### 4.3 端到端生命周期（文档工作流）

来源：[on-call.md](https://docs.flashduty.com/zh/on-call.md) 工作流摘要，以及分派 / 路由 / 降噪各页。

1. **进入**。专属集成直达该空间；或共享集成 + 路由；或标准 Event API / 邮件 / 控制台创建 / 外部提报链接。
2. **清洗**。告警 Pipeline、标签增强、排除规则（事件级直接丢弃，**无记录**）。
3. **降噪**。事件聚合 → 告警聚合（规则 / 智能）→ 抖动 / 静默 / 抑制。
4. **分派**。按空间内策略从上到下匹配，**命中即停**。无策略 = **无人通知**（接入仍在）。
5. **触达**。单聊（个人偏好或统一设置）+ 群聊。轮空值班表不发单聊，群聊仍发。
6. **认领 / 升级 / 关闭**。语音按键或 IM 卡片 ACK；超时升级；恢复或手动关闭。
7. **协同**。作战室、评论类型、行动项 / 跟进项、复盘、状态页。
8. **出站**。故障 / 告警 Webhook、Jira / ServiceNow / SDP、自定义操作。

共享路由细则（[routing-rules.md](https://docs.flashduty.com/zh/on-call/integration/alert-integration/routing-rules.md)）：标准路由手选目标空间；名称映射把标签值当空间名（如 `labels.team=payment` → 空间 `payment`），空间不存在则走默认路由或丢弃；流程控制「继续匹配 / 停止匹配」；默认路由仅在**全部规则未命中**时生效；一条规则可选多空间，每个空间各生成故障；支持拖拽排序、按空间筛选、预览（最多 20 条真实告警）、从其他集成复制、编辑历史对比回滚。这些 UI Pass 1 未打开，属文档。

新建空间的出厂降噪：事件聚合默认开、窗口 24h（1–1440 分钟）；告警聚合默认关（一条告警一条故障）；抖动检测默认「仅提醒」。本租户未点开这三块面板，不能证实 Default 空间是否仍是出厂值。

### 4.4 和 SRE Control 读模型时最容易撞车的一点

Flashduty 的 Incident **可以是自动生成的**（告警聚合的结果），也可以是控制台手动创建 / 外部提报。SRE Control 的 Incident 是人声明的多 Fault 协作容器。同名不同权：Flashduty Incident ≈「要叫人起来的那一团告警」；SRE Control Incident ≈「人决定升级成事故的那一层」。Flashduty 的 Alert 已经做完了 Alertmanager 式的去重与分组；它没有单独的 Fault 对象。分析时不要把三层对象强行对齐成四层。完整对比另文。

---

## 5. 降噪：事件聚合 / 告警聚合 / 抖动 / 静默 / 抑制 / 排除

降噪是 Channel 配置的产品面，不是全局管道里的隐式阶段。控制台把五块规则 + 一块排除都挂在空间 Configuration 下，和分派并列。Pass 1 **没有点开**这些 pane，下列参数全部来自文档 [noise-reduction.md](https://docs.flashduty.com/zh/on-call/channel/noise-reduction.md)。控制台能确认的只有：它们存在、分组叫 Noise reduction、KPI 卡在未开告警聚合时显示 "Visible after alert grouping is enabled"。

文档有一次重要语义变更（changelog）：静默 / 抑制从「拦截通知」改成在投递过程中**拦截故障生成**；被拦告警不再触发或合入故障，可选保留或丢弃。读旧博客时会看到「拦通知」口径，现行文档是「拦故障」。

### 5.1 事件聚合：同 `alert_key` 合成一条告警

问的是：上游反复推的「同一件事」要不要合成一条 Alert。键是 `alert_key`（≤255，可自动生成）。新建空间默认开，窗口默认 24h，可调 1–1440 分钟。

标准 Event API 在聚合之前还有两层去重（[standard-alert.md](https://docs.flashduty.com/zh/on-call/integration/alert-integration/alert-sources/standard-alert.md)）：完全相同 event 丢弃；状态 / 标题 / 描述与上一条一致则丢弃并更新属性。然后再过排除 / 静默 / 抑制。也就是说「去重」至少三层：接入幂等 → 事件聚合 → 告警聚合。

### 5.2 告警聚合：多条告警合成一条故障

新建空间默认关：一条告警一条故障。打开之后才有「降噪比」KPI。

两种算法：

- **智能**：ML 语义相似度；默认字段 `title` / `description` / `labels.service` / `labels.resource`，最多 10 个字段。文档**没给相似度阈值**；公开材料缺口里写过「控制台可能有滑条」，Pass 1 未打开，**不要写成已看见滑条**。
- **规则**：维度精确相等；可统一控制或细粒度分支（≤100 条，每组维度 ≤5）。

窗口：关 = 持续合入直到故障关闭；开 = 从「故障触发」或「告警合入」计时。默认上限 24h，可扩到 30 天。

风暴预警：最多 5 个阈值，每个 2–10000。单故障合入告警上限约 5000（文档写的是控制台渲染上限）。

模板变量 `.GroupMethod` 取值 `n` / `p` / `i`，对应不同聚合来源（文档列出变量，未在本次材料里展开三个字母的全称）。`.AlertCnt` / `.Alerts` 让通知文案能说「这团里有多少条」。

### 5.3 抖动 Flapping

参数：`max_changes` 默认 4（范围 2–100），窗口默认 60 分钟，静默默认 120 分钟。模式：关 / 仅提醒 / 提醒后静默。新建空间默认「仅提醒」——先让人看见闪烁，不立刻按住。

模板变量 `.IsFlapping`。控制台 Usage 没有单独的 Flapping 系列，抖动被算进故障生命周期，而不是独立计费事件。

### 5.4 静默 Silence

两种：单次（`30m` / `1h` / `1d` / `1w` …，可自动删除）或周期（星期 / 服务日历）。行为：直接丢弃，或保留并打标。快速静默从故障详情生成，对同一故障重复操作会改原规则，而不是堆很多条。changelog 还提到快速静默的标签排除。

服务日历本身是 Pro（控制台 `/calendar` 已门控），所以周期静默里「按服务日历」这条路，Free 租户大概率走不通。文档：每账户默认最多 5 个日历，给分派或静默做日期类型过滤（证券交易日等）（[service-calendar.md](https://docs.flashduty.com/zh/on-call/configuration/service-calendar.md)）。

Terraform 有 `flashduty_silence_rule`。

### 5.5 抑制 Inhibit

结构：新告警条件 + **10 分钟内**活跃告警条件 + 相同项。可配在空间或集成。行为与静默相同（丢弃或保留标记；现行语义是拦故障生成）。10 分钟窗口是文档写死的，不是用户可调项——材料没有「可改这个窗口」的句子。

Terraform 有 `flashduty_inhibit_rule`。

### 5.6 排除 Drop rules ≠ 静默 / 抑制

排除在**事件入口**丢弃，任何列表都不出现。静默 / 抑制发生在告警已经形成之后、故障生成之前（现行语义），可选留痕。控制台把 Drop rules 放在 Integrations 组而不是 Noise reduction 组，信息架构上把它当成「接入过滤器」，不是降噪策略。这是对的：排除没有「保留标记」，也没有周期，更像防火墙规则。

### 5.7 管道位置

```
Event API / 专属或共享集成
    → 两层接入去重（标准协议）
    → Pipeline / 标签增强
    → 排除（丢，无记录）
    → 事件聚合（alert_key + 窗口 → Alert）
    → 告警聚合（规则 / 智能 → Incident）
    → 抖动 / 静默 / 抑制（拦故障生成，可选留痕）
    → 分派策略匹配
```

没有单独的「全局降噪开关」。每个 Channel 一套。新建默认：事件聚合开、告警聚合关、抖动仅提醒——产品选择是「先保证同一 alert_key 不刷屏，默认不把不同告警捏成一团，免得误伤」。

---

## 6. 分派与升级：规则抽屉、Notify by 矩阵、值班表

分派是 Channel 内的有序列表，不是账户级全局策略。文档（[escalation-rule.md](https://docs.flashduty.com/zh/on-call/channel/escalation-rule.md)）和控制台抽屉可以对上，且控制台比文档多暴露了「严重程度 × 媒介」矩阵的具体控件。

### 6.1 匹配语义

控制台文案："Define incident notification and escalation rules. Incidents match rules in order and stop at the first match." 与文档「从上到下，命中即停」一致。建议底部留一条无过滤兜底；无策略 = 接入但不通知。轮空值班表：单聊不发，群聊仍发。

Default 空间已有一条规则卡：名称 **Default**，绿勾，badge "Assign order 1"，"Last modified 2024-01-01 22:19" + 操作者头像。虚线 "+ Add new rule"。时间是控制台原文，未标注时区。

### 6.2 创建 / 编辑 = 右侧抽屉，不是向导页

Pass 1 打开过创建抽屉并取消，未保存、未发测试。抽屉约 60% 宽，分节带编号，底部粘住 Save / Cancel，右上 X。未见 "test send" 按钮。

编号字段（控制台实勘）：

1. **Basic**。Name*（新建自动 "Strategy 4"）、Description。
2. **Rules & notifications**。
   - *Effective time* 开关（默认 unlimited）
   - *Incident filtering* 开关（打开才有匹配条件，否则全部命中）
   - *Delay window*：文案 "Notify after N second delay… no notification if incident auto-resolves or is closed during the delay"
   - *Notification template*：默认模板 + "View template details"
3. **Escalation path**。
   - "Level 1" + "Insert escalation level"
   - **Responders**：人 chip，例中是 SREMonitor
   - **Notify by**：见下一小节
   - *Repeat notifications*
   - **Escalation rule**：*"If the incident is [Open | Unacknowledged] for [N] minute, escalate to the next level"*
   - "Add escalation level"

文档把同一套东西收成六个要素，和抽屉分区不是一一对应，但字段能对上：

| 文档要素 | 抽屉里在哪 | 文档约束 |
| --- | --- | --- |
| 1. 触发条件 | Effective time + Incident filtering | 7x24 / 时段 / 服务日历；筛选集成、级别、标题、描述、标签；组内且、组间或 |
| 2. 通知对象 | Responders | 值班表 / 团队 / 个人 / 组合；值班表可按角色（主 / 备或自定义）定向 |
| 3. 通知方式 | Notify by | 单聊 + 群聊，见下 |
| 4. 延迟窗口 | Delay window | 0–3600 秒，默认 0 |
| 5. 通知模板 | Notification template | 每条策略必选，专业版功能 |
| 6. 升级 | Escalation path | 条件「未关闭」或「未关闭且未认领」；超时最少 1 分钟；默认首环节 30 分钟、不重复通知；重复间隔最小 0.5 分钟；可增删 / 插序 |

控制台枚举用 **Open / Unacknowledged**，文档用「未关闭 / 未关闭且未认领」。Open 对应进度未 Closed，Unacknowledged 对应尚未进入 Processing（无人认领）。材料没有第三种条件。

延迟窗口的产品意图写在 helper 里：等 N 秒，这段时间里自动恢复或被关掉就不吵人。它和抖动检测是两层——抖动是按变化次数进静默，延迟窗口是分派侧的「最后一厘米防抖」。

模板在 Free 租户里只能用系统预置 `默认模板`，创建自定义 disabled，与「专业版功能」一致。

### 6.3 Notify by：个人偏好 vs 统一矩阵

单聊两条 radio：

- **Follow personal notification preferences**
- **Follow default notification settings**

统一设置是一张 **严重程度 × 运输层** 矩阵：行 = Critical / Warning / Info，列 = **Voice / SMS / Email** 复选框。另有 *Group chat* 目标。

Pass 1 在这张默认矩阵里**没有看到 App、IM 私聊列**。文档个人偏好的故障分派场景里，这两项是存在的（语音 / 短信 / 邮件 / APP / IM，可多选，IM 可选具体集成实例）（[notifications.md](https://docs.flashduty.com/zh/on-call/configuration/notifications.md)、[personal-settings.md](https://docs.flashduty.com/zh/on-call/configuration/personal-settings.md)）。不要把「统一设置矩阵只有三列」写成「产品不支持 App / IM」——IM 应用在 Integrations 里，App 在个人中心和文档渠道矩阵里；只是这条抽屉里的 default settings 控件，Pass 1 只摘到 Voice / SMS / Email。

文档单聊媒介：电话、短信、邮件、App、IM 私聊。群聊：IM 应用群，或飞书 / 钉钉 / 企微 / Telegram / Zoom / Slack 机器人 Webhook；**域名不校验，可填自研网关**。控制台 IM 集成目录是钉钉 / 飞书 / Slack / Teams / 企微，没有 Telegram / Zoom 卡片——后两者在文档里以机器人 Webhook 形式出现。

个人偏好生效条件：直接分派到人，或策略单聊 =「遵循个人偏好」。选统一设置则忽略偏好。

### 6.4 动态分派：用标签改模板策略的对象

文档（[dynamic-notifications.md](https://docs.flashduty.com/zh/on-call/advanced/dynamic-notifications.md)）。环节从 0 起。标签或 Query 改对象：

| 目标 | reset | append |
| --- | --- | --- |
| 人员邮箱 | `layer_person_reset_(\d)_emails` | `layer_person_append_(\d)_emails` |
| 团队名 | `layer_person_reset_(\d)_team_names` | `layer_person_append_(\d)_team_names` |
| 企微 / 钉钉 / 飞书机器人 | `layer_webhook_reset_(\d)_{wecoms,dingtalks,feishus}` | 对应 append |

值逗号分隔。`reset` 优先于同维度 `append`。人员和团队同一维度；机器人按类型分别判断。changelog 提到 2026 年补了 append。控制台抽屉 Pass 1 未看到这些字段——动态分派是标签约定，不是抽屉里的另一组表单。

### 6.5 值班表

文档（[schedule.md](https://docs.flashduty.com/zh/on-call/configuration/schedule.md)）。控制台 `/oncall` 只有首次创建空态，**未进入创建表单**，下列是文档模型：

- 概念链：值班表 → 多层规则（白班 / 夜班叠加）→ 轮换 / 临时调班（Override **完全抢占**）/ 日期掩码 / 连续轮换 / 公平轮换。
- 周期：小时 / 天 / 周 / 月，1–30。
- 名称 1–40 字符，账户内唯一。描述 Markdown ≤500，可进通知。
- 角色：账户级角色库（主 / 备或自定义），分派可「所有人 / 指定角色 / 指定成员」。空态文案说 roles via support，和「需开通」一致。
- 换班通知：提前 N 分钟（5 分钟步长）或每天定时；单聊（个人偏好 / 统一设置）+ 群聊。
- 日历视图：一周 / 两周 / 月；个人值班计划 1 天–1 月。
- API 预览窗口最多 45 天。

Terraform 有 `flashduty_schedule`、`flashduty_escalate_rule`。

值班表是**账户级配置对象**，被 Channel 内分派规则引用，不是 Channel 的子资源。这和「Channel 拥有分派、分派引用值班表」的所有权方向一致。服务日历同理：账户级，被分派的生效时间和静默的周期过滤引用。

---

## 7. 通知媒介与 ACK 路径

营销页称「9 大通知渠道」（[flashduty.com/zh](https://www.flashduty.com/zh)）。文档渠道矩阵收成六类形态（[notifications.md](https://docs.flashduty.com/zh/on-call/configuration/notifications.md)）。控制台 Usage 图只画了 SMS / Email / Voice 三条系列——那是计费敏感的三条，不是渠道全集。

### 7.1 渠道矩阵

| 类型 | 形式 | 交互 | 用途 | 材料出处 |
| --- | --- | --- | --- | --- |
| Flashduty App | 单聊 | 原生检索 / 认领 / 关闭 / 升级 | iOS Critical Alerts 穿勿扰；Android 厂商通道。**仅故障分派场景** | 文档 |
| 语音 | 单聊 | 按键 ACK | 升级强制触达。仅故障分派 | 文档 + 抽屉矩阵 + Usage 图 |
| 短信 | 单聊 | 纯文本 | 无网兜底 | 同上 |
| 邮件 | 单聊 | 纯文本 / HTML | 详细报告。发信 `no-reply@notice.flashcat.cloud` | 同上 |
| IM 应用 | 单聊 / 群聊 | 卡片：认领 / 关闭 / 升级 / 评论 | 飞书 / 钉钉 / 企微 / Slack / Teams | 文档 + `/settings/source/im` |
| IM 机器人 | 单聊 / 群聊 | 链接跳转 | 无管理员权限的快速方案 | 文档；群聊 Webhook 域名不校验 |

私有化可关语音 / 短信 / APP（`disablePhone`），个人偏好只剩邮件和 IM。IM 失败时系统可自动用短信或邮件补提醒。

### 7.2 个人偏好四场景

| 场景 | 可选渠道 | 故意拿掉的 |
| --- | --- | --- |
| 故障分派 | 按 Critical / Warning / Info 分别选：语音 / 短信 / 邮件 / APP / IM（可多选，IM 可选实例） | — |
| 协作与跟进 | 短信 / 邮件 / IM（默认邮件） | 无 APP、无语音 |
| 值班轮换 | 短信 / 邮件 / IM | 无语音、无 APP |
| 内部状态页 | 仅 IM | 订阅邮件直发邮箱，不读偏好 |

手机 / 邮箱未验证则无法收短信 / 语音 / 邮件。IM 单聊需先在对应 Flashduty 应用里登录绑定。个人中心 APP Key 每账号最多 5 个。这些页 Pass 1 只到了 Profile（密码 / 邮箱 / 手机掩码 + 铅笔），没有打开 Notification / APP Key / Flashduty APP 子页。

### 7.3 语音号码（文档公开）

中国大陆：`(010)21364727`、`(021)32017538`、`(010)21364713`、`(010)21364708`、`(0571)23675454`、`(0571)23675496`。境外：`16465861127`，文档称覆盖 200+ 国家和地区。App 可把这些号码同步进通讯录防拦截。

### 7.4 模板：Go html/template + 自 fork sprig

专业版。引擎是 Go `html/template` + [flashcatcloud/sprig](https://github.com/flashcatcloud/sprig)。禁用 env / 网络 / 密码学 / 证书 / 随机数 / `fail`（[templates.md](https://docs.flashduty.com/zh/on-call/configuration/templates.md)）。

常用变量：`.Title` `.Description` `.DetailUrl` `.Num` `.IncidentSeverity` `.IncidentStatus` `.Progress` `.StartTime` `.Labels` `.AlertCnt` `.Alerts` `.Responders` `.ChannelName` `.FireType`（fire / refire）`.IsFlapping` `.GroupMethod`（n / p / i）。

函数：`toHtml` `date` `ago` `alertLabels` `joinAlertLabels` `jsonGet` `fireReason` `colorSeverity` `imageSrcToURL`（钉钉 / Slack）`transferImage`（飞书，≤10MB）。

渠道限制（超限行为不一样，这是实现细节）：

| 渠道 | 限制 | 超限 |
| --- | --- | --- |
| 企微应用卡片 | 最多 **8 行** | 文档未写截断策略 |
| 飞书 / 钉钉 / 企微机器人 | 约 **4000 字节** | — |
| Slack | 约 15000 字符 | 截断 |
| Teams | 约 28KB | 超限报错 |
| Telegram | 4096 字符 | 超限**不发送** |
| 语音 / 短信 | 短文本 | 默认「您有故障待处理：…」 |

飞书机器人默认模板含卡片按钮「故障详情」「认领」（`ack=1`）。渲染失败用默认模板兜底。控制台模板页的系统预置不可更改，和这套兜底是同一层。

机器人高级项：别名、严重程度过滤、触发事件类型（飞书 / 钉钉 / 企微 / Slack / Zoom）、@ 提醒。飞书 / Zoom 需用户映射表；钉钉默认手机号，可 @ 所有人。Telegram 国内需可访问代理，Chat IDs 可多个。Zoom Incoming Webhook 绑创建者账号，建议服务号。

### 7.5 ACK 路径：看见就能认领

| 入口 | 能做什么 | 出处 |
| --- | --- | --- |
| 控制台 / App | 认领、取消认领、关闭、升级、snooze / wake | 文档 |
| IM 卡片（应用集成） | 认领 / 关闭 / 升级 | 文档 |
| 机器人链接 | `{{.DetailUrl}}?ack=1` | 文档；飞书默认模板已带 |
| 语音 | 按键交互 | 文档 |
| Open API | `/incident/ack` `/incident/unack` `/incident/resolve` `/incident/reopen` `/incident/snooze` `/incident/wake` | 文档 |

没有「只有被分派的人能 ACK」。进度回退条件是全部取消认领。Pass 1 未打开故障详情，时间线上各渠道成功 / 失败态、转派事件新 UI 列在材料缺口里，本文不编。

### 7.6 作战室

专业版（[war-room.md](https://docs.flashduty.com/zh/on-call/advanced/war-room.md)）。飞书 / 钉钉 / 企微 / Slack；**同时只允许一个 IM 集成开作战室**。处理人变更自动拉人；关键操作回帖；时间线记作战室操作。企微群至少 2 名已关联成员。创建后 AI SRE 可自动跑一轮诊断回贴。本租户 Pro 门控，未见作战室开关。

---

## 8. 集成与 API / Terraform / MCP / OSS 边界

### 8.1 入站：integration_key 是唯一钥匙

标准告警：

`POST https://api.flashcat.cloud/event/push/alert/standard?integration_key=`

必填：`title_rule`（≤512）、`event_status`（Critical / Warning / Info / Ok）。可选：`alert_key`、`description`（≤2048）、`labels`（≤50，key≤128 Prometheus 风格，value≤2048）、`images[]`（alt / src / href）。频率：每集成 **100 次/秒、1000 次/分钟**，超限 429。可配字段映射 + 值映射，接收任意 JSON，数组最多 100 条。

夜莺专用：`/event/push/alert/n9e?integration_key=`，body 为 AlertCurEvent 数组。

控制台集成目录按 slug 添加：`/settings/source/alert/add/<slug>`。专属集成挂在 Channel 上直达；共享集成必须配路由。文档列出的源还包括：标准协议、HTTP Pull、DB Pull、Prometheus、Grafana、Zabbix、夜莺 / Flashcat、Open-Falcon、Uptime Kuma、Influxdata、Graylog、SkyWalking、ElastAlert2、Keep、各云厂商、PagerDuty Events API 兼容、Dynatrace、AppDynamics、SolarWinds、Splunk、Sentry、蓝鲸、观测云、监控宝、OceanBase、邮件等（[llms.txt](https://docs.flashduty.com/llms.txt)）。HTTP Pull / DB Pull 是 changelog 里的较新能力。

### 8.2 出站 Webhook

**故障 Webhook** 事件名是缩写枚举，几乎覆盖故障一生（[incident-webhook.md](https://docs.flashduty.com/zh/on-call/integration/webhooks/incident-webhook.md)）：`i_new` `i_assign` `i_a_rspd` `i_snooze` `i_wake` `i_ack` `i_unack` `i_storm` `i_custom` `i_rslv` `i_reopen` `i_merge` `i_comm` 以及一组 `i_r_*` 字段变更、一组 `i_wi_*` 工作项。Payload：`event_time`（毫秒）、`event_type`、`event_id`（需去重）、`person?`、`incident{...}`。可自定义请求体 `{{incident.title}}` + 值映射。

**告警 Webhook**（标准版+）：`a_new` `a_update` `a_merge`；`a_close` 系统自动、UI 不可勾选（[alert-webhook.md](https://docs.flashduty.com/zh/on-call/integration/webhooks/alert-webhook.md)）。

共同实现约束：POST JSON；HTTP 200 成功；**2 秒超时**；特定网络错误最多重试 1 次；可能乱序，用 `event_time` 过滤。SaaS 出口 IP（文档）：`47.94.95.118`、`123.56.8.183`、`47.94.193.81`、`1.13.19.96`。材料缺口写过「实际出口 IP 是否仍是这四个」未在控制台核验。

工单：Jira、ServiceNow、ServiceDesk Plus。控制台 Webhook Tab 里这三者和 Alert / Incident / Custom Action 并列，说明「出站」被做成同一目录，不是隐藏在故障详情的「导出」。

### 8.3 Open API / CLI / SDK / Terraform / MCP

- Endpoint **仅** `https://api.flashcat.cloud`，HTTPS + UTF-8 + `Content-Type: application/json`。多数 POST。鉴权：query `app_key=`。响应 `{request_id, data, error?}`。
- 文档称 **337** 个 API 操作、**39** 个服务（[developer/overview.md](https://docs.flashduty.com/zh/developer/overview.md)）；changelog 一处写 288/32，以 overview 较新表述为准，需以 OpenAPI JSON 再数。OpenAPI 文件在 `docs.flashduty.com/api-reference/on-call.openapi.zh.json` 等。
- CLI：`curl -sSL https://static.flashcat.cloud/flashduty-cli/install.sh | sh`，仓 [flashcatcloud/flashduty-cli](https://github.com/flashcatcloud/flashduty-cli)。
- Go SDK：`github.com/flashcatcloud/go-flashduty`，Go 1.24+。
- MCP：23 工具 / 8 模块；远程 `https://mcp.flashcat.cloud/mcp`；仓 [flashcatcloud/flashduty-mcp-server](https://github.com/flashcatcloud/flashduty-mcp-server)（约 5 stars）。
- Terraform：12 资源，含 `flashduty_channel`、`flashduty_escalate_rule`、`flashduty_schedule`、`flashduty_silence_rule`、`flashduty_inhibit_rule`、`flashduty_route`、`flashduty_template`、`flashduty_alert_pipeline`、`flashduty_incident` 等（[Terraform Registry](https://registry.terraform.io/providers/flashcatcloud/flashduty/latest/docs)）。

控制台前端 API 是另一条：同源 `/api/…`，无版本前缀，例如 `GET /api/team/list`。对外 Open API 和 Console API 是否同一后端、是否同一鉴权，公开材料没写。能确定的是对外强调 query `app_key`，Console 走登录会话。

SLA（2024-04-28，[service-sla.md](https://docs.flashduty.com/zh/compliance/service-sla.md)）：同城多活；告警上报后异步、出错重试；多渠道冗余；`api.flashcat.cloud` 全球加速。承诺：核心功能 99.9% 时间可用；99.9% 告警 5 分钟内投递。每 5 分钟周期错误率或延迟率 ≥0.1% 即该周期不达标。不含公测 / 免费 / 私有化。运营商 / 终端信号延迟除外。

### 8.4 OSS 边界：核心引擎闭源

GitHub 组织：

| Org | 说明 | 公开仓量级 |
| --- | --- | --- |
| [flashcatcloud](https://github.com/flashcatcloud) | enterprise observability management, ai-powered real-time incident center。2021-09-10 | 61–62 |
| [ccfos](https://github.com/ccfos) | CCF 开源发展委员会，夜莺捐赠后主仓在此 | 少（nightingale / huatuo 等） |
| [n9e](https://github.com/n9e) | 「欢迎把夜莺周边提交到这个 group」。2019-03-15 | 33 |

用户口头里的 `ccf-nightingale` **不是** GitHub org 名，实际是 **ccfos + n9e**。星标数字来自 2026-08-21 一次成功的 GitHub API / HTML，会变。调研后半段 API 403，n9e 33 仓、flashcatcloud 非 fork 全表未拉全。

**没有名为 `flashduty` 的核心后端开源仓。** On-call / 协作空间 / 分派引擎是闭源 SaaS。开源的是：

- 采集与告警生成：Categraf、夜莺（ccfos）、Ibex、n9e-helm
- Flashduty 边缘工具：CLI、Go SDK、MCP Server、Terraform provider、flashduty-tools（Python，批量改 webhook token）、sprig fork、knowledge-base（MDX 文档）
- 同 org 大量 fork（gin、mongo-go-driver、VictoriaMetrics、OTel collector、Grafana Agent 血统的 fc-agent、dd-sdk-*）属于依赖 / 二次开发，不是产品主体

夜莺 HA 对照（开源、可核）：多 `n9e` 共享 MySQL / Redis，规则自动分片，宕机接管；`n9e-edge` 从中心 Sync 规则、本地求值、事件经中心 API 写 MySQL，同时可直发钉钉 / 飞书 / FlashDuty；断连中心后求值仍用缓存，写库失败但外网在则可继续通知；边缘要独立 Redis。默认账号文档写 `root` / `root.2020`，端口 17000（[n9e 架构](https://n9e.github.io/docs/prologue/architecture/)）。这是 Flashduty **上游**的实现，不是 Flashduty 自己。

flashcatcloud 关键仓（非 fork 优先，星标会变）：

| 仓库 | Stars | 说明 |
| --- | --- | --- |
| categraf | 1300 | All-in-one collector。2026-08 仍活跃 |
| n9e-helm | 78 | Nightingale Helm |
| ibex | 46 | 告警自愈 |
| nightingale | 8 | **fork**，2023-04 后停更。正式仓是 ccfos/nightingale |
| flashduty-tools | 5 | Webhook 机器人批量改 token |
| flashduty-mcp-server | 5 | 官方 MCP |
| flashduty-cli | 低 | 官方 CLI，MIT，依赖 flashduty-sdk + cobra |
| go-flashduty | 1 | OpenAPI 类型化 SDK，Apache-2.0 |
| sprig | fork | 模板函数，文档指向 `flashcat` 分支 |
| catpaw-scripts | 5 | 运维脚本 |

n9e 周边 Top：`n9e/fe` 301（现行前端）、`fe-v5` 191、`fe-v4` 74、`prometheus-exporter-collector` 63、`wechat-sender` 60、`k8s-mon` 48、`n9e-mcp-server` 38。前端仓是独立 SPA，与 Go 后端分离。

---

## 9. UI / UX 设计语言与实现线索

本节几乎全部来自控制台 Pass 1。文档不写前端栈。

### 9.1 设计语言

- **布局**：固定图标轨 + 可折叠带标签模块导航 + 流体内容。内容卡最大宽度约 760–1000px，画布浅灰（约 `#F7F8FA`），白卡片，4–6px 圆角，1px 细边框。
- **密度**：中低。内边距 16–24px，8px 网格，控件高约 32px（`h-8`）。Tailwind 工具类：`bg-primary`、`focus-visible:*`、`dark:` ——暗色模式是一等公民。
- **颜色**：单一紫 / 紫罗兰品牌色打 CTA、链接、当前导航；绿 = 启用 / 健康；琥珀 = 警告 / 安装提示；红徽标打通知计数。三档严重程度配色线索：粉红 / 红、橙 / 琥珀、蓝 / 青。
- **语言**：English UI + 预置层中文残留。文档链跳到 `docs.flashduty.com/en/on-call/...`。产品和文档都做了中英两套，但数据层没洗干净。
- **空态两种**：表格用极简灰托盘 + "No data"；首次功能用插画猫 + 解释文案 + CTA。套餐门控复用同一套插画，只换 Upgrade。教学语气很重：每页标题旁文档深链。
- **表面选择**：
  - *Pages*：列表和对象详情（Channel 详情 = 带 Tab + 内导航的整页）
  - *Drawers*：嵌套配置对象的创建 / 编辑（分派规则：右侧、分节编号、粘性页脚）
  - *Popovers*：用户 / 组织菜单、过滤器 builder
  - *Modals*：几乎不用
- **表格套路**：表头 checkbox 批量选、每页 10 条、"Total N items" + 分页、列设置齿轮、导出按钮、可组合 "+ Filters" chip + 分段页签。这是 saved-view 风格，不是简单搜索框。
- **配额内联**：创建按钮旁直接写 `0 / 1`、`0 / 20 Created`。权限拒绝用 `oper-forbid` class，按钮还在，只是灭掉。
- **Channel 列表**：卡片活计数 + 星标 + 团队域 + grid/list 切换 + 按创建时间排序。空间是「被盯着的服务」，不是设置页里的一行。

### 9.2 实现线索（能写死的那些）

- **SPA**：单一 React 应用。路由切内容不全页刷新；title 会短暂滞后；"Loading…" 占位带猫咪。路由切换后残留 DOM 引用，证实客户端重渲染。具体 React 大版本、状态库、UI 组件库名称：**材料没写**，不要写成 Ant Design 或未证实的库。能证实的是 Tailwind utility 和 React SPA。
- **路由干净、按产品分前缀**：

  ```
  /channel
  /channel/detail/<id>
  /incident
  /status-page
  /post-incident
  /oncall
  /calendar
  /template
  /mapping
  /forms
  /fields
  /dashboard/usage
  /dashboard/insights/overview
  /settings/source/{alert|change|im|webhook}
  /settings/source/alert/add/<slug>
  /ai-sre/chat
  /rum/…
  /monit/rule/list
  /profile
  ```

- **实体 ID 像 snowflake**：Channel `2618595391671`，Org `2618591231671`，同一前缀空间。文档举例的故障 id 却是 Mongo 风格 ObjectID（`64b1352e376e32c85c56e25b`），短标识 `num` 取其后 6 位十六进制大写。两套 ID 并存：控制台组织 / 空间用十进制长整型，故障文档示例用 ObjectID。材料没有「已统一」的声明。
- **子 Tab 进 query**：`?tab=integration | notificationPolicy | baseSettings | advancedSettings`。可深链，且内部名暴露 API（`notificationPolicy`）。
- **Console API**：`/api/…`，无版本号，一资源一路径。例 `GET /api/team/list`。
- **遥测**：Datadog Browser SDK / RUM + **Session Replay** 已开（`/api/v2/rum`、`/api/v2/replay`，`ddsource=browser`）。`env: saas-prod`，`service: fd-console-prd`，`version: 202608202010`（日期戳 build id，部署于 2026-08-20）。Flashduty 自己卖 RUM，控制台却用 Datadog 收自己的前端遥测——实现上是「吃自己的狗粮」之前，先用更成熟的竞品盯生产。
- **无 websocket**：Pass 1 未观察到 websocket。Channel 卡片上的 Triggered / Processing 活计数更像轮询。
- **文档站分离**：控制台深链到 `docs.flashduty.com/en/on-call/...`（channel/create-edit、statuspage、channel/integrate-data）。中文文档在 `/zh/`。`llms.txt` 给机器索引。
- **后端推断（文档级，非控制台证实）**：API 风格 POST JSON、`request_id`、Go SDK、通知模板 Go html/template、故障 id 的 ObjectID 示例，材料推断服务端偏 Go。控制台前端框架细节、微服务拆分、消息队列、DB 选型：**公开文档未写**。企业版存储写过 VictoriaMetrics（指标）+ Apache Doris（日志 / 链路），那是私有化企业版，不是 Flashduty SaaS 的证明。

### 9.3 技术栈对照表（只写材料能确认的）

| 层 | 已知 |
| --- | --- |
| 夜莺后端 | Go，单二进制 `n9e` / `n9e-edge` |
| 夜莺前端 | 独立仓 n9e/fe（现代 SPA；具体框架未在本次 README 摘录中写死） |
| Categraf / Ibex / CLI / MCP / SDK | Go |
| 通知模板 | Go html/template + 自 fork sprig |
| Flashduty SaaS 后端 | **未开源**。从 API / SDK / 模板可推断偏 Go |
| Flashduty 控制台 | React SPA + Tailwind + dark mode；Datadog RUM |
| 企业版存储 | VictoriaMetrics + Apache Doris（产品页） |
| RUM 产品 | 多语言 SDK；flashcatcloud 下有 dd-sdk-* fork |
| Monitors | monit-agent、monit-edge（文档要求 v0.65.0+ 结构化查询）；ServiceMap 用 eBPF |
| AI SRE | MCP、A2A、云端 Sandbox、自托管 Runner（Linux systemd / Docker） |

---

## 10. AI SRE / Monitors / RUM 产品面

Flashduty 控制台已经不是「On-call 加三个灰入口」。最左轨四条产品线都有完整二级导航。Pass 1 对这三块只做了落地页点开，**没有深潜子页**。下面把「控制台看见的」和「文档声称的」分开写。

### 10.1 AI SRE

控制台落地页 `/ai-sre/chat`：问候 "Morning, SREMonitor"；黄色 beta 条（private beta for Pro，free trial）；composer 提示 "Type / for commands, @ to reference an incident"；+ 附件；Personal scope 下拉；模型 "Auto" 下拉；脚注 "AI generated, for reference only"。二级导航：New chat · Automations · Artifacts · Plugins · Knowledges · Environments · Chats list（搜索 + 过滤）。每个 On-call 页面右上角还有 AI SRE pill，开侧栏——助手是壳级能力，不是藏在一个路由里。

文档（[home.md](https://docs.flashduty.com/zh/home.md)、`/zh/ai-sre/*`、changelog）：公测、不单独收费。能力面：对话排障、IM @、自动化（cron / HTTP / 故障触发）、Skill、MCP、A2A、Knowledge Pack（DUTY.md）、记忆、Sandbox / BYOC Runner（Linux systemd / Docker）。作战室创建后可自动跑一轮诊断回贴。On-call 里还有 AI 摘要、AI 复盘、智能聚合、新奇故障、历史相似故障（后几项专业版）。

控制台能确认的实现线索：chat-first、命令 `/` 与故障引用 `@`、附件、scope 与模型选择、beta 对 Pro 开放但给 free trial。Automations / Artifacts / Plugins / Knowledges / Environments 这些二级项的表单 Pass 1 未打开，不要把文档能力清单写成「已经在 SREMonitor 里配过」。

### 10.2 Monitors

控制台落地页 `/monit/rule/list`：标题 "Alerting rules" + 规则 ID 查找框 + "Quick Edit"。琥珀横幅："Alert engine installation is required for Flashduty alerts. No installation detected — Install now"。双栏：左侧规则组（搜索 + "+"，tooltip "Click to create group"，空插画 "Alerting rules must be categorized under groups"）；右侧规则列表 "No data"。二级导航：Overview · Entity Tree · Alerting rules · Targets · Datasources · Alerting engines · Event Monitoring。

文档：统一告警引擎，对接 Prometheus / ES / Loki / ClickHouse / MySQL / Oracle / PostgreSQL / SLS / VictoriaLogs；组件 monit-agent、monit-edge；实体树；ServiceMap（eBPF Beta）。changelog：结构化查询、实体树排查视图、规则导入兼容夜莺 v6–v9 JSON、ServiceMap Beta。文档要求 monit-edge v0.65.0+ 才能用结构化查询。

产品含义：Flashduty 不再只做「收别人的告警」。Monitors 是自家规则引擎，但 **SaaS 控制台不替你跑引擎**——必须先装 Alert engine，否则规则页是空的。这和夜莺 `n9e` / `n9e-edge` 的中心-边缘同构：控制面在云上，求值在你这边。规则必须先建组再挂规则，和夜莺的业务分组习惯一致；兼容夜莺 JSON 导入说明目标用户就是从 n9e 迁过来的人。

### 10.3 RUM

控制台落地页 `/rum/dashboard/overview`：Pass 1 只看到模块导航，内容仍在 Loading。二级导航：Analytics · Applications · Performance · Error Tracking · Session Explorer · Session Replay。

文档：Web / Android / iOS / HarmonyOS / Flutter / Electron / 微信小程序；会话重放、Issue 聚合。changelog：微信小程序、HarmonyOS、移动符号化。实现线索：flashcatcloud 下有 Datadog Browser SDK fork（dd-sdk-*）；而 Flashduty **自己的控制台**也在用 Datadog RUM + Session Replay（`service: fd-console-prd`）。RUM 产品面和 On-call 共享同一套控制台壳，但路由前缀 `/rum/` 独立。

### 10.4 三条轨和 On-call 怎么接

材料能串起来的接点：

- AI SRE `@` 引用 incident；作战室自动诊断回贴；故障触发自动化。
- Monitors 求值后的告警进入 Flashduty 集成（专属 Channel 或共享 + 路由），再走 Event → Alert → Incident。
- RUM 的 Issue 聚合是否直接生成 On-call 故障：本次材料**没有**写出这条自动管道，不要编。
- 顶栏产品切换让同一组织身份同时用四条产品，权限点按 Platform / On-call / RUM / Monitors 切开。

---

## 11. 对标 PagerDuty：官方说法 vs 控制台实际

官方明确对标 PagerDuty（[comparison.md](https://docs.flashduty.com/zh/on-call/quickstart/comparison.md)）。**该页的逐条对比表没有被摘进本次两份材料**，所以本文不编一张「官方对照表」。能写的是：官方把自己放在 PD 的位子上、文档还列了 PagerDuty Events API 兼容集成，以及控制台实际长成什么样子。

### 11.1 官方已经说出口的

- Flashduty = 告警生成之后的统一事件处理；夜莺 / Prometheus / Zabbix / 云监控在前（[nightingale-vs-flashduty](https://flashcat.cloud/blog/nightingale-vs-flashduty/)）。这是 PD 经典「Events API 之后」的位置。
- SaaS 品牌已扩成 On-call + Monitors + RUM + AI SRE，不再只是 On-call。PD 近年也在往 Observability / AIOps 扩，叙事平行，实现不同。
- 营销数字：数千家企业、超过 10 亿条告警事件——未核验。
- 集成目录里有 **PagerDuty** 源，文档写 PagerDuty Events API 兼容：迁徙路径是「把打到 PD 的事件改打到 Flashduty」，不是双写。
- Channel 被做成协作空间，Terraform 资源名也是 `flashduty_channel`。官方术语提醒反复强调它不是通知媒介。

### 11.2 控制台实际：和 PD 的结构差，比功能清单更重要

Pass 1 写过一句关键观察：Channel 是中心对象 = 服务 / 团队告警管道，拥有集成、降噪、分派和指标；通知运输层不在 Channel 对象上。**这是相对 PagerDuty「services + notification rules」的关键设计差。**

展开成可核对的几点：

| 维度 | 控制台 / 文档里的 Flashduty | 和 PD 常见模型的差（观察，不是官方表） |
| --- | --- | --- |
| 主对象 | Channel（协作空间）拥有故障 | PD 主对象是 Service；通知规则在人身上 |
| 故障从哪来 | 默认同 alert_key 聚合为告警；告警聚合默认关，一条告警一条故障 | PD 也是 event → alert → incident，但 Service 级分组 / urgency 是另一套旋钮 |
| 严重程度 | 三档 Critical / Warning / Info，贯穿映射、矩阵、模板 | PD 常用 high / low urgency + severity 字段；Flashduty 把三档做成 Notify by 矩阵的行 |
| 分派归属 | 规则挂在 Channel 内，命中即停 | PD Escalation Policy 被 Service 引用，政策本身是可复用对象 |
| 个人通知 | 四场景偏好；策略可选「跟随个人」或「跟随统一矩阵」 | PD 个人 notification rules（push → SMS → phone）是账户级默认路径 |
| 延迟窗 | 0–3600 秒，窗内关闭则不通知 | 类似 PD 的 urgency / 等待，但 Flashduty 把它做成策略字段，和抖动检测分层 |
| 语音 / 短信 | 一等公民；Usage 图只画这三条计费敏感渠道；公开主叫号码 | PD 同样一等；Flashduty 把中国主叫号码写进文档 |
| IM | 飞书 / 钉钉 / 企微是目录一等，Slack / Teams 并列 | PD 以 Slack / Teams / Email 为西方默认；Flashduty 目录顺序是中国市场 |
| 作战室 | 专业版；同一时间只允许一个 IM 集成开 | PD 有 conference / Slack channel 自动拉起，不限「只能接一种 IM」 |
| 状态页 | Public 配额 0/1，Internal 0/0（本租户） | PD 有 Statuspage 产品线；Flashduty 做成 On-call 二级菜单 |
| 套餐 | Free 1 个 Channel；Pro / Standard 门控复盘、日历、洞察、自定义字段 | 门控颗粒在控制台用同一套猫 + 盾锁空态表达 |
| 自己产告警 | Monitors 要自装 Alert engine | PD 不自带 Prometheus 式规则引擎，靠 Events API |

控制台还有几处「看起来像 PD、实现不是 PD」：

- Incidents 列表的 "Assigned to me / My channel" 是人维 + 空间维，不是 PD 的 Service 维。
- 分派抽屉内部名 `notificationPolicy`，UI 叫 Escalation rules：一个对象同时承担「谁匹配」和「怎么升级」，没有拆成 PD 的 Service + Escalation Policy + Event Orchestration 三件套。
- 共享集成的路由（名称映射、继续 / 停止、一条规则打多个空间）更接近「事件编排」，但入口在集成上，不在独立 Orchestration 产品里。
- 自定义表单按生命周期切开：Declare / Acknowledge / Close，而不是一张万能 Incident Type。

### 11.3 控制台实际打薄了官方叙事的地方

- 官方文档 On-call 导航是中文树；控制台默认 English，预置数据中英混杂。国际化还在洗。
- 官方「100+ 告警源」；Pass 1 目录约 55 张卡片。长尾可能在搜索 / Other 里，但「打开 Integrations 先看到的」是 curated 头 + 中国云厂商字母序。
- 官方 IM 含 Telegram / Zoom 机器人；控制台 IM Tab 五家：钉钉、飞书、Slack、Teams、企微。
- 官方个人偏好有 App / IM；分派抽屉统一矩阵 Pass 1 只摘到 Voice / SMS / Email。App 存在于个人中心和文档，不一定出现在这条 default settings 表上。
- 官方文档创建向导有公开 / 私有、代管团队、跟随告警关闭；Pass 1 的 Basic / Advanced 没摘到这三项。可能未开通或未展开，**不能当控制台没有**，也不能当已经看见。
- 官方 AI SRE 公测不单独收费；控制台黄条写 private beta for Pro + free trial。Free 租户能进聊天页，不代表 Automations / Runner 都开放。
- Usage 对 Free 开放、Insights 对 Standard 门控：分析被劈成「计费用量」和「运营洞察」两层。PD 的 Analytics / Insights 也是高档功能，Flashduty 把用量留给所有人看——和短信 / 语音计费直接相关。

---

## 12. 对 SRE Control 的可借鉴点

完整对比另文（仓库快照见 `/workspace/research/alert-system.md`）。这里只列观察：Flashduty 哪些设计是被控制台和文档同时钉死的，值得 SRE Control 对照，而不是抄。SRE Control v0.1.0 的中心论点是 Alertmanager 拥有 Alert 生命周期，自己拥有 Fault / Impact / Incident。Flashduty 没有 Fault，Incident 可以由聚合自动生成。同名对象不要对齐。

### 12.1 对象与所有权

1. **给「管道」一个一等对象，并让列表页像工作台。** Channel 卡片上的 Triggered / Processing、星标、团队域、grid/list，比设置页里的一行 Service 更像日常入口。SRE Control 现在没有 Channel-as-service，最接近的是 `notification_policies` + `team_ops_settings.channels` JSON。如果要借鉴，先问「谁拥有集成 + 降噪 + 分派 + 活计数」，而不是先问「通知渠道表怎么建」。
2. **进度轴和监控轴拆开。** Triggered / Processing / Closed 是人态；告警全 Ok 是监控态；手动关闭不改监控态但停合入。SRE Control 已经有 source_state / response_state / suppression_state 三轴，方向比 Flashduty 更硬。可借鉴的是 UI 用词：Channel 卡只数 Triggered / Processing，Closed 不进红点。
3. **ACK 与分派解绑。** 看见就能认领；全部取消认领回退 Processing → Triggered。SRE Control 的 Lark 卡片 ACK 写不进 `acked_by`（无身份映射），和「看见即可」还有距离。
4. **不要把 Incident 自动生成当成必须抄的。** Flashduty 默认一条告警一条故障，告警聚合是可选、且 KPI 在未开启时显式说看不见降噪比。SRE Control 的 Incident 人声明 + 双关闭门，是另一条产品路。若借鉴聚合，应放在 Alert / Fault 层，不要自动升格成事故。

### 12.2 降噪与分派

5. **降噪按空间配置，排除放进接入组。** Drop rules 在 Integrations，其余五块在 Noise reduction。排除无记录，静默 / 抑制可留痕。SRE Control 对 AM 源跳过本地 silence / inhibit，只做权威安全的 dedup + mapping——和 Flashduty「每个 Channel 自己降噪」是相反的权力分配，不要为了功能对称把 AM 的抑制再做一遍。
6. **分派命中即停 + 无策略不通知。** 控制台把这句话写在 Escalation 面板顶部。兜底策略靠约定不靠隐式全局。SRE Control 的通知策略是 matcher → team default → global default，总有一条落点；Flashduty 允许「接入但完全安静」。两种都自洽，产品要显式选。
7. **Delay window 和 Flapping 分层。** 抖动按变化次数进静默；延迟窗是分派前最后 N 秒。SRE Control 已有 flap 表和 `wait_seconds`，可以对齐「两个旋钮、两个阶段」的叙事，不必合成一个开关。
8. **Notify by 做成严重程度 × 媒介矩阵，并允许「跟随个人 / 跟随统一」。** 这是个人偏好和团队政策的显式交接点。SRE Control 目前是团队级 policy steps，没有个人 notification rules。矩阵比「渠道列表」更教得会。
9. **动态分派用标签约定，不新增一套 UI。** `layer_person_reset_N_emails` 这种 reset / append 让路由标签能改升级对象。SRE Control 的 mapping / sre.* 标签已经在走「标签驱动自动化」；若要动态改值班对象，标签约定比再做一个编排产品轻。

### 12.3 通知与 ACK

10. **媒介按场景裁剪，不是一张万能偏好表。** 故障分派才有语音 / App；协作、换班、状态页逐步降级。SRE Control 只有 Feishu 卡片是可 ACK 的，email / webhook 是 fire-and-forget——和 Flashduty「协作场景故意去掉语音」是同一类克制。
11. **ACK 入口要有 `?ack=1` 这种零权限深链。** 机器人 Webhook 没有卡片按钮时，一条带 ack 的 URL 就能闭环。Open API 同时给 ack / unack / resolve / reopen / snooze / wake。SRE Control 的 repeat Ack 返回 200 已经对了；缺的是跨渠道同一语义。
12. **模板失败有默认兜底，渠道超限行为写死。** Telegram 超限不发送、Slack 截断、Teams 报错——实现时比「尽量发出去」更可预测。引擎用 Go html/template + 阉割 sprig，和 SRE Control 的 Go 栈同族。
13. **语音 / 短信可以后做，但 Usage 图已经按它们计费。** Flashduty Free 租户也能看见 SMS / Email / Voice 用量。SRE Control 的 phone 是 registry 占位、Validate 拒绝未注册。若永远不做电话，就不要在政策 UI 里露出它。

### 12.4 IA / UX / 实现

14. **三级壳：产品轨 / 模块导航 / 内容。** SRE Control 的全局导航是「SRE 主任务」，设置进控制中心（CLAUDE inv. 19）。Flashduty 把 On-call / Monitors / RUM / AI SRE 做成可切换产品，设置进头像。自托管单产品不必抄四轨，但「运行面和配置面分开、配置里再按对象分组」是同一原则。
15. **详情用整页 + `?tab=`，嵌套配置用抽屉。** Channel 详情可深链到 `notificationPolicy`；分派规则用 60% 宽编号抽屉、粘性 Save / Cancel。SRE Control 的 FaultWorkspace / IncidentRoom 已经是整页；策略编辑可以学抽屉，避免再开一堆 modal。
16. **空态分两种，门控复用教学插画。** 检索空 ≠ 首次空 ≠ 套餐空。配额写在按钮旁（`0 / 1`、`oper-forbid`）。自托管没有套餐，但仍有「未配 Alertmanager / 未配值班」的首次态，值得做成同一套插画语言。
17. **每页文档深链 + 页内 helper。** Owning team 的 MTTA/MTTR 归属、Delay window 的「窗内关闭不通知」，都写在控件旁边。比单独写一篇 CLAUDE.md 更接近操作者。
18. **snowflake / 可读短号并存。** 组织 / 空间是十进制长 ID，故障有 ObjectID + 6 位 `num`。SRE Control 用 `INC-` + 6 位数字。短号是给人念的，长 ID 是给 API 的——Flashduty 两套都暴露（复制 ID 图标 + `.Num` 模板变量）。
19. **OSS 边界清晰：生成开源，响应闭源。** 夜莺 + Categraf 做告警生成，Flashduty 做进入之后。SRE Control 选择 AM 做 Alert 权威，自己做 Fault / Incident，是同一类边界，只是切开的层不同。不要为了「功能对等」去自建 Ruler 或去抄 55 个云监控适配器。
20. **Monitors 的「控制面在云、引擎要自装」和夜莺中心-边缘同构。** 若 SRE Control 永远不自建 Ruler，这条可以忽略；若以后做规则工作站，琥珀横幅 "No installation detected — Install now" 是比「先下一本安装文档」更诚实的空态。

### 12.5 明确不必借鉴

- 把 Incident 当告警聚合的默认产出（和 SRE Control 人声明事故冲突）。
- 为对等而去补 Status page / 公开订阅 / 作战室多 IM。
- 在 AM 权威源上再做一层 Flashduty 式静默 / 抑制。
- 把 Channel 一词继续当通知媒介用——Flashduty 已经用这个词占了「协作空间」。SRE Control 现有 `notify.Channel` 接口是运输层。若引入协作空间，换一个名字（Space / Service Room），避免自己把两个概念撞车。
- 抄 Datadog RUM 收自己的控制台。那是 SaaS 实现选择，不是产品能力。

---

## 13. 材料缺口（避免被当成「产品没有」）

下列在两份材料里明确未核验或未打开。缺的是观察，不是功能不存在：

1. 降噪五块 pane 的真实控件、智能聚合是否有相似度滑条。
2. 分派抽屉以外的「测试发送」。
3. 故障详情时间线、严重程度色在真实数据上的样子、快速静默。
4. 集成添加向导字段、IM OAuth、作战室开关。
5. 个人偏好四场景的默认勾选、APP Key 权限点、`disablePhone`。
6. 模板编辑器的 `{{` 补全、按历史故障预览、分渠道分栏。
7. Plan 页现价与标准 / 专业准确矩阵（文档只链 [定价页](https://flashcat.cloud/flashduty/price/)）。
8. Teams / Members / Roles / SSO / Audit、Quick start checklist。
9. RUM 子页内容、AI SRE 除 chat 以外的子页。
10. Flashduty 闭源实现：服务拆分、队列、存储、多活机房名、出口 IP 是否仍是文档四个。
11. 官方 PagerDuty 对比页的逐条表格。
12. 夜莺 fe 的 `package.json` 框架、n9e 33 仓全表、flashcatcloud 非 fork 全表。
13. `help.flashcat.cloud` 是否只是跳转到 docs。
14. 未登录时的登录墙、SSO 子域、验证码。

---

## 14. 一句话

快猫星云用开源夜莺 + Categraf 做告警生成与采集，用私有化 Flashcat 企业版做北极星 / 灭火图场景化观测，用 SaaS Flashduty 做多源事件进入之后的 On-call 闭环，并在同一控制台壳里并上 Monitors / RUM / AI SRE。控制台 `/channel` 是协作空间，不是通知媒介；通知是 App / 语音 / 短信 / 邮件 / IM，由 Channel 内分派策略按命中即停匹配，再按个人偏好或严重程度矩阵打出。人态是 Triggered / Processing / Closed，监控恢复是另一根轴。核心引擎闭源；能从材料还原的实现线索是 React + Tailwind SPA、`?tab=` 深链、snowflake 式组织 / 空间 ID、ObjectID 式故障号、Go 系 API 与 html/template、Datadog RUM（`fd-console-prd` / `202608202010`）、以及夜莺 `n9e` / `n9e-edge` + MySQL / Redis 的开源对照架构。

主要来源：`flashcat-docs-oss.md`（文档 URL 见该文 §9）、`flashduty-console-pass1.md`（路径见该文 §2 表）。调研日 2026-08-21，Asia/Shanghai。
