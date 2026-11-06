# SRE Control 设计系统：Operations Ledger

## 产品定位：Alertmanager 权威的事故响应操作系统

本产品是事故响应操作系统，不是告警规则工作站，也不是以配置为主的后台。Prometheus、Thanos Ruler 或 vmalert 负责评估规则，Alertmanager 是唯一 Alert 权威；SRE Control 镜像 Alert 状态，并围绕 Entity、Fault、Impact、Incident 提供解释、判断和协作。

核心用户是维护共享基础设施的平台型 SRE。物理机、虚拟机、Kubernetes、数据库、消息队列等实体会同时支撑多个应用服务；应用服务及其 owning team 是第一阶段明确的业务终点。页面必须帮助值班人员从技术故障追踪 potential/confirmed/ruled-out Impact，但不能把图可达误写成已确认影响。

核心对象遵循 `Signal → Alert → Fault → Incident → Response → Recovery → Learning`：

- `Alert` 是 Alertmanager 权威事实的本地镜像，不等同于事故；source、response、suppression 三轴状态必须分开显示。
- `Fault` 是发生在 canonical Entity 上的技术故障，可关联 direct、trigger、cause_candidate、amplifier、symptom、supporting 或 excluded Alert 证据。
- `Impact` 是 Fault 对应用服务的影响判断，只有 potential、confirmed、ruled_out 三态。
- `Incident` 是围绕一个或多个 Fault 建立的协作与处置实体；没有明确业务影响或协作需求的 Fault 不必创建 Incident。
- `Entity/Alias/Relation` 用同一模型表达 infrastructure、platform、application 三层对象；关系方向由消费者/从属方指向提供者/宿主方。
- `Response` 包含角色、任务、时间线、通信、自动化动作、恢复与复盘。

产品由两个平面组成：

- 运行态：运行总览、Faults、Incidents、服务、拓扑、Alerts。
- 控制态：Alertmanager Sources、Alert Mapping、Entity/Alias 治理、Teams 与 Service Registration。

第一阶段视觉目标是桌面端“Operations Ledger + Fault 工作区 + Incident Room + 通用服务接入”。首页优先呈现 Alertmanager 数据源健康、按影响排序的活动 Fault、应用 Impact 与响应中的 Incident。拓扑是解释 Fault 和 Impact 的工具，不是首页主视觉，也不展示无法操作的全量毛线球。

## 产品原则

1. 首屏帮助值班人员回答：数据是否可信、哪些 Fault 最紧急、影响哪些应用与团队、是否达到 Incident 门槛。
2. 红色和橙色只表达告警严重度或高风险动作，不作为品牌装饰。
3. 高频页面优先信息密度与扫描效率；低频设置优先依赖清晰与防错。
4. 所有技术配置都提供可选择的合法值、影响说明和提交前校验。
5. 任何写入都必须有明确的提交中、成功、失败和未变更反馈。
6. 示例业务只作为数据内容出现；导航、布局、状态与动作中不得出现业务专用分支。

## 视觉语言

- 风格：Operations Ledger；克制、工程化、可信赖、高密度、桌面优先。
- 导航：深石墨固定侧栏；主工作区冷灰背景。
- 表面：白色主表面 + 1px 冷灰边界，少阴影。
- 圆角：6–8px；浮层最多 10px。
- 首屏不使用宣言式 Hero；状态摘要使用 36–48px 高的紧凑状态带或内联指标。
- 禁止：渐变、玻璃拟态、装饰性图表、营销式大卡片、过度留白、把拓扑图当首页背景。

## Token

```css
:root {
  --color-nav: #111827;
  --color-nav-active: #24364a;
  --color-canvas: #f4f6f8;
  --color-surface: #ffffff;
  --color-border: #d9dee6;
  --color-text: #172033;
  --color-text-secondary: #5c667a;
  --color-text-muted: #8a94a6;
  --color-primary: #315b7d;
  --color-critical: #c7352c;
  --color-warning: #d97706;
  --color-info: #2563a6;
  --color-success: #2f7d4b;
  --radius-control: 6px;
  --radius-surface: 8px;
  --space-1: 4px;
  --space-2: 8px;
  --space-3: 12px;
  --space-4: 16px;
  --space-5: 24px;
  --space-6: 32px;
}
```

