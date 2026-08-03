-- Infrastructure-first incident response foundation: topology projection,
-- technical faults, explainable alert evidence and blast-radius impacts.
-- Depends on 000001 (alerts + set_updated_at()).

CREATE TABLE topology_sync_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source          TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('running', 'completed', 'failed')),
    observed_at     TIMESTAMPTZ NOT NULL,
    snapshot_hash   TEXT NOT NULL,
    node_count      INTEGER NOT NULL DEFAULT 0 CHECK (node_count >= 0),
    edge_count      INTEGER NOT NULL DEFAULT 0 CHECK (edge_count >= 0),
    error           TEXT,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX topology_sync_runs_source_observed_idx
    ON topology_sync_runs (source, observed_at DESC);

CREATE TRIGGER topology_sync_runs_set_updated_at
    BEFORE UPDATE ON topology_sync_runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE resource_nodes (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source              TEXT NOT NULL,
    external_ref        TEXT NOT NULL,
    kind                TEXT NOT NULL CHECK (kind IN ('resource', 'infrastructure_capability', 'business_system')),
    name                TEXT NOT NULL,
    provider            TEXT NOT NULL DEFAULT '',
    environment         TEXT NOT NULL DEFAULT '',
    owner_team_id       UUID REFERENCES teams (id) ON DELETE SET NULL,
    labels              JSONB NOT NULL DEFAULT '{}'::jsonb,
    attributes          JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_observed_at    TIMESTAMPTZ NOT NULL,
    last_sync_run_id    UUID REFERENCES topology_sync_runs (id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source, external_ref)
);

CREATE INDEX resource_nodes_kind_idx ON resource_nodes (kind);
CREATE INDEX resource_nodes_labels_gin_idx ON resource_nodes USING GIN (labels);
CREATE INDEX resource_nodes_last_observed_idx ON resource_nodes (last_observed_at DESC);

CREATE TRIGGER resource_nodes_set_updated_at
    BEFORE UPDATE ON resource_nodes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Direction: from_node consumes/depends on to_node. Blast radius walks the
-- reverse direction from a failed provider to its consumers.
CREATE TABLE dependency_edges (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source          TEXT NOT NULL,
    from_node_id    UUID NOT NULL REFERENCES resource_nodes (id) ON DELETE CASCADE,
    to_node_id      UUID NOT NULL REFERENCES resource_nodes (id) ON DELETE CASCADE,
    relation        TEXT NOT NULL CHECK (relation IN ('depends_on', 'part_of')),
    criticality     TEXT NOT NULL DEFAULT 'medium'
                    CHECK (criticality IN ('low', 'medium', 'high', 'critical')),
    evidence        JSONB NOT NULL DEFAULT '{}'::jsonb,
    observed_at     TIMESTAMPTZ NOT NULL,
    sync_run_id     UUID NOT NULL REFERENCES topology_sync_runs (id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_node_id <> to_node_id),
    UNIQUE (source, from_node_id, to_node_id, relation)
);

CREATE INDEX dependency_edges_provider_idx
    ON dependency_edges (source, to_node_id, relation);
CREATE INDEX dependency_edges_consumer_idx
    ON dependency_edges (source, from_node_id, relation);

CREATE TRIGGER dependency_edges_set_updated_at
    BEFORE UPDATE ON dependency_edges
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE faults (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    correlation_key     TEXT NOT NULL,
    scope               TEXT NOT NULL CHECK (scope IN ('instance', 'capability')),
    status              TEXT NOT NULL CHECK (status IN ('candidate', 'active', 'monitoring', 'resolved')),
    severity            TEXT NOT NULL CHECK (severity IN ('critical', 'warning', 'info')),
    kind                TEXT NOT NULL,
    title               TEXT NOT NULL,
    summary             TEXT NOT NULL DEFAULT '',
    resource_node_id    UUID REFERENCES resource_nodes (id) ON DELETE SET NULL,
    entity_ref          TEXT NOT NULL DEFAULT '',
    confidence          DOUBLE PRECISION NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    metadata            JSONB NOT NULL DEFAULT '{}'::jsonb,
    first_detected_at   TIMESTAMPTZ NOT NULL,
    last_evaluated_at   TIMESTAMPTZ NOT NULL,
    resolved_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX faults_active_correlation_key_uidx
    ON faults (correlation_key) WHERE status <> 'resolved';
CREATE INDEX faults_status_severity_idx ON faults (status, severity);
CREATE INDEX faults_resource_node_idx ON faults (resource_node_id);

CREATE TRIGGER faults_set_updated_at
    BEFORE UPDATE ON faults
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE fault_alert_links (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fault_id            UUID NOT NULL REFERENCES faults (id) ON DELETE CASCADE,
    alert_id            UUID NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    role                TEXT NOT NULL CHECK (role IN (
                            'direct_impact', 'cause_candidate', 'trigger', 'amplifier',
                            'symptom', 'supporting', 'excluded'
                        )),
    rationale           TEXT NOT NULL,
    confidence          DOUBLE PRECISION NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    decision_origin     TEXT NOT NULL CHECK (decision_origin IN ('automation', 'human')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (fault_id, alert_id)
);

CREATE INDEX fault_alert_links_alert_idx ON fault_alert_links (alert_id);
CREATE INDEX fault_alert_links_fault_role_idx ON fault_alert_links (fault_id, role);

CREATE TRIGGER fault_alert_links_set_updated_at
    BEFORE UPDATE ON fault_alert_links
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE fault_relations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    from_fault_id   UUID NOT NULL REFERENCES faults (id) ON DELETE CASCADE,
    to_fault_id     UUID NOT NULL REFERENCES faults (id) ON DELETE CASCADE,
    relation        TEXT NOT NULL CHECK (relation IN ('contributes_to')),
    rationale       TEXT NOT NULL,
    confidence      DOUBLE PRECISION NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_fault_id <> to_fault_id),
    UNIQUE (from_fault_id, to_fault_id, relation)
);

CREATE INDEX fault_relations_to_idx ON fault_relations (to_fault_id, relation);

CREATE TRIGGER fault_relations_set_updated_at
    BEFORE UPDATE ON fault_relations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE fault_impacts (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fault_id            UUID NOT NULL REFERENCES faults (id) ON DELETE CASCADE,
    node_id             UUID NOT NULL REFERENCES resource_nodes (id) ON DELETE CASCADE,
    state               TEXT NOT NULL CHECK (state IN ('potential', 'confirmed')),
    path                JSONB NOT NULL DEFAULT '[]'::jsonb,
    rationale           TEXT NOT NULL,
    confidence          DOUBLE PRECISION NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    decision_origin     TEXT NOT NULL CHECK (decision_origin IN ('automation', 'human')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (fault_id, node_id)
);

CREATE INDEX fault_impacts_node_idx ON fault_impacts (node_id, state);

CREATE TRIGGER fault_impacts_set_updated_at
    BEFORE UPDATE ON fault_impacts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
