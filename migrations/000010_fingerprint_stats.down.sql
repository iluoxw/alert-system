-- 000010_fingerprint_stats 的完整回滚：删索引。
DROP INDEX IF EXISTS alerts_fingerprint_starts_idx;
