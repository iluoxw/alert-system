# I1 通知独立审查

**最终结论（bcb806d）：两项 P1 与一项 P2 均已复验关闭，未发现剩余阻断项。以下保留各轮审查与修复证据，末节为最终验收。**

审查提交：`efaf0f44dd48d4b661beb7f98469a2a4157f4610`；基线：`63778bf`。目录：`/private/tmp/alert-failure-notify`。审查仅阅读代码、执行只读 Git 和外部 Go overlay 测试，未改功能 worktree；审查前后 `git status --short` 为空。

结论：发现两种已用真实 Mapper / Sender / PostgreSQL 复现的 **P1 阻断行为**。它们共享根因：notification epoch 未原子绑定真实 Fault 激活转换，而是在通知回调中凭 Transition 和当前时间生成；没有通知回调的真实转换又不会生成 epoch。建议一起修复，不通过单独弱化测试断言绕过。

## 已证实缺陷

### [P1] 并发投影的过期 Transition 可重置 epoch，同一激活实际发送两次

位置：`internal/app/fault_outbox.go:240-241`；相关 `internal/store/fault_notifications.go:Activate`。

原有 Mapper 在 Upsert 前读取 previous.Status，读取没有锁。两个真实事务可以先读到同一个 candidate；第一个完成 candidate→active 后，第二个使用先前读到的 candidate，也返回 EnteredActive。通知回调对每个 EnteredActive 都用 `o.now().UnixNano()` 调 Activate，没有验证这是同一个已完成的激活。

独立复现用 pgx.Tx 包装器在第二个事务的 **真实 GetByCorrelationKey Scan 完成后**设置同步屏障；它不改查询结果、不 fake 仓储。期间第一个事务真实 Project → Trigger → 提交，再用真实 Sender 发出一条成功通知；解除屏障后第二个事务真实 Upsert + Trigger 再建立 epoch，随后真实 Sender 又发一条。

结果：`TestReviewConcurrentActivationDoesNotSendTwice` 退出 1，错误为「同一candidate→active转移被发送2次」。这是一次领域激活的两个持久逻辑意图，不是外部发送成功后崩溃导致的已接受 at-least-once 窗口。还会重置当前 owner_notified_at，并让第一个成功 epoch 失去当前覆盖资格。

建议把 epoch 变更绑定到数据库确认的真实状态转换（或等价的稳定激活标识），与 Fault 状态提交原子完成；并发读取的旧 Transition 不能单独驱动 Activate。不能以拓扑抑制或 Sender 幂等键补救，因为第二个 epoch/UUID 已绕开原幂等键。

### [P1] 静默 reprocess 重新激活 Fault 后仍沿用旧 epoch，显式补投被上一轮 sent 吞掉

位置：`internal/store/fault_notifications.go:32-35`（Enroll 已存在时永不更新 epoch），关联 `internal/app/fault_outbox.go:240-241` 和 `internal/api/service_catalog.go:reprocessAlert`。

序列：

1. 一个 Fault 完成激活通知，当前 delivery 为 sent。
2. 通过真实 Fault 生命周期方法进入 monitoring。
3. 管理员对仍 firing 的证据执行静默 reprocess：与 HTTP handler 完全一样，只调用真实 Project，不调用 TriggerFromMappingResult。Project 确实返回 EnteredActive，但 watch epoch 保持旧值。
4. 管理员显式 Submit，Enroll 沿用旧 epoch，Ensure 找到上一轮 sent，于是不建新意图。Preview 返回 `eligible=true / reason=ready / action=sent`。

`TestReviewSilentReprocessActivationCanBeExplicitlyNotified` 在真实 PG 上退出 1，错误为「新激活批次显式补投错误复用上轮结果」，输出的唯一 delivery 仍为旧 sent。后续普通 webhook 重新评估已 active 的 Fault 通常只得到 TransitionNone，也无法补救。

这破坏本轮「静默回放 → 预览 → 显式补投」与「当前激活 epoch 送达覆盖」的组合语义。静默回放应保持无通知 IO/入队副作用，但仍需要有正确的激活身份，供后续显式补投判断；上一轮 sent 不能覆盖新一轮激活。修复应连同第一项统一处理真实激活身份，而不是让 reprocess 自动发送。

## 复现文件与命令

源码：`/private/tmp/alert-iteration-issues/notify_review_test.go`。

