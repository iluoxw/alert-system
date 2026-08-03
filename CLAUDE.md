# alert-system

Go 告警管理系统（MVP）：接入已触发的告警事件（metric/log/trace 归一），经管道处理后按策略通知、升级、自愈预留。架构大量借鉴 Alertmanager / PagerTree 实践。规格与任务由 spec-workflow 管理（`.spec-workflow/specs/alert-system-mvp/`，dashboard 用 5001 端口，5000 被 macOS AirPlay 占用）。交付时间线与接手须知见 `CHANGELOG.md`。

## 产品定位与领域边界

- 长期定位是 Infrastructure-first 的事故响应操作系统：先理解共享基础设施及其业务影响，再进入 Incident 响应；告警配置后台只是控制平面，不是产品主体。
- 核心用户是维护大量共享基础设施能力的平台型 SRE；一项基础设施能力可支撑多个业务系统，一个业务系统也会依赖多项基础设施，因此资源关系必须建模为多对多依赖图，工作台要优先呈现基础设施健康、受影响业务和爆炸半径。
- 领域链路是 `Signal → Alert → Fault → Incident → Response → Recovery → Learning`：Alert 是异常证据，Fault 是资源或基础设施能力上的技术故障，Incident 是需要团队协作的响应事件，三者不可混同。
- 第一阶段由外部 Prometheus/Thanos Ruler/vmalert 负责规则求值，Alertmanager 是 Alert 生命周期、去重、分组、路由、静默、抑制和 Alert 级通知的唯一权威；SRE Control 只同步 Alert 状态并负责实体绑定、Fault、Impact 和 Incident，不自建 Ruler，也不对 Alertmanager 来源再次执行本地 route/group/silence/inhibit。规则工作站不属于第一阶段。
- 默认工作台应围绕数据源健康、当前技术 Fault、应用服务影响、负责团队和活跃 Incident 展开，按 confirmed impact、Fault severity、potential 数量和持续时间排序；原始 Alert、应用服务目录和分层拓扑作为下钻入口。进入 Incident 后才切换到责任接管、协作处置、通信、恢复和复盘；Alertmanager Source、AlertMapping、实体认领和团队登记统一收敛到控制中心。
- 第一阶段优先建立跨 `infrastructure / platform / application` 三层的通用实体依赖图、Fault、FaultAlertLink 和 Incident 最小闭环，并复用现有告警、时间线、通知日志、排班和 action rule 基础；爆炸半径先止于应用服务及其负责团队，不提前建模业务能力和用户旅程。
- 产品与实现的黄金验收场景是 E2B `500: Failed to place sandbox`：从缺失快照对象、无效 auto-resume/重连、重复或孤儿 Firecracker、CPU/HugePages 压力到 Sandbox create/resume SLO 受损，贯通拓扑、证据、Fault、Incident、Runbook、恢复和飞书通报。
- E2B 只是首个黄金场景，不是产品根对象。ManagedSystem 可作为责任、配置和导航视图，但不强制成为拓扑根；共享基础设施允许跨多个系统提供服务，核心页面与服务禁止依赖 `e2b.*` 等领域私有键。
- 采集 `source` 只表示数据来源，不能兼任实体身份或拓扑命名空间。默认按协议/技术类别复用 Alertmanager、Grafana、OTel 和拓扑适配器；新业务优先通过标准资源标识、应用服务登记和依赖关系接入，只有标准契约无法表达私有拓扑或受控动作时才增加业务专属 Connector。
- 扩展依赖方向固定为“接入方适配 SRE Control 核心契约”，禁止核心为每个业务增加私有实体、状态机、页面分支或 action key 判断。第一版核心契约收敛为跨层 Entity、Relation、Alert、Fault、FaultImpact 和负责团队；层级用于分类/展示，影响计算以多对多关系图为准，不能把 IaaS/PaaS/应用层误实现成一棵严格树。
- E2B 验收必须区分“placement attempt 失败很高但用户 create 仍成功”与“资源泄漏/过载已击穿用户 SLO”两个阶段：前者只形成 Fault 和风险提示，后者才建议宣布 Incident。
- `Failed to place sandbox` 不是领域实体名：一次节点 placement attempt 失败是 Signal，用户请求窗口超阈值是 Alert，单 SID 无法恢复是实例级 Fault，create/resume SLO 失守是能力级 Fault，需要跨团队响应时才由人宣布 Incident。
- 关联必须同时支持两个方向：常规基础设施告警可自底向上形成 candidate Fault/风险预警；用户级 `Failed to place` 超阈值后，系统也要自顶向下回溯拓扑、近期 Alert 和变更，构建根因候选、放大因素和排除证据。
- 硬验收是可重复的本地故障回放，最终验收是经单独授权的 E2B UAT 实机演练；生产 E2B 不作为自动化测试靶场。
- 产品边界不是通用 ITSM：不扩张为工单资产管理平台；AI 只能基于证据辅助总结、关联和建议，不替代事故负责人做高风险决策。

