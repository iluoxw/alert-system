# 交接说明 — era3-fault-workunit

> 交接时点：2026-09-20（第四次交接）。**P0–P5 全部完成并合入 main，打 tag `v0.3.0`**；收尾任务 41、42 一并完成。进度 **32 / 45**。
> 剩余 13 个任务全部卡在外部输入或人工动作上，见 §3.1 与 §6——**没有可以立即开工的编码任务**。
> 接手 Agent 负责实现，原主对话负责后续 review。**开工前完整读完本文件，再读 `tasks.md` 与 `Implementation Logs/`。**
> 本文件记录的是 `requirements.md` / `design.md` / `tasks.md` 之外、在实现过程中做出的裁决与踩过的坑；三份规格与 ADR-0006 仍是权威，冲突时以本文件的"实现裁决"为准（它们晚于规格、已被用户确认）。

---

## 1. 当前状态

### 1.1 分支与发布

| 分支 | 位置 | 说明 |
|---|---|---|
| `main` | tag `v0.3.0`（P0–P5 全部合入） | 已实现并验证的现实 |
| `era3` | 与 main 同点（已快进合并） | 集成分支；P4 解锁后继续在这里做 |

合并策略（用户已确认）：**每个阶段完成并全量验证后，era3 连同 CHANGELOG 快进合入 main**；合并需用户明确同意。第一次合并（P0–P3）已于 2026-09-19 完成。P1 在未配置 `CCLOUD_BASE_URL` 时完全不生效。

### 1.2 已完成（26）

| 阶段 | 任务 | 提交 |
|---|---|---|
| P0 | 1、2、3（ADR、方向文档裁决、不变量 22–24） | `01260e1` |
| P3 | 22 权威来源常量集 | `38bf93c` |
| P1 | 4 config、5 ccloud 客户端、6 MapTopology | `da95e0d` |
| P2 | 12 迁移 000026、13 AckFault、14 projector 写 owner + transition | `da95e0d` |
| P1 | 7 迁移 000027、8 跨源边 + 无流量标记 | `e67f377` |
| P2 | 15 FaultRepo 扩展、16 FaultOutbox、17 全入口触发 | `e67f377` |
| P1 | 9 syncer、10 待对齐 API/Tab、11 P1 验收闸门 | 第 3/4 批提交（见 `git log`） |
| P2 | 18 REST ack、20 Web 认领、21 P2 E2E | 第 3/4 批提交 |
| P3 | 23 Grafana 适配器、24 对账路径、25 幂等键、26 E2E、27 冻结标注 | 第 3/4 批提交 |
| P5 | 39 readyz 心跳、40 通知失败元告警 | `9fb8166` + review 修复 `638a3fd` |
| P5 | 37 先行指标、38 日报与总览展示 | `89ca0fb` + review 修复 `feedb19` |
| 收尾 | 41 CLAUDE.md 目录、42 CHANGELOG v0.3 | 随 v0.3.0 发布 |

每个任务的接口签名与**跨任务警告**都在 `Implementation Logs/` 里，开工前必读对应上游任务的记录。

### 1.3 最后一次全量验证（P5 收口、review 修复后，即 v0.3.0）

- `go build ./... && go vet ./... && go vet -tags=integration ./...`：零错误
- `go test -tags=integration ./internal/... -race -count=1`：19 个包全绿（store 约 290s，ops 约 37s）
- `test/e2e`：全绿（约 65s，含 Fault 通知 + Grafana 权威链；本地编排场景已改走 generic）
- `cd web && npm run build && npx oxlint`：通过
- P5 无新迁移；000028 仍预留给任务 28

### 1.4 迁移编号

| 编号 | 状态 |
|---|---|
| 000026 | 已用（任务 12） |
| 000027 | 已用（任务 7） |
| 000028 | **预留给任务 28**，其他任务不得占用 |

---

## 2. 实现过程中的裁决（规格文本未完全体现）

