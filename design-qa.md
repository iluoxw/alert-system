# Service Observability Phase 1X Design QA

> 2026-08-22 用户第二次复核重新打开本轮 QA：此前修复聚焦交互与拓扑层次，但没有逐画稿审计内容覆盖率和信息密度。下述 `passed` 仅保留为上一轮证据，不能作为当前出口。

## Reopened blocking findings

- [P1 resolved, retained] 线性三节点投影已重构为“主请求关系 + 资源与背景依赖”两层画布；新增焦点路径/全部依赖模式、画布内节点/关系计数、当前选择反馈、依赖关系提升为当前调查关系，以及 bounded graph Empty → 全量 declared topology 的显式转换。
- [P1 resolved by audit] 已逐一核对 Directory、Overview、完整工作区、Partial、Forbidden 五张画稿中的信息块、字段覆盖、内容密度、首屏占比和视觉层级，见下方区域矩阵。
- [P1 resolved] Directory 已扩成 7 条 E2B fixture rows；Overview 已扩成三组 9 条依赖；Workspace 已扩成 10 节点 / 10 关系，并由同一 Page View renderer 投影焦点态 8/7 与全量态 10/10。
- [P1 resolved by constraint] 缺少真实 samples 的趋势图没有伪造；新增节点没有机器信号时只展示 declared + Health Unknown。视觉差异在五张同视口合并图中显式保留。

## 逐画稿内容覆盖矩阵（Phase 1C 修复基线）

处置分类：`直接补齐` 表示 fixture 已有字段但 UI 未充分展示；`fixture 扩充` 表示设计事实源已明确、需同步 schema/example/Context/Page View；`禁止伪造` 表示当前没有足够机器事实。

### `/services` Directory

| 画稿区域 / 内容 | 当前 fixture | 审计时实现 | 差距与处置 |
|---|---|---|---|
| Managed / Critical / Partial / 未配置摘要 | 24 / 2 / 3 / 4 | 已完整展示 | 保留；Health 与 Data 继续正交。 |
| 高密度服务台账 | 仅 E2B 1 条；总量为 24 | 1 行后留下大面积空白 | `fixture 扩充`：增加 E2B 关联的 registration-only rows；只有 E2B 有 published Profile，其余保持 `profile=null` 与 Unknown/Partial/Unconfigured。 |
| 搜索、团队、Tier、健康、数据筛选 | 字段齐全 | 已实现并写入 URL | 保留；补回画稿中的 Profile/Layer 快速筛选，以 fixture 字段驱动。 |
| owner、Tier、Golden、Fault/Impact、Data、最近观测 | E2B 字段齐全 | 已展示 | `直接补齐`：profile 状态与 secondary Dashboard/Runbook 入口当前缺失。registration-only rows 不显示伪 Golden。 |
| 分页 / 结果密度 | `next_cursor=null` | 无分页 | 当前样例页无需造假页码；显示“当前 fixture 行 / 受管总量”即可。 |

### `/services/:service_id` E2B Overview

| 画稿区域 / 内容 | 当前 fixture | 审计时实现 | 差距与处置 |
|---|---|---|---|
| Identity / Critical / owner / Tier / Profile / CTA | 字段齐全 | 已展示 | 调整纵向密度，保证责任区首屏完整。 |
| 四大 Golden 值、基线变化、阈值/proxy | 4 个值、delta、threshold source、proxy reason | 值与 delta 已展示；阈值/proxy 仅悬停或小字 | `直接补齐`：显式显示窗口、基线变化、阈值来源与 proxy 说明。 |
| Golden 小趋势线 | 没有 timeseries samples | 仅通用 chart icon | `禁止伪造`：移除暗示真实趋势的图标，不用 CSS/art 画假 sparkline。 |
| 当前 Fault | 2 条完整摘要 | 已展示 | 保留并压缩行高。 |
| 请求入口 / 调度与运行 / 数据与证据三组依赖 | 现有 5 条且无 group；缺 Sandbox Pool、PostgreSQL、Redis、S3/JuiceFS | 5 条扁平列表 | `fixture 扩充`：新增显式 group 与缺失 declared rows；无信号的存储依赖为 Health Unknown。 |
| 最近事件 | 4 条 | 已展示，但首屏靠下 | `直接补齐`：保持 4 条并优化布局密度。 |
| 运行状态 / focus 启动 / 责任与入口 | 字段齐全 | 已展示 | `直接补齐`：显示 assembled freshness，确保三块在桌面首屏可扫描。 |

