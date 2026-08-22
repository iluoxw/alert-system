# /services 通用服务可观测性工作区设计

> 状态：Proposed
> 日期：2026-08-22
> 完整交接稿：[Service Observability 完整设计与开发交接稿](2026-08-22-service-observability-complete-design-handoff.md)
> 决策依据：[ADR-0006](../adr/0006-service-observability-workspaces-and-optional-agent-port.md)
> 路由族规范：[Services 路由族页面与交互规范](2026-08-22-services-route-family-page-spec.md)
> 页面规范：[Service Observability Workspace 页面与交互规范](2026-08-22-service-observability-page-spec.md)
> 范围：产品信息架构、Service Profile、诊断上下文、只读 tools 与 E2B 首个案例；不包含 UI、后端或 Agent 实现

## 1. 目标

把 alert-system 的 /services 从责任目录扩展为统一的服务运行入口。每个服务通过相同契约获得：

1. 服务总览与 ownership；
2. Google 四大黄金信号；
3. 服务和调用边 RED；
4. 宿主、池、队列、存储 USE；
5. 动态依赖拓扑；
6. 面向一次请求、Trace、实体或 Fault 的聚焦诊断；
7. OpenObserve 等来源的可回看证据；
8. 与 alert-system Fault、Impact、Incident 的一致关联；
9. 未来可被 Agent 复用的稳定上下文与只读 tool 边界。

E2B 是第一个完成全部能力的案例，用于证明这一抽象能够服务复杂平台系统，而不是把 E2B 逻辑写进 alert-system 核心。

## 2. 事实、推断与待验证假设

### 2.1 已确认事实

- /services 已有 Service Registration、owner team、tier、Dashboard、Runbook、Alias 和依赖关系。
- service_registrations 当前由 API 固定创建 application/business_system 实体，不能正确表示所有平台服务。
- alert-system 拥有 canonical Entity/Relation、Alert、Fault、Impact、Incident、用户身份与人工裁决。
- E2B UAT 当前把 OTLP metrics/派生 RED 双写到 OpenObserve 与 Prometheus-compatible 后端；原始 logs/traces 主要在 OpenObserve，节点与容量指标以 Prometheus-compatible 查询链路为首选。`sandbox_id` 是当前证据关联字段。
- Alertmanager 是 Alert 生命周期权威，Service Workspace 不能重新实现 Alert 状态机。
- E2B 原型中的数据是 fixture，不是生产实时查询。
- Agent 方案尚未确定，本阶段不实现 Agent。

### 2.2 设计推断

- Service Workspace 应当绑定 Service Registration，而不是用路由名硬编码 e2b。
- 服务之间真正稳定的共同点是 identity、topology、signals、evidence、domain state 和 typed tools，不是完全相同的业务阶段名称。
- UI 必须通过有限 renderer 解释 profile；若允许服务上传 React 组件，平台会退化成不可治理的插件市场。
- Tools 必须先支持确定性 UI 和人工调查，才能证明它们适合作为未来 Agent 工具。

### 2.3 待验证假设

- golden_overview、request_journey、dependency_topology 与 resource_fleet 足以表达 E2B，并至少覆盖一个第二服务。
- 常用 Service Workspace 首屏可以在 2 秒内返回基本数据，慢证据源随后分块加载。
- 一个 profile 的默认拓扑可以限制在 100 个节点和 200 条边以内；更大范围必须显式展开。
- Service Registration 扩展到 platform service 后，不会破坏现有 Alert Mapping 对 application impact 的判定。

## 3. 路由与信息架构

### 3.1 路由

| 路由 | 作用 | 说明 |
|---|---|---|
| /services | 服务目录 | ownership、tier、健康摘要、未治理状态 |
| /services/:service_id | 服务总览 | 默认 landing page，展示服务说明、黄金信号、Fault、依赖和入口 |
| /services/:service_id/observability | 可观测性工作区 | 拓扑、聚焦诊断、证据、资源下钻和时间线 |
| /infrastructure/:entity_id | 实体详情 | 保留现有实体级视角，从 Service Workspace 双向深链 |
| /faults/:fault_id | Fault 工作区 | 使用现有领域页面，不在 Service Workspace 复制 |
| /incidents/:incident_id | Incident 响应室 | 使用现有领域页面，不在 Service Workspace 复制 |

service_id 使用 Service Registration 的稳定 ID。URL 中的 focus 是可分享的视图状态，不是权限凭据：

