# Service Observability Phase 2 验收记录（Slice C：Task 8–11）

> 执行者：Cursor Cloud Agent / Claude Opus 5 High
>
> 分支：`codex/service-observability-phase2-20260823`
>
> 本文只记录**实际执行过的命令与真实结果**。未执行、执行失败或环境不支持的项目一律
> 在 §6 明确写出原因，不用"应当通过"代替"已经通过"。

## 1. SHA 基线

| 角色 | SHA |
|---|---|
| Slice C 派发基线（Slice B 验收后的远端 HEAD） | `a887bd5233b9582a3e55ba034530a7d8d707f749` |
| Task 8：`feat(api): expose service observability page views` | `deeb98e` |
| Task 9：`feat(app): assemble service observability read path` | `b64bba3` |
| Task 10：`feat(web): consume service observability page view APIs` | `1c2dfcc` |
| Task 11：`docs(serviceobs): record phase 2 acceptance evidence` | 本提交 |

整个 Phase 2 范围（含计划修订）的审计基线仍是计划 §2 记录的
`702726b90762a0c92e54ffeb7c7cea101b466c83`；上表的 `a887bd5` 是纯实现增量的审计基线。

Slice C 开工前的 preflight（分支、HEAD、worktree、远端）四项全部匹配 `a887bd5`，
过程中没有 reset / rebase / force push / 切分支。唯一一次 `git commit --amend` 发生在
Task 8 提交推送之前，只为移除一个被误加入的 18 MB 本地编译产物（`alert-system`
可执行文件）；该 blob 不在任何可达提交里（§5.3 有验证命令），同一提交同时把
`/alert-system` 加入 `.gitignore`。

## 2. Slice C 改动清单（`a887bd5..HEAD`，31 个文件）

### 后端

| 文件 | 变更 |
|---|---|
| `internal/store/service_observability.go` | 新增 keyset 分批 `ScanServiceIdentities`，与 `ListServiceIdentities` 共用同一条静态 SQL（过滤口径不可能漂移） |
| `internal/store/service_observability_integration_test.go` | 新增完整候选 keyset 遍历、过滤一致性与非法锚点的真库测试 |
| `internal/serviceobs/ports.go` | 新增 `CandidateScanner` 端口、`CandidateFilter/CandidateScanRequest/CandidateSet`、`ErrCandidateSetTooLarge`（包裹 `ErrScopeTooBroad`） |
| `internal/serviceobs/adapters/alertsystem/registration.go` | 实现 `ScanCandidates`：单只读快照内 keyset 走完全部候选，多取一行判定"严格超过上界"才失败 |
| `internal/serviceobs/view/directory.go` | 新增 `DirectorySort/DirectoryKey/DirectoryKeyLess/DirectoryKeyOfRow`；`ProjectDirectory` 按传入排序口径投影，不再静默重排 |
| `internal/serviceobs/view/projector.go` | 抽出 `LinksForIdentity`，让目录行与总览共用同一份入口投影 |
| `internal/serviceobs/facade/service.go` | 新增 facade：目录（完整候选摘要 + 单次缓存快照 + 游标分页）、总览、工作区、`RefreshSummary` |
| `internal/serviceobs/facade/cursor.go` | opaque base64url 游标：版本 + 查询指纹 + 稳定元组 + service ID，长度硬上限 512 |
| `internal/api/service_observability.go` | 三个只读端点、参数白名单、typed 错误到 404/409/422/503 的映射、actor 只从 session 取 |
| `internal/config/config.go` | 新增 `ServiceObservabilityConfig`（7 项），YAML/env/默认三层解析 + 从严校验 |
| `internal/app/service_observability.go` | 生产装配 + 启动日志 + 有界关闭 |
| `cmd/server/main.go` | 只注册 API 与关闭钩子 |
| `.env.example` | 7 项服务端配置 + 前端显式 fixture 开关 |

### 前端

| 文件 | 变更 |
|---|---|
| `web/src/serviceObservability/data.ts` | 唯一数据源选择点：双重门控、动态 import fixture、无 catch 回落、React Query key 含 service ID 与全部 server query |
| `web/src/api/client.ts` | 三条 typed 聚合 API + `ApiError.reasonCode` |
| `web/src/pages/ServiceDirectory.tsx` | 服务端筛选/排序/分页 + `next_cursor` 显式加载 + loading/422/503 状态 |
| `web/src/pages/ServiceOverview.tsx` | 总览 API + Unconfigured 横幅 + 404/422/503 状态 |
| `web/src/pages/ServiceObservabilityWorkspace.tsx` | 工作区 API + 409/404/422/503 状态 + live 重试即 refetch |
| `web/src/App.tsx` | 移除 fixture import，服务名改由 Page View 发布 |
| `web/src/serviceObservability/types.ts` | `focus` 可为 null（未聚焦）、抽出 `DirectorySummary` |
| `web/src/serviceObservability/workspaceState.ts`、`components/serviceObservability/JourneyRail.tsx` | 适配无 focus 状态，不发明 journey |
| `web/scripts/verify-service-observability.mjs` | 新增唯一选择点/双重门控/无回落/服务端分页/409-404-422/客户端无查询模板等断言 |

