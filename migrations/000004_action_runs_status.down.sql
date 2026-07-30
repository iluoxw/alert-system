ALTER TABLE action_runs DROP CONSTRAINT action_runs_status_check;
ALTER TABLE action_runs ADD CONSTRAINT action_runs_status_check
    CHECK (status IN ('triggered', 'succeeded', 'failed'));
