# Tasks Document — aiops-foundation

任务全局约束（每个任务的 Restrictions 默认包含，不再重复）：禁改 `.spec-workflow/`、禁 git 操作、禁改 go.mod/package.json 依赖（除非主对话明确授权）；中文注释解释「为什么」；`go build ./... && go vet ./...` 零错误；遵守 CLAUDE.md 全部核心不变量与安全约束；**所有降噪逻辑 opt-in，未配置时行为与现状逐字节一致**。

- [x] 1. 迁移 000008 与 config/store 结构扩展
  - File: migrations/000008_noise.up.sql、migrations/000008_noise.down.sql、internal/store/config_repos_routing.go、internal/store/alerts.go（Alert scan 所在文件以实际为准）、internal/config/config.go、internal/config/config_test.go
  - 迁移按 design.md「Data Models」：routes 加 `group_mode TEXT NOT NULL DEFAULT 'label'`、`storm_threshold_per_min INTEGER NOT NULL DEFAULT 0`；alerts 加 `route_id UUID`（NULL，不回填）、`flap_count INTEGER NOT NULL DEFAULT 0`、`flap_window_start TIMESTAMPTZ`、`flapping BOOLEAN NOT NULL DEFAULT false`；索引 `idx_alerts_route_created (route_id, created_at)`；建表 `route_storm_state`；down 全部回滚
  - store.Route 加 `GroupMode string`、`StormThresholdPerMin int`（列清单/scan/Create/Update 全链路）；domain.Alert 加 `RouteID string`、`FlapCount int`、`FlapWindowStart time.Time`、`Flapping bool` 及 alerts 仓储 scan（写入方法本任务不动，由任务 5/8 接）
  - config 加 `FlapWindowSeconds`（默认 0=关闭）、`FlapThreshold`（默认 3）；env FLAP_WINDOW_SECONDS/FLAP_THRESHOLD
  - Purpose: 全 spec 的 schema 与配置基座
  - _Leverage: migrations/000007_contacts.up.sql（编号与风格）、serviceability 任务 9 教训——**列加了 scan 必须同步加**，否则 "number of field descriptions" 全线炸_
  - _Requirements: 1.1, 2.2, 3.1, 4.1_
  - _Prompt: Role: Go 后端工程师（迁移与仓储） | Task: 按 design.md Data Models 落实迁移 000008、Route/Alert 结构与 scan 扩展、config FLAP 两项（含 config_test 默认值与 env 覆盖用例）| Restrictions: 迁移只加列建表不动数据；Route/Alert 既有字段语义不动；route_storm_state 建表列见 design.md C3 | Success: 迁移 up/down 往返；全量集成测试无 scan 列数错误；config 单测过_

- [x] 2. domain：TemplateFingerprint 归一化纯函数
  - File: internal/domain/template.go、internal/domain/template_test.go
  - `func TemplateFingerprint(text string) string`：单遍 token 归一（IPv4、IPv6、UUID、≥8 位十六进制串、连续数字 → `<*>`）后 FNV-64a 哈希取 `%016x`；不含变量的文本原样哈希
  - 表驱动单测：IPv4/IPv6/UUID/hex/纯数字/混合/无变量/空串/纯变量；同模板不同变量 → 同哈希；不同模板 → 不同哈希
  - Purpose: R2 模板归并的领域核心
  - _Leverage: internal/domain/fingerprint.go 的 FNV+分隔符手法_
  - _Requirements: 2.1_
  - _Prompt: Role: Go 工程师（领域纯函数） | Task: 按 design.md C2 实现 TemplateFingerprint 与表驱动全覆盖单测；token 判定用确定性词法规则（按分隔符切分后逐 token 判类），禁止引入第三方库 | Restrictions: 纯函数零 IO；同输入永远同输出；性能 O(len) 单遍 | Success: 全部用例 -race 过；边界（空串、全变量、粘连 token）有显式断言_

- [x] 3. domain：Resolve 增加 note 参数
  - File: internal/domain/（Resolve 所在状态机文件及其测试）、internal/api/alerts.go、internal/app/（TTL 等系统侧 Resolve 调用点）
  - `domain.Resolve(alert, actorName, reason, note, now)`：note 非空时写入 resolve timeline entry 的 detail；空串时 entry 形态与现状完全一致（既有测试零改动预期外的断言）
  - 全部调用点适配：API 层暂传 ""（任务 12 接 REST 字段）、TTL/inhibit 等系统侧传 ""
  - Purpose: R5 的领域层，先把签名铺平
  - _Leverage: domain.Ack 双参数先例（actorID/actorName 教训）；grep `domain.Resolve(` 找全调用点_
  - _Requirements: 5.1, 5.2_
  - _Prompt: Role: Go 工程师（领域状态机） | Task: 按 design.md C5 给 domain.Resolve 加 note 参数并适配全部调用点，补单测（note 入 entry detail；空串形态不变；幂等短路路径不受影响）| Restrictions: 不变量 6 一字不改——幂等短路、无条件清定时器语义不动；note 不落日志 | Success: go build ./... 零错误证明调用点找全；单测过_

