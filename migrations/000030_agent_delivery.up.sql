-- 分析任务只记录外部客户端的投递状态，与 alerts / faults 的处置状态独立。
CREATE TABLE agent_messages (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    role text NOT NULL,
    dedup_key text,
    envelope jsonb NOT NULL,
    snapshot_at timestamptz NOT NULL,
    route_id text NOT NULL,
    config_version text NOT NULL,
    priority integer NOT NULL,
    max_attempts integer NOT NULL CHECK (max_attempts > 0),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND max_attempts),
    dup_count bigint NOT NULL DEFAULT 1,
    ack_mode text NOT NULL DEFAULT 'on_receive' CHECK (ack_mode = 'on_receive'),
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','leased','acked','dead','expired')),
    lease_id uuid,
    lease_owner text,
    lease_expires_at timestamptz,
    visible_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    settled_at timestamptz,
    CHECK ((status = 'leased') = (lease_expires_at IS NOT NULL)),
    CHECK ((status IN ('leased','acked')) = (lease_id IS NOT NULL)),
    CHECK ((lease_id IS NULL) = (lease_owner IS NULL)),
    CHECK ((status IN ('acked','dead','expired')) = (settled_at IS NOT NULL))
);
CREATE UNIQUE INDEX agent_messages_active_key ON agent_messages(role, dedup_key)
    WHERE status IN ('pending','leased') AND dedup_key IS NOT NULL;
CREATE INDEX agent_messages_claim ON agent_messages(role, priority DESC, created_at, id) WHERE status = 'pending';
CREATE INDEX agent_messages_ttl ON agent_messages(expires_at) WHERE status IN ('pending','leased');
CREATE INDEX agent_messages_lease ON agent_messages(lease_expires_at) WHERE status = 'leased';
CREATE INDEX agent_messages_retention ON agent_messages(settled_at) WHERE settled_at IS NOT NULL;

CREATE TABLE agent_consumers (
    client_id text NOT NULL,
    role text NOT NULL,
    version text NOT NULL,
    last_seen_at timestamptz NOT NULL,
    PRIMARY KEY (role, client_id)
);
CREATE TABLE agent_message_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id uuid NOT NULL REFERENCES agent_messages(id) ON DELETE CASCADE,
    at timestamptz NOT NULL DEFAULT clock_timestamp(),
    kind text NOT NULL,
    from_status text,
    to_status text NOT NULL,
    actor text NOT NULL,
    detail jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX agent_message_events_history ON agent_message_events(message_id, id);
