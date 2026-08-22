# Service Observability Phase 2：alert-system 基础数据实施计划

> 状态：待 Cursor Cloud Agent / Claude Fable 5 High 评审；评审收敛前不得开始编码。
>
> 执行者：Cursor Cloud Agent / Claude Opus 5 High。
>
> 总调度与最终验收：Codex。

## 1. 目标

在不连接 Prometheus、OpenObserve 或其它生产遥测源的前提下，把 Phase 1 的三层 `/services` 一等 E2B demo 升级为真实的 alert-system 只读聚合边界：

1. Service Registration 支持 `platform/infrastructure_capability`，且旧调用方缺省仍为 `application/business_system`；
2. alert-system 启动时加载并校验 file-based Service Profile；
3. 三层 Page View 由统一的服务端 evaluator/projector 产生；
4. live 路径读取 Service Registration、canonical topology、Alert/Fault/Impact/Incident；
5. 未接入或过期的数据只在最小 signal/source/section 降级为 Unknown/Partial/Stale；
6. `/services` 摘要使用进程内 snapshot cache 与 bounded stale-while-revalidate；
7. 前端正常运行路径消费三个聚合 API，显式开发 fixture 模式继续保持 Phase 1 的完整 E2B demo 和五态视觉基线。

生产主落地始终是 alert-system；E2B 是首个 Service Profile；fixtures 优先；Agent 暂缓；缺失或过期数据按最小范围降级为 Unknown。

## 2. 基线与唯一设计事实源

实现基线：

- 远端分支：`codex/service-observability-phase2-20260823`
- 起始提交：`702726b90762a0c92e54ffeb7c7cea101b466c83`
- 起始 tree：`dad381ce3b2a8e07845ba1e4b98af6994292db00`
- 该 tree 已通过现有完整 Go、race、integration、web、contract 门禁。

实现与评审只以以下内容为设计事实源：

1. `docs/plans/2026-08-22-service-observability-complete-design-handoff.md`
2. `docs/contracts/service-observability/v1/README.md`
3. `docs/assets/service-observability/README.md`
4. 仓库当前 AGENTS 指令
5. `docs/contracts/service-observability/v1/*.schema.json` 与 `examples/*.json` 是上述契约 README 指向的机器可读定义。

旧会话、外部产品报告、历史分支实现均不得覆盖这些事实。

## 3. 本 Phase 明确不做

- 不连接生产 Prometheus、OpenObserve、Tempo、Loki 或任何其它遥测源；
- 不发布依赖未确认 metric names/labels/retention 的生产 E2B Profile；
- 不实现 Agent、模型、Prompt、session、memory、stream、Agent API 或 Agent UI；
- 不实现 Tool 执行器或 Evidence broker；本 Phase 只建立 Profile 引用所需的只读 Tool descriptor catalog 边界；
- 不注入故障，不执行写诊断，不改变 Alert/Fault/Impact/Incident 领域状态；
- 不修改 Kubernetes/OTel hold 区域：
  - `deploy/observability/`
  - `docs/adr/2026-08-13-otel-prometheus-dual-write-demo.md`
  - `docs/plans/2026-08-13-e2b-otel-prometheus-remote-write-demo.md`
- 不把 `e2b.context.json`、Page View fixtures 或其中的 synthetic ID 当作生产事实写入数据库；
- 不在 renderer、assembler、cache key 或 API handler 中加入 `if service == e2b` 一类分支；
- 不改变既有 `GET /api/v1/services` 的路径和响应结构。

## 4. Phase 2 关键裁决

### 4.1 live 与 fixture 两条数据路径

正常运行路径：

```text
authenticated API
  -> serviceobs facade
  -> alert-system registration/topology/domain adapters
  -> unavailable telemetry port (Phase 2)
  -> context assembler
  -> shared health/data evaluator
  -> page-view projector
```

