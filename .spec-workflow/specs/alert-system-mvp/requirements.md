# Requirements Document — alert-system-mvp

## Introduction

本系统是一个**告警管理平台**（Alert Management / Incident Response），定位对标 PagerTree / Squadcast / AlertOps：接收上游监控系统**已触发的告警事件**，完成去重、收敛、路由、值班通知、响应处置与对外通报的全生命周期管理。

**第一性原理推导的核心链路**：
`告警事件接入(Integration) → 归一化(统一事件模型) → 去重/关联 → 路由到值班团队(Team/On-Call) → 通知与响应(Ack/Resolve) → 复盘与通报(Reports/Status)`

本 spec 覆盖**第一期 MVP 最小闭环**：Integration（接入）+ Alerts（统一事件模型与生命周期）+ Team（团队/路由/值班）+ User（基础认证）。

### 关键架构决策（已经对抗性审查确认）

| # | 决策 | 理由 |
|---|------|------|
| D1 | 只接收「已触发的告警事件」，**不摄入 OTLP 原始遥测**（metric/log/trace 原始数据） | 规则求值留给上游（Prometheus/Grafana/Loki 等）；否则需自建存储+规则引擎，范围膨胀一个数量级 |
| D2 | metric/log/trace 产生的告警**归一处理**：统一信封（envelope）+ 类型化载荷（signal_context） | 核心管道只读信封；新增信号类型只需新增 adapter，管道零改动 |
| D3 | **不自建 CMDB**，作为外部 source of truth 通过 Integration 对接；Alert 实体预留一等公民字段 `resource_ref` | 避免双写和数据漂移；资源标签采用 OTel semantic conventions（`service.name`/`host.name` 等） |
| D4 | 生命周期策略随信号类型显式建模：`resolve_policy`（source-driven / ttl / manual）为信封级必备字段 | 日志/trace 类告警多为一次性事件，无上游 resolved 信号，不做 TTL 自动恢复会导致 firing 列表变垃圾场 |
| D5 | 去重键（fingerprint）策略随信号类型走：metric 用「规则 ID+标签集」，log/trace 用「规则 ID+资源」，**内容不参与哈希** | 日志内容参与哈希会导致每条指纹都不同，去重完全失效 |
| D6 | 降噪管道借鉴 Alertmanager 的成熟分层：`去重 → 分组 → 静默 → 抑制 → 路由 → 通知(重试/重复提醒)`，各阶段独立可插拔 | Alertmanager 是该领域十年验证的事实标准；只做去重不做分组/静默，通知风暴与维护窗口误报必然发生 |
| D7 | LLM 智能分析（分析/去重/抑制/根因）作为**旁路顾问层**预留：挂在管道各阶段的扩展点上、异步产出建议性标注，**不阻塞、不直接决策**核心管道 | LLM 有延迟/成本/幻觉三重不确定性，放进同步管道会破坏可靠性 NFR；竞品（Squadcast Reliability AI）同样定位为辅助层 |
| D8 | 告警自愈（Auto-Remediation）定位为**动作编排的触发方**而非执行方：本系统负责「何时触发、触发什么、结果回写」，工作流执行交给外部编排引擎（如蓝鲸标准运维/作业平台、Ansible） | 与 D1/D3 同一边界纪律——不在告警系统内造工作流引擎；自愈误执行会放大故障，执行层必须复用有审计/回滚能力的成熟平台 |

### 业界实践借鉴映射

| 机制 | 借鉴来源 | 本系统落点 |
|------|---------|-----------|
| fingerprint = 标签集哈希 | Alertmanager | D5 / R3 |
| Grouping（group_by / group_wait / group_interval） | Alertmanager | R9 |
| Silence（label matcher + 时间窗） | Alertmanager | R10 |
| Inhibition（高级别告警压制关联低级别） | Alertmanager | 二期（见开放问题 Q3） |
| repeat_interval（未恢复重复提醒） | Alertmanager | R6.5 |
| 树形路由 + matcher | Alertmanager route tree | R5（本期先做有序规则列表，树形结构为演进方向） |
| 值班表/升级链模型 | GoAlert / Grafana OnCall | R5 |
| 通知策略（渠道升级序列/urgency 分级触达） | PagerDuty notification rules / Grafana OnCall notification policies | R13 |
| 变更事件关联（Change Events / annotations） | PagerDuty / Grafana | R7.4 |
| AI 根因调查链（自动聚合上下文→假设→取证） | Grafana Sift / PagerDuty AIOps | R11.3 |
| 告警时间线与响应指标 | PagerTree / PagerDuty | R4 |

