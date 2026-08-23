# ADR-0007：以声明式 Service Integration Package 接入服务，并治理自动发现拓扑

## Status

Proposed — 2026-08-23

## Context

ADR-0006 已确定 `/services` 是通用服务运行入口，服务差异由
`ServiceObservabilityProfile`、canonical topology 与只读 `ToolDescriptor` 表达，禁止
服务上传前端代码或把查询、凭据、Prompt、脚本放进 Profile。

Phase 1/2 已证明三层 Page View、统一 Health/Data evaluator、file-based Profile Registry、
alert-system registration/topology/domain adapter 和 snapshot cache 可以在没有 Agent、没有生产
遥测的情况下独立工作。Phase 3 需要接入 Prometheus-compatible 与 OpenObserve 的真实只读
数据，同时回答三个长期问题：

1. E2B 的逻辑组件相对稳定，但 Pod、宿主和实例数量持续变化；新业务的组件与依赖结构也会
   变化，topology 不能靠每次手工改前端或静态 fixture 维护；
2. 新服务应以插件式交付接入，但任意 Go/JS/SQL/PromQL 插件会破坏统一升级、安全审查和
   provider-neutral 契约；
3. 未来 Agent 不能只等同于页面聊天框，而应能作为独立服务消费同一份受限事实和工具；
   但当前仍明确不实现 Agent。

现有 canonical graph 已有 `resource_nodes`、`dependency_edges` 与 `entity_aliases`。Alias 允许
零匹配和冲突被显式表示，适合对 observed identity 采取 fail-closed 解析。当前 v1 Profile 的
`service_ref` 使用部署环境中的真实 service/entity ID；跨环境可移植性必须在发布前编译解决，
不能在运行时偷偷把 ID 当成 entity key。

## Decision

### 1. Topology 分成三个事实层，不互相冒充

| 层 | 事实含义 | 生命周期 | 是否可自动写 canonical graph |
|---|---|---|---|
| Declared canonical skeleton | owner/platform 明确声明的逻辑组件、服务边界与依赖意图 | 显式版本化发布，直到被替换 | 是，但只能经现有受控 registration/topology 流程 |
| Discovered inventory | 采集源在当前环境看到的实例、宿主、池、数据库等清单 | snapshot + observed_at + TTL | 否；先解析 alias 与生成 reconciliation 结果 |
| Observed request overlay | traces/access 在指定时间窗实际看到的 caller → callee 路径 | 短 TTL，过期后 Stale/Unknown | 否；只覆盖已唯一解析的 canonical 对象 |

页面仍投影一个统一 graph，但必须保留 provenance：`declared`、`observed`、
`declared_and_observed`。未观察到一条 declared relation 不等于关系不存在；它降权显示而不删除。

E2B Profile 声明 API、Orchestrator、Envd、Traefik、资源池等稳定**逻辑节点**。Pod、host、
Firecracker 或其它实例数量变化进入 discovered inventory/resource fleet，页面展示实例数量、最坏
资源和异常成员，不把每次副本变化误解成组件拓扑变更。只有出现新的逻辑组件或依赖意图时，
才需要发布新的 declared skeleton/Profile 版本。

### 2. 自动发现必须经过确定性的身份解析与治理

发现流程固定为：

1. provider adapter 只返回有界的 external identity、关系方向、观测时间和来源证据；
2. external identity 先按 package 声明的 alias namespace 白名单归一化；
3. Alias 唯一匹配才允许形成 observed overlay；
4. 零匹配记为 `alias_not_found`，多匹配记为 `alias_conflict`；两者都只在最小对象/source
   降级为 Unknown，并生成不含原始敏感值的 reconciliation 摘要；
5. discovery 不自动创建 `resource_nodes`，不自动覆盖 declared dependency，也不自动确认 Impact；
6. owner/platform 审核后，才通过现有受控领域流程更新 canonical graph 或 Integration Package。

Phase 3 只实现只读解析、overlay 与 reconciliation 结果；不新增自动写 topology 的后台控制器。

### 3. Service Integration Package 是声明式接入单元

引入版本化的 **Service Integration Package（SIP）**。它是静态、可校验、可审查的文档包，
不是动态执行插件。推荐逻辑结构：

```text
service-integrations/<package-key>/
  manifest.json                 # 版本、兼容范围、服务选择器、依赖能力
  profile.template.json         # 编译为现有 v1 Profile
  telemetry-bindings.json       # 只含类型化 template ref 与参数
  topology-policy.json          # roots、alias namespaces、discovery/overlay 规则
  testdata/                     # discovery 输入与期望规范化输出
```

Package 可以声明：

