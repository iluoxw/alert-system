# 快猫星云、PagerDuty 与当前 `alert-system` 对比报告

> 调研日期：2026-08-21（Asia/Shanghai）
> 对比对象：Flashcat Cloud 控制台中的 Flashduty On-call、PagerDuty、当前仓库 `alert-system`（SRE Control v0.1.0 MVP）
> 结论类型：产品现场/官方资料/仓库代码事实与架构建议分开书写。供应商核心后端闭源，凡涉及其内部存储、队列或模型的内容均标记为“推断”。

## 1. 先给结论

三者解决的是同一条响应链上的不同问题：

```text
外部监控信号 → 去重/相关性 → 责任计算 → 通知升级 → 人工协作 → 恢复/学习
      └─ Flashduty、PagerDuty：把“告警到人”做成成熟 SaaS
      └─ alert-system：在告警之上增加 Fault/Evidence/Impact，回答“为什么、影响谁、是否应宣布事故”
```

我的推荐是：**保留当前项目的 Infrastructure-first 和 Alert/Fault/Impact/Incident 分层，把 Flashduty/PagerDuty 的响应控制面能力作为可选择的产品增强，而不是把当前项目改造成另一个通用 on-call 克隆。**

- Flashduty 最值得借鉴的是分层降噪、共享/专属接入、路由预览、通知模板和自定义表单；
- PagerDuty 最值得借鉴的是 Service Directory、升级策略与值班表的职责分离、Event Orchestration 的规则生命周期，以及 API/IaC/MCP 只读优先；
- 当前项目最有差异化的能力是实体依赖图、Fault 证据角色、业务 Impact 状态、Incident 宣布门槛和受控 Runbook。它们不应被“收到一条高优先级告警就自动建 Incident”的通用模型替代。

## 2. 对比方法和证据边界

### 2.1 Flashduty

已在应用内浏览器只读巡检协作空间、故障列表、状态页、复盘、分析、集成中心、通知模板、映射、表单、值班、服务日历、空间详情的降噪/分派和 AI-SRE。租户没有可展示的故障数据，多个高级模块受权限/套餐限制；报告因此重点记录 IA、字段、默认值和交互，而不是声称验证了实际送达。

