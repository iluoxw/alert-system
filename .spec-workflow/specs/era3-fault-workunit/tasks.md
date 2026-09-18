# Tasks Document — era3-fault-workunit

> **依赖顺序：P0 文档固化 → P1 身份对齐/拓扑导入 → P2 Fault 工作单元 → P3 Grafana 权威源 → P4 健康证据与影响 → P5 指标与可靠性。**
> `P1 ∥ P2 ∥ P3` 可并行；`P4` 依赖 `P1 + P2`；`P5` 依赖 `P2`。
> 迁移编号由主对话统一分配，本 spec 占用 **000026 / 000027 / 000028**，并行分支不得自行占号。
> 所有任务遵守 `design.md` 的目录结构与依赖方向，以及 `requirements.md` 的「硬约束」表。
> 每阶段收尾必跑：`go build ./... && go vet ./... && go vet -tags=integration ./...`、`go test -tags=integration ./... -race -count=1`、`cd web && npm run build && npx oxlint`。

## P0 文档固化（已完成）

- [x] 1. 新建 ADR-0006 决策文档
  - File: docs/adr/0006-fault-work-unit-and-platform-topology.md
  - 三根支柱、一条边界、八项裁决表、health 时间约束、ccloud 契约 7 条坑、外部 IO 执行位置、Consequences、Alternatives Considered
  - Purpose: 把方向讨论与八项裁决固化为不可再议的决策权威
  - _Leverage: docs/plans/2026-09-17-era3-direction-analysis.md_
  - _Requirements: 全部（本 spec 的决策来源）_
  - _Prompt: 不适用（文档任务已完成，保留以便追溯）_

- [x] 2. 方向文档状态同步与裁决列回填
  - File: docs/plans/2026-09-17-era3-direction-analysis.md
  - 头部改「已裁决」；§6.3 表格增加「裁决」列（D1→进入 v0.3、D2→active 即通知、D3→事务内产意图仅 Schedule、D4→飞书卡片先行、D5→冻结不删、D6→先量先行指标）
  - Purpose: 让探索文档与最终裁决不冲突，冲突时以 ADR 为准
  - _Leverage: docs/adr/0006-fault-work-unit-and-platform-topology.md_
  - _Requirements: 全部_
  - _Prompt: 不适用（文档任务已完成，保留以便追溯）_

- [x] 3. CLAUDE.md 新增不变量 22–24
  - File: CLAUDE.md
  - 22 Fault 是一等工作单元（归属/认领/通知门槛独立/冷却/拓扑抑制）；23 外部 IO 只走独立 loop 不新增带 IO 的 timer kind；24 Alert 生命周期层唯一且时代一冻结
  - Purpose: 让后续所有实现受同一组硬约束约束
  - _Leverage: 现有不变量 1–21 的写法与粒度_
  - _Requirements: R2.9, R5, R6.8, R7_
  - _Prompt: 不适用（文档任务已完成，保留以便追溯）_

## P1 身份对齐 + ccloud 拓扑导入

- [x] 4. config 增加 ccloud 配置项
  - File: internal/config/config.go, internal/config/config_test.go
  - 新增 CCLOUD_BASE_URL、CCLOUD_TOKEN_ENV（存放 token 的环境变量名，不直接存 token）、CCLOUD_PROJECT_IDS（逗号分隔）、CCLOUD_SYNC_INTERVAL（默认 10m，**0 = 完全关闭且不产生任何 IO**）
  - **实现裁决（2026-09-19）**：原文"默认 10m"与"interval>0 且缺 BASE_URL 时启动报错"互相矛盾（会让所有未接入 ccloud 的部署启动失败）。
    落地口径改为**以 BASE_URL 是否配置作为启用开关**：`CCloudConfig.Enabled() = BaseURL 非空 && SyncInterval > 0`；
    未配 BASE_URL 视为未接入、零影响；配了 BASE_URL 却缺 PROJECT_IDS 在 `config.Load` 即失败；interval=0 为显式关闭
  - Purpose: 让 ccloud 接入可整体开关，且 token 不落配置与日志
  - _Leverage: 现有 config 的 env 解析与 DSN 密码掩码口径_
  - _Requirements: R2.1, NFR-Security_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 在 internal/config 增加 ccloud 相关配置项，SYNC_INTERVAL 为 0 时整条链路短路；TOKEN_ENV 只存环境变量名 | Restrictions: 不引入新配置框架；token 值不得出现在日志或结构体 String() 输出；缺 BASE_URL/PROJECT_IDS 且 interval>0 时启动期报错 | Success: 单测覆盖 interval=0 短路、缺配置报错、token 不在错误信息中 | 完成后按工作流在 tasks.md 标记状态并用 log-implementation 记录_

- [x] 5. 实现 ccloud 客户端与 DTO
  - File: internal/ccloud/client.go, internal/ccloud/client_test.go
  - **先读 `internal/ccloud/testdata/README.md`**：DTO 必须按该目录两份真实响应定义；关键点——`Health *float64`（`null` 与 `0.0` 都是合法值且语义不同）、`nodeType` 可空、`serviceType` 有前导空格、`metadata` 可为 `null`、外层 `message` 可空且两视图字段不同（全链路多 trace 字段）
  - FetchFullLinkTopology(projectID)、FetchServiceTopology(appID)、FetchAppHealth(appIDs)；内置字段只留 from/to/totalCount 与节点分类所需字段，**不解析 `coreMetrics`**（两视图的量纲/大小写/顺序不一致，不得用于判定）
  - Purpose: 隔离平台契约变化，使 importer 能纯函数单测
  - _Leverage: internal/alertsource/client.go 的 HTTP + BearerTokenEnv 口径_
  - _Requirements: R2.1, R2.3_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师（HTTP 客户端） | Task: 定义 ccloud 三个接口的 DTO 与 Fetch 方法，全部带 context 与超时；只暴露 from/to/totalCount，不解析 edge.key 与 sourceInfo/targetInfo 名字 | Restrictions: 包内不得 import store；不得自行发明字段名，按真实响应定义；错误必须可区分超时与业务错误 | Success: httptest 假服务下单测通过，超时返回明确错误，DTO 可正确反序列化平台样例 JSON | 完成后按工作流标记状态并 log-implementation_

