# Tasks Document — alert-system-mvp

> 依赖顺序：A 基础设施 → B 核心域 → C 存储/调度 → D 管道 → E 接入 → F 通知/值班 → G API → H 扩展点 → I 前端 → J 端到端。
> 同组内任务可并行，跨组按序。所有代码遵循 design.md 的目录结构与依赖方向（一切依赖指向 domain，domain 零依赖）。

## A. 基础设施

- [x] 1. 初始化 Go 项目骨架与配置加载
  - File: go.mod, cmd/server/main.go, internal/config/config.go, Makefile
  - 初始化模块 `alert-system`（Go 1.24+），main.go 只做依赖组装与启动占位；config 支持环境变量+YAML（DB DSN、HTTP 端口、scheduler 轮询周期默认 5s）
  - Purpose: 建立可编译、可配置的项目底座
  - _Leverage: design.md 目录结构_
  - _Requirements: NFR-架构_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 工程脚手架专家 | Task: 初始化 go.mod 与 cmd/internal 目录骨架、config 加载（env>yaml 优先级），main.go 可编译运行输出版本号 | Restrictions: 不引入 Gin 之外的重框架；目录严格按 design.md；不写业务逻辑 | Success: `go build ./...` 通过，`make run` 启动并读取配置 | 完成后按工作流在 tasks.md 标记状态并用 log-implementation 记录_

- [x] 2. 数据库迁移：核心告警表
  - File: migrations/000001_core.up.sql, migrations/000001_core.down.sql
  - 建 alerts（含 fingerprint partial unique index WHERE status != 'resolved'、labels/resource_ref/signal_context JSONB + GIN 索引、severity_fallback、resolve_policy/ttl_seconds、dedup_count、group_key）、alert_events（status: pending/processed/failed）、timeline_entries、integrations 四表
  - Purpose: 落地 design.md Data Models 核心部分
  - _Leverage: design.md Data Models_
  - _Requirements: R1.6, R2, R3, R4.6_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: PostgreSQL DBA 兼 Go 后端 | Task: 按 design.md 编写 golang-migrate 迁移，含 partial unique index、GIN 索引、枚举约束 | Restrictions: 枚举用 CHECK 约束而非 PG ENUM 类型（便于演进）；所有表带 created_at/updated_at | Success: up/down 迁移可往返执行，索引经 EXPLAIN 验证可用于 fingerprint 冲突检测 | 完成后按工作流标记状态并 log-implementation_

- [x] 3. 数据库迁移：组织/路由/策略表
  - File: migrations/000002_org_routing.up.sql, migrations/000002_org_routing.down.sql
  - 建 teams、users、memberships（role: admin/member）、routes（priority+matchers JSONB）、schedules、shifts、escalation_policies（levels JSONB）、notification_policies（per-severity steps JSONB + binding matchers）
  - Purpose: 支撑路由、值班、双维度升级
  - _Leverage: design.md Data Models_
  - _Requirements: R5, R8, R13_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: PostgreSQL DBA | Task: 编写组织/路由/策略迁移，notification_policies.steps 结构须支持 `{wait, channels[], mode: replace|spread}` | Restrictions: 外键齐全；routes.priority 唯一约束 | Success: 迁移往返通过，样例策略 JSON 可插入 | 完成后按工作流标记状态并 log-implementation_

- [x] 4. 数据库迁移：静默/变更/通知日志/定时器/预留表
  - File: migrations/000003_aux.up.sql, migrations/000003_aux.down.sql
  - 建 silences、change_events（resource_ref JSONB + occurred_at 索引）、notification_log、timers（kind+fire_at 索引，status）、annotations（source='ai' 标注）、action_rules、action_runs
  - Purpose: 降噪、上下文、可靠通知与三个预留模块的数据基础
  - _Leverage: design.md Data Models_
  - _Requirements: R6.3, R7.4, R10, R11.5, R12.2_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: PostgreSQL DBA | Task: 编写辅助表迁移；timers 表须支持 `WHERE fire_at <= now() AND status='pending'` 高效轮询 | Restrictions: annotations/action_* 表本期仅建表不接逻辑 | Success: 迁移往返通过 | 完成后按工作流标记状态并 log-implementation_

## B. 核心域（零依赖）

