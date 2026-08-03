# Changelog

> 面向接手人的交付记录：按时间倒序，每节列「交付了什么、关键决策、验收状态」。
> 架构不变量与协作约定见 `CLAUDE.md`（改代码前必读）；规格文档见 `.spec-workflow/specs/`。

## [v0.1.0] 2026-08-02 — 首个可部署 MVP 基线

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
