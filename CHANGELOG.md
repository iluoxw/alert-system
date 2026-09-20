# Changelog

> 面向接手人的交付记录：按时间倒序，每节列「交付了什么、关键决策、验收状态」。
> 架构不变量与协作约定见 `CLAUDE.md`（改代码前必读）；规格文档见 `.spec-workflow/specs/`。

## [Unreleased]

## [v0.3.0] 2026-09-20 — Fault 成为一等工作单元

> 规格 `.spec-workflow/specs/era3-fault-workunit/`（45 个任务，本版交付 30 个），ADR-0006。
> 核心变化：Fault 从"告警的聚合产物"变成有归属、可认领、会主动通知的工作单元；
> Alert 生命周期层收敛到 Alertmanager 与 Grafana；新增平台拓扑接入与一组先行指标。
> 实现分 P0–P5 六批，由 coder 子代理执行、主对话逐批 review，每批全量验证后合入。

### 行为变更（升级前必读）

- **`kind=grafana` 集成改为 Alert 权威来源**（不变量 24）：必须携带 fingerprint，不再执行本地
  route/group/silence/inhibit，也不再发送 Alert 级通知；对账端点
  `/api/alertmanager/grafana/api/v2/alerts`。仍依赖本地编排的接入必须改用 generic 来源。
- **Fault 进入 active 会主动通知 owner team**（不变量 22）。目标取该 team 的
  `team_ops_settings.channels`，缺失回落 `FAULT_NOTIFY_FALLBACK_*`。**上线前必须配置保底目标**，
  否则无 owner 渠道的 Fault 只写时间线、无人收到。冷却、拓扑抑制、无目标三种跳过都写 Fault 时间线。
- **`/readyz` 新增后台任务心跳判定**：scheduler 或 pipeline 超过 3× 轮询间隔没有成功轮询返回 503
  （`/healthz` 语义不变，恒 200）。k8s 探针切换是独立的人工动作，本版未执行。

### 新增能力

- **Fault 工作单元**：`owner_team_id`、`acked_by`、`acked_at`（迁移 000026）；
  `POST /api/v1/faults/:id/ack` 同事务认领 + 时间线，重复认领按不变量 6 返回 200；Web 故障列表与工作台认领入口。
- **ccloud 平台拓扑接入**：新包 `internal/ccloud`（client / importer / syncer），独立周期 loop，
  事务外拉取、单短事务写入（不变量 23）。未命中别名的身份进 `unresolved_identities` 待对齐队列，
  绝不自动建实体（不变量 15）；平台 `totalCount=0` 的边落 `no_recent_traffic`，遍历时按 stale 处理（迁移 000027）。
  `GET /api/v1/admin/topology/unresolved` 与服务目录「待对齐身份」Tab 供人工认领。
- **通知发送最终失败兜底**：重试耗尽后向保底目标发一条元告警，让失败被人看见而不是静默丢弃；
  元告警自身失败只记日志与时间线，不再递归。
- **先行指标**：`GET /api/v1/ops/indicators`（admin only）——firing 告警映射率、无 owner 的 active Fault 数、
  Fault 认领时长中位、恢复提醒发出次数（近 24 小时）、未对齐身份数；日报追加「先行指标（平台级）」小节，
  运行总览加卡片。无数据一律返回 null 并显示「暂无数据」，不用 0 冒充。

### 新增配置

| 配置 | 说明 |
|---|---|
| `CCLOUD_BASE_URL` / `CCLOUD_TOKEN_ENV` / `CCLOUD_PROJECT_IDS` / `CCLOUD_ENVIRONMENT` / `CCLOUD_SYNC_INTERVAL` | 平台拓扑同步。**未配 `CCLOUD_BASE_URL` 时整个特性不生效、不产生任何 IO**；配了则 `CCLOUD_ENVIRONMENT` 必填（平台响应不含环境信息，不能从项目 ID 推断） |
| `CCLOUD_PATH_FULLLINK` / `CCLOUD_PATH_SERVICE` / `CCLOUD_PATH_HEALTH` | 三个接口路径当前是占位符，拿到真实路径后只改配置 |
| `FAULT_NOTIFY_COOLDOWN` / `FAULT_NOTIFY_FALLBACK_CHANNEL` / `FAULT_NOTIFY_FALLBACK_TARGET` | Fault 通知冷却与保底目标（target 是凭据） |
| `WEB_BASE_URL` | 通知里拼 `/faults/{id}` 详情链接 |

