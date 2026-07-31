# Tasks Document

任务编号与依赖：1 → 2/5 → 3/4/6 → 7 → 8 → 9 → 10 → 11/12/13 → 14。共同约束（每个任务的 Restrictions 均隐含）：禁改 `.spec-workflow/`、禁 git 操作、禁改 go.mod；遵守项目 CLAUDE.md 核心不变量（scheduler handler 不自开事务、持锁事务内只 Schedule notify_send、中文注释解释「为什么」）；实现前先 grep 本 spec 的 Implementation Logs 避免重复造轮子。

- [x] 1. 迁移 000006：severity_config 列与 team_ops_settings 表
  - File: migrations/000006_ops.up.sql, migrations/000006_ops.down.sql
  - escalation_policies 加 `severity_config JSONB NOT NULL DEFAULT '{}'`
  - 建 team_ops_settings 表（结构见 design.md Data Models，含 CHECK、UNIQUE(team_id)、set_updated_at 触发器）
  - down 可逆：DROP TABLE、DROP COLUMN
  - Purpose: 为 R1/R2 提供存储结构，存量数据零迁移
  - _Leverage: migrations/000002_org_routing.up.sql（触发器与注释风格）, migrations/000003_aux.up.sql_
  - _Requirements: 1.1, 1.4, 2.1_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: PostgreSQL 迁移工程师 | Task: 按 design.md「Data Models」一节编写 000006_ops 迁移（up/down），严格对齐既有迁移的注释风格（每表头部中文注释说明用途与需求编号） | Restrictions: 不改既有迁移文件；DEFAULT '{}' 必须保证存量升级策略行为不变；down 迁移必须完整可逆 | Success: docker 环境跑 migrate up/down/up 三步无错，`go test ./internal/store/... -race` 既有用例全绿 | 完成后：用 log-implementation 落账（artifacts 写表结构与列名），tasks.md 标 [x]_

- [x] 2. store：EscalationPolicy 增加 SeverityConfig 序列化
  - File: internal/store/config_repos_notify.go, internal/store/config_repos_notify_test.go（如有既有测试文件则扩展）
  - 定义 `SeverityConfig{Overrides map[string][]int; Skip []string}`，EscalationPolicy 加字段
  - scan/Create/Update 补 severity_config 列读写（JSON 往返）
  - Purpose: R1 配置的持久化通道
  - _Leverage: internal/store/config_repos_notify.go 的 scanEscalationPolicy/levels JSON 处理模式_
  - _Requirements: 1.1, 1.4_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（pgx 仓储层） | Task: 按 design.md「R1」接口定义扩展 EscalationPolicy 的 SeverityConfig 字段与列读写，测试覆盖空 config（'{}'）反序列化为零值 | Restrictions: 保持 store 方法 `tx pgx.Tx` 入参约定；不改 Levels 既有形状与语义 | Success: go build/vet 零错，store 集成测试（testcontainers）验证 SeverityConfig 序列化往返与空值兼容 | 完成后：log-implementation 落账（artifacts 写 SeverityConfig 结构定义），tasks.md 标 [x]_

- [x] 3. oncall：effectiveWaitSeconds 纯函数与 Chain 接入
  - File: internal/oncall/escalation.go, internal/oncall/escalation_test.go
  - 新增 `effectiveWaitSeconds(p store.EscalationPolicy, sev domain.Severity, level int) (int, bool)` 纯函数
  - Chain.Start 与 Chain.advance 取等待时长的两处改为经该函数；skip 时 Start 不建定时器
  - 表驱动单测：无覆写回退/部分覆写/skip/overrides 短于 levels/未知 severity 回退
  - Purpose: R1 核心行为
  - _Leverage: internal/oncall/escalation.go 既有 Start/advance；escalation_test.go 的 fake 模式_
  - _Requirements: 1.2, 1.3, 1.4, 1.5_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（领域状态机/定时编排） | Task: 实现 effectiveWaitSeconds 并接入 Chain.Start/advance，注释说明「advance 每级重读告警故 severity 中途变化自然采用当前值」的设计意图 | Restrictions: 不改 Handle/advance 的幂等判据与事务约定；Stop/取消语义不动（AC 1.5 靠既有机制满足，只需回归测试确认） | Success: 新表驱动测试全绿，`go test ./internal/oncall/... -race` 全绿，空 severity_config 下所有既有测试不改断言直接通过（向后兼容判据） | 完成后：log-implementation 落账（artifacts 写函数签名），tasks.md 标 [x]_

