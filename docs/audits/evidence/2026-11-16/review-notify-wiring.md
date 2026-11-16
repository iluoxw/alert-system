# I1 第二角度审查：装配、API、迁移与映射触发

范围：通知迭代产品提交 `efaf0f4`，基线 `63778bf`。工作目录 `/private/tmp/alert-failure-notify`。本轮仅阅读实现、固定提交差异、验收文档及既有结果文件；未修改源码、未提交、未运行 Git 写操作、测试或固定 Compose E2E。

## 当前结论

在本次指定范围内未发现新增 P0/P1/P2。此结论是代码与已有证据核对，不替代另一 Agent 的队列并发审查或主 Agent 的真实 E2E/CI。新 E2E 测试尚在由 notify_impl 补充，完成后追加只读审查结果。

## 核对结果

1. **生产与测试 harness 装配一致。** 两者均通过 app.Build 安装 Sender.BeforeFaultSend / OnFaultResult、delivery repo 与第 13 个普通 timer handler；都调用 ReconcileFaultNotificationTimer 并挂载 RegisterFaultNotificationRoutes。pipeline MappingStage 的激活回调在 Build 注入，两个入口的 alertsource.Reconciler 同样注入 TriggerFromMappingResult。没有只在测试中注入可靠送达回调、生产仍走旧行为的分叉。
2. **API 权限与事务。** 三个通知管理端点位于 `/api/v1/admin`，同时使用 sessionAuth+requireAdmin；UUID非法 400、资源不存在 404，异常返回通用错误。事务与 service 调用显式使用 `c.Request.Context()`，不会落入 Gin Context 不透传取消的旧模式。GET状态与POST preview 均在 READ ONLY事务调用纯查询 Preview；Submit持Fault锁后重读状态与目标，未信任客户端先前预览。
3. **提交与审计。** 已认领或非 active 不调queue；重复无变化提交不重复写补投时间线。通知目标原文没有经新管理响应或新Fault时间线返回，Target显示固定redacted，delivery输出目标哈希。真实发送payload沿用既有受控内部存储。
4. **迁移升级。** 000032为增量创建watch/delivery及唯一性约束，不改旧迁移、不回填28、不把历史 owner_notified_at回填sent。down按delivery→watch逆序删除，只删除本迁移元数据。已读取主Agent迁移回执 `/private/tmp/alert-iteration-issues/migration-results.json`：empty/29/31→32、down/up均passed，29/31哨兵数据preserved。该回执为主Agent证据，本审查未另跑迁移。
5. **mapping触发语义。** TriggerFromMappingResult仅接受 CreatedActive/EnteredActive；重复active评估不通知，重新进入active建立新epoch。管理员reprocess仍只调用Project，不调用激活hook，因此补映射重放保持静默；显式submit才登记watch/产生补投意图。旧历史Fault不被启动全库扫描偷偷补发，与验收文档的授权边界一致。
6. **持久对账入口。** 启动对账只登记缺失的global pending timer，handler续挂每分钟任务；Due每批100且排除非active/已ACK。保存点隔离单watch失败。启动对账生产报错只记日志、harness返回错误是可见的环境启动策略差异，未发现由此导致正常已登记watch丢失的新增问题；未做启动时数据库故障注入。

## 验证与限制

- 本轮没有执行新测试命令，没有把实现者记录当作本次独立实测。已阅读 API RBAC测试及通知PG验收记录、迁移结果JSON；队列并发、锁顺序、Sender取消/结果原子性由另一审查者重点覆盖。
- 新API成功HTTP+真实映射→静默回放→预览/提交链路仍需新E2E与主Agent串行验证。
- 没有运行生产外部渠道，不推断真实投递率或响应SLA。没有独立注入迁移过程中并发发送或降级回滚；down删除新元数据的风险已经在实现验收记录中明示。

## I2 修复证据核对（不重复跑）

按主Agent要求先检查原 `/private/tmp/alert-iteration-issues/review-scheduler.md`，确认 notify_impl 已追加 `cdfe1bd` 独立复核。其代码变更采用 LastProgressAt 与 persisted完成标记；报告覆盖成功完成、DB aborted/查询失败、取消、settle失败、旧fencing。已读取 `scheduler-cdfe1bd-review-tests.log`，确认6个focused PG场景实际PASS、总退出0（26.769s），并核对报告中overlay五场景记录。原P2已经由该独立复核关闭，本轮不重复运行。

## 新 E2E 测试代码冻结后的只读复核

notify_impl 已确认测试代码冻结、`go test -c` 编译通过，未执行 Compose。本审查随后只读核对 `test/e2e/fault_notify_test.go`：

- `TestFaultNotify_SilentReprocessPreviewAndBackfill` 使用真实 HTTP 创建来源、先触发未映射 occurrence，补策略后调用实际 reprocess API；确认无 watch/delivery/notify timer、owner_notified_at 仍为空，覆盖静默回放边界。
- POST preview 与 GET 状态均验证 ready/queue，前后 watch/delivery/Fault时间线/timer计数完全相同，检查响应不含真实目标原文。
- 显式 submit 后等待真实 Sender/fake渠道的持久 sent，再重复提交检查 delivery ID 稳定、仅1条timer/补投审计，检查目标脱敏与 owner_notified/ACK分离，最后按 Fault ResourceID 过滤实际 fake发送次数，未用全局累计数污染跨测试结果。
- 拓扑抑制测试先等待上游 delivery sent，再触发下游；新口径不再把queued误当送达。断言下游无独立notify timer且记录covered。
- timer计数改按payload内 GroupKey，适配新delivery ref_id，未让旧refID断言静默漏掉新timer。

本次只读测试设计审查未发现新增阻断问题；没有运行这些 E2E，因此不把编译通过或代码审查当成 N4 端到端验收通过。notify_impl 本轮还修改了activation_version协议和迁移；该产品修复属于另一个专项并发审查/主Agentledger验收范围，不能用本报告针对efaf0f4的结论自动覆盖整个后续提交。
