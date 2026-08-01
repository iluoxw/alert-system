# Design Document — aiops-foundation

## Overview

本设计落地六项需求：四项确定性降噪（窗口全归并、模板归并、风暴熔断、flapping 防抖）、一项数据地基（处置记录）、一项纯设计交付（AI enrichment 框架章节，见本文末章）。

三条总设计原则：

1. **全部 opt-in，零默认行为变化**：新配置的零值/缺省等于「关闭」，存量路由与告警的行为逐字节不变。
2. **不新增管道 stage、不改管道顺序**：降噪逻辑全部内嵌到既有 stage 的既有钩子（GroupStage 的组键计算、timer 注册、flush 前判定），dedup→route→group→silence→inhibit 不变量原样保持。
3. **通知仍只经既有意图接口与 outbox**：风暴摘要、flapping 提示都不引入新的发送路径，复用 NotifyGroup → notify_send 的既有链路，幂等键口径（不变量 4）不动。

## Steering Document Alignment

### Technical Standards（以项目 CLAUDE.md 核心不变量为准）

- 管道顺序不变量 1：不新增 stage（见上）。
- timer kind 不变量 2：新增第 11 个 kind `storm_summary`，在 internal/app 注册 handler。
- scheduler 事务模型不变量 3：storm_summary handler 签名带 tx、不自开事务、幂等（DELETE/UPDATE 天然幂等 + ScheduleIfAbsent 收敛）。
- outbox 不变量 4：风暴摘要通知在持锁事务内只 Schedule notify_send；round 意图时刻捕获随 payload 持久化。
- 不变量 5：window 全归并模式是既有组机制的第三种组键计算方式，不是 directDispatch。
- 不变量 6：Resolve 幂等口径不动，note 在幂等短路时丢弃。

### Project Structure

归一化纯函数进 `internal/domain/`；storm/flap 判定逻辑进 `internal/pipeline/`（stage 内嵌）与 `internal/store/`（仓储方法带 tx）；storm_summary handler 与装配进 `internal/app/`；迁移 000008 顺延。

## Code Reuse Analysis

### Existing Components to Leverage

- **GroupStage 三个钩子点全部复用**（stage_group.go）：组键计算（:112-121，加第三分支）、ScheduleIfAbsent/HasFired 编排（:130-141，flapping 跳过点）、Handle flush（:150-162，风暴判定点与成员过滤点）。
- **scheduler.ScheduleIfAbsent**：storm_summary 周期续期的 pending 收敛防抖，与 group_wait 同一机制。
- **notify outbox（internal/app/outbox.go）**：风暴摘要作为一种特殊「组通知」走 NotifyGroup 实现同款 Schedule notify_send 手法。
- **domain timeline**：flapping 进入/解除、resolution note 都是新的 timeline entry kind，复用既有 entry 结构与展示。
- **domain.Fingerprint 的 FNV+分隔符手法**（fingerprint.go）：TemplateFingerprint 同款实现模式。

### Integration Points

- 迁移 000008（routes 两列、alerts 四列、route_storm_state 表、索引）。
- internal/app 注册 storm_summary handler；config 新增 FLAP_* 两项。
- 前端：Resolve 弹窗加可选说明输入；路由编辑表单加分组模式与风暴阈值；告警列表/详情 flapping 徽标。

## Architecture

```
ingest → dedup(flap 翻转计数) → route(写 alerts.route_id) → group ──┬─ 组键: label | window | __template__
                                                                    └─ flapping 告警跳过 timer 注册
group_wait 到期 → GroupStage.Handle ── 风暴判定(route 近 1min 计数)
        │  未超阈值: NotifyGroup(过滤 flapping 成员) → notify_send（现状）
        └─ 超阈值/已熔断: 累计 suppressed_count + ScheduleIfAbsent(storm_summary)
storm_summary 到期 → 摘要 notify_send + 速率复查 → 续期 或 解除(补组 timer)
```

关键裁决与理由：

- **风暴判定放在组 flush 时刻而非 ingest 热路径**：ingest 每条告警都判定会引入写放大与额外查询；组 flush 是通知的必经收口且频率天然被 group_wait/group_interval 稀释。代价是「熔断从超阈值到生效有最多一个 group_wait 的滞后」——可接受，风暴场景下积压的正是 flush。
- **给 alerts 补 route_id 列**（迁移 000008，route stage 写入）：风暴计数需要「该路由近 1 分钟新告警数」，用 group_key LIKE 前缀既慢又脆；route_id + created_at 复合索引一查即得。同时偿还「告警无法按路由统计」的欠账。存量行 route_id 为 NULL（不回填，风暴判定只看新告警）。
- **flapping 只做「不驱动 timer + flush 过滤」的最小侵入**：不引入新通知意图接口。置位当次仍随组正常通知一次（消息中对 flapping 成员标注「抖动中」），之后窗口内该告警不再注册组 timer、flush 成员列表里被过滤——「收到一次提示、不再逐次轰炸」由此达成。

