ALTER TABLE fault_alert_links
    ADD COLUMN decided_by UUID REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN decided_at TIMESTAMPTZ;

CREATE INDEX fault_alert_links_decided_by_idx
    ON fault_alert_links (decided_by) WHERE decided_by IS NOT NULL;
