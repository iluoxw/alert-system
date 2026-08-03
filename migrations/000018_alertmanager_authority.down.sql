DROP INDEX IF EXISTS alerts_source_reconcile_idx;
DROP INDEX IF EXISTS alerts_suppression_state_idx;
DROP INDEX IF EXISTS alerts_response_state_idx;
DROP INDEX IF EXISTS alerts_source_state_idx;
DROP INDEX IF EXISTS alerts_source_occurrence_uidx;
DROP INDEX IF EXISTS alerts_fingerprint_unresolved_uidx;

CREATE UNIQUE INDEX alerts_fingerprint_unresolved_uidx
    ON alerts (fingerprint) WHERE status != 'resolved';

ALTER TABLE alerts
    DROP COLUMN IF EXISTS source_resolution_reason,
    DROP COLUMN IF EXISTS reconcile_miss_count,
    DROP COLUMN IF EXISTS reconcile_generation,
    DROP COLUMN IF EXISTS last_reconciled_at,
    DROP COLUMN IF EXISTS source_updated_at,
    DROP COLUMN IF EXISTS suppression_details,
    DROP COLUMN IF EXISTS suppression_state,
    DROP COLUMN IF EXISTS response_state,
    DROP COLUMN IF EXISTS source_state,
    DROP COLUMN IF EXISTS upstream_fingerprint;

DROP TRIGGER IF EXISTS alert_source_status_set_updated_at ON alert_source_status;
DROP TABLE IF EXISTS alert_source_status;
