-- notify_send 这类「handler 内部必须做外部 IO」的定时器不能在 scheduler 的 per-timer
-- 事务里执行：HTTP/SMTP 发送与指数退避重试期间会一直持有 timers 行锁和一个数据库连接。
-- 这里给 timers 增加一个非终态的中间状态 'claimed' 加租约列，让 worker 先在一个短事务里
-- 提交「这条我领走了」，释放事务与行锁之后再执行发送。
--
-- 'claimed' 故意不是终态：租约到期后这条定时器可以被任意 worker 重新领取，所以进程在
-- 发送途中崩溃不会永久丢通知（at-least-once；重复发送由 notification_log 的幂等键收敛）。
-- 反过来，租约未到期时它既不是 pending 也不是过期 claimed，并发 worker 的轮询查询看不到它，
-- 同一条定时器不会被两个 worker 同时消费。
ALTER TABLE timers DROP CONSTRAINT timers_status_check;
ALTER TABLE timers
    ADD CONSTRAINT timers_status_check
    CHECK (status IN ('pending', 'claimed', 'fired', 'canceled', 'failed'));

-- lease_expires_at 是当前 claim 的租约到期时刻。
-- claim_count 是这条定时器被领取过的次数，同时充当 fencing token：worker 回写最终状态时
-- 要求 claim_count 仍等于自己领取时看到的值，这样「租约已过期、已被另一个 worker 重新领取」
-- 之后，上一个 worker 迟到的回写会落空而不是覆盖新一轮的结果。
ALTER TABLE timers ADD COLUMN lease_expires_at TIMESTAMPTZ;
ALTER TABLE timers ADD COLUMN claim_count INT NOT NULL DEFAULT 0;

ALTER TABLE timers
    ADD CONSTRAINT timers_claimed_lease_check
    CHECK (status <> 'claimed' OR lease_expires_at IS NOT NULL);

-- 匹配轮询查询里「租约已过期的 claimed 行」这一分支：
-- WHERE status = 'claimed' AND lease_expires_at <= now()
CREATE INDEX timers_claimed_lease_idx ON timers (lease_expires_at) WHERE status = 'claimed';
