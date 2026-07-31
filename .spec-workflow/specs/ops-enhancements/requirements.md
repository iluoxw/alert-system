# Requirements Document

## Introduction

本 spec 将生产告警系统 weaponx 借鉴评估中「建议采纳」的三项运营能力落到 alert-system：

1. **按严重等级差异化的人员升级节奏**——critical 分钟级无人认领即升级，info 不即时升级只保留天级兜底，参数生产验证过（weaponx：DISASTER 3/10min、WARNING 60/120min、INFO 仅天级）；
2. **运营周期任务**——每日运营日报（含 MTTA 与 7 天对比）、悬置高危告警周期通报、每日值班播报，构成升级链之外的防漏单兜底与团队可见性；
3. **通知发送结果写时间线**——值班人员在告警详情页能看到「系统何时通过什么渠道通知了谁」，排查"我没收到"类投诉。

范围外（明确不做）：飞书真实发送对接（hold，等统一通知网关）；自愈/LLM 分析闭环（后续独立 spec）；时段感知通知、expected 预期告警、CMDB 通知人解析（backlog）。

**统一通知网关预留说明**：`internal/notify` 已有渠道无关的 `Channel` 接口与注册表（`channel.go`，phone 已在 Validate 层留位）。未来封装飞书/短信/邮箱/电话的统一网关以**单个 Channel 适配器**接入即可，本 spec 不新建渠道抽象；本 spec 所有新增通知（日报/通报/播报/升级提醒）必须统一走该注册表与 `notify_send` outbox 口径，保证网关接入时零改造覆盖。

## Alignment with Product Vision

alert-system 定位是「接入已触发告警 → 按策略通知、升级」的告警管理系统。MVP 完成了通知与升级的机制层；本 spec 补的是机制之上的**运营层**：让升级节奏匹配告警的实际紧迫度（减少高危漏响应与低危打扰），让团队对告警处理质量（MTTA、悬置量）有周期性可见，让每次通知行为可审计。三者都是值班团队日常运转的刚需，也是 weaponx 多年生产运行验证过的高价值实践。

## Requirements

### Requirement 1：按 severity 差异化的人员升级节奏

**User Story:** 作为值班负责人，我希望人员升级链的等待节奏按告警严重等级差异化——critical 几分钟无人认领就升级、info 不参与即时升级——从而既不漏高危也不被低危打扰。

#### Acceptance Criteria

1. WHEN 管理员配置团队升级策略 THEN 系统 SHALL 支持按 severity（critical/warning/info）为升级链各级分别指定等待时间，同时保留不区分 severity 的默认节奏。
2. WHEN 告警进入人员升级链 THEN 系统 SHALL 按该告警 severity 对应的节奏调度 `personnel_escalation` 定时器；无该 severity 覆写时回退默认节奏。
3. IF 某 severity 被配置为「不即时升级」（如 info）THEN 系统 SHALL 不为该告警创建 personnel_escalation 定时器。
4. IF 升级策略未配置任何 severity 覆写 THEN 系统行为 SHALL 与现版本完全一致（向后兼容，存量策略无需迁移数据）。
5. WHEN 告警被 Ack 或 Resolve THEN 升级定时器取消语义 SHALL 沿用现有不变量（无条件 Cancel，幂等）。
6. WHEN 通过 API 创建/更新升级策略且 severity 覆写值非法（未知 severity、负等待时间）THEN 系统 SHALL 拒绝并返回 400。

### Requirement 2：运营周期任务（日报 / 悬置通报 / 值班播报）

**User Story:** 作为团队管理员，我希望系统定期推送运营摘要、悬置高危告警通报与当日值班播报，让团队不依赖人工盯屏就能掌握告警处理状况。

#### Acceptance Criteria