| # | 裁决 | 影响的任务 | 依据 |
|---|---|---|---|
| D1 | **ccloud 启用开关 = `CCLOUD_BASE_URL` 是否配置**。`CCloudConfig.Enabled() = BaseURL 非空 && SyncInterval > 0`；未配 BaseURL 视为未接入、零影响；配了 BaseURL 却缺 PROJECT_IDS 在 `config.Load` 即失败 | 9、29、30 | 原规格"默认 10m"与"缺 BASE_URL 启动报错"互相矛盾。已写回 tasks.md 任务 4 |
| D2 | **ccloud 三个接口路径是占位符**（`/openapi/v1/topology/fulllink`、`/openapi/v1/topology/service`、`/openapi/v1/health`），用户裁决先 mock 不阻塞。任务 9 装配时**必须让路径可经配置覆盖**，拿到真实接口后只改配置 | 9、30 | 已写入 tasks.md 任务 9 |
| D3 | 平台 `totalCount=0` 的边落 `dependency_edges.no_recent_traffic`，遍历时按 stale 处理；**不得**通过改早 `observed_at` 伪造过期 | 9、10 | 已写入 tasks.md 任务 7 |
| D4 | `faults.owner_notified_at` 语义是"**通知意图已调度**"，不是"发送成功"（R5.6 原文措辞不同）。拓扑抑制需要在意图时刻看到上游已通知 | 40 | `internal/app/fault_outbox.go` 顶部注释 |
| D5 | 冷却、拓扑抑制、无通知目标三种"不发通知"都**必须写 Fault 时间线**，不能静默 | 33 及以后任何新的跳过分支 | 主对话验收修正 |
| D6 | reprocess 端点（人工重新映射）**不触发** Fault 通知（治理操作）。新增任何 `Projector.Project` 调用方都必须接入 `FaultOutbox.TriggerFromMappingResult` 钩子，否则等同漏报 | 任何新增 Project 调用方 | R5.10 |
| D7 | **飞书交互卡片整体延后**：uat 暂无飞书可回调的公网入口。P2 通知走 `lark_webhook` 纯文本 + 详情链接，认领在 Web 完成。任务 19 已移到顺延区；任务 34 同样依赖卡片回调，已标延后；R4.6、R5.7、R5.9、R9.3 已标延后 | 19、34、45 | 用户裁决 |
| D9 | **ccloud 环境由 `CCLOUD_ENVIRONMENT` 显式配置**（启用时必填，缺失 `config.Load` 失败）。平台响应没有环境字段，review 发现 syncer 曾把项目 ID 当作 environment 写入实体 | 9、29、30 | 第 3/4 批 review 修正 |
| D10 | **对账快照里首次出现的 occurrence 必须投影并触发 Fault 通知**（与 unknown→firing 恢复、快照缺失同等）。webhook 丢失/延迟时对账是唯一入口，review 前该路径不投影，等同漏报 | 任何修改 Reconciler 的任务、39 | 第 3/4 批 review 修正，不变量 14 |
| D11 | `unresolved_identities` 在别名补齐后**不删除行**；任务 37 的"待对齐数"指标必须只统计当前仍未解析的（如 `last_seen` 不早于最近一次成功同步），不能直接 count(*) | 37 | 第 3/4 批已知缺口 |
| D12 | **kind=grafana 集成改为权威来源是行为变更**：必须带 fingerprint、跳过本地 route/group/silence/inhibit 与 Alert 级通知。uat 当前无 grafana 集成；合入 main 时 CHANGELOG 须注明 | 26、合入 main | 不变量 24 |
| D8 | 新增配置：`FAULT_NOTIFY_COOLDOWN`（30m）、`FAULT_NOTIFY_FALLBACK_CHANNEL`（`lark_webhook`）、`FAULT_NOTIFY_FALLBACK_TARGET`（凭据，已脱敏）、`WEB_BASE_URL`（拼 `/faults/{id}` 详情链接） | 18、20、44 | 任务 16 |

---

## 3. 建议的下一步

### 3.1 批次顺序

| 批次 | 内容 | 前置 | 备注 |
|---|---|---|---|
| 第 3、4 批 | P2 18/20/21、P1 9–11、P3 23–27 | — | 已完成并合入 main |
| 第 5 批 | P5：39、40、37、38 | — | 已完成、review 修复并随 v0.3.0 合入 main |
| **第 6 批（阻塞中）** | P4：28–33、35、36 | **必须先拿到 `CCLOUD_HEALTH_WINDOW` 与三个接口的真实路径** | 迁移用 000028。拿不到这两样就无法开工，不要先写占位实现 |
| 延后 | 19、34、45 | uat 公网回调入口 | 见 D7 |
| 人工 | 43（k8s 探针切 /readyz）、44（uat 配保底通知目标） | 部署窗口 | 44 必须在 uat 上线 v0.3.0 前完成 |
| 第 6 批 | P4：28–33、35、36 | P1 + P2 完成，**且拿到 `CCLOUD_HEALTH_WINDOW`** | 29 规定该值必填、无默认，只能由平台团队提供 |
| 延后 | 19、34、45 | uat 公网回调入口 | 见 D7 |
| 收尾 | 41、42、43、44 | 各自前置 | 43、44、45 为人工动作，需用户确认后执行 |

