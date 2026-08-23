# Service Observability Phase 3：生产只读遥测与声明式接入实施计划

> 状态：Fable 5 High 已评审；REVISE 的四个 P1 已按最小范围关闭，可进入 Batch A
> 日期：2026-08-23
> 生产归属：alert-system / Firebreak
> 首个 Service Profile：E2B
> 基线：`91f7b1ca83576ae6a8d94bd2602bf06bc9b42f2e`
> 目标分支：`codex/service-observability-phase3-20260823`

## 1. 目标

在不改变三层 `/services` Page View、Health/Data 正交语义和既有 Alert/Fault/Impact/Incident
权威边界的前提下，把 Phase 2 的 `UnavailableObservationReader` 替换为真实的、可独立降级的
生产只读遥测路径：

1. Prometheus-compatible adapter 提供 Golden/RED/USE 规范化信号；
2. OpenObserve adapter 按逐条 `sandbox_id` 提供 focus、journey observation、logs/traces/access
   EvidenceRef 与 observed topology hints；
3. Alias resolver 只把唯一命中的 observed identity 合并到 canonical graph；
4. Tool runtime 统一执行权限、limits、deadline、取消、脱敏与审计；
5. Evidence broker 只返回 alert-system 内部的受控 deep link，并在打开时重新鉴权；
6. E2B 通过声明式 Service Integration Package 编译、验证和发布，不在核心加入 E2B 分支；
7. 用 UAT 现有只读数据完成 metric/label/stream/retention discovery 与 `sandbox_id` replay；
8. 任一来源缺失、失败、Forbidden 或过期，只在最小 binding/stage/relation/evidence 降级为
   Unknown/Unavailable，declared topology 与 alert-system domain 继续可用。

本 Phase 完成后，生产 Page View 仍由同一 `DiagnosticContext -> evaluator -> projector` 路径
产生。浏览器不获得 endpoint、凭据、SQL、PromQL、内部 ToolDescriptor template 或原始证据。

## 2. 权威设计与补充裁决

实施必须同时满足：

- [完整设计交接稿](2026-08-22-service-observability-complete-design-handoff.md)；
- [contracts v1](../contracts/service-observability/v1/README.md)；
- [视觉稿来源](../assets/service-observability/README.md)；
- [ADR-0006](../adr/0006-service-observability-workspaces-and-optional-agent-port.md)；
- [ADR-0007](../adr/0007-declarative-service-integrations-and-governed-observed-topology.md)；
- 当前仓库实际代码和测试；
- 仓库 AGENTS 规则以及本计划记录的用户补充约束。

用户补充裁决：

- 生产主落地始终是 alert-system；
- E2B 是首个、一等 Service Profile，不是核心特例；
- fixtures 优先用于确定性测试，但 production build/runtime 不得读取 fixture；
- Agent 暂缓实现；只记录 Agent-as-a-Service 边界；
- Kubernetes/OTel 相关配置继续 hold；
- topology 使用 declared skeleton + discovered inventory + observed overlay；
- 服务接入采用声明式 package，禁止动态业务代码插件；
- 缺失或过期数据按最小范围降级为 Unknown。

## 3. Phase 3 明确不做

- 不修改 `deploy/observability/`、Kubernetes manifest、Helm、OTel Collector、Vector、
  Prometheus scrape/remote-write 或 RED writer 配置；
- 不把本地未跟踪的 UAT ADR/Plan 加入、移动或改写到本分支；
- 不向 UAT/生产注入故障，不执行写查询，不改变 retention；
- 不实现 Agent API、Agent run、模型、Prompt、memory、SSE、聊天、provider、表或 worker；
- 不实现通用 Dashboard builder、前端代码插件或任意 Tool Execute API；
- 不把原始日志、Trace、Prometheus 样本长期保存到 alert-system；
- 不允许 SIP/Profile 携带 SQL、PromQL、Shell、secret、endpoint、header、Prompt 或 HTML；
- 不因当前采集缺口伪造 metric name、label、threshold、journey 完成或 observed relation；
- 不修改既有 Alert/Fault/Impact/Incident 状态机；
- 不把单个 `sandbox_id` 失败自动升级为 Fault/Incident。

冻结路径每个实现 slice 与最终候选都必须为零 diff：

```text
deploy/observability/
docs/adr/2026-08-13-otel-prometheus-dual-write-demo.md
docs/plans/2026-08-13-e2b-otel-prometheus-remote-write-demo.md
```

## 4. 事实、不确定项和 fail-closed 口径

