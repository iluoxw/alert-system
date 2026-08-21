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