## 排版

- 字体：系统无衬线；时间、计数、ID 使用 tabular numbers。
- 页面标题：24/32，600；区块标题：16/24，600；正文：14/22；辅助：12/18。
- 表格主字段 14/20，次字段 12/18；避免用纯灰小字承载关键状态。

## 布局

- 左侧导航：224px，可收起到 64px。
- 顶部上下文栏：56px，包含当前环境、搜索/快捷操作和用户菜单。
- 主内容：20–24px 内边距；最大宽度不强制居中，数据工作台充分使用横向空间。
- 首页：页标题/值班上下文 → Alert Source 健康带 → 8/4 栅格；左侧 Fault Ledger，右侧 Impact 与 Incident 队列。
- Fault 详情：主证据和关系解释 + 360px 固定判断/处置侧栏；返回时保持首页筛选与滚动位置。
- Incident Room：多 Fault 指挥条、响应任务/恢复门槛、统一时间线、角色与通信；primary fault 可切换但不丢失历史关联。
- 列表行高 48–56px；默认一屏至少显示 7 条 Fault，不使用大面积空状态插画。
- 设置页：240px 分组侧栏 + 内容区；复杂编辑使用 720–880px 宽抽屉或独立页面。
- 表单页底部：固定操作栏，始终可见保存/取消/校验状态。

## 核心组件

- `SeverityBadge`：文字 + 小形状/图标 + 色彩，不能只靠色彩。
- `AlertStateCell`：source/response/suppression 三轴在一个单元格分层展示，禁止合并为单一状态 Tag。
- `SourceHealthStrip`：显示 Alertmanager healthy/stale/error、最后成功对账、Alert 数量；异常时可直接进入 Source 详情。
- `FaultLedgerRow`：严重度轨道、Fault/Entity、confirmed/potential Impact 数量、负责团队、持续时间、状态和快捷预览。
- `ImpactDecision`：应用服务、owning team、potential/confirmed/ruled_out、证据来源和人工裁决动作。
- `CommandRail`：固定展示 Ack、Impact 判断、Incident 门槛和安全动作，不随证据长列表滚出视口。
- `EntityTable`：44–48px 行高，主字段加粗，次字段同单元格分层；危险操作收进菜单。
- `ConditionBuilder`：字段、运算符、值三段式；提供示例和即时校验。
- `EmptyState`：原因、前置条件、主操作、文档入口。
- `StickyFormFooter`：保存、取消、未保存提示、校验结果。
- `TimelineGroup`：相邻同类通知事件聚合，可展开明细。

## 文案规范

- 面向管理员使用业务词：匹配条件、渠道类型、分组字段、升级层级、通知方式。
- 禁止在主界面裸露 `kind`、`matcher`、`group_by`、`replace`、`R1`、`R5.4`。
- 按钮统一中文：保存、取消、确认删除、创建、编辑。
- 错误必须包含：发生了什么、为什么、如何修复；隐藏后端对象路径和内部枚举。

## 交互规范

- 首页 Fault 行单击打开可关闭的快速预览；“进入 Fault 工作区”或 Enter 打开完整页面。预览不得改变领域状态。
- 默认排序依次为 confirmed Impact、severity、potential Impact 数量、持续时间；排序依据要在列头或说明中可见。
- 图可达只能生成 potential；确认或排除 Impact 必须展示依据、操作者和时间，并允许追加说明。
- 只有 active Fault 且至少一个 confirmed application Impact 时，Incident 按钮才可用；禁用态必须说明缺失条件。
- Alertmanager Alert 只能 Ack、移交、备注，不能在 SRE Control 中手动 Resolve；silenced/inhibited 仍可作为 Fault 证据。
- Source stale 时保留最后已知状态，在全局状态带和相关页面显示数据可能过期，不把 Alert 自动标为 resolved。
- Select 必须有可访问名称，支持搜索时说明可按名称/账号搜索。
- 日期时间浮层互斥；Esc 仅关闭最上层；嵌套 Modal 应避免。
- 危险删除展示影响范围；级联删除明确列出将移除的关联配置。
- 提交成功必须核对服务端返回/重新加载后的真实状态，避免“假成功”。
