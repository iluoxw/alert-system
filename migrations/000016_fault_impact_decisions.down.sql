DROP INDEX IF EXISTS fault_impacts_decided_by_idx;

ALTER TABLE fault_impacts
    DROP COLUMN IF EXISTS decided_at,
    DROP COLUMN IF EXISTS decided_by;

ALTER TABLE fault_impacts
    DROP CONSTRAINT fault_impacts_state_check;

ALTER TABLE fault_impacts
    ADD CONSTRAINT fault_impacts_state_check
    CHECK (state IN ('potential', 'confirmed'));