### 完整模块规划（8+1）与本期范围

| 模块 | 本期 (MVP) | 后续 |
|------|-----------|------|
| Integration（接入/对接） | ✅ inbound adapters + 通知渠道 | CMDB 同步、更多 adapter |
| Alerts（告警生命周期） | ✅ 统一模型/去重/分组/静默/状态机 | 抑制（Inhibition）、拓扑关联收敛、根因提示 |
| Team（团队/路由/值班） | ✅ 团队、路由规则、值班表、升级 | 高级排班（轮换/交接） |
| User（用户/认证） | ✅ 基础账号 + RBAC（预留 SSO） | SSO 对接 |
| Overview（总览） | ➖ 仅基础列表统计 | 拓扑健康视图 |
| Broadcast（广播） | ❌ | 二期 |
| External Status（对外状态页） | ❌ | 二期 |
| Reports（报表/复盘） | ❌ | 二期 |
| AI/LLM 智能分析（预留） | ➖ 仅预留管道扩展点与数据基础 | 二期+：告警摘要、辅助去重/抑制建议、根因分析 |
| 自愈 Remediation（预留） | ➖ 仅预留动作触发接口与时间线记录 | 二期+：对接外部编排引擎、条件自动触发 |
| Rule Engine（自研规则引擎） | ❌ **非目标** | 需单独立项评估 |

## Alignment with Product Vision

暂无 steering 文档（product.md）。本节以 `examples/some-examples/` 竞品调研（PagerTree/AlertOps/Squadcast/PagerDuty）为产品愿景依据：做一个面向内部 IT/DevOps 团队的自研告警管理平台，替代商业 SaaS，并与内部生态（蓝鲸 CMDB、飞书）深度集成。

## Requirements

### Requirement 1 — 告警事件接入（Integration inbound adapters）

**User Story:** 作为 SRE，我希望把 Prometheus Alertmanager、Grafana 等上游监控系统触发的告警通过 webhook 接入本平台，以便所有来源的告警在一处集中管理。

#### Acceptance Criteria

1. WHEN 上游以 Alertmanager webhook 格式 POST 告警 THEN 系统 SHALL 将其翻译为统一告警事件并进入处理管道
2. WHEN 上游以 Grafana webhook 格式 POST 告警 THEN 系统 SHALL 将其翻译为统一告警事件并进入处理管道
3. WHEN 上游使用平台定义的通用 Webhook 格式（含鉴权 token）POST 告警 THEN 系统 SHALL 校验鉴权并接收该事件
4. IF 请求鉴权失败或 payload 不合法 THEN 系统 SHALL 返回 4xx 并记录拒绝原因，SHALL NOT 丢弃时静默成功
5. WHEN 新增一种上游告警来源 THEN 系统 SHALL 只需新增一个 adapter 实现，SHALL NOT 修改核心处理管道代码
6. WHEN adapter 接收事件成功 THEN 系统 SHALL 保证事件至少一次（at-least-once）进入管道，接入点故障恢复后 SHALL NOT 丢失已确认接收的事件

### Requirement 2 — 统一告警事件模型（信封 + 类型化载荷）

**User Story:** 作为平台开发者，我希望 metric/log/trace 等不同信号来源产生的告警在进入管道后只有一种统一结构，以便去重、路由、通知逻辑不感知来源差异。

#### Acceptance Criteria

1. WHEN 任意 adapter 输出告警事件 THEN 该事件 SHALL 包含统一信封字段：`fingerprint`、`severity`（critical/warning/info）、`status`（firing/resolved）、`title`、`labels`、`source`、`resource_ref`、`resolve_policy`、时间戳
2. WHEN 事件携带来源特有上下文（metric 表达式/当前值、log 样本行/查询 URL、trace_id/慢 span） THEN 系统 SHALL 将其存入 `signal_context` 类型化载荷，仅用于展示与跳转
3. WHEN 核心管道（去重/路由/通知/状态机）处理事件 THEN 其逻辑 SHALL 只依赖信封字段，SHALL NOT 读取 `signal_context`
4. WHEN 事件携带资源标识 THEN 系统 SHALL 采用 OTel semantic conventions 键名（如 `service.name`、`host.name`、`deployment.environment`）规范化到 `resource_ref` 与 `labels`
5. IF 上游 payload 缺失 severity 或无法映射 THEN 系统 SHALL 按 adapter 配置的默认等级兜底并在事件上标记「等级为兜底值」