### 4.1 已确认事实

- Phase 2 已有稳定 `ObservationReader` facade boundary、统一 evaluator、Page View projector、
  Profile/Tool catalogs、bounded topology/domain/cache 与三条只读 API；
- 当前 `ObservationFacts` 只承载 signals/sources/evidence，journey 仍固定 Unknown，尚不能表达
  focus match、stage observation 或 observed topology overlay；
- `entity_aliases` 允许零匹配与多匹配；现有 mapping 已使用 `alias_not_found` / `alias_conflict`
  fail closed；
- Prometheus HTTP API 提供 `/api/v1/query`、`/api/v1/query_range`、`/api/v1/series`、labels
  与 values；`timeout` 可作为服务端预算提示，`limit` 只作为后端声明支持时的可选优化，
  alert-system 自己的 response byte/row/cardinality bounds 才是权威；metadata 时间范围是近似值，
  不能单独证明有样本；
- OpenObserve Search API 使用微秒 start/end、row limit 与 timeout；Trace API 能按时间与 filter
  列出 trace，再按 trace ID 获取 spans；客户端断开必须取消当前 server-side request；
- OpenObserve 搜索可能返回 timeout、cancel、rate-limit、field/stream not found 等稳定错误；
- UAT 已有 Prometheus-compatible RED/node_exporter 数据与 OpenObserve logs/traces/access 设计，
  但实际 metric names、labels、逐条字段、retention 和 owner threshold 仍必须运行时确认。

### 4.2 未确认项不得猜测

- spanmetrics 规范化后的 RED metric 名与 label；
- create attempt/failure、placement rejection 的生产 metric；
- trace/log/access 中 stage、service、operation、host 的真实字段形状；
- OpenObserve 版本/edition 的 service-account 与 per-stream RBAC 能力；
- E2B SLO owner 对 threshold 与 HugePages proxy 的签字；
- 当前环境是否能从 Codex/Cursor runner 访问 UAT endpoint。

未确认时，代码可以支持 discovery 与显式 Unknown，但不能发布假的 E2B production binding。

## 5. 目标架构

```text
API -> facade -> Context Assembler -> ObservationCoordinator
                                  |-> SemanticSignalReader (Prometheus-compatible)
                                  |-> TraceReader / LogReader / AccessReader (OpenObserve)
                                  |-> AliasResolver (alert-system entity_aliases)
                                  |-> PermissionResolver
                                  |-> ToolInvocationAuditor
                                  `-> EvidenceBroker

Service Integration Package -> compiler -> concrete v1 Profile
                            `-> typed provider binding specs
Trusted provider registry   -> query AST/templates -> outbound HTTP clients
```

`ObservationCoordinator` 继续实现现有 `ObservationReader`，因此 `facade`、API 路由和前端 Page
View 不出现 provider 分支。它可以扩展内部 `ObservationFacts`，但不能改变浏览器契约中已有字段
含义。需要新增契约字段时只能做兼容性增加，并同步 schema/fixture/verifier/types/tests。

## 6. 关键设计裁决

### 6.1 多来源不能折叠成一个 telemetry source

新增稳定 source keys：

```text
prometheus_compatible
openobserve_logs
openobserve_traces
openobserve_access
```

`alert_system_registration/topology/domain` 保持不变。来源状态分别进入 `data_status.sources`；
OpenObserve logs Forbidden 不得把 traces 标成 Forbidden，Prometheus timeout 不得抹掉 observed path。

### 6.2 保持一个 Page View 装配入口，内部拆成 source-specific ports

建议新增内部端口：

```go
type SemanticSignalReader interface {
    ReadSignals(context.Context, SignalRequest) (SignalFacts, error)
}

type TraceReader interface {
    ReadTraces(context.Context, TraceRequest) (TraceFacts, error)
}

type LogReader interface {
    ReadLogs(context.Context, LogRequest) (LogFacts, error)
}

type AccessReader interface {
    ReadAccess(context.Context, AccessRequest) (AccessFacts, error)
}

type AliasResolver interface {
    ResolveAliases(context.Context, []ExternalIdentity) (AliasResolution, error)
}
```

Assembler 把只来自服务端 session 的 `Subject` 与服务端选择的 purpose
（`directory_summary` / `overview` / `focused_workspace`）传入内部 `ObservationRequest`；URL、query
和 SIP 都不能自报 Subject、purpose 或 permission。`directory_summary` 只调用生成服务摘要所需的
signal tools，不 fan-out logs/traces/access；`overview` 与 `focused_workspace` 再按 Profile renderer、
tool refs 和当前权限选择来源。

