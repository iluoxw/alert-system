-- Alertmanager authority milestone:
--   * integrations(kind='alertmanager') are logical AlertSources;
--   * alerts keep source/response/suppression as independent axes;
--   * webhook and API reconciliation share the occurrence identity
--     (source_integration_id, upstream_fingerprint, starts_at);
--   * alert_source_status stores sync watermarks and health facts only.

CREATE TABLE alert_source_status (
    integration_id       UUID PRIMARY KEY REFERENCES integrations (id) ON DELETE CASCADE,
    last_webhook_at       TIMESTAMPTZ,
    last_reconcile_at     TIMESTAMPTZ,
    last_success_at       TIMESTAMPTZ,
    last_error_at         TIMESTAMPTZ,
    last_error            TEXT,
    consecutive_failures  INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    snapshot_version      BIGINT NOT NULL DEFAULT 0 CHECK (snapshot_version >= 0),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER alert_source_status_set_updated_at
    BEFORE UPDATE ON alert_source_status
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE alerts
    ADD COLUMN upstream_fingerprint TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_state TEXT NOT NULL DEFAULT 'firing'
        CHECK (source_state IN ('firing', 'resolved', 'unknown')),
    ADD COLUMN response_state TEXT NOT NULL DEFAULT 'unacknowledged'
        CHECK (response_state IN ('unacknowledged', 'acknowledged')),
    ADD COLUMN suppression_state TEXT NOT NULL DEFAULT 'none'
        CHECK (suppression_state IN ('none', 'silenced', 'inhibited')),
    ADD COLUMN suppression_details JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN source_updated_at TIMESTAMPTZ,
    ADD COLUMN last_reconciled_at TIMESTAMPTZ,
    ADD COLUMN reconcile_generation BIGINT NOT NULL DEFAULT 0 CHECK (reconcile_generation >= 0),
    ADD COLUMN reconcile_miss_count INTEGER NOT NULL DEFAULT 0 CHECK (reconcile_miss_count >= 0),
    ADD COLUMN source_resolution_reason TEXT;

UPDATE alerts
SET source_state = CASE WHEN status = 'resolved' THEN 'resolved' ELSE 'firing' END,
    response_state = CASE WHEN status = 'acknowledged' THEN 'acknowledged' ELSE 'unacknowledged' END,
    suppression_state = CASE WHEN cardinality(silenced_by) > 0 THEN 'silenced' ELSE 'none' END,
    upstream_fingerprint = CASE
        WHEN source = 'alertmanager' THEN COALESCE(signal_context->>'vendor_fingerprint', '')
        ELSE ''
    END,
    source_updated_at = COALESCE(ends_at, last_seen_at, starts_at);

-- The old active-fingerprint index encoded one local lifecycle for every source.
-- Keep it for compatibility sources only; authoritative Alertmanager occurrences
-- use the upstream identity and may legitimately coexist across sources/startsAt.
DROP INDEX alerts_fingerprint_unresolved_uidx;

CREATE UNIQUE INDEX alerts_fingerprint_unresolved_uidx
    ON alerts (fingerprint)
    WHERE status != 'resolved' AND upstream_fingerprint = '';

CREATE UNIQUE INDEX alerts_source_occurrence_uidx
    ON alerts (source_integration_id, upstream_fingerprint, starts_at)
    WHERE source_integration_id IS NOT NULL AND upstream_fingerprint != '';

CREATE INDEX alerts_source_state_idx ON alerts (source_state, last_seen_at DESC);
CREATE INDEX alerts_response_state_idx ON alerts (response_state, last_seen_at DESC);
CREATE INDEX alerts_suppression_state_idx ON alerts (suppression_state, last_seen_at DESC);
CREATE INDEX alerts_source_reconcile_idx
    ON alerts (source_integration_id, reconcile_generation, source_state)
    WHERE upstream_fingerprint != '';

-- Existing Alertmanager integrations become logical sources without requiring a
-- second registration step. Sources without api_url remain visible as unconfigured
-- and still accept the webhook fast path.
INSERT INTO alert_source_status (integration_id)
SELECT id FROM integrations WHERE kind = 'alertmanager'
ON CONFLICT (integration_id) DO NOTHING;
