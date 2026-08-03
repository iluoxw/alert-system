DROP INDEX IF EXISTS fault_alert_links_decided_by_idx;
ALTER TABLE fault_alert_links
    DROP COLUMN IF EXISTS decided_at,
    DROP COLUMN IF EXISTS decided_by;
