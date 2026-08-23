# Service Observability Phase 3 Batch B Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 在不连接生产数据源、不发布 E2B production SIP 的前提下，以 fixtures/`httptest`/PostgreSQL integration tests 完成 Prometheus-compatible、OpenObserve、Alias/observed-overlay 三组通用只读能力，并让来源执行严格来自原子激活的 `CompiledIntegration`。

**Architecture:** 保持 `API -> facade -> Context Assembler -> ObservationCoordinator -> source-specific ports`。`Profile` 继续是浏览器/Page View 契约；`CompiledIntegration` 是 adapter 的唯一运行时 binding 事实。Batch B 先串行收紧 compiled runtime plan、journey stage targets 与 topology identity mapping，再将 Prometheus、OpenObserve、Alias/overlay 分成互不改写同一文件的三个并行冲突域；主 Cursor Cloud Agent 统一 join、集成、提交与推送。

**Tech Stack:** Go 1.25、标准库 `net/http`/`encoding/json`/`net/url`/`crypto/sha256`、现有 `provider.Client`、PostgreSQL/pgx v5、`httptest`、现有 JSON Schema verifier；不新增 provider SDK，不新增前端依赖。

---

## 0. 执行基线与不可变边界

- 仓库：`iluoxw/alert-system`。
- 分支：`codex/service-observability-phase3-20260823`。
- Batch B 基线必须是：`096174c2c2136a9113c0393de5e52c774b2352a7`。
- `096174c` 已通过 Codex 全量门禁与 Grok exact-SHA review；Grok 结论为 `PASS / no P0-P2 / no PR`。
- 生产主落地是 alert-system；E2B 是第一个一等 Service Profile，不是核心分支。
- fixtures 优先。所有 adapter 测试只使用合成 fixture、`httptest` 与隔离 PostgreSQL；本 Batch 不读取 `OBS_*`，不访问用户 `~/.zshrc`，不连接真实 Prometheus/OpenObserve。
- Agent 暂缓；不新增 Agent API、表、worker、UI 或 prompt。
- Kubernetes/OTel 保持冻结。以下路径必须相对基线零差异：
  - `deploy/observability/`
  - `docs/adr/2026-08-13-otel-prometheus-dual-write-demo.md`
  - `docs/plans/2026-08-13-e2b-otel-prometheus-remote-write-demo.md`
- 不修改 `web/`、`internal/api/`、`cmd/` 或 migration 序列；Task 8 才做 runtime/API/Page View 接线，Task 9 才做 discovery attestation 与 production publish。
- 缺失、失败、未授权或过期只在最小 binding/stage/relation/evidence 范围降级。Empty 是成功的零结果，不是 0、Healthy、Stale 或查询失败。

### 开始前检查

```bash
git status --short --branch
test "$(git rev-parse HEAD)" = "096174c2c2136a9113c0393de5e52c774b2352a7"
test "$(git rev-parse origin/codex/service-observability-phase3-20260823)" = "096174c2c2136a9113c0393de5e52c774b2352a7"
test -z "$(git status --short -- \
  deploy/observability \
  docs/adr/2026-08-13-otel-prometheus-dual-write-demo.md \
  docs/plans/2026-08-13-e2b-otel-prometheus-remote-write-demo.md)"
test -z "$(git diff --name-only 096174c2c2136a9113c0393de5e52c774b2352a7..HEAD -- \
  deploy/observability \
  docs/adr/2026-08-13-otel-prometheus-dual-write-demo.md \
  docs/plans/2026-08-13-e2b-otel-prometheus-remote-write-demo.md)"
```

预期：worktree clean；本地与远端均为同一 SHA；Cloud worktree 没有创建或修改三个 hold path。

三个 hold path 是原始本地 checkout 的未跟踪用户文件，普通 branch diff 无法证明它们未被改写。Codex
在原始 checkout `/Users/luoxiaowei/code/go/src/alert-system` 的只读基线摘要为：

```text
f2551f13455b9b5f20f6838cf053bd9019668bf52e35937b1fca4e3e685b0911  deploy/observability/uat-otel-prometheus-demo/otel-collector-configmap.yaml
e92f42686d77e36e0f231ae87be2c82dd0485e51577e8ba95b93db4b33656fed  deploy/observability/uat-otel-prometheus-demo/otel-red-writer-configmap.yaml
81ae2e1a51acc4b23ed9cba915c314a5eb723e5b56896657034e9cf241d360bf  deploy/observability/uat-otel-prometheus-demo/otel-red-writer-deployment.yaml
ec9a174943c104682639322dd849e677c76c477c2932415eedd20c3834da5ccd  deploy/observability/uat-otel-prometheus-demo/otel-red-writer-service.yaml
cf1df9b08543e55408ce35744f1752ac56a814dc14cec9fa6fdf28c5fde59168  deploy/observability/uat-otel-prometheus-demo/prometheus-scrape-job.yaml
e4d85321b15ad5403daa1f556c41cf89801d681d82a5f7acbfd07f10b0417026  deploy/observability/uat-otel-prometheus-demo/README.md
e14a0713a513eae84245f111426f818ace83670dda5f9260d3ea5d04a7e6ee83  deploy/observability/uat-otel-prometheus-demo/vector-configmap.yaml
19fc2dd89fd5ad70da01524d6ca177c38694dc0f3e6ffbb3ec35f29d6bfe3bac  docs/adr/2026-08-13-otel-prometheus-dual-write-demo.md
f6aea7d60458f7551113250576fa74582763559bc117c710089cb3d11d3348ff  docs/plans/2026-08-13-e2b-otel-prometheus-remote-write-demo.md
```