`ObservationCoordinator.ReadObservations` 在同一父 context 下并行执行已授权、且当前 purpose 真正
需要的来源；每个 child 使用 `min(Profile limit, ToolDescriptor limit, permission limit, global
limit)`，来源独立取消。单来源 deadline 转为该来源的最小范围 Unknown；父 context 取消后所有
HTTP request 必须立即终止并向上返回 `context.Canceled`/`DeadlineExceeded`，不能把已断开的请求
伪装成一个可返回的 Partial，也不能遗留 goroutine。

### 6.3 ObservationFacts 的兼容扩展

内部事实至少需要新增：

- `FocusResult`：not_requested / matched / empty / unknown；
- `JourneyStageObservations`：stage key、契约态 `ok|failed|pending|skipped|unknown`、observed_at、
  evidence ids、reason；内部不得再引入平行的 `completed` 状态；
- `ObservedTopologyHints`：source/target external identity、kind、observed_at、evidence ids；
- `AccessEvidence` / logs / traces 的独立 source state；
- `ReconciliationIssues`：alias_not_found/conflict/bounds，禁止包含未脱敏 focus/identity 原文。

Assembler 把唯一解析的 hints 合并进已有 `TopologyFacts`：

- canonical relation 已存在：provenance 变为 `declared_and_observed`，保留真实 relation ID；
- 只有 observed、但两端都唯一解析：使用 `obs_<sha256-prefix>` 形式的确定性 overlay ID；hash
  输入包含 service/profile、canonical source/target ID 与 relation kind，结果必须小于 v1 的 128
  字符上限，只存在当前 Context，不写数据库；
- 任一端未解析/冲突：不建 node/relation，只给 affected source/diagnosis Unknown；
- overlay 超 TTL：relation Stale/Unknown，declared 底图仍在；
- bounds 达限：保留已确认 facts，Data Partial，禁止静默截断。

### 6.4 Journey 状态只能来自 observed facts

Profile 只声明 stage 顺序与 canonical refs。状态规则：

- 明确成功事实 -> `ok`；
- 明确失败事实 -> failed；
- 已匹配 focus 且前序已到达、后续尚未观察 -> pending；
- source timeout/Forbidden/字段不确定/冲突 -> unknown；
- focus 查询成功无结果 -> journey section Empty，不生成 stages progress；
- stage evidence 必须引用当前 Context 的 EvidenceRef。

Phase 3 不从“没看到事件”推导 `skipped`；该枚举只为未来由受信 adapter 返回的显式、Profile
可验证的 not-applicable 事实保留。本阶段没有这种事实时一律 `unknown`，不得假跳过。

### 6.5 SIP 不携带自由查询

SIP binding 只允许选择受信模板和类型化参数。Prometheus 首批模板建议：

- `counter_rate.v1`；
- `counter_ratio_percent.v1`；
- `histogram_quantile_rate.v1`；
- `gauge_ratio_percent.v1`；
- `gauge_max.v1`。

OpenObserve 首批模板建议：

- `focus_records.v1`；
- `trace_by_focus.v1`；
- `access_by_focus.v1`；
- `logs_by_focus.v1`。

Metric/label/stream/field 必须满足 identifier allowlist；label 参数只允许等值字面量，禁止 regex、
否定、集合与自由 matcher；focus value 只能作为严格转义后的值，不能改变 identifier、SELECT 列、
ORDER BY 或 LIMIT。所有 templates 由 alert-system 代码/embedded registry 持有并过 golden tests。

### 6.6 权限与上游错误必须区分

- 当前用户缺少 ToolDescriptor permission：不调用上游；最小 Evidence/section Forbidden；
- upstream service account 401/403：source Unavailable + `upstream_auth_failed`，不是用户 Forbidden；
- 200 + hits=[]：只有查询确实完成时才是 Empty；
- timeout/cancel/5xx/rate-limit/parse error：Unavailable/Unknown，带稳定 reason 与 retryable；
- response 超 byte/row/cardinality limit：`scope_too_broad` 或 source Partial，不截断成 Complete。

Phase 3 提供 `PermissionResolver`；默认策略必须从 session 角色服务端计算。当前尚无 service-level
visibility policy，沿用已登录用户可见服务的现状，缺省矩阵固定为：

| permission | member | admin |
|---|---:|---:|
| `metrics.read` | allow | allow |
| `traces.read` | allow | allow |
| `faults.read` | allow | allow |
| `logs.read` | deny | allow |