- [x] 6. 实现拓扑映射（纯函数）
  - File: internal/ccloud/importer.go, internal/ccloud/importer_test.go
  - MapTopology(in, svc, resolve AliasResolver) ImportResult：**分类判据 `nodeType` 非空时用它、为空回落 `serviceType`**（`EXTERNAL_HTTP_SERVER` 节点 nodeType 为空），比较前 `TrimSpace`；CCLOUD_APP→application(source=ccloud, external_ref=node.key, alias ns ccloud.app_id)；APISIX/WAF→platform；RELY_NODE→**用 `node.key` 锚定**，合法 host:port 时同时写 `ccloud.rely_node` alias，未命中→Unresolved（value=node.key，附 host_port/service_type）；EXTERNAL_HTTP_SERVER 忽略并**计入 `DroppedExternalHTTP`**（非零说明该类型开始带边，需重新评估）；边方向 **from/to 与 consumer→provider 同向，直接照抄不翻转**；totalCount=0→Stale=true；自环丢弃；**CCLOUD_APP 若已有 service_registrations 登记 → alias 合并到已有 application 实体（不新建）；未登记 → 保留 unmanaged 待认领，不建团队、不改归属**
  - Purpose: 把平台契约坑固化为可测的纯函数，避免同步路径出错
  - _Leverage: internal/domain（不引入 store）；design.md 的 Code Reuse Analysis_
  - _Requirements: R1.1, R1.2, R1.7, R2.3, R2.4, R2.5, R2.8_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 领域建模工程师 | Task: 实现 MapTopology 纯函数，表驱动测试覆盖方向取自 from/to、自环丢弃、totalCount=0 标 stale、`:0` 进 Unresolved、同输入重放结果一致 | Restrictions: 不得 import store 或 database 相关包；不得写库；不得依据 info 中的名字推断身份 | Success: 单测全绿且耗尽平台样例数据；Unresolved 不产生任何实体 | 完成后按工作流标记状态并 log-implementation_

- [ ] 7. 迁移 000027：unresolved_identities
  - File: migrations/000027_ccloud_topology.up.sql, migrations/000027_ccloud_topology.down.sql
  - 建 unresolved_identities(namespace, value, first_seen, last_seen, sample_app, PRIMARY KEY(namespace, value))；确认 topology_sync_runs 是否已存在（ImportSnapshot 已在用），需要时补 source='ccloud' 枚举约束
  - Purpose: 承载 R1 的待对齐队列
  - _Leverage: 现有迁移风格（去掉枚举约束、CHECK 而非 PG ENUM）_
  - _Requirements: R1.2, R1.3_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: PostgreSQL DBA | Task: 编写 000027 迁移建 unresolved_identities，主键 (namespace, value) 以支持 UPSERT 幂等；先核对 topology_sync_runs 现有定义再决定是否改动 | Restrictions: 编号必须为 000027；up/down 必须可往返；不得改动其他迁移 | Success: 迁移往返执行通过，ON CONFLICT UPSERT 单测不产生重复行 | 完成后按工作流标记状态并 log-implementation_

- [ ] 8. 扩展 TopologySnapshotEdge 支持跨源目标节点
  - File: internal/domain/topology.go, internal/store/infrastructure.go, internal/store/infrastructure_integration_test.go
  - 新增 `ToNodeID *uuid.UUID`；ImportSnapshot 在 nodeIDs[external_ref] 查不到时回落 ToNodeID，用于表达 App→canonical RELY_NODE 跨源边
  - Purpose: 让平台 observed 边能指向 entity_registry 已存在的实体
  - _Leverage: 现有 ImportSnapshot（已含 topology_sync_runs + 按 source 删旧边）_
  - _Requirements: R1.1, R2.2, R2.6, R2.7_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端 + PostgreSQL | Task: 给 TopologySnapshotEdge 增加 ToNodeID 回落逻辑，保持既有 external_ref 路径行为逐字节不变；集成测验证按 source 隔离（ccloud 替换不动 entity_registry 边）与重放幂等 | Restrictions: 不改变现有函数签名语义；同一 tx 内二阶段查询必须先完整扫描并 Close 再发子查询（不变量 20） | Success: 集成测通过；重放同一快照边数不变；entity_registry 边未被触碰 | 完成后按工作流标记状态并 log-implementation_

- [ ] 9. 实现 sync 周期 loop 并接入装配
  - File: internal/ccloud/syncer.go, internal/app/app.go, cmd/server/main.go, test/e2e/harness.go
  - 模式同 internal/alertsource.Reconciler：事务外 fetch（全链路 + 逐应用服务拓扑）→ 短事务 ImportSnapshot；失败写 topology_sync_runs 并退避；启动即跑一轮
  - Purpose: 让拓扑图自动存在，不依赖手工触发
  - _Leverage: internal/alertsource.Reconciler、store.TopologyRepo.ImportSnapshot_
  - _Requirements: R2.1, R2.2, R2.9, R1.6, NFR-Reliability_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师（并发/调度） | Task: 以 alertsource.Reconciler 为模板实现 ccloud syncer 周期 loop 并在 internal/app 装配，interval=0 时不启动；失败退避且不产生重试风暴 | Restrictions: 不得使用 scheduler timer（不变量 3/23）；外部 IO 必须在事务外；不得 fail fast 阻塞启动 | Success: 单测/集成测验证 interval=0 不启动、失败退避、单轮幂等；main.go 与 e2e harness 均正确启动 | 完成后按工作流标记状态并 log-implementation_

- [ ] 10. 待对齐身份 API 与前端入口
  - File: internal/api/topology.go, internal/api/router.go, web/src/pages/Settings/ServiceCatalog.tsx
  - GET /api/v1/admin/topology/unresolved（admin only，按 last_seen 倒序分页，空集合返 []）；控制中心「服务目录」新增 Tab「待对齐身份」，操作=选择 canonical 实体创建别名（复用受管实体 PUT aliases）
  - Purpose: 让 SRE 能一次性把未对齐 RELY_NODE 补成别名，补完下一轮自动并入
  - _Leverage: internal/api/service_catalog.go 的受管实体 PUT、目录驱动可搜索选择器_
  - _Requirements: R1.4, R1.6_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go + React/TS 全栈 | Task: 实现 admin 端点与前端 Tab，别名创建复用既有受管实体 aliases PUT，不新增写入路径 | Restrictions: 端点必须 admin only；空集合返 [] 不返 null；选择器必须目录驱动可搜索，禁止手输 UUID；不得新增一级导航 | Success: 后端单测覆盖鉴权与空集合；前端 build + oxlint 通过；创建别名后下一轮同步即并入 | 完成后按工作流标记状态并 log-implementation_

