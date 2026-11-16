# 失败模式可靠性与正确性迭代计划

> 执行方式：按 writing-plans / executing-plans 工作流逐项交付；实现与独立验收使用 Codex sol 子 Agent，主 Agent 负责架构裁决、Git 操作与集成验收。用户已授权实施和 Issue + PR 交付，无需再等待开工确认。

**目标：** 修复通知失联、慢发送阻塞与无效影响准入，同时建立可解释的映射治理入口。

**架构：** 保留 PostgreSQL、事务内 outbox 与事务外外部 IO；通知送达状态持久化，调度按任务类型隔离执行容量。影响沿用确定性映射、显式依赖和人工决策边界，不扩大 Agent 的领域写权限。

**技术栈：** Go 1.25、pgx、PostgreSQL 16、Gin、React 19 / AntD 5；本轮不引入外部队列。

## 基线、范围与交付

- 基线：`63778bf6be92b94e6a85984eed91d06414a8c96f`，PR #10 尚未合并。所有本轮 PR 暂以 `codex/issue-9-agent-delivery` 为 base。
- 用户确认：本轮交付可靠性与正确性修复；健康证据采样/归因自动化、Agent 持久 inbox/report 只建后续 Issue。
- Issue 和 PR 一对一关联。实现 PR 使用 `Closes #N`，堆叠基线未并入默认分支前，Issue 保持 open，不提前宣称产品发布。
- 不自动合并、不部署线上、不发送测试通知给真实团队。故障注入只用隔离 PostgreSQL 和本地 fake 渠道。
- 原工作区及未提交审查文档保留。实现分别位于隔离 worktree；子 Agent 不执行 Git，不改其他 worktree。
- 主 Agent 负责所有迁移号：本轮通知状态使用 **000032**。其他迭代不分配 schema 迁移。历史 `000028` 保持空缺，健康证据以后使用实施时最大号加一。
- 已有低于最大号的迁移不修改、不重新编号、不 force 降低账本。编号规则修订属于本轮已授权修复。

## I1：通知送达、抑制补偿与补投