### 迁移

`000026`（Fault 归属与认领列）、`000027`（`unresolved_identities` 表、`dependency_edges.no_recent_traffic`）。
`000028` 预留给后续健康证据任务，其他改动不得占用。

### review 中发现并修复的缺陷

- **对账快照首次出现的告警不投影**：webhook 丢失或延迟时对账是唯一入口，该路径不触发 Fault 通知等同漏报。
- **元告警与原通知共用幂等键**：保底目标常常就是原通知失败的那个目标，元告警会把「从未送达」的
  `notification_log` 改写成 `sent`，既抹掉要暴露的失败，也让送达指标把失败计成成功。
- **恢复提醒指标没有统计窗口**：`timers` 的 fired 行会被 retention 清理，全表计数先涨后落、含义随保留配置变化。
- ccloud syncer 误把项目 ID 当作实体 environment 写入。

### 未完成（15 个任务，规格内）

- 健康证据与影响外播（P4，任务 28–33、35、36）：阻塞于平台团队提供 `CCLOUD_HEALTH_WINDOW` 与真实接口路径。
- 飞书交互卡片（任务 19、34、45）：阻塞于 uat 的公网回调入口。P2 通知走纯文本 + 详情链接，认领在 Web 完成。
- k8s 探针切到 `/readyz`（任务 43）、uat 配置保底通知目标（任务 44）：人工动作。

### 验收

`go build` + `go vet`（含 integration tag）零错误；`go test -tags=integration ./internal/... -race` 19 个包全绿；
`test/e2e` 通过（约 65s）；`web` build 与 oxlint 通过。

## [v0.2.0] 2026-09-17 — Infrastructure-first 事故响应发布基线

> 覆盖 v0.1.0 之后的全部改动：定位反转为 Alertmanager 权威 + Entity / Fault / Impact / Incident，
> 通知发送移出事务，以及 2026-09-12 uat 首次端到端运行后的修复。下列小节按时间倒序。

### 2026-09-17 — uat 首次端到端运行后的修复与补强

背景：2026-09-12 在 uat 首次跑通 Alert → Fault → Incident 全链路，随后暴露出以下缺口。用户裁决直接实施，未立 spec；
编码由三个并行 coder 子代理在隔离 worktree 完成，主对话逐项审查、修正后合并。

- **Incident 恢复提醒（新增第 12 个 timer kind `incident_recovery_nudge`）**：INC-000001 在 monitoring 停留 4 天无人收尾，
  系统没有任何提示。现在 open Incident 的全部 included Fault 有效证据告警都已解除时，按 `INCIDENT_NUDGE_INTERVAL`
  （默认 6h，`0` 关闭）经既有 Incident 通报 outbox 发提醒，列出证据解除时间与关闭门槛还差什么；**永远不推进状态**（不变量 11）。
  Declare 时 `ScheduleIfAbsent`、resolved 时同事务 Cancel、启动时 `ReconcileIncidentNudgeTimers` 对账存量 Incident。
  - 文案刻意用"解除"而非"恢复"，并固定提示人工确认：线上取证发现 `E2BNomadAgentDown` 因 Nomad client 从 Consul
    注销、采集目标消失而在 Alertmanager 侧"resolved"，但 4646 端口实际仍不可达——告警解除不等于故障恢复。
  - 主对话修正：持续时长改为从进入当前状态起算（原实现从 `declared_at` 起算，会掩盖"卡在观察期"这一信号）；
    补权威来源 `source_state` 判定单测（uat 证据全部来自 Alertmanager，原测试只覆盖了兼容来源分支）。
