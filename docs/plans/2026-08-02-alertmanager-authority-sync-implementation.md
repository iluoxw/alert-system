# Alertmanager 唯一 Alert 权威：同步里程碑实施说明

## 状态

已完成，2026-08-02。生产代码、迁移、控制中心 UI、自动化测试与真实浏览器 E2E
均已落地；本地 `5173/8080` 全链路可直接验收。

## 本里程碑交付边界

1. `integrations(kind=alertmanager)` 同时作为逻辑 `AlertSource`，一个 HA 集群只登记一次。
2. webhook 作为低延迟快路径；`GET /api/v2/alerts` 作为完整视图慢路径。
3. occurrence 幂等键固定为 `source_integration_id + upstream_fingerprint + starts_at`。
4. Alert 分离为 `source_state`、`response_state`、`suppression_state`，旧 `status` 只作为兼容投影。
5. Alertmanager occurrence 绕过本地 route/group/silence/inhibit/Alert 通知；Ack/Handoff/Note 仍归本地，Manual Resolve 返回冲突。
6. 首次从成功完整快照缺失进入 `unknown`；达到配置的连续缺失次数才进入 `resolved`。HTTP/API 失败只更新来源健康，不修改 Alert。
7. 凭据配置只保存环境变量名；数据库、API 响应和日志都不保存或回显明文 token。

## 事务与并发

- 拉取 Alertmanager API 在数据库事务外执行，默认 10 秒超时、16 MiB 响应上限。
- 每个 source 使用 PostgreSQL advisory lock 防止多副本同时对账。
- 完整响应解析成功后，才在一个短事务内推进 snapshot version、upsert occurrence、处理缺失和更新时间线。
- occurrence 写入另加 transaction advisory lock，旧 `source_updated_at` 不得覆盖新状态。

## 失败模式

| 失败 | 行为 |
|---|---|
| webhook 重复 | occurrence 唯一键 + upsert，`dedup_count` 增加，不创建第二行 |
| webhook 迟到 | `source_updated_at` 拒绝状态回退 |
| API 401/5xx/超时/非法 JSON | source failure 计数增加；Alert 不变 |
| 首次完整快照缺失 | `source_state=unknown`，保留响应状态与证据 |
| 连续完整快照缺失 | `source_state=resolved`，记录 `source-reconciled` 时间线 |
| source stale | UI 明示 stale；不把“看不到”解释成 resolved |
| SRE Control 停机 | Alertmanager 原通知链不经过本系统，不受阻断 |

## 非功能门槛

- webhook 入队 p95 < 500 ms；
- API 对账默认 30 秒，最小可配置 5 秒；
- 默认 stale 阈值 120 秒；
- 默认连续缺失确认 3 次，硬下限 2 次；
- 一次 source 只允许一个对账任务；
- 管理 API 仅 admin 可用；手工对账错误不暴露凭据。

## 验收

- firing webhook 建立权威 occurrence，重复 webhook 不复制；
- Ack 只改变 `response_state`，`source_state` 保持 firing；
- Manual Resolve 被拒绝；
- API 返回 `silencedBy`/`inhibitedBy` 时三轴正确；
- API 首次/连续缺失按 unknown/resolved 收敛；
- API 失败只导致 degraded/stale；
- Alertmanager 来源不产生本地 Alert 通知；Incident/Response 通知仍保留。

## 官方契约

- Alertmanager API v2 OpenAPI：<https://github.com/prometheus/alertmanager/blob/main/api/v2/openapi.yaml>
- Alertmanager webhook 配置与 payload：<https://prometheus.io/docs/alerting/latest/configuration/#webhook_config>

## 完成证据

- `go test ./...`：通过，包含完整 E2E（Alertmanager authority、E2B Fault/Incident、
  路由分组、静默、升级、到人通知和降噪场景）。
- `go test -tags=integration ./internal/store -run '^TestAlertmanagerAuthority_ReconcileLifecycle$'`：
  真实临时 PostgreSQL 通过。
- `npm run build && npm run lint`：通过；仅保留既有的大 bundle 警告。
- 真实浏览器：登记数据源 → 使用页面生成的 `5173/ingest/v1/hook/:hook_uuid` 投递 → 202 → 水位与
  firing 计数更新 → Alert 列表/详情三轴展示 → Ack 仅改变响应轴 → Manual Resolve 禁用。
