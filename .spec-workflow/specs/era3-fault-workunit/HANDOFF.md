# 交接说明 — era3-fault-workunit

> 交接时点：2026-09-19，分支 `era3` @ `e09626e`（已推送 origin）。进度 **15 / 45**。
> 接手 Agent 负责实现，原主对话负责后续 review。**开工前完整读完本文件，再读 `tasks.md` 与 `Implementation Logs/`。**
> 本文件记录的是 `requirements.md` / `design.md` / `tasks.md` 之外、在实现过程中做出的裁决与踩过的坑；三份规格与 ADR-0006 仍是权威，冲突时以本文件的"实现裁决"为准（它们晚于规格、已被用户确认）。

---

## 1. 当前状态

### 1.1 分支与发布

| 分支 | 位置 | 说明 |
|---|---|---|
| `main` | `d27e397`（tag `v0.2.0`） | 只包含"已实现并验证"的现实。**era3 的任何改动都还没进 main** |
| `era3` | `e09626e` | 集成分支：ADR-0006、不变量 22–24、spec、以及下表已完成任务的代码 |

合并策略（用户已确认）：计划文件与未完成代码不进 main；**每个阶段完成并全量验证后，era3 连同 CHANGELOG 合入 main**。建议第一个合并点是 P2 完成（任务 21 通过）——P1 已落地的部分在未配置 `CCLOUD_BASE_URL` 时完全不生效，随 P2 一起进 main 无风险。

### 1.2 已完成（15）

| 阶段 | 任务 | 提交 |
|---|---|---|
| P0 | 1、2、3（ADR、方向文档裁决、不变量 22–24） | `01260e1` |
| P3 | 22 权威来源常量集 | `38bf93c` |
| P1 | 4 config、5 ccloud 客户端、6 MapTopology | `da95e0d` |
| P2 | 12 迁移 000026、13 AckFault、14 projector 写 owner + transition | `da95e0d` |
| P1 | 7 迁移 000027、8 跨源边 + 无流量标记 | `e67f377` |
| P2 | 15 FaultRepo 扩展、16 FaultOutbox、17 全入口触发 | `e67f377` |

每个任务的接口签名与**跨任务警告**都在 `Implementation Logs/` 里，开工前必读对应上游任务的记录。

### 1.3 最后一次全量验证（e67f377 合并后）

- `go build ./... && go vet ./... && go vet -tags=integration ./...`：零错误
- `go test -tags=integration ./internal/... -race -count=1`：18 个包全绿
- `test/e2e`：11 个场景全绿
- 迁移 000026、000027：均在全新 PG 上验证过 up → down → up

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
| D8 | 新增配置：`FAULT_NOTIFY_COOLDOWN`（30m）、`FAULT_NOTIFY_FALLBACK_CHANNEL`（`lark_webhook`）、`FAULT_NOTIFY_FALLBACK_TARGET`（凭据，已脱敏）、`WEB_BASE_URL`（拼 `/faults/{id}` 详情链接） | 18、20、44 | 任务 16 |

---

## 3. 建议的下一步

### 3.1 批次顺序

| 批次 | 内容 | 前置 | 备注 |
|---|---|---|---|
| **第 3 批（建议立即）** | P2：18、20、21 ∥ P1：9、10、11 | 无 | **21 通过即 P2 收口，可走第一次 era3 → main 合并** |
| 第 4 批 | P3：23、24、25、26、27 | 无 | 与 P1/P2 文件交集小，可与第 3 批后半并行 |
| 第 5 批 | P5：37、38、39、40 | P2 完成 | 39（readyz）与 40（发送失败兜底）直接对应漏报风险 M2/M3，价值高 |
| 第 6 批 | P4：28–33、35、36 | P1 + P2 完成，**且拿到 `CCLOUD_HEALTH_WINDOW`** | 29 规定该值必填、无默认，只能由平台团队提供 |
| 延后 | 19、34、45 | uat 公网回调入口 | 见 D7 |
| 收尾 | 41、42、43、44 | 各自前置 | 43、44、45 为人工动作，需用户确认后执行 |

