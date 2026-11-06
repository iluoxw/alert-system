# 产品调研索引

本目录保存 2026-08-21 对快猫星云/Flashduty、PagerDuty 及当前 `alert-system` 的浏览器调研与对比报告。

## 报告

- [快猫星云（Flashcat Cloud / Flashduty On-call）分析报告](./2026-08-21-flashcat-flashduty-analysis.md)
- [PagerDuty 分析报告](./2026-08-21-pagerduty-analysis.md)
- [三方对比与产品建议](./2026-08-21-alert-system-comparison.md)

## 证据说明

1. 控制台部分使用应用内浏览器只读巡检；没有创建、保存、轮换、发送或删除任何产品数据。
2. Flashduty 现场租户没有可展示的故障数据，权限/套餐会使部分入口禁用；报告记录的是可见 IA、字段、默认值和交互。
3. PagerDuty 的 Incidents 页已现场验证；跨模块进入 Service Directory 时会话重定向到 identity 登录页，因此服务目录、值班和编排细节以官方文档/官方开源仓库为主，并在报告中标注边界。
4. 供应商核心后端没有公开源码；关于数据库、队列、分片和模型的描述只作为推断，不当作事实。
5. 报告不记录租户内部名称、用户信息、集成 key、APP key、hook URL 或任何凭据。