因此，已配置 Profile 但遥测尚未接入时：identity、declared topology、Fault/Impact/Incident 仍可用；Golden/RED/USE、observed path 与 evidence 在各自最小范围为 Unknown/Unavailable；整体 Data 为 Partial，不得显示历史绿值。

显式开发 fixture 模式：

- 继续由 `VITE_SERVICE_OBSERVABILITY_FIXTURE_MODE=true` 读取契约 examples；
- 保留 main、Partial、Forbidden、Empty、Stale 五态；
- 用于产品 demo、projector 契约回归和视觉 QA；
- fixture 模式不得成为 API 网络错误的静默 fallback。

### 4.2 Profile 发布语义

- `SERVICE_OBSERVABILITY_PROFILE_DIR` 为空：启动成功，registry 为空；总览为 Unconfigured，工作区返回 409；
- 显式配置的目录不可读：启动失败，避免部署者误以为 Profile 已加载；
- 单个 Profile schema/引用非法：服务继续启动，但该 Profile 被 quarantine，不进入 active registry；若能安全识别 service ID，总览显示 `profile=null`、Data Unconfigured，并给稳定配置错误 reason code；
- 同一 service 存在多个 active published Profile：全部冲突项 quarantine，绝不按文件名或加载顺序猜一个；
- E2B example 仅在测试和 fixture 模式加载。Phase 3 完成真实 discovery 后另行发布生产 E2B Profile v1。

### 4.3 当前授权口径

- 三个聚合 API 复用现有 session auth；
- 当前产品没有 service-level visibility policy，因此已登录用户的 service 可见性与既有 `GET /api/v1/services` 一致；不存在时统一 404，不新增虚假的团队级 403；
- actor、角色与 capabilities 只从服务端 session 计算，忽略 URL 中任何自报角色；
- Phase 2 live capabilities：`view_domain=true`；metrics/logs/traces/evidence deep link 只有对应 adapter 真正注册并授权时才为 true，因此本 Phase 默认为 false。

### 4.4 Health 与 Data 真值表

Profile v1 没有 optional signal 字段，所以所有 `signal_bindings` 都是 required：

1. signal `availability=available` 且 `observed_at` 未超过 Profile freshness TTL 才 eligible；
2. 任一 eligible signal Critical -> service Critical；
3. 否则任一 eligible signal Degraded -> service Degraded；
4. 否则任一 required signal 缺失、Forbidden、Unavailable、Expired 或 Stale -> service Unknown；
5. 只有全部 required signals 可用且未越阈值 -> service Healthy；
6. 没有方向阈值的 Traffic/Rate 永远 Neutral，不进入 worst-of；它缺失时仍使 required coverage 不完整；
7. Fault/Incident 数量不改写 Health；
8. reason code 必须包含产生最终状态的 binding key，禁止综合分数。

Data precedence：

```text
profile absent -> unconfigured
any source forbidden/unavailable/expired/unknown -> partial
else any source freshness=stale or cache snapshot stale -> stale
else -> complete
```

`availability=empty` 是一次成功查询，不自动把 Data 变成 Partial；Empty 只作用于对应 section。

### 4.5 Snapshot cache

- key：`service_id + environment + profile_version + default_window_seconds`；
- value 只能包含目录摘要：Health、Data、四大 Golden compact values、domain counts、observed_at；
- 默认 fresh TTL 45s，max stale 5m，可配置；
- fresh：直接返回；stale：返回旧摘要并显式 Stale，同时入 bounded refresh queue；miss：立即返回 Unknown/Unconfigured 占位并入队，不在目录请求内对每行同步扇出所有 source；
- 固定 worker 数、bounded queue、per-key singleflight；队列满时保留 Unknown/Stale，不额外起 goroutine；
- refresh 失败不覆盖最后成功快照；超过 max stale 后不得继续显示旧绿值；
- cache 不保存 logs、traces、原始 evidence 或凭据；
- 接口允许 Phase 3/后续替换为 PostgreSQL snapshot store。

## 5. 目标 package 与依赖方向

