-- Incident collaboration plane: human declaration, roles/tasks, append-only
-- response timeline, recovery gates and reliable incident notifications.

CREATE TABLE incidents (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    number                      BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    status                      TEXT NOT NULL CHECK (status IN ('declared', 'mitigating', 'monitoring', 'resolved')),
    severity                    TEXT NOT NULL CHECK (severity IN ('critical', 'warning', 'info')),
    title                       TEXT NOT NULL,
    summary                     TEXT NOT NULL DEFAULT '',
    declared_by                 UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    communication_channel       TEXT NOT NULL,
    communication_target_ref    TEXT NOT NULL,
    declared_at                 TIMESTAMPTZ NOT NULL,
    mitigated_at                TIMESTAMPTZ,
    monitoring_at               TIMESTAMPTZ,
    resolved_at                 TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX incidents_status_severity_idx ON incidents (status, severity, declared_at DESC);

CREATE TRIGGER incidents_set_updated_at
    BEFORE UPDATE ON incidents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE incident_fault_links (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id     UUID NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    fault_id        UUID NOT NULL REFERENCES faults (id) ON DELETE RESTRICT,
    relation        TEXT NOT NULL CHECK (relation IN ('primary', 'supporting')),
    linked_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (incident_id, fault_id),
    UNIQUE (fault_id)
);

CREATE INDEX incident_fault_links_incident_idx ON incident_fault_links (incident_id, relation);

CREATE TABLE incident_roles (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id     UUID NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    role            TEXT NOT NULL CHECK (role IN (
                        'incident_commander', 'technical_lead',
                        'communications_lead', 'business_liaison'
                    )),
    user_id         UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    assigned_by     UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    assigned_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (incident_id, role)
);

CREATE INDEX incident_roles_user_idx ON incident_roles (user_id);

CREATE TRIGGER incident_roles_set_updated_at
    BEFORE UPDATE ON incident_roles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE incident_tasks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id     UUID NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    title           TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'open'
                    CHECK (status IN ('open', 'in_progress', 'done', 'canceled')),
    owner_user_id   UUID REFERENCES users (id) ON DELETE SET NULL,
    created_by      UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    due_at          TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX incident_tasks_incident_status_idx ON incident_tasks (incident_id, status, created_at);

CREATE TRIGGER incident_tasks_set_updated_at
    BEFORE UPDATE ON incident_tasks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE incident_timeline_entries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id     UUID NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    actor_user_id   UUID REFERENCES users (id) ON DELETE SET NULL,
    actor_display   TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN (
                        'fact', 'hypothesis', 'decision', 'automation', 'communication',
                        'status_change', 'task_update', 'role_assignment'
                    )),
    summary         TEXT NOT NULL,
    detail          JSONB NOT NULL DEFAULT '{}'::jsonb,
    dedupe_key      TEXT,
    occurred_at     TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX incident_timeline_incident_time_idx
    ON incident_timeline_entries (incident_id, occurred_at, created_at);
CREATE UNIQUE INDEX incident_timeline_dedupe_uidx
    ON incident_timeline_entries (incident_id, dedupe_key) WHERE dedupe_key IS NOT NULL;

CREATE TABLE recovery_criteria (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id     UUID NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    key             TEXT NOT NULL,
    description     TEXT NOT NULL,
    gate            TEXT NOT NULL CHECK (gate IN ('mitigation', 'resolution')),
    required        BOOLEAN NOT NULL DEFAULT true,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'passed', 'failed')),
    evidence        JSONB NOT NULL DEFAULT '{}'::jsonb,
    evaluated_by    UUID REFERENCES users (id) ON DELETE SET NULL,
    observed_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (incident_id, key)
);

CREATE INDEX recovery_criteria_incident_gate_idx ON recovery_criteria (incident_id, gate, required, status);

CREATE TRIGGER recovery_criteria_set_updated_at
    BEFORE UPDATE ON recovery_criteria
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Reuse the reliable notification_log/outbox for Incident communications.
ALTER TABLE notification_log DROP CONSTRAINT notification_log_check;
ALTER TABLE notification_log
    ADD COLUMN incident_id UUID REFERENCES incidents (id) ON DELETE SET NULL;
ALTER TABLE notification_log
    ADD CONSTRAINT notification_log_exactly_one_resource
    CHECK (num_nonnulls(alert_id, group_key, incident_id) = 1);
CREATE INDEX notification_log_incident_id_idx
    ON notification_log (incident_id) WHERE incident_id IS NOT NULL;