### Requirement 3 — 去重与抑制（fingerprint）

**User Story:** 作为值班工程师，我希望同一问题反复触发的告警被合并为一条并累计次数，以便告警列表不被重复噪音淹没。

#### Acceptance Criteria

1. WHEN metric 类告警进入管道 THEN 系统 SHALL 以「规则 ID + 标签集」生成 fingerprint
2. WHEN log/trace 类告警进入管道 THEN 系统 SHALL 以「规则 ID + 资源标识」生成 fingerprint，日志/trace 内容 SHALL NOT 参与哈希
3. WHEN 新事件的 fingerprint 与某条未恢复告警相同 THEN 系统 SHALL 合并为同一告警并将去重计数 +1、更新最近发生时间，SHALL NOT 产生新告警或重复通知
4. WHEN 相同 fingerprint 的告警已处于 resolved 状态 THEN 新到达的 firing 事件 SHALL 创建新一轮告警（不复活旧告警）

### Requirement 4 — 告警生命周期与响应动作

**User Story:** 作为值班工程师，我希望对告警执行认领（Ack）、解决（Resolve）、交接（Handoff）等动作，且不同来源的告警都有明确的恢复机制，以便告警列表始终反映真实的故障现状。

#### Acceptance Criteria

1. WHEN 告警创建 THEN 其状态机 SHALL 为：`firing → acknowledged → resolved`，且 firing 状态可直接被 resolve
2. WHEN 值班人执行 Ack THEN 系统 SHALL 记录响应人与响应耗时（Response Time），并停止对该告警的升级
3. WHEN `resolve_policy = source-driven` 且上游发来 resolved 事件 THEN 系统 SHALL 自动将告警置为 resolved
4. WHEN `resolve_policy = ttl` 且超过配置的 TTL 未再收到同 fingerprint 的 firing 事件 THEN 系统 SHALL 自动将告警置为 resolved 并标记「TTL 自动恢复」
5. WHEN 值班人执行 Handoff THEN 系统 SHALL 将告警责任人转移给指定用户/团队并记录交接历史
6. WHEN 告警发生任何状态变更 THEN 系统 SHALL 记录不可篡改的时间线（谁、何时、什么动作）

### Requirement 5 — 路由、值班与升级（Team / On-Call / Escalation）

**User Story:** 作为团队负责人，我希望按标签/资源归属把告警路由到正确团队，并按值班表通知当班人、无响应时自动升级，以便告警总能在时限内送达能处理它的人。

#### Acceptance Criteria

1. WHEN 告警进入路由阶段 THEN 系统 SHALL 按优先级有序匹配路由规则（基于 labels 与 resource_ref），命中即路由到目标团队
2. IF 无任何路由规则命中 THEN 系统 SHALL 路由到配置的默认团队，SHALL NOT 静默丢弃
3. WHEN 告警路由到团队 THEN 系统 SHALL 依据该团队当前生效的值班表确定通知对象
4. WHEN 告警在升级策略配置的时限内未被 Ack THEN 系统 SHALL 按升级链逐级通知下一责任人
5. WHEN 管理员配置值班表 THEN 系统 SHALL 支持按周期轮换的排班并可查询「某时刻谁在值班」

### Requirement 6 — 通知渠道（Integration outbound）

**User Story:** 作为值班工程师，我希望通过飞书（首选渠道）及 Webhook 及时收到分级的告警通知，以便第一时间响应。

#### Acceptance Criteria

1. WHEN 告警需要通知 THEN 系统 SHALL 支持飞书与通用 Webhook 两类出站渠道（渠道层可插拔，后续可扩展）
2. WHEN critical 告警触发通知 THEN 系统 SHALL 即时发送；WHEN 低等级告警 THEN SHALL 支持按团队配置降噪（如仅工作时间通知）
3. IF 通知发送失败 THEN 系统 SHALL 按退避策略重试并记录失败原因，最终失败 SHALL 在告警时间线上可见
4. WHEN 值班人在飞书消息中点击 Ack/Resolve 交互按钮 THEN 系统 SHALL 执行对应动作（若飞书应用权限允许；否则降级为跳转链接）
5. WHEN 告警持续未恢复且超过配置的 `repeat_interval` THEN 系统 SHALL 重新发送提醒通知（借鉴 Alertmanager），且重复提醒 SHALL 遵守分组与静默规则

