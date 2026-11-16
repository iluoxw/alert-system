# Grafana Source 范围与原生分析投递验收（Issue #23）

关联：Refs #22。基线fb505a2，分支codex/issue-22-grafana-delivery。本迭代只实现来源/分析投递契约，不修改IAC/Grafana线上配置、OMA worker或报告模块。

## 最终行为与契约

1. `integrations.config` 可配置 `label_scope`（最多16条、唯一非空标签的精确等值AND）与 `rule_id_allowlist`（最多128个公开稳定ID）。两者均未配置时保持旧来源行为。IAC配置建议固定product=IAC及启用7+3规则、隔离canary的`sre.rule.id`列表；不要从中文标题猜UID。
2. Webhook与完整对账快照使用同一FilterEvents。真正其他product/不同规则可排除；已知allowlisted ID缺少或冲突范围标签直接失败，不能把过滤结果当恢复。完整原始输入在过滤前按source+fingerprint核对既有identity；已有fingerprint丢标签、换rule ID或滑出范围同样失败。API拒绝返回422，不记录标签/Hook原文。
3. 已登记alerts/alert_events后，范围或kind不可修改，API返回422要求新建Source；不能把缩小过滤当告警解除。对账HTTP拉取不持数据库锁；提交时持来源共享锁并重新核对kind/scope/API目标/认证环境变量名，拉取期间改变配置或禁用会拒绝应用原快照，不推进水位。需变范围时新Source受控切换，不能直接编辑既有Source制造解除。
4. 无法归属的DatasourceNoData/Error fail-closed，保留原快照，来源健康记录失败。有明确scope/稳定ID的合成技术告警可入库与触发分析；同规则原业务告警只转unknown、真实缺失计数归零，不因连续NoData/Error自动resolved。后续完整、可解释空快照才重新累计缺失确认。Mapping仍需精确正常alertname，不能只按stable ID把技术合成错误判为业务direct。
5. 修复managed hook与PG recorder两处仅Alertmanager登记batch的限制；Grafana原生Envelope.Source=grafana，角色配置接受source=grafana。Alertmanager旧分组/重复语义保持。
6. `agentprotocol.Member` 新增可选 `rule_id`，只从`sre.rule.id`读取且必须位于 `routing.public_rule_ids`白名单；中文alertname仍不突破原标识符白名单。禁止导出任意labels/annotations/URL。GroupPayload新增origin=webhook/snapshot，保留receiver_ref/group_ref/truncatedAlerts与成员opaque ref；schema_version保持1，向后兼容增加字段。
7. `Runtime.AttachReconciler` 在生产main完成接线。Grafana快照首次发现/unknown恢复的firing、缺失确认后的resolved，按稳定rule/title/severity合成组并事务内登记分析batch；每30秒正常快照不会反复派分析。合成组不伪称已知原上游group，origin明确snapshot。成员ref与真实Webhook相同，便于关联Alert/Fault，成员原指纹不出机器API。
8. migration **000034** 新增 `agent_grafana_occurrences(source_id,occurrence_ref,phase)`唯一收据，FK来源删除级联清理；与runtime预留000033没有依赖。收据与batch同事务，处理失败整体回滚；Webhook/补漏、迟到请求、ACK/消息留存清理后共享去重。收据不按消息retention删除，保留至Source删除；不要自动down丢弃去重历史。批量按稳定收据键排序，避免成员顺序不同造成唯一索引锁倒序。
9. 当组含新阶段时保留整个通过scope的上游组上下文，不因仅剩一条新resolved成员就把仍firing的组伪装成完整恢复。全部成员阶段均已登记时不再建batch。该幂等是分析准入，不宣称OMA模型会话或报告已经完成。

配置草稿（仅示例，实际列表应含真实启用7+3及已审查canary ID）：

```json
{
  "label_scope": [{"name":"product","op":"=","value":"IAC"}],
  "rule_id_allowlist": ["iac-go-system-errors","iac-go-metrics-stale","iac-sre-canary"]
}
```

```yaml
routing:
  source_ids: [<实际Grafana Source UUID>]
  public_rule_ids: [iac-go-system-errors, iac-go-metrics-stale, iac-sre-canary]
  routes:
    - {name: iac, source: grafana, source_id: <同一Source UUID>, role: iac, ttl: 1h}
  fallback: {role: triage, ttl: 1h}
```

Schema字段说明：Member为ref/rule_id/alertname/status/severity/startsAt/endsAt；GroupPayload为origin/receiver_ref/group_ref/truncatedAlerts/alerts。稳定ref算法仍为JSON字段边界编码的SHA256(source ID, upstream fingerprint, startsAt RFC3339Nano)，不新增暴露实体凭据。