```text
internal/api
  -> internal/serviceobs
      -> context / view / profile / health / cache ports
      -> model (leaf package)
internal/serviceobs/adapters/alertsystem
  -> serviceobs ports + internal/store/domain
internal/app
  -> registry + adapters + cache + facade assembly
```

硬约束：`api -> facade/view/context -> ports`；adapter 实现 ports；核心 package 不 import E2B 私有 package；前端不接触 PromQL/SQL/凭据/Tool 内部模板。

## 6. 实施任务

每个任务先写失败测试，再写最小实现，再运行本任务命令，再提交。不要把所有改动压成一个大提交。

### Task 1：泛化 Service Registration 的 layer/kind

**修改**

- `internal/domain/infrastructure.go`
- `internal/domain/infrastructure_test.go`
- `internal/api/service_catalog.go`
- 新增 `internal/api/service_catalog_test.go`
- `internal/store/service_catalog.go`
- 新增 `internal/store/service_catalog_integration_test.go`（`//go:build integration`）
- `web/src/api/client.ts`
- `web/src/pages/Settings/ServiceCatalog.tsx`

**测试先行**

1. 增加 `ValidateServiceClassification(layer, kind)`：只允许
   - `application/business_system`
   - `platform/infrastructure_capability`
2. API request 新增可选 `entity_layer`、`entity_kind`：
   - create 两字段均缺省时保持旧默认；
   - update 缺省时保留当前 registration 的分类，避免旧客户端把 platform 服务改回 application；
   - 半缺省且无法组成合法 pair 返回 422；
   - `infrastructure/resource` 不能登记为 Service；
3. store create/update 使用传入的合法 layer/kind，不再写死；select round-trip 保留；
4. 旧 V1 request 的 JSON 与响应不破坏；不需要新 migration，现有 DB CHECK 已支持目标值；
5. 控制中心表单增加“服务类型”两项，create/edit 均发送明确 pair；列表显示类型，不再把所有登记统称“应用服务”。

**验证**

```bash
go test ./internal/domain ./internal/api
go test -tags=integration ./internal/store -run 'ServiceCatalog.*Classification' -count=1
cd web && npm run lint && npm run build
```

**提交**

```text
feat(service-catalog): support platform service registrations
```

### Task 2：建立契约 model、schema validator 与只读 Tool catalog

**新增**

- `internal/serviceobs/model/profile.go`
- `internal/serviceobs/model/context.go`
- `internal/serviceobs/model/view.go`
- `internal/serviceobs/model/evidence.go`
- `internal/serviceobs/model/tool.go`
- `internal/serviceobs/contract/validator.go`
- `internal/serviceobs/contract/schemas/*.json`
- `internal/serviceobs/contract/validator_test.go`
- `internal/serviceobs/contract/schema_sync_test.go`
- `internal/serviceobs/tools/catalog.go`
- `internal/serviceobs/tools/catalog.v1.json`
- `internal/serviceobs/tools/catalog_test.go`

**修改**

- `go.mod`
- `go.sum`

**实现要求**

1. Go model 的 JSON shape 严格镜像 contracts v1；slice 输出必须是 `[]`，不能意外为 `null`；
2. 将五份 canonical schema 和 tool catalog 作为生成镜像嵌入 binary；`schema_sync_test.go` 必须逐字节/规范化 JSON 对比 `docs/contracts/service-observability/v1/`，防止双份漂移；
3. 使用 `github.com/santhosh-tekuri/jsonschema/v6@v6.0.3` 编译 Draft 2020-12 schema；compiler 只注册本地 embedded resources，运行时不得联网取 `$ref`；
4. 启动时 compile once，后续并发 Validate；错误返回稳定 path，不把完整敏感文档写日志；
5. Tool catalog 只提供 `Get/Has/List` descriptor，不提供 Execute；key + semantic major 唯一；source adapter 只是受控 key；
6. contract tests 对 Profile、Context、三种 Page View、EvidenceRef、ToolDescriptor examples 全部通过；负例必须拒绝额外 query/secret/prompt/executable 字段。

