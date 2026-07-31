-- serviceability R3: 渠道扩展预留位。统一通知网关/飞书直连到位时
-- ContactResolver（userID+kind→地址）直接可用，无需再迁移。
ALTER TABLE users ADD COLUMN lark_open_id TEXT;
