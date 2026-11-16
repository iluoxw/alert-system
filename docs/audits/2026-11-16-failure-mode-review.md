# 失败模式复核与优化建议

本次建议先修复通知送达语义、存量 Fault 补通知和调度隔离，再完善影响证据。原 review 指出了真实风险，但「没有自动 confirmed」「探针始终绿」「旧半径查询已造成页面分叉」不能作为已成立事实。

## 审查基线与限制

- 日期：2026-11-16。
- 当前分支：`codex/issue-9-agent-delivery`。
- HEAD：`63778bf6be92b94e6a85984eed91d06414a8c96f`。
- main / era3：`6539b152b97b7749e498a2ea86c323e70b85fef9`。
- 本次通过 `git ls-remote` 核对上述远程分支，均与本地对应提交一致。分支差异为 4 个提交，34 个文件，3331 行新增、26 行删除。
- 审查开始时工作区干净。本次新增审查文档及本地学习记录，未修改产品代码、部署和数据库。
- 未连接当前 UAT / PRO。4/28 映射率、未配置 fallback、Incident 停留 4 天都是历史快照，不能作为 11 月 16 日的运行现状。仓库 `replicas: 1` 也不等同于线上实际副本数。
- 以下路径及行号均针对上述 HEAD。建议是待实施方案，不是已交付能力。

## 对原 review 的校正

| 原判断 | 当前证据 | 结论 |
|---|---|---|
| 没有自动 confirmed，人工是唯一入口 | `internal/impact/projector.go:56` 已把非 stale 路径上的应用 direct evidence 判为 confirmed；`internal/store/faults.go:206` 查询时间窗重叠的应用 direct_impact 告警 | 不成立。缺的是健康采样、持续窗口、自伤消歧等 P4 能力，不是所有自动确认。该实现也已存在于 main 基线 |
| 慢发送期间探针一直绿 | `internal/api/directory.go:169` 在心跳超过 3 倍 poll interval 后返回未就绪；仓库 poll 默认 5 秒 | 只有默认 15 秒内的检测窗口；长发送可能使 readyz 变红。问题是发送进度、worker 存活和 API 接入可用性被混在一起 |
| 单 worker 同步外部 IO | `cmd/server/main.go:235` 只启动一个 scheduler Run；`internal/scheduler/scheduler.go:489` 同步调用事务外发送 | 成立。释放行锁不等于隔离慢任务；不应等健康证据完成后才修 |
| 两种半径算法造成页面分叉 | `ListBlastRadius` 全仓调用点只有定义和测试；影响计算调用 `ListBlastRadiusByNodeID` | 存在遗留 API，但本次没有发现页面使用旧查询的生产调用链，不能宣称已经出现两种页面结果 |
| Agent 是可选旁路 | 未配置不启动；ACK 与领域状态分离 | 成立，但启用后 `internal/ingest/server.go:102` 在原入库事务中调用批次写入，写入错误会回滚整个 webhook 批次，不能外推为完整故障隔离 |
| 告警假解除会立即触发 Fault resolved 冷却 | Alert 解除与 Fault resolved 是两个生命周期；映射中的 resolved 输入进入 monitoring | 冷却确实可能抑制新通知，但复现必须先满足旧 Fault 已 resolved，不能省掉人工收口或下线等状态转换 |

`000028` 为空只能证明那组迁移尚未落地，不能证明已有代码不具备任何直接证据确认能力。`test/e2e/service_mapping_test.go:96` 还有应用告警使 provider impact 变 confirmed、解除后退回 potential 的场景；本次阅读了该场景，没有重新执行整套 E2E。

## 优先处理的失效链

### 1. 通知入队被用作送达依据

`internal/app/fault_outbox.go:173` 在 Schedule 后立即写 `owner_notified_at`；`:201` 只要上游 active Fault 有这个时间，就抑制下游通知。`internal/notify/sender.go:306` 虽有最终失败处理，`internal/app/outbox.go:432` 也实现了 fallback 元告警，但二者没有撤销这个时间戳，也没有重新评估先前被抑制的下游 Fault。

