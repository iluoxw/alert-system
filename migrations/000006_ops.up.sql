-- ops-enhancements: R1 severity-scoped escalation cadence + R2 per-team
-- operational cron settings (daily report / stale sweep / on-call broadcast).
-- Reuses set_updated_at() defined in 000001_core.up.sql.

-- R1: escalation_policies 按 severity 覆写升级节奏。'{}' 表示无覆写，
-- effectiveWaitSeconds 恒回退 levels[i].wait_seconds，存量策略行为不变。
ALTER TABLE escalation_policies
    ADD COLUMN severity_config JSONB NOT NULL DEFAULT '{}';

-- R2: 团队级运营周期任务配置（日报/悬置扫描/值班播报），每团队一行。
CREATE TABLE team_ops_settings (
    id                            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id                       UUID NOT NULL UNIQUE REFERENCES teams (id) ON DELETE CASCADE,
    timezone                      TEXT NOT NULL DEFAULT 'Asia/Shanghai',
    daily_report_enabled          BOOLEAN NOT NULL DEFAULT false,
    daily_report_time             TEXT NOT NULL DEFAULT '09:00', -- HH:MM，按 timezone 解释
    stale_sweep_enabled           BOOLEAN NOT NULL DEFAULT false,
    stale_sweep_interval_seconds  INT NOT NULL DEFAULT 1800 CHECK (stale_sweep_interval_seconds >= 60),
    oncall_broadcast_enabled      BOOLEAN NOT NULL DEFAULT false,
    oncall_broadcast_time         TEXT NOT NULL DEFAULT '09:30',
    channels                      JSONB NOT NULL DEFAULT '[]', -- [{"kind":"lark","target":"..."}]
    created_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER team_ops_settings_set_updated_at
    BEFORE UPDATE ON team_ops_settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
