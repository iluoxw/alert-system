-- action_runs.status 增补 'skipped_cooldown' 取值：R12.4 冷却窗口内拒绝重复触发时，
-- 仍需落一条记录以支持下一次冷却判定的边界计算，但它不代表任何真实的动作执行，
-- 与 triggered/succeeded/failed 三个"确实执行过"的取值语义不同（见 action_runs.go
-- LastTriggered 的查询过滤逻辑：冷却判定本身要排除 skipped_cooldown 记录，否则每次
-- 因冷却被拒绝都会产生新记录，导致冷却窗口无限自我延长）。
ALTER TABLE action_runs DROP CONSTRAINT action_runs_status_check;
ALTER TABLE action_runs ADD CONSTRAINT action_runs_status_check
    CHECK (status IN ('triggered', 'succeeded', 'failed', 'skipped_cooldown'));
