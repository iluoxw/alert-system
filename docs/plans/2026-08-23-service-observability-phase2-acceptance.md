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

---

## 9. 验收纠正 Slice R3：画稿对齐、视觉可读性与响应式交互

> 执行者：同一固定 Cursor Cloud 会话 / Claude Opus 5 High。本节只追加，不改写前八节。

### 9.1 SHA 与范围

| 项 | 值 |
|---|---|
| R3 派发基线（远端 HEAD） | `85a8019c403dbfc4a3d5770010e4db995cfc8034` |
| R3 提交 `fix(web): raise service observability readability and path scrolling` | 本提交 |
| 改动文件 | `web/src/serviceObservability/serviceObservability.css`、`web/src/components/serviceObservability/TopologyExplorer.tsx`、`web/src/components/serviceObservability/WorkspaceContext.tsx` |

preflight 的分支、HEAD、worktree、远端四项全部匹配 `85a8019`；全程没有 reset / rebase /
amend / force push / 切分支。两个 `.tsx` 只改语义顺序与首屏取舍，没有触碰 Page View 合同、
Go 后端、fixtures 数据、查询契约、安全链接策略或依赖清单。

唯一批准的视觉基线是 `service-directory-v1`、`e2b-service-overview-v1`、
`e2b-workspace-focused-failure-v2`、`e2b-workspace-partial-unknown-v1`、
`e2b-workspace-forbidden-logs-v2`；`focused-failure-v1` 与 `forbidden-logs-v1` 未作为基线。

### 9.2 修正项与实测度量

度量方式：headless Chrome 固定 1536×1024 视口，读 `getComputedStyle` 与
`scrollWidth/clientWidth`，不靠目测估字号。

| 项 | 修正前实测 | 修正后实测 |
|---|---|---|
| 主路径节点名 | 8px | 11.5px |
| 主路径关系名 | 7px | 11px |
| 主路径 RED 值 | 8px | 11.5px |
| 依赖卡节点名 | 9px（`PostgreSQL`/`OpenObserve` 被省略号截断） | 11.5px（两者完整可读） |
| 依赖卡关系名 | 7px | 11px |
| 依赖卡信号值 | 8px | 11.5px |
| 补充声明关系 | 7px | 11px |
| 画布摘要 / 语义图例 | 9px / 8px | 11px / 10px |
| 次要 metadata（kind、信号标签、freshness） | 7px | 9–9.5px |

中央路径带此前是 `justify-content: center` + `overflow-x: auto`。字号提高后内容必然超宽，
而居中会把左端内容推到负偏移、`scrollLeft` 到不了，最左入口永久不可达。改为
`justify-content: flex-start` 配合首/末子元素 `margin: auto`：空间富余时仍视觉居中，溢出时
auto margin 归零、滚动范围完整。实测 `scrollWidth 754 > clientWidth 636`、
`maxScrollLeft 118`、左端可达。刻意不用 `justify-content: center` + auto margin 的写法——
实测那种组合只能滚到一半范围（`maxScrollLeft 80` vs 应有的 `160`）。

信息优先级按计划 §3 C7-2 固定为 **主请求与运行路径 > 运行资源依赖 > declared 数据与证据
依赖**，并且是改 `TopologyExplorer.tsx` 的 JSX 顺序而不是 CSS `order`：用 `order` 会让视觉
顺序与 DOM/Tab 顺序分叉，键盘用户拿到的是另一套顺序。实测 DOM 顺序 == 视觉顺序，
且没有任何 section 带非 `0` 的 `order`。层级另用呈现权重表达：主路径实边加投影、
declared 数据带用更浅底色与虚边。这里与画稿的空间摆放（数据带在路径上方）有意不同，
遵循的是计划里写明的优先级顺序。

右栏摘要首屏此前止于"仍需验证"，主动作被完全挤出可视区（`scrollHeight 571 >
clientHeight 518`，主动作 `fullyVisible: false`）。现在固定为 **当前选择 → 定位结果 →
合并支持事实（evidence + fault 合计最多 2 条）→ 仍需验证 → 主动作**，"不可用数据来源"
明细移到主动作之后。实测五段全部 `fullyVisible: true`。合并上限只影响摘要首屏取舍：
完整证据列表仍在"证据"页签，主动作"查看证据"就是入口，没有丢弃任何合同事实。

修正过程中我自己引入并修掉了三处回归，都是实测发现的：`auto-fit` 按 `minmax` 的**最大值**
算列数，`minmax(198px, 236px)` 只排两列，把最低优先级的 declared 数据带整段推出面板可视区
（已收敛到 `minmax(178px, 202px)`，三列一行）；三列面板 570px 装不下放大后的内容
（利用时间线下方约 40px 真实空档提到 606px）；信号列在 `align-items: flex-start` 下宽度按
`max-content` 算，标签溢出列宽互相压字（已加 `max-width: 100%`）。另外发现原实现的信号
层级是反的——标签粗体 11px、真正的读数只有 9.5px，现已交换。