可达失败序列：上游入队 → 下游被抑制 → 上游发送最终失败 → fallback 未配置或同样不可达 → 上下游都没有有效送达。fallback 存在会降低风险，但不修复状态语义。

建议把通知意图、渠道接受成功和人确认拆开。字段或记录至少能区分 queued / sending / sent / failed；`sent` 仅表示渠道接口接受，不代表人已读。成功回写要能关联 Fault，不能只留在 notification_log。

若希望等待上游通知来减少轰炸，应给下游保存「暂缓原因、上游任务、到期时间」，到期或上游失败后重新决定投递。不要仅把 `owner_notified_at` 的写入挪到成功回调：那会重新打开并发批次中的重复通知窗口。只有目标团队/渠道确实被上游通知覆盖时，才有充分理由抑制下游 owner 通知。

验收：上游失败且 fallback 不可达时，下游在设定的暂缓上限后进入投递；多渠道部分成功可解释；并发入队不风暴；进程崩溃后暂缓状态仍可恢复。上限由响应目标确定，不在本次猜默认值。

### 2. 补映射和补渠道之后仍可能无人收到

`internal/api/service_catalog.go:477` 明确规定 reprocess 不调用 Fault 通知。`internal/app/fault_outbox.go:185` 又只响应 CreatedActive / EnteredActive；已处于 active 的后续重算不补通知。因此，补规则并回放后即便出现 active Fault，也不能假设下一次 webhook 或对账一定补发。

同样，无通知目标时只写时间线；冷却抑制后也没有在本入口安排到期重新评估。对持续 active 的 Fault，这些都是需要显式补偿的情况。

建议保留静默回放能力，新增独立、可预览的补通知动作和限速对账：扫描仍 active 且缺少有效投递覆盖的 Fault，结合认领、人工静默、暂缓和历史失败状态决定是否补发。幂等标识应稳定绑定 Fault 激活批次与通知目标，不能每轮扫描都生成新 round。

映射治理应按原因区分 no_policy、alias_not_found、alias_label_missing、conflict、ignored；按来源、严重度、规则类型统计当前 firing occurrence 的覆盖情况，不用历史 4/28 作为今天的分母。修复建议需要显示受影响实例、预估 Fault 数量、通知目标和会发生的合并。

验收：已恢复历史告警回放不通知；仍 firing 的首次有效映射可补发一次；同批重放不重复；渠道补齐后未送达的 active Fault 可补发；ignored 与 unmapped 不混淆。

### 3. 慢渠道占住全部定时工作

建议继续使用 PostgreSQL timers，按任务类型拆执行容量：普通定时任务的 worker 不领取 notify_send；通知 worker 只领取 notify_send，并使用有限并发、独立超时和渠道限流。领取必须在拿到执行槽位后进行，避免任务先被 claim 再长时间排在内存队列中。保留现有 claim、租约、fencing、幂等和取消恢复约束。

| 方案 | 收益 | 局限与选择 |
|---|---|---|
| 多启动几个共享 Run | 改动小，短期提高吞吐 | 所有 worker 仍可同时被慢发送占满，不作为最终隔离方案 |
| 按 kind 划分 worker 池 | 保留现有 PG 队列，给定时任务保留执行容量 | 需要调整领取过滤、按池监控和优雅退出；推荐 |
| 引入外部消息队列 | 可独立扩展消费者 | 当前证据不足以证明收益覆盖运维和一致性成本，暂不推荐 |

健康检查分开记录 poll 进度、执行开始/完成、当前任务运行时长；同时监控各类到期任务最老等待时间、失败数、重领数和通知成功率。扩成多 worker 后，一个线程刷新共享心跳不能证明所有 worker 健康。

