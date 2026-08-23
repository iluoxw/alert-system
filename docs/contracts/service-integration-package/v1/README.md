# Service Integration Package 契约 v1

## 状态与边界

本目录是 [ADR-0007](../../../adr/0007-declarative-service-integrations-and-governed-observed-topology.md)
决定的 **Service Integration Package（SIP）** 机器契约，状态为 Draft。

SIP 是**声明式接入单元**，不是动态执行插件：它是静态、可校验、可审查的 JSON 文档，
在发布前由服务端 compiler 原子编译成使用真实 ID 的
[Service Observability Profile v1](../../service-observability/v1/service-observability-profile.schema.json)
与 server-only `CompiledIntegration`。Profile 是唯一可进入 Page View/浏览器的契约；
CompiledIntegration 只供受信 adapter 读取 typed provider bindings。运行时加载 manifest 配对的
两份编译产物，SIP 原文不参与请求路径。

本阶段不定义、也不实现 Agent、模型、Prompt、会话、写操作或通用 Tool Execute API。

## 为什么单独一个契约目录

`docs/contracts/service-observability/v1/` 是**浏览器与运行时消费**的五份契约（Profile、
DiagnosticContext、Page View、EvidenceRef、ToolDescriptor），它们与 `internal/serviceobs/contract`
的 embedded 镜像逐字节同步，并由 `verify_examples.py` 与 `TestEveryCanonicalSchemaIsEmbedded`
两侧守护。SIP 是**编写与发布期**的产物，有自己的版本线（package 可以升级而不改 Profile v1），
因此放在独立目录，避免与那五份运行时契约互相牵连。

两者的关系是单向的：SIP → compiler → `Profile v1 + CompiledIntegration`。Profile v1 的字段
语义不因 SIP 改变，CompiledIntegration 不属于浏览器契约，也不得由 API 序列化。

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
- 编译出的 Profile 继续过现有 schema、结构、Tool catalog 与 resolver 校验；service/root 必须
  同环境（或显式 global）且 root 从 service entity 有界可达；
- CompiledIntegration 中只保留 resolved IDs、typed bindings、topology policy 与由受信 registry
  推导的 source/capability/permission，不保留 entity-key fallback。

## draft 与 published

`draft` package 只能编出配对的 draft 产物，因此**不可能**被 Registry 选为 active published
bundle。`status: published` 只是发布意图，不是自授权：在 Phase 3 Task 9 定义并验证 discovery
digest 与 owner approval attestation 前，`Compiler.AllowPublished` 默认为 false，且不能由普通运行
配置打开；compiler 因此拒绝 published package。只有全部 required
binding 的 metric/label/stream 经 discovery 确认、owner 对 threshold/proxy 签字且发布 gate 显式
授权后，才允许生成并原子激活 published bundle。

两份 example 都是 `draft`：它们的 metric 名取自 OTel spanmetrics connector 与
node_exporter 的上游默认形状，尚未经 discovery 确认，不是生产事实。

## 有意缺席的槽位

E2B package 刻意**不声明** USE Saturation 与 USE Errors：placement admission wait/queue 的
直接指标尚未确认存在。缺席后该槽位保持 Unknown，这比复用 utilization 冒充 saturation
更诚实。Golden Saturation 用宿主 HugePages 使用率，并带 `measurement_role: proxy` 与
`proxy_reason`，页面必须显式标注为容量压力 proxy。

## Provider template 边界

`provider_bindings[].template_ref` 只能引用服务端**受信 provider template registry**里已注册的
模板。每条 binding 显式携带 `key`、`kind`、`target_ref`、`tool_key`、`template_ref` 与封闭的
typed params；`kind` 仅允许：