- [x] 4. api：升级策略 CRUD 的 severity_config 校验
  - File: internal/api/config.go, internal/api/config_test.go
  - 请求/响应体透传 severity_config；校验：未知 severity、负秒数、overrides 长于 levels、skip 含未知 severity → 400（中文字段级错误）
  - Purpose: R1 配置入口与防错
  - _Leverage: internal/api/config.go 既有升级策略端点与校验错误返回模式_
  - _Requirements: 1.1, 1.6_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（gin API 层） | Task: 扩展升级策略创建/更新端点的 severity_config 校验与透传，测试覆盖四类非法输入均 400 且不落库 | Restrictions: 沿用既有 RBAC（admin only）；错误信息中文、指明具体字段 | Success: go build/vet 零错，api 层测试全绿含四类 400 用例与合法配置 200 往返 | 完成后：log-implementation 落账（artifacts 写请求体 JSON 形状），tasks.md 标 [x]_

- [x] 5. store：OpsRepo（配置 CRUD + 统计 SQL）
  - File: internal/store/ops.go, internal/store/ops_test.go
  - team_ops_settings 的 Get/Upsert（每团队一行，ON CONFLICT (team_id)）
  - `DailyStats`（新增/恢复数、severity 分布、MTTA 均值与 percentile_cont(0.5) 中位）、`ListUnackedCritical`、`ListOpenLongerThan`
  - testcontainers 集成测试：构造带 acked_at 数据验证 MTTA、时间窗边界、无 Ack 时中位为空
  - Purpose: R2 数据层
  - _Leverage: internal/store/ 既有 repo 模式与 testcontainers 测试基建_
  - _Requirements: 2.1, 2.2_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（pgx + SQL 统计） | Task: 按 design.md「R2」接口实现 OpsRepo，统计 SQL 必须走 alerts 既有索引并限定时间窗（注释里写明依赖的索引名） | Restrictions: 方法带 tx 入参、不自开事务；MTTA 只统计有 acked_at 的行，除零/空集返回可判别的零值 | Success: go build/vet 零错，testcontainers 集成测试全绿（MTTA 均值/中位数值断言、时间窗边界用例） | 完成后：log-implementation 落账（artifacts 写全部方法签名与 DailyStats 结构），tasks.md 标 [x]_

- [x] 6. ops 包：settings 模型、nextFire 计算与日报文案纯函数
  - File: internal/ops/settings.go, internal/ops/report.go, internal/ops/report_test.go, internal/ops/settings_test.go
  - settings 领域模型（含 channels 解析）；`nextFire(kind, settings, now) time.Time`（团队时区、HH:MM、interval 截断、tz 加载失败回退 UTC 记日志）
  - 日报/悬置/播报三类文案构建纯函数（输入统计结构，输出 notify.Message；零告警日、MTTA 不可算显示「—」、7 天对比除零、无排班文案）
  - Purpose: R2 可单测的纯逻辑层
  - _Leverage: internal/notify/channel.go 的 Message 结构；internal/domain 的 Severity 常量_
  - _Requirements: 2.1, 2.2, 2.3, 2.6_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（纯函数领域层） | Task: 实现 ops 包的配置模型、nextFire 计算与三类文案构建，全部纯函数、fake clock 可测；文案中文、对敏感 label 脱敏 | Restrictions: 本任务不写 handler、不触 scheduler；nextFire 必须「从当前时间之后最近一个配置时刻」计算（错过轮次跳过语义的根基） | Success: 表驱动单测全绿（时区/跨日/错过轮次/除零/无排班），go vet 零错 | 完成后：log-implementation 落账（artifacts 写 nextFire 与文案函数签名），tasks.md 标 [x]_

- [x] 7. ops 包：三个 scheduler handler
  - File: internal/ops/handlers.go, internal/ops/handlers_test.go
  - 定义 `OpsNotifier` 意图接口（design.md 签名）；三个 handler：读 settings（禁用即返回不续期）→ 统计 → 文案 → 逐渠道 ScheduleOpsSend → 同事务自续期
  - 确定性 Round：日报/播报=团队时区当天 YYYYMMDD，悬置=窗口起点 Unix 秒；合成 GroupKey=`ops:{teamID}:{kind}`
  - 单测（fake store/notifier/clock）：确定性 Round 重算一致、禁用不续期、悬置为空静默但续期、handler 幂等
  - Purpose: R2 编排核心
  - _Leverage: internal/oncall/escalation.go 的 Handle/payload/fake 测试模式；任务 5/6 产物_
  - _Requirements: 2.1–2.6_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（定时编排/outbox 模式） | Task: 实现三个 ops handler，严格遵守 scheduler.Handler 约定（不自开事务、只经 OpsNotifier Schedule 发送意图、绝无同步外部 IO），注释解释确定性 Round 为何不违反「意图时刻捕获」不变量 | Restrictions: handler 内不 import notify.Sender；自续期用 Schedule（新 pending 行，markFired 前置语义）；播报复用 oncall.Resolver 需通过接口注入 | Success: `go test ./internal/ops/... -race` 全绿含幂等重投用例 | 完成后：log-implementation 落账（artifacts 写三个 kind 常量、OpsNotifier 签名、Round 口径——这是跨任务警告重点），tasks.md 标 [x]_