### 9.3 五张 1536×1024 同输入比较 + Empty/Stale

| 视图 | 结论 |
|---|---|
| Directory | 通过。E2B Critical + 真实 Golden；其余行 `Golden Unknown · 未配置 Profile`，Health 与 Data 保持正交，没有假 Healthy |
| Overview | 通过。四大黄金信号、当前 Fault、按 `group_key` 分组的 9 条关键依赖、最近事件、右栏运行状态/开始诊断/责任入口齐备 |
| Complete | 通过。焦点 8 节点 / 7 关系，`18 req/s · 7.2% · P95 3.48s` 位于服务边，HugePages 使用率 91% 位于资源节点 |
| Partial | 通过。Golden/RED/USE 全部 Unknown（无旧值、无 0、无绿色），PLACE 状态未知，declared 拓扑与 FLT-5F437252 保留，主动作只重试失败数据 |
| Forbidden | 通过。仅日志行 Forbidden（无结果数、无正文、无 deep link），Trace/host USE/Fault 仍可查看，服务健康不变，无全局重试 |
| Empty（`focus_empty_valid_query`） | 通过。横幅"查询成功，但没有匹配该 Sandbox ID 的请求"，保留 declared topology，不发明 journey；Empty 未被说成 Healthy |
| Stale（`observed_relation_stale`） | 通过。横幅"Observed path 已过新鲜度 TTL"，错误与容量压力仍在，observed 边转虚线 |

画稿归一化四条规则全部保持：`18 req/s`（不是 rpm）、Golden/RED/USE 来自
`prometheus_compatible`（OpenObserve 只负责 logs/traces/access Evidence）、资源节点称
"使用率 91%" 而 Golden 卡称"容量压力 proxy 91%"、Partial 横幅是 telemetry bundle 整体超时。
没有补造 sparkline、节点数字、第四条跨窗口事件、跨服务条目、额外节点或健康状态。

### 9.4 响应式（实测数值）

| 宽度 | Context Drawer | 页面级横向溢出 | 其它 |
|---|---|---|---|
| 1180 | 抽屉入口存在，点击后抽屉内 4 个页签与摘要真实渲染；行内上下文面板不再并列 | `scrollWidth 1180 == clientWidth 1180`，严格排除内部滚动容器后 0 个溢出元素 | — |
| 900 | 同上 | `900 == 900`，0 个溢出元素 | — |
| 390 | 同上 | `390 == 390`，0 个溢出元素 | `.topology-graph` `scrollWidth 566 > clientWidth 342`、左端可达、`maxScrollLeft 224`；关系列表入口可见且完整落在视口内 |

三个宽度下 Directory / Overview / Workspace 三条路由都单独测过，均无页面级横向溢出。

### 9.5 交互实测（13/13 通过）

journey stage 选中并写入 `stage=`；path node 选中驱动右栏"当前节点"；relation 选中驱动
"当前关系"；证据页签 4 行可见可点；timeline 事件选中并写入 `cursor=`；关系列表 ↔ 拓扑双向
切换（关系列表 7 行）；focus ↔ 全量在 `8 节点 · 7 关系` 与 `10 节点 · 10 关系` 之间切换；
浏览器后退/前进正确恢复 scope；Partial 的"重试失败数据"回到主视图且 RED 恢复；Forbidden
不出现全局重试；关系列表行 `tabIndex >= 0` 且可 focus + Enter；DOM 顺序 == 视觉顺序。
没有任何功能是被 CSS 隐藏的——每个入口都同时验证了"可见 + 命中测试通过 + 点击有效果"。

### 9.6 命令与真实结果

```bash
git diff --check                                              # 无输出
python3 docs/contracts/service-observability/v1/verify_examples.py  # PASS 5 schemas + 全部 fixture + 4 个 scenario
gofmt -l internal/ cmd/                                       # 无输出
go build ./... && go vet ./... && go vet -tags=integration ./...   # 全部通过
go test -race -count=1 ./internal/serviceobs/... ./internal/api ./internal/app \
  ./internal/store ./internal/config ./cmd/server             # 全部 ok
go test -count=1 ./...                                        # 仅 test/e2e 失败（缺 Docker）
cd web && npx oxlint && npx tsc -b                            # 通过
npm run test:service-observability                            # verifier 通过；fixture provider 140 项
npm run build                                                 # 通过
```

- **Docker 缺失是真实限制**：本机 `docker` 不在 PATH，`go test -tags=integration ./... -race`
  报 `rootless Docker not found`，`test/e2e` 的 `TestMain` 报"启动 docker compose 失败"。
  这两项在本轮**未执行成功**，不能算通过。R3 只改前端样式与两处前端语义顺序，不触碰
  store/迁移/HTTP 契约，因此该缺口与本切片改动无关，但仍需在有 Docker 的环境补跑。