~~~text
/services/{service_id}/observability
  ?focus_kind=sandbox_id
  &focus_value=sbx_hp16c3
  &from=2026-08-22T02:04:22Z
  &to=2026-08-22T02:09:22Z
~~~

服务端仍需按当前用户重新校验 service、entity 和 evidence 可见性。

### 3.2 /services 目录

现有 Service Directory 保留搜索、团队和 tier。卡片逐步增加：

- 当前健康：Healthy / Degraded / Critical / Unknown；
- 数据新鲜度；
- active Fault 与 confirmed Impact 数量；
- “打开服务”主入口；
- Dashboard、Runbook、实体详情作为次级入口。

目录健康是 profile 定义信号的摘要，不根据“最近没有 Alert”自动判绿。

### 3.3 服务总览

服务总览回答五个问题：

1. 谁负责？
2. 用户当前是否受影响？
3. 有哪些 active Fault / Incident？
4. 依赖哪些平台能力？
5. 从哪里开始诊断？

总览不承载完整拓扑交互，只提供黄金信号、Fault/Incident 摘要、关键依赖和最近事件。

### 3.4 可观测性工作区

| 区域 | 通用职责 | E2B 映射 |
|---|---|---|
| 顶部 | 黄金信号、时间窗、环境、数据新鲜度 | Create latency/traffic/errors、最坏 HugePages saturation |
| 左侧 | focus 输入与可选 journey 阶段 | sandbox_id；ACCEPT、PLACE、FC START、ENVD READY、ROUTED |
| 中央 | dependency/request topology、边 RED、节点健康 | SDK → Traefik → API → Orchestrator → Envd，资源存储作关联节点 |
| 右侧 | 诊断摘要、路径、证据、Fault 与影响、事件 | PLACE 失败摘要、OpenObserve EvidenceRef、candidate Fault |
| 底部 | 统一领域与运维时间线 | 发布、Alert、Fault、人工决策、恢复 |

Agent 未启用时，右侧第一个页签命名为“诊断摘要”，内容来自确定性规则和已取得证据。只有未来 Assistant Provider 可用且通过安全评审后，才出现“AI 助手”页签；二者不能混为一谈。

## 4. 指标方法的固定语义

| 方法 | 对象 | 指标 | 回答的问题 |
|---|---|---|---|
| 四大黄金信号 | 服务/用户旅程 | Latency、Traffic、Errors、Saturation | 用户是否受影响？ |
| RED | 服务节点、请求边 | Rate、Errors、Duration | 哪个服务或哪一跳异常？ |
| USE | 资源节点、资源舰队 | Utilization、Saturation、Errors | 哪个资源约束正在逼近或拒绝工作？ |

约束：

- Profile 不能把 CPU/HugePages 等 USE 指标绑定到请求边。
- Traffic 不能仅因高而判 Critical，必须结合容量、基线或 SLO。
- 服务级 Saturation 默认显示最坏位置并允许 top-k/分布下钻，不能只显示平均值。
- 缺数或超过 freshness TTL 一律为 Unknown。
- RED 与 USE 同期异常只形成候选关联；因果声明仍需要 Trace、错误日志、拒绝事件或人工证据。
- 健康色是派生视图，原始值、窗口、聚合、阈值来源和 observed_at 必须可查看。

## 5. Service Observability Profile

Profile 是声明式、版本化的 UI 与数据语义契约。JSON Schema 见：

- [ServiceObservabilityProfile](../contracts/service-observability/v1/service-observability-profile.schema.json)
- [E2B Profile 示例](../contracts/service-observability/v1/examples/e2b.profile.json)

概念结构：

~~~ts
interface ServiceObservabilityProfileV1 {
  schema_version: "1.0";
  profile_key: string;
  profile_version: number;
  service_ref: {
    service_id: string;
    root_entity_ids: string[];
  };
  focus_kinds: FocusKind[];
  journeys: JourneyDefinition[];
  views: ViewDefinition[];
  signal_bindings: SignalBinding[];
  tool_refs: string[];
  limits: ProfileLimits;
}
~~~

### 5.1 Profile 可以声明

- focus key 的显示名、格式约束和是否默认；
- journey 的有序阶段与阶段到拓扑实体/关系的映射；
- 从有限 renderer 中选择页面区块；
- signal semantic slot 到 tool output field 的绑定；
- 默认时间窗、freshness TTL、节点/边/行数限制；
- 服务端 Tool Registry 中允许使用的 tool key。

### 5.2 Profile 不可以声明