`access_by_focus.v1` 只有在输出 schema 保证不含 request/response body、header 或其它 raw payload 时
才可声明 `traces.read`；否则必须要求 `logs.read`。Evidence open 重新检查其来源 permission。任何 URL
自报 roles/permissions 继续无效；以后引入 team/service policy 必须只会收紧该矩阵并更新
`PolicyVersion`，不能静默放宽。

### 6.7 Evidence broker 不信任客户端 locator

Adapter 返回内部 locator；Page View 对外只给最小摘要、hash/record id 与 alert-system broker link。
Broker 打开时：

1. 重新读取 session 与 service visibility；
2. 重新检查对应 `logs.read` / `traces.read` / `metrics.read` / `faults.read`；
3. 校验 evidence grant 未过期、service/profile/focus/window 未漂移；
4. 只重定向到配置的 allowlisted upstream UI host/path，由浏览器使用自己的 SSO/session；broker
   永不把 service-account credential、内部查询或 locator 放进 redirect；
5. 记录 metadata audit，不记录 token、原始日志或完整 focus value。

Phase 3 使用 PostgreSQL 保存短期、opaque 的 evidence grant metadata，避免多副本路由或重启让
合法 deep link 随机失效。grant 在 workspace 装配完成、确定最终可投影 EvidenceRefs 后批量创建：
单视图数量取 Profile/global EvidenceRef limit 的最小值且硬上限 100，单次批量事务插入；失败只把
对应 EvidenceRefs 降级为 Unavailable 并移除 broker link，不影响 signals/domain。directory/overview
不预建未展示的 grant，broker open 不接受客户端 locator。

表中只保存 broker 重建 allowlisted link 所需的 typed record/trace locator、actor/service/profile
绑定、focus/window 的服务端 keyed digest、required permission、created_at 与 expires_at；permission
snapshot 只用于审计漂移，绝不能替代打开时的实时鉴权。不保存完整 focus、日志正文、span
attributes、查询、凭据或上游 Authorization。TTL 取配置上限与上游剩余 retention 的最小值；过期、
撤权或绑定不一致显示 Expired/Unavailable，不能绕过鉴权或回退到客户端 locator。

### 6.8 Outbound HTTP 安全基线

- endpoint 只来自服务端配置；禁止 URL userinfo、fragment、任意 query、非 http(s) scheme；
- production 默认 HTTPS；测试只允许 loopback `httptest` HTTP；
- credentials 只在 request header 注入，配置 String/log 永远脱敏；
- 禁止自动跨 host redirect；限制 response body bytes；
- 使用共享 Transport，但每次 request 都绑定 context deadline；
- 错误消息不带完整 URL query、Authorization、SQL/PromQL、原始 response body；
- metric/search 参数通过 form/JSON encoder，禁止字符串拼 URL；
- OpenObserve identifier/SQL literal 使用独立、被测试的 allowlist/escaping helper。

## 7. 实施任务

### Task 1：SIP contract、compiler 与 provider template registry

**输出**

- 新增 `internal/serviceobs/integration/`：manifest/model/schema/compiler/registry；
- 新增 docs 下的 SIP v1 schema、README 与 E2B draft package fixture；
- compiler 将 service/entity selectors 唯一解析为现有 v1 Profile 的真实 ID；
- provider template registry 校验 template ref、参数 schema、输出字段和 adapter capability；
- 编译清单包含 package/profile version、目标环境、resolved IDs、content digest；不含凭据；
- 回滚通过重新激活上一份不可变编译产物，不修改 v1 Profile ID 语义。

**测试**

- 合法 E2B draft package 编译后通过现有 Profile validator；
- selector 0/2+ 命中、未知 template、版本不兼容、禁止字段、raw query/secret/executable key
  全部 fail closed；
- compiler 不接受 fixture service ID 作为 production fallback；
- 第二个结构不同的最小 package fixture 能通过 contract，但不算 Phase 4 产品验收。

### Task 2：source-specific ports 与 ObservationCoordinator

**输出**

- 拆分 signal/trace/log/access/alias/permission/audit/broker ports；
- 扩展内部 ObservationFacts 表达 focus/journey/observed hints/reconciliation；
- ObservationRequest 携带服务端 Subject 与 purpose；coordinator 只并行调用当前 purpose、Profile 和
  权限共同选中的来源，并按稳定 source key 合并；
