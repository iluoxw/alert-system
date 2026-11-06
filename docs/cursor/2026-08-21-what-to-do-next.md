# 下周只改本仓库这三处假完成

本文不是「该向 Flashduty / PagerDuty 学什么」。那种清单谁都能写，也解释不了为什么你们已经有 `AlertMappings`、Fault 宣布门槛和 ops 渠道表，主路径还是说不清「通知了谁、谁认领了、谁在值班」。

现场对照只用来排除错抄。真正要动的是下面三条已经写进代码、但对外说法是假的路径。改完之前，不要开协作空间、不要做 inhibit 产品、不要做 PD 式 New Incident。

---

## 先停手：本仓库已经有、不要当缺口重做

| 别再立项 | 已经在哪 | 为什么重做是浪费 |
| --- | --- | --- |
| 告警映射待治理队列 | `web/src/pages/Settings/AlertMappings.tsx` | mapped / unmapped / conflict 队列和表单已在 |
| AM 来源禁止人工 Resolve | `web/src/pages/AlertDetail.tsx` | 「标记恢复」已按来源禁用 |
| Incident 宣布门槛 | `web/src/pages/FaultWorkspace.tsx` | 已要求 active Fault + confirmed `business_system` Impact |
| 团队渠道（kind + target） | `team_ops_settings.channels`，UI 在 `web/src/pages/Settings/TeamOpsSettings.tsx` | 日报/清扫/值班播报已经按 `{kind, target}` 发 |
| 空班不丢事件 | `internal/oncall/escalation.go` 的 `nobody_on_call` | 不要改成 PagerDuty「没人值班就丢」 |
| 导航按一级任务拆页 | `web/src/App.tsx` | 总览 / 故障 / 事故 / 告警 / 控制中心已拆，不要再平铺设置 |

inhibit 也不是 P0。`internal/pipeline/stage_inhibit.go` 是 `return nil` 占位；Alertmanager 权威源按不变量 14 本来就不该再跑本地抑制。把它做成产品卖点，会直接和已拍死的权威边界打架。

---

## 假完成 1：组通知把 Team UUID 当成飞书 `chat_id`

这是唯一一条**现在就会发错**的路径，优先于一切 UX。

`internal/app/outbox.go` 自己写着缺口：

- `teamNotifyChannel = "lark"`
- `NotifyGroup` / `NotifyRepeat` 的 `Target` 是 `alert.TeamID`
- `DispatchStep`（渠道升级）同样把 `alert.TeamID` 当 target
- 注释原文：「team 还没有渠道联系方式表」

这句话已经过时。表和 UI 都在：

- 存：`team_ops_settings.channels`（`internal/store/ops.go` / `internal/ops/settings.go`）
- 发：`internal/ops/handlers.go` 的 `sendToChannels` → `ScheduleOpsSend`，用的是 `ch.Kind` + `ch.Target`
- 配：`TeamOpsSettings` 已支持 `lark` / `lark_webhook` / `webhook` / `email`

`internal/notify/channel_lark.go` 对无前缀 target 按 `chat_id` 发送。组通知/重复提醒因此会拿团队 UUID 去调飞书。人员升级走 `ContactResolver`（`users.email` / `users.lark_open_id`）是通的；**团队维度三条意图是断的**。CHANGELOG 仍写「飞书真实发送 HOLD」，和 `channel_lark.go` 真 HTTP 发送并存——文档和代码已经互相拆台。

### 这一周怎么改（只动装配，不新建对象）

不要新建「团队联系人表」，也不要引入 Flashduty Channel。

1. `notifyOutbox.NotifyGroup` / `NotifyRepeat` / `DispatchStep` 读该告警 `TeamID` 的 `team_ops_settings.channels`，按 ops 同一套 `{kind, target}` 展开 `scheduleSend`。
2. 团队未配渠道或 `channels = []`：不调度团队维度发送，时间线写 `skipped: no team channel`。禁止再静默用 TeamID 顶。
3. `DispatchStep` 的政策 `channels` 是渠道种类（email/im），不是 chat_id。种类命中后，target 仍必须来自 ops 渠道表，不能继续塞 TeamID。
4. 测现有 outbox 单测：有渠道则 payload.target 是 webhook/open_id/chat_id；无渠道则零条 `notify_send`。不要为了绿而保留 UUID target。

改完之前，控制中心「通知策略 / 渠道升级」和「团队运营渠道」是两套互不相认的配置。运营按页面填了群机器人，告警仍对 UUID 发——这不是竞品差距，是本仓库自己的裂缝。

---

## 假完成 2：飞书卡片已经能 Ack，但写不进 `acked_by`

`internal/api/lark_callback.go` 已验签、已走与 REST 同一套 stopper。注释写明：

> actorID 传空字符串……没有「飞书身份 -> 内部 users」映射，`acked_by` 只能落 NULL。