1. WHEN 到达团队配置的日报时间（默认每日 09:00，团队可启停/改时间）THEN 系统 SHALL 生成并发送该团队的运营日报，内容至少包含：昨日新增/恢复告警数、按 severity 分布、MTTA（基于 `acked_at - starts_at`，含均值与中位数）、与前 7 天日均值的对比、当前未关闭超过 24 小时的告警清单（天级兜底提醒）。
2. WHEN 悬置扫描周期到达（默认每 30 分钟，团队可配）AND 存在 firing 且未 Ack 的 critical 告警 THEN 系统 SHALL 向团队渠道发送悬置通报（告警列表 + 各自持续时长）；IF 无此类告警 THEN 本轮 SHALL 静默不发。
3. WHEN 到达每日值班播报时间 THEN 系统 SHALL 依据 schedules/shifts 解析当天值班人并推送到团队渠道；IF 当天无排班 THEN 播报 SHALL 明示「无值班安排」。
4. 周期任务 SHALL 通过统一 scheduler（timers 表）实现：新增 timer kind 须在 `internal/app` 注册 handler；handler 幂等（at-least-once 安全）并在同事务内自续期下一轮。
5. 所有发送 SHALL 遵守 outbox 不变量：持锁事务内只 Schedule `notify_send`，禁止同步外部 IO；渠道经 `notify.Registry` 解析。
6. IF 系统停机错过若干轮 THEN 恢复后 SHALL 自动回到正常周期（不补发历史轮次），且日报 SHALL 以「团队+日期」为幂等键防止同日重发。

### Requirement 3：通知发送结果写入告警时间线

**User Story:** 作为处理告警的值班人员，我希望在告警详情时间线中看到系统的每次通知投递结果（何时、什么渠道、通知了谁、成功与否），以便排查「没收到通知」类问题。

#### Acceptance Criteria

1. WHEN 一条通知发送成功（notification_log 状态变为 sent）THEN 系统 SHALL 为关联告警追加时间线条目（actor=system，action=notify_sent，detail 含 channel/target/step）；分组通知 SHALL 为组内每条告警各追加一条。
2. WHEN 一条通知重试耗尽最终失败（状态变为 failed）THEN 系统 SHALL 追加 action=notify_failed 的时间线条目，detail 含最后错误摘要。
3. 时间线中的 target SHALL 脱敏展示（手机号/邮箱等部分掩码），detail 不得包含任何凭据或完整敏感标识。
4. 时间线追加 SHALL 与发送状态落库同事务，且以通知的 idempotency_key 保证幂等（崩溃重投不产生重复条目）。
5. 前端告警详情页时间线 SHALL 渲染 notify_sent/notify_failed 条目为可读文案（如「已通过 lark 通知 张三」）。

## Non-Functional Requirements

### Code Architecture and Modularity
- **复用既有机制，不加新基建**：周期任务复用 timers 统一调度（不引入 cron 库/外部 MQ）；发送复用 Channel 注册表与 notify_send outbox；时间线复用 timeline_entries append-only 约定。
- **领域纯函数**：severity 节奏解析、日报统计聚合逻辑放 domain/纯函数层，可单测；store 方法保持 `tx pgx.Tx` 入参约定。
- **配置边界清晰**：升级节奏是 escalation policy 的属性，运营任务开关/时刻是 team 级配置，不混入全局 config。

### Performance
- 悬置扫描与日报统计 SQL 走现有索引（alerts_status_idx / alerts_severity_idx / alerts_team_id_idx），单轮扫描不做全表遍历；日报统计限定时间窗查询。

### Security
- 日报/通报/播报内容对敏感 label 与通知目标脱敏，沿用现有敏感字段脱敏口径；不落敏感日志。
- 运营任务配置端点纳入现有 RBAC：修改配置 admin only，查看需登录。

### Reliability
- 周期任务 handler 严格幂等；单团队日报失败不影响其他团队（per-timer 独立事务既有语义）；错过轮次自恢复不补发。
- R1 向后兼容存量升级策略；R3 失败（时间线写入异常）不得阻断通知发送主链路的状态落库——同事务语义下按整体回滚重投处理，依赖幂等键收敛。

### Usability
- 升级节奏与运营任务配置在现有设置页扩展（沿用 AntD 表单模式），提供 severity 覆写的默认值建议（critical 3/10min、warning 60/120min、info 不升级——来源于 weaponx 生产参数，均可改）。
- 时间线通知条目文案中文、渠道与目标可读。
