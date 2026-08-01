# alert-system

Go 告警管理系统（MVP）：接入已触发的告警事件（metric/log/trace 归一），经管道处理后按策略通知、升级、自愈预留。架构大量借鉴 Alertmanager / PagerTree 实践。规格与任务由 spec-workflow 管理（`.spec-workflow/specs/alert-system-mvp/`，dashboard 用 5001 端口，5000 被 macOS AirPlay 占用）。交付时间线与接手须知见 `CHANGELOG.md`。

## 技术栈

- 后端：Go 1.25、gin v1.12、pgx v5.7（不用 ORM）、PostgreSQL 16、testcontainers-go（store 集成测试）
- 前端（web/）：Vite + React 19 + TS + AntD **5.29.3（锁 5，不升 6）** + React Query 5 + react-router-dom 7；无 Redux/Mobx；dev proxy `/api`→`:8080`
- 队列/定时器：不引入外部 MQ，统一用 PG `timers` 表（决策：先 PG 队列）

## 目录结构

- `cmd/server/` 薄入口；`internal/app/` 共享装配（wiring），cmd 与 test/e2e 共用，**不要在两处手写重复装配**
- `internal/domain/` 纯函数领域层（状态机、matcher、指纹）；`internal/store/` 仓储（所有方法带 `tx pgx.Tx` 参数，事务由调用方开）
- `internal/pipeline/` 管道 stage；`internal/ingest/` webhook 接入 + AM/Grafana 适配器；`internal/scheduler/` 统一定时器；`internal/oncall/` 值班与人员升级链；`internal/notify/` 通知策略/渠道/生命周期；`internal/api/` HTTP 层
- `migrations/` golang-migrate 格式，编号顺延（当前到 000011）；`test/e2e/` docker-compose 真实 PG 五场景 E2E

## 核心架构不变量（改代码前必读）

1. **管道顺序**：dedup → route → group → silence → inhibit（以 stage 文件内注释与 app 装配为准）。
2. **十一个 timer kind**：`alert_ttl`、`alert_repeat`、`personnel_escalation`、`group_wait`、`channel_escalation`、`notify_send`、`ops_daily_report`、`ops_stale_sweep`、`ops_oncall_broadcast`、`retention_sweep`、`storm_summary`。新增 kind 必须在 internal/app 注册 handler，未注册 kind 到期会被标 failed 不重试。
3. **scheduler 事务模型**：per-timer 独立事务；顺序是先 markFired 再调 handler（同事务），handler 失败整体回滚后另开小事务标 failed。`scheduler.Handler` 签名带 `tx pgx.Tx`，**handler 绝不能自开事务**——自开事务里 UPSERT 同 (kind, ref_id) 会与外层持有的行锁形成 PG 检测不到的跨连接死锁（曾把唯一 worker 永久焊死）；markFired 前置是为了让 handler 续期 INSERT 出新 pending 行而不是被清掉。handler 必须幂等（at-least-once 重投）。
4. **通知发送解耦（outbox）**：四个通知意图接口（oncall.Notifier / pipeline.GroupNotifier / notify.RepeatNotifier / notify.Dispatcher）都在持锁事务内被调，**只准 Schedule 一条 `notify_send` 定时器，禁止同步外部 IO**。幂等键口径：`AlertID或GroupKey|step|channel|target|round` 用 `"\x1e"` 连接——round 是**意图时刻**捕获的时间戳，随 payload 持久化（绝不能发送时现算，否则崩溃重投键会变）；timers 的 refID 保持业务元组**不含 round**（保留 pending 收敛防抖）。没有 round 维度曾导致"终身只发一次"，repeat/静默恢复全部被吞（违反 R6.5）。唯一键兜的是逻辑性重复调度，不兜"HTTP 已发未提交时崩溃"的重发窗口（at-least-once 固有代价，已接受）。
5. **无分组直发路径不存在**：路由 group_by 为空时走单例组（键 `{routeID}-single-{fingerprint}`），复用 group_wait/group_interval 同一编排，对齐 Alertmanager 语义。不要重新引入 directDispatch。
6. **处置幂等口径**：重复 Ack/Resolve 按当前状态短路 200 返当前状态（不 409、不重写时间线）；无论是否真转移都无条件调 Stop/CancelLifecycleTimers（不变量不依赖历史）；`ErrAlertResolved`/`ErrInvalidTransition` → 409。飞书卡片回调与 REST 端点必须保持同一语义。
7. **actor 双参数**：`domain.Ack(alert, actorID, actorName, now)`——actorID 进 `alerts.acked_by`（UUID 列，可空），actorName 进 timeline（TEXT，必填）。别再把用户名喂给 UUID 列。
8. **R10.5**：删除静默必须同一事务先 `DetachSilence`（array_remove alerts.silenced_by）再 Delete；SilenceStage 有 10s 规则缓存的已知自愈缝隙。
9. **AI enrichment 红线**：任何 AI/LLM 调用都是外部 IO，只准发生在异步 enrichment 层（管道外、任何持锁事务外，消费已提交数据）；管道 stage、scheduler handler、四大通知意图接口内禁止出现模型调用。LLM/enrichment 组件不可用时，系统全部既有功能必须 100% 不受影响——AI 是增强不是依赖。告警内容外发模型 API 前必须按既有口径脱敏。
10. **降噪一律 opt-in**：group_mode 默认 'label'、storm_threshold_per_min 默认 0、FLAP_WINDOW_SECONDS 默认 0——零值/缺省时全部降噪逻辑短路，行为与引入前逐字节一致。新增降噪能力必须遵守同一口径。

## 安全约束（必须持续遵守）

- 一切凭据 token 存 `hex(sha256(token))`，明文仅在创建响应里返回一次；webhook 走 integration token 鉴权；飞书回调必须验签（verification_token 非空校验）
- 密钥/token/密码不落日志（config 已做 DSN 密码掩码）；通知渠道内容对敏感字段脱敏；飞书凭据走 config
- 登录：bcrypt + dummy hash 防用户名枚举时序侧信道；session token crypto/rand 32B，cookie 存明文、库存哈希，固定过期 + 滑动续期
- RBAC：admin =「至少一个团队的 admin 角色」；处置端点仅需登录（member 可处置）；`/api/v1/admin/*` 全部 admin only（403）

## 常用命令

```bash
go build ./... && go vet ./...          # 必须零错误（含 test/e2e 包）
go test -tags=integration ./... -race -count=1  # 全量单测（store 测试带 //go:build integration 标签，漏 tag 会静默跳过 store；testcontainers 需 Docker）
cd web && npm run build && npx oxlint  # 前端验证（tsc -b 为准，编辑器诊断常 stale）
cd test/e2e && docker compose up -d    # E2E 依赖的真实 PG（initdb 自动跑 migrations）
```

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