- **incident.Service 服务层集成测试**：此前 533 行编排零服务层测试。新增 `internal/incident/service_integration_test.go`
  覆盖 Declare 准入、monitoring/resolved 双重门槛与 Fault 联动、primary 切换审计、excluded 解除阻塞、非法迁移。未发现缺陷。
- **受管实体支持别名与依赖**：`POST/PUT /api/v1/admin/entities`（infrastructure/platform 层）新增 `aliases`、`dependencies`，
  控制中心「服务目录」受管实体表单可编辑（目录驱动选择器，不变量 19）；`GET /infrastructure/nodes/:id` 回显。
  此前只有应用服务能挂别名，`prometheus.instance → 主机` 这类最常见的映射只能手写 SQL。别名/依赖读写抽为
  `store.replaceEntityAliases/replaceEntityDependencies/listEntity*` 由服务目录与受管实体共用，依赖边按 source 隔离。
  - 响应保持向后兼容：实体字段仍平铺在顶层，只追加 `aliases`/`dependencies`。子代理原先改为 `{"node",…}` 包装，
    E2E `TestGenericEntityRelations_MultiServiceBlastRadius` 因读不到顶层 `id` 失败，证实会打坏现有调用方，已改回平铺。
  - 主对话修正：PUT 中字段缺失（nil）表示保持不变、显式 `[]` 才清空。原实现一律整体替换，旧客户端或缓存的旧前端
    一次改名就会抹掉全部别名与依赖边，告警映射随之静默失效。
  - 顺带：依赖目标不存在由 500 改为 422（服务目录同样受益）；依赖边 `evidence.authority` 统一为 source 值。
- **路由级 `repeat_interval` 生效**：此前 `RouteResult.RepeatInterval` 写入后零消费，UI 表单可保存可回显但完全不生效。
  现在首个 `alert_repeat` 注册时按路由解析间隔写入 payload，续挂原样沿用；缺失/0 回落全局 `REPEAT_INTERVAL`，
  升级前的 nil payload 定时器行为不变。前端默认值由 3600 改为 0（使用全局默认），API 拒绝负数。
  - 主对话修正：路由查询的非 NotFound 错误改为上抛（同一 pgx.Tx 内吞错只会让后续语句报 aborted 并掩盖根因）。
- **分组通知列出成员**：`buildGroupMessage` 原先 Body 只有第一条告警标题；现在按严重度 → StartsAt → Title → ID 全序排序，
  最多列 5 条并以"…另有 N 条"收尾。主对话修正：原排序缺少 Title/ID 兜底，同级同时刻乱序到达会产出不同 Body。
- **uat 数据修正（非代码）**：补 `host:10.61.17.12/13` 实体与 `:4646/:9100` 别名、`capability:e2b-nomad → 三台主机` 依赖；
  手工数据归入 `entity_registry`/`service_registry` 以便界面编辑；`E2BNomadAgentDown` 从 `host_unreachable` 拆出为独立
  `nomad_agent_down`（主机已恢复但 agent 未恢复，两者不应收敛）。
- **修复 `retention_sweep` 从未启动**：`app.ReconcileRetentionTimer` 定义后从未被 `cmd/server` 或 E2E harness 调用，
  timers / notification_log 留存清理实际从未运行。现两处均已接入启动对账。接线前先补了配置校验
  （`config.RetentionConfig.Validate`，`config.Load` 与对账入口都会校验）：原先没有任何数值约束，
  `RETENTION_NOTIFICATION_LOG_DAYS=0` 会立即删光承载发送幂等键的 `notification_log`，让 at-least-once 重投变成重复发送；
  `RETENTION_SWEEP_INTERVAL=0` 会让 handler 自续期空转；E2E 以零值结构体字面量构造配置，直接接线会触发同样问题。
  现要求天数 ≥1、间隔 ≥1m、批量 ≥1，非法值启动即失败。
- 验收（2026-09-17）：`go build ./... && go vet ./... && go vet -tags=integration ./...` 零错误；
  `go test -tags=integration ./... -race -count=1` 18 个包全部 ok（含新增 `internal/incident`）；E2E 11 个场景全部通过
  （接入 retention 对账后复跑 config/app/E2E 仍全部通过）；`cd web && npm run build && npx oxlint` 通过。