**验证**

```bash
go test ./internal/serviceobs/contract ./internal/serviceobs/model ./internal/serviceobs/tools
python3 docs/contracts/service-observability/v1/verify_examples.py
```

**提交**

```text
feat(serviceobs): add v1 contract models and offline validation
```

### Task 3：file-based Profile Registry 与启动校验

**新增**

- `internal/serviceobs/profile/registry.go`
- `internal/serviceobs/profile/validate.go`
- `internal/serviceobs/profile/registry_test.go`
- `internal/serviceobs/profile/testdata/invalid-*.json`

**实现要求**

Registry 构造时注入：profile filesystem、contract validator、Tool catalog、外部引用 resolver。不要在 package 内读取全局 env 或数据库。

校验顺序：

1. 文件必须是普通 `.json`，大小有上限，拒绝 symlink/目录穿越；
2. JSON decode 使用 unknown-field/schema 双重拒绝；
3. schema validation；
4. 递归拒绝 forbidden key：`query/promql/sql/logql/shell/secret/token/headers/prompt/html/executable`；只检查 key，不对合法说明文字做 substring 猜测；
5. focus `value_pattern` 必须可由 Go RE2 编译；恰好一个 primary focus；
6. journey/view/signal/tool refs 全部存在且唯一；view order/key 唯一；threshold direction 与 degraded/critical 顺序合法；proxy 必须有 reason；
7. service ID、root entity IDs 通过 resolver 存在，且 root 属于相同 environment/服务边界；
8. tool refs 全部存在于已发布 catalog；
9. 每 service 只能有一个 `status=published` active profile。

Registry 暴露：

- `PublishedForService(serviceID)`
- `DiagnosticForService(serviceID)`
- `ListPublished()`

返回对象 immutable/deep copy，避免 handler 修改共享配置。

**测试矩阵**

- E2B example + fake resolver/canonical tool catalog 成功；
- schema invalid、未知字段、forbidden key、非法 regex、0/2 primary、悬空 ref、缺 tool、缺 service/root、duplicate published 均 quarantine；
- 单坏文件不阻断其它合法 Profile；目录本身不可读返回构造错误；
- 并发读无 race。

**验证**

```bash
go test -race ./internal/serviceobs/profile
```

**提交**

```text
feat(serviceobs): load and quarantine file profiles at startup
```

### Task 4：统一 Health/Data evaluator

**新增**

- `internal/serviceobs/health/evaluator.go`
- `internal/serviceobs/health/evaluator_test.go`

**测试先行**

表驱动覆盖：

- critical > degraded > missing unknown > healthy；
- above/below 两种 threshold；
- Traffic/Rate 无 threshold 为 Neutral；
- neutral 不改写 worst-of，但缺失仍影响 required coverage；
- stale/forbidden/unavailable/expired/value null 为最小 Unknown；
- proxy role/reason/threshold source 原样保留；
- reason codes 只列最终状态的 binding；
- profile absent -> Data Unconfigured；
- source failure -> Partial；全 available 但有 stale -> Stale；Empty 不触发 Partial；
- current Critical 与 observed relation Stale 正交，Health 仍 Critical，Data Stale。

Evaluator 是三页唯一 Health/Data 计算入口；前端和各 projector 不得二次计算。

**验证**

```bash
go test -race ./internal/serviceobs/health
```

**提交**

```text
feat(serviceobs): evaluate health and data truth tables
```

### Task 5：alert-system registration/topology/domain 只读 adapter

**新增**

- `internal/serviceobs/ports.go`
- `internal/serviceobs/adapters/alertsystem/registration.go`
- `internal/serviceobs/adapters/alertsystem/topology.go`
- `internal/serviceobs/adapters/alertsystem/domain.go`
- `internal/serviceobs/adapters/alertsystem/adapters_test.go`
- `internal/store/service_observability.go`
- `internal/store/service_observability_integration_test.go`（`//go:build integration`）