- 目标 service 的稳定选择器与环境约束；
- Profile 的 focus、journey、renderer、signal binding、limits 与 tool refs；
- provider-neutral 的 query template ref、严格类型参数和输出字段映射；
- discovery identity 字段到受控 alias namespace 的映射；
- fixtures、兼容版本和发布元数据。

Package 禁止包含：

- Go/JS/WASM、Shell、HTML、Prompt 或其它可执行代码；
- 原始 SQL、PromQL、LogQL；
- endpoint、Authorization header、token、密码或其它凭据；
- 任意浏览器 renderer 或可写 tool；
- 未经 allowlist 的字段名、URL 或 adapter key。

原始查询模板仍属于 alert-system 内部的**受信 provider template registry**。SIP 只能选择模板
并传递 schema 允许的参数，例如已发现并固定的 metric name、label 等值对、stream key、
focus field 和 stage mapping。label 名必须来自 identifier allowlist，值只能是被严格编码的
字面值；SIP 禁止 regex、否定、集合选择器和任何自由 matcher 表达式。PromQL/SQL 由 adapter
从类型化模板构造，服务方不能提交自由文本。

### 4. SIP 发布前编译为现有 v1 Profile，不改变 ID 语义

Package compiler 在发布前完成：

1. 校验 package schema、兼容版本、引用完整性和禁止字段；
2. 解析目标 Service Registration 与 root selector；
3. 要求每个 selector 在目标环境唯一命中；零匹配或冲突 fail closed；
4. 生成使用真实 `service_registrations.id` / `resource_nodes.id` 的 v1 Profile；
5. 让生成物继续经过现有 Profile schema、Tool catalog 和 resolver 校验；
6. 输出不可变的编译清单、内容摘要和回滚所需上一版本引用。

运行时仍只加载已编译的 v1 Profile。SIP 不绕过现有 Registry，也不让 v1 字段产生
entity-key fallback。Phase 3 先提供离线/启动前编译与验证能力，不修改 Kubernetes 发布配置。

### 5. 遥测 adapter 由平台受信实现，SIP 只配置能力

Prometheus-compatible、OpenObserve、alert-system domain 与 topology adapter 是平台级受信
组件。Phase 3 使用一个 provider-neutral `ObservationCoordinator` 并行调用独立来源：

- Prometheus-compatible：Golden/RED/USE 和 metric Evidence；
- OpenObserve：focus、journey observation、logs/traces/access Evidence 与 observed topology hints；
- Alias resolver：把 observed hints 唯一映射到 canonical entity/relation；
- Evidence broker：生成 alert-system 内部 deep link，并在打开时重新鉴权。

每个来源拥有独立 deadline、cardinality/row/response-size 上限、取消和 source status。一个来源
失败只把它负责的 binding、journey stage、overlay 或 EvidenceRef 降级为 Unknown/Unavailable；
不得清空 declared topology、其它来源或 alert-system domain facts。

### 6. 运行时 discovery 先产出清单，再发布生产绑定

Prometheus metric name/labels 与 OpenObserve stream schema/retention 必须通过只读 discovery
确认。Discovery 输出只包含名称、类型、时间边界、基数摘要和内容 hash，不复制样本、日志正文
或凭据。生产 E2B Package 只有在以下条件满足后才能从 draft 进入 published：

- 所有 required signal binding 的 metric/label 唯一匹配且单位可验证；
- `sandbox_id` 字段在 logs/traces/access 的逐条记录上存在，拒绝以 `sandboxid` 代替；
- trace service/operation identity 可唯一映射到 canonical aliases；
- retention、freshness TTL、HugePages 公式、placement rejection 语义和阈值均有审计记录；
- 未确认的 USE Saturation 继续为 Unknown，不能复用 utilization。

### 7. Agent 未来按 Agent-as-a-Service 设计，但本阶段零实现

未来 Agent 是可替换的外部/独立服务消费者，不以聊天 UI 为中心。预期边界是异步 run：

```text
submit(context_snapshot_ref, allowed_tool_descriptors, objective, budget)
  -> agent_run_id
get(agent_run_id)
  -> status + evidence-linked advisory artifact
cancel(agent_run_id)
```

Agent 只能拿到版本化 `DiagnosticContext`、当前身份允许的只读 `ToolDescriptor` 和短期 delegation；
输出必须引用 EvidenceRef、列出缺失事实并标注是否只是建议。模型、prompt、memory、run store、
event stream、成本策略、租户隔离、写动作和审批协议必须另开 ADR。

Phase 3/4 不新增 Agent API、表、worker、provider、UI slot、聊天框或动作工具。Agent 服务关闭、
超时或不存在时，`/services`、Evidence、Fault 与 Incident 必须完全不受影响。

### 8. Kubernetes/OTel 配置继续冻结