### Requirement 7 — 上下文关联：CMDB 资源 + 变更事件（预留 + 只读消费）

**User Story:** 作为值班工程师，我希望告警能标注所属服务/主机及其负责人信息（来自外部 CMDB），并能看到相关资源最近的变更事件，以便快速判断影响面、找到正确的人、关联疑似变更根因。

#### Acceptance Criteria

1. WHEN 告警事件进入管道 THEN `resource_ref` SHALL 作为 Alert 实体的一等公民字段被持久化（即使本期未对接 CMDB，仅存规范化标签）
2. IF 已配置 CMDB 对接（本期可选实现） THEN 系统 SHALL 通过 Integration 拉取/缓存只读资源信息用于告警富化，SHALL NOT 在本系统内修改 CMDB 数据
3. WHEN CMDB 不可用 THEN 告警核心管道 SHALL 正常工作（富化降级为跳过），SHALL NOT 阻塞告警处理
4. WHEN 上游发布/配置系统投递变更事件（deploy、配置变更、扩缩容等，借鉴 PagerDuty Change Events / Grafana annotations 模式） THEN 系统 SHALL 作为一类 inbound integration 接收，规范化后与 `resource_ref` 关联存储（本期可仅实现通用 webhook 一种）
5. WHEN 用户查看告警详情 AND 存在同资源（或关联组件）近期变更事件 THEN 系统 SHALL 在详情页展示「最近变更」上下文

### Requirement 8 — 用户与权限（User / RBAC）

**User Story:** 作为管理员，我希望管理用户账号并按角色控制权限，以便普通成员只能处理告警、管理员才能修改路由与值班配置。

#### Acceptance Criteria

1. WHEN 用户登录 THEN 系统 SHALL 支持账号密码认证，且认证层 SHALL 预留 SSO/OIDC 扩展点
2. WHEN 用户访问资源 THEN 系统 SHALL 按角色（admin / member）鉴权：member 可查看与处置告警，admin 可管理团队/路由/值班/集成配置
3. IF 未认证请求访问 API（webhook 接入端点除外） THEN 系统 SHALL 拒绝并返回 401

### Requirement 9 — 分组通知（Grouping，借鉴 Alertmanager）

**User Story:** 作为值班工程师，我希望同一类告警（如同一集群批量宕机）被合并成一条聚合通知而不是几十条独立消息，以便不被通知风暴淹没。

#### Acceptance Criteria

1. WHEN 路由规则配置了 `group_by` 标签集 THEN 系统 SHALL 将命中该路由、分组标签值相同的告警聚合为一个通知组
2. WHEN 新分组产生首条告警 THEN 系统 SHALL 等待 `group_wait`（默认 30s，可配）后再发送首次聚合通知，期间到达的同组告警合并入该通知
3. WHEN 已存在的分组有新告警加入或组内告警恢复 THEN 系统 SHALL 至少间隔 `group_interval` 才发送组更新通知
4. IF 路由未配置 `group_by` THEN 系统 SHALL 按单告警直发（不分组），行为向后兼容
5. WHEN 发送聚合通知 THEN 消息 SHALL 呈现组内告警数量、共同标签与最高 severity

### Requirement 10 — 静默与维护窗口（Silence，借鉴 Alertmanager）

**User Story:** 作为 SRE，我希望在计划维护前用标签匹配器创建限时静默规则，以便维护期间相关告警不打扰值班人、维护结束后自动恢复告警。

#### Acceptance Criteria

1. WHEN 用户创建静默规则（label matchers：等于/不等于/正则 + 起止时间 + 创建人 + 备注） THEN 系统 SHALL 在生效期内对匹配告警不发送任何通知
2. WHEN 告警被静默 THEN 告警本身 SHALL 仍正常接入、去重、记录与展示（列表可按 silenced 状态过滤），SHALL NOT 被丢弃
3. WHEN 静默到期 THEN 系统 SHALL 自动失效该规则；IF 匹配的告警仍处于 firing THEN 恢复其后续通知（含 repeat_interval 提醒）
4. WHEN 用户查看告警详情 THEN 被静默的告警 SHALL 显示命中的静默规则及其创建人与备注
5. WHEN 静默规则被手动提前删除 THEN 匹配告警的通知 SHALL 立即恢复正常流程

