DROP INDEX IF EXISTS timers_claimed_lease_idx;
ALTER TABLE timers DROP CONSTRAINT IF EXISTS timers_claimed_lease_check;

-- 回滚前必须先清空在途的 claimed 行，否则收紧 CHECK 会失败，而且旧代码不认识
-- 'claimed'，这些定时器会永久卡住。能归还成 pending 的归还（下一轮轮询继续处理），
-- 若同 (kind, ref_id) 已经存在 pending 行（timers_pending_kind_ref_uidx 局部唯一索引
-- 不允许第二条）则只能标 failed，避免回滚本身因唯一冲突失败。
UPDATE timers t SET status = 'failed'
WHERE t.status = 'claimed'
  AND EXISTS (
      SELECT 1 FROM timers p
      WHERE p.status = 'pending' AND p.kind = t.kind AND p.ref_id = t.ref_id
  );
UPDATE timers SET status = 'pending' WHERE status = 'claimed';

ALTER TABLE timers DROP COLUMN IF EXISTS claim_count;
ALTER TABLE timers DROP COLUMN IF EXISTS lease_expires_at;

ALTER TABLE timers DROP CONSTRAINT IF EXISTS timers_status_check;
ALTER TABLE timers
    ADD CONSTRAINT timers_status_check
    CHECK (status IN ('pending', 'fired', 'canceled', 'failed'));