- [x] 5. 信封模型与 fingerprint 计算
  - File: internal/domain/alert.go, internal/domain/fingerprint.go, internal/domain/fingerprint_test.go
  - AlertEvent/Alert 结构体（信封字段全集）；Fingerprint 按 D5 双策略：metric=规则ID+全标签集哈希，log/trace=规则ID+资源标识哈希，内容不参与
  - Purpose: 统一事件模型是 adapter 与管道的唯一契约
  - _Leverage: Alertmanager labels 哈希算法思路（不 import）_
  - _Requirements: R2, R3.1, R3.2_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 领域建模专家 | Task: 实现信封模型与双策略 fingerprint（FNV-1a 或 xxhash over sorted label pairs），表驱动测试覆盖 D5 两条路径与标签顺序无关性 | Restrictions: 包内仅标准库+哈希库；signal_context 定义为 json.RawMessage 对管道不透明 | Success: 测试覆盖同标签不同顺序同指纹、log 内容变化不改指纹 | 完成后按工作流标记状态并 log-implementation_

- [x] 6. 告警状态机与时间线条目
  - File: internal/domain/state.go, internal/domain/state_test.go
  - 状态机 firing→acknowledged→resolved（firing 可直达 resolved）；Transition(action, actor) 返回时间线条目；非法转移返回错误
  - Purpose: R4 生命周期的唯一权威实现
  - _Leverage: internal/domain/alert.go_
  - _Requirements: R4.1, R4.2, R4.5, R4.6_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 状态机实现专家 | Task: 实现全转移矩阵与 Ack 响应耗时计算、Handoff 记录，测试覆盖全部合法/非法转移 | Restrictions: 纯函数式，不触碰存储 | Success: 转移矩阵测试全绿，非法转移（如 resolved→acknowledged）被拒绝 | 完成后按工作流标记状态并 log-implementation_

- [x] 7. Label Matcher 求值器
  - File: internal/domain/matcher.go, internal/domain/matcher_test.go
  - matcher 类型：=、!=、=~、!~；MatcherSet.Matches(labels)；用于静默（R10）、路由（R5.1）、策略绑定（R13.5）三处
  - Purpose: 一份 matcher 实现三处复用
  - _Leverage: Alertmanager silence matcher 语义_
  - _Requirements: R5.1, R10.1, R13.5_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 开发者 | Task: 实现四种 matcher 与集合求值（AND 语义），正则预编译缓存 | Restrictions: 正则用 RE2（标准库）；空 matcher 集合返回 false 防止误匹配全量 | Success: 表驱动测试覆盖四种操作符与边界（空值标签、缺失标签） | 完成后按工作流标记状态并 log-implementation_

## C. 存储与调度

- [x] 8. store 基础设施与事件队列仓储
  - File: internal/store/store.go, internal/store/events.go, internal/store/events_test.go
  - pgx 连接池、事务助手；EventQueue：Enqueue（落库）、Dequeue（`FOR UPDATE SKIP LOCKED` 批量取 pending）、MarkProcessed/MarkFailed
  - Purpose: at-least-once 队列（D 组管道的输入端）
  - _Leverage: migrations 000001_
  - _Requirements: R1.6, NFR-可靠性_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 数据层专家（pgx） | Task: 实现连接池与 SKIP LOCKED 队列，集成测试用 testcontainers 验证并发消费不重不漏、崩溃后 pending 重投 | Restrictions: 队列操作必须在事务内；不引入 ORM | Success: 两 worker 并发消费 1000 事件无重复无丢失 | 完成后按工作流标记状态并 log-implementation_

- [x] 9. 告警仓储：去重写入与时间线
  - File: internal/store/alerts.go, internal/store/timeline.go, internal/store/alerts_test.go
  - UpsertByFingerprint：`ON CONFLICT (fingerprint) WHERE status != 'resolved'` 实现 R3.3 合并计数 / R3.4 新开一轮；列表过滤（severity/status/team/label）；时间线 append-only
  - Purpose: 去重的存储层原子实现
  - _Leverage: internal/store/store.go, internal/domain_
  - _Requirements: R3.3, R3.4, R4.6, NFR-可用性_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 数据层专家 | Task: 实现 partial-index upsert 去重、JSONB label 过滤查询、append-only 时间线 | Restrictions: 时间线表无 UPDATE/DELETE 语句；upsert 必须单条 SQL 保证原子 | Success: 集成测试覆盖同 fingerprint 并发写入计数正确、resolved 后新事件建新行 | 完成后按工作流标记状态并 log-implementation_

