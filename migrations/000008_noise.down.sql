-- 000008_noise 的完整回滚：反向删表/删索引/删列。

DROP TABLE IF EXISTS route_storm_state;

DROP INDEX IF EXISTS idx_alerts_route_created;

ALTER TABLE alerts DROP COLUMN IF EXISTS flapping;
ALTER TABLE alerts DROP COLUMN IF EXISTS flap_window_start;
ALTER TABLE alerts DROP COLUMN IF EXISTS flap_count;
ALTER TABLE alerts DROP COLUMN IF EXISTS route_id;

ALTER TABLE routes DROP COLUMN IF EXISTS storm_threshold_per_min;
ALTER TABLE routes DROP COLUMN IF EXISTS group_mode;
