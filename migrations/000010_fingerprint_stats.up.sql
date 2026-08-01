-- 支撑告警详情页「噪声画像」（ui-enrichment）需要的两条统计查询：同 fingerprint
-- 近 N 天发生次数、已解决历史的平均持续时长与最近记录。alerts_fingerprint_unresolved_uidx
-- （000001）是 WHERE status != 'resolved' 的 partial unique index，只覆盖当前未
-- resolved 的那一行，无法服务"扫描该 fingerprint 全部历史行（含多条 resolved）"
-- 这类查询，因此新增一条不带谓词、覆盖全部状态的 (fingerprint, starts_at) 索引。
CREATE INDEX alerts_fingerprint_starts_idx ON alerts (fingerprint, starts_at);
