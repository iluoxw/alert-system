-- 000011_alert_events_notify 的完整回滚：删触发器与函数。
DROP TRIGGER IF EXISTS timeline_entries_notify_changed ON timeline_entries;
DROP TRIGGER IF EXISTS alerts_notify_changed ON alerts;
DROP FUNCTION IF EXISTS notify_alert_changed();
