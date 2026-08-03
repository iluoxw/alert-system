# 多 Fault Incident 生命周期实施设计

## 目标

把 Incident 从“创建时绑定一个主 Fault 的只读外壳”补齐为真正的多 Fault 响应容器，并使以下不变量可以由数据库、服务端和 UI 共同验证：

1. 一个 Incident 可以保留多个跨层 Fault，但一个 Fault 仍至多属于一个 Incident；
2. 始终只有一个当前主 Fault，切换主 Fault 只改变诊断焦点，不删除旧关系或证据；
3. 人工排除 Fault 必须填写理由并留下操作者和时间，排除只影响当前 Incident 的关闭门槛，不改变 Fault 自身事实；
4. Incident 进入 monitoring 时，所有已纳入且仍为 active 的 Fault 一起进入 monitoring；
5. monitoring 阶段晚关联的 active Fault 必须单独标记缓解或被明确排除，否则 Incident 不能 resolved；
6. 所有关联、主 Fault 切换、排除/重新纳入和 Fault 生命周期推进都追加 Incident 时间线。

## 领域模型

`incident_fault_links` 保留两个相互独立的维度：

- `relation`: `primary | supporting`，表示当前诊断焦点；
- `disposition`: `included | excluded`，表示该 Fault 是否仍属于本次响应的关闭范围。

新增人工裁决字段：

- `disposition_rationale`
- `decided_by`
- `decided_at`

数据库增加“每个 Incident 最多一个 primary”的部分唯一索引。Primary 不允许直接排除；必须先把另一条已纳入关系切换为 primary，从而避免 Incident 失去主诊断对象。

## 服务端契约

### 关联 Fault

`POST /api/v1/incidents/:id/faults`

```json
{ "fault_id": "uuid" }
```

- 新关系固定为 `supporting + included`；
- 重复关联到同一 Incident 幂等返回 200；
- 已属于其他 Incident 返回 409；
- resolved Incident 不允许新增关系。

### 切换主 Fault

`PUT /api/v1/incidents/:id/faults/:fault_id/primary`

- Incident 行加锁后先把旧 primary 降为 supporting，再提升目标；
- 目标必须已关联且 disposition 为 included；
- 重复设置同一 primary 幂等；
- 变更写入 decision 时间线，记录 from/to。

### 裁决是否纳入

`PUT /api/v1/incidents/:id/faults/:fault_id/disposition`

```json
{ "disposition": "excluded", "rationale": "与当前事故属于独立因果链" }
```

- `included/excluded` 都要求明确理由；
- primary 不允许 excluded；
- 不删除关系；重新纳入同样追加时间线。

### 推进单条关联 Fault

`POST /api/v1/incidents/:id/faults/:fault_id/transitions`

```json
{ "to": "monitoring", "rationale": "数据库连接池恢复且观察 15 分钟稳定" }
```

- 只允许 included Fault 的 `active → monitoring` 和 `monitoring → resolved`；
- 用于 Incident 已在 monitoring 后才发现/关联的新 Fault；
- 必须填写证据理由并写 decision 时间线。

## Incident 状态约束

- 进入 `monitoring` 前仍使用现有 mitigation recovery gate；事务内将所有 `included + active` Fault 推进到 monitoring；
- 进入 `resolved` 前，除现有 resolution gate 和任务关闭条件外，再查询所有 `included` Fault；任一仍为 active 都返回 409；
- 进入 `resolved` 时将所有 `included + monitoring` Fault 推进到 resolved；candidate 和已 resolved Fault 保持原状；excluded Fault 的生命周期不受 Incident 状态变化影响。

这使事故级恢复证据能收口本次响应内的全部确定性技术故障，同时保留 candidate 根因假设和被排除的独立故障链。

## Incident Room 交互

- Fault 关系区新增“关联 Fault”；候选列表展示 Fault ID、标题、状态和层级；
- 主 Fault 卡片显示“主故障”；supporting 卡片提供“设为主故障”；
- included supporting Fault 可填写理由后“排除”；excluded Fault 保留在列表中并可“重新纳入”；
- monitoring Incident 中的 active Fault 提供“标记已缓解”，monitoring Fault 提供“标记已恢复”；
- 关闭按钮同时展示恢复判据、未关闭任务和 active 关联 Fault 三类阻塞原因。

## 验证

1. 领域单测：关系/裁决枚举与关闭前 active Fault 门槛；
2. Store 集成测试：唯一主 Fault、原子切换、裁决元数据、跨 Incident 唯一性；
3. API/服务集成：关联、切换、排除、晚关联 Fault 阻塞关闭；
4. `go test ./...`、`go vet ./...`、store integration、前端 build/lint；
5. 真实浏览器 E2E：给当前 E2B Incident 关联第二条 Fault，切为 primary，排除/重新纳入，推进状态并确认时间线与关闭门槛。

验证结果（2026-08-02）：领域/API/Store 测试、全仓 Go 测试与 vet、前端 build/lint、真实浏览器操作和完整 E2E 均通过。完整 E2E 期间额外暴露并修复了 Ack 与升级 handler 并发时“取消后又续挂下一层 timer”的窗口；修复后目标场景连续 10 次通过，随后完整 E2E 通过。

## 非目标

- 不把多个 Incident 合并或拆分；
- 不允许同一 Fault 同时参加多个 Incident；
- 不自动推断 primary；
- 不删除历史关系；
- 不在本阶段引入通用 ITSM 工单或复杂因果图编辑器。