### `/services/:service_id/observability` 完整态

| 画稿区域 / 内容 | 当前 fixture | 审计时实现 | 差距与处置 |
|---|---|---|---|
| 命令栏、Golden、5 阶段 Journey | 字段齐全 | 已展示 | 保留；Journey 与 topology 选择继续可逆写入 URL。 |
| 主请求路径与 E2B canonical nodes | Profile 已引用 API、Orchestrator、Sandbox Pool、Envd、Traefik；Page View 仅 3 nodes / 2 relations | 只见 API → Orchestrator → Host Fleet | `fixture 扩充`：Context 与 Page View 同步补齐 canonical Journey nodes 与 declared relations；实际请求边只有有 observed 证据的部分加粗。 |
| 数据/证据依赖 | Overview 已有 OpenObserve；画稿显示数据与存储依赖 | 中央图没有 | `fixture 扩充`：把 Overview 的 declared dependencies 投影到全量依赖层；无观测关系保持 declared + Unknown。 |
| RED / USE | 关系 3 个 RED、Host 3 个 USE | 已展示 | 保留；RED 只在边、USE 只在资源。 |
| 诊断摘要、证据、Fault、事件 | summary/evidence/domain/timeline 均有 | 分散在 4 个 tab；摘要只显示结论与按钮 | `直接补齐`：摘要页增加当前证据与领域状态预览，仍由 tab 深入。 |
| 时间线密度 | workspace 3 条、5 分钟；Overview 另有 4 条 | 3 条 | 工作区不跨窗口拷贝 Overview recovery；保留时间窗事实，不为视觉密度伪造第 4 条。 |

### Partial + Unknown

| 画稿区域 / 内容 | 场景 patch | 审计时实现 | 差距与处置 |
|---|---|---|---|
| telemetry bundle timeout 横幅与局部重试 | 已明确 metrics/logs/traces unavailable | 已实现 | 保留。 |
| Golden / RED / USE | 全部受影响值 Unknown | 已实现 | 保留，不能显示旧数字或 0。 |
| Journey 最小降级 | 画稿要求 ACCEPT 仍 ok、PLACE Unknown、后续阶段 pending；patch 当前把 5 阶段全改 Unknown | 5 阶段全 Unknown | `fixture 修正`：ACCEPT 保留 ok；PLACE Unknown；FC START / ENVD READY / ROUTED 保留 pending。 |
| declared topology / existing Fault | 明确保留 | 只保留稀疏 3 节点图；Fault 在 tab 内 | 随完整态扩充 declared graph；摘要页显式预览现有 Fault。 |
| 动态 Evidence | 应显示查询超时而非 0 条 | `evidence_refs=[]`，仅来源 notice | 保留无伪证据；使用 source-status notice 表达超时。 |

### Forbidden logs

| 画稿区域 / 内容 | 场景 patch | 审计时实现 | 差距与处置 |
|---|---|---|---|
| 页面仍保留 Golden、Journey、Topology、Trace、Host USE、Fault | 仅 logs source Forbidden，Trace/metrics/domain 可用 | 语义基本正确 | 随完整态扩充图内容；Health 不变。 |
| 日志行局部锁定 | `view_logs=false` 且 log evidence 被移除 | 有锁定行和权限提示 | `直接补齐`：明确显示 `logs.read`；禁止结果数、正文和 deep link。 |
| 可用 Trace 与权限说明动作 | capabilities + remaining Trace | 当前没有显式底部动作 | `直接补齐`：增加“查看可用 Trace”和“查看权限说明”，前者切到可用证据，后者只解释权限边界。 |

