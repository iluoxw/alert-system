# Operations Ledger UI 与交互设计

## 状态

- 日期：2026-08-02
- 状态：已由用户选择为实施基线
- 视觉源：`/Users/luoxiaowei/.codex/visualizations/2026/08/01/019fbd58-8fef-72a1-8abb-86cbf07196de/operations-ledger-prototype.html`
- 设计方向：Industrial / utilitarian，强调证据、状态、责任与可审计动作

## 目标

把 SRE Control 从“基础设施大屏 + 告警配置后台”重构为日常分诊工作台。默认视图回答四个问题：

1. Alertmanager 权威源当前是否可信；
2. 哪些技术 Fault 需要先处理；
3. 哪些应用服务已确认或可能受影响，负责团队是谁；
4. 哪些 Fault 已经升级为 Incident，需要进入协作响应。

## 核心工作流

```text
运行总览选择 Fault
  → Fault 工作区查看 Alert 证据
  → 人工确认或排除 application Impact
  → active Fault + confirmed application Impact 满足宣布门槛
  → 明确确认后创建 Incident
  → Incident Room 处理多 Fault、角色、任务、时间线、恢复门槛和 Runbook
```

系统只判断门槛是否满足，不替 SRE 决定是否宣布 Incident。

## 信息架构

### 运行态

- 运行总览：权威源提示、活动 Fault 队列、Impact 快速判断、活跃 Incident；
- Faults：当前阶段由运行总览的 Fault Ledger 承担入口；
- Incidents：当前阶段由运行总览和 Incident Room 承担入口；
- 服务、拓扑、Alerts：保留现有详情和告警证据入口，后续形成独立目录页。

### 控制态

- 数据接入：现有 integrations；
- 路由与升级：现有 routes、escalation policies、notification policies；
- 团队与服务登记：现有 teams、users、team ops settings；
- 静默和值班：继续保留现有能力，但不与 Alertmanager 权威状态混成一个字段。

## 页面设计

### 应用外壳

- 左侧 208px 深色导航，明确分成“运行态”和“控制态”；
- 顶部栏只保留当前页面名、环境、全局搜索与刷新；
- 主内容使用浅灰画布、白色硬边界面板、4–6px 小圆角；
- 不使用大面积渐变、营销式 Hero 或装饰性 KPI；
- critical / warning / monitoring 由窄色轨、状态文本和浅色背景共同表达，不能只靠颜色。

### 运行总览

- 顶部权威源条必须区分事实与待建设能力：当前实现明确显示 `Webhook mirror`，不得伪装成已具备 API reconciliation；
- 主区是按优先级排序的 Fault Ledger，而不是拓扑地图；
- 行内展示 Fault、canonical entity、confirmed/potential/ruled-out Impact 数量、负责团队、持续时间和状态；
- 选中行后右侧更新“快速判断”和 application Impact；
- 右侧 Incident 仅作为响应入口，不抢占首页主任务。

### Fault 工作区

- 页面头显示 Fault ID、状态、实体、负责人和持续时间；
- Alert 证据按角色展示，并明确拆分来源事实、响应状态和抑制状态；现有数据模型尚未完成三轴拆分时，UI 使用兼容标签并避免声称完整支持；
- Impact 只允许 `potential / confirmed / ruled_out` 三态；
- potential 可以由人工确认受影响或确认未受影响，动作必须记录操作者、时间和依据；
- 只有 active Fault 且至少一个 application Impact 为 confirmed 时，宣布按钮才可用；
- 已关联 Incident 时不重复创建，直接进入 Incident Room。

### Incident Room

- 顶部以 Incident 状态、标题、持续时间和状态推进门槛为主；
- 展示全部关联 Fault，并显式标出 primary；
- 主区优先顺序：响应任务 → 恢复门槛 → 受控 Runbook；
- 右侧为统一时间线；角色和通信信息保持可见但不压过处置任务；
- E2B Runbook 是黄金场景能力，放在通用 Incident Room 中，不改变通用导航和核心对象。

## 数据与交互契约

### Impact 人工裁决

请求：

```json
{
  "state": "confirmed",
  "rationale": "应用 SLO 5xx 已超过阈值",
  "confidence": 1
}
```

约束：

- 目标 node 必须属于当前 Fault 的 Impact 集合；
- 人工裁决优先于后续自动重算；
- `ruled_out` 必须是数据库约束允许的正式状态；
- 已有关联 Incident 时，Impact 裁决同步进入 Incident 时间线。

### Incident 宣布门槛

```text
fault.status == active
AND exists confirmed Impact whose node.kind == business_system
```

不再要求 Fault 必须是 capability scope；应用服务 Fault 也可以作为 Incident 的起点。

## 响应式与可访问性

- >= 1180px：Fault Ledger + 320px 判断栏；
- 900–1179px：判断栏下移，表格隐藏持续时间等次要列；
- < 900px：导航收窄，表格转为紧凑行，但核心动作仍可见；
- 交互元素必须有可访问名称、可见焦点和 disabled 原因说明；
- loading、error、empty、success 和 pending 状态必须可见；
- 动作成功后刷新 Fault、Overview 和 Incident 查询，不依赖页面重载。

## 非目标

- 本轮不实现 Alertmanager API reconciliation 引擎；
- 不新增服务目录、拓扑治理和 Alert Mapping 的完整配置页；
- 不自动宣布 Incident；
- 不把 E2B/CCH 写进通用导航或领域状态机；
- 不重写现有 Alert、Silence 与 Settings 的全部视觉细节。

## 验收

- 登录后首页与选定视觉方向一致，不再显示大面积营销 Hero；
- Fault 行选择与右侧详情联动；
- potential Impact 可真实写入 confirmed / ruled_out；
- 门槛在写入前后实时切换；
- Incident 可真实创建并进入 Incident Room；
- Incident Room 的任务、时间线、角色和恢复判据写操作继续可用；
- E2B 黄金场景和通用 CCH/基础设施样例均通过同一对象模型呈现。
