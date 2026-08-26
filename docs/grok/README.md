# 产品调研索引（Grok）

本目录保存 2026-08-21 对快猫星云 / Flashduty、PagerDuty，以及当前项目 SRE Control（`iluoxw/alert-system`）的产品调研与对照。

材料来自：公开文档与开源仓库、已登录控制台只读实勘、以及 GitHub connector 读取的 `alert-system` 源码。没有创建、保存、删除或发送任何产品数据。

## 报告

- [Flashduty 分析报告](./2026-08-21-flashcat-flashduty-analysis.md)
- [PagerDuty 分析报告](./2026-08-21-pagerduty-analysis.md)
- [三方对比报告](./2026-08-21-alert-system-comparison.md)

## 怎么读

1. Flashduty 报告先钉术语：Channel / 协作空间不是通知媒介；夜莺负责生成，Flashduty 负责收口。
2. PagerDuty 报告按 Operations Cloud 拆面：IR / AIOps / Advance / Automation / Status Pages。Urgency、Priority、Severity 不要混。
3. 对比报告以 SRE Control 自己的论点为准：Alertmanager 是 Alert 生命周期的唯一权威；Fault ≠ Incident。

## 方法边界

1. 控制台只做导航 / IA / 字段采集，未改任何对象。
2. Flashduty 租户 `SREMonitor` 做了 Channel 深潜；未打开的面板用文档补，并在正文标明「控制台未核验」。
3. PagerDuty EU 租户试用已过期。IR 主路径仍可看；AIOps / Advance / Automation Actions 等多处变成 upsell 整页。New Incident 对话框未能打开。
4. 供应商核心后端没有公开源码。数据库、队列、分片、模型的描述只作为实现推断，不当事实。
5. `alert-system` 对照基于仓库 tree `c42f7c270da493b1cc2ecb1b7f016722854ba0d7`（当时的 `main`）。
