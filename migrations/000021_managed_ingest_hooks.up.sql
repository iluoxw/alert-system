-- Managed ingest hook credentials:
-- token_hash now identifies one and only one Integration when the opaque hook
-- UUID is presented in /ingest/v1/hook/{hook_id}. Plaintext hook IDs are never
-- persisted, so uniqueness must be enforced on their hashes.
CREATE UNIQUE INDEX integrations_token_hash_uidx ON integrations (token_hash);
