# Service Observability 契约 v1

## 状态与边界

本目录是 alert-system 中 Service Workspace 的 provider-neutral 设计契约，目前状态为 Draft。它先服务人工调查和确定性 UI；未来 Agent 若启用，只能作为相同上下文与只读工具的可选消费者。

本阶段明确不定义或实现模型、Prompt、会话、记忆、Agent Run、流式事件、动作预览、Agent 数据表或写操作。

## 文件

| 文件 | 作用 |
|---|---|
| service-observability-profile.schema.json | 服务如何选择通用视图、focus、journey、RED/USE/黄金信号与 tools |
| diagnostic-context.schema.json | 服务端按当前身份组装的只读诊断快照 |
| service-observability-page-view.schema.json | `/services` 三层页面消费的服务端投影视图；浏览器不执行原始查询 |
| evidence-ref.schema.json | 指向源系统证据的有限、可审计引用 |
| tool-descriptor.schema.json | 服务端注册的类型化只读工具元数据 |
| examples/e2b.profile.json | E2B 首个 Service Profile fixture |
| examples/e2b.context.json | 一次 E2B Sandbox 创建失败的诊断上下文 fixture |
| examples/e2b.directory-view.json | E2B 主服务与关联 registration rows 的目录 fixture；只有 E2B 主服务带 published Profile |
| examples/e2b.overview-view.json | E2B 服务总览 fixture；关键依赖按请求入口、调度与运行、数据与证据分组 |
| examples/e2b.workspace-view.json | E2B 聚焦失败工作区 fixture；包含 Journey canonical nodes、declared dependencies 与 bounded observed overlay |
| examples/e2b.workspace-scenarios.json | 基于主工作区 fixture 的 RFC 6902 状态场景：Partial/Unknown、logs Forbidden、focus Empty、stale relation |
| examples/tool-catalog.json | E2B profile 使用的六类通用工具描述示例 |
| verify_examples.py | 校验 schema、fixture、跨文件引用、Context 投影与四类场景断言 |

所有 example 都是产品设计 fixture，不是生产实体、告警或遥测事实。

Phase 1 的 fixture 需要足够完整地驱动一等 E2B demo，但丰富度不能靠 renderer 硬编码或伪造动态值：

- 目录中的关联 registration rows 用于验证密度、筛选和 `Profile=null` 的治理入口，不代表第二个 Service Profile 已完成；
- Overview dependency 的 `group_key/group_label` 是服务端投影事实，浏览器不按实体名猜分组；
- Workspace 中无 signal 的 canonical/declared 节点和关系显示 Health Unknown，不因为拓扑存在而显示 Healthy；
- telemetry bundle timeout 只把受影响的 Golden/RED/USE、PLACE 观测与 Evidence 降级为 Unknown；已完成的 ACCEPT、后续 pending 阶段、declared topology 和已有 Fault 保留；
- 没有 timeseries samples 时不得绘制 sparkline。`sparkline` 字段只有在 fixture 或服务端实际提供 samples 时才能展示。

交接前或修改契约后运行：

```bash
python3 docs/contracts/service-observability/v1/verify_examples.py
```

脚本只读取设计文件；依赖 Python `jsonschema`，不会连接 OpenObserve、Prometheus 或 alert-system 数据库。

## 信任边界

- Profile 由平台发布并做 schema、引用完整性和兼容性校验；页面不得提交或覆盖 Profile。
- DiagnosticContext 由服务端依据登录身份、Service Registration、canonical topology 和当前数据源重新组装；客户端传入的角色与权限无效。
- Page View 由服务端从同一 DiagnosticContext 与领域摘要投影；浏览器只负责展示和交互，不重新计算 Health，不拼接 PromQL/SQL。
- EvidenceRef 只保存摘要、相关键和受控 locator；原始日志、指标和 Trace 留在 OpenObserve 等权威来源。
- ToolDescriptor 只能引用服务端查询模板，禁止在 Profile 中携带 SQL、PromQL、LogQL、Shell、凭据、URL Header、Prompt 或可执行代码。`source_adapter` 是 Tool Registry 中的受控 key，不在 schema 中写死供应商集合。
- 工具调用必须经过权限过滤、输入校验、时间窗与基数限制、超时、脱敏、审计。
- 每次调用的有效限制取 Profile、ToolDescriptor、当前权限策略和系统全局限制中的最小值。
- 缺失、超时或过期数据为 Unknown；不得因为没有查到数据而显示 Healthy。
- E2B 的 metrics 默认由 Prometheus-compatible adapter 查询，logs/traces 由 OpenObserve adapter 查询；采集可双写，但页面语义不得依赖单一产品名。

## 版本与演进

- schema_version 表示契约主版本；不兼容变更必须新增版本目录。
- profile_version 表示单个 Service Profile 的发布版本。
- Tool key 的末尾版本表示工具语义主版本，semantic_version 描述完整版本。
- Profile 只能引用当前 registry 已发布并获授权的 tool key。
- v1 在 E2B 和至少一个第二服务上验证前保持 Draft。

## E2B 集成原则

E2B 通过 Profile 表达 sandbox_id、Sandbox 创建 journey、组件拓扑与信号绑定。alert-system 的 renderer、领域模型和 tool 调用路径不得包含 E2B 专属分支。E2B dashboard 只提供带 service/focus 的深链；生产页面、权限、Fault/Impact/Incident 仍由 alert-system 承载。

## Agent 预留

未来可以在 DiagnosticContext 与当前用户允许的 ToolDescriptor 之上增加 Assistant Provider 端口，但该端口必须是可拔插消费者：关闭、故障或替换 Agent 时，Service Workspace、证据、Fault 和 Incident 均正常工作。任何写动作需要新的 ADR 与独立授权流程。
