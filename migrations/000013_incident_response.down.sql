DROP INDEX IF EXISTS notification_log_incident_id_idx;
ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_exactly_one_resource;
ALTER TABLE notification_log DROP COLUMN IF EXISTS incident_id;
ALTER TABLE notification_log
    ADD CONSTRAINT notification_log_check CHECK (alert_id IS NOT NULL OR group_key IS NOT NULL);

DROP TABLE IF EXISTS recovery_criteria;
DROP TABLE IF EXISTS incident_timeline_entries;
DROP TABLE IF EXISTS incident_tasks;
DROP TABLE IF EXISTS incident_roles;
DROP TABLE IF EXISTS incident_fault_links;
DROP TABLE IF EXISTS incidents;