## 真实 PostgreSQL 回归

使用独立testcontainers PostgreSQL16，无固定Compose共享资源。测试覆盖：

- Grafana标准HTTP webhook混合IAC/其他product，事件与Agent batch一致过滤；事务注入失败时events/batch/receipt均回滚。
- Native Source、稳定rule_id白名单、中文标题、目标/注解/secret不出机器载荷。
- Webhook先到、对账先到两种顺序；真实firing/confirmed absence resolved，重复快照不重复分析、迟到Webhook不重复、删消息模拟retention后仍不重复，member ref一致。
- 拉取期间改变无数据Source的scope：新配置提交，旧HTTP快照必须整体拒绝；已有occurrence后通过IntegrationRepo改变scope必须拒绝。
- 四类普通malformed（缺scope、scope冲突、同fingerprint换ID、全丢身份）分别连续4次快照及HTTP拒绝，已有firing不误resolved。
- 归属未知NoData/Error不推进水位；已知NoData/Error可投递技术分析，但原业务持续unknown/缺失计数0，不因多次缺样本“恢复”。
- 八路并发、成员逆序的相同组只有一个batch/每成员phase一条收据；34 SQL down/up保留旧Source与batch，收据被删除是明确回滚代价。
- 既有Alertmanager webhook事务/重启/fanout、租约/队列回归。

## 实际命令与结果

统一 `GOCACHE=/private/tmp/alert-correctness-gocache`。

- `go test ./internal/... ./cmd/... -count=1`：退出0，所有包通过；不包含固定test/e2e执行。
- `go test -race ./internal/store ./internal/ingest ./internal/alertsource ./internal/agentdelivery ./internal/api -count=1`：已通过；后续scope强化后最终重跑结果在下方补充。
- `go test -tags=integration -race ./internal/alertsource ./internal/agentdelivery -count=1`：退出0，alertsource13.721s、agentdelivery47.239s（普通scope丢标签强化前，最终新增边界由focused再次验证）。
- `go test -tags=integration -race ./internal/store -run 'TestIntegrationRepo|TestSourceScope' -count=1`：退出0，8.335s。
- `go test -tags=integration -race ./internal/agentdelivery -run 'TestGrafana' -count=1`：scope强化后退出0，19.613s；包含原生/补漏/NoData/普通malformed及8路并发。
- `go build ./...`、`go vet ./...`：退出0；scope强化后最终重跑结果在下方补充。

初始实现的自动编辑断言/编译字段错误、并发fixture缺必填hook_id_hash已修正并重跑，未降低断言阈值。自学习记录在本地`.learnings/issue-23-source-errors.md`，不包含凭据。

## 限制与集成待验收

- 未运行固定Compose E2E、真实Grafana生产通知、线上Source配置或OMA会话/结果接口；后者属于另两迭代。
- 当前只接受显式`sre.rule.id`，不会猜测rulename或rule_uid。root将捕获真实Grafana NoData/Error fixture；若其丢弃公开ID/归属，当前拒绝并保留unknown/失败证据，不能宣称此类消息已成功分析。后续只有审查过的公开身份契约才能扩展。
- 用Source全量原始快照做本地scope过滤，未宣称上游服务端支持filter参数；16MiB响应上限保持。其他业务无法归属的合成异常会保守使本scope快照失败，宁可degraded不凭局部成功快照解除告警。
- 收据代表分析已持久准入，不是投递ACK、模型完成或报告完成。已准入后改变角色路由不自动回放旧phase；需要显式受控重分析能力，当前没有暗中追加新任务。
- 000034独立空库/SQL down-up测试不替代root执行000033+000034联合真实migrate ledger与上线验证。应用回滚不自动down。
- 没有前端变更，不运行前端构建；新scope通过管理API/受控配置使用。未改go.mod、worker/report模块或.spec-workflow。

最终回归补充：普通malformed范围/既有fingerprint核验强化后，完整`go test -tags=integration -race ./internal/alertsource ./internal/agentdelivery -count=1`退出0，alertsource10.400s、agentdelivery44.395s。最终五包unit race store3.137s/ingest3.691s/alertsource2.198s/agentdelivery4.171s/api7.010s，完整build/vet退出0。新增34 SQL down/up及8路并发focused再次退出0（5.323s）。

成员身份精度补充：导出`agentprotocol.OccurrenceRef(source, sourceID, fingerprint, startsAt)`；Grafana按PG微秒归一化，Alertmanager保留旧纳秒算法。123456789ns真实PG fixture跨Webhook→PG回读→snapshot resolved仍保持Ref一致，focused race退出0（4.471s）。报告关联方应复用helper而不是重写hash。最后新增协议纯函数精度/来源隔离单测。
