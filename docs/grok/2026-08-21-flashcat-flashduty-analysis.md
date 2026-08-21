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
