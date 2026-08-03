-- Complete the first-phase Impact contract: topology creates potential impact,
-- direct service evidence or a human decision can confirm or rule it out.

ALTER TABLE fault_impacts
    DROP CONSTRAINT fault_impacts_state_check;

ALTER TABLE fault_impacts
    ADD CONSTRAINT fault_impacts_state_check
    CHECK (state IN ('potential', 'confirmed', 'ruled_out'));

ALTER TABLE fault_impacts
    ADD COLUMN decided_by UUID REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN decided_at TIMESTAMPTZ;

CREATE INDEX fault_impacts_decided_by_idx
    ON fault_impacts (decided_by) WHERE decided_by IS NOT NULL;
