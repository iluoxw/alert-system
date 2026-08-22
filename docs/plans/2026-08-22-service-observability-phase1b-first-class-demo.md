# Service Observability Phase 1B First-Class Demo Implementation Plan

> **For Codex:** REQUIRED SUB-SKILL: Use executing-plans to implement this plan task-by-task.

**Goal:** 在不接生产数据源、不实现 Agent 的前提下，把 fixtures 驱动的 E2B 三层 `/services` 路由收口成可完整调查、可分享、可响应式验收的一等 Demo。

**Architecture:** 保持 v1 Page View fixtures 为唯一页面事实输入，在前端增加纯投影 helper、URL 视图状态和通用展示组件。E2B 只存在于 registration/profile/view fixture；renderer 只根据 service、journey、topology、evidence、domain 和 timeline 字段渲染，缺失或过期数据只把受影响对象降级为 Unknown。

**Tech Stack:** React 19、TypeScript、React Router、Ant Design、Vite、Node fixture verifier、Codex Desktop in-app Browser。

---

## 设计取舍

1. **推荐：契约驱动的一等 Demo 收口。** 保留三份 Page View 和场景 patch，补齐真实交互、响应式与通用化；既能让 E2B 完整演示，也不会把 E2B 写进平台核心。
2. **仅做 CSS 美化。** 改动小，但无法解决静态页签、选择不联动、URL 不可恢复和 renderer 硬编码，不能作为 Phase 1 出口。
3. **提前扩充生产 API 或真实拓扑。** 画面可更丰富，但会越过 fixtures 优先和 Phase 2/3 边界，且可能发明当前契约没有的数据，因此本阶段拒绝。

## Phase 1B 退出条件

- `/services` 的筛选、排序、行级导航和返回位置可恢复，Health 与 Data 继续正交。
- `/services/:id` 仍只承担服务概览与诊断启动，不复制工作区。
- `/services/:id/observability` 的 stage、node、relation、Evidence 与 timeline 形成可逆联动，选择状态进入 URL。
- 完整、Partial、Forbidden、Empty、Stale 五种状态均保持最小范围降级，禁止假绿。
- 1180–1439px 与 901–1179px 使用调查上下文 Drawer；≤900px 保留可用导航、两列 Golden 与拓扑/表格切换。
- 通用 renderer 不含 E2B source key、固定 Fault ID、固定 relation ID、固定观测时间或 `Sandbox` 展示文案分支。
- schema、fixture verifier、lint、production build、浏览器主流程、console 与同视口 Design QA 全部通过。

### Task 1: 重新建立阻断式验收基线

**Files:**
- Modify: `design-qa.md`
- Modify: `web/scripts/verify-service-observability.mjs`

**Steps:**

1. 将当前 QA 结果改为 `blocked`，列出已确认的 P1/P2 缺口。
2. 给 verifier 增加 renderer 通用性、URL 状态、排序和 Drawer 结构的静态断言。
3. 运行 `npm run test:service-observability`，确认新增断言在实现前失败。

### Task 2: 完成服务目录交互闭环

**Files:**
- Modify: `web/src/pages/ServiceDirectory.tsx`
- Modify: `web/src/serviceObservability/serviceObservability.css`
- Create: `web/src/serviceObservability/directory.ts`

**Steps:**

1. 将可解释默认排序和七种用户排序实现为纯函数。
2. 将 `sort`、`direction` 与现有筛选一起写入 query string。
3. 实现键盘可排序表头、`aria-sort`、整行打开与次级操作隔离。
4. 为窗口、阈值、观测时间提供可聚焦的绝对解释；在 1024–1439px 固定关键列。
5. 验证搜索 Empty、清空筛选、排序和返回目录恢复。

### Task 3: 拆分通用 Workspace 投影与组件

**Files:**
- Create: `web/src/serviceObservability/workspaceModel.ts`
- Create: `web/src/serviceObservability/workspaceState.ts`
- Create: `web/src/components/serviceObservability/JourneyRail.tsx`
- Create: `web/src/components/serviceObservability/TopologyExplorer.tsx`
- Create: `web/src/components/serviceObservability/WorkspaceContext.tsx`
- Create: `web/src/components/serviceObservability/DomainTimeline.tsx`
- Modify: `web/src/pages/ServiceObservabilityWorkspace.tsx`

**Steps:**