Issue：[#11](https://github.com/iluoxw/alert-system/issues/11)。

**工作目录：** `/private/tmp/alert-failure-notify`。

**主要文件：** `internal/app/fault_outbox.go`、`internal/app/outbox.go`、`internal/notify/sender.go`、新增 `internal/store/fault_notifications.go`、新增迁移 `000032_fault_notification_delivery.up.sql/down.sql`；API、新配置与启动对账按实际需要接入 `internal/api`、`internal/config`、`internal/app/app.go`、`cmd/server/main.go`、`test/e2e/harness.go`。

1. 写回归用例，复现「上游仅入队/最终失败，下游被永久抑制」和「补渠道后 active Fault 不再进入激活通知」。验证旧实现不能满足这些断言。
2. 引入每个 Fault/通知目标的稳定意图与结果记录。至少区分等待、入队、成功、失败；成功是渠道接受，不是人阅读。历史 owner_notified_at 不能直接回填成已送达。通知目标原文不得经审计/API 输出。
3. Sender 成功/最终失败在现有短事务中回写结果与 Fault 时间线。回调不得执行外部 IO。通知 key 与 round 必须持久稳定，重复扫描不能生成新意图。epoch 绑定数据库内真实激活版本，不能信任事务外/写入前推断的 Transition；静默重激活不自动补发，但显式提交必须区别上一轮 sent。
4. 下游抑制检查真实送达与目标覆盖。仅 pending 时要么不抑制，要么使用显式配置的有限暂缓；缺省不得无限等待。不把未确认的响应 SLA 写成默认承诺。
5. 对暂缓、冷却和无目标记录做有界持久对账。周期任务启动时必须 reconcile，崩溃后能恢复；每次批量有上限。成功记录不重复发送；失败重投必须显式或有次数上限，避免永续重试风暴。
6. 提供管理员补投预览与提交入口。默认 reprocess 保持静默，预览不写任何状态；提交重新校验，不能信任客户端旧预览。已恢复/已认领对象不自动补投，补齐渠道后可触发一次有效通知。
7. 提供可查询的通知状态、阻塞原因和审计；至少通过现有 API 或明确的管理端点可操作，不能只提供无人调用的内部函数。
8. 自测真实 PG 升级与并发，输出 `docs/audits/2026-11-16-notification-acceptance.md`，区分已执行测试与待验证事项。

**验收矩阵：**

| ID | 故障/操作 | 必须满足 |
|---|---|---|
| N1 | 上游 queued 后发送失败，fallback 缺失 | 不把入队记为送达；下游不会永久失联 |
| N2 | 上下游不同 team/target | 不以无覆盖关系的通知抑制下游 |
| N3 | 多渠道部分成功 | 各目标结果可解释；失败目标不因其他成功而丢失 |
| N4 | 补规则→静默回放→预览→补投 | 预览无副作用；仍 active 未送达目标只产生一个稳定意图 |
| N5 | 补渠道/冷却到期/进程重启 | 对账可恢复；已恢复/认领不被自动轰炸 |
| N6 | 多实例同时补投/并发激活；静默重激活后补投 | PG 唯一性/锁保证逻辑意图不重复；过期 Transition 不生成新轮次，新激活不复用旧 sent；外部 IO 的 at-least-once 边界明确 |
| N7 | 从 29/31 升级，以及空库安装 | 新结构存在、已有数据保留；down/up 可往返，不回填 28 |

## I2：调度执行容量隔离

Issue：[#12](https://github.com/iluoxw/alert-system/issues/12)。

**工作目录：** `/private/tmp/alert-failure-scheduler`。

**主要文件：** `internal/scheduler/scheduler.go` 及测试、`internal/config/config.go`、`internal/api/directory.go`、`internal/app/app.go`、`cmd/server/main.go`；可新增 scheduler 管理状态端点。

1. 写真实 PG 回归：一个或全部通知执行槽被 fake handler 阻塞，同时插入普通 timer；旧实现普通 timer 不能及时完成。
2. `Run` 管理有限数量的同步 worker。普通池排除所有 detached kind；通知池仅领取 detached kind。每个 worker 完成当前任务才领取下一个，无 fire-and-forget，无额外持有租约的内存排队。
3. 并发与执行超时配置有明确上下界。默认值仅是工程资源配置，不宣称响应 SLA。执行上限必须小于 lease TTL，或提供经测试的续租；数据库与 settle 时间也必须纳入边界。
4. 保留 claim 提交后 IO、claim_count fencing、ctx 取消保留可恢复状态和成功后取消仍 settle 的语义。新增普通 timer kind 不需要逐个改过滤白名单。
5. 按 worker 记录轮询、在途开始、最近完成、执行耗时；输出按 kind 的 pending/overdue/failed、最老等待与重领。状态不包含 payload、ref_id 或渠道地址。
6. readiness 区分主处理循环与慢通知。慢渠道不使持久化入口失去就绪；通知池异常有独立可见状态，不能直接忽略。共享心跳不能覆盖单个 worker 的停滞。
7. 验证退出和竞态，输出 `docs/audits/2026-11-16-scheduler-acceptance.md`。

**验收矩阵：** S1 通知池占满时普通 timer 在测试约定时限内执行（使用同步屏障，避免随机器负载抖动）；S2 在途不超过配置并发且无超领；S3 正常空闲、合法长任务完成到下次取库之间不误报；持续 DB 失败不能伪造完成进度；S4 慢通知不打红接入 readiness、故障状态仍可查询；S5 租约过期重领、旧 token、失败、panic、取消、成功后取消全部回归；S6 race 通过。

## I3：生命周期与映射正确性

Issue：[#13](https://github.com/iluoxw/alert-system/issues/13)。

**工作目录：** `/private/tmp/alert-failure-correctness`。

**主要文件：** `internal/store/faults.go`、`internal/incident/service_integration_test.go`、`internal/mapping/projector.go`、`internal/api/service_catalog.go`、`internal/store/service_catalog.go`、`internal/impact/projector.go` 及相关测试；`internal/pipeline/doc.go`、`CLAUDE.md`。既有 spec 编号修订由主 Agent 统一更新。

1. 新增真实 PG 回归：provider 仍 active，唯一 confirmed 应用已下线；Declare 必须失败。补充仍有其他 active confirmed 时允许宣布，已有 Incident 不自动关闭。
2. 在权威 store 判定加 lifecycle 过滤，核对总览/列表口径；保留人工历史裁决，不删除历史关系。
3. 把映射匹配拆成可复用的只读评估。显式配置的 correlation labels 必须有值，否则产生稳定 reason_code，不自动建/合并 Fault；未配置维度沿用原聚合规则。升级前已绑定且缺维度的 occurrence 必须仍可沿历史绑定解除，保留人工证据；共享 Fault 还有未解除证据时不提前降级。
4. 管理端提供只读预览：目标 entity、fault kind、correlation key、已有/拟创建 Fault、缺失原因。真实 Project 复用同一计算路径；HTTP 预览无 evidence、timeline、timer、mapping result 写入。
5. 增加当前 firing 治理统计，按 source/severity/reason 区分；ignored 不能冒充 unmapped。集合为空返回 []，批量上限与管理员鉴权必测。
6. 跨节点自动 confirmed 的解释明确应用退化事实与因果不确定性，不改 direct evidence 判定规则。测试显式 App→Gateway 依赖可达、stale 证据不误确认、下线阻断；不翻转 ccloud trace 方向。
7. 修正 pipeline 注释顺序和新迁移使用最大编号+1的约定。输出 `docs/audits/2026-11-16-correctness-acceptance.md`。

**验收矩阵：** C1-C3 生命周期三种场景；C4 标签缺失治理/完整拆分/空维度兼容；C5 预览结果与真实投影一致且数据库状态不变；C6 当前 firing 统计按原因分类且排除 resolved；C7 入口/stale/下线拓扑；C8 无越权、无敏感目标输出。

## 后续迭代（本轮仅 Issue）

### I4 健康证据与归因

后续 Issue：[#14](https://github.com/iluoxw/alert-system/issues/14)。

前置输入：健康接口采样语义、窗口 W、连续周期 K、变更覆盖和真实数据基线。交付依次为版本化存证→独立探测 loop→自伤消歧矩阵→证据轨迹与恢复判据。验收包含缺样本/零值/stale/无流量、窗口未到、变更嫌疑、人工裁决、外部接口失败和真实迁移升级。不以 synthetic 测试宣称真实平台归因有效。

### I5 Agent 持久接收与回报

后续 Issue：[#15](https://github.com/iluoxw/alert-system/issues/15)。

前置输入：必达路由、结果脱敏字段、本地保留与容量限制。交付 inbox→ACK 恢复→受控执行→幂等 report；分别在落盘、ACK、执行前后 kill。旧 lease 拒绝不能触发执行，结果不修改领域状态；磁盘丢失与跨机接管另立保证。

## 分工与复核流程

1. 三名 sol 实现 Agent 在独立 worktree 实施 I1/I2/I3，各自补测试和验收记录。配置/装配交叉改动由主 Agent 集成时处理。
2. Agent 完成自测后，主 Agent 固化提交；轮换为另一名 sol 独立审查该提交的失败模式与验收覆盖。独立审查发现 P0/P1 或明确行为错误时退回修复，不用「测试绿」代替判断。
3. 主 Agent 在临时集成分支合并三个实现提交，运行构建、vet、后端全量/race/集成，以及必要 E2E。E2E 使用固定 Compose 资源，整个会话只允许主 Agent 串行运行一个 E2E 测试进程。最终 Go 包级并发设为 2，普通包的随机命名 testcontainers 可并行，避免误把“不得同时启动两套固定 E2E”扩大为全仓所有包必须串行。
4. 若修改前端，执行 `npm run build` 与 `npx oxlint`；未改前端时仍以 CI 完成已有前端回归。
5. Issue、计划、PR 描述写清最终范围与测试结果。PR 发布后检查对应 HEAD 的 CI 到完成；失败修复后才交付。

## 命令与通过标准

```bash
go build ./...
go vet ./...
go test -tags=integration ./... -race -count=1 -p=2
```

测试必须真实结束并退出 0；禁止把后台仍运行、skipped、缺少 integration tag 当成通过。若因环境失败，先记录错误和影响范围，修复环境或提供等价验证，不篡改测试阈值来掩盖产品失败。

本地 PostgreSQL 故障用例、迁移空库与存量升级、API 鉴权/幂等/无副作用、并发竞态为本轮发布到 PR 的验收闸门。线上启用不属于本轮授权范围。

## 状态与证据

- [x] 基线核对、用户范围确认、隔离 worktree 建立。
- [x] I1 通知实现、自测、激活版本与兜底身份修复（PR #19）。
- [x] I2 调度实现、自测与 readiness 审查修复（PR #17）。
- [x] I3 正确性实现、自测与历史 occurrence 升级修复（PR #18）。
- [x] 交叉独立审查及问题修复：4 项 P1、2 项 P2 均有红灯证据及固定 SHA 复验。
- [x] 主 Agent 集成验收：`86eb202` 的 build、vet、全仓 integration/race（含 E2E）全部退出 0。
- [x] 实现 PR 发布、产品 CI 收敛、Issue 与验收记录关联：#17 / #18 / #19，基于未合并的 #10。

最终状态由主 Agent 按实际证据更新，不由实现 Agent 自行勾选。

完成回执与最终固定提交见 [联合验收](../audits/2026-11-16-failure-mode-acceptance.md)。健康证据 #14、Agent inbox/report #15 按用户确认留待后续；本轮不合并、不部署。