## Components and Interfaces

### C1 窗口全归并分组（R1）

- routes 新列 `group_mode TEXT NOT NULL DEFAULT 'label'`，取值 `label`（现状）/ `window`。
- GroupStage 组键三分支：`window` → `groupKey = routeID + "-window"`（固定键，与 `-single-`/`%016x` 格式不碰撞）；`label` 且 group_by 空 → 单例组（现状）；`label` 且非空 → computeGroupKey（现状）。
- 固定组键天然获得窗口语义：组首 group_wait 等待收拢窗口内全部告警，其后 group_interval 节流——零新增编排代码。
- API 校验：`group_mode='window'` 时 group_by 必须为空（422，防止表意冲突）；路由 CRUD 与前端表单加该字段。
- 聚合摘要展示复用既有组通知内容（Message 已含 GroupKey/AlertIDs 条数）。

### C2 模板指纹与 `__template__` 分组键（R2）

- `internal/domain/template.go`：`func TemplateFingerprint(text string) string`——单遍 token 归一（IPv4、IPv6、UUID、≥8 位十六进制串、连续数字 → `<*>`），FNV-64a 哈希取 `%016x`。纯函数、表驱动全覆盖单测；不含变量的文本原样哈希（AC2.1）。
- 接入：group_by 支持保留键 `__template__`。GroupStage 组键计算时特判该键取 `TemplateFingerprint(alert.Title)` 而非 label 值（Title 是归一信封的标准字段，alert.go:102）。computeGroupKey 签名扩为接收「键→值」解析函数或预展开的取值表，实现阶段择小者。
- API 校验放行 `__template__`；文档注明它与普通 label 键可混用（如 `["__template__","cluster"]`）。
- opt-in：不配置该键时无任何路径触碰 TemplateFingerprint（AC2.3）。

### C3 风暴熔断（R3）

- routes 新列 `storm_threshold_per_min INTEGER NOT NULL DEFAULT 0`（0=关闭）。
- 新表 `route_storm_state(route_id UUID PRIMARY KEY, active BOOLEAN NOT NULL, activated_at TIMESTAMPTZ NOT NULL, deactivated_at TIMESTAMPTZ, suppressed_count INTEGER NOT NULL DEFAULT 0, last_summary_at TIMESTAMPTZ)`——进入/解除时刻即「可查询记录」（AC3.2），行保留不删，重复进入时 UPSERT 覆盖。
- 判定（GroupStage.Handle，flush 前）：threshold>0 时 `SELECT count(*) FROM alerts WHERE route_id=$1 AND created_at >= now()-'1 min'`（走 000008 的 (route_id, created_at) 索引）。超阈值或 state.active：跳过 NotifyGroup，`suppressed_count += len(members)`，`ScheduleIfAbsent(storm_summary, routeID, now+1min)`。
- `storm_summary` handler（第 11 个 timer kind，internal/app 注册）：
  1. 读 state；suppressed_count>0 → 经与 NotifyGroup 实现同款的 outbox 手法 Schedule 一条 notify_send 摘要（累计条数、时间范围、代表样本标题、详情链接；幂等键 `routeID|storm|channel|target|round`，round 意图时刻捕获随 payload 持久化）；计数清零。
  2. 速率复查：仍超阈值 → 续期 +1min；回落 → `active=false, deactivated_at=now`，并对该 route 下活跃（非 resolved）告警的 distinct group_key 逐个 `ScheduleIfAbsent(group_wait, group_key, now+group_interval)` **补一轮组 timer**——保证被熔断吞掉的组通知最终送达（不丢数据也不丢最终可达性；风暴多为同质告警，distinct 组数有限）。
- handler 全程使用 scheduler 传入的 tx，UPDATE/ScheduleIfAbsent 幂等，满足 at-least-once。

### C4 Flapping 防抖（R4）