### 3.2 并行度与文件边界

- 每条并行线**一次派 2–3 个相邻任务**，做完一批验收合并再派下一批；**同时最多 2 条线**（3 条并行时出现过共享文件冲突）
- 第 3 批两条线的文件边界：
  - P2 线：`internal/api/faults.go`、`internal/api/router.go`、`web/src/pages/Faults/*`、`test/e2e/fault_notify_test.go`
  - P1 线：`internal/ccloud/syncer.go`、`internal/api/topology.go`、`internal/app/app.go`（装配）、`cmd/server/main.go`、`test/e2e/harness.go`、`web/src/pages/Settings/ServiceCatalog.tsx`
  - **冲突点**：`internal/api/router.go`（18 与 10 都要注册路由）。建议 P1 线的任务 10 等 P2 线的 18 合入后再开始，或由主对话合并时手工解决

### 3.3 各待办任务的上游警告（摘自 Implementation Logs）

| 任务 | 必须知道的事 |
|---|---|
| 18 | `store.FaultRepo.Ack` **只写 faults 表、不写时间线**。必须在同一事务内配对：`domain.AckFault` 产出条目 → `FaultRepo.Ack` → `FaultRepo.AppendTimeline`，靠 `AckFault` 返回的 `ok` 判断是否写时间线（不要比较 struct）。重复认领按不变量 6 返回 200 + 当前状态 |
| 9 | 把 `ccloud.Edge` 转为 `domain.DependencyEdge`：`FromEntityID/ToEntityID → FromNodeID/ToNodeID`，`Stale → NoRecentTraffic`；以 `cfg.CCloud.Enabled()` 决定是否启动（D1）；接口路径可配置（D2）；写 `unresolved_identities` 时 `first_seen` 只在首次插入写 |
| 10 | 如需在边查询响应里透出 `no_recent_traffic`，`ListEdgesForNode` 与 `listEntityDependencies` 的 SELECT 目前没有该列；集合字段空时返回 `[]`（不变量 13） |
| 23 | `internal/ingest/server.go` 与 `alertmanager_api.go` 仍只对 `alertmanagerKind` 做"权威来源必须带 fingerprint"校验，Grafana 适配器必须一并覆盖；`alerts.source` 已改为写 `event.Source`，适配器必须设为 `grafana` |
| 33 | 影响外播需**新增方法**，不得复用 `QueueFaultActivated`；GroupKey 用不同后缀（如 `fault:{id}:impact_confirmed`）避免 refID 冲突；目标解析建议抽出 `resolveTeamChannels` 与任务 16 复用 |
| 40 | 区分"真正发送成功"须查 `notification_log`，不能用 `owner_notified_at`（D4） |

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

---

## 6. 等待人工处理的事项（不阻塞编码）

| 事项 | 阻塞 | 负责 |
|---|---|---|
| ccloud 三个接口的真实路径与鉴权方式 | 任务 9 上线（编码不阻塞，D2） | 用户 |
| `CCLOUD_HEALTH_WINDOW`、健康度刷新周期、负责人查询接口 | 任务 29–31 | 平台团队 |
| uat 飞书可回调的公网入口 | 任务 19、34、45 | 用户 |
| uat 部署 v0.2.0（线上仍是 0.1.0；Web 镜像尚未推送） | P2 上线前建议先完成，分开风险 | 用户 |
| 凭据轮换：飞书机器人 webhook token（排查时暴露过）、DB 密码（明文放在名为 `secret` 的 ConfigMap 中） | 安全 | 用户 |
| 本机 `.git/index.lock` 反复残留（已出现 3 次），疑似 root 运行的 `git-ai install-hooks` 进程所致 | 多 worktree 并行时会撞上 | 用户排查 |

---

## 7. 交回 review 时请提供

1. 每批的 `git log --oneline` 与对应任务号
2. 每个任务的 `log-implementation` 已落账、`tasks.md` 已标 `[x]`
3. 阶段收尾的全量测试与 E2E 真实输出
4. 新增迁移的往返验证结果（若有）
5. 偏离规格或本文件裁决之处，以及理由