`git diff --name-only a887bd5..HEAD` 未包含 `deploy/`、Kubernetes/OTel 配置或两个 hold
文档（§5.1）。

## 3. 两条口径修正的落地方式

### 3.1 目录摘要覆盖完整候选集合，且每个候选只读一次缓存

- 摘要分母不再受页面 `limit` 或 adapter `DirectoryLimit` 影响：facade 调
  `CandidateScanner.ScanCandidates`，adapter 在**一个** REPEATABLE READ 只读事务内用
  `(tier, name, id)` keyset 分批走完 metadata（environment/team/tier/q）过滤后的全部
  候选。
- 安全上界是显式的：默认 `MaxCandidates=2000`。判定方式是**多取一行**——恰好等于上界
  被接受，只有严格超过才返回 `ErrCandidateSetTooLarge`。它包裹 `ErrScopeTooBroad`，
  因此 HTTP 层映射成可由收窄筛选解决的 **422 `scope_too_broad`**，而不是 503。
- 一次请求只建一份 request-local 快照：`for identity := range candidates { Lookup(...) }`
  之后，摘要与 `health`/`data_status` 结果筛选都从这份快照读，不触发第二轮刷新。
- 覆盖测试：`TestScanCandidatesWalksEveryCandidateInOneSnapshot`、
  `TestScanCandidatesAcceptsExactBoundAndFailsBeyondIt`（3 个子用例）、
  `TestDirectorySummaryCoversCompleteCandidateSetWithOneLookupPerCandidate`、
  `TestDirectoryResultFiltersDoNotChangeSummaryDenominatorOrRereadCache`。

### 3.2 游标按编码元组恢复，锚点行被删也能继续

- 游标载荷 = 版本 + 查询指纹 + 稳定排序元组 + service ID，base64url、`<=512` 字符。
- 恢复逻辑只做 `DirectoryKeyLess(cursorKey, boundedKey(candidate), sort)`：**不要求**在
  新算出的列表里找到游标那一行。锚点行在两页之间被删除时，同一个比较函数给出同一个
  插入位置，翻页继续而不是失效或从头开始。
- 指纹覆盖 environment/team_id/tier/q/health/data_status/sort/direction 全部维度；
  `limit` 刻意不参与（换页大小是合法操作）。解码失败、长度超限、版本不符、指纹不匹配
  一律在任何数据库查询之前返回 422。
- 覆盖测试：`TestDirectoryCursorSurvivesDeletedAnchorRow`、
  `TestDirectoryCursorPaginatesWithoutGapsOrDuplicates`、
  `TestDirectoryCursorRejectsMismatchedQueryAndMalformedInput`（10 个子用例）、
  `TestDirectoryExplicitSortDrivesPaginationOrder`。

### 3.3 facade 子包的偏移与理由

计划把 facade 写在 `internal/serviceobs/service.go`，但 ports 已经落在
`package serviceobs`（`internal/serviceobs/ports.go`），而 `context/view/cache/adapters`
全部 import 它。编排层必须 import `context`（装配）、`view`（投影）与 `cache`（目录摘要），
放进 `package serviceobs` 会形成真实 import 环 `serviceobs -> view -> serviceobs`。因此
facade 落在 `internal/serviceobs/facade`，依赖方向仍严格是
`api -> facade -> view/context -> ports`，对外可观测契约（三个 API、状态码、reason
code、Page View 形状）一个字节都没有放宽。

## 4. 执行过的门禁与真实结果

命令全部在本 VM 执行，输出留档在 `/opt/cursor/artifacts/`（`go-gates.log`、
`go-race.log`、`go-integration.log`、`web-gates.log`、`live-api-checks.log`、
`scope-guards.log`、`history-hygiene.log`）。