- [x] 10. 配置类仓储（组织/路由/策略/静默/变更/通知日志）
  - File: internal/store/config_repos.go, internal/store/silences.go, internal/store/config_repos_test.go
  - teams/users/routes/schedules/escalation_policies/notification_policies/silences/change_events/notification_log 的 CRUD 与查询（routes 按 priority 有序、silences 按时间窗生效查询、change_events 按 resource_ref 近期查询）
  - Purpose: G 组 API 与 D/F 组运行时的读写支撑
  - _Leverage: internal/store/store.go, internal/domain/matcher.go_
  - _Requirements: R5, R7.4, R10, R13.5_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 数据层专家 | Task: 实现配置类仓储，重点：生效静默查询（now ∈ [starts_at, ends_at]）、资源近期变更查询（resource_ref 匹配 + occurred_at 倒序） | Restrictions: 单文件超 400 行则拆分；查询带 LIMIT | Success: 集成测试覆盖时间窗边界与 JSONB 匹配 | 完成后按工作流标记状态并 log-implementation_

- [x] 11. 统一持久化定时器 scheduler ⭐
  - File: internal/scheduler/scheduler.go, internal/scheduler/scheduler_test.go
  - Schedule(kind, refID, fireAt, payload)/Cancel(kind, refID)；轮询 worker（周期可配，SKIP LOCKED 取到期）；handler 注册表按 kind 分发；at-least-once + handler 幂等约定
  - Purpose: R4-TTL/R5-升级/R9-分组/R13-渠道升级四处共用的关键组件
  - _Leverage: internal/store（timers 表）_
  - _Requirements: R4.4, R5.4, R9.2, R13.2_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 并发与可靠性专家 | Task: 实现持久化定时器：注册/取消/轮询触发/handler 分发，集成测试验证重启后到期定时器补触发、Cancel 后不触发、同 (kind,refID) 重复 Schedule 为替换语义 | Restrictions: 触发回调 panic 必须 recover 并标记 failed；不用 cron 库，轮询即可 | Success: fake clock 单测 + testcontainers 集成测试全绿 | 完成后按工作流标记状态并 log-implementation_

## D. 管道

- [x] 12. 管道编排器与 hook 扩展点注册表
  - File: internal/pipeline/pipeline.go, internal/hook/registry.go, internal/pipeline/pipeline_test.go
  - Stage 接口 + 顺序编排（去重→分组→静默→抑制→路由）；PipelineContext 携带 Alert+路由结果；hook.Registry 在每个 stage 前后调用注册函数（超时 2s、panic 隔离、失败仅 WARN）
  - Purpose: D6 可插拔管道 + R11.1 扩展点
  - _Leverage: internal/domain, internal/store/events.go_
  - _Requirements: R2.3, R11.1, R11.6, D6_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 架构师 | Task: 实现 Stage 接口、编排器（从 EventQueue 消费驱动）与 hook 注册表（前后置、超时+recover 旁路隔离） | Restrictions: 编排器不 import 任何具体 stage 包（由 main 组装）；hook 失败不得影响 stage 结果 | Success: 单测验证 stage 顺序、hook panic 不中断管道 | 完成后按工作流标记状态并 log-implementation_

- [x] 13. 去重 stage 与 severity 兜底
  - File: internal/pipeline/stage_dedup.go, internal/pipeline/stage_dedup_test.go
  - 调用 UpsertByFingerprint；新告警时按 resolve_policy=ttl 注册 TTL 定时器（R4.4）；severity 缺失按 integration 默认值兜底并标记（R2.5）
  - Purpose: R3 落地
  - _Leverage: internal/store/alerts.go, internal/scheduler_
  - _Requirements: R2.5, R3, R4.4_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发 | Task: 实现去重 stage：upsert、合并时续期 TTL 定时器、severity 兜底标记 | Restrictions: 合并路径（dedup_count+1）不得触发新通知标志 | Success: 单测覆盖新建/合并/resolved 后新开三条路径 | 完成后按工作流标记状态并 log-implementation_

