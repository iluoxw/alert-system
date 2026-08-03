# Design QA — Operations Ledger / Incident-first

## 对照基线

- 设计源：`/Users/luoxiaowei/.codex/visualizations/2026/08/01/019fbd58-8fef-72a1-8abb-86cbf07196de/operations-ledger-prototype.html`
- 实现：`http://localhost:5173/`
- 浏览器：Codex Desktop in-app Browser
- 视口：设计源与实现均为 `1280 × 720`；in-app Browser 导出的实现内容画布为 `1265 × 712`，比较图只在右/下补白到 `1280 × 720`，没有缩放或裁剪 UI。
- 状态：管理员已登录；Overview 为真实本地 Fault/Incident 数据；Fault Workspace 为 E2B 黄金场景；Incident Room 为实际宣布后的 `INC-000001`。

## 同屏对照

### 运行总览

- 设计源：`design-audit/2026-08-02-source-overview.png`
- 实现：`design-audit/2026-08-02-implementation-overview.png`
- 同屏：`design-audit/2026-08-02-comparison-overview.png`
- 结论：通过。深色双分区侧栏、紧凑顶栏、权威源条、Fault Ledger 与右侧快速判断/Impact/Incident rail 的层级一致。
- 事实差异：设计源展示健康的 30 秒 Alertmanager API 对账；真实实现只具备 Webhook mirror，因此使用 warning strip 明确标注 reconciliation 尚未接入，不伪造健康状态。

### Fault Workspace

- 设计源：`design-audit/2026-08-02-source-fault.png`
- 实现：`design-audit/2026-08-02-implementation-fault.png`
- 同屏：`design-audit/2026-08-02-comparison-fault.png`
- 结论：通过。紧凑 Fault header、Alert 证据账本、三轴状态、Impact command rail、Incident gate 与解释路径的结构一致。
- 真实数据差异：设计源有 3 条示例证据；E2B 实现有 9 条真实回放证据，保持滚动而不压缩或丢弃信息。

### Incident Room

- 设计源：`design-audit/2026-08-02-source-incident.png`
- 实现：`design-audit/2026-08-02-implementation-incident.png`
- 同屏：`design-audit/2026-08-02-comparison-incident.png`
- 结论：通过。Incident header、关联 Fault chip、左侧响应任务/恢复门槛与右侧统一时间线的主结构一致；真实实现继续保留首屏以下的角色、通信和 Guarded Runbook 能力。
- 真实数据差异：设计源展示 2 个 Fault 和 4 个任务；当前实际 Incident 有 1 个 primary Fault 和 1 个已完成任务，这是运行状态差异，不是视觉缺失。

## 视觉修正记录

1. 首轮 Overview 在 1280px 过早折叠 command rail；已把双栏结构保持到 1050px，并收敛 table minimum width。
2. 首轮 Incident Room 把任务 rail 与时间线列宽反置；已改成与设计源一致的 `2fr / 0.9fr`。
3. Fault 与 Incident 的标题、证据、Impact、任务字号过密；已提升关键信息字号，同时保留 SRE 工作台的高信息密度。
4. Fault/Incident 详情页最初错误高亮“运行总览”；已分别高亮 Faults/Incidents，保持位置感和返回路径一致。
5. 三个页面均复采相同视口并重新生成同屏对照后通过。

## 交互与写链路回归

- potential application Impact：宣布 Incident 按钮保持禁用，并明确提示缺少 confirmed application Impact。
- Impact 人工裁决：通过 UI 将 `Insforge Preview Runtime` 从 potential 改为 confirmed；操作者、时间、依据与置信度可读回，Incident gate 随即解锁。
- Incident 宣布：创建 `INC-000001`，当前登录用户成为 Incident Commander，主 Fault、恢复判据、时间线和通知意图同事务创建。
- 飞书通知：真实 Webhook 发送成功；统一时间线记录 `attempts=1 / status=sent`，目标只回显为 `https://open.feishu.cn`。
- 响应任务：创建并完成 `E2E：确认 Insforge Preview Runtime 业务恢复`，任务时间线完整。
- 人工时间线：追加一条业务恢复观察假设，成功写入。
- 恢复判据：记录 `用户级 Sandbox create 成功率恢复到 SLO` 的观测证据，状态变为 passed，Incident 仍保持 declared 供后续体验。
- 干净新标签页控制台：error/warn 为 0。

## 自动化验证

- `npm run build && npm run lint`：通过。
- `go test ./... && go vet ./...`：通过。
- `go test -tags=integration ./internal/store -count=1`：通过。

## 最终分级

- P0：0
- P1：0
- P2：0
- P3：设计示例数据与真实 E2B 运行数据不同；保留真实数据与诚实的 Alertmanager 能力边界。

final result: passed