### 3.2 并行度与文件边界

- 每条并行线**一次派 2–3 个相邻任务**，做完一批验收合并再派下一批；**同时最多 2 条线**（3 条并行时出现过共享文件冲突）
- A 线已完成，占用的文件已全部合入 era3，B 线与其无重叠。
- B 线（37→38）的文件边界（**tasks.md 里的路径有两处与仓库现状不符，以此处为准**）：
  - 新建 `internal/ops/indicators.go` + `internal/ops/indicators_test.go`
  - 端点挂载：`internal/api/ops_settings.go`（**没有 `ops.go`**，沿用该文件的 Register 模式）或新建 `internal/api/ops_indicators.go`；装配在 `cmd/server/main.go` 与 `test/e2e/harness.go` **两处同步**
  - 日报：`internal/ops/report.go`（`DailyStats` 结构体 + `BuildDailyReport`，`report.go:14` / `report.go:71`）
  - 前端：`web/src/pages/OperationsOverview.tsx`（**不是 `Overview/Overview.tsx`，仓库里没有 Overview 目录**）、`web/src/pages/operationsHelpers.ts`、`web/src/api/client.ts`

### 3.3 各待办任务的上游警告（摘自 Implementation Logs）

| 任务 | 必须知道的事 |
|---|---|
| 37 | 见 §3.4 的逐指标口径。**五个指标里有四个的口径都不是字面可推的**，动手前先读 §3.4；"恢复提醒拦截数"口径存疑，**必须先问主对话再实现** |
| 38 | 总览只加卡片，不新增一级导航或页面（不变量 19）；日报追加小节要有空数据文案。`DailyStats` 已有 `MTTAAvgValid` / `Prev7DayAvgNew` 这类"无数据"处理先例（`report.go:14`），新指标照抄该模式，不要用 0 冒充"无数据" |
| 39 | 现有 `RegisterHealthRoutes` 注入 ready func（`internal/api/directory.go:104`），扩展它而不是另起路由。scheduler（`scheduler.go:534` Run）与 pipeline（`pipeline.go:187` Run）在**每次轮询成功**后用 `atomic.Int64` 记录时间戳，**空轮询（没取到任务）也算成功**，否则低峰期会误报 503。只看进程内状态、不查外部系统：ccloud、Alertmanager 不可用都不得导致 503。503 响应体不得含 DSN 或错误原文（现有测试 `directory_test.go:178` 已在查）。任务 43 的 k8s 探针切换是人工动作，不在本任务内 |
| 40 | `notify_send` 是事务外执行的 DetachedHandler（不变量 3），最终失败在 `Sender.markFailed`（`sender.go:286`）。建议：在写 failed 的同一短事务里 **Schedule 一条新的 `notify_send`** 发给 `FAULT_NOTIFY_FALLBACK_TARGET`，payload 带 `meta_alert=true`；元告警自身最终失败时**只记日志与时间线、不再 Schedule**，从根上杜绝递归。不要在 markFailed 里同步发 HTTP（绕过幂等与租约）。幂等键沿用不变量 4 口径，round 取原始意图的 round，保证原通知重投时不会重复发元告警。fallback target 为空时写时间线说明"无兜底目标"，不能静默（D5）。元告警内容不得包含原始目标 URL（不变量 13） |
| 18 | `store.FaultRepo.Ack` **只写 faults 表、不写时间线**。必须在同一事务内配对：`domain.AckFault` 产出条目 → `FaultRepo.Ack` → `FaultRepo.AppendTimeline`，靠 `AckFault` 返回的 `ok` 判断是否写时间线（不要比较 struct）。重复认领按不变量 6 返回 200 + 当前状态 |
| 9 | 把 `ccloud.Edge` 转为 `domain.DependencyEdge`：`FromEntityID/ToEntityID → FromNodeID/ToNodeID`，`Stale → NoRecentTraffic`；以 `cfg.CCloud.Enabled()` 决定是否启动（D1）；接口路径可配置（D2）；写 `unresolved_identities` 时 `first_seen` 只在首次插入写 |
| 10 | 如需在边查询响应里透出 `no_recent_traffic`，`ListEdgesForNode` 与 `listEntityDependencies` 的 SELECT 目前没有该列；集合字段空时返回 `[]`（不变量 13） |
| 23 | `internal/ingest/server.go` 与 `alertmanager_api.go` 仍只对 `alertmanagerKind` 做"权威来源必须带 fingerprint"校验，Grafana 适配器必须一并覆盖；`alerts.source` 已改为写 `event.Source`，适配器必须设为 `grafana` |
| 33 | 影响外播需**新增方法**，不得复用 `QueueFaultActivated`；GroupKey 用不同后缀（如 `fault:{id}:impact_confirmed`）避免 refID 冲突；目标解析建议抽出 `resolveTeamChannels` 与任务 16 复用 |
| 40 | 区分"真正发送成功"须查 `notification_log`，不能用 `owner_notified_at`（D4） |