- [ ] 11. P1 验证：映射单测与 source 隔离集成测
  - File: internal/ccloud/testdata/\*.json, internal/ccloud/importer_test.go, internal/store/infrastructure_integration_test.go
  - 用平台真实样例 JSON 作 fixture 的表驱动单测（方向、自环、totalCount=0、`:0` unresolved、重放幂等）；集成测验证 replace 按 source 隔离
  - **fixture 已就绪**（2026-09-18）：`internal/ccloud/testdata/fulllink_topology.json`（24 节点/77 边）、`internal/ccloud/testdata/service_topology_cplm-boot-prod.json`（22 节点/12 边），契约事实与证据见同目录 `README.md`
  - 断言矩阵必须覆盖 README 的契约事实：key 方向两视图相反（不可信）、from/to 照抄不改向、自环 2 条（其中一条 totalCount=18237 有流量）、totalCount=0 共 40 条、`lcp-app` 同名不同 key、`PRJ-INFO-PROD（cplm-ipm）` 全角名、`" POSTGRESQL"` 前导空格、`EXTERNAL_HTTP_SERVER` 的 `nodeType` 为空、`health` 的 `null` 与 `0.0` 各自语义、`:0` Redis 进 Unresolved 且 value=node.key、appId 24/40 两种长度均被接受
  - Purpose: P1 的验收闸门
  - _Leverage: test/e2e 与 testcontainers 既有基建_
  - _Requirements: R1.2, R1.3, R2.3–R2.7_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: QA 工程师（Go） | Task: 把平台真实响应落盘为 testdata fixture 后补齐 P1 单测与集成测，覆盖 requirements R1/R2 的全部验收条件 | Restrictions: 集成测必须带 //go:build integration 标签；不得依赖真实 ccloud 网络；fixture 不得脱敏掉 importer 需要断言的字段（from/to/totalCount/`:0`） | Success: `go test -tags=integration ./internal/... -race -count=1` 全绿，fixture 已入库 | 完成后按工作流标记状态并 log-implementation_

## P2 Fault owner / 认领 / 通知

> **范围调整（2026-09-18 用户裁决）**：飞书交互卡片的故障动作（原任务 19）延后至「顺延与人工动作」，
> 阻塞于 uat 尚无飞书可访问的公网回调入口。P2 的认领路径为 REST 端点 + Web；通知走 `lark_webhook` 纯文本。
> 受影响的验收条件 R4.6、R5.7、R5.9 已在 requirements.md 标注延后。

- [x] 12. 迁移 000026：Fault 归属/认领字段与 fault_timeline_entries
  - File: migrations/000026_fault_workunit.up.sql, migrations/000026_fault_workunit.down.sql
  - faults 增 owner_team_id / acked_by / acked_by_name / acked_at / owner_notified_at + 索引 (status, owner_team_id)；新建 fault_timeline_entries（**全库已核实无 fault 时间线表**：Alert 用 000001 timeline_entries，Incident 用 000013 incident_timeline_entries）
  - Purpose: 承载 R3/R4 的持久化
  - _Leverage: 000013 incident_timeline_entries 结构_
  - _Requirements: R3.1, R4.1, R4.4_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: PostgreSQL DBA | Task: 编写 000026 迁移，ackinfo 与 owner 字段可空，fault_timeline_entries 结构对齐 incident_timeline_entries | Restrictions: 编号必须为 000026；up/down 可往返；外键齐全；不得修改既有迁移 | Success: 迁移往返通过，索引经 EXPLAIN 可用于 (status, owner_team_id) 查询 | 完成后按工作流标记状态并 log-implementation_

- [x] 13. domain 层 Fault 认领语义
  - File: internal/domain/fault.go, internal/domain/fault_test.go
  - Fault 增 owner/acked 字段；AckFault(fault, actorID, actorName, now) 返回时间线条目；已 ack 短路不重写
  - Purpose: 把幂等口径固化为纯函数，供 REST 与飞书回调共用
  - _Leverage: domain.Ack(alert, actorID, actorName, now) 的双参数口径（不变量 7）_
  - _Requirements: R4.2, R4.3_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 领域建模工程师 | Task: 实现 AckFault 纯函数，actorID 进 UUID 字段、actorName 进时间线；重复 ack 短路返回当前状态 | Restrictions: domain 零外部依赖；不得触碰存储；不得返回 409 语义 | Success: 表驱动测试覆盖首次 ack、重复 ack、空 actorName 校验 | 完成后按工作流标记状态并 log-implementation_

- [x] 14. mapping projector 写入 owner 并暴露 transition
  - File: internal/mapping/projector.go, internal/mapping/projector_test.go
  - Upsert Fault 时 owner_team_id 取 resource_nodes.owner_team_id（解析不到置 NULL）；返回值带上 transition 信息（created / candidate→active）
  - Purpose: 让 owner 自动就位，并给通知触发提供判定依据
  - _Leverage: 现有 projector 的 Upsert 路径与 Fault 生命周期判定_
  - _Requirements: R3.2, R3.3, R5.1_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 在 projector 写入 owner_team_id 并返回 transition，供 stage_mapping 判断是否进入 active | Restrictions: 不得依据 Alert 标签或平台数据推断 owner（不变量 15）；不得在 projector 内做任何外部 IO | Success: 单测覆盖有 owner / 无 owner / candidate→active / 已 active 不重复触发四种情形 | 完成后按工作流标记状态并 log-implementation_

