# ADR-0002：使用 E2B Failed to place sandbox 作为黄金验收场景

## Status

Accepted

## Context

ADR-0001 已确定 `Signal → Alert → Fault → Incident → Response → Recovery → Learning` 的产品模型，但当前设计稿主要用 MySQL 故障作为示例，还没有一条能同时约束领域建模、UI、安全操作和端到端验收的真实场景。

`/Users/luoxiaowei/code/others/e2b/issues/` 保存了 E2B 自托管集群的真实事故证据、RCA、SOP 和安全工具。其中 `500: Failed to place sandbox` 不是单一错误，而是可能包装下列故障链的外层症状：

```text
UFFD/快照对象缺失
→ 无效 sandbox 被持续 connect/auto-resume
→ 跨节点 placement 重试
→ 重复/孤儿 Firecracker 残留
→ CPU、HugePages、并发槽位被占用
→ Node exhausted / envd timeout
→ create/resume SLO 受损
```

历史经验还证明：placement attempt 失败数可以被重试风暴严重放大，而新建 Sandbox 仍然成功。因此不能把“尝试失败率”直接等同于用户 SLO，也不能在没有业务影响证据时自动建立 Incident。

## Decision

1. 将 E2B `Failed to place sandbox` 定义为 alert-system 的黄金验收场景，用于驱动第一个完整的 Infrastructure-first 纵向切片。
2. 实现保持供应商中立：领域层使用通用 Resource、Capability、Business System、Alert、Fault、Incident 和 Runbook 契约；E2B 只是第一个连接器、fixture 和验收场景。
3. 采用混合验收：本地故障回放是必须稳定通过的硬门禁；隔离的 E2B UAT 实机演练是最终验收，需要单独授权和变更记录；生产环境不作为自动化故障注入靶场。
4. 黄金场景必须包含两个阶段：
   - 重试/残留资源已形成技术 Fault，但 create 合成探针仍然成功，不宣布 Incident；
   - 资源泄漏和过载继续放大，create/resume SLO 失守，系统建议并由人宣布 Incident。
5. alert-system 不内嵌 JumpServer/SSH 凭据，也不直接任意执行生产命令。诊断与处置通过可替换、白名单化的 Runbook Executor 进行，必须支持 dry-run、计划哈希、过期时间、人工审批和完整审计。
6. 运行时不直接解析 E2B 全量日志作为领域事实。E2B collector/fixture 先把指标、日志证据、资源快照和依赖边归一化，alert-system 再消费稳定契约。

## Consequences

### Positive

- 设计、领域、API、UI、安全执行和验收共享同一条真实故障主线，能防止页面先行、数据后补的假闭环。
- 故障链包含明确的因果候选、放大因素、症状和业务影响，可真实验证 FaultAlertLink 的角色和可解释性。
- 本地回放可重复，UAT 可检验真实协议和运维边界，两者互补。

### Negative

- 第一个纵向切片同时涉及拓扑、相关性、Incident 协作和安全动作，实现面较大。
- UAT 验收依赖可用的隔离环境、读权限、处置审批和飞书测试渠道，不能在 CI 中无条件运行。

## Alternatives Considered

**只用通用告警 fixture 演示 UI**

- 拒绝：只能验证告警展示，无法验证故障证据角色、拓扑影响和安全 Runbook。

**alert-system 直接 SSH/JumpServer 控制 E2B 生产集群**

- 拒绝：凭据与任意命令执行的爆炸半径过大，无法满足现有 E2B SOP 的授权、dry-run 和精确目标约束。

**仅使用实机 E2B 作为验收**

- 拒绝：结果受实时负载、拓扑、数据和外部权限影响，不可重复，也不适合开发回归。

## References

- `docs/plans/2026-08-02-e2b-golden-scenario-design.md`
- `/Users/luoxiaowei/code/others/e2b/issues/failed-to-place-sandbox/SOP.md`
- `/Users/luoxiaowei/code/others/e2b/issues/INCIDENT-2026-06-18-orchestrator-oom-orphaned-sandboxes.md`
- `/Users/luoxiaowei/code/others/e2b/issues/REPORT-sandbox-create-rate-2026-07-24.md`
