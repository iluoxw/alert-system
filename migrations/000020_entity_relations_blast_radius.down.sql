DROP INDEX IF EXISTS dependency_edges_consumer_global_idx;
DROP INDEX IF EXISTS dependency_edges_provider_global_idx;

DELETE FROM dependency_edges WHERE origin = 'declared';

ALTER TABLE dependency_edges
    DROP CONSTRAINT IF EXISTS dependency_edges_origin_sync_ck,
    DROP COLUMN IF EXISTS origin,
    ALTER COLUMN sync_run_id SET NOT NULL;