- [ ] 15. FaultRepo 扩展
  - File: internal/store/faults.go, internal/store/faults_integration_test.go
  - 新增 Ack、SetOwnerNotified、AppendTimeline、ListRecentlyResolvedByCorrelationKey(since)、ListDependencyProvidersByNodeID（如缺失）
  - Purpose: 支撑认领、通知冷却与拓扑抑制
  - _Leverage: 现有 FaultRepo 方法风格（全部带 tx pgx.Tx）_
  - _Requirements: R4.1, R4.4, R5.3, R5.4, R5.6_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go + PostgreSQL 工程师 | Task: 实现五个仓储方法，全部带 tx pgx.Tx，调用方开事务 | Restrictions: 方法内不得自开事务；不得在仍打开的 Rows 内嵌套查询（不变量 20）；能一次 JOIN 就一次取回 | Success: 集成测覆盖 Ack 幂等、时间线追加、按 correlation_key 查冷却窗口、按 node 查上游 provider | 完成后按工作流标记状态并 log-implementation_

- [ ] 16. 实现 fault_outbox.QueueFaultActivated
  - File: internal/app/fault_outbox.go, internal/app/fault_outbox_test.go
  - 五步判定：①同 correlation_key 冷却窗口内刚 resolved → 跳过；②上游 provider 已有 active 且 owner_notified → 不单发并写时间线原因；③目标取 owner team team_ops_settings.channels，缺失回落 FAULT_NOTIFY_FALLBACK_TARGET；④Schedule 一条 notify_send（幂等键 fault:{id}|activated|{channel}|{target}|{round}，round 为意图时刻并随 payload 持久化）；⑤SetOwnerNotified
  - Purpose: R5 的核心实现，同时保证不变量 4 不被破坏
  - _Leverage: internal/app/incident_outbox.go（非 Alert 对象走 notify_send 的先例）、internal/app/outbox.go 的 notifySendRefID_
  - _Requirements: R5.1, R5.2, R5.3, R5.4, R5.5, R5.6, R5.8_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师（异步通知编排） | Task: 以 IncidentOutbox 为模板实现 QueueFaultActivated，五个判定步骤各自可单测，只 Schedule 不发送 | Restrictions: 事务内禁止任何同步外部 IO；round 必须随 payload 持久化而非发送时现算；refID 不含 round | Success: 单测覆盖冷却跳过、上游抑制、owner 渠道缺失回落、正常调度一次；重复调用同 Fault 不产生第二条 pending | 完成后按工作流标记状态并 log-implementation_

- [ ] 17. 接入通知触发点（覆盖 Project 的全部调用入口）
  - File: internal/pipeline/stage_mapping.go, internal/pipeline/stage_mapping_test.go, internal/alertsource/reconciler.go, internal/alertsource/reconciler_test.go, internal/api/service_catalog.go
  - Project 返回 active 转移后、在同一 tx 内调用 QueueFaultActivated
  - **`Project` 有三个生产调用点，触发必须齐全**（已核实：`stage_mapping.go:30`、`reconciler.go:160` 与 `:172`、`service_catalog.go:484`）：
    pipeline 触发；对账两处均触发（Alertmanager 不可用恢复后的 Fault 只经此路径产生，漏触发等同漏报）；
    reprocess 端点**不触发**（治理操作），并在 log-implementation 中记录该结论
  - 触发逻辑写在调用方，不要下沉进 projector——projector 当前零通知依赖是不变量 4/9 成立的基础
  - Purpose: 让 Fault 进入 active 的瞬间产生通知意图，且不因入口不同而漏发
  - _Leverage: 现有 stage_mapping 的事务内职责边界；任务 14 的 transition 返回值_
  - _Requirements: R5.1, R5.10, R5.11, D3（裁决 3）_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 在 stage_mapping 与 alertsource.Reconciler 的 active 转移分支调用 fault_outbox，保持同事务只 Schedule；reprocess 端点显式不触发并注释说明 | Restrictions: 不得让 stage 或 reconciler 事务内出现外部 IO；不得把通知耦合进 projector；不得改变既有 stage 顺序与 dedup→mapping→route→group→silence→inhibit 语义；authoritative-safe 来源行为不变 | Success: 单测验证三个入口各自行为（pipeline 触发、对账两处触发、reprocess 不触发），同一次 active 转移只产生一条意图 | 完成后按工作流标记状态并 log-implementation_

- [ ] 18. Fault 认领 REST 端点
  - File: internal/api/faults.go, internal/api/router.go
  - POST /api/v1/faults/:id/ack（登录即可）；列表与详情响应返回 owner/acked 字段
  - Purpose: 提供 Web 与第三方的认领入口
  - _Leverage: 现有 alerts Ack 端点的幂等短路与 200 返回语义_
  - _Requirements: R4.3, R4.5, R4.7_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师（HTTP 层） | Task: 实现 ack 端点，参数校验与幂等短路对齐现有 alert ack 语义 | Restrictions: 仅需登录不需 admin；重复 ack 返回 200 当前状态而非 409；错误分支不得重写时间线 | Success: 单测覆盖首次/重复 ack、未登录 401、不存在 404 | 完成后按工作流标记状态并 log-implementation_

> **任务 19（飞书卡片故障动作）已移至「顺延与人工动作」**：阻塞于 uat 尚无公网可回调入口，不属于 P2 范围。
> P2 期间认领经 REST 端点（任务 18）与 Web（任务 20）完成。

- [ ] 20. 前端 Fault 归属与认领
  - File: web/src/pages/Faults/FaultWorkspace.tsx, web/src/pages/Faults/FaultList.tsx
  - Workspace 顶部显示 owner 与认领状态 + 认领按钮（乐观更新后失效 React Query 缓存）；List 增加 owner 列
  - Purpose: 让归属与认领在 Web 可见可操作
  - _Leverage: 现有 React Query hooks 与 AntD 5 组件_
  - _Requirements: R4.5, R4.7_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: React/TS 前端工程师 | Task: 实现 owner 展示与认领交互，已认领状态禁用按钮 | Restrictions: 锁 AntD 5 不升级；不得引入 Redux/Mobx；空值展示不能导致页面崩溃 | Success: `npm run build` 与 `npx oxlint` 通过；认领后列表与详情同步刷新 | 完成后按工作流标记状态并 log-implementation_

