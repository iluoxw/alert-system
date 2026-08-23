# Service Integration Package 契约 v1

## 状态与边界

本目录是 [ADR-0007](../../../adr/0007-declarative-service-integrations-and-governed-observed-topology.md)
决定的 **Service Integration Package（SIP）** 机器契约，状态为 Draft。

SIP 是**声明式接入单元**，不是动态执行插件：它是静态、可校验、可审查的 JSON 文档，
在发布前由服务端 compiler 编译成使用真实 ID 的
[Service Observability Profile v1](../../service-observability/v1/service-observability-profile.schema.json)。
运行时只加载已编译的 v1 Profile，SIP 本身不参与请求路径。

本阶段不定义、也不实现 Agent、模型、Prompt、会话、写操作或通用 Tool Execute API。

## 为什么单独一个契约目录

`docs/contracts/service-observability/v1/` 是**浏览器与运行时消费**的五份契约（Profile、
DiagnosticContext、Page View、EvidenceRef、ToolDescriptor），它们与 `internal/serviceobs/contract`
的 embedded 镜像逐字节同步，并由 `verify_examples.py` 与 `TestEveryCanonicalSchemaIsEmbedded`
两侧守护。SIP 是**编写与发布期**的产物，有自己的版本线（package 可以升级而不改 Profile v1），
因此放在独立目录，避免与那五份运行时契约互相牵连。

两者的关系是单向的：SIP → compiler → Profile v1。Profile v1 的字段语义不因 SIP 改变。

## 文件

| 文件 | 作用 |
|---|---|
| service-integration-package.schema.json | SIP v1 的唯一权威 schema |
| examples/e2b.package.json | E2B 首个 package（draft）；对应 Phase 3 Task 9 的 discovery 输入 |
| examples/postgresql.package.json | 结构不同的最小第二 package（draft）；证明契约不是为 E2B 定制 |

Go 侧镜像与 compiler 在 `internal/serviceobs/integration/`。schema 以 `go:embed` 编进
binary，并有 sync 测试与本目录逐字节比对，防止两份定义漂移。

## 硬性禁止项

schema 的每一层对象都是 `additionalProperties: false`，因此下列内容在**语法层面**就无法
出现在 package 里；compiler 另有一层递归键名黑名单作为纵深防御：

- 原始 SQL、PromQL、LogQL 或任何查询文本；
- Go/JS/WASM、Shell、HTML、Prompt 或其它可执行代码；
- endpoint、URL、host、Authorization header、token、密码或任何凭据；
- 浏览器 renderer 上传（renderer 只能从服务端已实现的枚举里选）；
- 未经 allowlist 的字段名、adapter key 或 template ref。

### label 只能是等值字面量

`params.labels` 是 `{name, value}` 数组。`name` 必须是 identifier，`value` 的字符集是
`[A-Za-z0-9_.:/@+-]`——里面没有 `*`、`~`、`!`、`|`、`(`、`)`、`{`、`}`、引号、反斜杠和空白，
所以 regex（`.*`）、否定（`!=`、`!~`）、集合选择器和任何自由 matcher 表达式都无法被表达。
这是 ADR-0007 §3 的要求：SIP 只能选模板并传字面参数，PromQL/SQL 由服务端 adapter 构造。

### 真实 ID 由 compiler 解析，package 只写稳定 entity_key

`target.service_selector`、`root_selectors` 以及 signal scope / journey stage 的引用全部使用
`entity_key`（与 alert-system 写入侧逐字一致的 `^[a-z0-9][a-z0-9._:/-]{2,127}$`），**不接受
UUID**。UUID 每个环境都不同，写进 package 就不可移植；而运行时 Profile 的
`service_ref.service_id` / `root_entity_ids` 又必须是真实的
`service_registrations.id` / `resource_nodes.id`。这个矛盾只能在发布前编译解决：

- 每个 selector 必须在目标环境**唯一命中**；
- 零命中或多命中一律 fail closed，不按名称猜、不做 entity_key fallback、也没有任何
  fixture ID 兜底路径；
- 编译产物继续过一遍现有 Profile schema、结构校验与 Tool catalog 校验。

## draft 与 published

package 的 `status` 决定编译产物的 Profile `status`：`draft` package 只能编出 `draft`
Profile，因此**不可能**被 Profile Registry 选为某个服务的 active published Profile。
只有全部 required binding 的 metric/label/stream 经运行时 discovery 确认、并拿到 owner
对阈值与 proxy 的签字之后，package 才允许改为 `published`（Phase 3 Task 9 的发布 gate）。

两份 example 都是 `draft`：它们的 metric 名取自 OTel spanmetrics connector 与
node_exporter 的上游默认形状，尚未经 discovery 确认，不是生产事实。

## 有意缺席的槽位

E2B package 刻意**不声明** USE Saturation 与 USE Errors：placement admission wait/queue 的
直接指标尚未确认存在。缺席后该槽位保持 Unknown，这比复用 utilization 冒充 saturation
更诚实。Golden Saturation 用宿主 HugePages 使用率，并带 `measurement_role: proxy` 与
`proxy_reason`，页面必须显式标注为容量压力 proxy。

## Provider template 边界

`telemetry_bindings[].template_ref` 只能引用服务端**受信 provider template registry**
里已注册的模板。registry 持有每个模板的：适配器能力、必填/可选参数槽位与类型、以及可用的
输出字段名。`signal_bindings[].output_field` 必须是所引用模板声明的输出字段之一。

模板对应的真实 PromQL/SQL 构造属于 Phase 3 Task 4/5 的 adapter，不在本契约内，也不会
出现在 package 或浏览器里。

首批模板：

| template_ref | adapter capability | 用途 |
|---|---|---|
| counter_rate.v1 | prometheus_compatible | counter 的区间速率 |
| counter_ratio_percent.v1 | prometheus_compatible | 两个 counter 的百分比 |
| histogram_quantile_rate.v1 | prometheus_compatible | histogram 分位数 |
| gauge_ratio_percent.v1 | prometheus_compatible | 两个 gauge 的百分比 |
| gauge_max.v1 | prometheus_compatible | gauge 的最坏值 |
| focus_records.v1 | openobserve | 按 focus 逐条取记录 |
| trace_by_focus.v1 | openobserve | 按 focus 取 trace 与 span |
| logs_by_focus.v1 | openobserve | 按 focus 取日志摘要 |
| access_by_focus.v1 | openobserve | 按 focus 取 access 记录摘要 |

## 校验

Go 侧：

```bash
go test ./internal/serviceobs/integration/...
```

它覆盖 schema 与本目录的逐字节同步、两份 example 的编译、selector 零命中/多命中、未知
template、参数错配、禁止键、自由 matcher 与 draft/published 门槛。