Overlay：`/private/tmp/alert-iteration-issues/notify-review-overlay.json`。只向 Go 编译器增加虚拟测试文件，不落地修改功能 worktree。

```bash
GOCACHE=/private/tmp/alert-review-notify-gocache go test \
  -overlay /private/tmp/alert-iteration-issues/notify-review-overlay.json \
  -tags=integration -race ./internal/app \
  -run 'TestReviewConcurrentActivation|TestReviewSilentReprocess' -count=1 -v
```

结果：退出 1，7.828s，两项真实失败。日志：`/private/tmp/alert-iteration-issues/notify-epoch-repro.log`。单独并发复现另见 `notify-activation-repro.log`，退出 1，5.195s。全部出站仅调用测试 Channel，未向真实团队发送。

## 已独立验证且未发现新增阻断项

- 新 Sender 在现有结果短事务内调用 OnFaultResult；notification_log、Fault 结果、owner_notified_at 与 Fault 时间线共同提交。BeforeSend 的 epoch/generation/ACK/状态检查位于外部 IO 前的短事务中。结果失败仍返回错误，未把“写失败审计成功”当作发送成功。
- 失败、孤儿 timer、显式 generation 最多增加两次；普通对账不无限重投 failed。claimed 即使租约过期仍有恢复路径，不误判孤儿。
- 上游覆盖按当前 watch epoch、active Fault、相同 channel 和 target_hash 的 sent 计算；queued/sending 不抑制；排除自身。上述 epoch 缺陷会破坏何谓当前 epoch，但覆盖 SQL 自身未发现额外目标错配。
- Preview 不登记 watch，不写 timer，HTTP 使用 READ ONLY；Submit 锁 Fault 后重新加载。管理 API 有 session/admin 保护，UUID 校验，原目标不出现在 delivery/preview/新 Fault 结果时间线里。
- 扫描上限 100；逐项保存点；没有预锁 watch。锁路径实际为普通 reconcile timer 行 → Fault → watch/delivery →通知 timer；Detached claim 已提交后才进行 Before/Result，因此没有“持有 notify_send 行锁再请求 Fault”的反向边。不同池并行会增加竞争，但代码阅读及独立 24 路竞争用例未发现新的可复现死锁。
- 新增迁移只创建 watch/delivery，不把旧 owner_notified_at 当成 sent 回填。down 会删除新投递元数据，这是明确回滚边界。本轮审查没有重新执行 ledger 矩阵，未把实现报告的迁移结果算为独立验证。

## 独立通过的命令

```bash
GOCACHE=/private/tmp/alert-review-notify-gocache go test -race \
  ./internal/app ./internal/api ./internal/notify -count=1
# 退出 0：app 1.525s、api 6.456s、notify 2.161s

GOCACHE=/private/tmp/alert-review-notify-gocache go test -tags=integration -race \
  ./internal/app -run '^TestFaultNotification' -count=1 -v
# 退出 0：16.140s，三个测试组真实执行

git diff --check 63778bf efaf0f4
# 退出 0，无输出
```

日志：`/private/tmp/alert-review-notify-unit.log`、`/private/tmp/alert-review-notify-pg.log`。既有通知验收用例通过，未覆盖这次独立发现的 epoch 场景。未运行固定 `test/e2e`、部署或修改 Git 状态。

## 建议与限制

- epoch 修复后应保留两个独立回归：一个同步屏障覆盖旧 Transition 并发，一个静默 Project 后显式补投；同时复核历史 sent、旧 receipt/result、ACK 后拒发和覆盖释放。
- 可补新旧版本 pending notify_send 在迁移后的实际 Sender 兼容测试；旧 payload 没有 FaultNotificationID，不应被误算为新结构的 sent。
- 此审查未做真实进程 kill、多机 lease 接管、网络已发但提交失败的真实渠道注入、PG16 ledger 或集成后的双池整套 E2E，不对这些场景作已验收声明。
- 外部复现最初以 timers.created_at 排最新 payload，PostgreSQL now() 是事务起始时刻，旧启动事务后来插入的第二条 timer 反而排序靠前后不确定；已改按当前 queued delivery 选择，排除测试误选旧 payload 的假阴性。该经验仅记于审查目录，未改功能代码或用户记忆。

## 2026-11-16 修复提交复核：59e3df7