- 生产 bundle 对 17 个 fixture ID / scenario key / DEV-only 符号零命中。
- `npm run build` 仍有既有的 chunk 体积提示（主应用 bundle > 500 kB 默认阈值），与本切片无关。
- 三个 hold path 从 `6e4eddd..HEAD`、R3 基线 `85a8019` 与工作区三个维度都零 diff；
  没有 Docker / Kubernetes / OTel / 迁移 / 契约 / fixtures 改动；Agent 关键词扫描为 0。

### 9.7 因"合同无事实、禁止造数"而有意保留的画稿差异

1. **没有 sparkline**：画稿的 Golden 卡带趋势曲线，合同没有 timeseries samples，
   契约 §"Phase 1 的 fixture"明确规定无 samples 不得绘制。保留差异。
2. **运行资源依赖只有一张卡**：画稿底部有 ClickHouse、OpenObserve 等多个运行资源，
   当前 fixture 只声明一条到 Host Fleet 的资源依赖。那一行因此留白，不补节点。
3. **事件时间线只有 3 条事件**：画稿有第四条跨窗口事件，合同窗口内只有三条。不补。
4. **主路径是 4 个节点**（Traefik → API/Client Proxy → Orchestrator Pool → Sandbox Pool），
   画稿画了 5 个（含 SDK/Envd）。节点集合由 bounded observed projection 决定，不补。
5. **数据带在主路径下方**：画稿把 PostgreSQL/Redis/JuiceFS 摆在主路径上方。计划 §3 C7-2
   规定的优先级是"主请求路径 > 运行资源 > declared 数据/证据"，本实现遵循书面优先级，
   spatial 摆放因此与画稿不同。
6. **右栏摘要首屏只显示 2 条合并支持事实**：画稿的证据列表有 3 行。首屏必须让"仍需验证"
   和主动作可见，完整列表在"证据"页签（4 行，实测可见可点）。
7. **右栏信号列的标签会省略号**：`API 到 Orchestrator 请求率` 在 113px 列宽下放不下。
   标签属次要 metadata（9.5px），读数是 11.5px；关系全名在紧邻上方的标题里已经完整给出。
8. **摘要面板里的证据摘要单行省略**：证据页签有纵向空间，同一组件在那里折两行完整显示。

---

## 10. Slice R3 修订：窄屏页面级横向溢出与门禁结论更正

> 本节只追加，不改写 §9。base SHA `e05e3a218a030d3648bcd3413418be6d12b2a405`。

### 10.1 §9.4 的"390 三路由 0 溢出"结论被推翻

Codex 用 in-app Browser 在 macOS 经典 15px 滚动条下复核，推翻了 §9.4 里
"390 页面级零横向溢出"这一条：

| 项 | Codex 原实测（macOS 经典滚动条） |
|---|---|
| `window.innerWidth` | 390 |
| `document.documentElement.clientWidth` | 375 |
| Directory `document.scrollWidth` | **387（页面级溢出 12px）** |
| Overview / Workspace `document.scrollWidth` | 375（正常） |
| 元凶 `.service-signal-legend` | rect/client width 351、scrollWidth 375 |

**假绿机制**：§9.4 的验证跑在 Cloud headless 的 overlay scrollbar 下，
`clientWidth == innerWidth == 390`，图例容器拿到 366px 恰好装得下，溢出被掩盖。经典滚动条
把可用宽度压到 375 / 容器 351 才暴露。这是**验证方法缺陷**，不只是一个偶然漏网的数值：
断言口径原本用的是 `innerWidth`，而只有 `document.documentElement.clientWidth` 才跨平台等价。
本节起统一按 `document.scrollWidth <= document.documentElement.clientWidth` 断言。

### 10.2 根因

`.service-signal-legend` 是页脚的一行"`信号：` + 四个 Health 徽章 + Traffic/Rate 说明"，
原规则是 `display: flex; gap: 12px` 且**没有 `flex-wrap`**。四个徽章各自
`white-space: nowrap`，flex 项的 `min-width: auto` 让它们无法收缩到文字宽度以下；说明文字
开头的 `Traffic/Rate` 又是不可断的拉丁 token。三者相加的最小内容宽度超过容器可用宽度，
内容溢出到文档流外面，把整个页面顶出横向滚动条。

R3 把该图例字号从 10px 提到 11px 是把它推过临界点的直接诱因，因此这是 **R3 自己引入的回归**，
不是历史遗留。Overview / Workspace 的同类元素 `.topology-semantics` 本来就带
`flex-wrap: wrap`，这正是它们 375 == 375 的原因；本次修复只是给 Directory 图例补齐同一口径。