### Requirement 11 — LLM 智能分析扩展点（本期仅预留）

**User Story:** 作为平台负责人，我希望系统在架构上预留 LLM 智能分析（告警摘要、辅助去重/抑制建议、根因分析）的接入点和数据基础，以便未来接入 AI 能力时不需要重构核心管道。

#### Acceptance Criteria

1. WHEN 设计核心管道各阶段（去重/分组/静默/路由/通知） THEN 每个阶段 SHALL 暴露可插拔的扩展点（hook/plugin 接口），本期可空实现
2. WHEN 未来 LLM 分析器接入 THEN 其 SHALL 以异步旁路方式消费告警事件、产出**建议性标注**（annotation：疑似重复、疑似关联、根因假设、摘要），SHALL NOT 阻塞或直接改写核心管道的决策
3. WHEN LLM 分析器执行根因调查 THEN 其 SHALL 能组合以下上下文（目标调查链：`告警 instance → 关联组件/业务(CMDB 图) → 近期变更事件 → 历史告警趋势 → 按需查日志 → 根因假设 → 触发诊断 runbook`）：
   - 从 `resource_ref` 出发遍历 CMDB 关联组件与业务（依赖 R7）
   - 聚合该资源及关联组件的近期变更事件（依赖 R7.4）与历史告警趋势（依赖本条第 5 款数据基础）
   - 通过**按需只读查询**接口检索上游日志/指标平台（查询结果不落库，不违反 D1 接入边界）
   - 判断后经 R12 动作触发器执行只读诊断类 runbook 辅助定位
4. WHEN LLM 产出标注 THEN 标注 SHALL 与告警关联存储并在详情页展示，且 SHALL 明确标识为 AI 建议（与人工/规则结果区分）
5. WHEN 本期落库告警数据 THEN 时间线、去重计数、处置动作、标签 SHALL 完整持久化——这是未来 LLM 分析与评估的数据基础
6. IF LLM 服务不可用或超时 THEN 核心告警链路 SHALL 完全不受影响

### Requirement 12 — 告警自愈触发（Auto-Remediation，本期仅预留）

**User Story:** 作为 SRE，我希望特定告警（如磁盘满、进程宕）能触发预定义的自愈流程（清理/重启/扩容），以便常见故障无人值守恢复、值班人只处理机器搞不定的问题。

#### Acceptance Criteria

1. WHEN 设计告警动作层 THEN 系统 SHALL 预留「动作触发器」接口：按规则（label/severity/资源匹配）将告警上下文以 webhook/API 方式投递给外部编排引擎，本期可空实现
2. WHEN 未来自愈动作被触发 THEN 触发记录（规则、目标流程、参数、触发时间） SHALL 写入告警时间线；WHEN 编排引擎回调执行结果 THEN 结果（成功/失败/输出摘要） SHALL 同样回写时间线
3. WHEN 配置自愈规则 THEN runbook SHALL 区分两类风险等级：**只读诊断类**（查状态/抓日志/收集现场，无副作用）MAY 配置为自动执行；**变更执行类**（重启/清理/扩容）默认模式 SHALL 为「需人工确认后执行」（值班人在通知中一键批准），全自动执行 SHALL 为按规则显式开启的例外
4. WHEN 同一 fingerprint 的告警在配置的冷却窗口内重复触发 THEN 系统 SHALL NOT 重复触发自愈（防重启风暴/自愈死循环），并升级通知人工介入
5. IF 自愈执行成功且回调确认恢复 THEN 系统 MAY 按规则配置自动 resolve 该告警并标记「自愈恢复」
6. IF 外部编排引擎不可用 THEN 告警核心链路 SHALL 不受影响，自愈降级为普通通知

### Requirement 13 — 通知策略：渠道升级与扩散（Notification Policy）

**User Story:** 作为团队负责人，我希望按告警等级配置「初始渠道 + 超时未响应后的渠道升级/扩散序列」，并且不同业务/service/组件可以绑定不同策略，以便低等级告警安静送达、高等级告警在无响应时逐步提高触达强度。

#### Acceptance Criteria