固定复核 SHA：`59e3df7e94354342bafb6e4476e95c73b0c44ca3`。原两项 P1 的独立外置 overlay **均通过**，9.231s：

```bash
GOCACHE=/private/tmp/alert-review-notify-gocache go test \
  -overlay /private/tmp/alert-iteration-issues/notify-review-overlay.json \
  -tags=integration -race ./internal/app \
  -run 'TestReviewConcurrentActivation|TestReviewSilentReprocess' -count=1 -v
```

日志：`/private/tmp/alert-iteration-issues/notify-epoch-fixed.log`。并发回归增加「总 delivery 只有 1 条」断言；修复后没有新的 queued delivery 时重放原 timer payload，继续严格要求实际 Send 次数为 1。没有将「修复后没有 queued」误当成测试失败，也未弱化幂等要求。

修复采用领域级 `activation_version`：INSERT 固定初值 1；只有 OLD 非 active、NEW active 才 +1 并清空 owner_notified_at；重复 active 更新保留版本。BEFORE INSERT/UPDATE 触发器与 Fault 更新在同一事务及行锁协议内，因此旧 Transition 不再单独制造新 epoch，静默 Project 也能推进真实版本而不入队。

- Watch 仅在通知回调或管理员显式 Submit 时登记到当前版本；Due 同时要求 watch.epoch=fault.activation_version。
- 即使 Due 已先返回旧候选，逐项 queue 在 Fault 锁内还会重新核对 RegisteredEpoch，旧 watch 不能在静默重激活后自动登记新版本。
- Begin 比较 delivery/watch/真实 Fault 版本，拒绝旧 epoch；Result 先取得 Fault 锁，再由新语句更新当前版本结果；Covered 也要求 watch、delivery、Fault 三者版本相同。已查看这些路径，没有发现额外的旧版本覆盖或回写漏洞。
- 独立重跑当前完整 `^TestFaultNotification` 真 PG race，通过；其中包含真实状态转换、旧 receipt/result、同目标覆盖与静默重激活旧 Due 双重检查。
- 本轮没有重新运行迁移 ledger；主 Agent 已提供新版真 ledger 通过回执，不能算作本审查的独立测试。

### [P2] 新版本号与 generation 相加后的 round 碰撞，使新激活的失败兜底被旧记录吞掉

位置：`internal/app/fault_outbox.go:223`；关联 `internal/app/outbox.go:buildMetaAlertRequest` 与 `internal/notify/sender.go:idempotencyKey`。

当前 `req.Round = epoch + generation`。采用连续 +1 的 activation_version 后，旧 epoch=2、generation=1，与新 epoch=3、generation=0 都得到 Round=3。主通知使用 delivery UUID + generation 做幂等，彼此区分；但 buildMetaAlertRequest 不保留 delivery 身份，meta 回到 legacy group/channel/target/round/meta 键。因此同一 Fault 上一次重试失败的兜底若已 sent，新激活首次发送失败的兜底会被判为同一条已成功通知，实际不再发送。

该问题影响失败兜底；原送达状态与上述两个 P1 已正确，不把本问题夸大为所有主通知都漏发。建议在合并前修复，避免下一轮新事故失败兜底静默丢失。

独立复现使用真实 buildMetaAlertRequest、真实 Sender、PG notification_log 与可计数测试 Channel；两个原意图有不同 FaultNotificationID/generation，但分别对应 2+1 与 3+0。同一 fallback 接受成功应发生两次，实际仅一次。

```bash
GOCACHE=/private/tmp/alert-review-notify-gocache go test \
  -overlay /private/tmp/alert-iteration-issues/notify-review-overlay.json \
  -tags=integration -race ./internal/app \
  -run TestReviewNewEpochFallback -count=1 -v
```

结果：退出 1，5.320s，错误「新epoch的fallback被上一轮retry吞掉: 实际发送=1」。源码继续保存在 `notify_review_test.go`，日志：`/private/tmp/alert-iteration-issues/notify-fallback-repro.log`。

建议为 meta 意图保存独立且稳定的原 delivery UUID + generation 身份，并区分主通知与 meta，不能直接把原 FaultNotificationID 复制过去触发 BeforeFaultSend 的失败状态拒绝。也可以设计经验证不会碰撞的持久 round，但不能继续使用加法合并两个独立维度。