- [x] 4. store：StormStateRepo
  - File: internal/store/storm.go、internal/store/storm_test.go
  - 方法（全带 tx）：`Get(ctx, tx, routeID) (*StormState, error)`（无行返回 nil）、`Activate(ctx, tx, routeID, now) error`（UPSERT：active=true、activated_at=now、deactivated_at=NULL、suppressed_count=0 仅在从非 active 转入时重置）、`AddSuppressed(ctx, tx, routeID, n int) error`、`MarkSummary(ctx, tx, routeID, now) error`、`Deactivate(ctx, tx, routeID, now) error`
  - 集成测试：UPSERT 往返、重复 Activate 不重置计数语义按实现约定断言、Deactivate 后行保留
  - Purpose: R3 熔断状态的数据层
  - _Leverage: internal/store/memberships.go 的仓储与集成测试模式_
  - _Requirements: 3.1, 3.2_
  - _Prompt: Role: Go 后端工程师（pgx 仓储层） | Task: 按 design.md C3 的 route_storm_state 表实现 StormStateRepo 五方法与集成测试 | Restrictions: 不自开事务；行只 UPSERT 不 DELETE（历史即记录）| Success: 集成测试 -tags=integration -race 过_

- [x] 5. pipeline：route stage 写 alerts.route_id + 风暴计数查询
  - File: internal/pipeline/stage_route.go、internal/pipeline/stage_route_test.go、internal/store/alerts.go（UpdateRouting 或新方法 + CountByRouteSince）
  - route stage 匹配到路由后把 route_id 持久化到 alerts（扩展 UpdateRouting 签名或新增列更新，与 team_id/group_key 透传口径一致，防误清空）
  - store 新增 `CountByRouteSince(ctx, tx, routeID string, since time.Time) (int, error)`（走 idx_alerts_route_created）
  - Purpose: R3 判定的数据通路
  - _Leverage: stage_route.go 既有 UpdateRouting 透传手法（CLAUDE.md：透传防误清空的对称操作先例）_
  - _Requirements: 3.1_
  - _Prompt: Role: Go 后端工程师（管道 stage） | Task: 按 design.md C3 让 route stage 写 alerts.route_id 并提供 CountByRouteSince 计数查询，补 stage 单测与 store 集成测试 | Restrictions: UpdateRouting 改签名时全调用点透传既有值防误清空（group stage 也调它）；迟到 resolved 零值告警跳过口径不变 | Success: 单测+集成测试过；route_id 在 E2E 种子链路真实落值_

- [x] 6. pipeline：GroupStage 组键三分支（window + __template__）
  - File: internal/pipeline/stage_group.go、internal/pipeline/stage_group_test.go
  - 组键计算改三分支：`route.GroupMode=="window"` → `routeID + "-window"`；否则 group_by 空 → 单例组（现状）；非空 → computeGroupKey，其中保留键 `__template__` 取 `domain.TemplateFingerprint(alert.Title)` 而非 label 值
  - 单测：window 固定组键、不同 fingerprint 落同组；`__template__` 同模板同组/异模板异组、与普通 label 键混用；`label` 模式回归现状
  - Purpose: R1+R2 的管道接入
  - _Leverage: stage_group.go:112-121 组键分支、任务 2 的 TemplateFingerprint_
  - _Requirements: 1.1, 1.3, 2.2, 2.3_
  - _Prompt: Role: Go 后端工程师（管道 stage） | Task: 按 design.md C1/C2 实现组键三分支与单测 | Restrictions: 组键格式与 `-single-`/`%016x` 不碰撞；UpdateRouting/HasFired/ScheduleIfAbsent 编排一行不动（不变量 5）；resolved/零值告警跳过口径不变 | Success: 新分支单测 + 既有 group 测试全绿_

- [x] 7. API：路由字段暴露与校验
  - File: internal/api/config.go（路由 CRUD 所在）、internal/api/ 对应测试、web/src/api/client.ts（Route 类型加字段）
  - 路由 CRUD 请求/响应加 `group_mode`（缺省 'label'）、`storm_threshold_per_min`（缺省 0）；校验：group_mode ∉ {label,window} → 422；`window` 且 group_by 非空 → 422（错误信息指明互斥）；group_by 放行保留键 `__template__`
  - Purpose: R1/R2/R3 的配置面
  - _Leverage: 既有路由 CRUD 校验与 422 口径；client.ts 并发编辑纪律（只 Edit 定位追加）_
  - _Requirements: 1.1, 2.2, 3.4_
  - _Prompt: Role: Go 后端工程师（HTTP API） | Task: 按 design.md C1/C3 暴露两个新路由字段并实现校验与测试；client.ts 只加类型字段 | Restrictions: 缺省值语义=关闭；既有路由请求（不带新字段）行为不变 | Success: 校验用例（合法/非法枚举/互斥/负数阈值 422）全过_