主要官方资料：[故障对象模型](https://docs.flashduty.com/zh/on-call/incident/what-is-incident)、[集成数据](https://docs.flashduty.com/zh/on-call/channel/integrate-data)、[告警降噪](https://docs.flashduty.com/zh/on-call/channel/noise-reduction)、[值班](https://docs.flashduty.com/zh/on-call/configuration/schedule)、[路由规则](https://docs.flashduty.com/zh/on-call/integration/alert-integration/routing-rules)。

### 2.2 PagerDuty

已在应用内浏览器现场验证 Incidents 页、导航、筛选、批量动作、空数据状态和当前值班卡片。跨模块进入 Service Directory 后会话被重定向到 identity 登录页，服务目录/策略页面未写成现场观察；这些部分以 [官方文档](https://support.pagerduty.com/main/docs/service-directory) 和官方 GitHub 证据为准。

### 2.3 当前项目

对比依据是仓库中的产品约束、领域模型、管道实现和前端 IA：

- [README.md](/Users/luoxiaowei/code/go/src/alert-system/README.md) 描述 MVP、技术栈、接入契约和部署方式；
- [CLAUDE.md](/Users/luoxiaowei/code/go/src/alert-system/CLAUDE.md) 定义 Infrastructure-first、Alertmanager 权威源、Fault/Impact/Incident 边界和事务不变量；
- [app.go](/Users/luoxiaowei/code/go/src/alert-system/internal/app/app.go:140) 装配 pipeline stages 和 PG timer handlers；
- [pipeline.go](/Users/luoxiaowei/code/go/src/alert-system/internal/pipeline/pipeline.go:37) 定义 authoritative-safe 规则；
- [alert.go](/Users/luoxiaowei/code/go/src/alert-system/internal/domain/alert.go:48)、[fault.go](/Users/luoxiaowei/code/go/src/alert-system/internal/domain/fault.go:9)、[incident.go](/Users/luoxiaowei/code/go/src/alert-system/internal/domain/incident.go:9) 定义核心对象；
- [App.tsx](/Users/luoxiaowei/code/go/src/alert-system/web/src/App.tsx:154)、[SettingsLayout.tsx](/Users/luoxiaowei/code/go/src/alert-system/web/src/pages/Settings/SettingsLayout.tsx:27)、[OperationsOverview.tsx](/Users/luoxiaowei/code/go/src/alert-system/web/src/pages/OperationsOverview.tsx:27) 定义当前控制台 IA 和工作台重心。

## 3. 产品定位对照

| 维度 | Flashduty | PagerDuty | 当前 `alert-system` |
|---|---|---|---|
| 首要问题 | 多来源告警接入、降噪、分派和多渠道叫醒 | 服务责任、事件编排、升级和企业响应 | 识别基础设施 Fault、判断业务 Impact、再进入 Incident 协作 |
| 稳定责任锚点 | 协作空间 + 团队/值班 | Service + Escalation Policy + Schedule | Entity/Relation + owning team；Service Catalog 是映射和责任视图 |
| 核心工作对象 | 故障（Incident），由 Alert 聚合而来 | Incident，由 Service 上的 Alert 触发 | Fault 是技术事实，Incident 是人工宣布的协作事件 |
| 上游关系 | 不生成监控告警，集中接入 Event | Event API/集成进入 Alert/Incident 链 | 第一阶段让 Alertmanager 保持 Alert 生命周期权威 |
| 部署形态 | SaaS 控制台，核心服务端闭源 | SaaS 控制台，核心服务端闭源 | 自部署 Go + PostgreSQL，PG timers，无外部 MQ |
| 差异化 | 细粒度噪声策略和中文/本地 IM 接入 | Service-centric 企业响应网络和生态 | 证据图、影响图、保守宣布、受控操作 |

## 4. 对象模型：相似的三层与关键分叉

### 4.1 三者的共同主线

三者都承认原始事件不等于需要人接电话的事故：

```text
Event/Signal → Alert → Incident/故障 → 通知与协作 → 恢复
```

Flashduty 官方明确写成 Monitor → Event → Alert → Incident；PagerDuty 文档也明确区分 Event、Alert、Incident；当前项目则把链路扩展为 `Signal → Alert → Fault → Incident → Response → Recovery → Learning`。

### 4.2 当前项目多出的一层：Fault 和 Impact

当前 `Alert` 是异常证据，`Fault` 是资源或能力上的技术故障，`Incident` 只在需要团队协作并满足宣布门槛时成立。`FaultEvidence` 还可以把同一条 Alert 标为 direct impact、cause candidate、trigger、amplifier、symptom、supporting 或 excluded；`FaultImpact` 则以 potential/confirmed/ruled_out 表示影响判断。

Flashduty 的 Alert→Incident 聚合和 PagerDuty 的 Alert→Incident 分组都很成熟，但它们主要回答“哪些信号属于同一个响应工作项”。它们没有在公开核心对象中提供与当前项目同等粒度的“证据角色—因果候选—业务影响确认”链。因此：

- 若产品目标是快速叫醒值班人，供应商模型更短、更易采用；
- 若目标是解释共享基础设施为什么影响多个服务，当前模型更适合，代价是需要拓扑、映射和人工决策。

### 4.3 状态维度的对照

| 状态维度 | Flashduty | PagerDuty | 当前项目 |
|---|---|---|---|
| 来源/恢复 | 关联 Alert 全恢复后故障可自动恢复 | resolve event/incident resolve 结束响应 | `SourceState` 独立于响应动作，支持 source/TTL/manual resolve policy |
| 人工响应 | Triggered/Processing/Closed，认领改变处理进度 | Triggered/Acknowledged/Resolved，ack 暂停升级 | `ResponseState` 与 source state 分离，Ack/Resolve 幂等 |
| 抑制 | silence/inhibition/flapping 等策略 | suppressed alert、dedup、grouping、orchestration | `SuppressionState` + 命中 ID；silence 已实现，inhibit 当前是占位 |
| 技术故障 | 主要由告警聚合后的故障承载 | 主要由 incident 承载 | Fault candidate/active/monitoring/resolved，允许先观察再宣布 |
| 业务影响 | 可在服务/状态页等外围表达 | Business Services/Service Graph 表达服务关系 | `FaultImpact` 保存路径、状态、置信度和决策来源 |

## 5. 端到端流水线对比

### 5.1 Flashduty：集中接入、分层降噪、协作空间分派

官方和现场配置可以还原为：

```text
集成事件
  → 标签增强/标准化
  → 事件聚合（同 alert_key → Alert）
  → 共享路由 → 协作空间
  → 告警聚合（规则/智能 → 故障）
  → 抖动/静默/抑制/风暴
  → 分派策略 → 值班/团队/个人/群聊
  → 升级、认领、关闭、状态页/复盘
```

它的优点是所有“告警到人”的控制点在一个产品里，用户不必先部署外部 Alertmanager。代价是平台承担了上游告警生命周期，路由、聚合、静默和抑制的组合需要很强的命中解释。

### 5.2 PagerDuty：事件编排、服务责任、升级求值

PagerDuty 的主链更接近：

```text
Event API/Integration
  → Global/Service/Routing Event Orchestration
  → dedup_key → Alert
  → service/grouping/global grouping
  → Service 的 Escalation Policy
  → Schedule 求值 → 通知/升级
  → Ack/Responders/Workflow/Resolve
```

它把“规则怎么处理事件”和“谁长期负责服务”拆成不同对象。Global Alert Grouping 可以跨服务合并，但第一条 Alert 所属服务决定主要升级策略，这是减少噪声与保持责任准确之间的权衡。

### 5.3 当前项目：权威 Alert 镜像 + 本地 Fault 投影

应用装配明确注册 `dedupStage → mappingStage → routeStage → groupStage → silenceStage → inhibitStage`，并注册 alert TTL、重复通知、人员升级、分组等待、渠道升级、通知发送、运营任务、保留清理和风暴摘要等 timer handler。

关键分叉在 [pipeline.go](/Users/luoxiaowei/code/go/src/alert-system/internal/pipeline/pipeline.go:37)：识别为 Alertmanager authoritative source 后，只有显式实现 `RunForAuthoritativeSource()` 的 stage 继续执行；[stage_mapping.go](/Users/luoxiaowei/code/go/src/alert-system/internal/pipeline/stage_mapping.go:16) 只做 Alert→实体/Fault/Impact 投影，不再执行本地 route/group/silence/inhibit/Alert 通知。

这不是“少实现了几段管道”，而是一个有意的权威边界：外部 Alertmanager 负责 Alert 级事实，SRE Control 负责更高层的实体、故障、影响和事故。兼容来源才走本地路由、分组和通知链。

## 6. 噪声治理对比

| 能力 | Flashduty | PagerDuty | 当前项目现状 | 判断 |
|---|---|---|---|---|
| 事件去重 | 同 `alert_key` 在窗口内聚合 | `dedup_key` 更新同一 Alert | fingerprint/upstream fingerprint、dedup count | 三者都成熟；当前需继续强化跨 adapter 指纹契约 |
| Alert→Incident 相关性 | 规则聚合或智能聚合；窗口、维度、风暴阈值 | service grouping、global grouping、智能/内容/时间模式 | route group_by、window 全归并、模板指纹、group wait/interval | 当前已有确定性骨架，但缺少供应商级可视化和智能候选 |
| 抖动 | 默认可启用提醒/提醒后静默 | 通过 grouping、suppression、urgency 等组合 | flap count/window/state，只影响通知不改状态 | 当前语义清楚，可借鉴 Flashduty 的配置解释 |
| 静默 | 单次/周期/日历，条件 AND/OR，丢弃或标记 | 编排 suppress、服务设置、维护/抑制能力 | 10 秒规则缓存，命中 ID 持久化，告警仍可见 | 当前已有可审计命中记录；需要预览和历史回放 |
| 抑制 | 根因告警存在时压制相关告警，支持保留标记 | orchestration suppress/drop、alert grouping | `InhibitStage` 目前恒返回 nil，占位 | P0 明显缺口，且不能只做“静默复制” |
| 风暴 | 阈值、摘要、恢复状态 | 编排/智能分组与 AIOps 组合 | route 级阈值、storm state、摘要 timer | 当前有实现基础，但要补解释、解除和权限 UX |

### 6.1 当前项目最需要补的不是更多开关，而是解释

供应商控制台都把“命中了哪些维度、为什么合并、为什么没通知”放在配置/预览语境中。当前代码已经保存 `SilencedBy`、`GroupKey`、`RouteID` 和 storm state，但前端还没有形成统一的决策解释面。建议每条 Alert/Fault 提供：

1. 输入事实和来源；
2. 每个 stage 的命中/跳过原因；
3. authoritative source bypass 原因；
4. 最终通知意图、抑制原因和下一次 timer；
5. 规则版本、评估时间和可重放样本。

## 7. 路由和责任计算

### Flashduty

共享集成通过精确/通配符/正则路由到协作空间，支持 continue/stop、默认路由、缺失目标的回退/丢弃、规则重排、历史、diff、恢复和最多 20 条真实告警预览。协作空间内的分派策略再按顺序匹配，延迟、值班、个人/团队、通道、重复和升级阶段都在策略中定义。

### PagerDuty

Event Orchestration 以有序规则集做动态路由、字段增强、priority/severity、suppress/drop 和自动化；Service 再绑定 escalation policy，policy 的 levels 引用 users/teams/schedules。Service Directory 是长期责任目录，Incident 列表是短期响应队列。

### 当前项目

本地 `routeStage` 读取 routes，提供 team/group/wait/interval/repeat/mode，并有约 10 秒缓存；配置 IA 把 Alertmanager 数据源、告警映射、本地告警路由、实体与依赖、团队成员和值班安排分开。对于 authoritative Alert，route stage 被跳过，责任要通过映射和实体 owning team 推导。

### 结论

当前项目的“责任”是两步：

```text
Alert → 映射为 canonical Entity/Fault → owning team / Incident 角色
兼容来源 Alert → 本地 route → team / group → 通知策略
```

这比供应商的单一 Service/协作空间更能表达共享基础设施，但会产生 mapping conflict/unmapped 的运营负担。必须把映射缺失做成一等待办和安全兜底，不能默默落到一个万能默认团队。

## 8. 值班、升级和通知

| 方面 | Flashduty | PagerDuty | 当前项目 |
|---|---|---|---|
| 值班模型 | 日/周/自定义轮换，白夜班、主备、假期覆盖、临时调班、连续/公平轮换、服务日历 | 多层 schedule、限制、时区、override、shift、follow-the-sun | `internal/oncall` + schedules/人员升级；PG timers 驱动链路 |
| 升级模型 | 分派策略的多个阶段，延迟 0–3600 秒，关闭可取消，未关闭/未认领条件 | escalation policy 有序 levels、timeout、重复轮次、ack 停止升级 | personnel/channel escalation、repeat、Ack/Resolve Stop/Cancel 生命周期 timer |
| 通道 | App/电话/SMS/邮件/飞书/钉钉/企微/Slack 等，模板独立 | 推送、电话、短信、邮件、Slack/Teams 等生态，urgency 决定叫醒 | 飞书卡片为主，支持邮件/渠道扩展；outbox + notify_send 定时器 |
| 模板 | Go template、附加信息、多 IM 预览 | 事件/通知模板、工作流和集成扩展 | 通知策略/渠道已有基础，但模板中心和多通道预览不如供应商 |
| 取消/幂等 | 关闭/恢复改变通知链 | ack timeout、resolve、reassign 有明确事件 | 代码约束重复 Ack/Resolve 短路 200，timer chain 有锁屏障，at-least-once |

当前项目在可靠性语义上反而更严格：通知意图写入 outbox/timer，事务内禁止同步外部 IO，ack/resolve 无条件停止生命周期 timer；但在排班日历、临时覆盖、值班预览和多渠道模板 UX 上明显落后。建议先复用已有 scheduler 语义，再补供应商式配置体验，不要重写成浏览器触发的同步通知。

## 9. Incident 协作、影响和恢复

### 9.1 供应商的强项

- Flashduty 把认领、关闭、时间线、状态页、复盘、表单和 AI-SRE 放在故障生命周期周围；
- PagerDuty 把 responders、incident workflows、时间线、Analytics、Business Services、Service Graph 和状态能力放在 Incident/Service 生态中；
- 两者都能较快让值班人从“收到消息”进入“多人协作”。

### 9.2 当前项目的强项

当前 `Incident` 领域包含 declared/mitigating/monitoring/resolved、primary/supporting Fault、included/excluded disposition、incident commander/technical lead/comms/business liaison、任务、恢复判据和事实/假设/决策/自动化/通信/状态等时间线。Incident Room 前端还将恢复门槛、关联 Fault、响应任务、统一时间线和 Guarded Runbook 放在同一工作台。

这使当前项目能表达：

1. “现在只是候选 Fault，尚未宣布 Incident”；
2. “这个 Alert 是症状，不是主故障”；
3. “影响路径尚为 potential，需要人工确认”；
4. “恢复必须满足所有任务、Fault 和证据门槛”；
5. “自动化只能执行受控 action，不能任意 Shell”。

### 9.3 明显缺口

当前项目尚未达到供应商的外围闭环：公开状态页、正式复盘模板、服务标准、响应者邀请、跨团队沟通编排、AI 知识库/插件和成熟分析看板。它们应在核心 Fault/Impact 稳定后补齐，而不是先把产品扩张成 ITSM。

## 10. UX 和控制面设计对比

### Flashduty 的设计语言

Flashduty 以协作空间为上下文，详情页把集成、排除、五类降噪、分派和高级配置集中在一个责任域；现场还能看到配额/权限门禁、默认策略、模板预览、表单未配置提示。优点是配置路径短；风险是功能多、规则组合复杂，且部分子页有权限/版本文案复用。

### PagerDuty 的设计语言

PagerDuty 把 Incidents、Services、People、Automation、AIOps、Analytics、Integrations、Status、AI 分成多个工作台。优点是“当前响应”和“长期服务治理”分离；代价是跨模块跳转多，且本次现场就遇到跨模块重新认证。

### 当前项目的设计语言

[App.tsx](/Users/luoxiaowei/code/go/src/alert-system/web/src/App.tsx:154) 的主导航聚焦运行总览、Fault、Incident、Alert、Service；响应区目前主要是静默；管理员进入控制中心后，[SettingsLayout.tsx](/Users/luoxiaowei/code/go/src/alert-system/web/src/pages/Settings/SettingsLayout.tsx:27) 按数据与模型、组织与责任、响应策略、自动化、兼容接入分组。[OperationsOverview.tsx](/Users/luoxiaowei/code/go/src/alert-system/web/src/pages/OperationsOverview.tsx:54) 直接展示受管基础设施、活动 Fault、已确认业务影响和响应中 Incident。

这是一种“运营 ledger”而非“集成目录”设计：入口优先回答“现在需要判断什么”。它符合项目定位，但需要借鉴供应商的详情抽屉、预览、规则历史和空状态引导，降低控制中心的配置门槛。

## 11. API、开源和部署路线

| 维度 | Flashduty | PagerDuty | 当前项目 |
|---|---|---|---|
| 官方 SDK/CLI | [go-flashduty](https://github.com/flashcatcloud/go-flashduty)、[flashduty-cli](https://github.com/flashcatcloud/flashduty-cli) | [go-pagerduty](https://github.com/PagerDuty/go-pagerduty)、[python-pagerduty](https://github.com/PagerDuty/python-pagerduty) | 内部 REST/前端 client，暂无公开 SDK |
| IaC | 未发现等同 PagerDuty Terraform Provider 的核心官方 provider | [terraform-provider-pagerduty](https://github.com/PagerDuty/terraform-provider-pagerduty) | 部署 manifest/Makefile，策略尚未全量 IaC 化 |
| MCP | [flashduty-mcp-server](https://github.com/flashcatcloud/flashduty-mcp-server)，默认偏只读、任务导向 | [pagerduty-mcp-server](https://github.com/PagerDuty/pagerduty-mcp-server)，默认只读，写工具显式开启 | 尚无公开 MCP；可先提供只读查询/解释工具 |
| Schema | go SDK README 说明 OpenAPI 生成 | [api-schema](https://github.com/PagerDuty/api-schema) 公开 OpenAPI | API 契约在代码与前端 client 内部维护 |
| 核心服务源码 | 未公开 | 未公开 | 全部在本仓库可审查，便于自托管和定制 |
| 运行责任 | 厂商承担 SaaS 可用性、存储和升级 | 厂商承担 SaaS 可用性、存储和升级 | 团队承担 PG、迁移、timer worker、通知凭据和扩缩容 |

当前项目的透明度和可控性是优势，但 API 稳定性、客户端生态和外部自动化入口还没有形成产品化护城河。建议把“只读诊断 API + OpenAPI schema + SDK/MCP”列入公开契约路线，而不是直接开放任意写接口。

## 12. 安全、可靠性和可运营性

### 供应商可借鉴点

1. Flashduty 的集成 key、共享/专属边界、模板和路由预览，减少凭据与责任混淆；
2. PagerDuty MCP 默认只读、Terraform 日志脱敏、编排规则和 integration key 分层，体现最小权限；
3. 两者都把规则、值班和通知作为可审计对象，而不是只在前端 local state 中保存。

### 当前项目已有的强约束

- hook UUID 只在创建/轮换时显示，库内存 SHA-256，轮换立即使旧 URL 失效；
- PG timer 采用独立事务、幂等 handler、at-least-once；
- 通知写意图而非锁内同步外部 IO；
- Alertmanager source mapping 采用 authoritative-safe，避免双重通知/双重抑制；
- Incident Runbook 需要 dry-run、计划哈希、有效期、停重试记录和独立审批；
- 前端统一处理 401，SSE 作为全局事件通道。

### 需要警惕的差距

1. 本地部署把数据库、timer worker、出站凭据和升级容量都变成团队运维责任；
2. 10 秒 route/silence cache 与规则变更需要在 UI 明示生效延迟；
3. authoritative source bypass 可能让管理员误以为本地静默生效，必须在详情页显式展示；
4. inhibit 仍是占位，不能在产品说明中宣称已具备完整根因抑制；
5. 单一默认团队兜底若没有 unmapped/conflict 看板，会隐藏责任数据质量问题。

## 13. 能力矩阵和优先级

| 能力 | 当前项目 | Flashduty | PagerDuty | 对当前项目建议 |
|---|---|---|---|---|
| Alert 去重/恢复 | 已有，含 source/TTL/manual | 已有，事件聚合 | 已有，dedup_key | 保持兼容并补回放 |
| 确定性路由 | 已有（兼容来源） | 强 | 强（编排） | 补版本、preview、diff、rollback |
| 智能相关性 | 模板指纹/规则分组 | 智能告警聚合 | AIOps/grouping | 先做候选建议和解释，暂不自动决策 |
| 抖动/风暴 | 已有基础实现 | 完整产品化 | 通过 AIOps/编排组合 | 补阈值解释、摘要和解除审计 |
| 抑制 | 占位 | 完整 | suppress/drop | P0 实现并明确 authoritative 边界 |
| 服务目录/依赖 | Entity/Relation/Service Catalog | 协作空间/团队 | Service Directory/Graph | 保持图模型，补目录 UX/标准 |
| 值班/升级 | 后端基础已有 | 丰富 | 丰富 | 补假期/临时覆盖/时区预览 |
| 多渠道/模板 | 飞书主路径 + 扩展 | 丰富 | 丰富 | 抽象模板与通道能力，保留 outbox |
| Incident 协作 | Fault/Impact/roles/tasks/timeline/runbook | 故障/状态页/复盘/AI | responders/workflows/analytics | 保持证据门槛，补状态页/复盘/响应者 |
| 只读自动化 | 尚无公开 MCP | MCP/CLI/SDK | MCP/CLI/SDK/Terraform | 先发布只读 explain/query API |
| 自托管 | 强 | 未见核心自托管代码 | 未见核心自托管代码 | 把运维成本做成部署/升级文档与指标 |

## 14. 明确的产品取舍

### 应该借鉴

1. **Flashduty 的降噪工作台**：事件聚合、告警聚合、抖动、静默、抑制、风暴分层显示，带默认值、命中预览和规则历史；
2. **两家的规则可运营性**：预览、命中解释、版本、diff、回滚、测试样本；
3. **PagerDuty 的责任分离**：Service/Entity 目录、Escalation Policy、Schedule、临时覆盖不要互相嵌套成一张巨大表单；
4. **供应商的通知模板和表单**：把通道渲染与响应动作解耦，认领/关闭时采集结构化信息；
5. **API-first 自动化**：OpenAPI schema、Go/TypeScript client、只读 MCP，再由审计过的写工具覆盖有限动作；
6. **生命周期外围**：状态页、复盘、运营指标和 AI 作为 Incident 的后续视图。

### 不应该直接复制

1. 不要让本地 Alertmanager 和 SRE Control 同时拥有 Alert dedup/group/silence/inhibit/通知权；
2. 不要因为聚合成功就自动把高等级 Alert 宣布成 Incident；需经过 Fault/Impact/人类确认门槛；
3. 不要把所有实体压成一棵 Service 树；共享基础设施必须保留多对多关系；
4. 不要让 AI 直接决定高风险修复、关闭 Fault 或扩大影响范围；
5. 不要为追求 SaaS 式功能数量牺牲 PG timer、outbox、幂等和锁屏障等可靠性不变量。

## 15. 建议路线图

### P0：可信决策闭环

- 实现真正的 inhibit evaluator，覆盖根因/同标签/时间窗/保留标记/审计；
- 为每条 Alert/Fault 增加 pipeline decision trace：命中、跳过、原因、规则版本、下一 timer；
- authoritative source 详情页显式展示“本地 route/group/silence/inhibit 未执行”的原因；
- 建立 unmapped/conflict 责任队列和默认兜底告警，避免静默吞错。

### P1：降噪和路由运营体验

- 路由、分组、静默、抑制、风暴规则统一 preview/diff/history/rollback；
- 支持录入最多 N 条历史 Alert 做 dry-run，并显示“将通知谁/为什么不通知”；
- 把 10 秒缓存和生效时间在 UI 中显式化；
- 对 group window、template fingerprint、flapping、storm summary 增加回放测试。

### P2：值班和通知产品化

- 补时区、假期、临时覆盖、交接预览和“现在谁值班”；
- 抽象 Notification Template/Channel，保留 Feishu 卡片但允许 email/webhook/IM 扩展；
- 将延迟、重复、升级取消条件做成可解释的策略模拟器；
- 保持 outbox 和 timer 的事务语义，不在前端直接调用外部通知。

### P3：Incident 生命周期外围

- 增加公共/内部状态页、复盘模板、行动项和响应者邀请；
- 将 Incident 时间线和 Fault Evidence/Impact 关联展示；
- 用 MTTA/MTTR、噪声削减、映射质量和恢复门槛通过率构建运营看板。

### P4：开放自动化

- 生成 OpenAPI schema 和稳定客户端；
- 发布只读 query/explain API 和 MCP；
- 写工具只覆盖 Ack/Resolve/追加时间线/创建任务等低风险动作，默认关闭并有审计；
- 评估 Terraform provider，仅管理可声明的实体、映射、路由和值班配置，不把瞬时 Incident 当 IaC 资源。

### P5：证据辅助智能

- AI 仅基于 Alert、拓扑、Fault Evidence、Impact、变更和时间线给出候选关联/摘要；
- 所有建议显示证据、置信度、缺失数据和可撤回动作；
- 任何关闭 Incident、扩大业务影响、执行 Runbook 的动作继续经过人和现有安全门槛。

## 16. 最终判断

如果目标是“今天就有一个多渠道 on-call 产品”，PagerDuty 和 Flashduty 的成熟度明显更高：它们在连接器、升级、值班、通知、规则和运营外围上形成了完整闭环。

如果目标是“把共享基础设施的技术故障、根因候选、业务影响和恢复证据做成可审计的 SRE Control”，当前项目的方向更独特，也更值得坚持。正确的竞争策略不是复制供应商的所有页面，而是：

```text
保留：Alertmanager 权威边界 + Entity/Relation + Fault/Evidence/Impact + Guarded Incident
吸收：Flashduty 的降噪 UX + PagerDuty 的责任/升级/IaC/API 设计
补齐：解释、抑制、值班体验、状态/复盘、只读自动化
拒绝：双重 Alert 权威、无证据自动宣布、AI 直接高风险操作
```

这会让 `alert-system` 在“告警响应平台”之外形成清晰的产品护城河：它不只回答“谁被叫醒”，还回答“什么是真故障、影响谁、为什么这样判断、何时才可以安全收口”。
