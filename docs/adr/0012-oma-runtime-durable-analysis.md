# ADR 0012：复用角色队列的常驻 OMA 分析运行时

日期：2026-11-16。状态：实施中。父 Issue #22；本迭代 #24。

## 决策

复用 agent_messages 的角色、claim、receipt、ACK，不增加第二套投递协议。常驻 cmd/oma-worker 与 SRE Control 使用同一受控 PostgreSQL；每个有界执行槽空闲时才 claim 一条。在同一个短 PG 事务内先写入按 message_id 唯一的收件记录，再复用 Queue.AckTx 校验并 ACK 原 receipt，标为 received 后原子提交。事务中途崩溃全回滚，提交后从独立账本恢复；ACK 只表示持久接收。

使用迁移 33 记录 runs、外部提交 actions、OMA events、证据与报告。运行租约具有递增 fencing token，续租与 checkpoint 使用条件更新。执行过程中的所有 OMA/broker IO 在数据库事务外。阶段为 received → session intent → 首轮 JSON 取证意图 → broker 证据 → 同会话二轮事件 → JSON 报告。报告仅关联已有 Alert/Fault，不自动改变任何领域状态。

真实事件订阅使用当前实例验证的 `/v1/sessions/{id}/events/stream` SSE。每个事件 ID 去重、持久化游标；断连与周期补漏使用 GET events 分页重叠读取。Last-Event-ID 是否能回放未获证明，只视为 best effort。不能把 session idle/end_turn 当作分析成功，必须验证本轮 JSON、query_id 白名单及事实证据引用。

## 外部提交未知

OMA 尚未提供已验证的服务端幂等保证。创建会话/追加 user.message 前先持久化稳定提交标识与请求摘要；崩溃恢复时，创建会话按唯一标题/metadata 在有界列表中查回，追加事件按本次 submission_id 在 user.message 中查回。无法查证时持久记录 submission_unknown/partial，不盲重发 POST，不自动以新会话掩盖旧请求。

## 接口

Broker 由 internal/iacbroker 提供固定 query_id 的只读取证；运行时通过 `EvidenceCollector(context.Context, []string) (json.RawMessage,error)` 适配，查询范围/凭据/窗口由 broker 强制，不从模型提供的 URL、SQL、shell、appId 派生。运行时最多允许 6 个无参数白名单 ID。

首轮协议为 sre.analysis-intent/v1；最终协议为 sre.analysis-result/v1。报告包含事实/推断/证据不足/建议及有界证据引用；schema、身份、生命周期状态由程序控制。未知/失败证据不会自动得到健康结论。模型/查询503或超时保留 partial；进程关闭保留待恢复 checkpoint。

结果只读 API 使用现有 sessionAuth + requireAdmin，返回 runs/report 和脱敏证据摘要、原 Alert/Fault ID；不输出 receipt、队列 envelope 原文、API key、原始事件正文或任意工具输出。消息表7天保留不能级联删除分析审计。

## 取舍与限制

与本机 inbox 相比，同PG常驻 worker 不依赖个人电脑或单机磁盘；但不能宣称数据库丢失时可恢复。分析状态表不是新的告警队列，现有 deterministic 通知不依赖其可用性。没有真实外部幂等支持时，提交未知只能提供可审计恢复/阻断，不能同时保证无重发和自动持续推进。

验证重点：ACK前后崩溃；ACK结果未知；会话POST前后崩溃；SSE断连/GET补漏/重复事件；模型503/空结果；证据失败/越权query；相同报告幂等；旧fence拒绝；Alert/Fault精确绑定及RBAC。