### 跨页面层级与密度

| 项目 | 差距 | 修复要求 |
|---|---|---|
| 首屏占比 | Overview 与 Workspace 部分关键块被纵向挤出；Directory 反而过空 | 以 1536×1024 同输入对照调整间距、行高和分栏，不靠缩小字体掩盖缺内容。 |
| 信息层级 | Workspace 中 summary、证据、领域状态彼此割裂 | 摘要只做预览和下一步，详情仍放对应 tab，避免重复整块内容。 |
| 事实边界 | 视觉稿有趋势线和更多运行数字，合同没有 samples | 缺数据处用 Unknown / declared / unavailable；不画伪趋势、不补伪动态值。 |
| 通用性 | 不能在 renderer 中按 `service_id === e2b` 分支 | 所有分组、节点、关系与状态由 Page View fixture 字段和通用投影驱动。 |

## 验收范围

- 生产主落地：`alert-system`
- 首个 Service Profile：E2B
- 数据边界：仅消费 v1 Page View fixtures 与场景 patch；未连接生产数据源
- 功能边界：无 Agent、AI 对话、自动 Incident 或领域写入
- 降级原则：缺失、过期或受限数据只影响最小对象范围，其余当前事实继续展示

## 设计事实源与截图设置

- 视觉基准：
  - `docs/assets/service-observability/service-directory-v1.png`
  - `docs/assets/service-observability/e2b-service-overview-v1.png`
  - `docs/assets/service-observability/e2b-workspace-focused-failure-v2.png`
  - `docs/assets/service-observability/e2b-workspace-partial-unknown-v1.png`
  - `docs/assets/service-observability/e2b-workspace-forbidden-logs-v2.png`
- 实现截图目录：`/Users/luoxiaowei/.codex/visualizations/2026/08/22/01a0290f-aa8e-7d40-bce7-c69fedd1f7d4/service-observability/phase1d-content-fix`
- 主比较视口：1536 × 1024 CSS px，device pixel ratio 1
- 响应式视口：1180 × 900、900 × 900
- Browser 带原生纵向滚动条的原始截图可能为 1521 × 1014；合并图只在右/下补白到 1536 × 1024，未缩放、未裁切实现内容。

## 同输入视觉比较

| 页面 / 状态 | 实现截图 | 源图 + 实现合并图 | 结果 |
|---|---|---|---|
| Directory | `01-directory.png` | `compare-01-directory.png` | 通过：7 行、7 类筛选、Profile 与次级入口首屏可见 |
| E2B Overview | `02-overview.png` | `compare-02-overview.png` | 通过：三组 9 条依赖、4 事件、完整责任区首屏可见 |
| Workspace 完整态 | `03-workspace-complete.png` | `compare-03-workspace-complete.png` | 通过：数据依赖 / 主路径 / 运行资源三层画布与右栏预览首屏可见 |
| Partial + Unknown | `04-workspace-partial.png` | `compare-04-workspace-partial.png` | 通过：受影响值 Unknown、既有 Fault 与三项超时来源首屏可见 |
| Forbidden logs | `05-workspace-forbidden.png` | `compare-05-workspace-forbidden.png` | 通过：单行锁定、可用 Trace/USE/Fault 与权限动作首屏可见 |
| Empty | 浏览器场景回归 | 无独立源图，按 Page View scenario 验收 | 通过 |
| Stale relation / table | 浏览器场景回归 | 无独立源图，按 Page View scenario 验收 | 通过 |

### 合同优先的剩余可见差异

以下差异是对机器契约的有意遵守，不是遗留视觉缺陷：