## 技术栈

- 后端：Go 1.25、gin v1.12、pgx v5.7（不用 ORM）、PostgreSQL 16、testcontainers-go（store 集成测试）
- 前端（web/）：Vite + React 19 + TS + AntD **5.29.3（锁 5，不升 6）** + React Query 5 + react-router-dom 7；无 Redux/Mobx；dev proxy `/api`→`:8080`
- 队列/定时器：不引入外部 MQ，统一用 PG `timers` 表（决策：先 PG 队列）
- 容器构建统一使用 `docker buildx build`；本地构建必须显式 `--load`，发布镜像使用
  `--push`，禁止退回传统 `docker build`/`docker push` 流程。

## 目录结构

- `cmd/server/` 薄入口；`internal/app/` 共享装配（wiring），cmd 与 test/e2e 共用，**不要在两处手写重复装配**
- `internal/domain/` 纯函数领域层（状态机、matcher、指纹）；`internal/store/` 仓储（所有方法带 `tx pgx.Tx` 参数，事务由调用方开）
- `internal/pipeline/` 管道 stage；`internal/ingest/` webhook 接入 + AM/Grafana 适配器；`internal/scheduler/` 统一定时器；`internal/oncall/` 值班与人员升级链；`internal/notify/` 通知策略/渠道/生命周期；`internal/api/` HTTP 层
- `migrations/` golang-migrate 格式，编号顺延（当前到 000021）；`test/e2e/` docker-compose 真实 PG，包含 E2B 黄金场景纵向切片

## 核心架构不变量（改代码前必读）