| 门禁 | 命令 | 结果 |
|---|---|---|
| 契约校验 | `python3 docs/contracts/service-observability/v1/verify_examples.py` | PASS（5 份 schema + 全部 fixture 与四个场景） |
| 构建 | `go build ./...` | PASS |
| vet | `go vet ./...` | PASS |
| vet（integration tag） | `go vet -tags=integration ./...` | PASS（新增真库测试可编译） |
| 单元全量 | `go test ./...` | 除 `test/e2e` 外全部 ok；`test/e2e` 因无 Docker 失败（§6.1） |
| race | `go test -race -count=1 ./internal/serviceobs/... ./internal/api ./internal/app ./internal/store ./internal/config ./cmd/server` | 全部 ok |
| integration | `go test -tags=integration ./... -race -count=1` | **未通过**：`internal/store` 与 `test/e2e` 都依赖 Docker（§6.1） |
| 前端 lint | `cd web && npx oxlint` | 零 findings |
| 静态验证器 | `cd web && npm run test:service-observability` | PASS（3 routes, 5 workspace states） |
| 前端构建 | `cd web && npm run build`（`tsc -b` + vite build） | PASS |

### 4.1 生产 bundle 不含 fixture

`data.ts` 改为在 fixture 分支里 `await import('./fixtures')` 之后重新构建，并在
`dist/assets/*.js` 里检索 `svc_e2b_fixture` / `sbx_hp16c3`：**零命中**（改造前
`data-*.js` 里有 15 处）。production build 因此既没有 fixture 数据，也没有可以进入
fixture 模式的代码路径。

### 4.2 守护检查（逐条解释，不机械当缺陷）

- hold 路径：`git diff --stat <base>..HEAD -- deploy/ docs/adr/2026-08-13-*.md docs/plans/2026-08-13-*.md`
  在 `a887bd5..HEAD` 与 `702726b..HEAD` 两个区间都输出 **0 行**；diff 文件名里也没有
  任何 `deploy/`、k8s、otel、collector 路径。
- `Agent|Assistant|prompt|session|memory|stream` 在非测试代码里共 16 处命中，逐条属于
  以下三类，没有一处是 Agent/模型/prompt 实现：
  1. **约束型注释**（10 处）：例如 `model/tool.go` 的"绝不携带 SQL/PromQL/凭据/prompt"、
     `api/service_observability.go` 的"actor 只来自服务端 session"；
  2. **既有登录会话**（2 处）：`authed.Use(r.sessionAuth())` 与 `Subject 是服务端从
     session 计算出的身份`——指的是 Phase 1 之前就有的 HTTP 登录会话，不是 Agent session；
  3. **子串误命中**（4 处）：`profile/validate.go` 的 `"prompt"` 是**禁用 key 黑名单**
     里的条目；`reasonUpstreamUnavailable` 与 `UpstreamFingerprint` 里的 `stream` 来自
     `upstream`。
- `promql|logql|select .* from|authorization:|api[_-]?key|secret` 在
  `web/src/serviceObservability` 与工作区页面：**零命中**。
- 通用核心里唯一的 `e2b` 字样是 `view/projector.go` 的注释"这里没有任何
  `if service == e2b` 分支"；`cache/store.go` 里唯一的 `any` 出现在"Summary 的字段图里
  没有 any、[]byte 或自由 map"这句注释里。

## 5. 真实运行验证（本地 PostgreSQL 16 + live API）

Docker 不可用，因此改用本机 PostgreSQL 16 集群做真实端到端验证：`initdb` 到
`/tmp/pgdata`（端口 5433）→ 顺序执行 `migrations/*.up.sql` 全部 24 组迁移 →
`cmd/seed-admin` 建管理员 → `cmd/seed-e2b-demo` 导入 9 个 canonical 节点/9 条 Alert/
3 个 Fault → 通过 **live 管理 API** 创建 7 个 Service Registration（platform 与
application 两类分类各有，全部 201）→ 启动 `go run ./cmd/server`。

### 5.1 启动日志（无敏感内容）

```text
INFO app: Service Observability 只读读路径已装配 profiles_published=0 profiles_non_published=0
  profiles_quarantined=0 profile_dir_configured=true cache_ttl=45s cache_max_stale=5m0s
  cache_max_entries=4096 refresh_workers=4 refresh_queue=256 max_window_seconds=86400
```

放入一份真实 UUID 的 published Profile 后重启：`profiles_published=1`。日志只有计数、
布尔与配置值，没有目录路径、文档正文或 service ID。

### 5.2 live API 结果（完整输出见 `live-api-checks.log`）