### [124a938~eceb03e] 2026-08-22 ~ 08-28 — 通知发送移出事务 + 移除 E2B 产品面

- **scheduler 第二种执行模式（`0965eb4`）**：新增 `DetachedHandler` / `RegisterDetachedHandler`。
  编排为短事务 `SELECT … FOR UPDATE SKIP LOCKED` 领取 → 置 `claimed` 并写租约 → 提交释放行锁
  → 事务外**同步**执行（占住 worker 槽位，禁止 goroutine）→ 另开短事务按 `claim_count`
  fencing token 回写 fired/failed。`claimed` 不是终态，租约过期可被任意 worker 重领，
  所以崩溃不丢通知，代价是 at-least-once。迁移 000024。
- **`notify_send` 切换到事务外模式（`a394a31`）**：慢渠道/超时渠道不再把事务和 timers 行锁
  拉到分钟级。Sender 的占位写入、幂等键（含持久化 Round）、失败回写与时间线口径逐字节不变；
  其余十个 kind 保持事务内模式。**这是不变量 3 唯一的例外分支。**
- **回滚修复（`629d326`）**：同一 `(kind, ref_id)` 可以合法并存多条 `claimed`，000024 down
  一次性改回 pending 会撞 `timers_pending_kind_ref_uidx`；改为每个元组至多归还一条 pending
  （按 `created_at DESC, id DESC`），其余标 failed。
- **优雅退出修复（`124a938`）**：SIGTERM 取消 ctx 后，不再把已领取的发送 settle 成 failed
  （那会永久丢通知），保持 `claimed` 待租约过期重领；成功路径仍脱离取消回写 fired。
- 注释口径统一（`cbae6d4`）：幂等键只收敛"逻辑性重复调度"，不兜"已发出未确认即崩溃"的重发窗口。
- **移除 E2B demo 产品面（`eceb03e`）**：删除 `internal/e2b` 回放 fixture、关联评估器、UAT
  Runbook 执行器、`seed-e2b-demo` 命令、黄金场景 E2E 与相关文档/UI；迁移 000025 清理
  `e2b.%` Runbook 行。Runbook 服务收敛为通用 `DisabledExecutor` 装配（不变量 12）。
- 验收：合入后首次线上运行发生在 2026-09-12（uat，INC-000001 宣布通报）：
  `notify_send` `claim_count=1`、`fired`，`lark_webhook` 发送成功。

### [3140817~4d0efa8] 2026-08-04 ~ 08-13 — 部署形态修正、品牌与飞书卡片网关

- **Web 运行时 API 地址（`2511458`）**：Web 镜像不再写死 API 地址、不再由容器内 Nginx 代理 API；
  入口脚本把 `API_BASE_URL` 注入浏览器 `runtime-config.js`，缺失或非法时 fail fast。同源分流交给
  集群 Ingress/Gateway；跨 origin 直连只能经 `CORS_ALLOWED_ORIGINS` 精确白名单（`internal/api/cors.go`）。
  **这推翻了 v0.1.0 中"同源 nginx 代理"的部署描述。**
- **内网镜像构建（`2511458`）**：Go/Alpine/Node 基础镜像、GOPROXY/GOSUMDB、NPM registry 均由
  Makefile 构建参数显式注入，禁止隐式访问 Docker Hub/`proxy.golang.org`；默认平台 `linux/amd64`；
  三个镜像各有可单独重试的 push target。
- **Firebreak 品牌与移动端（`3140817`）**：品牌接入，窄屏导航抽屉与布局适配；前后对比截图归档于
  `docs/audits/2026-08-04-mobile/`。
- **飞书事件卡片动作网关（`fc280c0`）**：卡片按钮回调复用 REST 处置语义（不变量 6），Incident
  状态推进经 `SetLarkIncidentTransitioner` 进入同一 Service；迁移 000023 `lark_card_actions`
  做动作幂等与审计（业务事务内先占位、失败随事务回滚，飞书重试可安全重做），并给
  `users.lark_open_id` 加唯一索引。设计见 `docs/plans/2026-08-13-feishu-card-action-design.md`。