- SQL、LogQL、PromQL 或任意查询文本；
- HTTP header、Token、Cookie、Secret 或 DSN；
- Shell、SSH、Runbook 执行命令；
- React/Vue/HTML/脚本代码；
- prompt、模型名或 Agent 内部指令；
- Fault/Impact/Incident 的私有状态机；
- 绕过 RBAC 或扩大租户/时间范围的规则。

## 6. 通用渲染器

| Renderer | 输入 | 输出 | 失败降级 |
|---|---|---|---|
| golden_overview | Golden SignalBinding | 四大信号卡片 | 单项 Unknown，不隐藏其它项 |
| request_journey | focus + journey + stage observations | 有序阶段与断点 | 无 focus 时显示输入和示例，不虚构路径 |
| dependency_topology | Entity/Relation + RED | 动态拓扑 | observed 边 stale 时变灰，保留 declared 边 |
| resource_fleet | Resource Entity + USE | top-k、分布、单机下钻 | 数据源失败时仅保留已知资源清单 |
| evidence_list | EvidenceRef | 摘要、来源、新鲜度、深链 | 原文过期时保留摘要与完整性信息 |
| domain_timeline | Alert/Fault/Impact/Incident/Change | 统一时间线 | 任一来源缺失标 Partial |

Renderer 只消费标准数据，不知道 E2B、CCH 等业务名称。

## 7. Diagnostic Context

DiagnosticContext 是页面、确定性诊断规则和未来 Agent 共用的只读上下文。JSON Schema 见 [DiagnosticContext](../contracts/service-observability/v1/diagnostic-context.schema.json)。

它包含：

- actor、service、environment、tenant、time range；
- 当前 focus 与 journey stage；
- topology snapshot 和 freshness；
- Golden/RED/USE 的规范化信号；
- EvidenceRef；
- Alert/Fault/Impact/Incident 的只读引用；
- 当前允许的 tool keys 与查询限额。

它不包含：

- 浏览器 DOM；
- 完整原始日志或 Trace；
- 数据源凭据；
- 模型 prompt、对话历史或长期记忆；
- 任何可执行动作；
- 客户端自行声明的 actor、roles 或权限。

服务端按登录身份和 profile 生成 Context。页面只提交 service、focus 和 time range。

## 8. EvidenceRef

EvidenceRef 是对 OpenObserve、Prometheus-compatible 指标、Alertmanager 投影、拓扑快照或人工裁决的有限引用。JSON Schema 见 [EvidenceRef](../contracts/service-observability/v1/evidence-ref.schema.json)。

每个引用至少包含：

- evidence ID、source、kind；
- source-side locator 或 server-generated deep link；
- correlation keys；
- observed/fetched time 和 freshness；
- 脱敏摘要；
- 完整性 hash 和 sensitivity。

原始遥测仍留在源系统。查询不到、权限不足和已超过保留期是三种不同状态，不能统一解释成“没有异常”。

## 9. Typed Tool Registry

ToolDescriptor JSON Schema 见 [ToolDescriptor](../contracts/service-observability/v1/tool-descriptor.schema.json)。

首版工具全部只读：

| Tool 类别 | 示例 | 主要消费者 |
|---|---|---|
| context | context.resolve_focus.v1 | 页面、确定性规则 |
| topology | topology.expand_neighbors.v1 | 拓扑 renderer |
| metrics | metrics.query_semantic_signal.v1 | Golden/RED/USE renderer |
| logs | evidence.search_logs.v1 | 证据页 |
| traces | evidence.search_traces.v1 | 路径与证据页 |
| domain | domain.list_service_faults.v1 | Fault 与时间线 |

### 9.1 Descriptor 必须包含

- 稳定 key 与语义版本；
- source adapter；
- JSON Schema 输入输出；
- read_only: true；
- timeout、最大时间窗、行数和基数；
- freshness TTL；
- required permissions；
- server-owned query template ref；
- 审计与脱敏策略。

### 9.2 调用路径

~~~mermaid
sequenceDiagram
    actor SRE
    participant UI as Service Workspace
    participant API as alert-system API
    participant REG as Tool Registry
    participant AD as Source Adapter
    participant SRC as Prometheus / OpenObserve / Domain Store

    SRE->>UI: 选择 service、focus、time range
    UI->>API: 请求 DiagnosticContext 或执行只读 tool
    API->>REG: 校验 service profile、actor、tool 和输入 schema
    REG->>AD: 执行 server-owned typed query
    AD->>SRC: 使用服务端身份读取有限数据
    SRC-->>AD: 原始结果
    AD-->>REG: 规范化 signal / EvidenceRef
    REG-->>API: 脱敏、限量、带 freshness 的结果
    API-->>UI: 标准数据
