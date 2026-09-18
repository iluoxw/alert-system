-- era3 P2：Fault 成为一等工作单元——归属（owner team）与认领（ack）。
-- owner_team_id 只能来自 resource_nodes.owner_team_id（不变量 15），不得由本迁移或
-- 业务代码从 Alert 标签等不可信输入推断，因此这里只加列不回填历史数据。
ALTER TABLE faults
    ADD COLUMN owner_team_id       UUID REFERENCES teams (id) ON DELETE SET NULL,
    ADD COLUMN acked_by            UUID REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN acked_by_name       TEXT,
    ADD COLUMN acked_at            TIMESTAMPTZ,
    ADD COLUMN owner_notified_at   TIMESTAMPTZ;

CREATE INDEX faults_status_owner_team_idx ON faults (status, owner_team_id);

-- 全库已核实无 fault 时间线表：Alert 用 000001 timeline_entries，Incident 用
-- 000013 incident_timeline_entries，结构在此对齐 incident_timeline_entries。
CREATE TABLE fault_timeline_entries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fault_id        UUID NOT NULL REFERENCES faults (id) ON DELETE CASCADE,
    actor_id        UUID REFERENCES users (id) ON DELETE SET NULL,
    actor_name      TEXT NOT NULL,
    action          TEXT NOT NULL,
    detail          JSONB NOT NULL DEFAULT '{}'::jsonb,
    at              TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX fault_timeline_entries_fault_idx ON fault_timeline_entries (fault_id, at DESC);