1. 先用纯函数覆盖默认选择、相邻对象、stage/evidence 交集和事件关联。
2. 从 fixture 字段推导 source 状态、Fault deep link、signal label 和观测时间。
3. 将页面拆为命令栏、Journey、拓扑、调查上下文和时间线，移除 E2B 专属硬编码。
4. 对 Profile 尚无 UI label 的 focus kind 使用通用 humanize fallback，不增加服务分支。

### Task 4: 完成调查联动与可分享 URL

**Files:**
- Modify: `web/src/serviceObservability/workspaceState.ts`
- Modify: `web/src/pages/ServiceObservabilityWorkspace.tsx`
- Modify: `web/src/components/serviceObservability/*.tsx`

**Steps:**

1. URL 读取并规范化 `focus_kind`、`focus_value`、`from`、`to`、`stage`、`selected`、`tab`、`topology` 和 `cursor`。
2. 点击 stage 同时筛选拓扑与证据；空交集显示 bounded projection Empty。
3. 点击 node 高亮相邻关系并显示节点信号/证据；点击 relation 高亮端点并显示边 RED/Trace。
4. 点击 timeline event 固定 cursor，并同步 stage、node 或 relation。
5. 浏览器前进/后退恢复相同调查上下文；URL 不写入凭据、查询或证据正文。

### Task 5: 响应式、Loading 与可访问性收口

**Files:**
- Create: `web/src/serviceObservability/useMediaQuery.ts`
- Modify: `web/src/pages/ServiceObservabilityWorkspace.tsx`
- Modify: `web/src/serviceObservability/serviceObservability.css`

**Steps:**

1. ≥1440px 保持三列；1180–1439px 将上下文放入覆盖式 Drawer。
2. 901–1179px 将 Journey 收敛为顶部可折叠阶段条，仍用上下文 Drawer。
3. ≤900px 使用两列 Golden、可用的拓扑/关系列表切换和移动导航。
4. 保留 2px 蓝色 focus、原生键盘顺序、polite live region 和 reduced-motion。
5. 为 fixture provider 的首次进入与场景切换提供不闪烁布局的区块 skeleton，不伪造网络延迟。

### Task 6: 五状态回归与 Design QA

**Files:**
- Modify: `web/scripts/verify-service-observability.mjs`
- Modify: `design-qa.md`
- Modify: `.learnings/LEARNINGS.md`

**Steps:**

1. 运行 contracts validator、fixture verifier、lint 和 production build。
2. 在 in-app Browser 验证目录、总览、主工作区和五种状态的核心交互。
3. 检查 1536、1180、900 三个视口、键盘 focus、Drawer 与 console。
4. 将源视觉稿和同状态浏览器截图放在同一比较图中；修复全部 P0/P1/P2。
5. 仅在最终证据完整时把 `design-qa.md` 改为 `final result: passed`，并解决 `LRN-20260822-003`。

### Task 7: 用户复核后的中心调查画布收口

**Files:**
- Modify: `web/src/serviceObservability/workspaceModel.ts`
- Modify: `web/src/serviceObservability/workspaceState.ts`
- Modify: `web/src/components/serviceObservability/TopologyExplorer.tsx`
- Modify: `web/src/pages/ServiceObservabilityWorkspace.tsx`
- Modify: `web/src/serviceObservability/serviceObservability.css`
- Modify: `web/scripts/verify-service-observability.mjs`
- Modify: `design-qa.md`

**Steps:**

1. 将中心画布从稀疏线性节点改成 observed 主请求关系与 declared 资源/背景依赖的层次投影，不虚构合同外节点。
2. 新增 URL 化的 `scope=focus|all`；Empty 或 bounded projection 外阶段只在焦点范围为空，并提供显式“查看全部依赖”。
3. 在画布内显示当前范围、节点/关系数量与当前选择；选择 declared 依赖后将其提升为当前调查关系，同时保留已有 Journey 上下文。
4. 让画布与关系列表共用同一范围状态；在 390px 下页面不横向溢出，拓扑自身可横向浏览且列表替代入口可见。
5. 重新生成 1536px 同输入合并图，并回归 Complete、Partial、Forbidden、Empty、Stale、1180/900/390px 后再关闭重新打开的 P1。

## 执行说明

- 当前 checkout 已有用户与本任务的未提交改动，本轮不创建 worktree、不暂存、不提交，也不覆盖无关文件。
- 生产主落地仍是 `alert-system`；E2B 是首个 Service Profile，不是核心特例。
- 本计划不改变 Phase 2–5 顺序：生产数据投影、真实遥测 adapters、第二服务验证和 Agent 均留在后续阶段。
