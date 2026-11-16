# 失败模式迭代联合验收

日期：2026-11-16。产品基线：`63778bf6be92b94e6a85984eed91d06414a8c96f`（PR #10）。本次按用户确认只实施可靠性与正确性修复；健康证据自动化和 Agent 持久执行只建后续 Issue。

## 交付与固定提交

| 交付 | Issue | PR | 产品提交 |
| --- | --- | --- | --- |
| 计划、审查与验收证据 | — | [#16](https://github.com/iluoxw/alert-system/pull/16) | 文档分支独立更新 |
| 通知实际送达、覆盖与补投 | [#11](https://github.com/iluoxw/alert-system/issues/11) | [#19](https://github.com/iluoxw/alert-system/pull/19) | `bcb806d` |
| 调度分池与可观测状态 | [#12](https://github.com/iluoxw/alert-system/issues/12) | [#17](https://github.com/iluoxw/alert-system/pull/17) | `cdfe1bd` |
| 生命周期与映射治理 | [#13](https://github.com/iluoxw/alert-system/issues/13) | [#18](https://github.com/iluoxw/alert-system/pull/18) | `ba0e483` |

联合产品验收提交：`86eb2026fd4c1e16fe53f5c841f7eac24fa52291`，由上述三个实现分支及计划初稿汇合；测试期间不修改产品代码。

各实现 PR 以 `codex/issue-9-agent-delivery` 为 base；不是已经进入 main 的产品发布。合并时先处理 #10，再将三个实现 PR 调整到 main，并以新的合并目标重跑 CI；本次不会自动执行这些合并动作。未合并、未部署、未向真实团队发送测试通知。后续范围：[健康证据 #14](https://github.com/iluoxw/alert-system/issues/14)、[Agent inbox/report #15](https://github.com/iluoxw/alert-system/issues/15)。

详细计划和逐项验收矩阵见 [迭代计划](../plans/2026-11-16-failure-mode-iterations.md)。各实现者的 focused 测试回执分别见本目录 notification、scheduler、correctness acceptance 文档（位于对应实现 PR）。

## 独立审查发现与处理

实现和独立复核由 sol 子 Agent 交叉执行，主 Agent 负责范围、并发协议取舍、GitHub 交付、迁移与联合验收。所有失败都保留原始证据，未以放宽断言或增加等待阈值解决。

| 发现 | 真实失败场景 | 修复 |
| --- | --- | --- |
| I2 P2 | 合法长任务完成后、下一轮取库前 readiness 误报 | 只有成功提交业务事务或 fencing settle 才记成功完成进度；DB 查询/提交失败不伪造进度 |
| I3 P1 | 升级前缺维度的 occurrence 解除后删除证据、Fault 永久 active | 缺维度禁止新建，但沿已有绑定收口，保留历史和人工证据，不重开已 resolved Fault |
| I3 P1 | 解除 SQL 等待行锁期间新增 firing 已提交，却用旧语句快照降级共享 Fault | 先独立获取 Fault 父行锁，再在新快照核对证据；新增/替换证据使用单父行屏障，不扩大删除旧关联的跨 Fault 锁范围 |
| I1 P1 | 两事务读到同一旧 candidate，过期 Transition 再生成 epoch 导致重复发送 | 数据库原子维护 activation_version，通知 epoch 绑定真实状态转换 |
| I1 P1 | 静默回放重新激活后，显式补投错误复用上一轮 sent | 当前领域版本与 watch 登记分开；静默新版本不自动登记，显式补投建立新版本意图 |
| I1 P2 | epoch+generation 相加碰撞、多目标失败共用 fallback，导致元告警误去重/互相替换 | 元告警独立保留源 delivery UUID、generation 和目标哈希，Sender key 与 timer ref 使用同一身份，不触发主投递成功回调 |

原始与修复复验报告归档于 `evidence/2026-11-16/review-*.md`；报告明确审查 SHA、命令、已执行结果及覆盖限制。

CI 还发现两类 E2E 夹具需要同步合同：Grafana 策略显式要求 cluster，但旧 webhook/对账夹具缺值；通知 timer 改用稳定 delivery UUID 后，旧测试仍按旧 ref_id 前缀计数。分别补齐同一 cluster、断言对账/解除保持同一 Fault，以及改为持久 payload GroupKey 计数并等待真实 sent 后测试覆盖。

## 迁移验收

已验证迁移内容 SHA-256：up 为 `a97399298c26232f444d6948e91bd0ce69650918a716682249afc27cd819dc70`，down 为 `ecad2f2ea88ae26642e8aa95c33016e70977d58a994c1bb5ebf7af606a22b33f`。

迁移 32 的最终版本包含 watch/delivery 表，以及 `faults.activation_version` 与维护触发器。历史迁移 28 保持空缺，既有迁移未重编号。

已用 PostgreSQL 16 和真实 golang-migrate ledger 执行以下三种起点，全部升级至 `32/dirty=false`，再 down 一步至 31、重新 up 至 32：

- 空库安装。
- 版本 29 存量升级。
- 版本 31 存量升级。

29/31 场景保留 teams、旧 active Fault、历史 owner_notified_at 哨兵数据；不回填 watch/delivery。普通更新不改变 activation_version，monitoring→active 恰好加一并清本轮 owner_notified_at。down 清理新增列和触发器，再 up 保留既有 Fault。down 会丢弃新通知元数据，不承诺降级后的投递记录恢复。

可复现脚本与实际结果：`evidence/2026-11-16/verify_migrations.py`、`migration-results.json`。

```sh
python3 docs/audits/evidence/2026-11-16/verify_migrations.py \
  /path/to/checked-out-notification-or-integration-branch \
  --image sre-control-migrate:issue9-63778bf-offline \
  --output /tmp/migration-results.json
```

本次使用已有缓存迁移镜像，Dockerfile 固定 v4.18.3，但镜像 `-version` 输出为 `dev`，未把构建约定当作运行版本证明。脚本默认提供公开 `migrate/migrate:v4.18.3` 镜像选项；所有容器独立命名、临时复制迁移、结束自动清理，不操作开发数据库。

## 联合验收状态

最终联合验收已于 2026-11-16 完成。固定提交 `86eb2026fd4c1e16fe53f5c841f7eac24fa52291`，执行前后 HEAD 一致、工作区干净；以下命令真实结束，全部退出 0：

```sh
GOCACHE=/private/tmp/alert-integration-gocache go build ./...
GOCACHE=/private/tmp/alert-integration-gocache go vet ./...
GOCACHE=/private/tmp/alert-integration-gocache \
  go test -tags=integration ./... -race -count=1 -p=2
```

integration 标签包含普通单测和数据库集成测试；仓库没有以 `!integration` 排除的另一套用例。最终完整包输出归档于 `evidence/2026-11-16/integration-results.txt`。存储包 326.316s、scheduler 82.007s、完整 E2E 79.175s；这些是本地测试耗时，不是产品性能/SLA 指标。

E2E 使用唯一固定 Compose 项目，覆盖既有权威接入链路及新增 HTTP 静默 reprocess→预览无写入→显式补投→重复提交幂等/脱敏。出站仅 fake 渠道，E2E PostgreSQL 已由 TestMain 清理。前端改动使用实现 PR 的 CI 完成 build/oxlint 验证。

早期预检期间尚有审查修复进行，因此不把其结果作为最终提交证据；以上最终命令在产品代码冻结后单独重跑。

## 实现 PR 的 CI

以下固定 HEAD 的后端构建、vet、全仓 integration/race（含 E2E）及前端 build/oxlint 均已通过：

| PR | HEAD | GitHub Actions |
| --- | --- | --- |
| #17 | `cdfe1bd` | [通过](https://github.com/iluoxw/alert-system/actions/runs/43791044085) |
| #18 | `ba0e483` | [通过](https://github.com/iluoxw/alert-system/actions/runs/43792117927) |
| #19 | `bcb806d` | [通过](https://github.com/iluoxw/alert-system/actions/runs/43792873975) |

## 边界

- sent 表示渠道接受，不表示人员已读/认领。外部发送成功、数据库回写前崩溃仍可能重复，保留 at-least-once 语义。
- 默认 reprocess 静默。历史未登记 Fault 需管理员预览和显式补投；失败只允许有界显式重试。
- 单进程资源配置和测试时限不是生产 SLA，也未进行生产规模压测。
- 未执行真实进程 kill、多机接管、真实飞书/邮件或线上 UAT/PRO 验收。恢复证据来自真实 PG 持久状态、服务实例重建、屏障并发和 fake 渠道。
- 本轮没有自动宣布 Incident，没有健康探测归因，也没有让 Agent 把 ACK/分析写回领域状态。