- [ ] 21. P2 端到端验证
  - File: test/e2e/fault_notify_test.go
  - Alertmanager webhook → Fault active → 恰好一条 notify_send（owner team channel）→ **REST `POST /api/v1/faults/:id/ack`** → acked；重复 ack 200 短路；同 correlation_key 冷却内不重发；上游已通知时下游不发
  - 说明：卡片 ack 回调路径随任务 19 延后，本阶段不断言；E2E 用既有 mock 渠道验证发送意图，与 uat 选用哪条飞书通道无关
  - **对账路径断言（R5.10）**：模拟 Alertmanager 短暂不可用后恢复（告警在快照中重新出现，`unknown→firing`），
    断言该路径同样产生**恰好一条** notify_send；再对同一 Fault 手动 reprocess，断言**不产生**新的通知意图
  - Purpose: P2 的验收闸门
  - _Leverage: test/e2e 现有 Alertmanager 权威测试基建_
  - _Requirements: R3, R4, R5 全部验收条件_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: QA 工程师 | Task: 编写 P2 E2E，逐条断言 requirements R3/R4/R5 的验收条件 | Restrictions: 使用既有 Compose project/端口，单轮运行；不得并行跑第二轮 | Success: docker compose up -d 后 E2E 全绿 | 完成后按工作流标记状态并 log-implementation_

## P3 Grafana 权威源

- [x] 22. 权威来源常量集与替换硬编码
  - File: internal/domain/alert.go, internal/pipeline/stage_dedup.go, internal/mapping/projector.go, internal/store/alert_sources.go, internal/api/config.go
  - 新增 AuthoritativeSourceKinds = {alertmanager, grafana} 与 IsAuthoritativeSourceKind()；替换 4 个文件中的字面量（alert_sources.go 改 kind IN）
  - Purpose: 让第二条权威源只是配置问题，而不是到处加分支
  - _Leverage: internal/domain/alert.go 的 IsAuthoritativeSource() 已是宽松判定（SourceIntegrationID != "" && UpstreamFingerprint != ""），无需改动_
  - _Requirements: R6.1, R6.2_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师（重构） | Task: 抽出常量集并替换全部字面量出现点，行为对 alertmanager 逐字节不变 | Restrictions: 禁止保留任何散落字面量；不得改变现有 SQL 语义（= 改 IN）；不得顺手重构无关代码 | Success: `go build ./... && go vet ./...` 通过；既有单测与集成测全绿；grep 不到散落字面量 | 完成后按工作流标记状态并 log-implementation_

- [ ] 23. Grafana ingestion 适配器
  - File: internal/ingest/grafana.go, internal/ingest/grafana_test.go
  - Source="grafana"；UpstreamFingerprint 取 payload.alerts[].fingerprint；StartsAt/EndsAt 与 Alertmanager 同口径；标记为权威来源
  - Purpose: 让 Grafana Alerting（含 ES 日志告警）以同一契约进入
  - _Leverage: internal/ingest 现有 Alertmanager 适配器_
  - _Requirements: R6.3, R6.4, R6.5_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师（adapter） | Task: 实现 Grafana 适配器，重用 Alertmanager 的信封归一，只改 Source 与 fingerprint 来源 | Restrictions: 不得为 Grafana 新建独立的管道分支；不得发送 Alert 级通知；信封字段口径不得偏离 AM 适配器 | Success: 单测覆盖 firing/resolved 载荷、fingerprint 提取、缺 fingerprint 时的行为 | 完成后按工作流标记状态并 log-implementation_

- [ ] 24. 对账客户端按 kind 选择路径
  - File: internal/alertsource/client.go, internal/alertsource/client_test.go
  - 按 source.Kind 选路径：alertmanager `/api/v2/alerts`，grafana `/api/alertmanager/grafana/api/v2/alerts`；ValidateConfig 接受 grafana；鉴权沿用 BearerTokenEnv
  - Purpose: 让周期对账对两种来源都成立
  - _Leverage: internal/alertsource/client.go 现有实现与 reconciler（按 source 迭代，无需改动）_
  - _Requirements: R6.3, R6.7_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 增加按 kind 的路径选择与 ValidateConfig 分支 | Restrictions: 不改变 alertmanager 既有路径与鉴权；不得引入第二种认证方式 | Success: httptest 验证两种 kind 各自命中正确路径；Config 校验对未知 kind 仍拒绝 | 完成后按工作流标记状态并 log-implementation_

- [ ] 25. 校验 occurrence 幂等键对 Grafana 成立
  - File: internal/store/alert_sources.go, internal/store/alert_sources_integration_test.go
  - 确认 source_id + upstream_fingerprint + starts_at 对 Grafana 成立；如已成立则记录结论，不写代码
  - Purpose: 避免重复升级幂等键造成漏报或重复
  - _Leverage: 现有 occurrence 幂等实现_
  - _Requirements: R6.4_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 用集成测证明 Grafana 载荷下幂等键成立；若成立则在 log-implementation 中记录结论 | Restrictions: 除非测试证伪，否则不得改动幂等键口径 | Success: 集成测覆盖同 occurrence 重复投递不产生重复记录 | 完成后按工作流标记状态并 log-implementation_

- [ ] 26. P3 端到端验证
  - File: test/e2e/grafana_authority_test.go
  - webhook firing/resolved、对账 absence → stale 不 resolved、Fault 映射生效
  - Purpose: P3 的验收闸门
  - _Leverage: test/e2e/alertmanager_authority_test.go（复制改造）_
  - _Requirements: R6 全部验收条件_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: QA 工程师 | Task: 以 AM 权威测试为模板改造出 Grafana 版本，重点断言「看不到 ≠ resolved」 | Restrictions: 单轮运行；不得修改 AM 测试的既有断言 | Success: E2E 全绿 | 完成后按工作流标记状态并 log-implementation_

- [ ] 27. 时代一冻结标注
  - File: README.md, web/src/pages/Settings/Routes.tsx, web/src/pages/Settings/Silences.tsx
  - 相关页面与文档标注「仅 generic 来源生效」，说明 AM/Grafana 来源不经过本地 route/group/silence
  - Purpose: 让冻结成为显式契约，防止后续误加能力
  - _Leverage: CLAUDE.md 不变量 24 的表述_
  - _Requirements: R6.8_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: React/TS + 技术写作者 | Task: 增加来源适用范围说明，使用 Tooltip 或页面级说明而非常驻大段文字 | Restrictions: 不得删除任何时代一功能；不得新增一级导航；不得在业务页面常驻产品机制说明（CLAUDE.md 约定 19） | Success: `npm run build` 与 oxlint 通过；说明文案与不变量 24 一致 | 完成后按工作流标记状态并 log-implementation_