- E2E 环境隔离并支持飞书用户绑定（`4d0efa8`）。

### [14b55ee~22edc53] 2026-08-02 ~ 08-03 — 定位反转：Infrastructure-first 事故响应

**这是项目的第二个时代。** 此前系统是自建的 Alertmanager 式控制面；此后 Alertmanager 成为
Alert 生命周期、分组、路由、静默、抑制与 Alert 级通知的唯一权威，SRE Control 只负责
Entity / Fault / Impact / Incident。未立 spec，决策记录在 `docs/adr/0001~0005` 与
`docs/plans/2026-08-01-infrastructure-first-fault-incident-design.md`。

- **设计定稿（`14b55ee`、`22edc53`）**：ADR 0001 事故优先模型、0003 Alertmanager 权威 + 分层实体、
  0004 显式服务目录与确定性告警映射、0005 统一实体关系与保守爆炸半径。CLAUDE.md 不变量 11~21 由此落笔。
- **事故响应核心（`4ce72ad`）**：迁移 000012~000017——基础设施 Fault、Incident 响应、受控 Runbook、
  Fault/Impact 人工裁决、多 Fault Incident 生命周期。Incident 必须人工宣布，受 mitigation /
  resolution 双重门槛约束（不变量 11、18）。
- **Alertmanager 权威接入（`6b5ed39`）**：迁移 000018~000022。Alert 拆为 `source_state` /
  `response_state` / `suppression_state` 三态；webhook 实时入队 + `/api/v2/alerts` 周期对账
  （`internal/alertsource`）；occurrence 幂等键 `source_id + upstream_fingerprint + starts_at`；
  唯一入口 `POST /ingest/v1/hook/{hook_uuid}`。管道对权威来源只执行 dedup + mapping（不变量 1、14）。
- **服务影响映射（`6b5ed39`）**：`internal/mapping` 确定性 Alert → Entity → Fault 投影，只有命中
  AlertMappingPolicy 或携带受信标准标签才建 Fault（不变量 17）；`internal/impact` 反向依赖图遍历，
  图可达只产生 `potential`，人工裁决不被自动重算覆盖（不变量 16）。
- **中文事故优先工作台（`5c0c204`、`c42f7c2`）**：运行总览、故障、事故、告警证据为一级入口，
  配置收敛到控制中心（不变量 19）；UI 审计证据归档。
- 验收：迁移 000012~000020 的能力在 2026-09-12 之前**线上零使用**（faults/incidents/policies 均为 0）；
  2026-09-12 在 uat 首次端到端跑通 Alert → Entity(别名收敛) → Fault → Impact → Incident，
  不变量 16/17/18 在真实数据上验证通过。

## [v0.1.0] 2026-08-02 — 首个可部署 MVP 基线

> 注：本节"Web+API 同 Pod、同源 nginx 代理"已被 `2511458` 推翻，现行部署形态见上文。

- Alert Source 接入升级为 managed hook：SRE Control 分发
  `/ingest/v1/hook/{hook_uuid}`，URL 独立完成来源识别与鉴权；hook 仅存哈希、仅显示
  一次并支持轮换，source_id 与历史关联保持稳定。旧 source_id + Bearer 路径已删除。
- 容器构建统一使用 Docker Buildx：本地镜像显式 `--load`，仓库发布通过 `--push`
  生成可配置的多架构 manifest list。

- 根 `Dockerfile` 提供 `api`、`web`、`migrate` 三个 target；API/Web 均为非 root
  运行时，镜像写入版本与 Git revision 标签，不包含明文凭据。
- Makefile 扩展为本地 CI 和发布入口：`make ci` 复现 GitHub Actions，
  `make release-check` 加做镜像 smoke test 与 Kubernetes 校验。