补充同根因的多目标证明：在 59e3df7 且工作区干净时，用两个不同原 delivery UUID、同 epoch/round、不同失败目标、同 fallback，调用真实 scheduleNotifyFailureMetaAlert。两条独立失败的 meta timer 被 pending 唯一索引折成一条（`count=1`，预期 2）。因此修复不能只改 Sender 幂等键，必须同时给 notifySendRefID 独立 meta 身份。

```bash
GOCACHE=/private/tmp/alert-review-notify-gocache go test \
  -overlay /private/tmp/alert-iteration-issues/notify-review-overlay.json \
  -tags=integration -race ./internal/app \
  -run TestReviewMultiTargetFailure -count=1 -v
```

结果：退出 1，8.548s。日志 `notify-meta-timers-repro.log`，源码仍在同一个外置测试文件。此项与 epoch/generation collision 一起归入同一个 P2 meta 身份缺失问题，不另计一个重复缺陷。

59e3df7 当前完整通知 focused PG race 的独立通过用时为 33.508s，`git diff --check efaf0f4 59e3df7` 退出 0。随后等待新固定 SHA 再复验，不对实施中的文件做并行重测。

## 最终复核：bcb806d（全部已发现问题关闭）

固定 SHA：`bcb806da0ac670ddfb651dbf991ce5357692c267`。比较修复前 `59e3df7`。功能 worktree 审查前后干净，未修改源码或 Git 状态。

**结论：通过本轮独立审查。原两项 epoch P1 在 59e3df7 已关闭；此次 meta 身份 P2 也已关闭，没有剩余已发现阻断项。** 全仓联合验收、固定 E2E 与 CI 仍由主 Agent 完成，本结论不替代这些闸门。

- 新 payload 保留 `MetaSourceDeliveryID` 与 `MetaSourceGeneration`，原 `FaultNotificationID` 不复制给 meta。
- `notifySendRefID` 与 Sender 的 `idempotencyKey` 都调用同一个 `notify.FaultMetaIdentity`，身份包含原 delivery UUID、generation，以及 channel/target 哈希。相同源与目标的重放保持稳定，不再依赖 epoch+generation 相加的 round；跨 delivery、generation、fallback target/channel 都分离。
- meta 的 BeforeFaultSend、OnFaultResult 路径均显式被 `!MetaAlert` 排除；成功或失败不会更新主 delivery/owner_notified_at。markFailed 在 MetaAlert 时提前收口，不调用 OnFinalFailure 递归登记下一条 meta。
- 没有新 source identity 的旧 meta payload 保持原 legacy key/ref 行为，未把旧格式改成依赖新字段才能消费。

独立外置两种故障复现均通过：

```bash
GOCACHE=/private/tmp/alert-review-notify-gocache go test \
  -overlay /private/tmp/alert-iteration-issues/notify-review-overlay.json \
  -tags=integration -race ./internal/app \
  -run 'TestReviewNewEpochFallback|TestReviewMultiTargetFailure' -count=1 -v
# 退出 0，9.954s
```

这两项仍分别严格断言跨 epoch/retry 的真实 Sender 外发次数为 2、不同原目标失败的 pending meta timer 数为 2。未调整为允许合并。日志：`/private/tmp/alert-iteration-issues/notify-meta-fixed.log`。

另外仅执行与本修复直接相关的 focused 回归，没有扩大全套：

```bash
GOCACHE=/private/tmp/alert-review-notify-gocache go test -race \
  ./internal/app ./internal/notify \
  -run 'TestFaultMeta|TestScheduleNotifyFailureMetaAlert' -count=1
# 退出 0：app 1.459s、notify 1.795s

GOCACHE=/private/tmp/alert-review-notify-gocache go test -tags=integration -race \
  ./internal/app -run '^TestFaultNotificationMeta' -count=1 -v
# 退出 0，3.768s

git diff --check 59e3df7 bcb806d
# 退出 0，无输出
```

真实 PG 结果隔离回归验证两个源意图分别发送、重复 payload 不再外发、meta 自身失败不递归、原 delivery 保持 failed、owner_notified_at 保持空。单元回归验证 generation/target 差异、payload 序列化与 round 变化后的身份稳定。日志：`notify-meta-fixed-unit.log`、`notify-meta-fixed-coverage.log`，均位于本审查目录。
