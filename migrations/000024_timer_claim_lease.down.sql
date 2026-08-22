DROP INDEX IF EXISTS timers_claimed_lease_idx;
ALTER TABLE timers DROP CONSTRAINT IF EXISTS timers_claimed_lease_check;

-- 回滚前必须清空在途的 claimed 行：收紧 CHECK 之前它们必须变回旧代码认识的状态，
-- 否则回滚本身失败、这些定时器也会永久卡住。
--
-- 关键约束：同一个 (kind, ref_id) 可以合法地同时存在**多条** claimed。
-- timers_pending_kind_ref_uidx 只约束 status='pending'，所以第一轮 notify_send 还在
-- 事务外执行（那一行是 claimed）时，同业务元组的新一轮意图会先插入一条新的 pending，
-- 随后被另一个 worker 领取，于是出现两条 claimed 而一条 pending 都没有。
-- "把所有 claimed 一次性改回 pending" 在这种形态下会直接撞唯一索引：
--   ERROR: duplicate key value violates unique constraint "timers_pending_kind_ref_uidx"
--
-- 收口规则：每个 (kind, ref_id) 至多归还一条 pending（且仅当该元组当前没有 pending
-- 行），其余 claimed 一律标 failed。
--
-- 保留哪一条的取舍：按 created_at DESC 取最新插入的那条，与 Schedule 的替换语义
-- （同一业务元组只保留最新意图）一致——多条 claimed 里最新那条携带最新的 Round 与
-- payload，旧轮次的消息内容可能已经过期（告警可能已经被 Ack/Resolve）。id DESC 只是
-- created_at 相同（同一事务内插入）时的兜底，保证结果确定而不依赖物理行序。
-- 被标 failed 的那些是回滚时刻明确放弃的通知意图：唯一索引不允许它们并存为 pending，
-- 留在表里可审计，且后续 repeat/升级链到期会重新驱动出新的发送意图。
WITH keep AS (
    SELECT DISTINCT ON (t.kind, t.ref_id) t.id
    FROM timers t
    WHERE t.status = 'claimed'
      AND NOT EXISTS (
          SELECT 1 FROM timers p
          WHERE p.status = 'pending' AND p.kind = t.kind AND p.ref_id = t.ref_id
      )
    ORDER BY t.kind, t.ref_id, t.created_at DESC, t.id DESC
)
UPDATE timers SET status = 'pending'
WHERE status = 'claimed' AND id IN (SELECT id FROM keep);

-- 上一条语句每个元组至多放行一条，因此这里剩下的 claimed 全部落 failed。
UPDATE timers SET status = 'failed' WHERE status = 'claimed';

ALTER TABLE timers DROP COLUMN IF EXISTS claim_count;
ALTER TABLE timers DROP COLUMN IF EXISTS lease_expires_at;

ALTER TABLE timers DROP CONSTRAINT IF EXISTS timers_status_check;
ALTER TABLE timers
    ADD CONSTRAINT timers_status_check
    CHECK (status IN ('pending', 'fired', 'canceled', 'failed'));