- [x] 14. 分组 stage
  - File: internal/pipeline/stage_group.go, internal/pipeline/stage_group_test.go
  - group_key = route_id + hash(group_by 标签值)；组首告警注册 group_wait 定时器，后续加入者只更新组；到期 handler 交给 notify 聚合发送；组更新受 group_interval 节流；未配置 group_by 直发
  - Purpose: R9 落地；升级序列以组首时间为基准（design 残留风险 ②）
  - _Leverage: internal/scheduler, internal/store_
  - _Requirements: R9_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（参考 Alertmanager dispatch/aggrGroup 模型） | Task: 实现分组 stage 与组状态维护，fake clock 测试 group_wait/group_interval 时序、组内新告警不重置升级基准 | Restrictions: 组状态持久化在 alerts.group_key + timers，不引入内存态唯一真相 | Success: 时序测试覆盖首发等待、节流更新、无 group_by 直发 | 完成后按工作流标记状态并 log-implementation_

- [x] 15. 静默 stage
  - File: internal/pipeline/stage_silence.go, internal/pipeline/stage_silence_test.go
  - 用 MatcherSet 求值生效静默；命中则标记 silenced_by（告警照常落库与展示，仅抑制通知）；静默过期/删除后恢复通知由 notify 侧检查
  - Purpose: R10 落地
  - _Leverage: internal/domain/matcher.go, internal/store/silences.go_
  - _Requirements: R10_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发 | Task: 实现静默 stage：生效窗口内 matcher 命中即标记，记录命中的静默 ID 列表 | Restrictions: 不删除不丢弃告警；多条静默同时命中须全记录 | Success: 单测覆盖时间窗边界、多静默叠加、过期自动失效 | 完成后按工作流标记状态并 log-implementation_

- [x] 16. 路由 stage 与抑制 stub
  - File: internal/pipeline/stage_route.go, internal/pipeline/stage_inhibit.go, internal/pipeline/stage_route_test.go
  - 路由：按 priority 有序匹配 routes.matchers，命中即定 team_id，无命中走默认团队（R5.2）；抑制 stage 本期恒通过（接口占位，二期替换）
  - Purpose: R5.1-5.3 落地 + D6 管道完整性
  - _Leverage: internal/domain/matcher.go, internal/store/config_repos.go_
  - _Requirements: R5.1, R5.2, R5.3_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发 | Task: 实现有序路由匹配与默认团队兜底、抑制空实现 | Restrictions: 路由规则缓存需带失效（配置变更后 ≤10s 生效）| Success: 单测覆盖优先级顺序、无命中兜底、缓存失效 | 完成后按工作流标记状态并 log-implementation_

## E. 接入

- [x] 17. ingest 端点、adapter 注册表与通用 Webhook adapter
  - File: internal/ingest/server.go, internal/ingest/adapter.go, internal/ingest/generic.go, internal/ingest/generic_test.go
  - `POST /ingest/{integration_id}`：token 鉴权（constant-time 比较）→ adapter.Parse → 信封校验 → Enqueue → 202；非法 payload 4xx + 结构化拒绝日志（R1.4）
  - Purpose: R1 接入骨架与第一个 adapter
  - _Leverage: internal/domain, internal/store/events.go_
  - _Requirements: R1, R2.1_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go API 开发 | Task: 实现 ingest HTTP 层、InboundAdapter 接口注册表、通用 webhook adapter（平台自定义 JSON schema） | Restrictions: 落库成功才返回 202；token 哈希存储比对 | Success: httptest 覆盖鉴权失败/坏 payload/成功入队三路径 | 完成后按工作流标记状态并 log-implementation_