- [x] 8. notify + domain：发送结果写时间线（R3）
  - File: internal/domain/state.go, internal/notify/sender.go, internal/notify/mask.go, internal/notify/sender_test.go, internal/notify/mask_test.go
  - domain 新增 TimelineActionNotifySent/NotifyFailed 常量（旧 TimelineActionNotify 标 deprecated 保留）
  - `maskTarget(kind, target)` 纯函数（邮箱/手机号/URL/ID 四类 + 空串）
  - Sender 成功路径：UpdateStatus(sent) 同事务按 AlertID 或 Msg.AlertIDs 扇出 notify_sent 条目；markFailed 迁移到 notify_failed 并同样扇出；error 截断 200 字符；ops 空 AlertIDs 不追加
  - Purpose: R3 全部后端行为
  - _Leverage: internal/notify/sender.go 的 markFailed 既有时间线写法与 sender_test 基建_
  - _Requirements: 3.1, 3.2, 3.3, 3.4_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（可靠投递/幂等） | Task: 按 design.md「R3」实现时间线回写与脱敏，注释说明「重投由 alreadySent 占位短路兜幂等、无需额外判重」 | Restrictions: 不改幂等键算法与 Round 语义；时间线追加必须与状态回写同一 WithTx；detail 不含凭据与完整敏感标识 | Success: sender_test 扩展用例全绿（成功追加/分组扇出 N 条/重投不重复/空 AlertIDs 不追加），mask 表驱动测试全绿，`go test ./internal/notify/... -race` 全绿 | 完成后：log-implementation 落账（artifacts 写新 action 常量与 maskTarget 签名），tasks.md 标 [x]_

- [x] 9. app 装配：意图接口实现、kind 注册与启动对账
  - File: internal/app/outbox.go, internal/app/app.go
  - outbox 实现 OpsNotifier.ScheduleOpsSend（notify_send payload 携带 GroupKey/Round/channel/target/Msg；refID=`ops|team|kind|channel|target` 不含 round）
  - app.go 注册三个 ops kind handler（与既有六 kind 同处）；启动对账：扫 team_ops_settings 对启用任务 ScheduleIfAbsent(nextFire)
  - Purpose: R2 接线与自愈兜底；cmd 与 e2e 共用装配（不变量：不手写重复装配）
  - _Leverage: internal/app/outbox.go 既有四个意图接口实现与 notifySendRefID 模式；app.go 注册段_
  - _Requirements: 2.4, 2.5, 2.6_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（应用装配/wiring） | Task: 在 internal/app 完成 OpsNotifier 实现、三 kind 注册与启动对账，对账与 API 保存共用同一 nextFire 函数 | Restrictions: 装配只写在 internal/app（cmd 与 test/e2e 复用）；ScheduleOpsSend 在持锁事务内只 Schedule、无 IO；未注册 kind 到期标 failed 是既有语义，注册务必齐全 | Success: go build ./... && go vet ./... 零错（含 test/e2e 包），全量 `go test ./... -race` 不回归 | 完成后：log-implementation 落账（artifacts 写 refID 口径与对账入口函数——跨任务警告重点），tasks.md 标 [x]_

- [x] 10. api：ops-settings 端点
  - File: internal/api/ops_settings.go, internal/api/ops_settings_test.go
  - `GET /api/v1/teams/:id/ops-settings`（登录即可）、`PUT /api/v1/admin/teams/:id/ops-settings`（admin）
  - PUT 校验：channels 经 notify.Validate、HH:MM 格式、interval ≥ 60、timezone 可 LoadLocation；保存事务内按开关 Schedule/Cancel 对应 timer
  - Purpose: R2 配置入口
  - _Leverage: internal/api/config.go 的 CRUD/RBAC 模式；任务 6 的 nextFire；任务 9 的装配_
  - _Requirements: 2.1, 2.2, 2.3（NFR：安全/RBAC）_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: Go 后端开发（gin API + 事务编排） | Task: 实现 ops-settings 两端点，保存事务内完成 timer 的 Schedule/Cancel（开关翻转语义），测试覆盖四类校验 400 与开关翻转后 timers 表状态断言 | Restrictions: GET 登录即可、PUT admin only（403 语义沿用）；校验失败不得部分落库 | Success: api 测试全绿，手工 curl 验证 GET/PUT 往返与 timers 行变化 | 完成后：log-implementation 落账（artifacts 写两端点 method/path/请求响应 JSON），tasks.md 标 [x]_