Cursor Cloud 无权也无需复现这些未跟踪文件；Batch B 完成后由 Codex 在原始 checkout 重新计算并逐项
比对，不能用“无匹配 pathspec 的 git diff 返回 0”作为未触碰证据。

## 1. Batch B 必须先固定的设计裁决

### 1.1 Runtime binding source 是唯一执行事实

新增 server-only `integration.RuntimeBindingSource`：按本次 `service_id + environment + Profile`
返回已经通过 `ValidateBundle`、且实际 Profile digest 再次匹配的不可变 `CompiledIntegration` 副本：

```go
type RuntimeBindingSource interface {
    ResolveCompiled(
        context.Context, string, string, model.Profile,
    ) (CompiledIntegration, error)
}
```

接口不得接受 SIP 原文、endpoint、query 或 secret；不能只按 profile key/version 命中后跳过 digest、
service/environment 与 bundle identity 交叉核对。

Coordinator 与每个 adapter 都只按这个精确 identity 解析 bindings：

- Coordinator 只解析一次，用它决定本次是否执行 signals/traces/logs/access；
- Coordinator 把该次快照中已经选中的 `[]integration.CompiledBinding` 放入内部
  `SignalRequest.Bindings` / `FocusRequest.Bindings`，adapter 不再二次 lookup；
- source-specific ports 可以依赖 server-only integration binding，但 `ObservationRequest`、Profile、
  context/view/api/web 仍不得持有或序列化 CompiledIntegration；
- adapter 只从请求 bindings 取得 `template_ref + typed params + source_key + permission`；
- `Profile.Views[].renderer` 只决定展示，不再决定 provider 执行；
- 没有对应 binding 的 source 是 **not requested**，不是 `*_not_connected`；
- resolver 返回的 identity 与请求 Profile 不一致时 fail closed，绝不回退到 renderer 或 SIP。

当前 source 尚无生产 loader 是有意的：Task 8/9 再把原子 bundle registry 接入。Batch B 用只读
static fake source 测试；不增加临时文件 fallback。一次 Coordinator 调用内所有 adapters 必须共享同一
份 cloned binding snapshot，避免激活切换时出现两次 lookup 的 TOCTOU 混版。

### 1.2 Journey targets 必须保留 stage 结构

当前 `CompiledBinding.ResolvedTargetIDs` 把所有 journey stage 的 entity/relation ID 扁平合并，adapter
无法知道一个观测应挂到哪个 stage。改为：

```go
type CompiledJourneyStageTarget struct {
    StageKey    string   `json:"stage_key"`
    EntityIDs   []string `json:"entity_ids"`
    RelationIDs []string `json:"relation_ids"`
}
```

- signal binding 继续使用 `ResolvedTargetIDs`；
- journey binding 的 `ResolvedTargetIDs` 必须为空，改用稳定排序的 `ResolvedJourneyStages`；
- focus/evidence 两者都为空；
- 不按 ID 类型猜 entity/relation，不靠 Profile label 反推 stage。

### 1.3 Stage mapping 是严格字面映射，不是自由表达式

为 trace/journey 模板增加封闭参数：

```go
type StageValueMapping struct {
    StageKey string   `json:"stage_key"`
    Values   []string `json:"values"`
}

type StatusValueMapping struct {
    State  string   `json:"state"` // ok|failed|pending；不允许 skipped
    Values []string `json:"values"`
}
```

- `stage_field`、`stage_mappings`、`status_field`、`status_mappings` 都是受 schema 限长的字段/字面值；
- adapter 对字面值做 SQL literal escaping，只生成等值判断；不接受 regex、SQL 片段或 matcher；
- stage key 必须命中该 journey 的已声明 stage，重复 value 或重复 stage mapping 拒绝编译；
- 没有经过 discovery/owner 确认的 mapping 时，focus/evidence 仍可返回，但每个 stage 只返回局部
  `unknown/stage_mapping_unavailable`；不能用 stage label、数组顺序或 operation substring 猜测；
- 本阶段仍不生成 `skipped`。

E2B 的 OpenObserve 最近样本已过期，且 operation→stage 值尚未确认；draft package 不补造映射，production
publish gate 继续关闭。完整/失败 journey 由合成 fixtures 验证。

### 1.4 Topology identity mapping 必须显式表达方向与实例角色

把无法一一对应的 `identity_fields[] + alias_namespaces[] + instance_role` 收敛为：

```go
type IdentityMapping struct {
    Field     string `json:"field"`
    Namespace string `json:"namespace"`
    Role      string `json:"role"` // caller|callee|caller_instance|callee_instance
}
```

- `caller -> callee` 决定 observed relation 方向；
- `caller_instance`/`callee_instance` 只进入 instance inventory，不自动成为逻辑 component；
- namespace 必须同时属于 topology policy allowlist；不同 namespace 永不静默合并；
- adapter 只能选择 mapping 声明的字段；未知字段不查询、不输出；
- instance 增减只改变 inventory count/summary，不改写 declared component/dependency；
- draft E2B mapping 可以继续存在，但不能充当 production discovery/owner attestation。

Instance side output 的内部形状固定为：

```go
type ObservedInstanceHint struct {
    Component   ExternalIdentity
    Instance    ExternalIdentity
    ObservedAt  time.Time
    EvidenceIDs []string
    SourceKey   string
}
```

它只表达“本窗口看到某 component 的某 instance”，不表达 canonical node 已存在或健康。

### 1.5 Context-local overlay 永不写 canonical graph

Alias resolver 只批量读取既有 `entity_aliases`；reconciler 是纯函数：