`users.lark_open_id` 从迁移 000007 就在。卡片点了「认领」之后：

- 时间线有一条 OpenID
- `alerts.acked_by` 仍是 NULL
- Web 详情的响应态和「谁认的」对不上

这不是「还没做飞书」。这是半套已经接到生产回调形状上的身份洞。UAT 里任何人点一张卡片，协作状态就会和 REST 认领分叉。

### 开会 15 分钟必须先选，再写代码

**选 A（这个月要给真人点卡片）**：回调里用 `action.OpenID` 查 `users.lark_open_id`，查到才调用 `domain.Ack(..., userID, displayName, now)`；查不到则 200 + 时间线 `ack_unmapped_openid`，**不要**把响应态改成 acknowledged。用户页已经能填 `lark_open_id`，缺的是这一跳查询。

**选 B（这个月不演示卡片认领）**：卡片按钮改为「已阅 / 打开详情」，Ack 只留 Web；或回调只写时间线、不调 `Ack`。同时改掉卡片成功文案里任何「已认领」字样。

不要第三种：继续空 `actorID` 调 `Ack`。那会制造「飞书已认领、库里没人」的脏数据，后续再补映射也洗不干净。

禁止借这个需求做 Flashduty「频道内看见即可认领」。我们没有 Channel 可见性模型，不变量 6 的处置幂等是按登录用户设计的。

---

## 假完成 3：侧栏「当前值班」是写死的角色文案

`web/src/App.tsx`：

```text
当前值班 · {data?.is_admin ? 'Platform SRE' : '值班成员'}
```

这不是 oncall resolver。admin 永远看见「Platform SRE」，其他人永远看见「值班成员」。`OperationsOverview.tsx` 里搜不到「值班」。`TeamOpsSettings` 的值班播报会把真实班次发到群，侧栏却对登录用户撒谎。

班次物化已经在 `web/src/pages/Settings/Schedules.tsx`（`starts_at` / `ends_at`）。空班行为已经在升级链里。缺的不是「学 PagerDuty 做当前值班员首页」，而是不要在一级壳上挂假数据。

### 这一周只做诚实的一种

二选一，不要两个都做：

- **30 分钟**：删掉这行，改成用户名 + 团队名，或「值班状态见排班」。撒谎比空白更贵。
- **接现成 resolver**：对用户所属团队调已有 Current()，显示真名；空班显示「当前无人值班」，与 `nobody_on_call` 同一口径。不要新做值班首页。

一等 override（休假/顶班）排在这三处假完成之后。现在班表只能靠改 rotation 底表，但那是真缺口，不是假完成；先修撒谎，再补能力。

---

## 唯一值得从详情页补的一句话（不是新功能）

`AlertDetail` 已有映射卡和三轴状态，缺的是对运营说清楚权威旁路：

> 本条来自 Alertmanager。本地路由 / 分组 / 静默 / 抑制未执行。要静默请去上游，或到「静默」页理解它只管非权威源。未映射请到控制中心「告警映射」。

从现有 unmapped 卡链到 `/settings/alert-mappings` 即可。不要为这个开 decision-trace 大项目；trace 是 P1，在组通知 target 修正之后才有运营意义。

---

## 明确不要做的（对照过现场之后）

这些在三套调研里都好看，对**当前这个 HEAD** 有害：

1. 把首页改成 Flashduty Channel 或 PagerDuty Incidents inbox。总览已经按基础设施 / Fault / confirmed Impact / Incident 排，这是 ADR 不是过渡 UI。
2. 自动从 Alert 建 Incident。宣布门槛已经写在 Fault 工作台。
3. 给 AM 源实现本地 inhibit / 智能聚合。stage 是 stub 是有意的。
4. 新建团队联系人模型或协作空间对象。`team_ops_settings.channels` 就是你们选过的形态。
5. 把「当前值班 · Platform SRE」做成可配置文案。那是在保养谎言。
6. 在 CLAUDE.md 再抄一遍 P0 五条而不改 `outbox.go`。下一次对照 Agent 仍会写出同一份通用建议。

---

## 建议的提交切分（方便和其他 Agent 的文档对照）

1. **commit 1**：`internal/app/outbox.go` 团队目标改走 `team_ops_settings.channels`；无渠道则跳过并写时间线。这是唯一能让「通知了团队」这句话重新成立的改动。
2. **commit 2**：按会上选的 A 或 B 改 `internal/api/lark_callback.go`（及已有「actorID 为空」的测试，那些测试今天是在固化漏洞）。
3. **commit 3**：`web/src/App.tsx` 去掉假值班文案，或接到 Current()；可选在 `AlertDetail` 加那一行权威说明。

三笔都落地之前，对比报告里的「学习值班 UX / 学习认领 / 学习群通知」继续没有验收对象。
