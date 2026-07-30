-- Org/routing/policy tables: teams, users, memberships, routes, schedules,
-- shifts, escalation_policies, notification_policies.
-- Depends on 000001 for the shared set_updated_at() trigger function.

-- teams: routing targets and RBAC scope (R5, R8).
CREATE TABLE teams (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER teams_set_updated_at
    BEFORE UPDATE ON teams
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- users: local accounts; SSO/OIDC is a future authn adapter (R8.1).
CREATE TABLE users (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username       TEXT NOT NULL UNIQUE,
    password_hash  TEXT NOT NULL,
    display_name   TEXT NOT NULL,
    email          TEXT,
    enabled        BOOLEAN NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- memberships: user<->team RBAC binding (R8.2: admin manages config, member handles alerts).
CREATE TABLE memberships (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    team_id     UUID NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    role        TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (team_id, user_id)
);

CREATE INDEX memberships_user_id_idx ON memberships (user_id);

CREATE TRIGGER memberships_set_updated_at
    BEFORE UPDATE ON memberships
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- routes: ordered rule list matching labels/resource_ref to a target team (R5.1-5.3),
-- plus per-route grouping windows consumed by the grouping stage (R9).
CREATE TABLE routes (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    priority                 INTEGER NOT NULL UNIQUE,
    matchers                 JSONB NOT NULL DEFAULT '[]'::jsonb,
    target_team_id           UUID NOT NULL REFERENCES teams (id) ON DELETE RESTRICT,
    group_by                 JSONB NOT NULL DEFAULT '[]'::jsonb,
    group_wait_seconds       INTEGER NOT NULL DEFAULT 30,
    group_interval_seconds   INTEGER NOT NULL DEFAULT 300,
    repeat_interval_seconds  INTEGER NOT NULL DEFAULT 14400,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX routes_target_team_id_idx ON routes (target_team_id);

CREATE TRIGGER routes_set_updated_at
    BEFORE UPDATE ON routes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- schedules: one on-call schedule per team, evaluated in a fixed timezone (R5.5).
CREATE TABLE schedules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id     UUID NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    timezone    TEXT NOT NULL DEFAULT 'UTC',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (team_id, name)
);

CREATE INDEX schedules_team_id_idx ON schedules (team_id);

CREATE TRIGGER schedules_set_updated_at
    BEFORE UPDATE ON schedules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- shifts: materialized on-call periods (not a recurrence rule), so "who is
-- on-call at time T" is a plain range query (R5.5).
CREATE TABLE shifts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    schedule_id  UUID NOT NULL REFERENCES schedules (id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    starts_at    TIMESTAMPTZ NOT NULL,
    ends_at      TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);

-- Supports: WHERE schedule_id = ? AND starts_at <= :t AND ends_at > :t
CREATE INDEX shifts_schedule_time_idx ON shifts (schedule_id, starts_at, ends_at);

CREATE TRIGGER shifts_set_updated_at
    BEFORE UPDATE ON shifts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- escalation_policies: per-team escalation chain, levels = [{wait_seconds, targets}] (R5.4).
CREATE TABLE escalation_policies (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id     UUID NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    levels      JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX escalation_policies_team_id_idx ON escalation_policies (team_id);

CREATE TRIGGER escalation_policies_set_updated_at
    BEFORE UPDATE ON escalation_policies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- notification_policies: per-severity channel escalation sequences (R13.1-13.2).
-- Binding precedence (R13.5), resolved by the PolicyEngine at read time:
--   matchers hit -> team default (team_id set) -> global default (team_id NULL).
-- steps shape: {"critical": [{"wait_seconds": 0, "channels": [...], "mode": "replace|spread"}], "warning": [...], "info": [...]}
CREATE TABLE notification_policies (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    matchers    JSONB NOT NULL DEFAULT '[]'::jsonb,
    team_id     UUID REFERENCES teams (id) ON DELETE CASCADE,
    steps       JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX notification_policies_team_id_idx ON notification_policies (team_id);

CREATE TRIGGER notification_policies_set_updated_at
    BEFORE UPDATE ON notification_policies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