- 单来源 panic/error/timeout/cancel 不拖垮其它来源；
- 没有配置任何 provider 时保持 Phase 2 精确语义 `telemetry_not_connected`；部分配置时，当前
  purpose 已选择但未连接的 source 必须显式 Unavailable（如 `openobserve_not_connected`），不能
  省略；当前 purpose 根本未请求的 source 才不出现在结果中。

**测试**

- Prometheus success + OpenObserve timeout；反向组合；两者 timeout；logs Forbidden + traces success；
- Prometheus-only、OpenObserve-only、完全未配置，以及 directory summary 不触发 evidence calls；
- parent cancellation 传播，goroutine 可回收；
- duplicate/missing binding 不覆盖其它 binding；
- source status、sections、Data/Health 真值表保持最小范围 Unknown；
- race 测试覆盖并发合并。

### Task 3：配置、secret/redaction 与安全 HTTP client

**输出**

- 扩展 `ServiceObservabilityConfig`，endpoint 为空表示 provider 未连接；
- Prometheus/OpenObserve 分别拥有 timeout、response bytes、并发与 provider limits；
- evidence grant TTL、audit retention days、grant cleanup batch 与 background refresh 最小间隔都为
  bounded server config；非法关系启动失败，不允许请求参数覆盖；
- secret 从服务端 env/file provider 读取，不进入 YAML 示例明文、日志或 stats；
- 统一 outbound client 禁止跨 host redirect、限制 body、保留 context cancellation；
- 启动日志只记录 enabled/capability/limit，不记录 endpoint path、org、stream、用户名或 secret。

**测试**

- 非法 scheme/userinfo/redirect/response-too-large/timeout/secret String 全部覆盖；
- 未配置 provider 启动成功并显式 Unknown；显式错误配置启动失败；
- production 与 test HTTP policy 分离，不能靠 caller 自报环境绕过。

### Task 4：Prometheus-compatible semantic signal adapter

**输出**

- 使用官方 `/api/v1/query` / `query_range`，POST form 编码；provider `limit` 只有 capability
  discovery 明确支持时才发送，alert-system 自身 bounds 始终执行；
- 解析 scalar/vector/matrix 与 warnings，拒绝 NaN/Inf、未知 result type 和超 cardinality；
- 从 typed template 构造 rate/ratio/histogram/gauge 查询；
- 每个 binding 独立 availability/freshness/observed_at/source key；
- discovery 只读列出受限 metric/series/labels，并输出可审计 manifest；
- HugePages 使用率固定为 `(Total-Free)/Total`，Total<=0 为 Unknown；
- Traffic/Rate 无方向 threshold 时 neutral；Saturation 缺直接指标时 Unknown。

**测试**

- `httptest` 覆盖成功、empty、warning、422、503、timeout、malformed、oversize、cardinality；
- P95 histogram、rate、error ratio、HugePages 公式的 golden tests；
- metric/label 参数注入、动态 focus label、高基数选择器被拒绝；
- missing/stale binding 不复用旧值，不填 wall clock observed_at；
- 兼容 Prometheus JSON envelope，不依赖产品特定 UI。

### Task 5：OpenObserve focus/log/trace/access adapter

**输出**

- 使用固定 Search/Trace templates；start/end 始终为服务端绝对 UTC 微秒；
- 查询字段固定为 package binding allowlist，逐条关联只接受 `sandbox_id`；
- logs/traces/access 分开调用、权限、source status 与 limits；
- Trace spans 投影 journey observation、directed topology hints 与 trace EvidenceRef；
- logs/access 只返回脱敏摘要、record/hash/time 与 broker locator，不返回正文；
- discovery 读取 stream list/schema/stats，确认字段与 retention 时间边界；
- request context 取消触发 HTTP disconnect；不自行切分无界时间窗。

**测试**

- `sandbox_id` 精确匹配，`sandboxid`、substring、SQL injection、未知 field/stream 全部拒绝；
- focus matched/empty/unknown；stage ok/failed/pending/unknown；本阶段不得凭缺失事实生成 skipped；
- logs Forbidden 不调用 upstream 且不泄露 count；traces 仍成功；
- upstream 401/403 与用户 Forbidden 区分；
- timeout/cancel/rate-limit/parse/oversize/row bound 映射稳定 reason；
- 原始 log/span attributes 不进入 Page View、audit 或错误文本。

### Task 6：Alias resolver、observed overlay 与 topology reconciliation

**输出**

