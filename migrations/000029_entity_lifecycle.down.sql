DROP INDEX IF EXISTS resource_nodes_lifecycle_idx;

ALTER TABLE resource_nodes
    DROP COLUMN IF EXISTS decommission_reason,
    DROP COLUMN IF EXISTS decommissioned_by,
    DROP COLUMN IF EXISTS decommissioned_at,
    DROP COLUMN IF EXISTS lifecycle_state;
