# 快猫星云 Flashduty 产品分析报告

- 调研日期：2026-08-21（Asia/Shanghai）
- 读者：正在做 SRE Control（`iluoxw/alert-system`）的人。目标不是写一篇功能清单，而是把 **产品怎么被设计出来、控制台怎么被实现出来** 钉死到可引用的事实。
- 材料纪律：只使用两份已读材料，不补拍、不编数字、不编截图。
  - 公开文档与开源仓库：`/workspace/research/flashcat-docs-oss.md`（未登录控制台；未 clone 仓库）
  - 已登录控制台实勘 Pass 1：`/workspace/research/flashduty-console-pass1.md`（组织 **SREMonitor**，Owner，Org ID `2618591231671`，UI 语言 English，夹杂残留中文）
- 方法边界：Pass 1 只做导航 / IA 盘点 + Channel 深潜；未创建、未删除、未保存任何对象。分派规则抽屉打开过一次做字段采集后取消。未打开的面板（事件聚合、告警聚合、抖动、静默、抑制、排除、集成向导、故障详情、RUM 子页、AI SRE 子页等）一律用文档补，并标明「控制台未核验」。
- 术语先钉死：**Channel / 协作空间 ≠ 通知媒介**。控制台路径 `/channel`、Terraform 资源 `flashduty_channel`、文档「协作空间」是同一对象：服务 / 团队的告警管道与协作容器。真正的通知渠道是 App / 语音 / 短信 / 邮件 / IM 应用或机器人，挂在分派策略的 Notify by、个人偏好、以及 Integrations → Instant messages 上。

---

## 1. 产品定位与族谱

快猫星云（Flashcat）对外自称「智能时代的全栈可观测性解决方案提供商」，覆盖指标、日志、APM、RUM、监控告警和 On-call，并把 AI 放在故障发现、根因分析、事件响应这条故事线上（[flashcat.cloud](https://flashcat.cloud/)）。真正卖货时，它拆成三条商业线和两条开源线，而不是一个产品五个皮肤。