**实现要求**

1. ports 返回规范化事实与每个 source 的 status；不得返回 Page View；
2. registration list 是 lightweight identity query，不为目录逐行读取 aliases/dependencies；只返回 enabled registrations，metadata filter 参数化；
3. topology 从 service entity + Profile roots 构造 bounded canonical neighborhood：consumer/caller -> provider/callee；保留 declared provenance、criticality、observed_at；max nodes/relations 达限时显式 partial，不静默截断；
4. 无 Profile 时仍以 service entity 与 registration declared dependencies 形成基础总览；
5. domain 查询关联当前 service neighborhood 的 active Alert/Fault/Impact/Incident；focus 存在时只用白名单 focus kind 在 JSON labels/resource_ref 参数化匹配；
6. Fault summary、confirmed/potential Impact、Incident 与 timeline 只投影已提交事实；不得创建领域对象或从 Fault 数量推导 Health；
7. 同一 overview/workspace 组装中的 registration/topology/domain 读使用只读事务一致快照；
8. adapter context deadline/cancellation 原样下传；没有字符串拼 SQL；
9. 当前 Phase 2 的 telemetry port 使用显式 `UnavailableObservationReader`，只返回对应 Unknown/Unavailable，不读取 fixture 或外部网络。

**集成测试**

使用临时 PostgreSQL seed 两种 service classification、declared dependencies、mapped alert、Fault、confirmed/potential Impact、linked Incident，验证：

- service 边界正确；
- 第二个 service 的领域事实不会串入；
- focus 只收窄 domain refs；
- bounds 与 cancellation；
- 所有查询均为只读，数据库行数不变。

**验证**

```bash
go test ./internal/serviceobs/adapters/alertsystem
go test -tags=integration ./internal/store -run ServiceObservability -count=1
```

**提交**

```text
feat(serviceobs): read alert-system service topology and domain facts
```

### Task 6：DiagnosticContext assembler 与三种 Page View projector

**新增**

- `internal/serviceobs/context/assembler.go`
- `internal/serviceobs/context/assembler_test.go`
- `internal/serviceobs/view/projector.go`
- `internal/serviceobs/view/directory.go`
- `internal/serviceobs/view/overview.go`
- `internal/serviceobs/view/workspace.go`
- `internal/serviceobs/view/projector_test.go`
- `internal/serviceobs/view/testdata.go`

**实现要求**

1. assembler 只组装事实：subject、service、profile、scope/focus、sources、signals、topology、evidence refs、domain state、policy；
2. 每个 adapter 独立 deadline；非关键 adapter 失败转为 source/section status，只有 registration/认证等关键依赖失败才返回页面级 error；
3. workspace query 先校验 Profile focus kind/value、绝对 UTC from/to、window 和 Profile/global bounds；无 focus 时只返回 service-level declared topology，不发明 observed journey；
4. projector 只做稳定显示投影和格式化，不发原始查询；三页调用同一个 health evaluator；
5. Profile 缺失：directory/overview 返回 identity/domain + profile null + Unconfigured；workspace 返回 typed `ErrProfileUnconfigured`；
6. missing telemetry：只把 Golden/RED/USE/journey/observed/evidence 变 Unknown/Unavailable，declared topology 和 domain 保留；
7. topology node/relation 没有 signal 时 Health Unknown，不能因存在即 Healthy；
8. Traffic/Rate display 保留中性状态；HugePages proxy 明示 measurement role/reason；真实 USE saturation 缺失为 Unknown；
9. deterministic summary 在本 Phase 只允许 `incomplete` 或由已提交单一事实产生的 `observation`；不得凭缺失遥测给 candidate/confirmed cause；
10. request ID 服务端生成，assembled_at、observed_at、freshness 分离；所有 slice 输出稳定排序和非 null。

**契约回归**