- Kubernetes 采用 Web+API 同 Pod、同源 nginx 代理、migrate initContainer；配置
  startup/liveness/readiness probes、资源边界、只读根文件系统和 restricted Pod 安全项。
- `overlays/local` 提供 PostgreSQL StatefulSet、PVC 和开发 Secret；production overlay
  只保留镜像与应用基线，真实 Secret/数据库由部署环境负责。
- 管理员初始化改为显式 Job，不随每次滚动发布自动重置密码。
- 设计与验收边界见 `docs/plans/2026-08-02-mvp-container-kubernetes-deployment-design.md`。

## 接手须知（2026-08-01 时点）

- 全部交付已提交：aiops-foundation 与 ui-enrichment 合并在 `621810a`（两 spec 在 state.go/alerts.go/AlertDetail.tsx 等共享文件上深度交织，文件级拆分无法保证各提交点独立构建），实时化在 `06b48e8`，文档收口在其后一提交。
- 最后一次全量验证（2026-08-01）：`go test -tags=integration ./... -race -count=1` → **640 passed / 18 packages**；`go build ./... && go vet ./...` 零错误；`cd web && npm run build && npx oxlint` 绿。
- 本地环境：dev PG 走 `deploy/docker-compose.yml`（宿主机 **5433**，注意 initdb 只在首次建卷时跑迁移，之后新增迁移要手动 psql 应用——已补齐到 000011）；后端 `bin/alert-system` 跑 8080（`DB_DSN` 指 5433，见 `.env.example`）；前端 Vite dev 5173 代理 `/api`→8080。
- store 集成测试必须带 `-tags=integration`，裸跑 `./...` 会静默跳过。

## [06b48e8] 2026-08-01 — 告警页面实时化（SSE + 轮询兜底）

用户裁决直接实施，未立 spec。

- 迁移 000011：alerts（INSERT/UPDATE）与 timeline_entries（INSERT）挂 `pg_notify('alert_changed', 告警ID)` 触发器。**为什么用触发器**：NOTIFY 只在事务提交后投递（满足不变量 4），管道/REST 处置/scheduler 三类写入方零代码全覆盖，多副本部署时各副本自行 LISTEN 即可。
- `internal/events`：进程内 Bus（Publish 非阻塞，慢订阅者丢信号不反压——丢的只是信号，前端有轮询兜底）+ RunListener（独立 pgx 连接 LISTEN，断线 1s→30s 退避重连）。
- `GET /api/v1/events`：SSE 流（仅需登录，25s 心跳，只推 `{"alert_id":...}` 信号不推数据——权限与数据口径仍只有 REST 一份）。装配只在 `cmd/server/main.go`，`app.Build` 与 E2E 零触碰。
- 前端：`web/src/hooks/useAlertEvents.ts`（EventSource + 2s 节流合并 invalidate）挂 AppLayout 全站一条连接；列表/详情 15s 轮询兜底（`refetchIntervalInBackground: false`）；SSE 断线静默降级回轮询。
- 验收：640 passed；真机端到端通过（写备注 → SSE 流即刻收到事件；未登录 401；Vite 代理可穿透）。

## [621810a] 2026-07-31 ~ 08-01 — ui-enrichment（详情页信息密度，6 任务）

对照 WeaponX 生产系统裁剪出的 P1 三项。规格：`.spec-workflow/specs/ui-enrichment/`。

- **噪声画像**：迁移 000010 加 `(fingerprint, starts_at)` 索引；`FingerprintStats`（30 天计数 + 已解决平均时长，无样本返 nil 非 0）与 `ListRecentResolvedByFingerprint`（LATERAL 取最新 resolve entry 的 note/reason；excludeID 空串必须 NULL 绑定——pgx 空串直绑 uuid 报错）；`GET /api/v1/alerts/:id/stats`；详情页独立 query 渲染「30 天 N 次 · 平均持续 X」+ 首发 Tag + 最近已解决记录（失败不阻塞详情主体）。
- **URL 链接化**：`web/src/utils/linkify.tsx` 纯函数拆段渲染，协议白名单由 `https?://` 正则天然保证，禁 dangerouslySetInnerHTML（告警内容是不可信输入）。注意：alerts 表没有独立 description 列，AM/Grafana adapter 把 description/runbook 都塞进 `signal_context.annotations`，链接化接入 annotations 即全覆盖。
- **独立备注**：`domain.AddNote` 纯函数（不是状态转移，不接收 Alert，resolved 后可补记）；`POST /api/v1/alerts/:id/notes`（空/超 1000 字符 422，note 不落日志）；时间线 note entry 用灰色 EditOutlined 区分状态转移。
- 明确不借鉴：WeaponX 的「期望/无效」双状态标记（会污染状态机不变量 6）。