- Directory 的 7 行全部属于 E2B fixture set；只有 E2B 根服务发布 Profile，其余 6 行是 registration-only 关联实体并保持 Health Unknown。第二个真实 Service Profile 仍属于 Phase 4。
- Overview 与 Workspace 视觉稿含趋势线和更多动态运行数字；Page View 没有 timeseries samples、实例数或版本事实，因此实现只展示已有 baseline delta、observed RED、Host USE 与 assembled time，不画假趋势或假绿状态。
- Workspace 默认焦点投影为 8 节点 / 7 关系；关闭“仅显示焦点路径”后展示 10 节点 / 10 关系。数据与证据依赖、主请求与运行路径、运行资源依赖均来自通用投影；无信号节点保持 declared + Unknown。
- 视觉稿中的近似单位、Host USE 来源和 HugePages 文案已按 assets README 归一化：边 Rate 为 `18 req/s`，资源使用率与 Golden capacity proxy 分开，直接 saturation 缺失时为 Unknown。

## 五态语义回归

| 状态 | 关键断言 | 结果 |
|---|---|---|
| 完整 | PLACE 默认聚焦 `API / Client Proxy → Orchestrator Pool`；边 RED、Host USE、Trace、Fault 与 timeline 同时可用 | 通过 |
| Partial | 四个 Golden、边 RED 与 Host USE 变 Unknown；ACCEPT 保持 ok、PLACE 变 Unknown、后三阶段保持 pending；declared topology 与已有 Fault 保留；只出现“重试失败数据” | 通过 |
| Forbidden | 仅日志行锁定；无日志正文、结果数或 deep link；Trace、Host USE、Fault 仍可用；无全局重试；服务 Health 仍为 Critical | 通过 |
| Empty | 查询成功、Data Complete、无 Evidence；保留 declared topology；无默认节点/关系选择；不发明 journey/path | 通过 |
| Stale | Data Stale；过期 observed relation 为虚线并在关系表显示 `Stale · 10:00:00`；当前 metrics Health 与 stale path 保持正交 | 通过 |

额外验证：Host USE 同时含 Available 与 Unknown signal 时，资源组保持 Available，并逐项标出直接 saturation Unknown；只有该组全部缺失时才整体 Unknown。

## 交互与可访问性

- Directory：搜索 Empty、清除筛选、七种 URL 排序、`aria-sort`、团队/Tier 快速筛选、整行 Enter 打开、次级链接隔离、查询状态和滚动恢复均通过。
- Overview：主 CTA 与 focus launcher 均生成可分享 URL；非法 focus 显示 `aria-invalid=true`、明确错误说明并禁用提交；时间窗来自 workspace fixture。
- Workspace：stage、node、relation、Evidence、timeline cursor、页签、拓扑模式和 `scope=focus|all` 均写入 URL；浏览器后退恢复完全相同的选择上下文。
- 焦点/全量：默认 PLACE 画布显示 8 节点 / 7 关系；关闭“仅显示焦点路径”后显示 10 节点 / 10 关系。数据与证据依赖、主请求与运行路径、运行资源依赖分层展示，表格视图使用同一范围状态。
- 依赖调查：选择 Orchestrator → Host Fleet 后 URL、画布当前选择和右栏标题同步更新；Journey PLACE 不被无匹配 stage 的关系选择误清除。浏览器后退可恢复画布模式与同一选择。
- canonical stage：选择 `FC_START` / `ENVD READY` / `ROUTED` 会使用 fixture 中的 canonical entity refs 更新 URL 与焦点投影；不会按阶段名称在 renderer 中临时造节点或关系。
- 键盘：排序控件为原生 `button`，行与关系表为显式可聚焦对象；全局 `:focus-visible` 为 2px 蓝色轮廓；状态同时使用图标和文字，不依赖单一颜色。
- 关系画布提供等价表格视图；表格行可选择并同步右侧上下文。
- 路由页面使用真实 `React.lazy` + `Suspense` 加载边界；只在 chunk 尚未完成时显示稳定 Skeleton，不人为增加延迟。

