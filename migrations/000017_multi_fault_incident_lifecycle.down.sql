DROP INDEX IF EXISTS incident_fault_links_one_primary_uidx;

ALTER TABLE incident_fault_links
    DROP COLUMN IF EXISTS decided_at,
    DROP COLUMN IF EXISTS decided_by,
    DROP COLUMN IF EXISTS disposition_rationale,
    DROP COLUMN IF EXISTS disposition;