本 ADR 只改变 alert-system 内的契约、只读 adapters、配置模型、测试和文档。以下内容不在
Phase 3/4 修改范围：

- `deploy/observability/`；
- UAT OTel Collector、RED writer、Vector、Prometheus scrape/remote-write 配置；
- 任何 Kubernetes manifest、ConfigMap、Deployment、Service 或 Helm values。

现有 UAT 采集拓扑只作为只读 discovery 和 smoke 的外部前提；缺少数据时按最小范围 Unknown，
不得为让测试变绿而调整采集配置。

## Consequences

### Positive

- E2B 的副本变化与逻辑架构变化被分开处理，拓扑不会因实例抖动反复重写。
- 新服务可复用 SIP、有限 renderer 和受信 adapters 接入，不需要核心 `if service == ...`。
- 自动发现能够补充 observed path，同时 Alias 冲突不会污染 canonical graph。
- v1 Profile 的真实 ID 语义保持不变，现有 Registry/Page View 无兼容破坏。
- Agent 将来可作为独立服务扩缩容和治理，不绑死在页面聊天或某个模型框架。

### Negative

- 平台需要维护 package schema、compiler、provider templates、alias 治理和 discovery 清单。
- 新 metric/stream 不能由服务方随意粘贴查询立即上线，必须先经过 template 与安全审查。
- 未治理 identity 会以 Unknown/Partial 暴露，短期可能比自动猜测显得“不完整”。
- 编译产物与 package 版本需要明确发布/回滚流程。

### Neutral

- 自动发现不是“自动相信”。它自动采集事实，但 canonical 写入仍是治理动作。
- OpenObserve/Prometheus 可替换；SIP 依赖的是 adapter capability 与 template ref，不是浏览器产品名。
- 未来可以演进到受签名的进程外 provider，但不在证据不足时提前引入动态代码加载。

## Failure Modes And Mitigations

| Failure | Required behavior | Mitigation |
|---|---|---|
| discovery 返回未知 identity | 该 hint Unknown，不建实体 | alias allowlist + reconciliation report |
| alias 一对多 | 冲突对象隔离，其它对象保留 | fail closed；不按名称猜测 |
| provider 超时/取消 | 对应 source/binding Partial/Unknown | per-source context deadline + cancellation |
| 查询返回过多 series/rows | 显式 scope/cardinality error | typed limits 的最小值 + 多取一项检测截断 |
| SIP 引用不存在模板 | package/profile 不发布 | compiler + registry 双重校验 |
| 上游凭据失效 | source Unavailable，不伪装为用户 Forbidden | 区分平台配置错误与当前用户权限 |
| 用户缺 `logs.read` | 仅 logs Evidence Forbidden；不调用上游 | server-side permission resolver |
| Evidence deep link 过期 | 显示 Expired/Unavailable | bounded broker TTL + 打开时重新鉴权 |
| Agent 不可用 | 不影响任何基础页面 | Agent 端口不在 Phase 3/4 运行路径 |

## Alternatives Considered

### 由 traces/discovery 自动创建和更新 canonical graph

拒绝。短期观测、采样缺口和身份冲突会污染影响半径与领域事实；observed overlay 必须与 owner
声明的结构意图区分。

### 每个服务提交一个可执行插件

拒绝。动态 Go/JS/WASM 或自由查询会扩大供应链、凭据、SSRF、数据外泄和升级风险，也会让
renderer/assembler 重新出现服务特例。

### 把所有 provider 查询直接写进 Profile

拒绝。Profile 会失去 provider-neutral 与可安全下发到 Page View 的边界；查询模板继续只存在于
受信 server registry。

### 只支持 E2B 的固定 adapter

拒绝。它能更快展示首个 demo，但无法通过第二服务验收，并会把 E2B binding 变成核心分支。

### Phase 3 同时实现 Agent chat/run

拒绝。生产遥测、权限、证据和 topology identity 尚未完成前，Agent 只会放大不稳定边界。当前
只记录 Agent-as-a-Service 方向，另开 ADR 决策运行时。

## References

- [ADR-0006：Service Observability Workspace 与可选 Agent 端口](0006-service-observability-workspaces-and-optional-agent-port.md)
- [Service Observability 完整设计交接稿](../plans/2026-08-22-service-observability-complete-design-handoff.md)
- [Service Observability contracts v1](../contracts/service-observability/v1/README.md)
- [Prometheus HTTP API](https://prometheus.io/docs/prometheus/latest/querying/api/)
- [OpenObserve Search API](https://openobserve.ai/docs/reference/api/search/search/)
- [OpenObserve Trace Search API](https://openobserve.ai/docs/reference/api/traces/trace-search-api/)
- [OpenObserve Stream API](https://openobserve.ai/docs/reference/api/stream/)