- [x] 11. web：升级策略表单 severity 覆写编辑器
  - File: web/src/…（升级策略设置组件及其类型/接口文件，遵循现有目录约定）
  - severity_config 编辑：per-severity 各级等待时长覆写 + skip 勾选；预填默认建议值（critical 3/10min、warning 60/120min、info skip）并注明可改
  - Purpose: R1 可用性
  - _Leverage: web 设置页既有升级策略表单与 AntD 5 表单模式、React Query mutation 模式_
  - _Requirements: 1.1, 1.6（NFR：可用性）_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: 前端开发（React 19 + AntD 5 + React Query 5） | Task: 扩展升级策略表单支持 severity_config 编辑与默认建议值预填，秒↔分钟展示换算清晰 | Restrictions: AntD 锁 5 不升 6；不引入新状态库；后端 400 错误信息按字段展示 | Success: cd web && npm run build && npx oxlint 零错，表单往返（含清空覆写回到默认行为）人工走查通过 | 完成后：log-implementation 落账（artifacts 写组件名与 props），tasks.md 标 [x]_

- [x] 12. web：运营任务设置 tab
  - File: web/src/…（设置页新增 tab 组件与 API hook）
  - team_ops_settings 表单：时区、三任务开关/时刻/间隔、channels 列表编辑
  - Purpose: R2 可用性
  - _Leverage: 设置页既有 7 tab 结构与表单模式；任务 10 端点_
  - _Requirements: 2.1, 2.2, 2.3（NFR：可用性）_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: 前端开发（React 19 + AntD 5 + React Query 5） | Task: 新增「运营任务」设置 tab，对接 GET/PUT ops-settings，非 admin 只读展示 | Restrictions: AntD 锁 5；401/403 口径沿用全局约定 | Success: npm run build + oxlint 零错，admin 保存生效、member 只读，人工走查通过 | 完成后：log-implementation 落账（artifacts 写组件与 hook 名），tasks.md 标 [x]_

- [x] 13. web：时间线渲染通知结果条目
  - File: web/src/…（AlertDetail 时间线渲染逻辑）
  - notify_sent →「已通过 {channel} 通知 {target}」；notify_failed →「通过 {channel} 通知 {target} 失败（N 次尝试）」；旧 "notify" action 渲染兼容
  - Purpose: R3 前端呈现
  - _Leverage: AlertDetail 既有时间线 action→文案映射_
  - _Requirements: 3.5_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: 前端开发（React） | Task: 时间线渲染新增 notify_sent/notify_failed 两 case，文案中文，未知 action 兜底渲染不回归 | Restrictions: 只改渲染映射，不动数据获取逻辑 | Success: npm run build + oxlint 零错，构造两类条目的告警详情页人工走查通过 | 完成后：log-implementation 落账（artifacts 写映射位置），tasks.md 标 [x]_

- [x] 14. 全量验证与回归
  - File: 无新文件（必要的修复性小改）
  - `go build ./... && go vet ./...`、`go test ./... -race -count=1`（含 testcontainers）、`cd web && npm run build && npx oxlint`、`test/e2e` 五场景全量回归
  - 真机抽验：配置 60s 悬置扫描灌 critical 不 Ack，断言 notification_log 出现 `ops:` GroupKey 且同轮唯一；重启进程验证启动对账把 failed 链拉起
  - Purpose: 收口验收（R1 向后兼容的最终判据是 E2E 全绿）
  - _Leverage: 全部前置任务；deploy/docker-compose 本地环境_
  - _Requirements: All_
  - _Prompt: Implement the task for spec ops-enhancements, first run spec-workflow-guide to get the workflow guide then implement the task: Role: 集成验收工程师 | Task: 执行全量构建/测试/E2E 回归与两项真机抽验，发现的问题就地修复并复跑 | Restrictions: 只做修复性小改，架构调整须回报主对话裁决；不改测试断言来「让测试通过」 | Success: 四条命令全部零错、五场景 E2E 全绿、两项真机抽验通过并留存证据（命令输出摘要） | 完成后：log-implementation 落账（artifacts 写验收证据要点），tasks.md 标 [x]_