- 全局 config 两项：`FLAP_WINDOW_SECONDS`（默认 0=关闭）、`FLAP_THRESHOLD`（窗口内翻转次数）。选全局而非 route 级：抖动是告警自身属性与路由无关，且避免再宽 routes 表。
- alerts 新列：`flap_count INTEGER NOT NULL DEFAULT 0`、`flap_window_start TIMESTAMPTZ`、`flapping BOOLEAN NOT NULL DEFAULT false`。
- 翻转计数（dedup stage 的状态转移点，firing↔resolved 双向）：窗口滑出 → 重置 count=1、window_start=now；否则 count++。`count ≥ threshold && !flapping` → 置 flapping=true，写时间线 `flapping_start`；置位当次事件照常走完管道（该次组通知即「一次提示」，组消息对 flapping 成员标注抖动中）。
- 抑制：flapping 告警在 GroupStage.Process 跳过 ScheduleIfAbsent（不再驱动组 timer）；GroupStage.Handle 的 members 过滤 flapping 成员，过滤后为空则整组跳过通知。状态机、入库、dedup、时间线全不受影响（AC4.3）。
- 解除（惰性）：下一次状态转移判定时窗口已滑出 → flapping=false、写时间线 `flapping_end`，恢复正常；告警 resolve 进终态则自然结束。不为解除引入 timer——代价是「停止抖动后标记要等下一次事件才摘除」，UI 徽标可能滞留；接受，因为滞留的标记不产生任何错误通知。

### C5 处置记录（R5）

- REST `POST /alerts/:id/resolve` body 增加可选 `note string`（上限 1000 字符，超限 422 复用既有校验口径）。
- `domain.Resolve` 签名扩为 `(alert, actorName, reason, note, now)`，note 写入 resolve timeline entry 的 detail；系统侧调用（TTL、inhibit 等）传空串，entry 形态与现状一致。
- 幂等短路（已 resolved 再 Resolve）：返 200 现状、note 丢弃、时间线不重写（不变量 6 一字不改）。
- 飞书卡片回调不支持 note（卡片无输入框），仅 REST/前端。前端 Resolve 改为弹窗（可选 TextArea + 确认），详情页时间线展示 note。
- 安全：note 不落日志；进通知内容时与既有时间线文本同等对待。

### C6 前端增量

- 路由表单：分组模式 Select（按标签/窗口全归并）+ 风暴阈值 InputNumber（0=关闭）；group_by 输入在 window 模式下禁用。
- 告警列表/详情：flapping 徽标（Tag「抖动中」）。
- Resolve 弹窗（C5）。

## Data Models

### 迁移 000008_noise.up.sql（要点）

```sql
ALTER TABLE routes ADD COLUMN group_mode TEXT NOT NULL DEFAULT 'label';
ALTER TABLE routes ADD COLUMN storm_threshold_per_min INTEGER NOT NULL DEFAULT 0;
ALTER TABLE alerts ADD COLUMN route_id UUID;            -- route stage 写入；存量 NULL 不回填
ALTER TABLE alerts ADD COLUMN flap_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE alerts ADD COLUMN flap_window_start TIMESTAMPTZ;
ALTER TABLE alerts ADD COLUMN flapping BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX idx_alerts_route_created ON alerts (route_id, created_at);
CREATE TABLE route_storm_state ( ... 见 C3 ... );
```

### Go 结构（新增/变更要点）

- `store.Route` 增 `GroupMode string`、`StormThresholdPerMin int`；`domain.Alert` 增 `RouteID`、`FlapCount`、`FlapWindowStart`、`Flapping`。
- `domain.TemplateFingerprint(text string) string`（纯函数）。
- `store.StormStateRepo`：`Get/Upsert/AddSuppressed/Deactivate`，全部带 `tx pgx.Tx`。
- `domain.Resolve(alert Alert, actorName string, reason ResolveReason, note string, now time.Time)`。
- config 增 `FlapWindowSeconds`、`FlapThreshold`。

## Error Handling

### Error Scenarios

1. **group_mode='window' 且 group_by 非空**：路由 CRUD 422，错误信息指明互斥。
2. **note 超长**：422，不截断不静默。
3. **storm_summary handler 失败**：scheduler 既有语义——整体回滚、标 failed；因 ScheduleIfAbsent 收敛，下一次组 flush 判定仍超阈值会重新拉起摘要 timer，熔断不会因单次失败永久失联。
4. **风暴计数查询失败**：视为「未超阈值」放行通知（fail-open）——降噪失效好过通知丢失；错误只记日志、handler 返回 nil（scheduler 对 handler 错误是整体回滚 + 标 failed 不重试，若返回错误会连本次 NotifyGroup 意图一起回滚，通知永久丢失）。
5. **未配置（阈值=0 / FLAP 窗口=0 / group_mode='label'）**：所有新逻辑短路，行为与现状逐字节一致。