单纯把心跳移到发送结束不能消除队头阻塞。慢渠道不应直接导致仍能可靠持久化 webhook 的 API 实例退出接入流量；数据库不可写、真正的接入能力故障仍应影响 readiness。后台停摆用独立状态和外部监测暴露，避免把业务积压掩盖成健康。

验收：通知 worker 全部被慢渠道占用时，TTL、升级、恢复提醒仍在预先约定的误差内执行；停止进程后在途任务可以重领；旧 fencing token 不能覆盖新结果；重复发送的 at-least-once 边界保持明确。

### 4. 修正影响语义和宣布门槛

`internal/store/faults.go:395` 的 HasConfirmedBusinessImpact 缺少 `n.lifecycle_state='active'`。下线流程保留人工裁决，所以仍 active 的 provider Fault 可能拿已下线应用的历史 confirmed 通过宣布门槛。建议统一宣布、列表、总览对有效业务影响的判定，并覆盖「最后一个 confirmed 应用下线」「多个影响里仍有 active 应用」「已存在 Incident 不被下线自动关闭」等用例。

现有自动确认是「依赖可达 + 应用自身有直接退化证据」。这能证明应用退化并提供关联候选，不能单凭时间重叠证明平台是根因。当前节点证据查询还使用 `last_seen_at >= fault.FirstDetected - 5 minutes`，这不是完整的健康采样和自伤消歧机制。

建议先区分「业务退化已确认」「平台归因待判定」，让 Incident 可以组织响应，同时避免卡片把相关性说成因果。再接入版本化证据：采样时间、来源、有效期、实体身份、观察窗口、部署变更和对照证据。缺样本、来源 stale、无流量都不能直接判健康。健康 evidence 不可用时保持不确定状态，人工裁决不被覆盖。

恢复侧保留人工关闭门槛，给每项判据展示最近证据、证据是否过期、负责人及阻塞项；nudge 可提醒或升级协作，不能代替恢复证明。

### 5. 拓扑、关联和兼容入口的边界

- 入口拓扑：`internal/ccloud/importer.go:292` 保留调用方向；ADR-0006 已承认入口故障传播缺口。优先允许登记 `App → Gateway` 的明确依赖，并保留 trace 调用边的原始语义。不要把所有边全局翻转或默认双向传播。入口不可达、数据过期、覆盖不足都应显示为信息缺口，而不是「无影响」。
- Fault 关联：`internal/mapping/projector.go:291` 的空 correlation labels 有意按实体与 kind 聚合。为磁盘等资源实例提供显式稳定维度，校验必要标签缺失，并展示合并预览；不要给全部 Fault 默认加 startsAt 或 fingerprint，那会破坏跨告警收敛。冷却应有到期重评估和升级策略，不能无限吞掉仍 active 的新一轮。
- generic：当前兼容处理链仍在，配置入口也未把历史兼容来源彻底封死；但直接换 kind 可能先被不同 payload 协议拒绝，不应断言「选错一定双发」。建议统计存量依赖，再禁止新增 generic、限制权威来源降级转换，给保留来源明确兼容开关与审计。同步修正 `internal/pipeline/doc.go:1` 的过时顺序说明。
- 凭据：URL bearer 的风险需要靠接入层路径脱敏、轮换演练和限流控制。当前契约明确使用 hook UUID，不能在小修中直接改成另一套鉴权。需要平滑轮换时，再设计新旧凭据短期共存与撤销窗口。

## Agent M1 的优化边界

`internal/agentclient/client.go:151` 是 ACK 后执行 handler；已有集成用例明确接受 ACK 后崩溃丢分析。关键通知必须继续走确定性的 Fault 通知路径，不能依赖 Agent 分析成功。

如产品开始要求分析任务可恢复，推荐下一阶段先做本地持久 inbox：领取 → 校验 → 持久写入 → ACK → 执行 → 持久结果与 report 重试。消息 ID 负责本地去重；ACK 结果不确定时重试同一 receipt，旧 lease 失效后必须按服务端结果处理，不能盲目执行。inbox 只能覆盖本机进程崩溃，不能宣称覆盖磁盘丢失；跨主机恢复需要另外设计服务端执行状态和租约。