---

### 3.4 任务 37 的逐指标口径（已实现，作为口径参考保留）

五个指标的名字都很短，但口径大多不是字面意思。**按下表实现，有疑问先问，不要自行推断**：

| 指标 | 口径 | 坑 |
|---|---|---|
| firing 告警映射率 | 分母：`source_state='firing'` 的 Alert 数；分子：其中已命中 AlertMappingPolicy 或受信任标准标签、已关联 Fault 的数量 | 分母为 0 时返回 **null**（前端显示"暂无数据"），**不得返回 0 或 NaN**。不变量 17：未映射告警进待治理队列，这个比率就是它的可见化 |
| 无 owner 的 active Fault 数 | `faults` 中 `status='active' AND owner_team_id IS NULL` | 有现成索引 `faults_status_owner_team_idx`（000026），照用 |
| Fault 认领时长中位数 | `acked_at - first_detected_at` 的中位数，只取 `acked_at IS NOT NULL` 的 Fault（**本行原写 `created_at` 是主对话写错**，R10.1 的口径是 `first_detected_at`，实现已按后者落地） | 列在 000026：`acked_at`、`acked_by`、`acked_by_name`。用 `percentile_cont` 在 SQL 层算，别捞回 Go 里排序。无样本时返回 null |
| 恢复提醒发出次数 | 用户裁决为"已发出的 nudge 次数"：`timers` 中 `kind='incident_recovery_nudge' AND status='fired'`，**且 `fire_at` 落在显式窗口内**（日报=昨日，端点=近 24 小时） | 不变量 11：nudge **永不推进状态**，不存在"拦截了一次自动关闭"这种事件。窗口不可省：`timers` 的 fired 行会被 retention 按天数清理，全表计数先涨后落、含义随保留配置变化（review 修正） |
| 未对齐身份数 | 只统计**当前仍未对齐**的：`unresolved_identities` 的行在别名补齐后**不会被删除**（D11），必须按 `last_seen >= 最近一次成功同步的 observed_at` 过滤——syncer 每轮只 UPSERT 仍未解析的身份，已解析的自然停止刷新 | 直接 `count(*)` 会把历史上所有对齐过的身份全算进去，指标只增不减。最近一次成功同步取 `topology_sync_runs` 中 `source='ccloud' AND status='completed'` 的最新 `observed_at`；**没有任何成功同步记录时返回 null**，不是 0（uat 当前没开 ccloud，这是常态） |

其他约束：端点 admin only（`/api/v1/ops/indicators`）；集合字段即使为空也返回 `[]`（不变量 13）；聚合全部在 SQL 层完成；不得为指标新建表或物化视图。

### 3.5 A 线遗留的两个观察（不阻塞，B 线不必处理）

1. **pipeline 的心跳记在 Dequeue 成功处**，所以"每个批次都失败"时 `/readyz` 仍是绿的。这是有意取舍：真返回 503 会让 pod 被摘出服务、告警 webhook 直接收不进来，比继续跑更糟。代价是 readyz 只能发现"停摆"，发现不了"一直失败"——**后者正是任务 37 指标要补的盲区**。
2. **任务 43 切探针时**：HTTP 服务比 worker 早几毫秒起来，首轮轮询前 `/readyz` 返回 503。k8s 默认 `initialDelaySeconds` 足以覆盖，不用改代码。

---

## 4. 工作协议（按 CLAUDE.md 协作约定）

