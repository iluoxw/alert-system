-- Evolve observed topology edges into the shared EntityRelation store. Snapshot
-- edges remain tied to sync runs; service-owner declarations are durable config.

ALTER TABLE dependency_edges
    ALTER COLUMN sync_run_id DROP NOT NULL,
    ADD COLUMN origin TEXT NOT NULL DEFAULT 'observed'
        CHECK (origin IN ('observed', 'declared'));

ALTER TABLE dependency_edges
    ADD CONSTRAINT dependency_edges_origin_sync_ck CHECK (
        (origin = 'observed' AND sync_run_id IS NOT NULL) OR
        (origin = 'declared' AND sync_run_id IS NULL)
    );

-- Existing indexes lead with source and cannot serve canonical cross-source
-- traversal. These indexes keep both reverse blast-radius and forward provider
-- closure bounded by relation type.
CREATE INDEX dependency_edges_provider_global_idx
    ON dependency_edges (to_node_id, relation, observed_at DESC);
CREATE INDEX dependency_edges_consumer_global_idx
    ON dependency_edges (from_node_id, relation, observed_at DESC);