- 用 E2B Profile/Context fixture 驱动通用 projector，不出现 E2B 分支；
- 输出分别通过 Page View schema；
- 断言 context -> view 的 identity/profile/scope/focus/signal/topology/domain refs 不漂移；
- fixture 固定时钟下验证 main；场景 patch 仍由现有 contract verifier验证 Partial/Forbidden/Empty/Stale。

**验证**

```bash
go test -race ./internal/serviceobs/context ./internal/serviceobs/view
python3 docs/contracts/service-observability/v1/verify_examples.py
```

**提交**

```text
feat(serviceobs): assemble context and project three page views
```

### Task 7：snapshot cache 与 bounded SWR

**新增**

- `internal/serviceobs/cache/store.go`
- `internal/serviceobs/cache/memory.go`
- `internal/serviceobs/cache/refresh.go`
- `internal/serviceobs/cache/cache_test.go`

**测试先行**

- fresh hit 不触发 refresh；
- stale hit 立即返回旧值并标 Stale；
- miss 返回 Unknown/Unconfigured placeholder 并入队；
- 同 key 并发只执行一次 refresh；
- worker/queue 上限可证明，队列满不增生 goroutine；
- refresh 成功原子替换；失败保留最后成功值；
- 超过 max stale 后旧 Healthy 不再可见，退化 Unknown；
- canceled caller 不取消已由 worker 接管的有界 refresh，但 shutdown context 能停止全部 worker；
- race 测试通过；cache value 类型无法容纳 raw evidence/log/trace。

进程关闭时由 app lifecycle 停止 refresh workers；测试不得依赖 sleep，使用 fake clock/channel。

**验证**

```bash
go test -race ./internal/serviceobs/cache
```

**提交**

```text
feat(serviceobs): add bounded stale-while-revalidate summaries
```

### Task 8：serviceobs facade 与三个只读 API

**新增**

- `internal/serviceobs/service.go`
- `internal/serviceobs/service_test.go`
- `internal/api/service_observability.go`
- `internal/api/service_observability_test.go`

**修改**

- `internal/api/server.go`（只有必要的通用 helper；不要把业务组装塞进 Router）

**API**

```text
GET /api/v1/service-observability
GET /api/v1/service-observability/:service_id
GET /api/v1/service-observability/:service_id/workspace
```

**目录参数**

- `environment/team_id/tier/health/data_status/q/sort/cursor/limit`
- limit 默认 50、硬上限 200；enum 与 CSV 值严格白名单；
- stable sort tuple + service ID 作为 opaque base64url cursor；解码/版本/sort 不匹配返回 422，不传入 SQL；
- 默认排序：Health severity、confirmed Impact、active Fault、tier、name、service ID；
- directory 只读 cache；cache miss 不做 N×source 同步 fan-out。

**总览参数**

- `environment`、`window_seconds`；默认来自 Profile 或 300s；
- 单 section 失败仍 200 + Partial；service registry 失败 503；not found 404。

**工作区参数**

- `environment/focus_kind/focus_value/from/to/timezone`；
- from/to 必须同时出现并为绝对 UTC；timezone 只影响显示；
- invalid focus -> 422 `invalid_focus`；scope 超限 -> 422 `scope_too_broad`；Profile 未配置 -> 409 `profile_unconfigured`；service 不存在 -> 404 `service_not_found`。

**安全/HTTP 测试**

- 未登录 401；登录 member 可读；URL 角色被忽略；
- 不泄露 profile path、SQL、PromQL、secret、内部 error；
- request context cancellation 到 facade；
- Partial/Forbidden/Empty/Stale 均为 200 view 状态，不误用 500；
- response JSON 通过 Page View schema；
- `GET /api/v1/services` 原测试与 shape 不变。

**验证**

```bash
go test -race ./internal/serviceobs ./internal/api
```

**提交**

```text
feat(api): expose service observability page views
```

### Task 9：配置与生产装配

**修改**

- `internal/config/config.go`
- `internal/config/config_test.go`
- 新增 `internal/app/service_observability.go`
- 新增 `internal/app/service_observability_test.go`
- `cmd/server/main.go`
- `.env.example`

