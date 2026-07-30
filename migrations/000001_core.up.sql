-- Core alert tables: integrations, alerts, alert_events, timeline_entries.
-- PostgreSQL 15+. gen_random_uuid() is built into core since PG13, no extension needed.

-- Shared trigger to keep updated_at current on every UPDATE.
CREATE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- integrations: inbound/outbound adapter registrations (R1, R2.5).
CREATE TABLE integrations (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind              TEXT NOT NULL,
    name              TEXT NOT NULL,
    token_hash        TEXT NOT NULL,
    default_severity  TEXT NOT NULL DEFAULT 'warning'
                      CHECK (default_severity IN ('critical', 'warning', 'info')),
    enabled           BOOLEAN NOT NULL DEFAULT true,
    config            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (name)
);

CREATE TRIGGER integrations_set_updated_at
    BEFORE UPDATE ON integrations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- alerts: deduplicated alerts, one row per firing cycle (R2, R3, R4).
CREATE TABLE alerts (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fingerprint            TEXT NOT NULL,
    status                 TEXT NOT NULL DEFAULT 'firing'
                           CHECK (status IN ('firing', 'acknowledged', 'resolved')),
    severity               TEXT NOT NULL
                           CHECK (severity IN ('critical', 'warning', 'info')),
    severity_fallback      BOOLEAN NOT NULL DEFAULT false,
    title                  TEXT NOT NULL,
    source                 TEXT NOT NULL,
    labels                 JSONB NOT NULL DEFAULT '{}'::jsonb,
    resource_ref           JSONB NOT NULL DEFAULT '{}'::jsonb,
    signal_context         JSONB NOT NULL DEFAULT '{}'::jsonb,
    resolve_policy         TEXT NOT NULL
                           CHECK (resolve_policy IN ('source', 'ttl', 'manual')),
    ttl_seconds            INT,
    source_integration_id  UUID REFERENCES integrations (id) ON DELETE SET NULL,
    dedup_count            INT NOT NULL DEFAULT 1,
    group_key              TEXT,
    team_id                UUID,
    silenced_by            UUID[] NOT NULL DEFAULT '{}',
    acked_by               UUID,
    acked_at               TIMESTAMPTZ,
    starts_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    ends_at                TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- R3.3/R3.4: only one non-resolved alert per fingerprint may exist, so a new
-- firing event can upsert via ON CONFLICT (fingerprint) WHERE status != 'resolved'.
-- Once resolved, the fingerprint is free again for a new round (R3.4).
CREATE UNIQUE INDEX alerts_fingerprint_unresolved_uidx
    ON alerts (fingerprint) WHERE status != 'resolved';

CREATE INDEX alerts_labels_gin_idx ON alerts USING GIN (labels);
CREATE INDEX alerts_resource_ref_gin_idx ON alerts USING GIN (resource_ref);
CREATE INDEX alerts_status_idx ON alerts (status);
CREATE INDEX alerts_severity_idx ON alerts (severity);
CREATE INDEX alerts_team_id_idx ON alerts (team_id);
CREATE INDEX alerts_group_key_idx ON alerts (group_key);

CREATE TRIGGER alerts_set_updated_at
    BEFORE UPDATE ON alerts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- alert_events: raw inbound event queue, consumed via FOR UPDATE SKIP LOCKED (R1.6).
CREATE TABLE alert_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    integration_id  UUID REFERENCES integrations (id) ON DELETE SET NULL,
    alert_id        UUID REFERENCES alerts (id) ON DELETE SET NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'processed', 'failed')),
    payload         JSONB NOT NULL,
    error           TEXT,
    processed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial index matches the worker's consumer query
-- (WHERE status = 'pending' ORDER BY created_at ... FOR UPDATE SKIP LOCKED).
CREATE INDEX alert_events_pending_idx
    ON alert_events (created_at) WHERE status = 'pending';

CREATE INDEX alert_events_alert_id_idx ON alert_events (alert_id);
CREATE INDEX alert_events_integration_id_idx ON alert_events (integration_id);

CREATE TRIGGER alert_events_set_updated_at
    BEFORE UPDATE ON alert_events
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- timeline_entries: append-only, immutable audit trail per alert (R4.6).
CREATE TABLE timeline_entries (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_id    UUID NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL,
    detail      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX timeline_entries_alert_id_idx ON timeline_entries (alert_id, created_at);

-- Table is append-only by convention (application never issues UPDATE); the
-- trigger only exists to satisfy the created_at/updated_at baseline on every table.
CREATE TRIGGER timeline_entries_set_updated_at
    BEFORE UPDATE ON timeline_entries
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
