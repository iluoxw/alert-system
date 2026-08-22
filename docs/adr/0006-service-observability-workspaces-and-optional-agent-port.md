# ADR-0006：以 `/services` 承载通用服务可观测性工作区，Agent 作为可选消费者

## Status

Accepted — 2026-08-22

## Context

alert-system 已有 canonical Entity/Relation、Service Registration、Alert Mapping、Fault、Impact、Incident 以及 `/services` 服务目录。当前 `/services` 只展示应用责任卡片，并跳转到基础设施实体详情；它还不是值班人员面向单个服务的完整运行工作区。

E2B 动态拓扑原型验证了一个更完整的服务视角：

- 顶部用 Google 四大黄金信号回答用户是否受影响；
- 服务节点和调用边用 RED 定位异常服务或异常跳；
- 宿主、池、队列和存储用 USE 验证资源约束；
- 以 `sandbox_id` 聚焦一次用户请求，关联 OpenObserve 证据和 alert-system 的 Fault/Impact；
- 从服务诊断自然进入 Fault 审查和 Incident 响应，但不复制这些领域状态机。

该能力不能只服务 E2B。后续每个服务都应以相同方式进入 alert-system，而不是继续增加业务专属页面、关联器和核心代码分支。ADR-0004 已拒绝把每个业务做成携带私有领域逻辑的 System Pack/Connector，因此新的扩展边界必须以声明式契约和有限通用渲染器为主。

AI Agent 的模型、运行框架、部署方式、会话协议和安全策略尚未选定。当前可以确定的只有：未来 Agent 必须消费同一份服务上下文、证据引用和类型化 tools，且不能成为这些基础能力存在的前提。

## Decision

1. **`/services` 是通用服务运行入口。** 路由规划为：
   - `/services`：服务目录；
   - `/services/:service_id`：服务总览；
   - `/services/:service_id/observability`：动态拓扑与请求/实体诊断工作区。
2. **E2B 是第一个 Service Observability Profile，不是核心特例。** E2B 的 `sandbox_id`、Sandbox Create 阶段、组件拓扑和 OpenObserve 查询模板通过 profile 与 tool 引用表达；核心 UI 不出现 `if service == e2b` 的业务分支。
3. **服务目录从“应用服务列表”演进为“运维服务目录”。** `service_registrations` 继续提供稳定 ID、ownership、tier、链接和 enabled 状态，但后续应允许登记绑定不同 layer 的既有 canonical entity。不能为了让 E2B 出现在 `/services`，把平台能力错误标记成 `application/business_system`。现有应用服务数据保持兼容。
4. **每个服务使用版本化的 `ServiceObservabilityProfile`。** Profile 只声明：
   - 服务根实体与默认拓扑范围；
   - 可用 focus key 和用户旅程阶段；
   - 黄金信号、RED、USE 的语义槽位；
   - 有限通用 renderer；
   - 可调用的只读 tool key；
   - freshness、容量和查询边界。
5. **Profile 不包含任意查询或可执行代码。** SQL、LogQL、Shell、URL 凭据和模型 prompt 均不得进入 profile。Profile 只能引用服务端已注册、版本化、类型化的 ToolDescriptor；查询模板由 adapter 管理。
6. **UI 使用有限通用渲染器。** 首批 renderer 为 `golden_overview`、`request_journey`、`dependency_topology`、`resource_fleet`、`evidence_list` 和 `domain_timeline`。服务可以选择和配置这些 renderer，但不能上传前端代码。
7. **基础诊断在没有 Agent 时必须完整可用。** Service Workspace 直接展示权威数据、确定性健康规则、证据和 Fault/Impact 状态。E2B 首版右栏使用“诊断摘要”而不是伪造的“AI 诊断”；Agent 未启用时不显示聊天输入、置信度幻觉或自动动作。
8. **当前只定义 Agent 扩展端口，不实现 Agent。** 稳定输入仅包含 `DiagnosticContext` 和当前身份允许的只读 `ToolDescriptor`。不在本阶段实现模型适配器、prompt、会话、长期记忆、Agent Run/SSE 协议、Agent 数据表或 Agent 动作网关。
9. **Tools 先服务人和确定性 UI，再服务未来 Agent。** Tool Registry 必须可由 Service Workspace 的按钮和证据页直接调用；未来 Agent 只能复用同一工具契约，不能获得浏览器和人类用户没有的隐式数据通道。
10. **领域写入保持现状。** Fault Evidence、Impact、Incident 和 Runbook 继续使用 alert-system 已有 API、人工门槛、RBAC 与审计。本 ADR 不授予 Agent 任何写权限；若未来需要 Agent 建议或动作预览，必须单独 ADR。
11. **`e2b-observe` 保留为原型与视觉验收场。** 它不是生产部署单元，也不是契约权威。生产设计、schema 和后续实现以 alert-system 仓库为准。

