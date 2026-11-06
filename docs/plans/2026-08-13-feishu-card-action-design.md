# Firebreak 飞书智能助手卡片交互设计（第一阶段）

## 边界

本阶段只实现 Firebreak 与飞书智能助手之间的“事件卡片 + 明确按钮操作”闭环：

- 不接入 Aily，也不把飞书当作告警存储或第二个 Alert 权威；
- Alertmanager 仍是 Alert 的唯一权威，Firebreak 只接收 webhook 镜像并负责关联 Fault、影响与 Incident；
- 飞书只承载通知、身份确认和低风险处置入口；
- 未来 AI Agent 可以复用同一个动作网关，但本阶段不引入模型、工具调用或自动决策。

## 统一动作信封

卡片按钮的 `value` 使用以下字段：

```json
{
  "resource_type": "alert | fault | incident",
  "resource_id": "uuid",
  "action": "ack | resolve | advance | mitigate",
  "request_id": "消息动作唯一键",
  "version": 1
}
```

Alert 额外保留 `alert_id` 字段，仅用于已经发出的旧卡片回调；新卡片和新代码以
`resource_type/resource_id` 为准。

动作回调地址为 `/callbacks/lark/card`。服务端依次执行：飞书验签 → `open_id` 映射到
启用中的 Firebreak 用户 → `request_id` 幂等占位 → 领域状态机 → 结果审计。动作占位、
领域变更和结果快照在同一个 PostgreSQL 事务内提交，失败会整体回滚，飞书重试不会产生
半成功记录。

## 当前支持的动作

| 资源 | 卡片动作 | 领域效果 |
|---|---|---|
| Alert | 认领 | `firing → acknowledged`，写 Alert 时间线并停止升级 |
| Alert | 解决 | 进入 `resolved`，写人工解决时间线，停止升级和生命周期定时器 |
| Fault | 推进故障 | `candidate → active → monitoring → resolved`，每次点击只推进一级 |
| Incident | 进入处置 | 使用 Incident 状态机进入 `mitigating`，写事件时间线并投递后续通知 |

卡片不再暴露尚未实现的“宣布事故”和“查看时间线”按钮。详情查看使用 `DetailURL`
链接；若没有配置详情地址，卡片只显示已实现的动作按钮。

## 身份与权限

管理员在“设置 → 用户”中维护飞书 `open_id`。未绑定或已禁用用户只能看到通知，不能
执行卡片动作。短期按用户绑定授权；后续可在动作网关增加团队/资源级 RBAC，而不改变
卡片信封和回调入口。

迁移 `000023_lark_card_actions` 创建动作审计表，并为 `users.lark_open_id` 建立部分
唯一索引。部署前必须正常执行数据库迁移。

## 后续 Agent 扩展点

未来 Agent 不应直接修改 Alert/Fault/Incident 表，也不应绕过飞书验签逻辑。推荐复用
动作网关的资源类型、动作名、request_id、操作者和结果审计模型，再增加服务端到服务端
的受控身份（例如 Agent ID + 策略授权），并保留人工确认和同一领域状态机作为最终闸门。