1. **管道顺序**：dedup → alert mapping → route → group → silence → inhibit（以 stage 文件内注释与 app 装配为准）。Alertmanager 权威来源只执行显式标记为 authoritative-safe 的 dedup + mapping，继续跳过后四段本地控制面。
2. **十一个 timer kind**：`alert_ttl`、`alert_repeat`、`personnel_escalation`、`group_wait`、`channel_escalation`、`notify_send`、`ops_daily_report`、`ops_stale_sweep`、`ops_oncall_broadcast`、`retention_sweep`、`storm_summary`。新增 kind 必须在 internal/app 注册 handler，未注册 kind 到期会被标 failed 不重试。
3. **scheduler 事务模型**：per-timer 独立事务；顺序是先 markFired 再调 handler（同事务），handler 失败整体回滚后另开小事务标 failed。`scheduler.Handler` 签名带 `tx pgx.Tx`，**handler 绝不能自开事务**——自开事务里 UPSERT 同 (kind, ref_id) 会与外层持有的行锁形成 PG 检测不到的跨连接死锁（曾把唯一 worker 永久焊死）；markFired 前置是为了让 handler 续期 INSERT 出新 pending 行而不是被清掉。handler 必须幂等（at-least-once 重投）。人员/渠道升级 handler 的续挂与 Ack/Resolve 的 Stop 之间必须经过 `timer-chain` 事务级 advisory 读写屏障：handler 已持有 timer 行锁，只能 `try shared`，失败就无副作用结束；Stop 必须先拿 exclusive、等待已开始的 handler 提交，再取消其新续挂的 pending timer。禁止把 handler 改成阻塞式 shared lock，否则会重新引入 timer 行锁与 Stop 的锁顺序反转。
4. **通知发送解耦（outbox）**：告警的四个通知意图接口（oncall.Notifier / pipeline.GroupNotifier / notify.RepeatNotifier / notify.Dispatcher）以及 Incident 状态通报都在持锁事务内被调，**只准 Schedule 一条 `notify_send` 定时器，禁止同步外部 IO**。幂等键口径：`AlertID或GroupKey或IncidentID|step|channel|target|round` 用 `"\x1e"` 连接——round 是**意图时刻**捕获的时间戳，随 payload 持久化（绝不能发送时现算，否则崩溃重投键会变）；timers 的 refID 保持业务元组**不含 round**（保留 pending 收敛防抖）。没有 round 维度曾导致"终身只发一次"，repeat/静默恢复全部被吞（违反 R6.5）。唯一键兜的是逻辑性重复调度，不兜"HTTP 已发未提交时崩溃"的重发窗口（at-least-once 固有代价，已接受）。Incident 创建/状态推进与通报意图必须同事务提交，发送结果写入 Incident 独立时间线。
5. **无分组直发路径不存在**：路由 group_by 为空时走单例组（键 `{routeID}-single-{fingerprint}`），复用 group_wait/group_interval 同一编排，对齐 Alertmanager 语义。不要重新引入 directDispatch。
6. **处置幂等口径**：重复 Ack/Resolve 按当前状态短路 200 返当前状态（不 409、不重写时间线）；无论是否真转移都无条件调 Stop/CancelLifecycleTimers（不变量不依赖历史）；`ErrAlertResolved`/`ErrInvalidTransition` → 409。飞书卡片回调与 REST 端点必须保持同一语义。
7. **actor 双参数**：`domain.Ack(alert, actorID, actorName, now)`——actorID 进 `alerts.acked_by`（UUID 列，可空），actorName 进 timeline（TEXT，必填）。别再把用户名喂给 UUID 列。
8. **配置缓存提交边界**：删除静默必须同一事务先 `DetachSilence`（array_remove alerts.silenced_by）再 Delete；Route/Silence 配置 API 只能在事务成功提交后调用 `CacheInvalidator`，让下一条事件立即重新加载。直接绕过 API 改数据库时仍只保证 10s TTL 内自愈。
9. **AI enrichment 红线**：任何 AI/LLM 调用都是外部 IO，只准发生在异步 enrichment 层（管道外、任何持锁事务外，消费已提交数据）；管道 stage、scheduler handler、四大通知意图接口内禁止出现模型调用。LLM/enrichment 组件不可用时，系统全部既有功能必须 100% 不受影响——AI 是增强不是依赖。告警内容外发模型 API 前必须按既有口径脱敏。
10. **降噪一律 opt-in**：group_mode 默认 'label'、storm_threshold_per_min 默认 0、FLAP_WINDOW_SECONDS 默认 0——零值/缺省时全部降噪逻辑短路，行为与引入前逐字节一致。新增降噪能力必须遵守同一口径。
11. **Incident 必须人工宣布并受双重关闭门槛约束**：自动关联只能创建/更新 Fault；任意 scope 的 `active Fault + confirmed business_system impact` 均可由登录用户宣布 Incident，应用服务 Fault 也允许作为响应起点。同一个 Fault 至多绑定一个 Incident。进入 monitoring 前必须满足全部 mitigation criteria，并把所有 `included + active` 关联 Fault 推进为 monitoring；进入 resolved 前必须同时满足 resolution/观察窗口、所有响应任务为 `done/canceled`，且不存在 `included + active` Fault，再把所有 `included + monitoring` Fault 推进为 resolved。前后端都要提示门槛，后端事务内必须再次校验，禁止任意 PATCH 状态绕过。
12. **Runbook 只执行白名单类型化动作**：禁止 command/shell 入口。生产默认 `DisabledExecutor`，未显式接入受支持 API 时宁可审计失败也不退化成本机命令。`cleanup_apply` 只能由 Incident Commander/Technical Lead 执行，必须引用成功且未过期的 dry-run、重新计算并匹配计划内容 SHA256、再次确认停止重试、持有相同 hash 的有效审批，且审批人与执行人分离；所有拒绝也必须落 `runbook_runs` 和 Incident 时间线。`e2b.lifecycle_verify` 永远要求单独 UAT 授权，普通 API 参数不能自行开启；`.uat` 域名本身不是隔离证据，执行器还必须校验服务端环境/模板/授权白名单与精确 ingress IP，禁用环境代理，不回退使用生产 URL/key，并在任何已创建 Sandbox 的路径上独立超时清理。外部执行开始后，即使 HTTP 请求被取消，清理与最终审计也必须分别使用脱离 request cancellation 的有界 context，禁止遗留永久 `running` 记录。
13. **API 审计输出必须安全且结构稳定**：集合字段即使为空也返回 `[]`，不能返回 `null` 让页面崩溃；Incident 的通信目标对外响应只允许返回脱敏后的审计引用（Webhook 最多到 origin），原始目标只留在可靠发送内部，禁止经详情、列表、总览或 Fault 关联响应泄露。
14. **Alertmanager 是第一阶段 Alert 唯一权威**：外部 Ruler 求值并向 Alertmanager 发 Alert；SRE Control 的 Alertmanager 接入只做可靠同步、幂等落库、实体绑定和 Fault 关联，禁止再次应用本地 route/group/silence/inhibit 或发送重复的 Alert 级通知。Alert 必须拆成 Alertmanager 写入的 `source_state=firing/resolved/unknown`、用户写入的 `response_state=unacknowledged/acknowledged` 和 Alertmanager 写入的 `suppression_state=none/silenced/inhibited`；Ack/Handoff/Note 只是本地响应协作，Alertmanager 来源禁止人工 Resolve。同步基线是普通通知 Webhook 实时入队加 `/api/v2/alerts` 周期对账，Event Recorder 仅为可选增强；HA 副本归属同一逻辑 AlertSource，occurrence 幂等键使用 `source_id + upstream_fingerprint + starts_at`。Alert webhook 唯一入口是 `POST /ingest/v1/hook/{hook_uuid}`：hook UUID 是只显示一次、只存 SHA-256、可轮换的 URL bearer credential，通过它反查稳定 source_id；禁止兼容 source 主键路径或 Authorization/X-Ingest-Token Header 鉴权。Alertmanager 不可用或数据过期时标记来源 stale，禁止把“暂时看不到”推断为 resolved。SRE Control 只拥有 Fault/Impact/Incident 生命周期。
15. **实体身份与来源解耦**：跨源拓扑使用 canonical Entity、EntityAlias 和 EntityRelation；Prometheus、Kubernetes、CMDB、OTel 等来源的 external ref 只能作为 alias，不能各自创建互不相认的业务实体。应用服务及 `owner_team_id` 必须由显式服务登记提供权威值；遥测仅可自动发现 `unmanaged` 待认领实体，禁止依据不可信 label 自动创建团队或改变归属。关系边统一以消费者/从属对象为 `from`、提供者/承载对象为 `to`，爆炸半径从故障提供者沿反向边遍历。
16. **图可达不等于真实影响**：第一版 FaultImpact 只使用 `potential / confirmed / ruled_out` 三态，不引入模糊的 likely，也不自动推理副本、主备、ISR 或容量。EntityRelation 区分绑定拓扑快照的 `observed` 与服务负责人维护的 `declared`，方向统一为 `consumer → provider`；跨来源反向遍历最大深度 8，只能生成 potential。只有应用 SLO/合成探针 direct evidence 或人工确认才能变为 confirmed，持续健康证据或人工排除才能变为 ruled_out；人工裁决永远不被自动重算覆盖。只有 active Fault 加至少一个 confirmed 应用服务 Impact 才允许宣布 Incident；observed 关系过期只能标 stale，不能提高影响置信度或自动确认。
17. **自动 Fault 必须显式准入**：只有命中 AlertMappingPolicy 或携带受信任标准 `sre.entity.key / sre.fault.kind / sre.evidence.role` 标签的 Alert 才能自动创建或更新 Fault；未映射 Alert 进入待治理队列，禁止按 alertname 猜测建 Fault。correlation key 由 canonical entity、fault kind 和声明的关联标签生成，不含业务硬编码前缀。direct firing 可建立 active Fault；仅 supporting/cause_candidate/trigger/amplifier/symptom 证据最多建立 candidate。Silenced/Inhibited 只影响通知，仍可作为 Fault 证据。
18. **Incident 是多 Fault 响应容器**：一个 Incident 可关联多个跨层 Fault，并保留一个可审计变更的 `primary_fault_id` 表示当前主要诊断；更换 primary 必须追加时间线，不得删除旧 Fault、关系或证据。事故初期可由应用服务 Fault 发起，后续再关联基础设施根因 Fault。关闭前所有关联 active Fault 必须进入 monitoring/resolved 或被人工明确排除，禁止只关闭当前 primary。
19. **全局导航只表达 SRE 一级任务**：运行总览、故障、事故、告警证据和直接响应动作属于全局入口；数据源、告警映射、团队/服务、用户、值班、升级、通知和自动化统一收进分组控制中心，禁止再同时平铺到全局侧栏和页面横向 Tab。每个一级入口必须对应独立 URL、页面职责和用户任务，禁止用查询参数只改标题或滚动位置来伪装成多个页面。一级菜单与对应页面标题统一使用中文，Alertmanager/Webhook 等产品名或协议名可保留原文。全局侧栏必须支持桌面端折叠、记忆用户偏好并在窄屏自动紧凑，折叠态仍须保留语义名称、工具提示和退出入口。业务页面禁止常驻展示“唯一权威、能力边界、系统如何工作”等产品机制说明；这类内容进入文档、按需帮助或控件 Tooltip，页面级 Alert 只保留真实异常、风险、状态冲突或当前操作后果。尚无可用页面的未来能力不占据一级导航；设置表单必须按用户任务组织，不能直接平铺数据库字段。受管用户、团队、服务、故障等引用必须使用目录驱动、可搜索且非虚拟化的选择器，禁止要求用户手输用户名或 UUID；后端没有类型化语义或目录 API 时不得伪造下拉能力。
20. **pgx 单连接结果集必须先释放再查询**：store 在同一个 `pgx.Tx` 中执行二阶段查询时，必须先完整扫描主 `Rows`、检查 `rows.Err()` 并显式 `Close()`，再发起 Alias/子资源查询；禁止在仍打开的流式结果集内嵌套查询，否则真实请求会报 `conn busy`。可一次 JOIN/聚合时优先一次取回。
21. **事件失败必须可提交且有退避**：pipeline 外层批次事务负责 Dequeue 与最终状态，每条事件必须在独立 PostgreSQL 保存点内执行业务 stage 与 MarkProcessed；stage 失败先回滚该保存点，再由仍健康的外层事务 MarkFailed，禁止在已经 aborted 的事务分支继续写失败状态。任何批次级轮询错误都必须无条件退避，不能因已取到事件而形成 CPU/日志重试风暴。