### 10.3 本机（Cursor Cloud）复现与修复后实测

复现用等效内容宽度 375px（即 macOS 的 `clientWidth`）：

| 状态 | Directory `scrollWidth` / `clientWidth` | 图例 `clientW` / `scrollW` | 图例行数 |
|---|---|---|---|
| 修复前 | **389 / 375（溢出 +14px）** | 351 / 377（溢出 26px） | 3 |
| 修复后 | 375 / 375（溢出 0） | 351 / 351（无溢出） | 2 |

与 Codex 的 387 / 375（+12px）、351 / 375 相差约 2px，来自 Linux 与 macOS 对同一段中英文
混排的字体度量差异；元素、机制与方向完全一致。修复后同尺寸复测：

| 宽度（= `clientWidth`） | Directory | Overview | Workspace | Complete/Partial/Forbidden/Empty/Stale |
|---|---|---|---|---|
| 360 | 360 == 360 | 360 == 360 | 360 == 360 | 全部 360 == 360 |
| 375 | 375 == 375 | 375 == 375 | 375 == 375 | 全部 375 == 375 |
| 390 | 390 == 390 | 390 == 390 | 390 == 390 | 全部 390 == 390 |

七个视图 × 三个宽度共 21 次测量全部无页面级横向溢出，console 全程 0 warning / 0 error。

内部横滚是**预期行为**，不算页面溢出，并且未受影响：375 下 `.service-table-scroll`
`scrollWidth 1240 > clientWidth 349`、左端可达、`maxScrollLeft 891`。375 下图例换行为两行，
说明文字独占第二行且未被裁剪（`lastSpanClipped: false`）。

另外在 360 / 375 对三条路由做了"不换行 flex 行"专项扫描，**0 个**其它页面级溢出源。
1536 下 Overview 有两个 `.service-dependency-group-cell` 内容比自身盒宽 2px（`91` vs `93`），
但它们位于依赖表格内、右边缘在 x≈325，够不到视口边界，不构成页面级溢出；本轮不在范围内改动，
如实记录为观察项。

### 10.4 宽屏零回归

| 宽度 | Directory / Overview / Workspace | Directory 页脚图例 |
|---|---|---|
| 900 | 全部 `scrollWidth == clientWidth` | 单行、height 42、无溢出、`column-gap 12px`、`margin-left 10px` |
| 1180 | 同上 | 同上 |
| 1536 | 同上 | 同上 |

1536 Directory 与 §9 版本做了逐像素比对：**内容区（x ≥ 208）diff bbox 为 `None`**，
表格区与页脚区 diff 同为 `None`；整页唯一差异是侧栏左上角 30×32 的应用 logo 字形
（占整页 0.061%，与本次仅作用于 `.service-signal-legend` 的改动无关）。

### 10.5 §9.6 Docker 结论更正

§9.6 把 Docker 缺失写成了本轮的通用限制，这一表述需要收窄：**那只描述 Cursor Cloud 环境**。
Codex 的本地环境 Docker 可用，并已实际执行通过：

```bash
env -u DB_DSN go test -count=1 ./...                      # full unit，通过
env -u DB_DSN go test -tags=integration ./... -race -count=1   # integration -race，通过
```

因此 Phase 2 的 full unit 与 integration race 套件在本地是**已通过**状态，不能继续写成全局
未验证。本次修订在 Cloud 侧仍然缺 Docker（`docker` 不在 PATH），这两条命令在 Cloud 未执行；
本轮改动只涉及一条 CSS 规则，不触碰 store / 迁移 / HTTP 契约 / Go 代码。

### 10.6 §9.1 "改动文件"澄清

§9.1 表格里的三个文件指的是 R3 的**实现文件**（一个 CSS + 两个 tsx 的语义顺序小改），
不含该切片同时追加的文档。本次修订的实现文件只有
`web/src/serviceObservability/serviceObservability.css` 一个，另外两个是本文件与 `design-qa.md`
的追加证据。

### 10.7 本次修订的命令与门禁

```bash
git diff --check                                              # 无输出
python3 docs/contracts/service-observability/v1/verify_examples.py  # PASS 5 schemas + 全部 fixture + 4 scenario
go build ./... && go vet ./...                                # 通过
go test -race -count=1 ./internal/serviceobs/... ./internal/api ./internal/safelink  # 全部 ok
cd web && npx oxlint && npx tsc -b                            # 通过
npm run test:service-observability                            # verifier 通过；fixture provider 140 项
npm run build                                                 # 通过
```

生产 bundle 对 17 个 fixture / scenario / DEV-only token 零命中；三个 hold path 从
`6e4eddd..HEAD`、base `e05e3a2` 与工作区三维度零 diff；未改动任何 `.tsx` / Go / contracts /
fixtures / 依赖 / Kubernetes / OTel 文件。
