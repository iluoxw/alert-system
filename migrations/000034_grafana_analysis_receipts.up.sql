-- Grafana webhook 与对账补漏共用的 occurrence/phase 分析准入收据。
-- 不随任务/报告留存清理，避免迟到 webhook 再次执行同一阶段；删除 Source 时回收。
CREATE TABLE agent_grafana_occurrences (
 source_id UUID NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
 occurrence_ref TEXT NOT NULL,
 phase TEXT NOT NULL CHECK (phase IN ('firing','resolved')),
 recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 PRIMARY KEY(source_id, occurrence_ref, phase)
);