- 批量、单事务读取 alias，避免每个 span N+1；
- namespace 仅来自 SIP topology policy allowlist；
- 0/1/2+ 命中分别为 not-found/resolved/conflict；
- 合并 declared + observed 时保留 relation provenance、真实 canonical IDs 与 TTL；
- overlay-only relation 使用长度小于 128 的 `obs_<sha256-prefix>` context-local deterministic ID，
  hash 输入包含 service/profile/canonical endpoints/kind，不写 `dependency_edges`；
- E2B component node 与 instance inventory 分离，instance count 进入 fleet/summary；
- reconciliation issues 可审计但不泄露原始高敏 identity。

**测试**

- declared-only、observed-only、both、stale、not-found、conflict、bounds；
- caller -> callee 方向不反转；Fault blast-radius 领域遍历不受影响；
- topology adapter 失败不扩大 domain boundary；
- 两个 service 的 alias/overlay 不能串线；
- 未解析 hint 不创建 node/relation、不显示 Healthy。

### Task 7：PermissionResolver、Tool runtime、audit 与 Evidence broker

**输出**

- Tool execution 仅为内部 typed interface，不新增通用浏览器 execute endpoint；
- permissions 由 session Subject 与服务端 policy 计算；
- limits 取 Profile/Tool/policy/global 最小值；
- 在当前 migration 序列 000024 之后新增 `000025_service_observability_tools.up/down.sql`；实施前必须
  重新扫描编号，冲突就停止而不是改写别人的 migration。store 方法进入
  `internal/store/service_observability_tools.go` 并遵循现有 `pgx.Tx`/Repo 约定；
- PostgreSQL invocation audit 每次 tool 调用只保留一条 bounded row，包含 request_id、actor ID、
  actor kind、service、profile、tool、started/finished、duration、row/cardinality、result
  status/reason；不含 secret/query/raw evidence；
- 事务边界固定为三段：独立事务插入 `started` 并提交 -> provider 调用完全位于 PostgreSQL 事务外
  -> 独立事务条件更新 completion 并提交。start 失败不调用 provider；completion 失败丢弃 provider
  结果并返回 `audit_unavailable`；任何持锁事务内禁止同步外部 IO；
- 用户触发的 workspace/tool/broker open 使用真实 actor 并逐调用审计；background directory refresh
  使用固定 system actor、只调用 summary 必需 tools、受 refresh concurrency/最小间隔限制，仍逐调用
  留一条 audit row，不用 start+completion 两行制造双倍写放大；
- PostgreSQL bounded Evidence grant store 在最终投影后按单视图硬上限 100 批量单事务创建；TTL、
  keyed focus/window digest 与 broker open endpoint 遵循 §6.7；
- 复用现有 `retention_sweep` handler，新增 audit retention 与 expired grant 的 bounded batch 删除；
  不注册一个无人处理的新 timer kind；
- broker 再鉴权、allowlist redirect、过期/撤权 fail closed。

**测试**

- URL role/permission 注入无效；member/admin 缺省矩阵与 logs.read 最小 Forbidden；
- 未授权时 upstream 调用次数为 0；
- limit/deadline/cancel/audit metadata 完整；
- start/provider/completion 三段不共享事务，provider 阻塞时没有 idle-in-transaction；每次调用恰好
  一条 audit row，system actor refresh 受写预算约束；
- evidence grant 绑定 actor/service/profile/focus/window，跨用户/跨服务/篡改/过期全部拒绝；
- grant 只为最终投影 EvidenceRefs 批量创建，0/1/100/101 边界、批量写失败的局部 Unavailable、
  keyed digest 与实时权限变化均覆盖；
- unsafe redirect/header injection 被拒绝；
- audit start/completion sink 失败分别验证“provider 未调用”与“结果不返回”；
- grant/audit 表不保存 query、secret、完整 focus、raw evidence；`retention_sweep` 可有界清理过期
  grant/audit，down migration 可回滚且不触碰既有领域表。

### Task 8：Assembler、projector、API 与前端兼容接入

**输出**

- journey 使用真实 observations，不再固定 Unknown；
- observed overlay 与 RED/USE scope attachment 保持现有统一 evaluator；
- capabilities 同时考虑 provider connected 与当前用户 permission；
- evidence deep link 指向 broker；局部 retry 只重试失败来源；
- `formatSourceName` 为 `prometheus_compatible`、`openobserve_logs`、`openobserve_traces`、
  `openobserve_access` 增加明确本地化词条，不依赖 fallback 猜文案；
- Empty/Partial/Forbidden/Stale UI 继续遵循现有画稿和 query contract；
- production build 无 fixture/provider secret/query template 内容。

**测试**