~~~

Tool Registry 不接受 profile 或浏览器提供的查询语句。无结果、超时、超限和权限拒绝必须返回不同的结构化状态。

## 10. Agent 扩展端口：只预留，不实现

本阶段只承诺未来任何 Assistant Provider 的输入边界：

~~~ts
interface AssistantExtensionInputV1 {
  context: DiagnosticContextV1;
  allowed_tools: ToolDescriptorV1[];
}
~~~

当前明确不决定：

- 使用哪种模型或 Agent 框架；
- 同步、SSE、WebSocket 或任务队列协议；
- 会话、长期记忆和运行记录模型；
- prompt 结构；
- 多 Agent 编排；
- Agent 输出事件；
- Agent 动作预览或写入权限。

UI 预留一个 extension slot，但默认关闭且不渲染。未来启用条件至少包括：

1. E2B 与第二服务已证明 DiagnosticContext/Tools 足以支撑人工诊断；
2. provider 方案和数据使用政策已确认；
3. Prompt Injection、RBAC、审计、限流、取消和降级方案完成单独评审；
4. Agent 失败不影响 Service Workspace；
5. 任何写入能力经过独立 ADR。

## 11. E2B Profile

E2B 首个案例使用以下映射：

| 契约项 | E2B 值 |
|---|---|
| Service | E2B 平台服务登记；必须绑定正确的 platform canonical entity |
| Primary focus | sandbox_id |
| Secondary focus | trace_id、fault_id、entity、relation |
| Default journey | sandbox_create |
| Journey stages | ACCEPT、PLACE、FC_START、ENVD_READY、ROUTED |
| Golden | create p95、creates/min、create error rate、最坏资源 saturation |
| RED | 服务节点和实际请求边的 rate/errors/duration |
| USE | Orchestrator host HugePages/CPU/NVMe/NBD utilization、placement queue/wait saturation、ResourceExhausted errors |
| Evidence | OpenObserve logs/traces、Prometheus-compatible metrics、Alert、拓扑快照、人工裁决 |
| Domain | candidate/active Fault、potential/confirmed/ruled_out Impact、Incident 引用 |

### 11.1 sbx_hp16c3 确定性验收

- focus 到 PLACE 阶段和 API → Orchestrator 异常边；
- 显示边 RED：18 req/s、7.2% errors、p95 3.48s；
- 允许打开 Orchestrator 舰队并定位 orch-16 HugePages 91%；
- 调度拒绝日志是直接证据，HugePages 同期饱和是根因候选；
- 形成 capability candidate Fault，但没有直接业务影响时不建议宣布 Incident；
- OpenObserve 不可用时仍显示 Prometheus 信号、declared topology 和已有领域状态，依赖 logs/traces 的 observed path 与证据部分为 Partial/Unknown；Prometheus-compatible adapter 不可用时仅 Golden/RED/USE 变 Unknown；
- 整条路径不需要 Agent 才能完成。

### 11.2 对当前视觉稿的调整

- 页面外壳使用 Firebreak 现有导航，不复制独立 E2B 应用导航。
- 左侧重复的“当前 SID 阶段”只保留工作区内一份。
- AI 诊断改为诊断摘要。
- 顶部搜索改为通用 focus 输入，placeholder 由 profile 提供。
- “执行只读诊断”调用 Tool Registry，不调用模型。
- “预览 candidate Fault 草稿”不在首版实现；需要时跳转现有 Fault 工作区人工处理。
- Agent 搜索、Agent 置信度和 Agent 工具调用次数在 provider 未启用时不展示。

## 12. 接入第二个服务的标准流程

1. 在 Service Catalog 登记 service、owner、tier、canonical root entity、Alias 和 declared dependencies。
2. 发布 Alert Mapping Policy，保证 Alert → Entity → Fault 映射确定且可解释。
3. 声明 ServiceObservabilityProfile，选择 focus、journey、renderer 和 signal slots。
4. 只引用已审核的 typed tools；若缺工具，先扩展通用 adapter/tool，不向 profile 写查询。
5. 准备正常、异常、未知和 stale 四类 fixture。
6. 验证 Golden/RED/USE 语义、证据深链、Fault/Impact 和权限隔离。
7. 若需要新增 renderer，必须证明至少两个服务可复用，否则留在外部 Dashboard 深链中。

