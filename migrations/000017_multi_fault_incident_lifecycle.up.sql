-- Multi-Fault Incident lifecycle: diagnostic focus and response inclusion are
-- independent, auditable dimensions. Relationships remain append-preserving.

ALTER TABLE incident_fault_links
    ADD COLUMN disposition TEXT NOT NULL DEFAULT 'included'
        CHECK (disposition IN ('included', 'excluded')),
    ADD COLUMN disposition_rationale TEXT NOT NULL DEFAULT '',
    ADD COLUMN decided_by UUID REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN decided_at TIMESTAMPTZ;

CREATE UNIQUE INDEX incident_fault_links_one_primary_uidx
    ON incident_fault_links (incident_id)
    WHERE relation = 'primary';
