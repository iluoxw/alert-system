-- era3 P1 第二批：ccloud 拓扑同步的待对齐身份队列，与 dependency_edges 的
-- “统计窗口无流量”标记。topology_sync_runs / resource_nodes / dependency_edges
-- 已在 000012 建立并被 ImportSnapshot 复用，本迁移不重建，只按需追加。

-- 未对齐 RELY_NODE（不变量 15/23）：不得建实体，只落待人工认领的身份队列。
-- value 必须是 node.key（不是 host:port），否则多个 ":0" 会互相覆盖同一主键行
-- （design.md §迁移 000027 已论证）。UPSERT 靠 (namespace, value) 主键幂等收敛。
CREATE TABLE unresolved_identities (
    namespace       TEXT NOT NULL,
    value           TEXT NOT NULL,
    host_port       TEXT,
    service_type    TEXT,
    first_seen      TIMESTAMPTZ NOT NULL,
    last_seen       TIMESTAMPTZ NOT NULL,
    sample_app      TEXT,
    PRIMARY KEY (namespace, value)
);

-- ADR-0006：平台 totalCount=0 表示统计窗口内无流量，语义不同于 observed_at
-- 过期（数据陈旧）。二者都只能把遍历结果降级为 stale，不能互相替代，因此
-- 显式落列而不是复用 observed_at 动态判断。
ALTER TABLE dependency_edges
    ADD COLUMN no_recent_traffic BOOLEAN NOT NULL DEFAULT false;