- Context/Page View schema 与 projection invariants；
- E2B main、Prometheus-only failure、OpenObserve-only failure、logs Forbidden、focus Empty、stale
  overlay、alias conflict；
- API 200/403/404/409/422/503 语义不漂移；
- React 三路由、URL 恢复、局部 retry、Evidence broker、键盘/表格 topology；
- production bundle 搜索 fixture IDs、scenario keys、endpoint、template text、secret token 均零命中。

### Task 9：E2B discovery、production SIP 与只读 UAT smoke

**前置**

- 只读 endpoint/credential 已由用户或部署环境提供；
- 不在终端输出 secret、完整 Authorization、日志正文或个人数据；
- 网络不可达不是理由去修改 Kubernetes/OTel 配置。

**步骤**

1. 运行 discovery，保存脱敏 manifest：metric names/types/labels、stream schema/time bounds、hash；
2. 固化 E2B typed bindings，只使用确认后的名称与逐条 `sandbox_id`；
3. 验证 RED histogram buckets 与 labels 能计算 P95；
4. 验证 HugePages Total/Free、host alias/IP 映射；
5. 验证 placement rejection metric/log 语义；直接 saturation 不存在时保持 Unknown；
6. replay 一个已有 UAT `sandbox_id`，不创建新 Sandbox、不注入故障；
7. 比对 journey、API -> Orchestrator RED、host USE、EvidenceRef、domain deep link；
8. 记录 retention、observed_at、timezone、freshness 与 threshold approval 状态；
9. 只有全部 required binding 与 owner gate 完成后，E2B SIP/Profile 才标为 published。

**验收**

- discovery/replay 命令退出 0，输出无 secret/raw evidence；
- 任一 required 事实未确认时发布命令 fail closed，并明确缺口；
- UAT source 暂不可达时，本地 contract/adapters 仍可完成，但 Phase 3 状态必须标记“实现完成、
  生产发布未验收”，不能宣称整个 Phase 完成。

### Task 10：Phase 3 全量验收与文档收口

**门禁**

```bash
git diff --check
python3 docs/contracts/service-observability/v1/verify_examples.py
gofmt strict-empty check on changed Go files
go build ./...
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./internal/serviceobs/... ./internal/api ./internal/app ./internal/config ./internal/store ./cmd/server
cd web && npx oxlint && npx tsc -b
cd web && npm run test:service-observability
cd web && npm run build
```

有 Docker 时再运行 integration-tag/store/e2e；无 Docker 时必须把缺口写进验收，不能把未运行说成
通过。外部 UAT smoke 与 hermetic CI 分开，凭据缺失不让普通 PR CI 失败，但发布 gate 必须要求。
migration/store integration 还必须证明 000025 up/down、三段 audit 事务、batch grant、
`retention_sweep` 清理和 concurrent broker open；provider 阻塞探针不得出现
`idle in transaction`。

**文档**

- Phase 3 acceptance 记录 exact SHA、source discovery digest、UAT smoke 与所有未决项；
- ADR-0007 经 Fable/Codex review 后更新状态；
- contracts README 记录 SIP 与 production-vs-fixture 边界；
- 不在文档中粘贴 secret、完整 endpoint、原始 SQL/PromQL 或日志正文。

## 8. Cursor Cloud 执行切片

只使用固定 Cursor Cloud 会话：

```text
https://cursor.com/agents/bc-5a993b86-e20a-46b1-b033-028118eced93
```

### Review 0：Fable 5 High

只审查 ADR-0007 与本计划，输出：

- P0/P1/P2 gaps；
- 是否存在与 v1 契约、Phase 2 ports、权限或 Unknown 最小降级冲突；
- SIP 是否仍可能携带自由查询/代码；
- topology alias/overlay 是否可能污染 canonical graph；
- Phase 3 是否误实现 Agent 或触碰 Kubernetes/OTel；
- 建议的最小计划修订。

Fable 不写运行代码，不开始 Task 1。

2026-08-23 评审结果为 `REVISE`、无 P0。四个 P1（journey 枚举、PostgreSQL 工程契约、自由
matcher 措辞、grant 创建预算）已在本计划与 ADR-0007 最小修订关闭；Fable 明确确认修订后可直接
进入 Batch A，因此不再增加第二轮全量计划评审。

### Opus 5 High Batch A：Task 1–3

保持一次 Cloud 回合、一次最终 push，但内部做两个可回退 checkpoint：A1 只完成 Task 1 的
SIP/schema/compiler/registry；A2 再完成 Task 2–3 的 ports/coordinator/config/security client。每个
checkpoint 独立 commit 并先跑自己的 focused tests，A1 失败不得进入 A2；最终一起 push，Codex 只在
Batch 完成后跑 contract、focused unit/race/build，不增加 GUI 往返，也不做浏览器全量 QA。

