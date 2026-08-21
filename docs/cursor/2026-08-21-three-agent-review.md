# Codex / Cursor / Grok 三套调研报告对照

评审日期：2026-08-21  
材料：

| Agent | 分支 | 目录 |
| --- | --- | --- |
| Codex | `origin/codex/flashduty-pagerduty-research-20260822` | `docs/codex/` |
| Cursor | `cursor/flashduty-pagerduty-research-1812` | `docs/cursor/` |
| Grok | `origin/grok/flashduty-pagerduty-research-20260822` | `docs/grok/` |

本篇只评三套**报告质量**和应采纳的**后续决策**，不再重写 Flashduty / PagerDuty 产品说明书。

---

## 1. 先给结论

三套报告在战略层几乎同案：

- 不要把 SRE Control 改造成 Flashduty 协作空间或 PagerDuty Service inbox。
- 坚持 `Alert ≠ Fault ≠ Incident`，Alertmanager 拥有 Alert 生命周期。
- 两家该借的是降噪/分派/值班的**可运营 UX**，不是自动建 Incident，也不是在 AM 源上再做一层静默/抑制/智能聚合。

质量不在「谁写得更长」，而在三件事：

1. **证据纪律**：现场没打开的页面有没有写成「已验证」。
2. **对照深度**：有没有钉到本仓库真实代码，而不是只复述 CLAUDE.md。
3. **建议是否可拍板**：是功能愿望清单，还是带 A/B 的产品决策。

按这个尺子：

| 维度 | 最强 | 说明 |
| --- | --- | --- |
| 战略护栏（别抄错世界观） | Cursor ≈ Grok | 首页/对象根/自动建单写得最干净 |
| 证据边界与「没看见就不写有」 | Grok > Codex > Cursor | Grok 每份末尾有材料缺口；Codex 把 PD 会话丢失写进边界 |
| 控制台现场覆盖 | Cursor ≈ Grok > Codex | Codex 进不了 PD Service Directory，核心模型改靠文档 |
| 本仓库代码对照 | Grok > Codex > Cursor | Grok 钉到 outbox / 三轴 / mapping 队列；Codex 钉到 pipeline 与 inhibit 占位 |
| 可执行决策 | **Grok** | P0 五条是「选 A 还是 B」，不是「建议补齐值班」 |
| 工程缺口优先级 | **Codex** | decision trace、authoritative bypass 提示、unmapped 队列 |
| 可读性与决策者友好 | Cursor | 短、表多、一句话定位清楚；代价是实现裂缝写浅 |

**后续不要三选一当唯一真源。** 用 Cursor/Grok 的世界观当红线，用 Grok 的 P0 决策当产品拍板清单，用 Codex 的管道解释/抑制边界当工程补丁。Cursor 原文里「New Incident 表单字段」「管道有 inhibit」两处应降级，以 Grok/Codex 为准。

---

## 2. 三套报告各自画像

### Codex（约 0.8 万字）

**强处**

- 证据分层写得专业：现场 / 文档 / 开源 / 推断分开，README 明确不写凭据和租户名。
- 对本仓库对照最「工程师」：直接引用 `pipeline.go` authoritative-safe、`stage_mapping.go`、timer kinds、`App.tsx` IA。
- 抓到一个可验证的实现事实：`InhibitStage.Process` 恒返回 nil，不能对外宣称已有根因抑制。
- 建议里有一条其它两家写浅的：**每条 Alert/Fault 的 pipeline decision trace**（命中/跳过/规则版本/下一 timer）。这和两家控制台「为什么没通知我」的产品语言对齐，也是自托管可运营性的缺口。

**弱处**

- PagerDuty 现场在进 Service Directory 时掉登录，服务/EP/通知规则几乎全靠文档。Cursor/Grok 都进了服务详情和用户通知规则，Codex 在 PD 内核上现场证据最弱。
- 写错过 Flashduty Terraform：官方有 `flashcatcloud/terraform-provider-flashduty`，Codex 写成「未发现等同官方 provider」。
- 本地绝对路径 `/Users/luoxiaowei/code/go/src/alert-system/...` 不该进仓库文档。
- P0「实现完整 inhibit」没有先写清 **AM 权威源必须继续跳过**。按 CLAUDE 不变量 14，在 AM 源上补 inhibit 是错方向。这条应收窄为：仅非 AM 兼容路径，且主 UX 不要做成和 AM silence 对等的开关。

### Cursor（约 0.7 万字）

**强处**

- 对照报告最适合给人读：术语表、三种配置根、能力矩阵「第一阶段该不该追」一眼能用。
- 现场覆盖面宽：Flashduty 空间/集成/AI SRE/状态页/用量；PagerDuty 服务四 Tab、Standards 1/8、通知橙条、EP Always on-call、加购墙。
- 战略句最硬：抄自动 Incident = ADR-0001 作废；首页继续回答基础设施健康，不改成频道墙或 inbox。
- 明确用 ADR 后模型，而不是 MVP 初稿的 PagerTree 对标。这一点 Codex 稍弱（仍大量写「当前项目少了供应商外围」）。

