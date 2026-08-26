# PagerDuty 调研报告

> 调研日期：2026-08-21（Asia/Shanghai）
> 现场入口：[PagerDuty Incidents](https://cvtesreops.eu.pagerduty.com/incidents?status=acknowledged%2Ctriggered%2Cresolved)
> 证据标记：**现场观察**来自已登录控制台的只读巡检；**官方文档**来自 PagerDuty Support/Developer；**开源证据**来自 PagerDuty 官方 GitHub；**推断**是基于公开行为模型的架构判断，不代表 PagerDuty 内部实现。

## 1. 一句话结论

PagerDuty 是一个以 **Service（服务）+ Escalation Policy（升级策略）+ Schedule（值班表）** 为责任骨架、以 **Event → Alert → Incident** 为响应主链的企业级事件响应 SaaS。它不只是一个通知转发器：事件先经过去重、编排和告警分组，再绑定服务和当前值班人，按升级级别通知；响应期间提供确认、重新分派、响应者、工作流、时间线和恢复后的分析/状态页能力。

它的设计重心与快猫星云相近，但产品哲学略有不同：

1. PagerDuty 把 **Service** 作为组织和责任的稳定锚点，把“谁负责”放在服务目录、服务标准和升级策略中；
2. 把事件规则逐步迁移到 **Event Orchestration**，以有序规则集决定路由、优先级、严重度、抑制、丢弃和自动化；
3. 用 alert grouping、global alert grouping、dedup key 和优先级/紧急度组合控制噪声与叫醒成本；
4. 通过公开 API、SDK、Terraform Provider、CLI、MCP Server 形成 API-first 的自动化外围，但核心 SaaS 后端并未开源。

## 2. 调研范围和证据边界

### 2.1 控制台现场

最初的事件列表页处于已登录状态，能够看到完整导航、空数据状态、筛选器、批量动作和账户概览。后来从事件页进入 Service Directory（以及直接打开其 URL）时，被重定向到 PagerDuty identity 登录页；本次没有读取或输入用户凭据，也没有把未打开的服务目录页面写成“现场已验证”。服务目录、服务图、值班和编排的细节因此以官方文档和公开代码为主，现场与文档证据在下文分开标注。

本次没有创建事件、编辑服务、改变升级策略、发送通知或调用写 API。控制台里的租户名称、用户姓名、内部服务名和任何密钥均不写入报告。

### 2.2 主要资料

- [Incidents 官方文档](https://support.pagerduty.com/main/docs/incidents)
- [Alerts 官方文档](https://support.pagerduty.com/main/docs/alerts)
- [Event Orchestration](https://support.pagerduty.com/main/docs/event-orchestration) 与 [示例](https://support.pagerduty.com/main/docs/event-orchestration-examples)
- [Global Alert Grouping](https://support.pagerduty.com/main/docs/global-alert-grouping)、[Alert Grouping](https://support.pagerduty.com/main/docs/alert-grouping)
- [Escalation Policies](https://support.pagerduty.com/main/docs/escalation-policies)、[Policies and Schedules](https://support.pagerduty.com/main/docs/escalation-policies-and-schedules)
- [Service Directory](https://support.pagerduty.com/main/docs/service-directory)、[Services and Integrations](https://support.pagerduty.com/main/docs/services-and-integrations)
- [Shift-based Schedules](https://support.pagerduty.com/main/docs/shift-based-schedules)、[Schedule Examples](https://support.pagerduty.com/main/docs/schedule-examples)
- [PagerDuty Developer API Reference](https://developer.pagerduty.com/api-reference/)
- [PagerDuty 官方 GitHub 组织](https://github.com/PagerDuty)

## 3. 控制台信息架构（现场观察）

### 3.1 顶层导航

登录后的汉堡菜单展示了以下一级域：

| 一级域 | 现场可见入口 | 产品含义 |
|---|---|---|
| Incidents | All Incidents、Alerts、Incident Types | 处置对象和事件入口 |
| Services | Service Directory、Business Services、Service Graph、Service Custom Fields | 责任、依赖和服务目录 |
| People | 人员、团队、值班相关对象 | 责任主体 |
| Automation | 自动化和工作流 | 响应动作编排 |
| AIOps | 智能分组、噪声治理等 | 相关性和智能辅助 |
| Analytics | 报表与响应指标 | 运营度量 |
| Integrations | 接入、编排、扩展 | 上下游连接 |
| Status / AI / Support | 状态页、AI、帮助 | 沟通、智能和支持 |

这套 IA 把“事件处置”和“服务责任”并列为两个入口：事件页解决当前响应，服务目录解决长期归属和标准化。这与只围绕告警列表组织的产品相比，更适合大型组织的服务治理。

### 3.2 Incidents 页字段和状态

现场的 Incidents 页（空数据）包含：

- 试用已到期提示；
- 最近 30 天的统计卡，分别显示当前用户/全部用户的 open incidents、triggered、acknowledged 数量；
- 状态筛选：Open、Triggered、Acknowledged、Resolved、Any Status；
- 责任范围：Assigned to me / All；时间范围 Last 30 days；
- 通过 incident 编号跳转、全文搜索；
- 表格列：status、urgency、type、title、created、service、assigned to；
- 批量 Acknowledge、Reassign、Resolve、Snooze；New Incident；
- 最近 7 天活动区（本租户现场显示日志读取错误）；
- 当前值班卡片，展示某个默认升级策略的一层值班关系；
- Getting Started、API、Integrations、Status、Cookie 等外围入口。

空数据很重要：它验证了页面的筛选、批量动作和字段契约，但不能验证真实事件进入后如何排序、通知或合并。试用到期和活动日志读取错误也属于本租户环境现象，不应推断为产品的普遍故障。

### 3.3 子菜单所透露的对象边界

Incidents 子菜单把 Response（All Incidents）、Alerts 和 Incident Types 分开；Services 子菜单把 Service Directory、Business Services、Service Graph 和 Service Custom Fields 分开。由此可见，PagerDuty 并没有把 alert、incident、service 混成一张表：告警是事件信号，故障是人要响应的工作项，服务是责任和配置的长期对象。

## 4. 对象模型和生命周期

官方资料可以归纳出下面的关系：

```text
监控/应用/自动化
       │ Events API / 集成
       ▼
Event（触发或恢复事件）
       │ dedup_key、编排、抑制、路由
       ▼
Alert（服务上的告警）
       │ 分组或手动合并
       ▼
Incident（需要人响应的故障）
       │ 服务 → 升级策略 → 值班表/用户
       ├─通知、确认、重新分派、响应者、工作流、时间线
       └─恢复、分析、状态页、复盘
```

### 4.1 Event、Alert、Incident 的职责

- **Event** 是外部系统发来的事实，通常包含事件动作、去重键、来源和上下文；Events API v2 的公共入口是 `https://events.pagerduty.com/v2/enqueue`，具体字段以 [官方 API 文档](https://developer.pagerduty.com/api-reference/) 为准。
- **Alert** 是服务接收并保留的可处理信号。具有相同 `dedup_key` 的触发事件可以更新既有 alert；恢复事件结束该去重链。
- **Incident** 是响应工作项。一个 incident 可以由一个或多个 alert 形成，挂在某个 service 上，并使用该 service 的 escalation policy；它有触发、确认、恢复等响应状态和完整时间线。

### 4.2 状态、优先级、紧急度

官方文档区分几组容易混淆的概念：

- Incident 状态通常为 **Triggered → Acknowledged → Resolved**。确认会暂时停止升级和重复通知；达到 acknowledgment timeout 后仍未恢复会重新触发；Resolve 结束 incident。
- Alert 可以被抑制、分组或去重，不一定都会形成 incident；没有可用 on-call target 的事件可能被丢弃或无法产生可通知的 incident。
- **Alert 本身不能被 Acknowledge**，只能处于 triggered 或 resolved；Acknowledge 是 Incident 的响应动作。一个 incident 下的所有 alert 都恢复后，incident 才会恢复。这一差异会影响 API 映射和 UI 批量动作设计。
- **Severity** 描述事件严重程度；**Priority** 是组织为响应排序定义的优先级；**Urgency** 描述是否立即叫醒人。服务可配置 high、low、dynamic 或 support-hours urgency，低紧急度不一定自动升级。

这使 PagerDuty 能把“同样严重但是否立即打电话”拆开治理，适合把业务时间、支持时间和夜间响应策略纳入同一服务模型。

### 4.3 时间线和响应参与者

Incident 时间线记录触发、通知、确认、重分派、升级、恢复、备注和自动化事件。官方还支持 responders、incident workflows 等协作对象，使临时专家可以被拉入当前响应而不必永久成为该服务的一线值班人。这个设计把“长期责任”和“临时协助”分离。

## 5. 事件接入与编排

### 5.1 接入层

PagerDuty 的事件接入通常以 integration key/endpoint 为边界：监控系统把事件发送到 Events API 或现成集成，事件再进入 global、service 或 routing 级别的 Event Orchestration。官方 API 和 SDK 暴露 REST 资源，EU 租户使用对应的 `api.eu.pagerduty.com` API 主机（官方 MCP README 也明确列出该主机）。

### 5.2 Event Orchestration 的层次

官方文档把编排拆成：

1. **Global Orchestration**：在事件进入具体服务前统一做字段标准化、路由、抑制、丢弃、优先级/严重度调整或触发自动化；
2. **Service Orchestration**：针对某个服务的事件继续细化；
3. **Event Rules / Routing**：根据来源、内容、PD-CEF 字段、时间、阈值或动态条件执行规则。

规则按顺序求值，命中后可以继续执行或停止；catch-all 可以把未命中的事件路由到默认服务、压制成 suppressed alert、丢弃，或交给后续处理。动作包括 enrichment、severity/priority 设置、suppress、drop、动态路由和自动化触发。

这是一种“策略解释器”思路，而不是把每个集成写成一段固定代码：事件格式先映射到可匹配字段，规则再决定后续对象和动作。官方还限制单个组织可用的 integration key 数量，并在高级条件中支持 schedule、时间窗口和阈值，说明编排层同时承担安全边界和运营规则。

### 5.3 Rulesets 的迁移信号

PagerDuty 官方已将旧的 Rulesets/Event Rules 标记为 **2025-01-31 EOL**，建议迁移到 Event Orchestration；旧规则仍可通过 API 访问。这个演进说明产品在把分散、历史兼容的规则入口收敛成一套分层、可观察的编排模型。对选型而言，不能把旧 Rulesets 教程当成未来主路径。

## 6. 降噪、去重和分组

### 6.1 去重（deduplication）

`dedup_key` 是事件到 alert 的稳定关联键。相同服务和去重键的后续 trigger 会更新同一 alert，而不是无限创建新 alert；resolve 事件结束关联。它适合监控系统已经能稳定生成指纹的场景。

### 6.2 服务级 Alert Grouping

Alert grouping 作用于同一服务中多个 alert 的相关性，可配置 intelligent、content-based、unified、time-based 或关闭等模式。智能模式使用事件内容和选定字段寻找相似性；内容模式依赖指定字段；时间模式按窗口合并。分组后的 alert 共享一个 incident 响应面，但原始 alert 仍保留，便于查看证据。

### 6.3 Global Alert Grouping

Global Alert Grouping 位于 Event Orchestration 之后，允许跨服务把相关 alert 放进同一组。官方文档说明，组内第一个 alert 所属的服务决定 incident 的升级策略，其他服务来源会保留在组里；一个组最多可覆盖 250 个服务。智能字段最多选择 5 个，时间窗口为 5–60 分钟并可滚动延长到 24 小时（具体上限以当前套餐和文档为准）。

这体现出一个关键权衡：跨服务合并能减少风暴，但“第一条 alert 决定负责人”可能让错误路由的代价扩大，因此必须配合服务目录、默认路由和命中解释。

### 6.4 现场与文档的差异

现场 Incident 页只显示结果侧的列表、筛选和批量动作；分组、编排和服务配置并不直接堆在列表里，而是从 Services/Integrations/AIOps 进入。也就是说，PagerDuty 把“响应操作”和“规则建设”分成不同工作台，降低普通值班人员误改策略的风险，但增加了跨模块学习成本。

## 7. Service、Escalation Policy 和 On-call

### 7.1 Service Directory

官方 Service Directory 描述了一个可搜索的服务清单，常见列/卡片信息包括服务状态、owner/team、当前 on-call、最近 incident、未关闭 incident 和服务标准。它是责任治理的目录，而不仅是一个 integration 名称列表。Business Services 和 Service Graph 进一步把技术服务与业务能力、依赖关系连接起来。

由于本次跨模块会话失效，以上字段来自官方 Service Directory 文档，不声明为本次现场页面截图中逐项出现。

### 7.2 Escalation Policy

一个 escalation policy 由有序 level 组成：第一层立即通知目标用户、团队或 schedule；在 escalation timeout 内没有 ack，就进入下一层；可配置重复轮次（官方文档说明最多可重复若干轮，具体上限受套餐限制）。ack 会停止继续升级，resolve 会结束通知链。一个 service 通常绑定一个主要 escalation policy，多个服务可以复用同一值班表。

这套模型把“服务归属”和“通知顺序”分开：服务负责选择策略，策略负责选择当前人，值班表负责把抽象角色解析成具体人。

### 7.3 Schedule 引擎

官方 schedule 文档支持：

- 小时、日、周、月等轮换周期；
- 多层 schedule、限制区间和时区；
- overrides/临时替班；
- follow-the-sun 跨时区接力；
- shift-based schedule 和工作时段；
- 将 schedule 作为 escalation policy 的 target。

这类配置不是一次性“当前值班人”，而是一个可计算的时间函数。每次 incident 通知时，系统需要根据事件时间、层级、限制和 override 求值，才能得到实际收件人。

## 8. 响应、协作和运营闭环

PagerDuty 的产品面覆盖从叫醒到学习的多个阶段：

1. 事件进入并经过编排、去重和分组；
2. 服务绑定升级策略，通知当前值班人；
3. 值班人确认、备注、重分派或拉入 responders；
4. 按 incident workflow/automation 执行检查、沟通或修复动作；
5. 恢复后关闭，保留时间线和响应指标；
6. 通过 Analytics、Status、Business Services/Service Graph 和复盘能力反馈到服务治理。

现场导航同时出现 Automation、AIOps、Analytics、Status、AI，说明 PagerDuty 试图把“事后报表”和“事前服务标准”都放到同一控制面，而不是只卖一个电话通知器。

## 9. API、自动化和开源项目

### 9.1 官方 GitHub 组织

[PagerDuty 官方 GitHub 组织](https://github.com/PagerDuty)公开了 100 多个仓库。与本次分析最相关的项目如下：

| 仓库 | 可验证的实现线索 | 对产品的意义 |
|---|---|---|
| [go-pagerduty](https://github.com/PagerDuty/go-pagerduty) | Go API client/CLI，覆盖 v2 API；提供 `NewClient`、context 方法、可暴露的 HTTP client/`Do`、debug/error 与 rate-limit 处理，并含 webhook v3 签名辅助；仓库采用 Apache-2.0。 | REST 资源按 endpoint 分组，适合服务端和运维脚本复用。 |
| [python-pagerduty](https://github.com/PagerDuty/python-pagerduty) | 官方 Python 公共 API client。 | 说明 API 不依赖单一语言生态。 |
| [api-schema](https://github.com/PagerDuty/api-schema) | 公开 OpenAPI schema；README 说明由自动化流程从内部来源生成，面向公开 API 参考。 | schema 是 SDK、文档和 IaC 的共同契约，核心服务源码仍不公开。 |
| [terraform-provider-pagerduty](https://github.com/PagerDuty/terraform-provider-pagerduty) | Go 编写的 Terraform provider，可用 HCL 管理 service、team、schedule、escalation policy 等资源；部分 API 需要 list 后做内存/缓存查找，provider 提供缓存支持并对认证 header 做安全日志处理。 | 把响应策略当作可审计、可回滚的基础设施配置。 |
| [pagerduty-mcp-server](https://github.com/PagerDuty/pagerduty-mcp-server) | 官方 Python MCP Server；默认只读，可用 `--enable-write-tools` 显式开启写工具；支持 stdio、Streamable HTTP、SSE；README 给出 EU API host `https://api.eu.pagerduty.com`，并警告 HTTP transport 需要自行处理认证/安全。 | AI/自动化入口先以只读为默认，降低误操作面。 |
| [incident-response-docs](https://github.com/PagerDuty/incident-response-docs) | 公开的事件响应文档/示例集合。 | 将产品流程、实践和自动化示例外化，降低采用成本。 |

### 9.2 API-first 的实现推断

从公开 schema、SDK、Terraform provider 和 MCP 可以合理推断：PagerDuty 的外部控制面以版本化 REST API 为稳定契约，控制台、IaC、CLI 和 AI 工具都围绕同一对象集合工作。可能的逻辑组件包括事件接收器、规则求值器、告警/事件存储、schedule 求值器、通知/升级调度器和审计时间线。

但这些是**架构推断**。PagerDuty 没有公开核心 SaaS 的数据库、消息队列、分片、调度器或 AIOps 模型实现，不能据此断言具体采用 Kafka、Postgres、微服务或某种机器学习算法。

### 9.3 安全和权限信号

官方 MCP 默认只读、写工具需要显式开关；Terraform provider 对认证 header 做日志保护；Event Orchestration 以 integration key、规则集和服务边界控制事件流。这些公开设计共同表明：PagerDuty 把“自动化可用”与“自动化可写”分开，适合先在审计/只读模式验证，再逐步开放变更权限。

## 10. 产品优势、代价和风险

### 优势

1. Service → Escalation Policy → Schedule 的责任链稳定、易于规模化；
2. Event Orchestration 把路由、字段增强、抑制、丢弃和自动化统一到有序规则模型；
3. dedup、服务级分组和跨服务全局分组覆盖不同噪声形态；
4. Incident timeline、responders、workflows、analytics 和状态能力形成闭环；
5. 公开 API schema、SDK、Terraform 和 MCP 让策略可以代码化并纳入审计。

### 代价和风险

1. 同一行为可能受 integration、global orchestration、service orchestration、grouping、service settings 和 escalation policy 多层影响，命中解释和回放能力不足时排障困难；
2. Global grouping 以第一条 alert 的服务决定升级策略，错误路由会放大责任偏差；
3. Service-centric 模型强调“谁负责”，但不天然表达故障证据、业务影响或基础设施因果关系；
4. 套餐、试用状态和旧规则迁移会影响实际可用能力；现场已观察到试用到期提示，不能用公开宣传页替代租户能力核对；
5. 核心后端闭源，数据驻留、内部队列、模型和故障回放等能力需要依赖厂商的 SLA 与 API。

## 11. 对当前项目的启示（简述）

对本仓库的直接启发是：可以借鉴 PagerDuty 的 service catalog、升级策略/值班表分层、Event Orchestration 的预览与版本化、Terraform/API/MCP 只读优先；但不应把“产生 incident”等同于“已经确认业务影响”。当前 `alert-system` 的 Fault/Evidence/Impact 门槛仍应保留，PagerDuty 的 Service 更适合作为责任映射输入，而不是替代影响判断。

## 12. 结论

PagerDuty 的核心竞争力是把企业责任网络计算成可靠的叫醒路径：事件编排减少噪声，Service 绑定责任，Schedule 在时间轴上解析人，Escalation Policy 按确认和超时推进通知，Incident 工具再把响应过程沉淀为可审计记录。它是成熟的通用响应平台；如果产品需要更强的基础设施因果、证据链和保守的影响确认，还需要在其 service-centric 模型之上增加显式 Fault/Impact 层。