### Opus 5 High Batch B：Task 4–6

Prometheus/OpenObserve/alias-overlay。完成后一次 push，Codex 跑 provider `httptest`、projection、
store integration 与 security negative tests。

### Opus 5 High Batch C：Task 7–8

权限/audit/broker 与跨层 Page View/UI。完成后一次 push，Codex 跑 API、前端、production-bundle
和浏览器状态矩阵。

### Opus 5 High Batch D：Task 9–10

只读 discovery/UAT、production E2B SIP 与验收文档。缺少 endpoint/credential 或 owner approval 时
停止发布，不修改采集配置，不伪造验收。

Cursor 每个 batch 只报告：base SHA、head SHA、改动文件、关键决策、命令真实结果、未运行项、
hold path 检查和 push 状态。中间进度以远端 SHA 为准，不要求 Codex 高频轮询 GUI。

## 9. Grok review 策略

Grok 不参与开发。每个 Phase 只在候选 SHA 上集中 code review 一次，重点检查：

- auth/SSRF/query injection/secret leakage；
- timeout/cancellation/goroutine/response bounds；
- Empty/Unknown/Forbidden/Unavailable 映射；
- alias conflict 与跨 service 数据串线；
- profile/package/provider registry 是否出现 E2B 核心分支；
- evidence broker 的重新鉴权与 grant 绑定；
- 未确认数据是否被伪造成 production fact。

真实缺陷统一进入一个窄 PR；Grok 不合并。Codex 独立复现、审查 diff、跑对应门禁后决定合入。
机械格式修正继续追加到同一 PR，不新开 PR、不触发整轮重复评审。

## 10. Phase 3 完成定义

只有下列证据全部成立，Phase 3 才能标记完成：

- [ ] SIP contract/compiler 能生成真实 ID 的合法 v1 Profile，禁止代码/query/secret；
- [ ] Prometheus 与 OpenObserve adapter 使用受信 typed templates，全部 hermetic tests 通过；
- [ ] E2B main path 能从真实只读 sources 投影 Golden/RED/USE、journey、overlay 与 EvidenceRef；
- [ ] `sandbox_id` 是逐条关联字段，`sandboxid` 被拒绝；
- [ ] metric names/labels/stream fields/retention 已 discovery 并记录 digest；
- [ ] HugePages 公式与 host aliases 验证；直接 saturation 缺失时仍 Unknown；
- [ ] logs Forbidden、provider timeout、focus Empty、stale overlay、alias conflict 均最小降级；
- [ ] current-user Forbidden 与 upstream credential failure 不混淆；
- [ ] Evidence broker 再鉴权、grant 绑定、redirect allowlist 与 audit 通过；
- [ ] audit 每次 tool 调用恰好一行，provider 位于事务外；background summary refresh 不读取
  evidence 且受 system-actor 写预算约束；grant 单视图不超过 100 并由 `retention_sweep` 清理；
- [ ] 三页仍使用同一 evaluator，前端无 provider query/credential；
- [ ] fixture mode 仍仅 DEV，production bundle 无 fixture/provider secret/template；
- [ ] `GET /api/v1/services` 与既有领域路径不破坏；
- [ ] Codex 全量门禁、浏览器验收、GitHub CI 和 Grok review 收敛；
- [ ] Kubernetes/OTel 三个 hold path 零改动；
- [ ] Agent 运行代码、API、UI、表和 worker 均不存在；
- [ ] E2B SLO threshold/proxy 发布 gate 有 owner 审核证据；
- [ ] 生产 E2B SIP/Profile 已发布或明确因外部 gate 未满足而保持未完成，不能降格验收。

## 11. Phase 4 输入

Phase 4 选择 PostgreSQL 作为与 E2B 结构不同的第二 Service Profile，避免触碰共享 Kubernetes
配置。它应使用 instance/database focus、Golden/USE 为主、journey 可为空，证明：

- 同一 SIP/compiler/provider adapters 可接入非请求旅程型服务；
- renderer/assembler 不增加 `if profile == postgresql`；
- v1 Profile/Tool/Page View contracts 足够表达结构差异；
- 通过后将 contracts v1 从 Draft 提议为 Stable。

Agent 仍不在 Phase 4 实现。Agent-as-a-Service 需要独立 ADR 与后续 Phase。