- [x] 18. Alertmanager 与 Grafana adapters
  - File: internal/ingest/alertmanager.go, internal/ingest/grafana.go, internal/ingest/testdata/*.json, internal/ingest/vendors_test.go
  - 两家真实 webhook payload → 信封翻译（含 resolved 状态透传、OTel 语义键规范化、resolve_policy=source）；golden file 测试
  - Purpose: R1.1/R1.2 两大主力上游
  - _Leverage: internal/ingest/adapter.go, Alertmanager webhook payload 结构_
  - _Requirements: R1.1, R1.2, R2.4_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 集成开发（熟悉 Alertmanager/Grafana webhook 格式） | Task: 实现两个 vendor adapter 与 golden file 测试（真实样本 payload 入 testdata） | Restrictions: 不猜字段——以两家当前文档格式为准；未知字段进 signal_context | Success: golden 测试覆盖 firing/resolved/批量告警样本 | 完成后按工作流标记状态并 log-implementation_

- [x] 19. 变更事件 adapter
  - File: internal/ingest/changeevent.go, internal/ingest/changeevent_test.go
  - 通用变更事件 webhook（kind/resource_ref/payload/occurred_at）→ change_events 表；与告警管道隔离（不进队列）
  - Purpose: R7.4 变更上下文数据源
  - _Leverage: internal/ingest/adapter.go, internal/store/config_repos.go_
  - _Requirements: R7.4_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 集成开发 | Task: 实现变更事件接入与存储，resource_ref 键名按 OTel 语义规范化 | Restrictions: 与告警事件队列完全隔离 | Success: httptest 验证入库与规范化 | 完成后按工作流标记状态并 log-implementation_

## F. 通知与值班

- [x] 20. Channel 接口、Webhook 渠道与可靠发送
  - File: internal/notify/channel.go, internal/notify/channel_webhook.go, internal/notify/sender.go, internal/notify/sender_test.go
  - Channel 接口 + 注册表（电话渠道留插件位，未启用渠道配置校验 R13.6）；sender：指数退避重试、notification_log 落账、发送前幂等键查重（崩溃恢复不重发）
  - Purpose: R6.1/R6.3 可靠发送底座
  - _Leverage: internal/store（notification_log）_
  - _Requirements: R6.1, R6.3, R13.6_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 可靠性工程 | Task: 实现渠道抽象、webhook 渠道、带退避重试与幂等去重的 sender | Restrictions: 重试上限可配；最终失败写时间线；幂等键 = alert/group + step + channel + target | Success: 单测覆盖重试耗尽、幂等去重、未启用渠道校验报错 | 完成后按工作流标记状态并 log-implementation_

- [x] 21. 飞书渠道与交互卡片
  - File: internal/notify/channel_lark.go, internal/api/lark_callback.go, internal/notify/channel_lark_test.go
  - oapi-sdk-go 发送告警卡片（等级配色、聚合组摘要 R9.5）；卡片按钮 Ack/Resolve 回调端点（验签）→ 调状态机；无权限时降级为跳转链接
  - Purpose: R6.2/R6.4 主力渠道
  - _Leverage: larksuite/oapi-sdk-go, internal/domain/state.go_
  - _Requirements: R6.2, R6.4, R9.5_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 开发（熟悉飞书开放平台卡片与回调验签） | Task: 实现飞书渠道与回调处理，mock SDK 测试卡片结构与回调动作 | Restrictions: 凭据走 config 不落日志；回调必须验签 | Success: mock 测试覆盖发送/Ack 回调/降级链接三路径 | 完成后按工作流标记状态并 log-implementation_

- [x] 22. 通知策略引擎（渠道升级/扩散）
  - File: internal/notify/policy.go, internal/notify/policy_test.go
  - 策略求值链（binding matchers → team 默认 → global 默认）；按 severity 取升级序列；每步经 scheduler 注册下一步定时器；Ack/Resolve/静默命中即 Cancel；replace/spread 两模式；渠道升级不改 severity/status 并写时间线（R13.3）
  - Purpose: R13 核心
  - _Leverage: internal/scheduler, internal/notify/sender.go, internal/domain/matcher.go_
  - _Requirements: R13, R10.3_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发 | Task: 实现策略引擎：求值链、步进状态机（当前步持久化在 timer payload）、fake clock 测试 warning:email→5min→IM 与 critical:IM→3min→电话 两个需求样例 | Restrictions: 步进 handler 幂等（检查告警仍 firing 未 Ack 未静默）；组维度驱动时以组首时间为基准 | Success: 需求样例场景测试全绿，Ack 后定时器取消 | 完成后按工作流标记状态并 log-implementation_

- [x] 23. 值班求值与人员升级链
  - File: internal/oncall/resolver.go, internal/oncall/escalation.go, internal/oncall/resolver_test.go
  - OnCallAt(teamID, t)：按 schedules/shifts 求当班人（R5.5）；人员升级链：未 Ack 超时经 scheduler 逐级推进（R5.4），升级停止条件=Ack；与渠道升级正交并行
  - Purpose: R5.4/R5.5 落地
  - _Leverage: internal/scheduler, internal/store/config_repos.go_
  - _Requirements: R5.4, R5.5_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（参考 GoAlert/Grafana OnCall 排班模型） | Task: 实现轮换排班求值与升级链推进，fake clock 测试跨时区/跨天班次边界与逐级升级 | Restrictions: 升级 handler 幂等；无人可通知时告警时间线记录警告而非静默失败 | Success: 「某时刻谁在值班」查询与三级升级链时序测试全绿 | 完成后按工作流标记状态并 log-implementation_

- [x] 24. 生命周期定时 handlers：TTL 恢复与 repeat_interval
  - File: internal/notify/handlers.go, internal/notify/handlers_test.go
  - TTL 到期→自动 resolved+时间线标记「TTL 自动恢复」（R4.4）；repeat_interval 到期→未恢复未静默则重发提醒并续约（R6.5）；resolved 事件（source-driven）触发升级/重复定时器清理
  - Purpose: 生命周期与通知的闭环粘合
  - _Leverage: internal/scheduler, internal/domain/state.go, internal/notify/sender.go_
  - _Requirements: R4.3, R4.4, R6.5_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发 | Task: 实现 TTL/repeat_interval/source-resolved 三类 handler 并注册到 scheduler | Restrictions: 全部幂等；repeat 提醒遵守分组与静默（R6.5） | Success: fake clock 场景测试：log 告警超时自愈、静默期间 repeat 不发、解静默后恢复 | 完成后按工作流标记状态并 log-implementation_

## G. API 与鉴权

- [x] 25. HTTP 服务、session 认证与 RBAC
  - File: internal/api/server.go, internal/api/auth.go, internal/api/middleware.go, internal/api/auth_test.go
  - Gin 路由骨架 `/api/v1`；bcrypt 登录 + HttpOnly session cookie；authn 接口预留 OIDC 实现位（R8.1）；RBAC 中间件（admin/member）；ingest 端点豁免 session（R8.3）
  - Purpose: R8 落地与 API 底座
  - _Leverage: internal/store/config_repos.go_
  - _Requirements: R8_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go API 安全开发 | Task: 实现登录/登出/session 中间件/角色鉴权，Authenticator 定义为接口（本期 password 实现，预留 oidc） | Restrictions: session 固定过期+滑动续期；密码错误与用户不存在返回同一错误（防枚举） | Success: httptest 覆盖 401/403/member 越权矩阵 | 完成后按工作流标记状态并 log-implementation_

- [x] 26. 告警 API：列表/详情/处置动作
  - File: internal/api/alerts.go, internal/api/alerts_test.go
  - 列表（severity/status/team/label 过滤 + 分页）、详情（含时间线、signal_context、命中静默、最近变更 R7.5）、POST ack/resolve/handoff（调状态机 + 取消升级定时器）
  - Purpose: R4 处置入口 + NFR-可用性
  - _Leverage: internal/domain/state.go, internal/store/alerts.go, internal/scheduler_
  - _Requirements: R4, R7.5, NFR-可用性_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go API 开发 | Task: 实现告警查询与处置端点，处置动作事务内完成状态转移+时间线+定时器取消 | Restrictions: 动作幂等（重复 Ack 返回 409 或幂等成功，选一并文档化）| Success: httptest 覆盖过滤组合、处置动作、越权 | 完成后按工作流标记状态并 log-implementation_

- [x] 27. 配置管理 API：集成/团队/路由/值班/策略/静默
  - File: internal/api/config.go, internal/api/silences.go, internal/api/config_test.go
  - integrations（生成 token 只展示一次）、teams/users、routes、schedules、escalation/notification policies（含未启用渠道校验 R13.6）、silences CRUD（提前删除立即恢复通知 R10.5）
  - Purpose: 全部配置面的管理入口（admin only）
  - _Leverage: internal/api/middleware.go, internal/store/config_repos.go_
  - _Requirements: R1.3, R5, R10, R13.5, R13.6_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go API 开发 | Task: 实现配置 CRUD，重点校验：策略引用渠道存在性、routes priority 冲突、静默时间窗合法性 | Restrictions: 全部 admin 权限；token 明文仅创建响应返回一次 | Success: httptest 覆盖校验失败与成功路径 | 完成后按工作流标记状态并 log-implementation_

## H. 预留扩展点

- [x] 28. action 触发器与 enrich 客户端（接口 + 空实现）
  - File: internal/action/trigger.go, internal/enrich/cmdb.go, internal/action/trigger_test.go
  - action：Trigger(rule, alert) 接口 + 冷却窗口检查（同 fingerprint 窗口内拒绝并升级人工 R12.4）+ action_runs 落账，执行器本期为 no-op webhook 占位；enrich：Lookup(resourceRef) 接口 + 空实现（CMDB 客户端二期），超时 2s 旁路
  - Purpose: R11/R12 承诺的「扩展点是实物」
  - _Leverage: internal/hook/registry.go, internal/store_
  - _Requirements: R7.2, R7.3, R12.1, R12.2, R12.4, R12.6_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 架构师 | Task: 实现两个扩展点接口、冷却窗口检查与落账，注册到 hook 注册表验证旁路隔离 | Restrictions: 空实现必须真实走通「注册→调用→落账」链路，不是死代码 | Success: 单测验证冷却拒绝、超时旁路不阻塞管道 | 完成后按工作流标记状态并 log-implementation_

## I. 前端

- [x] 29. Web SPA 脚手架、登录与告警列表
  - File: web/（Vite + React + TS + AntD 脚手架）, web/src/pages/Login.tsx, web/src/pages/AlertList.tsx, web/src/api/client.ts
  - 登录页、告警列表（severity/status/team/label 过滤、分页、等级配色徽标、silenced 标识）
  - Purpose: NFR-可用性列表侧
  - _Leverage: /api/v1 契约（任务 25/26）_
  - _Requirements: R8, NFR-可用性_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: React+TS 前端开发（AntD） | Task: 搭建 SPA、登录会话管理、告警列表页（过滤器组合、轮询刷新） | Restrictions: API 类型定义与后端 JSON 严格对齐；不引入重状态库（用 React Query） | Success: 本地联调可登录、过滤、翻页 | 完成后按工作流标记状态并 log-implementation_

- [x] 30. 告警详情页与配置管理页
  - File: web/src/pages/AlertDetail.tsx, web/src/pages/Silences.tsx, web/src/pages/Settings/*.tsx
  - 详情页一屏：状态/等级/资源/去重计数/时间线/最近变更/signal_context 跳转/Ack-Resolve-Handoff 按钮（对齐 PagerTree 信息密度）；静默管理与团队/路由/值班/策略/集成设置页（admin）
  - Purpose: NFR-可用性详情侧 + 配置面 UI
  - _Leverage: web/src/api/client.ts_
  - _Requirements: R4, R7.5, R10, NFR-可用性_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: React 前端开发 | Task: 实现详情页（时间线组件、处置动作、变更上下文卡片）与配置管理页 | Restrictions: member 角色隐藏 admin 页；处置按钮按告警状态可用性联动 | Success: 详情页一屏呈现全部 NFR 要求信息，处置动作联调通过 | 完成后按工作流标记状态并 log-implementation_

## J. 端到端验证

- [-] 31. E2E 场景测试与主流程验收
  - File: test/e2e/e2e_test.go, test/e2e/docker-compose.yml
  - design.md 三场景：① Alertmanager webhook→去重→路由→mock 飞书收到→Ack→升级取消；② 静默生效零通知→过期恢复 repeat；③ ttl 告警超时自动 resolved；外加 R13 样例（warning email→IM 升级）
  - Purpose: 全链路需求验收
  - _Leverage: 全部组件, testcontainers/docker-compose_
  - _Requirements: All_
  - _Prompt: Implement the task for spec alert-system-mvp, first run spec-workflow-guide to get the workflow guide then implement the task: Role: QA 自动化工程师（Go） | Task: 编写四个 E2E 场景（真 PG + mock 渠道 + 压缩时钟配置），作为 MVP 验收基线 | Restrictions: 不 mock 存储与 scheduler；用短时长配置（秒级）替代真实分钟等待 | Success: 四场景在 CI 可重复通过 | 完成后按工作流标记状态并 log-implementation_
