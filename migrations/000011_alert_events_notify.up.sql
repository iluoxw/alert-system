-- 告警变更实时推送（SSE）的事件源：用数据库触发器在事务提交时通过 pg_notify 广播
-- 变更的告警 ID，而不是让应用层每处写入代码自己发布事件。选触发器而非应用层发布的
-- 理由：写入 alerts/timeline_entries 的路径有三类（管道 stage、REST 处置端点、
-- scheduler handler），应用层发布要在三处都手动补代码且容易漏；触发器在表级别
-- 一次性兜住全部写入方。pg_notify 只在事务提交后才真正投递给 LISTEN 端，天然满足
-- CLAUDE.md 不变量 4"持锁事务内禁止外部 IO"——通知本身不是同步 IO，是提交后异步
-- 广播的信号，且未来多副本部署时每个副本各自 LISTEN 即可获得推送，不需要额外的
-- 应用层分发机制。

CREATE OR REPLACE FUNCTION notify_alert_changed() RETURNS trigger AS $$
BEGIN
    IF TG_TABLE_NAME = 'timeline_entries' THEN
        PERFORM pg_notify('alert_changed', NEW.alert_id::text);
    ELSE
        PERFORM pg_notify('alert_changed', NEW.id::text);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER alerts_notify_changed
    AFTER INSERT OR UPDATE ON alerts
    FOR EACH ROW EXECUTE FUNCTION notify_alert_changed();

-- 时间线追加（如处置动作、备注）也要刷新告警详情页，即使 alerts 表本身当次未必
-- 有变更（例如 addNote 只写 timeline_entries）。
CREATE TRIGGER timeline_entries_notify_changed
    AFTER INSERT ON timeline_entries
    FOR EACH ROW EXECUTE FUNCTION notify_alert_changed();
