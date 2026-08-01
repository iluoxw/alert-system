-- 000009_flap_history 的完整回滚：删索引。
DROP INDEX IF EXISTS alerts_fingerprint_resolved_idx;