**新增配置**

```text
SERVICE_OBSERVABILITY_PROFILE_DIR=
SERVICE_OBSERVABILITY_CACHE_TTL=45s
SERVICE_OBSERVABILITY_CACHE_MAX_STALE=5m
SERVICE_OBSERVABILITY_REFRESH_WORKERS=4
SERVICE_OBSERVABILITY_REFRESH_QUEUE=256
SERVICE_OBSERVABILITY_MAX_WINDOW_SECONDS=86400
```

要求：

- YAML/env/default 解析和非法值测试；worker/queue/TTL/max stale/window 必须为正且关系合法；
- profile dir 为空是合法的 empty registry；显式不可读才启动失败；
- app 装配 Tool catalog、Profile Registry、alert-system adapters、Unavailable telemetry reader、evaluator、cache、facade；
- main 只注册 API 与 lifecycle，不包含投影规则；
- startup 日志只输出 profile published/quarantined 数量与稳定 reason，不输出文档正文或路径中的敏感内容；
- shutdown 等待 cache workers 有界退出；
- 不修改 deploy/Kubernetes 文件。

**验证**

```bash
go test -race ./internal/config ./internal/app ./cmd/server
```

**提交**

```text
feat(app): assemble service observability read path
```

### Task 10：前端从 fixture-only 升级为 live API + 显式 fixture 模式

**修改**

- `web/src/api/client.ts`
- 新增 `web/src/serviceObservability/data.ts`
- `web/src/serviceObservability/fixtures.ts`
- `web/src/pages/ServiceDirectory.tsx`
- `web/src/pages/ServiceOverview.tsx`
- `web/src/pages/ServiceObservabilityWorkspace.tsx`
- `web/src/App.tsx`
- `web/scripts/verify-service-observability.mjs`
- `.env.example`

**实现要求**

1. client 增加三条 typed API；Page View types 继续集中在 `serviceObservability/types.ts`，避免重复 shape；
2. `data.ts` 是唯一数据源选择点：
   - fixture env=true -> 契约 fixtures/scenarios；
   - 否则 -> API；
   - API failure 不得 fallback fixture；
3. React Query key 包含 service ID 与所有 server query；URL 是筛选/focus/time/topology/tab 的可分享事实源；
4. directory 的 filter/sort/cursor 由 API 负责；fixture 模式复用现有 client-side helper以保持 demo；
5. loading、404、409、422、503 分别有明确页面/section状态与 retry；Partial/Stale/Forbidden/Empty 仍渲染已有内容；
6. “重试失败数据”在 live 模式调用 React Query refetch，不再修改 scenario；scenario 参数只在 fixture 模式有效；
7. App shell 不在 live 模式全局 import fixture 来猜 service name；实际 service identity 来自 Page View；
8. 保留 Phase 1 的交互、响应式、表格拓扑、键盘、ARIA 与五态内容丰富度；不加 Agent 文案或占位；
9. browser 仍不出现 query template、PromQL、SQL、credential。

**静态/构建验证**

更新 verifier 同时断言：

- fixtures 四文件仍驱动显式 fixture mode；
- 三页 live path 调用三个 API；
- 不存在 catch 后返回 fixture 的代码；
- E2B-only literal 不进入 generic renderer；
- 现有路由、URL state、可访问性断言保留。

```bash
cd web
npm run lint
npm run test:service-observability
npm run build
```

**提交**

```text
feat(web): consume service observability page view APIs
```

### Task 11：跨层验收与文档收口

**新增/修改**

- 新增 `docs/plans/2026-08-23-service-observability-phase2-acceptance.md`
- 必要时更新 `design-qa.md`，但不得改写 Phase 1 既有证据；只追加 Phase 2 live/fixture 数据路径结果。

**必须执行**

