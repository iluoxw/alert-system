DROP TABLE IF EXISTS fault_notification_deliveries;
DROP TABLE IF EXISTS fault_notification_watches;
DROP TRIGGER IF EXISTS faults_activation_version ON faults;
DROP FUNCTION IF EXISTS maintain_fault_activation_version();
ALTER TABLE faults DROP COLUMN IF EXISTS activation_version;
