-- Generic service catalog and deterministic Alert -> Entity -> Fault mapping.
-- This is an expand-only migration over resource_nodes: the existing topology
-- projection keeps working while service registrations get a canonical identity.

ALTER TABLE resource_nodes
    ADD COLUMN entity_key TEXT,
    ADD COLUMN layer TEXT NOT NULL DEFAULT 'infrastructure'
        CHECK (layer IN ('infrastructure', 'platform', 'application')),
    ADD COLUMN managed BOOLEAN NOT NULL DEFAULT false;

UPDATE resource_nodes
SET entity_key = source || ':' || external_ref,
    layer = CASE
        WHEN kind = 'business_system' THEN 'application'
        WHEN kind = 'infrastructure_capability' THEN 'platform'
        ELSE 'infrastructure'
    END;

ALTER TABLE resource_nodes ALTER COLUMN entity_key SET NOT NULL;
CREATE UNIQUE INDEX resource_nodes_entity_key_uidx ON resource_nodes (entity_key);
CREATE INDEX resource_nodes_layer_managed_idx ON resource_nodes (layer, managed, name);

CREATE TABLE service_registrations (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_id           UUID NOT NULL UNIQUE REFERENCES resource_nodes (id) ON DELETE CASCADE,
    owner_team_id       UUID NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    description         TEXT NOT NULL DEFAULT '',
    tier                TEXT NOT NULL DEFAULT 'tier-2'
                        CHECK (tier IN ('tier-0', 'tier-1', 'tier-2', 'tier-3')),
    dashboard_url       TEXT NOT NULL DEFAULT '',
    runbook_url         TEXT NOT NULL DEFAULT '',
    enabled             BOOLEAN NOT NULL DEFAULT true,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX service_registrations_owner_idx
    ON service_registrations (owner_team_id, enabled);

CREATE TRIGGER service_registrations_set_updated_at
    BEFORE UPDATE ON service_registrations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Duplicate alias claims are intentionally representable. Resolution requires
-- exactly one enabled entity; multiple enabled owners become a conflict queue.
CREATE TABLE entity_aliases (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    namespace       TEXT NOT NULL,
    alias_value     TEXT NOT NULL,
    entity_id       UUID NOT NULL REFERENCES resource_nodes (id) ON DELETE CASCADE,
    enabled         BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace, alias_value, entity_id)
);

CREATE INDEX entity_aliases_lookup_idx
    ON entity_aliases (namespace, alias_value) WHERE enabled;
CREATE INDEX entity_aliases_entity_idx ON entity_aliases (entity_id);

CREATE TRIGGER entity_aliases_set_updated_at
    BEFORE UPDATE ON entity_aliases
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE alert_mapping_policies (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                    TEXT NOT NULL UNIQUE,
    priority                INTEGER NOT NULL UNIQUE CHECK (priority >= 0),
    source_integration_id   UUID REFERENCES integrations (id) ON DELETE SET NULL,
    matchers                JSONB NOT NULL DEFAULT '[]'::jsonb,
    target_entity_id        UUID REFERENCES resource_nodes (id) ON DELETE RESTRICT,
    alias_namespace         TEXT NOT NULL DEFAULT '',
    alias_label             TEXT NOT NULL DEFAULT '',
    fault_kind              TEXT NOT NULL,
    fault_scope             TEXT NOT NULL DEFAULT 'capability'
                            CHECK (fault_scope IN ('instance', 'capability')),
    evidence_role           TEXT NOT NULL CHECK (evidence_role IN (
                                'direct_impact', 'cause_candidate', 'trigger', 'amplifier',
                                'symptom', 'supporting', 'excluded'
                            )),
    correlation_labels      JSONB NOT NULL DEFAULT '[]'::jsonb,
    confidence              DOUBLE PRECISION NOT NULL DEFAULT 0.9
                            CHECK (confidence >= 0 AND confidence <= 1),
    enabled                 BOOLEAN NOT NULL DEFAULT true,
    version                 INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        target_entity_id IS NOT NULL OR
        (alias_namespace <> '' AND alias_label <> '')
    )
);

CREATE INDEX alert_mapping_policies_runtime_idx
    ON alert_mapping_policies (enabled, priority);

CREATE TRIGGER alert_mapping_policies_set_updated_at
    BEFORE UPDATE ON alert_mapping_policies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE alert_mapping_results (
    alert_id            UUID PRIMARY KEY REFERENCES alerts (id) ON DELETE CASCADE,
    status              TEXT NOT NULL CHECK (status IN ('mapped', 'unmapped', 'conflict', 'ignored')),
    policy_id           UUID REFERENCES alert_mapping_policies (id) ON DELETE SET NULL,
    entity_id           UUID REFERENCES resource_nodes (id) ON DELETE SET NULL,
    fault_id            UUID REFERENCES faults (id) ON DELETE SET NULL,
    reason_code         TEXT NOT NULL,
    explanation         TEXT NOT NULL,
    correlation_key     TEXT NOT NULL DEFAULT '',
    evaluated_at        TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX alert_mapping_results_status_idx
    ON alert_mapping_results (status, evaluated_at DESC);
CREATE INDEX alert_mapping_results_entity_idx
    ON alert_mapping_results (entity_id, evaluated_at DESC);

CREATE TRIGGER alert_mapping_results_set_updated_at
    BEFORE UPDATE ON alert_mapping_results
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
