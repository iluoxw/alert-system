DROP TRIGGER IF EXISTS alert_mapping_results_set_updated_at ON alert_mapping_results;
DROP TABLE IF EXISTS alert_mapping_results;
DROP TRIGGER IF EXISTS alert_mapping_policies_set_updated_at ON alert_mapping_policies;
DROP TABLE IF EXISTS alert_mapping_policies;
DROP TRIGGER IF EXISTS entity_aliases_set_updated_at ON entity_aliases;
DROP TABLE IF EXISTS entity_aliases;
DROP TRIGGER IF EXISTS service_registrations_set_updated_at ON service_registrations;
DROP TABLE IF EXISTS service_registrations;
DROP INDEX IF EXISTS resource_nodes_layer_managed_idx;
DROP INDEX IF EXISTS resource_nodes_entity_key_uidx;
ALTER TABLE resource_nodes
    DROP COLUMN IF EXISTS managed,
    DROP COLUMN IF EXISTS layer,
    DROP COLUMN IF EXISTS entity_key;
