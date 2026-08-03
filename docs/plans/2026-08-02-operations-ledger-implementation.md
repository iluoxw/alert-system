# Operations Ledger Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 把已确认的 Operations Ledger 视觉与分诊流程落到现有 React/Go 系统，并打通 Impact 人工裁决到 Incident 宣布的真实写链路。

**Architecture:** 保留现有 Go + PostgreSQL + React Query + AntD 技术栈，在领域层补齐 Impact 三态和人工裁决，在 API 层暴露受审计写入口，在前端重组 App shell、运行总览、Fault 工作区和 Incident Room。Alertmanager API 对账仍是后续能力，本轮 UI 明确呈现 Webhook mirror 事实，不伪造数据源健康。

**Tech Stack:** Go 1.25、gin、pgx、PostgreSQL 16、React 19、TypeScript 6、React Query 5、AntD 5、Vite 8、oxlint、Browser E2E。

---

### Task 1: 固化 Impact 三态领域契约

**Files:**
- Modify: `internal/domain/fault.go`
- Modify: `internal/domain/fault_test.go`

**Step 1: Write the failing test**

增加 `TestImpactStateIsValid`，断言 potential、confirmed、ruled_out 有效，其他字符串无效。

**Step 2: Run test to verify it fails**

Run: `go test ./internal/domain -run TestImpactStateIsValid -count=1`

Expected: FAIL，缺少 `ImpactStateRuledOut` 或 `IsValid`。

**Step 3: Write minimal implementation**

新增 `ImpactStateRuledOut` 和 `ImpactState.IsValid()`，并给 `FaultImpact` 增加人工裁决人、时间字段。

**Step 4: Run test to verify it passes**

Run: `go test ./internal/domain -run 'TestImpactStateIsValid|TestValidateIncident' -count=1`

Expected: PASS。

### Task 2: 迁移并持久化人工 Impact 裁决

**Files:**
- Create: `migrations/000016_fault_impact_decisions.up.sql`
- Create: `migrations/000016_fault_impact_decisions.down.sql`
- Modify: `internal/store/faults.go`
- Modify: `internal/store/infrastructure_faults_test.go`

**Step 1: Write the failing integration test**

覆盖：automation potential → human ruled_out；后续 `ReplaceAutomatedImpacts` 不覆盖 human；裁决人和时间可读回。

**Step 2: Run test to verify it fails**

Run: `go test -tags=integration ./internal/store -run TestFaultImpactHumanDecisionWins -count=1`

Expected: FAIL，缺少迁移和 `UpsertHumanImpact`。

**Step 3: Implement migration and repository method**

扩展数据库 check 为三态，新增 `decided_by/decided_at`；仓储校验实体、状态、置信度并使用 `ON CONFLICT` 写入 human decision。

**Step 4: Run test to verify it passes**

Run: `go test -tags=integration ./internal/store -run TestFaultImpactHumanDecisionWins -count=1`

Expected: PASS。

### Task 3: 暴露 Impact 裁决 API 并修正 Incident 门槛

**Files:**
- Modify: `internal/api/infrastructure.go`
- Modify: `internal/api/incidents.go`
- Modify: `internal/incident/service.go`
- Modify: `internal/domain/incident.go`
- Modify: `internal/domain/incident_test.go`
- Modify: `test/e2e/e2b_fault_test.go`

**Step 1: Add failing API/E2E assertions**

新增 `PUT /api/v1/faults/:id/impacts/:node_id` 的 confirmed/ruled_out 写入断言，并断言 application-scope active Fault 只要有 confirmed business_system Impact 也可宣布 Incident。

**Step 2: Run focused tests**

Run: `go test ./internal/domain ./internal/incident ./internal/api -count=1`

Expected: FAIL，端点或门槛仍缺失。

**Step 3: Implement endpoint and generic gate**

验证 Fault 与 Impact 关系，写入 actor/rationale/confidence；有 Incident 时追加 decision 时间线。Declare 只要求 active Fault + confirmed business_system Impact，不再限制 capability scope。E2B Fault 保留专用恢复判据，其他 Fault 使用通用恢复判据。

**Step 4: Run focused tests**

Run: `go test ./internal/domain ./internal/incident ./internal/api -count=1`

Expected: PASS。

### Task 4: 更新 TypeScript API 契约

**Files:**
- Modify: `web/src/api/client.ts`

**Step 1: Extend types**

把 `FaultImpact.state` 扩为 potential/confirmed/ruled_out，增加裁决元数据和 `decideFaultImpact()`。

**Step 2: Verify TypeScript consumers fail where exhaustive handling is missing**