## 13. 安全边界

- service、tenant、actor、team 和 roles 由服务端从会话与目录解析，不能信任 URL 或 profile 自报。
- focus value、日志、Trace attributes、Alert annotations 和 profile 显示文本均是不可信数据。
- 工具输入经过 JSON Schema 校验，未知字段、过长字符串、过大时间窗和过高基数 fail closed。
- 浏览器只看到 server-generated EvidenceRef 和 deep link，不获得 OpenObserve 凭据。
- Profile 发布是管理动作，需要版本、操作者和审计；运行态只读取已发布版本。
- 原始日志默认不进入 DiagnosticContext；受限摘要必须经过脱敏。
- 本阶段无 Agent 写工具、Shell、SSH、自由 SQL 或 Runbook 执行。

## 14. 失败模式与降级

| 失败 | 页面行为 | 禁止行为 |
|---|---|---|
| Profile 缺失 | 显示服务总览和配置缺口 | 不猜测 renderer 和指标 |
| Profile 版本不兼容 | 明确配置错误并保留基础服务信息 | 不静默使用旧字段 |
| Tool 未注册或未授权 | 对应区块 Partial/Forbidden | 不换用相似工具绕过 |
| OpenObserve 不可用 | 保留 Prometheus 信号、declared topology/domain；observed path 与日志/Trace 证据 Partial | 不把无结果当健康 |
| Prometheus-compatible adapter 不可用 | Golden/RED/USE 为 Unknown；保留 OpenObserve 路径/证据与 domain | 不清空整页或复用过期绿值 |
| observed relation stale | 关系变灰并显示时间 | 不据此自动确认 Impact |
| 指标缺失 | 单项 Unknown | 不以 0 或绿色代替 |
| 高基数/超限 | 显示超限并要求缩小范围 | 不静默截断后给确定结论 |
| Agent 未配置 | 完整确定性 UI，无 Agent tab | 不展示假 AI 响应 |

## 15. 提议的非功能目标

| 类别 | Phase 1 目标 |
|---|---|
| 首屏 | 服务身份、owner、domain 摘要 p95 ≤ 1s |
| 标准工作区 | 默认 5 分钟窗口首批信号与拓扑 p95 ≤ 2s |
| 慢源 | 单独加载，10s 超时后 Partial，不阻塞其它区块 |
| 取消 | focus/time range 改变后停止发起旧范围的新查询 |
| 限界 | 默认 ≤100 nodes、≤200 relations、每 tool ≤200 rows |
| 新鲜度 | 100% signals/evidence/observed relations 带 observed_at/freshness |
| 审计 | 100% Tool 调用带 request_id、actor、service_id、profile version |
| 隔离 | 单一 profile/tool 失败不影响 /services 目录及 Fault/Incident |

这些是提议目标，不代表当前系统已经达到。

## 16. 分阶段交付

### Phase 0：契约与 fixture

- 固定 Profile、DiagnosticContext、EvidenceRef、ToolDescriptor v1 draft。
- 以 E2B fixture 校验 renderer 和 Unknown/Partial 语义。
- 不改数据库，不调用真实 OpenObserve，不实现 Agent。

### Phase 1：/services 通用工作区

- 新增 /services/:service_id 和 /observability 路由。
- 用 mock/fixture 实现通用 renderer。
- 右栏只提供确定性诊断摘要、证据和现有领域深链。

### Phase 2：只读数据接入

- 接入 topology、OpenObserve 与 domain typed tools。
- 服务端生成 DiagnosticContext，支持权限、限流、取消、Partial 和审计。
- E2B 黄金场景通过真实只读数据验收。

### Phase 3：泛化验证

- 接入第二个非 E2B 服务，不新增核心业务分支。
- 根据验证结果调整 renderer/profile，并将稳定 schema 标记为 1.0。
- 决定 Service Registration 对非 application entity 的兼容 migration。

### Deferred：Agent

- provider、运行协议、模型治理和写动作均延后。
- 只有基础上下文与 tools 验证稳定后，才提交独立 Agent ADR。

## 17. 待确认问题

1. E2B 在 canonical entity 中应绑定哪个 platform root entity，以及 ownership 团队。
2. /services/:service_id 是否直接用 UUID，还是新增不可变的路由 slug。
3. Profile 首版存储在版本化文件、数据库，还是先文件后数据库。
4. 第二个泛化验收服务选择 CCH 还是其它服务。
5. OpenObserve Adapter 的服务端身份、字段白名单与 retention 查询能力。