| 场景 | 实际结果 |
|---|---|
| 未登录 | `401` |
| 目录 `limit=2` | `200`，`summary.managed=7`（完整候选集合），页面 2 行，`next_cursor` 长 235 字符 |
| 用 `next_cursor` 翻完 | 4 页 / 7 行 / 7 unique（不重复不漏行），每页 `managed` 恒为 7 |
| `health=unknown` vs `health=critical` | items 7 vs 0，两次 `summary` **完全一致** |
| 旧游标 + 新排序 | `422 invalid_cursor` |
| `sort=cpu` / `direction=sideways` / `health=exploded` / `limit=201` / `team_id=not-a-uuid` / `cursor=zzz` | 依次 `422 invalid_filter/invalid_filter/invalid_filter/invalid_limit/invalid_filter/invalid_cursor` |
| 总览（无 Profile） | `200`，`profile=null`，Data `unconfigured`，identity/owner/tier/links 全部保留，`golden_signals=[]` |
| 总览 `?environment=production`（与登记不符）/ 不存在的 service | 都是 `404 service_not_found` |
| 总览 `?window_seconds=0` | `422 invalid_window` |
| 工作区（无 Profile） | `409 profile_unconfigured` |
| 工作区只传 `focus_kind` / 只传 `from` | `422 invalid_focus` / `422 invalid_time_range` |
| 总览（已发布 Profile） | `200`，四张 Golden 全部 `value=null`、`display=Unknown`、`availability=unavailable`、`unknown_reason=telemetry_not_connected`，saturation 保留 `measurement_role=proxy`；Data `partial`；`observed_at` 是登记事实时间而不是请求时刻 |
| 工作区（已发布 Profile + 合法 focus） | `200`，journey 五段全 `unknown/telemetry_not_connected`，topology 节点 Health `unknown`，`diagnostic_summary=incomplete`，`capabilities` 只有 `view_domain=true`，`evidence_refs=[]` |
| 工作区非法 focus / 越界窗口 | `422 invalid_focus` / `422 scope_too_broad` |
| 目录缓存行为 | 第一次请求是 cache miss 占位（`golden=0`、Data partial），后台刷新后第二次请求返回 4 张 Unknown Golden 卡——bounded stale-while-revalidate 真实生效 |

### 5.3 历史与工作区卫生

```text
git log --diff-filter=A --name-only a887bd5..HEAD   # 仅 8 个源码文件，无二进制
git ls-files | grep -Ei '(^|/)(alert-system|server)$|pgdata|serviceobs-profiles'  # none
git log --branches --tags --diff-filter=A -- alert-system   # 空（误加的产物不可达）
git rev-list HEAD --objects | grep -c ' alert-system$'      # 0
git status --porcelain                                      # 空
```

临时 PostgreSQL 数据目录（`/tmp/pgdata`）、Profile 目录（`/tmp/serviceobs-profiles`）与
seed 数据都在 VM 的 `/tmp` 下，不在仓库内，也没有被跟踪。

## 6. 未执行 / 未完成项与原因

### 6.1 Docker 依赖的两套测试（未通过）

`internal/store` 的 integration 测试与 `test/e2e` 都通过 testcontainers /
docker compose 启动 PostgreSQL，且 `setupTestStore` 没有 DSN 覆盖入口。本 VM 没有
`docker`（`exec: "docker": executable file not found in $PATH`），因此：

- `go test -tags=integration ./... -race -count=1` **失败**，失败原因全部是容器无法启动，
  不是断言失败；
- 新增的 `TestServiceObservabilityScanWalksCompleteCandidateSetByKeyset` 因此**没有在真库
  上跑过**，只保证 `go vet -tags=integration ./...` 通过（可编译、签名正确）。
- 缓解：keyset SQL 的真实行为改用 §5 的本地 PostgreSQL 16 + live API 端到端验证覆盖
  （目录分页 4 页 7 行不重不漏、摘要分母恒定、过滤生效）。这条路径执行的是同一条
  `ScanServiceIdentities` 语句。
- 建议：Codex 在有 Docker 的环境补跑
  `go test -tags=integration ./internal/store -run ServiceObservability -count=1` 与
  `go test -tags=integration ./... -race -count=1`。

### 6.2 视觉验收只完成 live 模式的一部分

已实际生成并逐张核对的 live 模式截图（1280×800 桌面视口）：

- `live-directory.webp`：目录 7 行、摘要 7/0/0/6、Unknown + Unconfigured、已发布 Profile
  行显示 `Profile v1` 与四个"未知"信号、Data 列显式 `Stale`；
- `live-overview-profile.webp`：四张 Golden 卡 `Unknown / telemetry not connected`，
  saturation 卡带 `PROXY` 标记，运行状态 `Unknown` + `Partial`，右栏 focus 启动器；