- 0/1/2+ enabled claims => not-found/resolved/conflict；
- 两端唯一 resolved 后才产生 observed relation；
- 已有 canonical relation 时保留真实 relation ID，provenance 合并为
  `declared_and_observed`；
- observed-only 使用 `<128` 字符的 `obs_<sha256-prefix>`，hash 输入包含 service/profile/version、
  canonical endpoints 与 kind；
- 不执行 `INSERT/UPDATE/DELETE dependency_edges`；
- TTL 过期只让对应 overlay Stale/Unknown，declared 底图和 domain boundary 不变；
- 未解析 identity 只产出不含原值的 issue/count，不建 node/relation、不显示 Healthy。

## 2. Cursor Cloud 并发拓扑与所有权

只使用固定会话：

```text
https://cursor.com/agents/bc-5a993b86-e20a-46b1-b033-028118eced93
```

### Review B0：Fable 5 High（只审计划）

审查本文件与 ADR-0007/Phase 3 主计划，不写代码。必须明确回答：

1. runtime source planning 是否完全来自 CompiledIntegration；
2. stage mapping 与 identity mapping 是否仍允许自由查询、E2B 特判或隐式猜测；
3. Empty/Stale/Forbidden/partial 的最小降级是否可实现；
4. 三个并行 lane 是否有共享文件冲突；
5. alias/overlay 是否可能写 canonical graph 或扩大 domain boundary；
6. 是否误触 Agent/Kubernetes/OTel/production publish。

有 P0/P1 必须先最小修订本计划并再次做 delta review；没有 P0/P1 才切换 Opus 5 High。

### Opus 5 High 编码并发

主 Agent 先独占完成 B0 shared slice 并跑门禁。B0 commit 完成后才同时启动三个子任务：

| Lane | 独占文件域 | 禁止改写 |
|---|---|---|
| P — Prometheus | `internal/serviceobs/adapters/prometheus/**` | integration/ports/coordinator/provider/docs/store |
| O — OpenObserve | `internal/serviceobs/adapters/openobserve/**` | integration/ports/coordinator/provider/docs/store |
| T — Alias/overlay | `internal/store/service_observability_aliases.go`、`internal/serviceobs/adapters/alertsystem/aliases.go`、`internal/serviceobs/reconcile/**` | integration/ports/coordinator/provider/docs |

并发规则：

- 子任务不得 commit、push、改 branch 或编辑共享文件；
- 每个子任务只报告 diff、测试命令与真实结果；
- 主 Agent join 后逐文件审查，再串行处理共享 glue/docs；
- 主 Agent 统一 `gofmt`、完整门禁、checkpoint commits 与最终一次 push；
- 任一 lane 发现必须修改共享契约，立即停止该处实现并交还主 Agent；禁止复制一份平行类型绕开冲突。

## 3. Task B0 — shared runtime contract（串行，先于并发）

### B0.1 先写失败测试：runtime source plan 不看 renderer

**Files:**

- Create: `internal/serviceobs/integration/runtime.go`
- Create: `internal/serviceobs/integration/runtime_test.go`
- Create: `internal/serviceobs/observe/planner.go`
- Create: `internal/serviceobs/observe/planner_test.go`
- Modify: `internal/serviceobs/observe/coordinator.go`
- Modify: `internal/serviceobs/observe/coordinator_test.go`

**Step 1: 写失败测试**

覆盖：

- Profile 有 evidence renderer、compiled 只有 traces+logs 时，access 不在 plan；
- Profile 无 evidence renderer、compiled 有匹配 focus 的 traces binding 时，focused workspace 仍计划 traces；
- directory/overview 即使 compiled 有 evidence binding 也只计划 requested signal；
- focused workspace 没有 focus 时不 fan-out evidence；
- focus kind A 不能触发只绑定 kind B 的 evidence；
- requested signal keys 只选中对应 compiled signal bindings；
- source key/capability/permission/identity 错配 fail closed；
- no providers 保持现有 `telemetry_not_connected` 精确语义；partial provider + 无 binding 不伪造 not-connected。

```bash
env -u DB_DSN go test ./internal/serviceobs/integration ./internal/serviceobs/observe -run 'Runtime|PlanSources|NoBinding' -count=1
```

预期：新增测试先失败。

**Step 2: 实现最小 runtime source 与 planner**

- `RuntimeBindingSource.ResolveCompiled` 返回深拷贝并校验实际 Profile digest；测试 fake 不暴露共享可变
  slice；
- Coordinator 在任何 provider 已配置时只解析一次 exact compiled integration；
- `planSources` 输入 purpose、focus、requested binding keys、Profile 与 compiled bindings；
- source plan 按稳定 source key 保存 selected `[]CompiledBinding`，而不是只有四个从 renderer 猜出来的
  布尔值；现有四个 reader 的 dispatch 可以继续用明确字段，但每个 request 必须携带该 source 的选中
  bindings；
- resolver/identity 失败作为 observation source failure 返回上层局部降级，不影响 registration/topology/domain。

**Step 3: 跑测试并提交 checkpoint**

```bash
gofmt -w internal/serviceobs/integration/runtime.go internal/serviceobs/integration/runtime_test.go \
  internal/serviceobs/observe/planner.go internal/serviceobs/observe/planner_test.go \
  internal/serviceobs/observe/coordinator.go internal/serviceobs/observe/coordinator_test.go
env -u DB_DSN go test -race ./internal/serviceobs/integration ./internal/serviceobs/observe -count=1
git diff --check
git add internal/serviceobs/integration internal/serviceobs/observe
git commit -m "refactor(serviceobs): plan runtime sources from compiled bindings"
```