**弱处**

- 对本仓库实现裂缝写得不够：`notify` 的 target 用 `alert.TeamID`、飞书卡片写不进 `acked_by`、inhibit 占位，这三件 Grok/Codex 有、Cursor 原文几乎没有。
- PagerDuty「New Incident 表单字段」写得很具体；Grok 同租户记录 CTA 裂开、对话框没打开。两份现场冲突时，**字段清单不得当已核验**。
- 「管道有 inhibit」未标明占位，读者会以为已具备 Flashduty 那种抑制。
- README 只有目录，没有方法边界。Grok/Codex 的「没打开的面板用文档补」更好。
- 建议偏 UX 清单（on-call now、快速静默、空账号引导），缺少「选 A 还是 B」的拍板结构。

### Grok（约 2.5 万字）

**强处**

- 材料纪律最好：每份分析末尾有「材料缺口，禁止写成产品没有」。Pass 1 没进的降噪五块、Schedule 创建表单、PD New Incident，都明确降级。
- 术语防撞车最细：Channel（FD 协作空间）≠ `notify.Channel`（运输层）；Service（PD 路由单元）≠ 我们的服务目录；Urgency ≠ Severity ≠ Priority。
- 对照钉到代码：`outbox.go` 用 TeamID 当群目标、卡片 HMAC 后 `acked_by` 保持 NULL、mapping 未映射进治理队列、`nobody_on_call` 写时间线而不是丢事件。
- **建议是决策表，不是路线图愿望。** P0-1～P0-5 每条有选项、建议、依据。这是三套里唯一能直接开会拍板的部分。
- 正确反对「为功能对称在 AM 源上做 FD 静默 / PD Intelligent Grouping」。

**弱处**

- 过长。决策者读 §1 + §12 + §13 即可，中间大量矩阵会淹没拍板句。
- 引用内部 scratch（`/workspace/research/*.md`）不在本仓库，复现成本高。
- 个别租户标识写进正文（Org ID、User ID）。Codex 选择不写，更稳。
- 「领先清单」12 条略满。其中 E2B 垂直闭环、自托管无套餐阉割是真优势；「已经领先电话 SaaS」这类语气可以收。

---

## 3. 事实冲突与应采信哪边

| 冲突点 | Codex | Cursor | Grok | 采信 |
| --- | --- | --- | --- | --- |
| PD Service Directory 是否现场打开 | 否，被踢回登录 | 是，并写了四 Tab / Graph | 是，并列出短 ID | Cursor/Grok。Codex PD 服务细节当文档 |
| PD New Incident 字段 | 未声称打开 | 列出 Title/Type/Service/Urgency… | 明确没打开，CTA 无 href | **Grok**。Cursor 字段降为未核验 |
| Flashduty Terraform | 写成没有官方 provider | 提到有 | 写了 12 个资源 | Grok/Cursor |
| 本地 inhibit | 占位、恒 nil | 「管道有 inhibit」 | 阶段存在且 AM 源跳过 | **Codex + Grok**：存在但是 stub；AM 源禁止当主能力 |
| 群通知 target | 未写 | 未写 | `TeamID` UUID 当 target | **Grok**，可用 `internal/app/outbox.go` 复核 |
| 飞书 Ack 是否写入 `acked_by` | 未写 | 未写 | 只进时间线，UUID 保持 NULL | **Grok** |
| 没人值班时 | 未强调 | 默认团队兜底 | `nobody_on_call` 留洞 | **Grok** 更精确；与「禁止静默丢弃」不矛盾 |
| 是否该做完整 inhibit 为 P0 | 是 | 未列为第一优先 | 明确不要在 AM 源做 | 收窄：非 AM 兼容路径可做；**不是**产品主 UX |

---

## 4. 三家建议重叠（已可当共识，不必再吵）

下面这些三套都赞成，后续直接当红线：

1. 不自动把 Alert/聚合结果宣布成 Incident。
2. 不在 AM 权威源上再跑本地 route/group/silence/inhibit/Alert 级通知。
3. 不用协作空间或 PD Service 替换 Entity 图当产品根。
4. 首页继续是运行总览 / Fault，不做 inbox 或频道墙。
5. AI 不进管道、不占一级导航、不可宣布 Incident / 改 Impact。
6. 不追 750 集成、公开状态页、语音 SaaS 来「功能对等」。
7. 学 PD：升级策略、值班表、个人通知偏好拆开；手建 Incident 不走 ingest hook。
8. 学 FD：事件聚合 ≠ 告警聚合；红点不数已关闭；控件旁写清「为什么不通知」。
9. Runbook 继续白名单，不学「自定义 action 目录」放松不变量 12。

---

## 5. 后续：明确该拍的决策（合并后的唯一清单）