1. 任务状态：派发前在 `tasks.md` 标 `[-]`，验收通过标 `[x]`；**只在 era3 上由主对话/接手 Agent 修改**，子代理禁改 `.spec-workflow/`
2. 实现分支/worktree **必须从 era3 拉**，否则读不到不变量 22–24。本次两批都出现过 worktree 基线停在 `d27e397` 的情况，子代理需先 `git log --oneline -1` 核对，干净时 `git reset --hard era3` 对齐
3. 每个任务完成后 `log-implementation`：artifacts 写可 grep 的接口签名，summary 写跨任务警告
4. 每阶段收尾必跑：`go build ./... && go vet ./... && go vet -tags=integration ./...`、`go test -tags=integration ./... -race -count=1`、`cd web && npm run build && npx oxlint`
5. `test/e2e` 固定端口，**任何时刻只能运行一轮**；跑之前 `docker ps --filter name=firebreak-e2e` 确认无残留
6. 新增迁移必须在全新 PG 上实测 up → down → up（方法：`docker run -d postgres:16` + 按文件名顺序 `psql -f` 全部 up，再执行目标 down/up，用完删容器）

---

## 5. 验收经验：子代理常见的问题（review 时重点查）

| 问题 | 实例 | 怎么查 |
|---|---|---|
| **新增字段只在领域对象里赋值，没有真正落库/读回** | 任务 14 的 `owner_team_id`：projector 填了、`FaultRepo.Upsert` 没写、`faultColumns` 没扩展，单测只测纯函数所以全绿 | 凡新增持久化字段，必须有 `//go:build integration` 的"写入 → 读回"测试 |
| **测试没跑完就交报告** | 第一批 P2 在 store 测试未完成时交回，store/pipeline 结果与迁移往返均由主对话补跑 | 派发时写明"必须等全部结果再汇报"，验收时核对报告里有每个包的 `ok` 行 |
| **规格互相矛盾时选择"推迟"而不是解决** | 任务 4 把启动期校验推迟到任务 9 | 看到"故意不在 X 里调用"一类注释时，追问是否把问题留给了下游 |
| **"不发通知"的分支静默返回** | 任务 16 冷却分支原先不写时间线 | 所有 early return 检查是否留有可追溯记录 |
| 并发与去重 | pipeline 与对账可能同时触发同一 Fault | 靠 `notify_send` 的 refID 不含 round 收敛，任务 16 已有测试；任务 21 E2E 需再断言"恰好一条" |
| 并行线合并后的交汇 | 任务 16 的拓扑抑制调用了任务 8 刚改的遍历查询 | 合并后必须跑一次全量，不能只信各自的测试 |
| **测试把"当前实现"当成预期写死** | 任务 24 的对账测试断言"快照首次出现不触发"，而正确行为是必须触发（漏报）；任务 40 的全部测试都把 fallback 目标设成与原目标不同的值，恰好绕开了唯一会出事的那条路径 | 看测试时先问"这个断言是从需求推出来的，还是从实现读出来的"；**边界值要专门挑与生产配置一致的那个**（如 fallback 目标 == 原目标） |

---

## 6. 等待人工处理的事项（不阻塞编码）

| 事项 | 阻塞 | 负责 |
|---|---|---|
| ccloud 三个接口的真实路径与鉴权方式 | 任务 9 上线（编码不阻塞，D2） | 用户 |
| `CCLOUD_HEALTH_WINDOW`、健康度刷新周期、负责人查询接口 | 任务 29–31 | 平台团队 |
| uat 飞书可回调的公网入口 | 任务 19、34、45 | 用户 |
| uat 部署 main（`63efc10`，含迁移 000026/000027）；需同时推送 Web 镜像 | Fault 通知在真实数据上验证 | 用户 |
| **任务 44：uat 配置 `FAULT_NOTIFY_FALLBACK_TARGET` 与 `WEB_BASE_URL`** | 部署 main 前必须完成，否则无 owner 渠道的 Fault 只写时间线、无人收到 | 用户 |
| ccloud `CCLOUD_ENVIRONMENT` 的取值（D9） | 任务 9 上线 | 用户 / 平台团队 |
| 凭据轮换：飞书机器人 webhook token（排查时暴露过）、DB 密码（明文放在名为 `secret` 的 ConfigMap 中） | 安全 | 用户 |
| 本机 `.git/index.lock` 反复残留（已出现 4 次，最近一次 2026-09-19 23:28），疑似 root 运行的 `git-ai install-hooks` 进程所致 | 多 worktree 并行时会撞上 | 用户排查 |

---

## 7. 交回 review 时请提供

1. 每批的 `git log --oneline` 与对应任务号
2. 每个任务的 `log-implementation` 已落账、`tasks.md` 已标 `[x]`
3. 阶段收尾的全量测试与 E2E 真实输出
4. 新增迁移的往返验证结果（若有）
5. 偏离规格或本文件裁决之处，以及理由