- `live-overview-unconfigured.webp`：未配置 Profile 的横幅、禁用的"进入可观测性工作区"
  按钮、`Profile 未配置`、依赖/事件空态文案；
- `live-workspace-unknown.webp`：Partial 横幅、请求阶段面板、单节点拓扑 Health Unknown、
  证据页签 Unknown、五分钟窗口时间线；
- `live-workspace-422.webp`：非法 focus 的显式"定位参数非法"页面状态与两个返回入口。

**未完成**：`VITE_SERVICE_OBSERVABILITY_FIXTURE_MODE=true` 下 1536/1180/900/390 四个视口
的五态视觉复核。桌面预览工具在这一步挂住，按指令没有重跑，因此本文**不声称** fixture
模式或响应式断点已通过视觉验收。fixture 路径当前的证据只有：静态验证器（fixtures 仍
驱动显式 fixture 模式、双重门控、无回落）与 `data.ts` 的类型/构建检查。
建议 Codex 在 `VITE_SERVICE_OBSERVABILITY_FIXTURE_MODE=true` 下按计划 §6 Task 11 的
"Codex 视觉验收"清单复核四个视口与五态。

### 6.3 `design-qa.md` 未改动

Phase 1 视觉证据没有任何改写；Phase 2 的 live 证据集中在本文，因此没有向
`design-qa.md` 追加内容。若 Codex 完成 §6.2 的 fixture 视觉复核，可在那时按"只追加"
口径补记。

## 7. 已知限制（不夸大、不隐瞒）

1. **翻页快照弱一致性**：候选身份集合在单个只读快照内一致，但目录摘要来自进程内
   snapshot cache，两页之间可能被后台刷新更新。因此极端情况下相邻页仍可能重排或重复
   一行，刷新页面即可恢复。Phase 2 明确接受这一点，不把它描述成强一致分页；只有产品
   确需强一致时才引入 server-side snapshot token。
2. **游标文本字段有前缀界**：服务名与团队名在游标里按 96 字节（rune 对齐）收敛，用来
   把游标钉死在 512 字符内。比较时**两侧同时收敛**，所以游标锚点自身永远被正确排除
   （不会重复、不会重启分页）；只有前 96 字节完全相同、且"按全名排序"与"按 service ID
   排序"结果不同的行，才可能在两页之间重复或跳过。
3. **候选安全上界是硬失败**：metadata 过滤后超过 2000 个候选会返回 422
   `scope_too_broad`。这是刻意的——返回一个被截断的摘要分母比让用户收窄筛选更危险。
   部署规模超过这个量级时需要显式调大配置并重新评估目录性能。
4. **actor.team_ids 目前为空**：仓储层没有"列出某用户全部团队"的查询，而 Phase 2 也没有
   service-level visibility policy，team_ids 只参与服务端内部的
   `permission_snapshot_id` 指纹，不影响任何可见性判定，也不出现在响应里。等真正引入
   服务可见性策略时必须补齐。
5. **`from`/`to` 接受带偏移量的 RFC3339 并归一到 UTC**：不接受没有时区的裸时间戳。
   带 `+08:00` 的输入表达的仍是绝对瞬间，因此被接受后统一转成 UTC；`timezone` 只影响
   显示。
6. **`layer` / `profile` 两个前端筛选只在 fixture 模式出现**：服务端没有对应筛选维度，
   live 模式下继续渲染它们会变成"看起来能筛、其实无效"。live 模式的等价治理入口是
   `data_status=unconfigured`（Profile 缺失必然是 Data Unconfigured）。
7. **遥测仍未接入**：Golden/RED/USE、journey、observed path 与 evidence 在 live 模式恒为
   Unknown/Unavailable，`capabilities` 只有 `view_domain=true`。这是 Phase 2 的设计目标，
   不是缺陷；Phase 3 替换 `ObservationReader` 后无需改动 API 与前端。

## 8. 下一 Phase 输入

- Phase 3 只需替换/新增 ports：Prometheus-compatible semantic signal adapter、
  OpenObserve focus/log/trace/access adapter、EvidenceRef broker 与其 RBAC/审计/限额；
  `facade`、`view`、`api` 与前端不需要为此改结构。
- 真实 metric names/labels/retention 经运行时 discovery 固化后，才发布生产 E2B Profile
  v1；本次 live 验证用的 `/tmp/serviceobs-profiles/e2b-local.json` 是本机演示用 artifact，
  不入库、不是生产 Profile。
- Agent 继续不实现。`DiagnosticContext + ToolDescriptor + EvidenceRef` 已经是稳定的
  未来输入边界。
