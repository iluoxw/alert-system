-- 独立分析账本；message_id 无级联 FK，角色消息的7天清理不能删除报告审计。
CREATE TABLE oma_analysis_runs (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 message_id UUID NOT NULL UNIQUE,
 role TEXT NOT NULL,
 client_id TEXT NOT NULL,
 source_id UUID NOT NULL,
 event_type TEXT NOT NULL,
 receipt JSONB NOT NULL,
 envelope JSONB NOT NULL,
 state TEXT NOT NULL DEFAULT 'receiving' CHECK(state IN ('receiving','received','awaiting_intent','collecting','awaiting_report','completed','partial','submission_unknown','failed')),
 session_id TEXT NOT NULL DEFAULT '',
 intent JSONB,
 evidence JSONB,
 cursor TEXT NOT NULL DEFAULT '',
 stream_connections INTEGER NOT NULL DEFAULT 0,
 last_stream_at TIMESTAMPTZ,
 lease_owner TEXT NOT NULL DEFAULT '',
 lease_token BIGINT NOT NULL DEFAULT 0,
 lease_expires_at TIMESTAMPTZ,
 attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 reason_code TEXT NOT NULL DEFAULT '',
 alert_ids UUID[] NOT NULL DEFAULT '{}',
 fault_ids UUID[] NOT NULL DEFAULT '{}',
 binding_status TEXT NOT NULL DEFAULT 'pending',
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX oma_analysis_runs_due ON oma_analysis_runs(next_attempt_at,created_at) WHERE state IN ('receiving','received','awaiting_intent','collecting','awaiting_report');
CREATE INDEX oma_analysis_runs_alerts ON oma_analysis_runs USING GIN(alert_ids);
CREATE INDEX oma_analysis_runs_faults ON oma_analysis_runs USING GIN(fault_ids);
CREATE TABLE oma_analysis_actions (
 run_id UUID NOT NULL REFERENCES oma_analysis_runs(id) ON DELETE CASCADE,
 stage TEXT NOT NULL CHECK(stage IN ('create_session','submit_evidence')),
 submission_id TEXT NOT NULL UNIQUE,
 request JSONB NOT NULL,
 status TEXT NOT NULL DEFAULT 'prepared' CHECK(status IN ('prepared','submitting','submitted','unknown')),
 event_after BIGINT NOT NULL DEFAULT 0,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(run_id,stage)
);
CREATE TABLE oma_analysis_events (
 sequence BIGSERIAL PRIMARY KEY,
 run_id UUID NOT NULL REFERENCES oma_analysis_runs(id) ON DELETE CASCADE,
 session_id TEXT NOT NULL,
 event_id TEXT NOT NULL,
 event_type TEXT NOT NULL,
 text TEXT NOT NULL DEFAULT '',
 stop_reason TEXT NOT NULL DEFAULT '',
 processed_at TIMESTAMPTZ,
 received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(session_id,event_id)
);
CREATE TABLE analysis_results (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 run_id UUID NOT NULL UNIQUE REFERENCES oma_analysis_runs(id) ON DELETE CASCADE,
 schema_version INTEGER NOT NULL CHECK(schema_version=1),
 content_hash TEXT NOT NULL,
 report JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