### B0.2 按 stage 编译 journey targets

**Files:**

- Modify: `internal/serviceobs/integration/compiled.go`
- Modify: `internal/serviceobs/integration/compiled_test.go`
- Modify: `internal/serviceobs/integration/compiler_test.go`

**Step 1: 写失败测试**

- 一个 stage 同时有 entity/relation refs，编译后仍按 stage 分组；
- 同一 entity 出现在两个 stage 时不去重到失去 stage 归属；
- journey 的 `ResolvedTargetIDs` 为空；
- signal/focus/evidence 原语义不变；
- canonical marshal 两次字节与 digest 相同；strict bundle decode 仍拒绝未知字段。

**Step 2: 实现 `CompiledJourneyStageTarget` 与深拷贝/identity 校验**

数组一律非 nil、按 Profile stage 顺序输出；每个 ID 列表保持 selector 声明顺序且不得含空 ID。

**Step 3: 验证**

```bash
gofmt -w internal/serviceobs/integration/compiled.go \
  internal/serviceobs/integration/compiled_test.go internal/serviceobs/integration/compiler_test.go
env -u DB_DSN go test ./internal/serviceobs/integration -run 'Journey|Compiled|Bundle' -count=1
```

### B0.3 收紧 stage/status 与 topology identity mapping

**Files:**

- Modify: `internal/serviceobs/integration/model.go`
- Modify: `internal/serviceobs/integration/templates.go`
- Modify: `internal/serviceobs/integration/bindings.go`
- Modify: `internal/serviceobs/integration/compiler.go`
- Modify: `internal/serviceobs/integration/compiler_test.go`
- Modify: `internal/serviceobs/integration/security_test.go`
- Modify: `internal/serviceobs/integration/schemas/service-integration-package.schema.json`
- Modify: `docs/contracts/service-integration-package/v1/service-integration-package.schema.json`
- Modify: `docs/contracts/service-integration-package/v1/README.md`
- Modify: `docs/contracts/service-integration-package/v1/examples/e2b.package.json`
- Modify: `docs/contracts/service-integration-package/v1/examples/postgresql.package.json`

**Step 1: 先写 schema/model/compiler 负例**

拒绝：

- unknown stage key、空 values、重复 stage value；
- `skipped` 或未知 status state；
- status mappings 没有 status field、stage mappings 没有 stage field；
- regex/SQL 控制字符/超长 mapping value；
- identity mapping 的未知 role、重复 field+role、namespace 不在 allowlist；
- caller/callee 缺任一端却声称能产 topology hints；
- legacy `identity_fields`/`instance_role` 作为 unknown fields 被 strict schema 拒绝。

**Step 2: 实现封闭 slots 与 compiled mapping**

- `trace_by_focus.v1` 可选接收 stage/status mappings；
- mappings 只允许 exact literals；compiler 对 target journey 做 stage key 交叉校验；
- `CompiledTopologyPolicy` 保存完整 `IdentityMappings`，不再保存两组无对应关系的数组；
- E2B package 保持 `draft`。未确认 stage values 不填写，不把历史 stale 数据写成生产映射；
- PostgreSQL package 验证平台通用性，不得被迫携带 E2B 字段。

**Step 3: 同步两份 schema 并验证**

```bash
cmp -s internal/serviceobs/integration/schemas/service-integration-package.schema.json \
  docs/contracts/service-integration-package/v1/service-integration-package.schema.json
python3 docs/contracts/service-observability/v1/verify_examples.py
env -u DB_DSN go test ./internal/serviceobs/integration -count=1
git diff --check
```

### B0.4 扩展 provider JSON request 与内部事实类型

**Files:**

- Modify: `internal/serviceobs/provider/http.go`
- Modify: `internal/serviceobs/provider/http_test.go`
- Modify: `internal/serviceobs/observation_ports.go`
- Modify: `internal/serviceobs/ports.go`
- Modify: `internal/serviceobs/subject_parity_test.go`

**Step 1: `PostJSON` TDD**

- request body 来自 `json.Marshal` 后的 bytes；Content-Type 固定；
- context cancellation、redirect、response cap、header 注入与 URL/error redaction 复用 `do`；
- 不新增任意 method/path/header API；
- 错误文本不包含 JSON body、focus、org、stream 或完整 URL。

**Step 2: 增加最小通用事实**

- 稳定 reason：`no_series`、`stage_mapping_unavailable`；
- `ObservedInstanceHint` 与 `ObservationFacts.InstanceHints`；
- Alias resolved entry 带投影 context-local node 所需的 canonical entity metadata；
- `SignalRequest.Bindings` / `FocusRequest.Bindings` 只接收 Coordinator 从同一 compiled snapshot 选中的
  server-only bindings；adapter 必须再次检查 source key/kind，遇到错配 fail closed；
- `SortIssues`/normalize 对新增数组保持稳定、非 nil；
- 不改变浏览器 schema。

**Step 3: B0 门禁与 checkpoint**

```bash
gofmt -w internal/serviceobs/provider internal/serviceobs/observation_ports.go \
  internal/serviceobs/ports.go internal/serviceobs/subject_parity_test.go \
  internal/serviceobs/integration internal/serviceobs/observe
git diff --check
python3 docs/contracts/service-observability/v1/verify_examples.py
env -u DB_DSN go test -race ./internal/serviceobs/provider ./internal/serviceobs/integration \
  ./internal/serviceobs/observe ./internal/serviceobs -count=1
go vet ./internal/serviceobs/...
git add internal/serviceobs docs/contracts/service-integration-package/v1
git commit -m "feat(serviceobs): define trusted adapter execution contracts"
```