## P4 健康证据 + 自伤消歧 + 影响外播

- [ ] 28. 迁移 000028：健康证据与影响标记
  - File: migrations/000028_fault_health_evidence.up.sql, migrations/000028_fault_health_evidence.down.sql
  - 建 fault_health_evidence(fault_id, node_id, observed_at, health, sla, window_seconds, source, baseline) + 索引 (fault_id, observed_at DESC)；fault_impacts 增 flags JSONB 默认 '{}'
  - Purpose: 承载 R7/R8 的证据与标记
  - _Leverage: 现有迁移风格_
  - _Requirements: R7.1, R8.4_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: PostgreSQL DBA | Task: 编写 000028 迁移 | Restrictions: 编号必须为 000028；up/down 可往返；flags 默认 '{}' 非 NULL；不得修改既有迁移 | Success: 迁移往返通过，索引可用于取最新样本 | 完成后按工作流标记状态并 log-implementation_

- [ ] 29. config：健康探测配置与必填校验
  - File: internal/config/config.go, internal/config/config_test.go
  - 增 CCLOUD_HEALTH_PROBE_INTERVAL(1m)、CCLOUD_HEALTH_WINDOW(**必填**)、IMPACT_RULED_OUT_STREAK(3)、CHANGE_SUSPECT_WINDOW(30m)；probe 开启但 WINDOW 未配置时启动期 fail fast
  - Purpose: 避免用猜测的窗口值做出错误的自动确认
  - _Leverage: 任务 4 引入的 config 风格_
  - _Requirements: R7.5, R8.3, R8.4_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 增加探测配置并在窗口缺失时启动失败，错误信息明确指向配置项名 | Restrictions: 不得为 WINDOW 提供默认值；probe=0 时完全短路不校验窗口 | Success: 单测覆盖 probe 开启且窗口缺失→报错、probe 关闭→不校验 | 完成后按工作流标记状态并 log-implementation_

- [ ] 30. 实现 healthprobe 周期 loop
  - File: internal/ccloud/healthprobe.go, internal/app/app.go
  - loop：ListOpenActiveFaults → 半径内 application 节点（有 ccloud.app_id alias）→ 事务外 FetchAppHealth → 短事务写 evidence（首样本按需标 baseline）→ 触发 impact.RebuildFault
  - Purpose: 为影响自动确认提供 direct evidence
  - _Leverage: 任务 9 的 syncer 装配模式、impact.RebuildFault_
  - _Requirements: R7.2, R7.3, R7.6, R7.7_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 实现健康探测 loop，事务外 fetch、短事务写证据后触发影响重算 | Restrictions: 不得使用 scheduler timer；不得生成应用层 Fault；外部调用必须有超时与退避 | Success: 单测/集成测验证：无 ccloud.app_id alias 的节点被跳过、证据字段完整、重算被触发 | 完成后按工作流标记状态并 log-implementation_

- [ ] 31. RebuildFault 健康判定分支
  - File: internal/impact/projector.go, internal/impact/projector_test.go
  - 在 hasDirect 之后插入 health 分支：`observed_at ≥ T+W` 且 `health<sla` 且 `baseline≥sla` 且无 flagged 变更 → confirmed；`T+W` 后连续 K 周期达标 → ruled_out；否则保持 potential；入口先跳过已人工裁决节点
  - Purpose: R8 的核心，自动回答「是不是平台的问题」
  - _Leverage: 现有 potential(0.65)/stale(0.4)/confirmed(0.95) 判定链与 ReplaceAutomatedImpacts_
  - _Requirements: R8.1, R8.2, R8.3, R8.6_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 领域/算法工程师 | Task: 实现 health 判定矩阵，纯函数化以便表驱动测试；人工裁决节点直接跳过 | Restrictions: 不得覆盖人工裁决（不变量 16）；不得修改既有 direct evidence 分支语义；不得引入 likely 等模糊态 | Success: 表驱动测试覆盖窗口未到/基线低于 SLA/有变更标记/连续达标不足 K/连续达标 K 次/人工已裁决 | 完成后按工作流标记状态并 log-implementation_

- [ ] 32. 变更消歧接入
  - File: internal/ingest/changeevent.go, internal/api/alerts.go, internal/impact/projector.go
  - change_events.resource_ref 支持 ccloud.app_id 键；[T−CHANGE_SUSPECT_WINDOW, now] 内有发布 → flags.suspected_self_change=true 且状态留 potential
  - Purpose: 避免把业务自己刚发布造成的故障误判成平台故障
  - _Leverage: 现有 change_events 与 resource_ref 匹配_
  - _Requirements: R8.4, R8.5, R8.7_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 扩展 resource_ref 匹配键并在重算时读取变更窗口 | Restrictions: 不得新建第二套变更匹配引擎；平台发布 webhook 不可用时消歧退化为不生效且必须在页面明示 | Success: 单测覆盖窗口内有变更→flag 置位且留 potential、无变更→正常判定 | 完成后按工作流标记状态并 log-implementation_

- [ ] 33. 影响外播通知
  - File: internal/app/fault_outbox.go, internal/app/fault_outbox_test.go
  - QueueImpactConfirmed（目标=受影响应用 owner team channels，卡片 resourceType="impact" value 含 fault_id+node_id，动作 confirm/rule_out）；Fault resolved → QueueImpactResolved 给所有 confirmed 团队
  - Purpose: R9 的核心，让业务团队在卡片上确认或推翻
  - _Leverage: 任务 16 的 fault_outbox 骨架_
  - _Requirements: R9.1, R9.2, R9.4, R9.5_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 实现影响外播两个入口，门槛与 Fault 通知独立，只 Schedule | Restrictions: 幂等键含 round 且随 payload 持久化；事务内禁止同步 IO；对外响应不得泄露原始通信目标（不变量 13） | Success: 单测覆盖 confirmed 外播一次、resolved 通知所有 confirmed 团队、无 owner 渠道时的回落行为 | 完成后按工作流标记状态并 log-implementation_

