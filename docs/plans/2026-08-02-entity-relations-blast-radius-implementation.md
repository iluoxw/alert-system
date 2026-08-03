# EntityRelation 与爆炸半径 v1 实施计划

## 状态

Completed — 2026-08-02

## 目标

让应用服务以显式数据声明对平台/基础设施实体的依赖；基础设施 Fault 沿跨来源关系图形成可解释的 potential application Impact，并由并发的应用 direct evidence 或人工裁决确认/排除。

## 非功能约束

- 不增加外部图数据库、消息队列或新后台 worker。
- 单次遍历最大深度 8，关系 API 最多处理 500 个实体，open Fault 重算有界。
- 所有关系与 Impact 更新在调用方事务内完成；失败整体回滚。
- path 去环；observed stale 保留但不能自动 confirmed。
- 人工 Impact 决策和 canonical identity 不得被自动重算覆盖。
- Alertmanager 仍是唯一 Alert 权威，Incident 仍只能人工宣布。

## 实施步骤与验收

1. **Expand 关系模型**
   - 输出：migration 000020；dependency edge 支持 declared/observed；跨来源索引。
   - 验收：旧快照导入不变；declared 关系允许无 sync run；约束拒绝混合身份。
2. **实体与服务依赖治理**
   - 输出：managed infrastructure/platform entity API；service dependencies 读写。
   - 验收：entity key/layer 不可变；服务依赖与 Alias/ownership 同事务更新；空集合返回 `[]`。
3. **通用爆炸半径投影**
   - 输出：跨来源反向/正向有界遍历与 Impact Projector。
   - 验收：基础设施 Fault → application potential；应用 direct evidence → 非 stale 路径 confirmed；resolved 后回落 potential；人工 ruled_out 不被覆盖。
4. **UI 与交互**
   - 输出：控制中心“实体与依赖”；基础设施实体登记；服务依赖编辑；Fault 路径解释。
   - 验收：创建/编辑/错误/空态可用；路径显示实体名称、跳数、来源与 stale 状态；不暴露 UUID 作为主要内容。
5. **双场景 E2E 与真实浏览器**
   - 输出：E2B、CCH 共享基础设施/平台依赖场景；真实 UI 读写和 Webhook 注入。
   - 验收：两个业务无核心分支；potential/confirmed/ruled_out 与 Incident 门槛符合 ADR；全量 Go、integration store、lint/build 通过。

## 实际验收结果

- 自动化 E2B/CCH 场景验证同一共享 Kubernetes Fault 同时传播到两个应用；应用 direct evidence 可确认、恢复后回落 potential，人工 ruled_out 在后续自动重算中保持优先。
- 真实浏览器从控制中心登记 platform 实体、新 CCH 应用、Alias 和 `application → platform` declared 依赖，并创建固定实体 Alert Mapping；平台 Alert 经页面分发的 `/ingest/v1/hook/{hook_uuid}` 地址进入系统。
- Fault 工作区展示 `platform → application` 可读路径；potential 时 Incident 禁用，direct evidence 后 confirmed 并解锁，恢复后再次关闭。
- 浏览器审计额外修复了历史服务空集合导致的白屏、Mapping 只能选择应用服务、Fault 状态轴使用旧 status、全局 Incident 列表语义不清等问题。
- 验证命令全部通过：`go test ./... -count=1`、`go test -tags=integration ./internal/store -count=1`、`go vet ./...`、`npm --prefix web run lint`、`npm --prefix web run build`。Vite 仅保留既有的主 chunk 大于 500 kB 警告。

## 本期不包含

- CMDB/Kubernetes/OTel Connector 的完整同步协议；
- 自动冗余、容量、主备或根因推理；
- arbitrary relation type 编辑器；
- 自动 Incident、业务能力和用户旅程建模；
- 图数据库或异步全图物化。