相比直接改成「执行完才 ACK」，这个方向保留 M1 的接收语义，不必立刻把长模型调用接入投递租约。但执行与结果上报仍可能重试，handler 必须幂等。结果只追加证据或建议，不直接修改 Fault / Impact / Incident。

启用后的 agent_batches 写入与告警入库同事务，是投递意图持久性的选择。保留这个事务时，应保证 AI 与 handler 始终在事务外，并监控批次写入失败、积压、TTL 和 dead 状态；不能通过忽略批次写入错误来宣称可靠隔离。需要连分析存储故障也不影响接入时，应另立决策，明确从哪个已持久化的事实重建分析任务。

## 迁移编号必须先纠正

旧 spec 仍要求健康证据使用 000028，但当前代码最大迁移号为 000031。Dockerfile 固定使用 golang-migrate v4.18.3；其 Up 从数据库当前版本开始，readUp 只获取 Next(version)，不会补跑后来插入的较小版本。因此，对已升级至 29、30 或 31 的环境，事后增加 000028 不能靠正常 migrate up 生效。依据：[golang-migrate v4.18.3 实现](https://github.com/golang-migrate/migrate/blob/v4.18.3/migrate.go#L502)。

建议在实施前修订编号约定，使用实施时最大号加一，不改已发布迁移、不 force 降低账本。测试必须包括空库安装，以及从 29、31 版本的存量库真实 migrate up；当前部分测试直接顺序执行全部 SQL，只能覆盖空库结构，不能发现这个升级缺口。这里是基于工具实现的升级风险判断，本次没有读取线上 migration ledger。

## 建议的交付顺序

| 批次 | 范围 | 完成标准 |
|---|---|---|
| 1 | 通知 queued/sent/failed 语义、暂缓抑制与失败释放、active Fault 补投 | 不存在仅因「上游已入队」永久失联的下游；补映射和补渠道后可限速补投 |
| 2 | scheduler 分池、按池进度与积压监测 | 慢通知不拖住响应定时器；健康状态能区分执行慢与循环死亡 |
| 3 | 生命周期门槛、映射原因治理、关联预览 | 下线业务不充当宣布凭据；每条重要 firing 告警的去向可解释 |
| 4 | 入口依赖语义、健康证据与归因消歧 | 回答「应用是否退化」与「平台是否为原因」时分别提供证据 |
| 5 | Agent 持久接收与结果回报 | 需要可靠分析时可恢复执行，仍不赋予 Agent 领域状态裁决权 |

迁移编号约定修订应先于以上任何新迁移；generic 限制与过时注释可单独小批次交付。实际线上 fallback、来源存量、映射率与队列时延需要只读基线后再制定数值目标。

## 本次验证

以下命令全部通过：

```bash
go test ./internal/app ./internal/impact ./internal/mapping ./internal/api ./internal/scheduler ./internal/notify ./internal/agentclient ./internal/agentdelivery -count=1

go test -tags=integration ./internal/scheduler ./internal/agentdelivery ./internal/store -run 'TestScheduler_DetachedHandler_ClaimCommittedAndLockReleasedDuringHandler|TestScheduler_DetachedHandler_ExpiredClaim_IsReclaimedAndRetried|TestScheduler_SettleClaim_StaleTokenDoesNotOverwriteReclaim|TestLostPollAndCrashAfterAck|TestCanonicalTraversalSkipsDecommissionedAndDownstream|TestFaultImpactHumanDecisionWins' -count=1 -p=1
```

第二组在本地 PostgreSQL 测试容器上执行了 6 个选定集成用例，验证了现有租约与断连边界、下线遍历、人工裁决保留。已有测试通过只证明这些已覆盖行为，不证明本文指出的通知补偿缺口已经修复；上述新增故障场景是后续实施验收项。本次未执行远端发布、线上破坏性故障注入或整套 E2E。