- [ ] 34. 飞书卡片影响动作
  - File: internal/api/lark_callback.go
  - 新增 case "impact"：confirm / rule_out 落到既有 PUT /api/v1/faults/:id/impacts/:node_id 的人工裁决语义
  - Purpose: 让业务团队无需登录即可裁决
  - _Leverage: 现有 alert/fault/incident 分支与幂等表_
  - _Requirements: R9.3_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 增加 impact 分支并复用人工裁决存储路径 | Restrictions: 与 REST 端点保持同一语义；不得新建裁决写入路径；不得绕过验签 | Success: 单测覆盖 confirm/rule_out 幂等与非法 value | 完成后按工作流标记状态并 log-implementation_

- [ ] 35. 前端影响轨迹展示
  - File: web/src/pages/Faults/FaultWorkspace.tsx
  - 影响表显示 health 轨迹（value/sla/window/observed_at）与 flags（疑似自身变更等）；人工裁决入口沿用既有 impacts PUT
  - Purpose: 让人能复核自动结论
  - _Leverage: 既有 impacts 表格与 AntD 5 组件_
  - _Requirements: R7.6, R8.4_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: React/TS 前端工程师 | Task: 展示 health 轨迹与 flags，标注疑似自身变更 | Restrictions: 锁 AntD 5；不新增页面级产品机制说明；空数据不得崩页 | Success: npm run build 与 oxlint 通过，轨迹与 flags 正确渲染 | 完成后按工作流标记状态并 log-implementation_

- [ ] 36. P4 验证
  - File: internal/impact/projector_test.go, test/e2e/fault_impact_test.go
  - 判定矩阵单测 + fake ccloud health server E2E：confirmed → 影响卡片 → rule_out 回调后重算不覆盖
  - Purpose: P4 的验收闸门
  - _Leverage: test/e2e 既有 fake 服务模式_
  - _Requirements: R7, R8, R9 全部验收条件_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: QA 工程师 | Task: 补齐判定矩阵单测与 P4 E2E | Restrictions: 不得依赖真实 ccloud；E2E 单轮运行 | Success: 全部测试绿 | 完成后按工作流标记状态并 log-implementation_

## P5 先行指标 + 可靠性

- [ ] 37. 先行指标接口
  - File: internal/ops/indicators.go, internal/api/ops.go, internal/api/router.go
  - GET /api/v1/ops/indicators（admin only）：firing 告警映射率、无 owner 的 active Fault 数、Fault 认领时长中位数、恢复提醒拦截数、未对齐身份数
  - Purpose: 在 MTTR 数据够用之前提供可看的先行指标
  - _Leverage: internal/ops/report.go 的查询风格；任务 7 的 unresolved_identities_
  - _Requirements: R10.1_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 实现五个先行指标查询与端点，聚合在 SQL 层完成 | Restrictions: admin only；空集合返 []；不得为指标新建表或物化视图 | Success: 单测覆盖各指标的 SQL 与空数据情形（返回 0 或 null 语义需明确且不崩页） | 完成后按工作流标记状态并 log-implementation_

- [ ] 38. 日报与运行总览展示指标
  - File: internal/ops/report.go, web/src/pages/Overview/Overview.tsx
  - 日报追加一节；运行总览增加对应卡片
  - Purpose: 让指标进入既有可见路径而非只活在 API 里
  - _Leverage: 现有日报装配与总览卡片_
  - _Requirements: R10.2_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go + React/TS | Task: 日报追加指标小节，总览增加卡片 | Restrictions: 不新建报表体系或页面；不新增一级导航；空数据展示需明确 | Success: 日报渲染测试通过；前端 build 与 oxlint 通过 | 完成后按工作流标记状态并 log-implementation_

- [ ] 39. /readyz 增强
  - File: internal/api/directory.go, internal/api/directory_test.go
  - 增加 scheduler/pipeline 最近一次成功轮询距今天数，超过 3× 轮询间隔返回 503；/healthz 保持恒 200
  - Purpose: 让 scheduler/管道静默停摆可见
  - _Leverage: internal/api/directory.go RegisterHealthRoutes 与注入 ready func 的既有设计_
  - _Requirements: R10.3, R10.5_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 扩展 readyz 判定并在停摆时返回 503 | Restrictions: 不得改动 /healthz 语义；不得让 readyz 依赖外部 IO（ccloud 不可用不应导致 503）；k8s 探针切换属于人工动作，不在本任务内执行 | Success: 单测覆盖正常、超期 503、外部 IO 不可用仍 200 | 完成后按工作流标记状态并 log-implementation_

- [ ] 40. 通知最终失败兜底
  - File: internal/notify/sender.go, internal/scheduler 相关 handler
  - notify_send 最终失败 → 向 fallback target 发送一次元告警（不重试风暴）
  - Purpose: 让发送失败被人感知，而不是静默丢弃
  - _Leverage: notify_send 的 claim/租约/fencing 机制_
  - _Requirements: R10.4_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 在发送最终失败路径追加一次元告警，避免自激循环 | Restrictions: 不得改变 notify_send 的 claimed+租约+fencing 语义；不得把失败改成永久 fired；元告警自身失败不得再递归告警 | Success: 单测覆盖失败一次发元告警、元告警失败不递归 | 完成后按工作流标记状态并 log-implementation_

## 顺延与人工动作

- [ ] 41. CLAUDE.md 核心目录增补 internal/ccloud/
  - File: CLAUDE.md
  - 在 §核心目录结构 增加 `internal/ccloud/` 说明（client/importer/syncer/healthprobe 职责与"外部 IO 只在此包"的约束）
  - Purpose: 让目录说明与结构一致
  - _Leverage: 任务 5–9、30 的实际落地结构_
  - _Requirements: NFR-Architecture_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: 技术写作者 | Task: 按实际落地结构补目录说明 | Restrictions: 等 P1 建包完成后再写，避免文档与结构不符 | Success: 说明与实际目录一致 | 完成后按工作流标记状态并 log-implementation_

- [ ] 42. CHANGELOG 记录 v0.3
  - File: CHANGELOG.md
  - P0–P2 完成后统一记录（含迁移 000026/000027、新包、端点、前端改动）
  - Purpose: 满足 CLAUDE.md「交付时间线与接手须知」要求
  - _Leverage: 既有 CHANGELOG 结构_
  - _Requirements: —_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: 技术写作者 | Task: 随 v0.3 发布统一记录 | Restrictions: 不得在 P0 阶段提前记录（P0 无代码变更） | Success: 条目与实际变更一致 | 完成后按工作流标记状态并 log-implementation_