只有以上命令全部通过才启动 P/O/T 三个并行 lane。

## 4. Lane P — Prometheus-compatible semantic adapter

**Files:**

- Create: `internal/serviceobs/adapters/prometheus/adapter.go`
- Create: `internal/serviceobs/adapters/prometheus/errors.go`
- Create: `internal/serviceobs/adapters/prometheus/promql.go`
- Create: `internal/serviceobs/adapters/prometheus/response.go`
- Create: `internal/serviceobs/adapters/prometheus/discovery.go`
- Create: `internal/serviceobs/adapters/prometheus/adapter_test.go`
- Create: `internal/serviceobs/adapters/prometheus/promql_test.go`
- Create: `internal/serviceobs/adapters/prometheus/response_test.go`
- Create: `internal/serviceobs/adapters/prometheus/discovery_test.go`
- Create synthetic JSON under `internal/serviceobs/adapters/prometheus/testdata/`

### P1. PromQL builder 先测后写

测试固定模板的 exact string/AST：

- `counter_rate.v1`；
- `counter_ratio_percent.v1`，common/numerator/denominator labels 分开；
- `histogram_quantile_rate.v1`；
- `gauge_max.v1`；
- `gauge_ratio_percent.v1` 生成 Total/Free 两个独立查询，比例在本地逐成员计算。

同时拒绝 metric/label/group-by 注入、重复 label、动态 focus label、退化 ratio 与超长范围。实现只能从
`integration.CompiledBinding` 的 typed params 生成 PromQL；adapter API 不接受 raw query。

```bash
env -u DB_DSN go test ./internal/serviceobs/adapters/prometheus -run 'PromQL|Template|Injection' -count=1
```

### P2. 严格解析官方 envelope

覆盖 `status/data/resultType/result/warnings/errorType/error`：

- scalar/vector/matrix；
- quoted float 与 sample timestamp；
- warning 保留为受限内部 reason，不回显 response body；
- string/未知 resultType、native histogram（当前模板不支持）、NaN/±Inf、malformed、重复/超基数拒绝；
- series 顺序不可信，adapter 自己稳定排序。

HTTP 401/403 -> `observe.ErrUpstreamAuthFailed`；429 -> rate limited；5xx -> read failed；body/row bound ->
typed bounds error。网络失败/HTTP000 永不映射成 auth、Forbidden、Empty 或 Stale。

### P3. 每 binding 独立执行与最小降级

- directory summary 只 instant query requested bindings；overview/focused workspace 才按有界 window 追加
  `query_range` sparkline；
- 所有请求使用 POST form；`limit` 只有显式 capability manifest 表示支持时发送，本地 MaxSeries 永远检查；
- exact duplicate template+params 在一次 request 内只执行一次，再投影到多个 binding；
- 一个 binding 422/503/malformed 只让该 binding Unknown；其它 binding 值保留；
- 全部失败时 source unavailable，部分失败时 source unknown/Partial；
- query 成功零 series：对应 signal `unknown/no_series`，source 仍表示查询成功；全部 binding 零 series 时
  source Empty。不得填 0、不得填当前 observed_at、不得改判 Stale；
- observed_at 取真实 sample timestamp；matrix sparkline 取真实样本，不补点。

### P4. HugePages golden behavior

按 group labels 对 Total/Free 做 exact join：

- 每个 `Total <= 0` 成员局部 Unknown；
- 缺 Total/Free 的成员局部 Unknown；
- 有效成员计算 `(Total-Free)/Total*100`；
- worst-of 只在有效成员取 max；
- 有未知成员时保留有效最坏值，但 source/binding coverage 显式 partial，不把未知节点当 0%；
- 所有成员无效时 signal Unknown；
- Traffic/Rate 没阈值仍由 evaluator 判 neutral；adapter 不写 Healthy。

### P5. 只读 discovery manifest

使用官方 `/api/v1/metadata`、`/api/v1/series`、`/api/v1/labels`/label values 的 bounded 请求，输出：

- metric name/type/unit；
- label names 与 series cardinality 摘要；
- approximate time bounds 的明确标记；
- capability flags（含 query `limit` 是否已证实）；
- canonical JSON digest。

不得保存 sample values、endpoint path、auth、完整 selector 或任意 query。默认 manifest 只用于 Task 9 gate，
本 Batch 不发布 SIP。

### P6. Lane P 自检（不 commit）

```bash
gofmt -w internal/serviceobs/adapters/prometheus
git diff --check -- internal/serviceobs/adapters/prometheus
env -u DB_DSN go test -race ./internal/serviceobs/adapters/prometheus -count=1
go vet ./internal/serviceobs/adapters/prometheus
```

## 5. Lane O — OpenObserve focus/log/trace/access adapter

**Files:**

- Create: `internal/serviceobs/adapters/openobserve/adapter.go`
- Create: `internal/serviceobs/adapters/openobserve/errors.go`
- Create: `internal/serviceobs/adapters/openobserve/query.go`
- Create: `internal/serviceobs/adapters/openobserve/response.go`
- Create: `internal/serviceobs/adapters/openobserve/evidence.go`
- Create: `internal/serviceobs/adapters/openobserve/discovery.go`
- Create: corresponding `_test.go` files
- Create synthetic JSON under `internal/serviceobs/adapters/openobserve/testdata/`

### O1. Identifier、literal 与 JSON request TDD

- organization/stream/field 只接受明确 allowlist 与长度上限；path segment 安全编码；
- SQL 只由固定 `SELECT allowlisted_fields FROM validated_stream WHERE validated_focus_field = literal`
  模板生成；单引号按 SQL literal 规则成对转义；