只保留现在不拍就会在生产里说不清的条目。实现任务另立项，这里只拍产品。

### P0（本迭代必须写进 CLAUDE / ADR，再动代码）

**P0-1 团队触达身份（采 Grok）**

- 选 A：Team 有一等联系人（飞书 `chat_id` / webhook / 邮件别名），group / repeat / channel-escalation 打真实目标。
- 不选 B：继续用 `alert.TeamID` 假装是群。
- 不先做「协作空间」对象。脊柱仍是 Fault。

**P0-2 飞书卡片 Ack 的法律含义（采 Grok）**

- 必须先选：A) OpenID 映射到本地 user 才能写 `acked_by`；或 B) 卡片只记时间线，认领以 Web 为准。
- 禁止默认为 Flashduty「看见即可」——我们没有 Channel 可见性模型。

**P0-3 空班行为写死（采 Grok，拒绝 PD）**

- 维持 `nobody_on_call` 时间线警告，事件留下。
- 禁止改成「丢且不可审计」。
- 群聊是否仍发：等 P0-1 有真实群目标后再加，作为 A 的附加，不是第三种默默行为。

**P0-4 一等 override（采 Grok / PD）**

- 休假/顶班走 override 窗口，不改底表 rotation，也不靠「把人从通知规则里删掉」。

**P0-5 详情页必须解释权威边界（采 Codex）**

- AM 来源 Alert/Fault 详情显式写：本地 route/group/silence/inhibit **未执行**及原因。
- 未映射 / mapping conflict 进待办，禁止静默落到万能默认团队还不告警。
- 每条 Alert 提供 stage 命中/跳过/下一 timer（decision trace）。这是运营闭环，不是装饰。

### P1（主路径不挡，但决定形状）

1. **inhibit**：只做非 AM 兼容路径的可解释抑制；AM 源继续跳过。不要当 P0 产品卖点。
2. **urgency / 班后打扰**：可从 severity 拆一个策略旋钮；不要引入 PD Priority / SEV-1 第三套。
3. **个人通知规则**：要么只做团队矩阵，要么「跟随个人 / 跟随统一」二选一；禁止一次上 PD 六段 + FD 四场景。
4. **Ack timeout**：策略级、默认关；Snooze 晚于 override，且 ≠ `/silences`。
5. **电话/短信**：政策 UI 先不露出；Validate 拒绝保持，直到有明确电话产品。
6. **升级策略热改**：开火时快照（学 PD），不要让已开火 timer 默默读最新 JSON。
7. **标准 Events 信封**：若加，机器 trigger/resolve 走 hook；人声明 Incident 走 REST。
8. **空账号最小闭环**：一个 AM Source + 一条 Mapping + 一个默认团队 + 当前值班人可见。学 PD Default Service+EP 的「十分钟能看见谁值班」，不学自动建 Incident。
9. **快速静默**：从当前 Fault/Alert 生成 matcher，列出被丢弃标签及原因（学 FD）。
10. **MTTA/MTTR**：若做，口径挂 Fault/Incident，不挂 AM Alert inbox。

### P2（承认缺口，不排进当前主线）

- 公开状态页、复盘产品、Insights SKU、SSO/SCIM、Mobile/Push、公平轮换/iCal、钉钉/企微/Slack、开放 MCP/Terraform。
- LLM 只挂 mapping 治理队列的建议，不挂通知摘要。Embedding 保持永久排除。

---

## 6. 怎么用这三套文档

给不同读者三份主读，避免再读 4 万字：

| 目的 | 主读 | 辅读 |
| --- | --- | --- |
| 统一对象语言、防抄错 | Cursor 对比 §1–§4 | Grok 对比 §2–§3 |
| 开会拍 P0 | **本文 §5** + Grok 对比 §12 | — |
| 排工程（trace / inhibit / 缓存提示） | Codex 对比 §6、§12、§15 | `internal/pipeline/stage_inhibit.go`、`internal/app/outbox.go` |
| 核对 Flashduty 降噪/分派字段 | Grok Flashduty §5–§7 | Codex Flashduty §6–§7 |
| 核对 PagerDuty 服务/通知现场 | Cursor PagerDuty §4–§5 或 Grok PagerDuty §2–§5 | Codex 仅文档部分 |

Cursor 对比报告继续适合当对外摘要；Grok 对比报告当内部决策底稿；Codex 当工程备忘。不要再开第四份同等长度的产品说明书。

---

## 7. 一句话

Codex 最像严谨的现场+代码备忘录，但 PD 现场断了、有一处开源事实写错，inhibit P0 需要收窄。Cursor 最像给负责人看的战略对照，世界观对，实现裂缝和一处 PD 表单证据偏满。Grok 最像能直接开会的决策书，也是唯一把「团队群目标 / 卡片 Ack / override / 空班」写成必须二选一的报告。

后续只做一件事：把本文 §5 的 P0 五条写进 CLAUDE/ADR，再按那五条改代码。功能对等表可以停了。
