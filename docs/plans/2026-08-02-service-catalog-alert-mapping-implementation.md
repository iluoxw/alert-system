# 应用服务目录与 Alert Mapping 实施计划

## 状态

Completed — 2026-08-02

## 目标

在不修改核心代码的前提下接入 E2B 与 CCH 两类应用服务，让 Alertmanager 权威 Alert 经过显式身份和策略契约稳定成为可解释 Fault；无策略、Alias 缺失和冲突进入治理队列。

## 边界

本期包含：

- canonical application entity 与显式 owning team；
- Entity Alias；
- 确定性 Mapping 策略、预览和版本递增；
- Mapping 结果/治理队列与单条重新评估；
- Alert → Fault evidence → application Impact 投影；
- 服务目录、Mapping 控制面和 Alert 解释 UI；
- E2B + CCH 通用链路 E2E。

本期不包含：完整 CMDB 自动同步、任意 EntityRelation 编辑器、全量 E2B 旧关联器迁移、自动 Incident、规则求值工作站。

## 实施步骤与验收

1. **Expand 数据模型**
   - 输出：migration 000019、service/alias/policy/result 领域和仓储。
   - 验收：旧 topology/fault 测试继续通过；重复 Alias 可持久化并返回冲突。
2. **统一投影链路**
   - 输出：authoritative-safe MappingStage 与 Reconciler Projector。
   - 验收：Webhook/API 对账都能幂等写同一 Mapping/Fault；权威 Alert 仍无本地通知。
3. **管理 API 与 UI**
   - 输出：服务登记、策略 CRUD/预览、治理结果、重新评估；运行态服务目录和 Alert 映射解释。
   - 验收：构建、lint、错误/空态与响应式检查通过。
4. **通用性 E2E**
   - 输出：同一核心下的数据化 E2B/CCH 接入场景。
   - 验收：两服务分别映射 active Fault + confirmed application Impact；未知 Alert 为 unmapped；重复 Alias 为 conflict 且不创建 Fault。
5. **真实浏览器全链路**
   - 输出：本地迁移、重启、服务登记、策略发布、Alert 注入和页面钻取。
   - 验收：API/浏览器/全量测试均通过，服务保持可测试状态。

## 完成结果

- migration 000019 已在本地真实 PostgreSQL 应用；应用服务、Alias、Mapping 策略与结果均可读写。
- 浏览器实际登记 `UI CCH Control Plane`、OPS 负责团队与 `prometheus.service` Alias，发布无业务私有字段的 CCH Mapping 策略。
- 页面生成的 Alertmanager Webhook 凭据实际接收 firing Alert，完整钻取验证 `Alert → mapped 解释 → active Fault → direct_impact 证据 → confirmed application Impact → OPS`。
- 未知 Alert 实际进入治理队列，原因是 `no_policy`，没有创建 Fault；页面“重新评估”保持幂等并给出仍需治理反馈。
- E2B/CCH 自动化 E2E 证明两类业务仅靠 service + alias + policy 数据接入；重复 Alias 返回 conflict 并隔离 Fault 创建。
- 修复真实交互发现的 pgx `conn busy` 列表问题，以及实体详情裸露团队 UUID 的内容问题；canonical entity key 在 API 层保持不可变。

## 验证记录

- `go test ./... -count=1`：通过，其中 `test/e2e` 68.347s。
- `go test -tags=integration ./internal/store -count=1`：通过，207.403s。
- `go vet ./...`：通过。
- `npm run lint`、`npm run build`：通过；仅保留既有 bundle 大小警告。
- 真实浏览器：服务目录、实体责任、Mapping 发布/预览/治理、Alert 解释和 Fault 工作区均通过。