- `sandbox_id` 是一个普通 Profile focus field，不在核心写 E2B 分支；
- `sandboxid` 不会被 case folding、substring 或 fallback 接受；
- absolute `start_time/end_time` 使用服务端 `time.Time` 转 UTC 微秒；`from=0`、`size=min(binding,
  profile,global)`；不切分无界窗口；
- body 通过 `json.Marshal` + `provider.Client.PostJSON`；错误不回显 SQL/body/focus/org/stream。

```bash
env -u DB_DSN go test ./internal/serviceobs/adapters/openobserve -run 'Query|Identifier|Literal|Injection' -count=1
```

### O2. Search/trace response 与状态映射

- logs/traces/access 按 compiled source key 分开选 binding、调用、limits 与 source status；
- 只解析 allowlisted selected fields、`_timestamp` 与受控 record/trace/span identity；额外 raw attributes 丢弃；
- zero hits -> FocusEmpty/section Empty；不写当前 observed_at；
- 401/403 仅在收到 HTTP response 后映射 upstream auth failure；用户 Forbidden 由 Coordinator 在调用前处理，
  upstream call count 必须为 0；
- 429/timeout/cancel/5xx/parse/oversize/row bound 使用稳定 typed mapping；
- parent cancellation 返回 context error并断开 HTTP，不包装成 Partial；
- 同 source 的一条 binding 失败不丢弃其它独立 binding 已确认 facts；全局 auth failure 除外。

### O3. EvidenceRef 只含摘要与内部 locator

- log/access 不保存正文、request/response body、header 或任意 raw attribute；
- trace 不保存完整 span attributes；
- summary 只由固定模板生成，包含类型、时间、状态与 hash，不拼接 provider message；
- record ID 缺失时，用 stream/type/timestamp/受控 identity 的 canonical tuple 生成 digest；不 hash 原始正文；
- locator 只有 provider/dataset/record-or-trace id/query_template_ref，没有上游 URL；deep link 留给 Task 7/8 broker；
- 错误、audit fake 与 `slog` 测试中不出现 focus value、Authorization、SQL 或 fixture raw payload。

### O4. Journey、topology 与 inventory side outputs

- stage field/value 仅按 compiled `StageValueMappings` exact match；status exact mapping只产
  ok/failed/pending；缺 mapping/未知 value 只把对应 stage Unknown；
- 没有事实不推导 skipped；focus empty 时 journey section Empty，不造 progress；
- stage evidence IDs 必须引用本次结果里的 EvidenceRef；
- caller/callee/caller_instance/callee_instance 只从 compiled `IdentityMappings` 取字段；
- caller→callee 产生 `ObservedTopologyHint`；instance 产生 `ObservedInstanceHint`，不直接建 node/relation；
- 同一 trace binding 同时投影 focus、journey、trace evidence、topology/inventory，只发一次 exact query；
- 两条逐字节相同 binding 也按 execution fingerprint 去重，不因多个展示位重复上游调用。

### O5. Stream discovery manifest

使用官方 stream list/schema/settings/stats 的 bounded 只读 endpoint，记录：

- stream name/type；field name/type；min/max observed time；
- retention 是 explicit/unknown，不从“只看到 1–2 天数据”猜策略；
- required focus/stage/identity fields 的 exact presence；
- content digest 与 capability flags。

不保存字段值、样本、正文、endpoint、organization、用户名或凭据。E2B 当前 access 无 exact
`sandbox_id`、journey mapping/retention 未确认，manifest 必须让 production gate 保持 blocked。

### O6. Lane O 自检（不 commit）

```bash
gofmt -w internal/serviceobs/adapters/openobserve
git diff --check -- internal/serviceobs/adapters/openobserve
env -u DB_DSN go test -race ./internal/serviceobs/adapters/openobserve -count=1
go vet ./internal/serviceobs/adapters/openobserve
```

## 6. Lane T — Batch alias resolver、inventory 与 observed overlay

**Files:**

- Create: `internal/store/service_observability_aliases.go`
- Modify: `internal/store/service_observability_integration_test.go`
- Create: `internal/serviceobs/adapters/alertsystem/aliases.go`
- Modify: `internal/serviceobs/adapters/alertsystem/adapters_test.go`
- Create: `internal/serviceobs/reconcile/overlay.go`
- Create: `internal/serviceobs/reconcile/overlay_test.go`

### T1. PostgreSQL batch alias lookup

先写 `//go:build integration` 测试：

- 输入去重后用一次 query、同一 read transaction 查询所有 identities；
- 0/1/2+ enabled rows 分别返回 not-found/resolved/conflict；disabled claim 不计；
- duplicate input 只返回一个稳定 entry；
- canonical metadata（真实 ID/entity key/name/layer/kind/environment）随 resolved entry 返回；
- 达到 identities 上界显式 bounds error，空输入不查库；
- 不逐 identity N+1；测试 runner 记录 query count；
- 不修改 `entity_aliases` 或 `dependency_edges`。

实现优先使用 `unnest($1::text[], $2::text[]) WITH ORDINALITY` + left join/count 的单条静态 SQL；不拼
namespace/value。

```bash
env -u DB_DSN go test -tags=integration ./internal/store -run 'ServiceObservability.*Alias' -count=1
```

### T2. Alias adapter 与 namespace gate

- `AliasAdapter` 实现 `serviceobs.AliasResolver`，通过现有 `ReadRunner.WithRead` 保持单事务；
- trim/长度/总数上界在进 DB 前检查；
- namespace allowlist 由 reconciler 从 compiled topology policy传入筛选，adapter 不维护 E2B 静态清单；
- issue 只包含 namespace、reason、count、source key；不包含 alias value；
- 同名但不同 namespace (`dc`/`datacenter`/`zone`/`environment`) 永不合并。

