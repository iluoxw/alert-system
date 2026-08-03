# 路由清单

| 路由 | 页面 | 权限 | 关键依赖 |
|---|---|---|---|
| `/login` | `Login` | 公开 | 登录 API |
| `/` | `AlertList` | 登录 | 告警列表、团队轻量列表、用户/团队名称映射 |
| `/alerts/:id` | `AlertDetail` | 登录 | 详情、时间线、统计、认领/恢复/移交/备注 |
| `/silences` | `Silences` | 登录 | 静默 CRUD、MatchersEditor |
| `/settings` | 重定向至接入 | 管理员 | AdminGuard |
| `/settings/integrations` | `Integrations` | 管理员 | 接入 CRUD、一次性 token |
| `/settings/teams` | `Teams` | 管理员 | 团队 CRUD、成员 Drawer |
| `/settings/users` | `Users` | 管理员 | 用户 CRUD |
| `/settings/routes` | `RoutesPage` | 管理员 | 路由 CRUD、MatchersEditor |
| `/settings/schedules` | `Schedules` | 管理员 | 值班表 CRUD、班次、轮换生成 |
| `/settings/escalation-policies` | `EscalationPolicies` | 管理员 | 升级链、SeverityConfigEditor |
| `/settings/notification-policies` | `NotificationPolicies` | 管理员 | matcher、三级渠道升级序列 |
| `/settings/ops-settings` | `TeamOpsSettings` | 管理员 | 团队运营任务配置 |

## 导航行为

- 主导航根据 pathname 选择“告警 / 静默 / 设置”。
- 设置 Tabs 根据路径尾段选择资源。
- 未登录统一跳转 `/login`；非管理员访问设置统一跳转 `/`。
- 未匹配路由统一跳转 `/`。
