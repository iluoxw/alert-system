# Service Observability 视觉稿来源

## 生成方式

五张视觉稿均使用 Codex 内置 Image Gen 生成并经过人工语义检查。它们用于产品设计与视觉验收，不是生产数据截图。

输入参考：

- 功能与布局：先前选定的 E2B 请求焦点拓扑稿；
- 视觉系统：Firebreak 桌面端现有运行总览截图；
- 规范来源：ADR-0006、Service Observability Workspace 详细设计与 v1 contracts。

## 资产

| 文件 | 状态 |
|---|---|
| service-directory-v1.png | /services 服务目录；跨服务分诊与治理入口 |
| e2b-service-overview-v1.png | /services/:service_id 服务总览；E2B 落点与诊断启动页 |
| e2b-workspace-focused-failure-v2.png | 聚焦请求失败、RED/USE 与证据完整；当前视觉基准 |
| e2b-workspace-partial-unknown-v1.png | telemetry bundle 超时、页面 Partial、动态观测值 Unknown；图中文字中的 OpenObserve 代表该场景的聚合来源，不是生产硬编码 |
| e2b-workspace-forbidden-logs-v2.png | logs.read 缺失、日志行局部 Forbidden；当前视觉基准 |

同目录中的 focused-failure-v1 与 forbidden-logs-v1 是本轮语义校正前的保留版本，不应作为实现基准。

## 交接时的文字归一化

Image Gen 用于布局、密度、层级与状态表达，生成图中的小字号并不覆盖机器契约。开发验收必须按下表归一化：

| 图中可能出现的文字 | 实现中的权威文字/语义 |
|---|---|
| `18 rpm` 或其它近似单位 | `18 req/s`；这是 API → Orchestrator 的 RED Rate |
| Host USE 来源写成 OpenObserve | Golden/RED/USE 默认来自 `prometheus_compatible`；OpenObserve 负责 logs/traces/access Evidence |
| HugePages 91% 直接称“饱和度” | 资源节点称“使用率 91%”；Golden 卡称“容量压力 proxy 91%”，直到存在直接 placement queue/wait 指标 |
| Partial 横幅只写 OpenObserve 超时 | 该视觉场景是 telemetry bundle（Prometheus metrics + OpenObserve logs/traces）全部超时 |

以上四项是视觉实现的强制覆盖规则。最终浏览器截图不得复现这些生成文字偏差。

## 最终提示词

### 服务目录

Use case: ui-mockup。生成 Firebreak 桌面应用的 /services 服务目录，以现有 Firebreak shell 为权威视觉系统。页面是面向 SRE 的高密度服务台账，不使用营销卡片或 A/B/C/D 评分。顶部显示 Managed、Critical、Partial、未配置摘要；提供搜索、团队、Tier、健康状态、数据状态筛选；主体用可排序表格展示服务、owner、tier、健康、四大黄金信号摘要、活跃 Fault/Impact、数据状态和最近观测。

健康与数据状态必须是两个正交字段：Healthy / Degraded / Critical / Unknown 只表达信号健康，Complete / Partial / Stale / Unconfigured 只表达数据可用性。Traffic 只显示值或基线变化，不因上升自动着红。E2B 为 Critical；PostgreSQL 可显示 Health Unknown + Data Partial，以证明缺数不等于健康。主动作是打开服务，Dashboard/Runbook 为次级动作。中文清晰，不裁切，不出现 Agent UI。

### E2B 服务总览

Use case: ui-mockup。生成 Firebreak 桌面应用的 /services/svc_e2b_fixture 服务总览。该页位于服务目录与完整可观测性工作区之间，只回答责任、用户影响、当前 Fault、关键依赖和从哪里开始诊断，不复制完整动态拓扑。

顶部展示 E2B identity、Critical、owner Platform SRE、Tier T1、Profile v1，以及“进入可观测性工作区”主动作。中部依次为四大黄金信号、当前需要判断的 Fault、按请求入口/调度与运行/数据与证据分组的关键依赖台账、最近事件。右栏展示运行状态、按 Sandbox ID 与时间窗定位请求、责任与 Dashboard/Runbook/服务登记入口。健康与数据完整性分开；创建流量变化为中性蓝；HugePages 91% 为橙色饱和风险；不出现 Agent、AI 诊断或对话框。

### 聚焦失败主状态

Use case: ui-mockup。生成 1440 × 1024 的高保真 Firebreak 桌面应用页面。以选定 E2B 拓扑稿作为功能与布局参考，以 Firebreak 现有桌面页面作为权威视觉系统。页面路由为 /services/svc_e2b_fixture/observability，服务为 E2B；保留深色 208px 导航、64px 白色顶栏、浅灰画布、白色低圆角工作表面和 Firebreak 蓝/橙红/健康色。

页面包含服务命令栏、四大黄金信号、单一 Sandbox 创建阶段、请求与依赖拓扑、诊断摘要/证据/Fault 与影响/事件右栏以及底部事件时间线。焦点为 sandbox_id sbx_hp16c3，PLACE 失败，API → Orchestrator 边显示 18 req/s、7.2%、P95 3.48s，宿主 10.60.199.160 显示 HugePages 使用率 91%。RED 位于服务边，USE 位于资源节点。移除 AI、Agent、聊天、置信度、工具调用、Fault 草稿和 Incident 控件。Unknown 必须解释为缺失或过期，不能假绿。中文清晰，不裁切，不使用霓虹、玻璃、深色主画布或水印。

### Partial + Unknown

Use case: ui-mockup。保持聚焦失败主状态的 Firebreak shell、尺寸、网格、排版与组件层级，只修改数据可用性。场景为页面所依赖的 telemetry bundle（Prometheus-compatible metrics 与 OpenObserve logs/traces）在当前五分钟窗口全部查询超时，而 alert-system 仍有 Service Registration、declared topology 与已有 Fault。

页面显示 PARTIAL 和“部分数据不可用”；Golden、RED、USE 观测值全部变为 Unknown，不能显示旧数字、0 或健康绿色；PLACE 为“状态未知”；声明节点与关系保留，实时关系改为灰色虚线；Evidence 行显示“查询超时”；诊断摘要为“诊断未完成”；已有领域状态仅显示 FLT-5F437252 active，来源 alert-system 和更新时间，不继承历史因果结论。主动作只重试失败数据，次动作查看已有 Fault。

### 日志 Forbidden

Use case: ui-mockup。保持聚焦失败主状态的所有 Golden、journey、topology、failure 与 timeline 数据，只选择右侧“证据”页签。场景为当前 SRE 可看拓扑、规范化指标、Trace、host USE 和 Fault，但缺少 logs.read。

证据面板显示“部分证据受限”和 FORBIDDEN · LOGS。日志行带锁，显示“无权查看 / 需要权限 logs.read / 权限说明”，不得显示结果数、正文或 deep link；Trace、host USE、Fault 仍可查看。说明“权限拒绝不等于没有证据；受限项不会参与当前诊断结论”。Forbidden 只作用于日志行，不把整页变为 403，也不改变服务健康。