## Consequences

### Positive

- alert-system 从告警/Fault 页面集合演进为按服务组织的统一值班入口。
- E2B、CCH 和后续服务共享同一领域模型、交互骨架和工具安全边界。
- 服务差异由 profile、canonical topology 和 typed tools 表达，避免核心代码线性增长。
- 无 Agent、模型不可用或未来更换 Agent 框架时，基础诊断能力不受影响。
- 未来 Agent 获得的是干净、受限、可审计的基础上下文，而不是浏览器 DOM 或任意查询权限。

### Negative

- 现有 `service_registrations` 与 UI 文案偏向 application，需要兼容性演进才能正确表示 E2B 等平台服务。
- 通用 renderer 会限制服务完全自定义布局；新增表达能力需要先证明可复用性。
- Profile 与 ToolDescriptor 的版本、兼容性和发布治理会增加平台维护工作。
- 服务负责人需要显式维护 focus、journey、signals、freshness 和工具引用，不能只依赖遥测自动猜测。

### Neutral

- E2B 可以保留最丰富的首个 profile，但不能借此向核心领域加入 E2B 专属状态。
- OpenObserve 继续保存原始 metrics/logs/traces；alert-system 只保留规范化值、摘要和 EvidenceRef。
- `/infrastructure/:id` 仍是实体级详情；`/services/:service_id/observability` 是服务级聚合视角，两者互相深链而不互相替代。

## Non-Functional Requirements

- Agent 或任一外部证据源不可用时，`/services`、Fault 和 Incident 页面仍可用。
- 每个指标、证据和 observed relation 必须显示 `observed_at` 与 freshness；缺失或超时状态为 `unknown`，禁止假绿。
- 单次工作区默认最多加载 profile 声明的实体、关系、时间窗、行数和基数；扩大范围必须由用户可见操作触发。
- Profile 和 ToolDescriptor 采用显式 schema/version；不兼容版本必须 fail closed，不能静默降级成其它语义。
- Tool 调用必须经过当前用户与团队可见性过滤、输入 schema 校验、限流、超时、脱敏和审计。
- Service Workspace 不长期复制 OpenObserve 原始日志或 Trace。

## Alternatives Considered

### 把 E2B 页面放进 e2b-dashboard

拒绝。e2b-dashboard 面向 Sandbox/Template 管理，使用 Team API Key，缺少 alert-system 的用户、RBAC、Fault/Impact/Incident 和人工审计边界。它只提供带 focus 的深链入口。

### 为 E2B 单独部署 e2b-observe 服务

拒绝。会形成重复登录、导航、拓扑、Fault 投影和运维入口；后续每个服务再复制一套会使 alert-system 失去统一控制面的价值。

### 每个服务实现一个前端插件或 System Pack

拒绝。任意前端代码和业务专属核心逻辑会破坏统一升级、安全审查和可解释性。只允许声明式 profile、有限 renderer 和类型化工具引用。

### 现在就确定并实现 Agent 平台

拒绝。模型和运行框架尚未选择，提前固化 Agent Run、流式事件、存储和动作协议会让基础可观测性依赖未知方案。先验证 Service Workspace 与 tools。

### 完全不预留 Agent 边界

拒绝。若 profile、context 和 tool 输出只为当前页面拼装，未来会再次重构数据与权限边界。当前只稳定 provider-neutral 输入端口，不实现运行时。

## Compatibility And Migration

1. 先新增 Service Observability 契约和 E2B fixture，不修改现有告警管道或数据库。
2. 将 `/services` 卡片的主要入口逐步改为 `/services/:service_id`；保留 `/infrastructure/:id` 深链。
3. 用 E2B profile 验证 `sandbox_id → journey → topology → RED/USE → evidence → Fault`，全部使用 mock/fixture 和只读 tools。
4. E2B 验收后，再决定 `service_registrations` 支持非 application entity 的 migration 与管理 UI。
5. 至少用第二个非 E2B 服务验证 profile 无需核心分支后，才把契约标记为 Stable。
6. Agent 方案确定前，不新增 Agent API、表、worker 或可见 AI 交互。

## References

- [ADR-0003：Alertmanager 权威与分层实体模型](0003-alertmanager-authoritative-layered-entity-model.md)
- [ADR-0004：显式服务目录与确定性 Alert Mapping](0004-explicit-service-catalog-and-deterministic-alert-mapping.md)
- [ADR-0005：统一实体关系与保守影响半径](0005-unified-entity-relations-and-conservative-blast-radius.md)
- [E2B 黄金场景设计](../plans/2026-08-02-e2b-golden-scenario-design.md)
- [Service Workspace 详细设计](../plans/2026-08-22-service-observability-workspace-design.md)