## 响应式验收

| 视口 | 证据 | 断言 | 结果 |
|---|---|---|---|
| 1536 × 1024 | `03-workspace-complete.png` | Journey / 三层 topology / context 三列；焦点开关、主边、资源依赖、右栏预览和 timeline 首屏可见 | 通过 |
| 1180 × 900 | `responsive-1180.png` | inline context 不存在；调查上下文按钮存在；焦点画布 `669 = 669` 无内部横向溢出；`bodyWidth 1165 ≤ innerWidth 1180` | 通过 |
| 1180 × 900 Drawer | `responsive-1180-drawer.png` | 440px 覆盖式 Drawer；背景遮罩；四个页签与主动作完整 | 通过 |
| 900 × 900 | `responsive-900.png` | 两列 Golden、横向 Journey、移动导航、焦点开关与上下文 Drawer 入口；`bodyWidth 885 ≤ innerWidth 900` | 通过 |
| 390 × 844 | `responsive-390.png` | 页面无横向溢出；拓扑画布内部 `312 < 566` 可横向浏览；关系列表替代入口保持可见 | 通过 |

## 自动化与浏览器证据

- `python3 docs/contracts/service-observability/v1/verify_examples.py`：通过 5 个 Draft 2020-12 schemas、三份 Page View、Context/Profile/Evidence/Tool 引用和四个 scenario patch。
- `npm run test:service-observability`：`service-observability verification passed: 3 routes, 5 workspace states`。
- `npm run lint`：通过。
- `npm run build`：通过；Service route 已拆为独立 chunks。Vite 仍提示既有主应用 bundle 大于默认 500 kB，该提示不影响 Phase 1 fixture 路由正确性。
- in-app Browser：目录、Overview、五态、URL history、焦点/全量切换、依赖提升、bounded Empty、Drawer、1180/900/390px overflow 与加载边界通过。
- Browser console：71 条 debug、29 条 info，0 warning，0 error；仅 Vite 连接/HMR 和 React DevTools 开发提示。

## 迭代记录

1. Phase 1A 只具备三层路由和状态骨架，已知 selection、URL、Drawer、目录排序与通用 renderer 问题，因此 QA 重新标记为 blocked。
2. Phase 1B 拆分纯投影与展示组件，移除 source key、Fault ID、relation ID、静态时间和 Sandbox 展示分支；补齐可逆联动与响应式 Drawer。
3. 第一轮同输入比较后，补回 fixture-driven Golden baseline delta，修正 1180px command bar 布局、Overview 通用 service-kind 图标和诊断输入说明。
4. Forbidden 对比发现 Host USE 因一个直接 saturation 缺失而被整体标 Unknown；已改为最小范围降级，并重新生成、重新检查合并图。
5. 用户复核重新打开中心画布 P1：右侧 Context 联动与语义正确不能替代请求路径/依赖面板本身的空间层次和交互密度。
6. 将拓扑投影增加 `scope=focus|all`，把 observed 主请求边与 declared 资源依赖分层；加入画布内选择反馈、依赖提升、bounded Empty CTA 和共用范围的关系列表，并在 1536/1180/900/390px 重新比较与回归。
7. 用户再次复核后按五张画稿逐区审计内容覆盖率；Directory 扩为 7 行，Overview 扩为三组 9 条依赖，Workspace Context 同步补齐证据、Fault 与不可用来源预览。
8. 中央画布由成对关系卡片重构为数据与证据依赖、主请求与运行路径、运行资源依赖三层网络；焦点态 8/7、全量态 10/10，节点和关系选择继续写入 URL 并驱动右栏。
9. 最终五张 1536×1024 合并图及 1180/900/390px 响应式证据重新生成；未发现 P0、P1 或 P2 视觉、交互、语义问题。

final result: passed
