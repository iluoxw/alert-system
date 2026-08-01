-- aiops-foundation 确定性降噪地基（design.md「Data Models」）：路由分组模式/风暴阈值、
-- 告警路由归属与 flapping 标记、风暴熔断状态表。只加列建表，不动存量数据。

-- routes: 窗口全归并分组模式（R1，C1）与风暴熔断阈值（R3，C3）；均 opt-in，
-- 默认值等价于现状（group_mode='label' 走既有 label 分组，storm_threshold_per_min=0 关闭熔断）。
ALTER TABLE routes ADD COLUMN group_mode TEXT NOT NULL DEFAULT 'label';
ALTER TABLE routes ADD COLUMN storm_threshold_per_min INTEGER NOT NULL DEFAULT 0;

-- alerts: route_id 由 route stage 写入（R5.1 既有职责扩展），供风暴计数按路由统计
-- （避免 group_key LIKE 前缀扫描）；存量行为 NULL，不回填。flap_* 三列是 R4 抖动防抖状态。
ALTER TABLE alerts ADD COLUMN route_id UUID;
ALTER TABLE alerts ADD COLUMN flap_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE alerts ADD COLUMN flap_window_start TIMESTAMPTZ;
ALTER TABLE alerts ADD COLUMN flapping BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX idx_alerts_route_created ON alerts (route_id, created_at);

-- route_storm_state: 每路由的风暴熔断状态（C3），行按 route_id 保留不删——
-- 进入/解除的时刻本身就是可查询的历史记录（AC3.2），重复进入时 UPSERT 覆盖。
CREATE TABLE route_storm_state (
    route_id          UUID PRIMARY KEY,
    active            BOOLEAN NOT NULL,
    activated_at      TIMESTAMPTZ NOT NULL,
    deactivated_at    TIMESTAMPTZ,
    suppressed_count  INTEGER NOT NULL DEFAULT 0,
    last_summary_at   TIMESTAMPTZ
);
