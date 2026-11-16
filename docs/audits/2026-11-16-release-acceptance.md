# 2026-11-16 可靠性修复发布验收

已完成 Firebreak **dev** 发布与隔离通知端到端验收。API、Web 运行 `reliability-fb505a2`，数据库为 `32 / dirty=false`。本次没有发布 PRO，也没有改变 IAC、E2B 的业务关系或通知配置。

跟踪：[Issue #20](https://github.com/iluoxw/alert-system/issues/20)。脱敏实测记录：[acceptance.json](evidence/2026-11-16-release/acceptance.json)，迁移回执：[migration.log](evidence/2026-11-16-release/migration.log)。

## 提交与发布

PR #10、#17、#18、#19、#16 已依次合并。发布提交为 `fb505a2b2d1130407c319f7a4b2ff49cc52cec4a`；排除文档后与已完成联合验收的 `86eb202` 无差异。[最终 main CI](https://github.com/iluoxw/alert-system/actions/runs/43816127760) 的后端 build、vet、PG integration/race/E2E 以及前端构建、lint 全部成功。

目标为 CCloud 内网集群 50 的 Firebreak dev，入口 `https://sre-control.test.seewo.com`。API/Web 滚动发布后均为期望、当前、就绪、可用副本 1，流量权重 100%，新 Pod 重启数 0。实际 Pod imageID 与发布镜像 digest 一致。

| 组件 | 标签 | digest（sha256） |
|---|---|---|
| API | `reliability-fb505a2` | `eb0fbe718f7e44d9b3fc5a6737dd6f3bc5abc6ac5cfeb78f565326eb0e6a8db9` |
| Web | `reliability-fb505a2` | `8aa4e750b59bae7b898aa2e54ad01ad41b65a1999cf2561c6b88424174076be7` |
| migrate | `reliability-fb505a2` | `b053d52ca3948d008d8bc16bc873d68a335f588515bfe439e47b5a3a8bfc2dc8` |

三个产品镜像均为 linux/amd64，OCI revision 为发布提交。一次性验收工具镜像另记在 evidence 中，不作为产品镜像。

应用没有迁移 initContainer。本次独立 Job 先校验旧账本为 31，再执行迁移 32；回读确认 dirty=false、通知表及 `faults_activation_version` 触发器存在。未回填历史 `owner_notified_at` 为 sent。

## 线上验收

| 场景 | 实际结果 |
|---|---|
| 真实域名与认证 | 主页、runtime-config 为 200；匿名 me 为 401；临时管理员登录及管理接口正常 |
| 健康端口 | 通过 API Pod 的独立端口检查，`/healthz`、`/readyz` 均为 200 |
| 调度装配 | normal 1 worker、detached 2 workers；三者成功轮询时间均推进，最终无 degraded、在途或逾期任务 |
| 未映射测试 occurrence | 独立来源、团队及运行标签命中 `unmapped/no_policy`，没有误命中三条真实 E2B 策略，也不在 Agent 路由来源白名单内 |
| 静默回放 | 精确策略将该 occurrence 映射到独立测试实体并建立 active Fault；watch、delivery、通知时间线均为 0，接收端次数为 0 |
| 通知预览 | 结果为 ready、单目标 queue；API 前后响应相同，两次只读数据库快照相同，未登记 watch、delivery 或通知时间线；全局 notify_send 历史行数保持 14 |
| 显式提交 | 一条 delivery 变为 sent；独立集群内接收端仅接受一次，载荷关联同一测试 Fault；owner_notified_at 此后才有值，尚未人工 ACK |
| 重复提交 | 再提交两次，delivery ID、epoch=1、generation=0 不变，接收端次数仍为 1 |
| 人工认领与解除 | 两次 ACK 返回同一认领状态；原 occurrence 的 resolved webhook 使 Alert 解除；随后测试实体下线，最终 Fault 为 resolved，不将下线收口描述为自动恢复 |
| 通知审计 | 仅一条 `notify_backfill` 和一条 `notify_sent`；最终无 pending/claimed 的 notify_send；管理响应没有接收目标原文 |
| 测试清理 | 两个测试实体下线、策略与来源禁用、测试团队周期任务关闭且渠道清空；临时管理员禁用后原会话返回 401；接收端 Deployment/Service/ConfigMap 和临时凭据 Secret 已删除 |

测试运行标识 `rollout-20261116-3c7e1b`。保留禁用的来源、实体、Fault 和审计记录供追溯，未把审计记录硬删除。团队和管理员均为本次独立创建，未重置现有账号。

本次线上没有注入慢发送、失败、超时、租约抢占或全池饱和。此类边界仍由此前独立 PG/race/E2E 验收覆盖；不能把本次成功发送解释为线上压力测试。

## 当前治理基线

发布前后真实 firing 均为 0，治理统计为 `items=[]`。205-alertmanager 为 healthy，对账持续成功，unknown=0、连续失败数为 0。隔离测试清理后再次回到空治理统计。

因此本轮没有真实告警的映射写入对象，也没有可计算的当前覆盖率分母。历史 4/28 不用于本轮验收，空统计不等于 100% 覆盖。

用户明确入口依赖留待 IAC 服务接入。本轮保留 E2B 实体、alias、策略及依赖原样；部署平台、观测数据存储和受监控业务之间不能凭名称推断依赖。IAC 接入另行分析当前 Go 生产链路及 OMA Demo，不翻转既有 trace 边。

## 运行边界与回退

- API/Web 仍是单副本。本次验证发布与功能，没有证明高可用。
- 平台 readiness、liveness 都仍指向 `/healthz`；`/readyz` 仅被本次直接检查。平台 Ready 数不能代表普通调度池一直受健康门控。
- 当前 execution timeout 为 120 秒，settle 上限 15 秒，租约 300 秒。超时依赖 handler 遵守 context；外部发送已成功但结果未落库的窗口仍可能重投。
- sent 仅表示渠道接受。本次只向专用接收端发送，不证明真实人员已读或真实通知渠道的送达率。
- 旧镜像指针为 API `issue9-63778bf`、Web `0.3.2`。回退应用前还须评估新 timer payload 与旧代码的兼容性；不自动执行数据库 down，避免丢失新投递账本。
- dev 数据库中的 prod/production 是领域记录的环境标签，不改变本次部署目标为 dev 的事实。
