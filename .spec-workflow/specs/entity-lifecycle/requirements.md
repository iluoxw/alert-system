# Requirements Document — entity-lifecycle

## Introduction

本 spec 补上一个被真实事故暴露的盲区：**受管实体没有生命周期**。系统能表达「我看不到它了」（stale），但无法表达「它没了」（decommissioned），而这两者要求的处置完全相反——前者要人去排查，后者要把它从图里摘掉。

**触发案例（uat，2026-09-20 实测）**：

| 事实 | 数据 |
|---|---|
| 节点 `10.61.17.11`（`p-ai-sandbox-001`）**已从 E2B 集群剔除**（用户确认的现实） | — |
| 采集目标随之消失，告警"自动解除" | `E2BNodeExporterDown` 解除于 09-14，`E2BNodeExporterCoverageGap` / `E2BNomadAgentsCoverageGap` 解除于 09-19 |
| INC-000001 因此无法收口，持续 monitoring | 195h56m |
| `incident_recovery_nudge` 每 6 小时提醒一次 | 已 fired 12 次（近 24h 内 4 次） |
| 实体仍以受管身份留在拓扑里 | `resource_nodes` 无任何生命周期列，`last_observed_at` 停在 09-12 |
| 后台无法表达下线 | 受管实体只有 `POST /api/v1/admin/entities`、`PUT /api/v1/admin/entities/:id`，**无删除、无状态字段** |

这正是不变量 11 存在的原因：只要实体没有终态，「告警消失」就永远是歧义信号。补上生命周期，这类事故才能从「靠人工确认」变成「下线即可收口」。

**范围之外（明确不做）**：
- 不做资产管理 / CMDB 台账（不变量：产品边界不是通用 ITSM）。
- 不自动发现下线：遥测消失**永远**不能推断为下线（见 R1.4）。
- 不做实体删除。历史 Fault 与时间线必须保持可追溯，删除会让 `faults.entity_ref` 悬空。
- 不改 Alert 生命周期层（不变量 14 / 24）。
- 不引入新的 timer kind，不做定时自动下线。

## Alignment with Product Vision

| 产品主张 | 本 spec 的落地 |
|---|---|
| 领域链路 `Signal → Alert → Fault → Incident → Response → Recovery → Learning` | 补上 Recovery 段的一个合法终态：**下线收口**，区别于「恢复」 |
| 爆炸半径以多对多依赖图为准 | 下线实体退出遍历，避免把不存在的机器算进影响面 |
| AI / 自动化只基于证据辅助，不替人做高风险决策 | 下线是**纯人工**动作，系统只做前置校验与联动提示 |

## 硬约束（违反即返工）

引自 `CLAUDE.md` 核心架构不变量，实施期不得偏离：

| 约束 | 内容 | 涉及 |
|---|---|---|
| 不变量 11 | Incident 必须人工宣布并受双重关闭门槛约束；nudge 永不推进状态；文案写"解除"而非"恢复" | R3 R5 |
| 不变量 13 | 集合字段即使为空也返回 `[]`；审计输出结构稳定、不泄露敏感目标 | R4 |
| 不变量 15 | 遥测只能发现 `unmanaged` 待认领实体，**禁止依据不可信 label 自动创建团队或改变归属**；同理禁止自动改生命周期 | R1 |
| 不变量 16 | 图可达不等于真实影响；`potential / confirmed / ruled_out` 三态不变；人工裁决永不被自动重算覆盖 | R2 |
| 不变量 19 | 一级导航只表达 SRE 一级任务；实体管理属于控制中心，不新增一级入口；引用必须目录驱动可搜索 | R4 |
| 不变量 20 | 同一 `pgx.Tx` 内二阶段查询必须先扫完 `Rows`、检查 `err`、`Close` 再发子查询 | R2 |
| 不变量 22 | 「没有告警了」只是缺乏证据，不等于恢复 | R5 |

**迁移编号**：era3-fault-workunit 已占用 `000026 / 000027 / 000028`（028 预留给其任务 28）。本 spec 从 **`000029`** 起，不得占用更小的号。

## Requirements

### R1 实体生命周期状态

**User Story:** 作为平台 SRE，我希望能把一台已经下线的机器标记为「已下线」，从而让系统不再把它当成一个暂时失联、需要排查的实体。

#### Acceptance Criteria