## Testing Strategy

### Unit Testing

- `TemplateFingerprint` 表驱动：IPv4/IPv6/UUID/hex/数字/混合/无变量/空串/纯变量；相同模板不同变量 → 同哈希，不同模板 → 不同哈希。
- GroupStage：window 组键、`__template__` 组键、flapping 跳过注册、flush 过滤、风暴判定三态（未配置/超阈值进入/已熔断累计）——fake repo/scheduler/notifier，现有测试模式。
- dedup 翻转计数：窗口内累计、窗口滑出重置、置位/惰性解除时间线。
- storm_summary handler：摘要意图参数、清零、续期/解除+补组 timer、幂等重投。
- domain.Resolve note：入 entry、幂等短路丢弃。

### Integration Testing（-tags=integration）

- StormStateRepo 全方法；000008 迁移列/索引存在性；route_id 写入。

### End-to-End Testing

- 追加两场景：①模板归并——3 条只差 IP 的告警经 `__template__` 路由合为一组、一条聚合通知；②窗口全归并——不同 fingerprint 告警落同组。风暴/flapping 依赖速率与多次翻转的时序编排，E2E 成本高且单测+集成已覆盖判定逻辑，不做 E2E（此为显式裁剪，非遗漏）。

---

## AI Enrichment 框架（R6，纯设计——本章即交付物，代码零实装）

### 定位与红线

AI 能力（建议合并、事件总结、相似历史参考等）一律实现为**异步 enrichment 层**：独立于管道的消费循环，读取**已提交**的告警/时间线数据，产出「建议」类元数据，绝不阻塞、绝不回写管道路径。

**红线条款（随本 spec 实施同步进 CLAUDE.md 核心不变量，由主对话执行）**：

> 任何 AI/LLM 调用都是外部 IO，只准发生在异步 enrichment 层（管道外、任何持锁事务外，消费已提交数据）；管道 stage、scheduler handler、四大通知意图接口内禁止出现模型调用。LLM/enrichment 组件不可用时，系统全部既有功能必须 100% 不受影响——AI 是增强不是依赖。告警内容外发模型 API 前必须按既有口径脱敏。

### Advisor 接口形态（纸面约定，实装时照此填空）

```go
// Advisor 是一种 AI 能力的抽象：输入已提交的告警上下文，输出零到多条建议。
// 实现内部可以做任意外部 IO（模型调用），由 enrichment 循环在事务外调用。
type Advisor interface {
    Kind() string  // "merge_suggestion" | "summary" | ...
    Advise(ctx context.Context, in AdviseInput) ([]Suggestion, error)
}
// AdviseInput：告警快照 + 时间线快照 + 同组成员摘要（只读副本，脱敏后）。
```

执行位置：独立 goroutine 循环（或未来预留 timer kind `enrichment_poll`），小事务领取待处理告警 → **事务外**调 Advisor → 小事务写回 suggestions。与 scheduler 的关系：可复用 timers 表做触发，但 handler 只做「领取+落建议」，模型调用永远在事务外（这正是与 storm_summary 等常规 handler 的本质区别，实装时需在 app 层专门建这个「事务外调用」骨架）。

### 建议元数据模型（届时新迁移建表，现在不建）

```
suggestions(id, kind, ref_type('alert'|'group'), ref_id, source(advisor 标识+版本),
            confidence NUMERIC, status('pending'|'accepted'|'rejected'),
            payload JSONB, created_at, decided_by, decided_at)
```

要点：建议永远是 advisory——`accepted` 由人工在 UI 点击后置位，置位动作触发的实际操作（如合并组）走既有确定性 API，AI 路径自身没有任何直接改数据的权力；`decided_*` 即人工反馈数据，为将来评估建议质量积累样本。

### 首批候选能力与前置（实装另立 spec，按用户裁决届时再议）

1. 事件总结（只读 timeline → 摘要文本，落 suggestions.payload，UI 展示）——最低风险首选。
2. 相似历史告警参考（基于 resolution note 的确定性检索起步：同 route + 同模板指纹的已解决告警及其 note——注意这一步完全不需要模型，是 R5 数据的直接确定性红利）。
3. 合并建议（依赖人工反馈样本积累后再评估，且不走 embedding——方案届时须另行论证确定性替代或经用户重新裁决）。
