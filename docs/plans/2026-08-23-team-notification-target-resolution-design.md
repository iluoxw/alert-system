# 团队通知目标解析设计

## 背景与目标

`NotifyGroup`、`NotifyRepeat`、`DispatchStep` 和风暴摘要当前把 Team UUID 直接写入 `notify.SendRequest.Target`。飞书渠道会把无前缀 target 当作真实 chat ID，因此这条路径会向错误目标发送。仓库已经通过 `team_ops_settings.channels` 保存团队级 `{kind,target}`，本次复用该唯一配置来源，不新增平行的渠道表。

目标是在通知意图进入 `notify_send` outbox 前解析出真实 target，同时保持 Alert、Fault、Incident 的现有世界观、人员升级与渠道升级正交关系，以及 `notify.Sender` 的幂等语义。

## 方案比较与选择

1. **意图创建时解析（采用）**：在调用 `scheduleSend` 的现有事务中读取团队渠道，按请求的 channel kind 生成一个或多个具体 `SendRequest`。payload、timer ref ID 和 Sender 幂等键都固化真实 target；配置变更只影响后续意图。
2. **Sender 发送时解析（拒绝）**：payload 继续携带 Team UUID，由 Sender 临发送前查配置。这样在途意图会随配置漂移，timer ref ID 与最终发送 target 不一致，也把团队解析和现有 `TargetUserID` 到人解析混在一起。
3. **新增 team_channels 表（暂缓）**：长期可以把通知身份从运营任务设置中拆成一等对象，但当前会与 `team_ops_settings.channels` 形成双写和迁移成本，不是修复错误发送的最小方案。

## 数据流与降级

- Group、Repeat 与 Storm 保持现有团队默认渠道 `lark`，但只向设置中 kind 为 `lark` 的具体 targets 发送。
- `DispatchStep` 把策略中的 channel kinds 与团队配置逐 kind 求交集；一个 kind 可映射多个 targets，精确重复项去重。
- `ScheduleOpsSend` 已携带具体 `ops.ChannelTarget`，不再解析。
- 团队未保存设置、channels 为空、或某个请求 kind 无 target 时，不再用 Team UUID 兜底，也不阻断 repeat/人员升级/后续渠道升级。对关联告警追加 `notify_skipped` 时间线，detail 固定包含 `status=skipped`、`reason=no_team_channel`、channel、team ID、step 与 notification kind。
- Storm 没有活跃告警可关联时，以结构化日志记录跳过；存在活跃告警时按告警扇出时间线。
- 查询渠道失败属于基础设施错误，返回 error 让当前事务回滚并进入现有 scheduler 失败路径，不能伪装成“未配置”。

## 组件边界

- `notifyOutbox` 新增窄接口读取团队渠道，以及窄接口追加时间线；不直接依赖 HTTP API。
- `opsSettingsRepo` 适配 `store.OpsRepo`，把 `store.ErrNotFound` 转成空渠道，把 store 类型映射为 `ops.ChannelTarget`。
- Storm 复用同一个 `notifyOutbox` 团队发送方法，避免重新实现 target 解析和跳过审计。
- `notify.Sender`、人员 `ContactResolver`、通知策略匹配和 `team_ops_settings` API 均保持不变。

## 验收

- 四条团队通知路径的 payload 不再出现 Team UUID 作为 target。
- 多 target、部分 kind 缺失、全缺失、重复配置、仓储错误均有单测。
- 缺渠道时没有 `notify_send`，但关联告警出现可读的 `notify_skipped`；其他升级链继续运行。
- E2E seed 通过真实 ops-settings API 配置 mock lark/email targets，并断言 mock 收到的 target 是配置值而不是 Team UUID；动态降噪团队也各自配置独立 lark target。
- `go test ./...`、相关 E2E、前端 lint/build 与 `git diff --check` 通过后才能推送。
