-- 只迁移结构；历史 owner_notified_at 是意图时间，不可回填成 sent。
CREATE TABLE fault_notification_watches (
 fault_id UUID PRIMARY KEY REFERENCES faults(id) ON DELETE CASCADE,
 epoch BIGINT NOT NULL,
 reason TEXT NOT NULL DEFAULT 'queued',
 next_check_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX fault_notification_watches_due ON fault_notification_watches(next_check_at);
CREATE TABLE fault_notification_deliveries (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 fault_id UUID NOT NULL REFERENCES faults(id) ON DELETE CASCADE,
 epoch BIGINT NOT NULL,
 channel TEXT NOT NULL,
 target_hash TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('queued','sending','sent','failed','covered','cancelled')),
 generation INTEGER NOT NULL DEFAULT 0 CHECK(generation BETWEEN 0 AND 2),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(fault_id,epoch,channel,target_hash)
);