```bash
python3 docs/contracts/service-observability/v1/verify_examples.py
go test ./...
go vet ./...
go test -race ./internal/serviceobs/... ./internal/api ./internal/app ./internal/store
go test -tags=integration ./... -race -count=1
cd web && npm run lint && npm run test:service-observability && npm run build
```

**守护检查**

```bash
git diff --name-only 702726b90762a0c92e54ffeb7c7cea101b466c83...HEAD
rg -n 'Agent|Assistant|prompt|session|memory|stream' internal/serviceobs internal/api/service_observability.go web/src/pages/ServiceObservabilityWorkspace.tsx
rg -n 'promql|logql|select .* from|authorization:|api[_-]?key|secret' web/src/serviceObservability web/src/pages/ServiceObservabilityWorkspace.tsx
```

人工检查 diff 中不得包含三个 hold 路径；第二、第三条 `rg` 命中必须逐条解释，不能机械认为全是问题。

**Codex 视觉验收**

在 `VITE_SERVICE_OBSERVABILITY_FIXTURE_MODE=true` 下复核 1536/1180/900/390 视口与五态，重点比较请求路径/依赖面板、内容密度、筛选恢复、focus URL、局部重试、键盘和等价表格；在 live API 模式复核 Unconfigured、Partial/Unknown、404/409/422/503，不允许网络错误被 fixture 掩盖。

**最终提交**

```text
docs(serviceobs): record phase 2 acceptance evidence
```

## 7. Phase 2 验收矩阵

### 功能

- [ ] application 与 platform 两类 registration 可 round-trip；旧 request 缺省兼容；
- [ ] invalid Profile 被 quarantine，合法 Profile active；
- [ ] 三个聚合 API schema-valid；
- [ ] live identity/topology/domain 来自 alert-system；
- [ ] 未接遥测只影响相应 signal/section，Health Unknown/Data Partial；
- [ ] Profile 缺失时 overview 可用、workspace 409；
- [ ] directory cache fresh/stale/miss/failed refresh 均符合真值表；
- [ ] fixture 五态完整保留，API 失败不 fallback fixture。

### 架构

- [ ] renderer/assembler 无 E2B 分支；
- [ ] 三页 Health/Data 共用服务端 evaluator；
- [ ] adapter 实现 ports，核心不依赖 store/vendor；
- [ ] existing `/api/v1/services` 不破坏；
- [ ] cache 不含 raw evidence/log/trace；
- [ ] 浏览器无凭据、查询模板或原始 tool descriptor 内部实现；
- [ ] Agent 与 Kubernetes/OTel 均未进入 diff。

### 质量

- [ ] contract verifier、Go unit/race/integration、vet、web lint/build/static verifier 全绿；
- [ ] cancellation、bounds、RBAC、404/409/422/503、Partial/Stale 覆盖；
- [ ] 无数据假绿、无过期绿值、Traffic/Rate 中性、proxy 明示；
- [ ] Phase 1 视觉和交互无退化；
- [ ] acceptance 文档记录命令、结果、SHA、已知限制和下一 Phase 输入。

## 8. Cursor Cloud Agent 交付格式

编码 Agent 每完成一个 Task 输出：

1. 目标；
2. 已完成文件与行为；
3. 测试命令和结果；
4. 仍存在的不确定性/风险；
5. 下一 Task。

最终交付必须给出：branch、exact SHA、完整 diff 文件清单、所有验证结果、未执行项目及原因。不得自行 merge main；不得修改 hold 路径。

## 9. 后续 Phase 输入

Phase 2 完成后，Phase 3 只需要替换/补充 ports：

- Prometheus-compatible semantic signal adapter；
- OpenObserve focus/log/trace/access adapter；
- EvidenceRef broker、RBAC/audit/limits/cancellation；
- UAT `sandbox_id` 只读 replay；
- 真实 metric/label/retention discovery 后发布 E2B Profile v1。

Agent 继续不实现。Phase 4 选择非 Kubernetes 的结构差异服务（首选 PostgreSQL）验证无核心分支接入。