1. `resource_nodes` SHALL 增加生命周期状态，取值仅 `active` / `decommissioned`，默认 `active`；既有行迁移后 SHALL 全部为 `active`（行为与引入前逐字节一致）。
2. 系统 SHALL 记录下线的审计信息：下线时间、操作人、下线原因（必填文本）。
3. 下线 SHALL 是**人工动作**，且 SHALL 写入实体时间线（或等价审计记录），包含操作人与原因。
4. 遥测、拓扑同步、告警映射 SHALL NOT 自动把实体置为 `decommissioned`；`last_observed_at` 过期只能表达 stale（沿用不变量 15）。
5. 系统 SHALL 支持把 `decommissioned` 恢复为 `active`（误操作可逆），且同样写审计记录。
6. 下线 SHALL NOT 删除实体、别名、依赖边或任何历史引用；`faults.entity_ref` 等历史关联必须保持可解析。

### R2 下线实体退出影响计算

**User Story:** 作为平台 SRE，我希望已下线的机器不再出现在爆炸半径里，从而不被不存在的资源误导判断。

#### Acceptance Criteria

1. 爆炸半径遍历 SHALL 跳过 `decommissioned` 实体，且 SHALL NOT 经由它继续向下游传播。
2. WHEN 实体被下线 THEN 由它产生的 `potential` Impact SHALL 被标记为 `ruled_out`，理由记为实体已下线。
3. 人工裁决过的 Impact（`confirmed` 或人工 `ruled_out`）SHALL NOT 被本机制覆盖（不变量 16）。
4. 下线实体的依赖边 SHALL 保留在库中（供历史追溯），但 SHALL NOT 参与实时遍历。
5. 默认列表与工作台视图 SHALL 不展示 `decommissioned` 实体；SHALL 提供显式筛选查看它们。
6. 先行指标（`entity-lifecycle` 相关计数）SHALL 只统计 `active` 实体，避免已下线资源长期拉高分母。

### R3 下线前置校验与联动收口

**User Story:** 作为平台 SRE，我希望下线一台机器时，系统告诉我它身上还挂着哪些未收口的 Fault 和 Incident，从而不留下孤儿事故。

#### Acceptance Criteria

1. WHEN 用户发起下线 THEN 系统 SHALL 先返回该实体关联的未收口对象清单：`active` / `monitoring` 状态的 Fault、未关闭的 Incident、未解除的告警证据；集合为空时返回 `[]`（不变量 13）。
2. IF 存在 `active` Fault THEN 系统 SHALL 拒绝静默下线，要求操作人显式选择：一并按「实体已下线」收口，或先自行处理。
3. WHEN 操作人选择一并收口 THEN 系统 SHALL 在**同一事务**内把这些 Fault 推进到 `resolved`，并在各自时间线写明原因是实体下线（不是恢复）。
4. 系统 SHALL NOT 自动关闭 Incident。Incident 关闭仍须走不变量 11 的双重门槛与人工确认；下线只能把它的关联 Fault 收口，并在 Incident 时间线追加一条说明。
5. 下线动作 SHALL 幂等：重复下线按当前状态短路返回 200，不重写审计记录（沿用不变量 6 的处置口径）。

### R4 API 与控制中心页面

**User Story:** 作为平台 SRE，我希望在实体详情页直接完成下线，并能查回历史上下线过哪些实体。

#### Acceptance Criteria

1. 系统 SHALL 提供 admin only 的生命周期变更端点，接受下线原因与「是否一并收口关联 Fault」的显式选择。
2. 实体列表 SHALL 支持按生命周期状态筛选，默认只看 `active`。
3. 实体详情 SHALL 展示生命周期状态、下线时间、操作人与原因。
4. 页面 SHALL 在下线前把 R3.1 的未收口清单呈现给操作人确认，不得在用户看不到影响面的情况下直接提交。
5. 相关入口 SHALL 收在控制中心的实体/服务目录下，SHALL NOT 新增一级导航（不变量 19）。

### R5 假恢复口径的收敛

**User Story:** 作为值班 SRE，我希望当证据告警因为采集目标消失而解除时，系统给我的提示能直接引导到「确认是否已下线」，而不是让我反复猜。

#### Acceptance Criteria

1. `incident_recovery_nudge` 的文案 SHALL 在既有「可能源于采集目标下线或服务发现注销」提示之外，给出可执行出口：确认实体已下线则走下线收口。
2. 文案 SHALL 继续使用「解除」而非「恢复」（不变量 11）。
3. WHEN Incident 的全部证据实体都已 `decommissioned` THEN nudge SHALL 在提示中明示这一事实，帮助操作人判断可以收口；但 SHALL NOT 自动推进 Incident 状态。
4. 本 spec SHALL NOT 改变 nudge 的触发条件与间隔语义（`INCIDENT_NUDGE_INTERVAL`，0 关闭）。