## 安全约束（必须持续遵守）

- 一切凭据 token 存 `hex(sha256(token))`，明文仅在创建响应里返回一次；webhook 走 integration token 鉴权；飞书回调必须验签（verification_token 非空校验）
- 密钥/token/密码不落日志（config 已做 DSN 密码掩码）；通知渠道内容对敏感字段脱敏；飞书凭据走 config
- 诊断进程时禁止使用 `ps eww`、`env`、`/proc/*/environ` 等环境转储命令；只读取脱敏启动日志或逐项检查明确的非敏感配置。意外打印的继承凭据一律按已暴露处理并提示轮换
- 登录：bcrypt + dummy hash 防用户名枚举时序侧信道；session token crypto/rand 32B，cookie 存明文、库存哈希，固定过期 + 滑动续期
- RBAC：admin =「至少一个团队的 admin 角色」；处置端点仅需登录（member 可处置）；`/api/v1/admin/*` 全部 admin only（403）

## 常用命令

```bash
go build ./... && go vet ./...          # 必须零错误（含 test/e2e 包）
go test -tags=integration ./... -race -count=1  # 全量单测（store 测试带 //go:build integration 标签，漏 tag 会静默跳过 store；testcontainers 需 Docker）
cd web && npm run build && npx oxlint  # 前端验证（tsc -b 为准，编辑器诊断常 stale）
cd test/e2e && docker compose up -d    # E2E 依赖的真实 PG（initdb 自动跑 migrations）
```

