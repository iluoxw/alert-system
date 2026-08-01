-- 支撑 dedup stage 在 refire（resolved 后同 fingerprint 再次 firing）时延续 flap
-- 基线的查询：alerts_fingerprint_unresolved_uidx（000001）是 partial unique index，
-- WHERE status != 'resolved'，天然不覆盖 resolved 行，无法复用来查"该 fingerprint
-- 最近一条 resolved 历史行"。dedup 是每个事件必经的热路径，这条查询不能退化成
-- 全表顺序扫描，因此新增一条按 (fingerprint, created_at DESC) 的 partial index，
-- 谓词限定 status = 'resolved' 与目标查询完全对齐。
CREATE INDEX alerts_fingerprint_resolved_idx
    ON alerts (fingerprint, created_at DESC) WHERE status = 'resolved';