### T3. Pure overlay reconciliation

表驱动测试覆盖 declared-only、observed-only、both、stale、not-found、conflict、bounds、direction：

- declared+observed 保留 declared real relation ID 与 merged IDs；
- observed-only 生成 deterministic `obs_...`，相同输入相同 ID，service/profile/endpoints/kind 任一变化 ID
  必变且长度 <128；
- overlay node 使用 resolver 返回的 canonical metadata，不创建 synthetic canonical ID；
- 任一端 unresolved/conflict 不建 edge；
- caller→callee 不反转；
- stale hint 只让该 overlay stale；
- topology source 失败/overlay bounds 不改变 domain boundary；
- 两个 service/profile 的 hint 不能串线；
- 函数无 store dependency、无写接口。

### T4. Frozen 15 / observed 17 inventory fixture

构造 E2B 风格但使用通用类型的 synthetic fixture：

- declared logical components 固定；
- frozen inventory 15，observed 17，部分重合；
- resolved instance 正确增删并按 component 汇总；
- 未知两个 alias 只增加 `alias_not_found` count，不创建 canonical entity；
- instance 数量变化不创建/删除 logical component 或 dependency；
- 不把 unknown instance 计成 Healthy 或 0% utilization。

### T5. Lane T 自检（不 commit）

```bash
gofmt -w internal/store/service_observability_aliases.go \
  internal/store/service_observability_integration_test.go \
  internal/serviceobs/adapters/alertsystem/aliases.go \
  internal/serviceobs/adapters/alertsystem/adapters_test.go \
  internal/serviceobs/reconcile
git diff --check -- internal/store/service_observability_aliases.go \
  internal/store/service_observability_integration_test.go \
  internal/serviceobs/adapters/alertsystem internal/serviceobs/reconcile
env -u DB_DSN go test -race ./internal/serviceobs/adapters/alertsystem ./internal/serviceobs/reconcile -count=1
env -u DB_DSN go test -tags=integration ./internal/store -run 'ServiceObservability.*Alias' -count=1
```

## 7. Task B4 — 主 Agent join、共享 glue 与验收

### B4.1 独立审查三个 lane diff

逐项检查：

- 子任务只改自己的文件域；
- 无 raw query/endpoint/secret/SIP 读取；
- 无 `if service == e2b`、`sandboxid` fallback 或 Profile renderer source planning；
- 所有 slice 非 nil、输出稳定排序；
- adapter error 不包含 response body/SQL/PromQL/focus/org/stream；
- context cancellation 与 goroutine join 有测试；
- `dependency_edges` 只有 SELECT/测试断言，没有 observed overlay 写入。

### B4.2 补跨 lane integration tests

**Files:**

- Modify: `internal/serviceobs/observe/coordinator_test.go`
- Modify: `internal/serviceobs/integration/compiled_test.go`
- Create: `internal/serviceobs/adapters/adapters_integration_test.go`（仅需要时）
- Modify: `docs/adr/0007-declarative-service-integrations-and-governed-observed-topology.md`
- Modify: `docs/plans/2026-08-23-service-observability-phase3-production-readonly-telemetry.md`
- Modify: `docs/contracts/service-integration-package/v1/README.md`

必须覆盖：

1. compiled 只有 Prometheus+traces+logs：access reader 即使注册也调用 0 次；
2. Prometheus 一 binding 503 + 其它 success：只缺失 binding Unknown，source Partial；
3. traces matched + logs Forbidden：Focus 仍 matched，logs 上游调用 0；
4. traces empty + logs timeout：Focus Unknown，不宣称 Empty；
5. stale trace observation：journey/topology/evidence 只在自己的 scope Stale/Unknown，declared topology 保留；
6. access compiled binding 存在但 exact focus field discovery 缺失：access 局部 Unknown，不能 fallback；
7. 同 trace binding 的 journey/evidence/topology side outputs只有一次 HTTP；
8. second-service synthetic compiled integration 使用完全相同 runtime，无 E2B branch；
9. overlay reconciliation 不改变 domain reader seeds/boundary；
10. `ObservationFacts.ReadAt` 只透传真实 adapter read time；zero/failed 不填 wall clock observed_at。

### B4.3 文档事实同步

- 记录 adapter 支持已完成不等于 production connected/published；
- 保留 2026-08-23 实测事实：HTTPS/auth/schema 可用，但当前数据约 52.4h stale，access focus、journey
  mapping、retention、fresh current data 仍 blocked；
- E2B SIP 继续 draft；fixtures 继续是产品验收事实，live discovery 只作为发布 gate；
- 明确 topology 是 declared skeleton + context-local observed overlay + dynamic instance inventory；
- 明确后续 Agent-as-a-Service 方向，但本 Phase 零实现。

### B4.4 主 Agent checkpoints

```bash
git add internal/serviceobs/adapters/prometheus
git commit -m "feat(serviceobs): add prometheus semantic signal adapter"

git add internal/serviceobs/adapters/openobserve
git commit -m "feat(serviceobs): add openobserve evidence adapter"

git add internal/store/service_observability_aliases.go \
  internal/store/service_observability_integration_test.go \
  internal/serviceobs/adapters/alertsystem internal/serviceobs/reconcile
git commit -m "feat(serviceobs): reconcile observed topology overlays"

git add internal/serviceobs docs/adr docs/plans docs/contracts/service-integration-package/v1
git commit -m "test(serviceobs): close phase3 batch b integration"
```

