DROP TABLE IF EXISTS fault_timeline_entries;

DROP INDEX IF EXISTS faults_status_owner_team_idx;

ALTER TABLE faults
    DROP COLUMN IF EXISTS owner_notified_at,
    DROP COLUMN IF EXISTS acked_at,
    DROP COLUMN IF EXISTS acked_by_name,
    DROP COLUMN IF EXISTS acked_by,
    DROP COLUMN IF EXISTS owner_team_id;