- [x] 8. pipeline：dedup 翻转计数与 flapping 置位/解除
  - File: internal/pipeline/stage_dedup.go、internal/pipeline/stage_dedup_test.go、internal/store/alerts.go（flap 字段更新方法）
  - firing↔resolved 双向转移点计数：窗口滑出（now - flap_window_start > FLAP_WINDOW）→ 重置 count=1、window_start=now；否则 count++；`count ≥ FLAP_THRESHOLD && !flapping` → flapping=true + 时间线 `flapping_start`；下一次转移判定时窗口已滑出 → flapping=false + 时间线 `flapping_end`（惰性解除）
  - FLAP_WINDOW_SECONDS==0 时全部逻辑短路（零行为变化）
  - Purpose: R4 的检测与状态维护
  - _Leverage: dedup stage 既有状态转移点与时间线写入手法；任务 1 的 alerts flap 列_
  - _Requirements: 4.1, 4.2, 4.4_
  - _Prompt: Role: Go 后端工程师（管道 stage） | Task: 按 design.md C4 在 dedup 的状态转移点实现翻转计数、flapping 置位与惰性解除，写时间线，补单测（fake clock 控制窗口）| Restrictions: 只改 flap 三字段与时间线，dedup 合并/fingerprint 语义不动；状态机不受 flapping 影响（AC4.3）；未配置时短路 | Success: 窗口累计/滑出重置/置位/惰性解除/未配置短路五类用例全过_

- [x] 9. pipeline：GroupStage flapping 抑制与风暴熔断判定
  - File: internal/pipeline/stage_group.go、internal/pipeline/stage_group_test.go
  - Process：flapping 告警跳过 ScheduleIfAbsent（置位当次事件除外——以「本次事件把 flapping 从 false 置为 true」为界，置位当次照常注册让提示送出；实现上以 pctx 标记传递，细节实现定）
  - Handle（flush）：members 过滤 flapping 成员，滤后为空则跳过 NotifyGroup；threshold>0 时 CountByRouteSince 判定，超阈值或 state.active → 跳过 NotifyGroup、Activate（如需）+ AddSuppressed(len(members)) + `ScheduleIfAbsent("storm_summary", routeID, now+1min)`；计数查询失败 fail-open 放行通知并返回错误由 timer 重试
  - 组通知消息对 flapping 成员标注「抖动中」（Message 构造处，接口不改）
  - Purpose: R3 作用点 + R4 抑制点
  - _Leverage: 任务 4 StormStateRepo、任务 5 CountByRouteSince、stage_group.go Handle:150-162_
  - _Requirements: 3.1, 3.4, 4.1, 4.3_
  - _Prompt: Role: Go 后端工程师（管道 stage） | Task: 按 design.md C3/C4 在 GroupStage 实现 flapping 抑制与风暴判定，fake 依赖单测覆盖：未配置短路/首次超阈值进入/已熔断累计/滤后空组跳过/fail-open | Restrictions: NotifyGroup 接口签名不改；持锁事务内零外部 IO（不变量 4）；风暴组键从 group_key 反解 routeID 或经 route 上下文获取，实现定但须有测试锚 | Success: 全部单测过；既有 group flush 测试回归绿_

- [x] 10. app：storm_summary handler（第 11 个 timer kind）
  - File: internal/app/storm.go、internal/app/storm_test.go、internal/app/app.go（注册）
  - handler（签名带 tx，不自开事务）：读 state → suppressed_count>0 则按 NotifyGroup 实现同款 outbox 手法 Schedule 一条 notify_send 摘要（累计条数、时间范围、代表样本标题、详情链接；幂等键 `routeID|storm|channel|target|round`，round 意图时刻捕获随 payload 持久化）+ MarkSummary + 计数清零 → 速率复查：仍超阈值续期 +1min；回落 → Deactivate + 对该 route 活跃告警 distinct group_key 逐个 `ScheduleIfAbsent(group_wait, group_key, now+group_interval)` 补组 timer
  - internal/app 注册 kind（未注册 kind 到期标 failed 的既有口径）；handler 幂等（UPDATE/ScheduleIfAbsent 天然收敛）
  - Purpose: R3 的摘要发送与熔断解除
  - _Leverage: internal/app/retention.go（handler+注册+自续期先例）、internal/app/outbox.go（round/幂等键口径）_
  - _Requirements: 3.1, 3.2, 3.3_
  - _Prompt: Role: Go 后端工程师（scheduler handler） | Task: 按 design.md C3 实现 storm_summary handler、注册与单测（摘要意图参数、清零、续期/解除+补组 timer、重投幂等）| Restrictions: 绝不自开事务（跨连接死锁前科）；持锁事务内只 Schedule notify_send；round 绝不能发送时现算；timers refID 不含 round | Success: 单测全过；kind 注册在 internal/app 可 grep_