`test/e2e` 使用固定 Compose project/端口，任何时刻只能运行一轮。终端命令返回 `session_id` 时必须用 `write_stdin(session_id)` 读取到真实结束；外层工具的 cell id 不能替代终端 session，禁止在未确认前一轮结束时补跑第二轮。

本机有 rtk 命令代理 hook（token 优化）；rg 传含 `|` 的模式时用 `grep -nE 'a|b'` 风格，避免未转义括号被解析为分组。

## 协作约定

- 中文注释/文档/回复；代码注释解释「为什么」与约束，不复述代码
- 具体编码派 coder 子代理（sonnet）执行，主对话做架构裁决与实测验收；SendMessage 派任务全文放 message 字段（content 会丢）
- spec-workflow 实现协议：任务标 `[-]` → 派发 → 实测抽验 → log-implementation 落账（artifacts 写可 grep 的接口签名与跨任务警告）→ 标 `[x]`；子代理禁改 `.spec-workflow/`、禁 git 操作、禁改 go.mod（除非授权）
- 前端 401 口径：`/auth/login` 自身 401 特判不走全局跳转（防死循环），其余 401 清 React Query 缓存跳 /login

## 已知悬置（不阻塞）

- alerts.team_id / acked_by 补 FK 迁移
- web npm audit 2 个 high（发布前处理）
- AlertList 与 AppLayout 双登出按钮视觉冗余（后续 polish）