1. WHEN 管理员定义通知策略 THEN 策略 SHALL 表达为「按 severity 区分的渠道升级序列」：每级包含渠道集合与等待时长，例如 warning：`email → (5min 未 Ack) → IM`；critical：`IM → (3min 未 Ack) → 电话`
2. WHEN 告警在某级渠道通知后、等待时长内未被 Ack THEN 系统 SHALL 执行下一级动作；每级动作 SHALL 支持两种模式：**替换**（改用更强渠道）或**扩散**（叠加新渠道/更大通知面）
3. WHEN 渠道升级发生 THEN 告警的 severity 与 status SHALL NOT 改变（渠道升级只提高触达强度，不改写告警语义），且升级过程 SHALL 记入时间线
4. WHEN 告警被 Ack THEN 该告警的渠道升级序列 SHALL 立即终止
5. WHEN 绑定策略 THEN 系统 SHALL 支持按业务/service/component 维度（经路由规则、labels、resource_ref 匹配）绑定不同策略，未命中时按「团队默认策略 → 全局默认策略」逐级兜底
6. WHEN 通知策略引用了未启用的渠道（如电话渠道尚未接入供应商） THEN 系统 SHALL 在配置时校验并提示，SHALL NOT 在运行时静默跳过

> 与 R5 升级链的关系：**渠道升级**（同一批责任人、更强的触达方式，本条）与**人员升级**（下一级责任人，R5.4）是两个正交维度，均由「超时未 Ack」驱动、被 Ack 终止，可组合使用。

## Non-Functional Requirements

### Code Architecture and Modularity
- **Single Responsibility Principle**: adapter、管道、路由、通知各自独立成包，每个文件单一职责
- **Modular Design**: inbound adapter 与 outbound 通知渠道均为可插拔接口实现；核心管道不依赖任何具体 adapter
- **Dependency Management**: 核心域（Alerts/Team）不得依赖 Integration 的具体实现，依赖方向：adapter → 统一模型 ← 核心域
- **Clear Interfaces**: 信封 schema 是 adapter 与管道之间唯一契约；signal_context 对管道不可见

### Performance
- 单实例告警接入吞吐 ≥ 100 events/s（内部平台规模足够，显式不追求海量）
- 从接入到发出首次通知的端到端延迟 P95 ≤ 10s

### Security
- Webhook 接入端点必须携带 per-integration token 鉴权；管理 API 必须登录鉴权
- 密钥/token 不落日志；通知内容中的敏感字段可配置脱敏

### Reliability
- 告警事件 at-least-once 处理：已确认接收的事件在进程重启后不丢失（持久化队列或先落库再处理）
- 通知失败自动重试（指数退避，上限可配）；升级链保证无人响应时不会静默终止
- CMDB、飞书等外部依赖不可用时，核心接入-去重-路由链路必须可用（降级而非阻塞）

### Usability
- 告警详情页一屏呈现：状态、等级、资源、去重计数、时间线、signal_context 跳转链接（对齐 PagerTree 调研截图的信息密度）
- 所有列表支持按 severity/status/team/label 过滤

## 显式假设与开放问题（不确定性声明）

| # | 类型 | 内容 | 把握度 |
|---|------|------|-------|
| A1 | 假设 | 首选 IM 通知渠道为飞书（依据：内部生态） | 高，但需确认 |
| A2 | 假设 | 上游以 Alertmanager/Grafana 为主 | 中，需确认实际监控栈 |
| A3 | 假设 | CMDB 指蓝鲸 CMDB，本期只预留字段、对接可延后 | 中 |
| Q1 | 开放 | log 类告警 TTL 默认值（建议 1h，按规则可覆盖） | 待定 |
| Q2 | 开放 | 电话/短信渠道已确认为 critical 告警的升级终点（R13），但供应商/对接方式待选型；选型前策略引擎先就位、渠道以插件位预留 | 待定 |
| Q3 | 开放 | Inhibition（抑制）是否提前到 MVP：Alertmanager 将其列为核心，但规则配置复杂且依赖良好的标签体系，暂定二期 | 待定 |
| Q4 | 开放 | 自愈执行引擎选型（蓝鲸标准运维/作业平台？Ansible/AWX？自研脚本网关？）——影响 R12 触发器的对接协议 | 待定 |

## 三个月后失效风险（对抗性预判）

1. **最大风险**：D4/D5（生命周期与去重策略）设计不当导致告警列表噪音失控、狼来了效应——因此两者列为信封级必备字段而非后补
2. **次风险**：接入边界被突破（有人要求直接接原始日志流做规则求值）——D1 作为显式非目标写入，突破须重新立项
3. **规模误判**：若接入规模远超 100 events/s（如全公司接入），持久化与队列选型需重估