- `signal`：target 指向 `signal_bindings[].key`，每个 signal 恰好一个 provider binding；
- `focus`：target 指向 `focus_kinds[].key`，同一 focus kind 至多一个；
- `journey`：target 指向 `journeys[].key`，每个 journey 恰好一个；
- `evidence`：target 指向用于参数化查询的 `focus_kinds[].key`，logs/traces/access class 由受信
  template registry 推导；同一 focus 可以各有一条 logs、traces、access binding，只有相同
  `(target_ref, derived evidence class/source key)` 的重复项才拒绝。

Observed topology hints 是 trace template 的声明输出，由 `topology_policy` 约束，不另设会重复同一
trace 查询的 `topology` binding。请求 purpose 不属于 SIP；它只能由服务端路由选择。registry
还声明 source capability、required permission、允许的 kind、参数槽位、主输出与固定 side outputs。

多个来源各自报告 focus 结论是**正常**的：coordinator 按 `matched > unknown > empty` 合并它们，
因此 logs 与 traces 都带 focus side output 不冲突。真正被拒绝的是**显式** `kind=focus` binding
与任何 side output 提供者共存——那是同一判定的第三次查询。同理，如果 journey 已经用一次 trace
读取顺带产出 trace EvidenceRef，再显式绑一条同 focus 的 traces evidence 也会被拒绝。
Binding kind 只标识主要 target；focus、trace EvidenceRef、topology hints 等 side outputs 必须由
adapter 在同一次有界读取中投影，package 不能自行增加，也不能为同一 trace 重复发起查询。compiler 必须
验证 `tool_key` 已发布且属于 package `tool_refs`、template capability 等于 ToolDescriptor 的
`source_adapter`、template permission 被 descriptor 覆盖；`signal_bindings[].output_field` 必须是
对应模板声明的输出字段。

编译结果的 `CompiledIntegration` 保存解析后的 target、typed params 与由 registry 推导的 source
key/capability/permission，并与 Profile 通过 manifest 中的 `profile_digest` / `integration_digest`
配对。缺任一产物、摘要错配或单独激活必须 quarantine 整对。

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

每个模板的 capability、source key 与 required permission 都是 registry 的既有事实，由平台维护。
它们不会因为某个接入方的逐条字段恰好也出现在另一个流里就改变——那等于把单个服务的数据布局
写进平台契约。要改动既有权限语义必须有权威文档或模板输出 schema 的明确依据。

`counter_ratio_percent.v1` 使用 `common_labels`、`numerator_labels`、`denominator_labels` 三组
typed 等值条件。任一有效 selector 内 label 名重复，或 numerator/denominator 是同一 metric 且
两边的有效 label selector 完全相同时，compiler 必须拒绝；否则会把任意非零样本恒算成 100%。E2B draft
中的 error 判别 label 仅表达上游默认意图，真实字段尚未经 discovery 确认，不能宣称是真实错误率。

## 认证与 secret 边界

Provider 认证模式显式为 `none|basic|bearer`（OpenObserve 实测是 Basic `user:password`，
因此不能只提供 bearer token）。配置里只写**引用**——env 名或文件路径，两者互斥；凭据值由被引用
的 env 或文件提供，文件必须是常规文件且不超过 4 KiB。endpoint 未配置却声明凭据会让启动失败。

配置结构的 `String`/`GoString`/`MarshalJSON`/`LogValue` 全部只输出安全表示：连 username 与
organization 都不输出，因此把整份配置打进日志或做一次 `json.Marshal` 都不会泄露。生产仍然只接受
HTTPS，没有任何 YAML 或环境变量开关能切换到明文；UAT 仅 HTTP 是外部发布 blocker，不在本仓库
加绕过。

## 校验

Go 侧：

```bash
go test ./internal/serviceobs/integration/...
```

它覆盖 schema 与本目录的逐字节同步、两份 example 的双产物确定性编译与 digest 配对、selector
零命中/多命中/环境错配/不可达、provider target 与 capability/permission 错配、未知 template、
ratio 退化、参数错配、禁止键、自由 matcher 与 draft-only 发布门槛。
