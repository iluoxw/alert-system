-- Managed hook 的事务交接点。在每份角色任务均成功入队后，同事务删除批次。
CREATE TABLE agent_batches (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    dedup_key text NOT NULL,
    raw_webhook bytea CHECK (octet_length(raw_webhook) <= 1048576),
    envelope jsonb NOT NULL,
    policies jsonb NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'dead')),
    failures integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    visible_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    failed_at timestamptz
);
CREATE INDEX agent_batches_ready ON agent_batches (visible_at, id) WHERE status = 'pending';
CREATE INDEX agent_batches_group_order ON agent_batches (dedup_key, id) WHERE status = 'pending';
CREATE INDEX agent_batches_retention ON agent_batches (failed_at) WHERE status = 'dead';
