-- 飞书卡片动作幂等与审计。动作在业务事务中先占位，业务成功才随事务提交；
-- 失败会回滚占位，飞书重试可以安全重做。
CREATE TABLE lark_card_actions (
    request_id       TEXT PRIMARY KEY,
    resource_type    TEXT NOT NULL CHECK (resource_type IN ('alert', 'fault', 'incident')),
    resource_id      UUID NOT NULL,
    action           TEXT NOT NULL,
    operator_lark_id TEXT NOT NULL,
    operator_user_id UUID REFERENCES users (id) ON DELETE SET NULL,
    payload          JSONB NOT NULL DEFAULT '{}'::jsonb,
    result           JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX lark_card_actions_resource_idx
    ON lark_card_actions (resource_type, resource_id, created_at DESC);

CREATE UNIQUE INDEX users_lark_open_id_uidx
    ON users (lark_open_id) WHERE lark_open_id IS NOT NULL;
