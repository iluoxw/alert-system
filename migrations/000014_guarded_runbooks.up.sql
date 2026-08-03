-- Guarded incident runbooks. This is intentionally separate from the legacy
-- alert action_rules/action_runs tables: runbooks are human-driven response
-- operations bound to an Incident, with plans, approvals and execution audit.

CREATE TABLE runbooks (
    action_key      TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    description     TEXT NOT NULL,
    risk_level      TEXT NOT NULL CHECK (risk_level IN ('readonly', 'coordination', 'mutating')),
    enabled         BOOLEAN NOT NULL DEFAULT true,
    uat_only        BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER runbooks_set_updated_at
    BEFORE UPDATE ON runbooks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

INSERT INTO runbooks (action_key, name, description, risk_level, uat_only) VALUES
    ('e2b.stop_invalid_retries', '记录停止无效重试', '记录执行人、工单和停止时间；不直接控制业务方', 'coordination', false),
    ('e2b.snapshot_audit', 'Sandbox 快照只读审计', '对精确目标生成缺对象与状态交叉验证证据', 'readonly', false),
    ('e2b.fc_audit', 'Firecracker 只读审计', '检查精确目标的残留进程与节点容量', 'readonly', false),
    ('e2b.cleanup_dry_run', 'Sandbox 清理预演', '生成带 SHA256 和短时效的精确清理计划', 'readonly', false),
    ('e2b.cleanup_apply', 'Sandbox 精确清理', '仅消费已审批的有效计划并调用受支持的 DELETE API', 'mutating', false),
    ('e2b.lifecycle_verify', 'Sandbox 生命周期验证', '在单独授权的 UAT 模板执行 create→pause→resume→delete', 'mutating', true);

CREATE TABLE runbook_runs (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id         UUID NOT NULL REFERENCES incidents (id) ON DELETE CASCADE,
    action_key          TEXT NOT NULL,
    mode                TEXT NOT NULL CHECK (mode IN ('coordination', 'readonly', 'dry_run', 'apply')),
    status              TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'rejected')),
    parameters          JSONB NOT NULL DEFAULT '{}'::jsonb,
    plan                JSONB NOT NULL DEFAULT '{}'::jsonb,
    plan_sha256         TEXT NOT NULL DEFAULT '',
    plan_expires_at     TIMESTAMPTZ,
    source_plan_run_id  UUID REFERENCES runbook_runs (id) ON DELETE RESTRICT,
    requested_by        UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    executed_by         UUID REFERENCES users (id) ON DELETE RESTRICT,
    requested_at        TIMESTAMPTZ NOT NULL,
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    result              JSONB NOT NULL DEFAULT '{}'::jsonb,
    error               TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX runbook_runs_incident_time_idx
    ON runbook_runs (incident_id, requested_at DESC);
CREATE INDEX runbook_runs_source_plan_idx
    ON runbook_runs (source_plan_run_id) WHERE source_plan_run_id IS NOT NULL;
CREATE UNIQUE INDEX runbook_runs_active_apply_uidx
    ON runbook_runs (source_plan_run_id)
    WHERE action_key = 'e2b.cleanup_apply' AND status IN ('running', 'succeeded');

CREATE TRIGGER runbook_runs_set_updated_at
    BEFORE UPDATE ON runbook_runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE runbook_approvals (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_run_id     UUID NOT NULL REFERENCES runbook_runs (id) ON DELETE CASCADE,
    plan_sha256     TEXT NOT NULL,
    decision        TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
    approved_by     UUID NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    comment         TEXT NOT NULL DEFAULT '',
    approved_at     TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (plan_run_id, approved_by)
);

CREATE INDEX runbook_approvals_plan_decision_idx
    ON runbook_approvals (plan_run_id, decision, approved_at DESC);
