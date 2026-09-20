-- entity-lifecycle：受管实体的人工终态。下线不是软删除，历史 Fault / 别名 / 边
-- 仍指向该行；只是退出实时遍历与默认列表。遥测同步不得改这四列（R1.4）。
ALTER TABLE resource_nodes
    ADD COLUMN lifecycle_state     TEXT NOT NULL DEFAULT 'active'
        CHECK (lifecycle_state IN ('active', 'decommissioned')),
    ADD COLUMN decommissioned_at   TIMESTAMPTZ,
    ADD COLUMN decommissioned_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN decommission_reason TEXT;

CREATE INDEX resource_nodes_lifecycle_idx ON resource_nodes (lifecycle_state);