## [621810a] 2026-07-31 — aiops-foundation（确定性降噪，14 任务）

规格：`.spec-workflow/specs/aiops-foundation/`。**路线裁决：只做确定性降噪，embedding 永久排除，LLM 仅预留异步 enrichment 层不实装**（CLAUDE.md 不变量 9/10 由此落笔）。

- 迁移 000008（noise：routes 加 group_mode/storm_threshold_per_min 等）、000009（flap_history）。
- 模板指纹分组（group_mode='template'）、告警风暴汇总（storm_summary timer kind，第十一个）、flapping 抑制（FLAP_WINDOW_SECONDS）、留存清理（retention_sweep）。
- **降噪一律 opt-in**：零值/缺省时全部短路，行为与引入前逐字节一致。

## [5494e67] 2026-07-31 — serviceability（通知到人，17 任务）

- 团队成员/值班/联系方式打通；email 渠道（第一个真正到人的渠道，SMTP 未配时 Send 返回 ErrSMTPNotConfigured 走 markFailed，不是启动期报错）；users.lark_open_id 预留位（迁移 000007）。

## [46bc610] 2026-07-31 — ops-enhancements（14 任务）

- severity 差异化升级节奏（escalation_policies.severity_config，'{}' 表示无覆写存量不变）；团队级运营周期任务（日报/悬置扫描/值班播报，三个 timer kind + 启动对账 ReconcileOpsTimers）；通知结果写时间线（notify_sent/notify_failed）。

## [5f34647~1093144] 2026-07-30 — 交付工程化 + CI 修复

- deploy/ 本地环境、seed-admin 初始管理员引导、GitHub Actions CI、README。
- 两个 CI 坑：store 测试需 `-tags=integration`；package-lock 需 npm@10 生成。

## [4a08be4~d2d4441] 2026-07-29~30 — MVP 主体（31 任务）

- 接入：AM/Grafana/generic 三 adapter，integration token（hex sha256）鉴权。
- 领域：状态机（firing/acked/resolved + append-only 时间线）、指纹去重、matcher。
- 编排：dedup→route→group→silence→inhibit 管道；PG timers 表统一定时器；通知升级链（人员/渠道两级）；outbox 解耦（幂等键含 round 维度——曾因缺 round 导致"终身只发一次"）。
- 曾踩的大坑（详见 CLAUDE.md 不变量 3）：scheduler handler 自开事务与外层行锁形成跨连接死锁，把唯一 worker 永久焊死——handler 一律用传入 tx。
- Web 控制台：告警列表/详情/处置、路由/静默/升级策略/通知策略/integration 全套 CRUD、登录会话（bcrypt + 防枚举）、RBAC。
- 飞书卡片处置回调（验签），与 REST 处置共用 stopper/lifecycle 保证不变量一致。
- E2E：docker-compose 真实 PG 五场景。

## 悬置事项（不阻塞）

- 飞书真实发送与回调对接：HOLD 等统一通知网关（lark_open_id 已预留）。
- alerts.team_id / acked_by 补 FK 迁移。
- web npm audit 2 个 high（发布前处理）。
- store/events 集成测试镜像 postgres:15-alpine 与 prod/e2e 的 16 不一致（存量惯例，建议统一）。
- SSE 心跳 25s 写死常量未接 config；events.Bus 单锁实现（几百连接够用，几千级再分片）。
- AlertList 与 AppLayout 双登出按钮视觉冗余。