- [ ] 43. k8s 探针切换（人工动作，需用户确认）
  - File: deploy/k8s/（配置建议）, ccloud 平台（实际执行方）
  - API 的 liveness 与 readiness 均改为 /readyz（当前两者都打恒 200 的 /healthz，未经过任何依赖检查）
  - Purpose: 让探针真正起作用
  - _Leverage: 任务 39 实现的新 readyz_
  - _Requirements: R10.5_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: SRE | Task: 在 ccloud 平台修改探针配置 | Restrictions: 属于平台侧配置变更，必须由用户确认后执行，本 spec 不自动执行；不得在代码仓库内假设配置已生效 | Success: 平台探针指向 /readyz 并经验证 | 完成后按工作流标记状态并 log-implementation_

- [ ] 44. uat 配置 Fault 通知保底目标（人工动作，P2 上线前必须完成）
  - File: ccloud 平台 ConfigMap（实际执行方）, deploy/k8s/（配置建议）
  - 现状：uat 的 `team_ops_settings.channels` 从未配置过；`FAULT_NOTIFY_FALLBACK_TARGET` 与 `FAULT_NOTIFY_COOLDOWN` 是本 spec 新增配置项，uat ConfigMap 中不存在
  - 后果：不配置时 P2 代码全绿、E2E 通过，但部署到 uat 后一条 Fault 通知也发不出去（回落目标为空）
  - **选型（已裁决：两步走第一步）**：先用 `lark_webhook`（群自定义机器人 hook URL），代价是纯文本无认领按钮，认领回 Web 端完成；
    完整卡片能力见任务 45。理由：不被飞书后台配置阻塞，P2 的核心价值（Fault 出现有人被通知到）立即可见
  - 动作：为 owner team 配置 `lark_webhook` 渠道与 hook URL，或至少配置全局 fallback target；确认冷却窗口取值
  - Purpose: 让 P2 的价值在 uat 真实可见，而不是只在测试里成立
  - _Leverage: 任务 16 的回落逻辑、现有 team_ops_settings 配置页_
  - _Requirements: R5.2_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: SRE | Task: 在 uat 配置 lark_webhook 保底通知目标与冷却窗口 | Restrictions: 属于平台侧配置变更，必须由用户确认后执行；飞书 hook URL 属凭据，不得写入代码仓库、日志或对外响应（不变量 13、安全约束） | Success: uat 上人为制造一次 active Fault 能在群里收到文本通知 | 完成后按工作流标记状态并 log-implementation_

- [ ] 45. 飞书自建应用与卡片回调打通（人工动作，两步走第二步）
  - File: 飞书开放平台后台（实际执行方）, ccloud 平台 ConfigMap, 本仓库仅配置说明
  - 现状：uat ConfigMap 无任何 `LARK_*`；启动日志 WARN 确认 verification_token 未配置；`lark` 渠道虽已注册但凭据为空，发送必然失败
  - 四件事：① 建自建应用取 `LARK_APP_ID`/`LARK_APP_SECRET`；② 后台配置卡片回调地址指向 `POST /api/v1/callbacks/lark/card`；
    ③ 配置 `LARK_VERIFICATION_TOKEN`（安全约束要求非空验签）与 `LARK_ENCRYPT_KEY`；④ 给值班人绑定 `users.lark_open_id`（未绑定者点按钮会被拒绝）
  - **已知阻塞点**：回调地址要求飞书侧能访问到 uat 服务；uat 目前为内网部署，需先确认对外入口是否存在
  - **未决**：值班人 open_id 目前只能经 `PUT /api/v1/admin/users/:id` 逐个填写；人数多时是否需要批量导入或自助绑定入口，本 spec 未覆盖
  - 完成后：把 owner team 渠道由 `lark_webhook` 切到 `lark`（纯配置变更），并**解冻任务 19**（卡片故障动作的实现与验证与本任务同期进行）
  - Purpose: 让 R5.7 的卡片认领在真实环境成立，把认领从 Web 前移到卡片
  - _Leverage: internal/notify/channel_lark.go 已实现的按钮卡片；internal/api/lark_callback.go 已实现的验签与幂等_
  - _Requirements: R5.7, R4.3, R4.6_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: SRE | Task: 完成飞书自建应用配置、卡片回调地址、验签凭据与 open_id 绑定，并把团队渠道切到 lark | Restrictions: 平台侧配置变更，必须由用户确认后执行；app_secret / verification_token / encrypt_key 属凭据，不得入仓库或日志；不得为绕过网络限制而关闭验签 | Success: 值班人在卡片上点「认领」后 Fault 状态变为已认领，且未绑定 open_id 的点击被明确拒绝 | 完成后按工作流标记状态并 log-implementation_

- [ ] 19. 飞书卡片故障动作（**延后**：阻塞于公网回调入口，与任务 45 同期进行）
  - File: internal/api/lark_callback.go, internal/api/lark_callback_test.go
  - handleFaultAction 新增 action="ack"（登录即可，复用 domain.AckFault）；现有「推进状态」动作改名 "advance" 且限 admin；补齐 actor 校验
  - Purpose: 让值班在卡片上直接认领
  - **延后理由（用户裁决 2026-09-18）**：uat 目前没有飞书可访问的公网入口，卡片回调无法验证；此时实现只会产出无法端到端验证的代码
  - **解冻条件**：任务 45 的公网回调入口就绪后，与任务 45 同期实现与验证
  - P2 期间替代路径：REST 端点（任务 18）+ Web 认领（任务 20）
  - _Leverage: internal/api/lark_callback.go 现有 alert 分支的验签与幂等表_
  - _Requirements: R4.3, R4.6, R5.9_
  - _Prompt: Implement the task for spec era3-fault-workunit, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端工程师 | Task: 扩展 fault 分支动作，ack 与 REST 语义一致，advance 加 admin 限制与 actor 校验 | Restrictions: 必须保持与 REST 端点同一语义；不得绕过 verification_token 校验；不得删除既有动作 | Success: 单测覆盖 ack 幂等、advance 非 admin 拒绝、actor 缺失拒绝并可审计 | 完成后按工作流标记状态并 log-implementation_
