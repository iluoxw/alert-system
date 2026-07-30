-- Silence/change-event/notification-log/timer tables plus the three reserved
-- extension-point tables (annotations, action_rules, action_runs).
-- Reuses set_updated_at() defined in 000001_core.up.sql.

-- silences: label-matcher + time-window maintenance silences (R10).
CREATE TABLE silences (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    matchers    JSONB NOT NULL,
    starts_at   TIMESTAMPTZ NOT NULL,
    ends_at     TIMESTAMPTZ NOT NULL,
    created_by  UUID, -- FK to users(id) once 000002 lands
    comment     TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);

-- Supports the "currently active silences" lookup:
-- WHERE starts_at <= now() AND ends_at >= now().
CREATE INDEX silences_active_window_idx ON silences (ends_at, starts_at);

CREATE TRIGGER silences_set_updated_at
    BEFORE UPDATE ON silences
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- change_events: normalized deploy/config/scale change events (R7.4).
-- kind is adapter-defined and open-ended (deploy/config_change/scale_out/...),
-- so it is left unconstrained rather than CHECK-enumerated.
CREATE TABLE change_events (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind          TEXT NOT NULL,
    resource_ref  JSONB NOT NULL DEFAULT '{}'::jsonb,
    payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at   TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- resource_ref containment lookup (@>) plus descending time order, to support
-- "recent changes for this resource" (R7.5).
CREATE INDEX change_events_resource_ref_gin_idx ON change_events USING GIN (resource_ref);
CREATE INDEX change_events_occurred_at_idx ON change_events (occurred_at DESC);

CREATE TRIGGER change_events_set_updated_at
    BEFORE UPDATE ON change_events
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- notification_log: per-send-attempt record for retries/idempotency (R6.3).
CREATE TABLE notification_log (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_id         UUID REFERENCES alerts (id) ON DELETE SET NULL, -- single-alert notification
    group_key        TEXT, -- grouped notification (R9); exactly one of the two is set
    channel          TEXT NOT NULL,
    target           TEXT NOT NULL,
    step             INT,
    idempotency_key  TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'sent', 'failed')),
    attempts         INT NOT NULL DEFAULT 0,
    last_error       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (alert_id IS NOT NULL OR group_key IS NOT NULL)
);

CREATE UNIQUE INDEX notification_log_idempotency_key_uidx ON notification_log (idempotency_key);
CREATE INDEX notification_log_alert_id_idx ON notification_log (alert_id) WHERE alert_id IS NOT NULL;
CREATE INDEX notification_log_group_key_idx ON notification_log (group_key) WHERE group_key IS NOT NULL;

CREATE TRIGGER notification_log_set_updated_at
    BEFORE UPDATE ON notification_log
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- timers: unified persistent scheduler backing TTL/escalation/group_wait/
-- channel-escalation (R4.4, R5.4, R9.2, R13.2). kind is dispatched to a
-- handler registry at runtime, so it is left unconstrained rather than
-- CHECK-enumerated (new kinds should not require a migration).
CREATE TABLE timers (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind       TEXT NOT NULL,
    ref_id     TEXT NOT NULL, -- alert id or group_key depending on kind; polymorphic, no FK
    fire_at    TIMESTAMPTZ NOT NULL,
    payload    JSONB NOT NULL DEFAULT '{}'::jsonb,
    status     TEXT NOT NULL DEFAULT 'pending'
               CHECK (status IN ('pending', 'fired', 'canceled', 'failed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one pending timer per (kind, ref_id): a repeated Schedule() call
-- replaces the existing pending timer instead of creating a second one.
CREATE UNIQUE INDEX timers_pending_kind_ref_uidx ON timers (kind, ref_id) WHERE status = 'pending';

-- Matches the poller's due-timer query:
-- WHERE status = 'pending' AND fire_at <= now() ... FOR UPDATE SKIP LOCKED.
CREATE INDEX timers_pending_fire_at_idx ON timers (fire_at) WHERE status = 'pending';

CREATE TRIGGER timers_set_updated_at
    BEFORE UPDATE ON timers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- annotations: AI-suggested annotations, reserved for R11 (table only, no logic yet).
-- kind is CHECK-enumerated per the four annotation types named in design.md R11.2.
CREATE TABLE annotations (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    alert_id   UUID NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    source     TEXT NOT NULL CHECK (source IN ('ai', 'human', 'rule')),
    kind       TEXT NOT NULL
               CHECK (kind IN ('dedup_suspect', 'correlation_suspect', 'root_cause_hypothesis', 'summary')),
    content    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX annotations_alert_id_idx ON annotations (alert_id);

CREATE TRIGGER annotations_set_updated_at
    BEFORE UPDATE ON annotations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- action_rules: auto-remediation trigger rules, reserved for R12 (table only, no logic yet).
CREATE TABLE action_rules (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    matchers          JSONB NOT NULL,
    runbook_ref       TEXT NOT NULL,
    risk_level        TEXT NOT NULL CHECK (risk_level IN ('readonly', 'mutating')),
    mode              TEXT NOT NULL CHECK (mode IN ('auto', 'manual_confirm')),
    cooldown_seconds  INT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER action_rules_set_updated_at
    BEFORE UPDATE ON action_rules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- action_runs: auto-remediation trigger history, reserved for R12 (table only, no logic yet).
-- fingerprint is denormalized from alerts.fingerprint: R3.4 means the same
-- fingerprint can span multiple alert rows across a resolve/refire cycle, and
-- the cooldown check (R12.4) must key on fingerprint, not a single alert_id.
CREATE TABLE action_runs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id       UUID NOT NULL REFERENCES action_rules (id),
    alert_id      UUID NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    fingerprint   TEXT NOT NULL,
    triggered_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    status        TEXT NOT NULL DEFAULT 'triggered'
                  CHECK (status IN ('triggered', 'succeeded', 'failed')),
    result        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Cooldown-window check: recent runs for a given rule + fingerprint, newest first.
CREATE INDEX action_runs_cooldown_idx ON action_runs (rule_id, fingerprint, triggered_at DESC);
CREATE INDEX action_runs_alert_id_idx ON action_runs (alert_id);

CREATE TRIGGER action_runs_set_updated_at
    BEFORE UPDATE ON action_runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