- [x] 11. E2E：模板归并与窗口全归并两场景
  - File: test/e2e/noise_test.go
  - 场景①：路由 group_by=["__template__"]，投 3 条只差 IP 的告警（固定 startsAt + waitFor 重投手法对齐 10s 路由缓存）→ 断言合为一组、一条聚合通知（锁 alert.ID 集合 + target，防种子编排误判——journey 竞态教训）；场景②：group_mode='window'，投不同 fingerprint 告警 → 同组一条通知
  - Purpose: R1/R2 端到端判据
  - _Leverage: test/e2e/journey_test.go 的 waitFor/断言双锁定手法、harness 既有装配_
  - _Requirements: 1.1, 1.2, 2.2_
  - _Prompt: Role: Go 测试工程师（E2E） | Task: 按 design.md Testing Strategy 写两场景 E2E，断言必须同时锁定告警 ID 与 target | Restrictions: 复用 harness 不重复装配；风暴/flapping 不做 E2E（设计已显式裁剪）| Success: docker compose 环境三次重跑稳定绿_

- [x] 12. API+前端：Resolve note
  - File: internal/api/alerts.go、internal/api/alerts_test.go（如有）、web/src/api/client.ts、web/src/pages/AlertDetail.tsx、web/src/pages/AlertList.tsx（Resolve 入口）
  - REST resolve body 加可选 `note`（>1000 字符 422）；透传 domain.Resolve；幂等短路时丢弃；前端 Resolve 改弹窗（可选 TextArea）+ 详情时间线展示 note
  - Purpose: R5 端到端
  - _Leverage: 任务 3 的 domain 签名；飞书卡片回调不带 note（传 ""，语义不变量 6 两端一致）_
  - _Requirements: 5.1, 5.2, 5.3_
  - _Prompt: Role: 全栈工程师 | Task: 按 design.md C5 打通 REST note 字段与前端弹窗/时间线展示 | Restrictions: 幂等口径不动；note 不落日志；client.ts 只 Edit 定位追加 | Success: 带 note/不带 note/超长 422/重复 resolve 短路四用例过；npm run build + oxlint 绿_

- [x] 13. 前端：路由表单与 flapping 徽标
  - File: web/src/pages/Settings/Routes.tsx（路由编辑所在，以实际为准）、web/src/pages/AlertList.tsx、web/src/pages/AlertDetail.tsx
  - 路由表单：分组模式 Select（按标签分组/窗口全归并）+ 风暴阈值 InputNumber（0=关闭，help 文案说明）；window 模式下 group_by 输入禁用并清空；告警列表/详情 flapping Tag「抖动中」
  - Purpose: R1/R3/R4 配置与可见性
  - _Leverage: 任务 7 的 client.ts 字段；AntD 5.29.3 既有表单模式_
  - _Requirements: 1.1, 3.4, 4.1_
  - _Prompt: Role: 前端工程师（React+AntD5） | Task: 按 design.md C6 实现路由表单两字段与 flapping 徽标 | Restrictions: AntD 锁 5 不引入 6 的 API；window 模式与 group_by 的互斥在前端也约束（后端 422 兜底）| Success: npm run build + npx oxlint 绿；表单交互自测通过_

- [x] 14. 全量回归收口 + CLAUDE.md 红线落笔（主对话执行，不派子代理）
  - File: CLAUDE.md、test/e2e/（如需修补）
  - 主对话亲手：①CLAUDE.md 核心不变量补两条——storm_summary 第 11 个 kind、AI enrichment 红线条款（design.md R6 章节定稿文本）；②四条验证命令全绿（build/vet/全量集成/前端）；③grep 抽验：opt-in 零值短路点、幂等键口径、kind 注册
  - Purpose: R6.2 + 全 spec 回归判据
  - _Leverage: serviceability 任务 17 收口清单模式_
  - _Requirements: 6.1, 6.2, 6.3_
  - _Prompt: 主对话自任务，不派发。子代理禁改 CLAUDE.md 的约定在此由主对话亲自执行闭环 | Success: 四条命令全绿；CLAUDE.md 两条新条款可 grep；测试计数相对 534 的增量与任务清单吻合_
