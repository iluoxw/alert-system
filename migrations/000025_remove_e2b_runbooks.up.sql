-- Remove E2B demo runbook definitions and dependent audit rows. Historical 000014
-- seed remains in migration history; new installs no longer retain E2B product surface.

DELETE FROM runbook_approvals
WHERE plan_run_id IN (SELECT id FROM runbook_runs WHERE action_key LIKE 'e2b.%');

DELETE FROM runbook_runs WHERE action_key LIKE 'e2b.%';

DELETE FROM runbooks WHERE action_key LIKE 'e2b.%';

DROP INDEX IF EXISTS runbook_runs_active_apply_uidx;