Run: `cd web && npm run build`

Expected: 旧 UI 需要处理 ruled_out 或新契约。

### Task 5: 重构应用外壳与运行态导航

**Files:**
- Modify: `web/src/App.tsx`
- Modify: `web/src/index.css`

**Step 1: Implement shell**

按视觉源实现 208px 深色双分组导航、紧凑顶部栏、环境标识、全局搜索和刷新；保持现有认证、RBAC、SSE 与路由。

**Step 2: Verify build**

Run: `cd web && npm run build`

Expected: PASS。

### Task 6: 实现 Fault Ledger 运行总览

**Files:**
- Modify: `web/src/pages/OperationsOverview.tsx`
- Modify: `web/src/index.css`

**Step 1: Implement selectable ledger**

按 confirmed impact、severity、potential impact、持续时间排序；选中行更新快速判断、Impact 与 Incident rail。使用真实 API 数据，不硬编码 E2B/CCH 分支。

**Step 2: Implement honest source strip**

显示 Alertmanager 为权威的产品契约，同时明确当前实现是 Webhook mirror、API reconciliation 未接入。

**Step 3: Verify build and empty/loading/error states**

Run: `cd web && npm run build && npm run lint`

Expected: PASS。

### Task 7: 实现 Fault 工作区 Impact 裁决与 Incident 门禁

**Files:**
- Modify: `web/src/pages/FaultWorkspace.tsx`
- Modify: `web/src/index.css`

**Step 1: Implement impact actions**

potential 行提供“确认受影响 / 确认未受影响”，提交真实 API，pending 时禁用重复操作，成功后刷新 Fault 和 Overview。

**Step 2: Implement gate copy and declare modal**

门槛只依赖 active + confirmed application Impact；原因文案明确指出缺少哪一项。通知目标继续由用户显式填写且不回显敏感 URL。

**Step 3: Verify build**

Run: `cd web && npm run build && npm run lint`

Expected: PASS。

### Task 8: 收敛 Incident Room 到响应账本结构

**Files:**
- Modify: `web/src/pages/IncidentRoom.tsx`
- Modify: `web/src/index.css`

**Step 1: Recompose layout**

加入关联 Fault 条带，主列依次显示响应任务、恢复门槛、Runbook，右列为统一时间线和角色/通信；保留所有现有写操作。

**Step 2: Verify interactions**

覆盖任务完成/重开、角色分配、时间线追加、恢复证据、状态推进与 Runbook 入口。

**Step 3: Verify build**

Run: `cd web && npm run build && npm run lint`

Expected: PASS。

### Task 9: 更新项目不变量与 Design QA

**Files:**
- Modify: `CLAUDE.md`
- Modify: `design-qa.md`

**Step 1: Reconcile invariants**

把旧的 capability-only Incident 门槛更新为 active Fault + confirmed application Impact，保留 E2B 作为黄金场景而非通用约束。

**Step 2: Start full stack**

Run: 使用项目现有 PostgreSQL、Go server 与 Vite 启动方式，确认 `http://localhost:5173/` 可登录。

**Step 3: Capture source and implementation**

同一桌面 viewport 分别捕获选定视觉源与真实应用的 Overview、Fault Workspace、Incident Room。

**Step 4: Run Design QA**

比较字体、密度、布局、颜色、图标、内容和核心交互。修复所有 P0/P1/P2，直到 `design-qa.md` 的 `final result: passed`。

### Task 10: 全链路 E2E 验收

**Files:**
- Modify as needed: `test/e2e/e2b_fault_test.go`
- Modify as needed: `design-qa.md`

**Step 1: Run automated verification**

Run: `go build ./... && go vet ./...`

Run: `go test ./internal/domain ./internal/api ./internal/incident -count=1`

Run: `go test -tags=integration ./internal/store -count=1`

Run: `cd web && npm run build && npm run lint`

Expected: 全部 PASS。

**Step 2: Run browser E2E**

使用 `admin/admin123` 登录，执行：选择 potential Impact 的 active Fault → 确认 Impact → 验证 Incident 按钮解锁 → 宣布 Incident → 进入 Incident Room → 创建并完成任务 → 追加时间线记录。

**Step 3: Run golden scenario**

Run: `go test -tags=integration ./test/e2e -run TestE2B -count=1`

Expected: E2B Alert→Fault→Impact→Incident→Recovery→飞书 mock 通知链路 PASS。

**Step 4: Review diff**

Run: `git diff --check`

Expected: 无空白错误；不提交或覆盖与本计划无关的用户改动。