每个 commit 前先确认 staged diff 不含其它 lane 的未完成文件；如果实际改动不适合四个提交，允许主 Agent
合并 checkpoint，但最终远端只认完整门禁后的单一 tip。

## 8. 完整门禁

### 8.1 静态与契约

```bash
test -z "$(gofmt -l $(git diff --name-only 096174c2c2136a9113c0393de5e52c774b2352a7 -- '*.go'))"
git diff --check 096174c2c2136a9113c0393de5e52c774b2352a7..HEAD
cmp -s internal/serviceobs/integration/schemas/service-integration-package.schema.json \
  docs/contracts/service-integration-package/v1/service-integration-package.schema.json
python3 docs/contracts/service-observability/v1/verify_examples.py
go build ./...
go vet ./...
go vet -tags=integration ./...
```

### 8.2 Focused race

```bash
env -u DB_DSN go test -race -count=1 \
  ./internal/serviceobs/integration \
  ./internal/serviceobs/provider \
  ./internal/serviceobs/observe \
  ./internal/serviceobs/adapters/prometheus \
  ./internal/serviceobs/adapters/openobserve \
  ./internal/serviceobs/adapters/alertsystem \
  ./internal/serviceobs/reconcile
```

### 8.3 Full suites

```bash
env -u DB_DSN go test -count=1 ./...
env -u DB_DSN go test -tags=integration -race -count=1 ./...
```

Docker 不可用时不得把 e2e 报成 PASS：记录环境阻塞，同时至少跑完所有不依赖 Docker 的 focused tests。
本机已有可用 Docker 时必须运行完整两条命令。

### 8.4 安全与边界 grep

```bash
rg -n 'OBS_ENDPOINT|OBS_AUTH|AUTH_USERNAME|Authorization:|BEGIN (RSA|OPENSSH)|raw_query|if .*e2b|service.*==.*e2b' \
  internal/serviceobs internal/config docs/contracts/service-integration-package/v1
rg -n 'INSERT INTO dependency_edges|UPDATE dependency_edges|DELETE FROM dependency_edges' \
  internal/serviceobs/reconcile internal/serviceobs/adapters/openobserve \
  internal/serviceobs/adapters/alertsystem/aliases.go internal/store/service_observability_aliases.go
test -z "$(git status --short -- \
  deploy/observability \
  docs/adr/2026-08-13-otel-prometheus-dual-write-demo.md \
  docs/plans/2026-08-13-e2b-otel-prometheus-remote-write-demo.md)"
test -z "$(git diff --name-only 096174c2c2136a9113c0393de5e52c774b2352a7..HEAD -- \
  deploy/observability \
  docs/adr/2026-08-13-otel-prometheus-dual-write-demo.md \
  docs/plans/2026-08-13-e2b-otel-prometheus-remote-write-demo.md)"
git status --short
```

grep 命中受控 Authorization header 注入或测试负例时人工逐项解释；不能只凭“有输出”判失败，也不能忽略
真实 secret/alias/E2B branch 命中。最终 `git status` 必须 clean。

## 9. 远端候选与 Grok review

主 Agent 仅在完整门禁通过后 push：

```bash
git push origin HEAD:codex/service-observability-phase3-20260823
test "$(git rev-parse HEAD)" = "$(git rev-parse origin/codex/service-observability-phase3-20260823)"
```

报告 base SHA、final SHA、commit 列表、文件域、全部命令真实结果、未运行项、hold path 与 push 状态。

随后让 Grok bot `alert-system` 只 review：

```text
096174c2c2136a9113c0393de5e52c774b2352a7..FINAL_SHA
```

重点：query injection、auth/secret leakage、response/cardinality bounds、Empty/Stale/Forbidden、context
cancellation、compiled binding source planning、alias conflict、overlay canonical pollution、E2B special case。无问题
输出 PASS/no PR；确认问题最多提一个窄 PR，target 当前 Phase 3 branch，不 merge、不开始 Task 7。

## 10. Batch B 完成定义

- [ ] source plan 完全由 exact CompiledIntegration bindings 决定，renderer 不再触发 provider；
- [ ] journey compiled targets 按 stage 保留 entity/relation refs；
- [ ] stage/status 与 topology identity mappings 是封闭、可验证的 exact mappings；
- [ ] Prometheus adapter 覆盖 scalar/vector/matrix、warnings、Empty、Partial、bounds、P95/rate/ratio/
  HugePages；
- [ ] OpenObserve adapter 覆盖 focus/log/trace/access、EvidenceRef redaction、journey/topology/inventory side
  outputs 与所有失败映射；
- [ ] Alias resolver 单事务批量查询，overlay 只存在当前 Context，不写 canonical graph；
- [ ] frozen 15 / observed 17 fixture 证明 logical component 与 instance inventory 分离；
- [ ] E2B 与第二服务都走同一 adapter/compiler/runtime，没有核心特判；
- [ ] production E2B SIP 仍为 draft，当前 stale/live gaps 没被假装关闭；
- [ ] Agent、Kubernetes、OTel、web/API/migrations 零越界；
- [ ] Codex 全量门禁与 Grok exact-SHA review 均通过。

## 11. 官方接口依据

- Prometheus HTTP API：`https://prometheus.io/docs/prometheus/latest/querying/api/`
- OpenObserve Search API：`https://openobserve.ai/docs/reference/api/search/search/`
- OpenObserve Trace Search API：`https://openobserve.ai/docs/reference/api/traces/trace-search-api/`
- OpenObserve Stream API：`https://openobserve.ai/docs/reference/api/stream/`
