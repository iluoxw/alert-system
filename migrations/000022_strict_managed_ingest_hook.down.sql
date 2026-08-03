ALTER INDEX integrations_hook_id_hash_uidx RENAME TO integrations_token_hash_uidx;
ALTER TABLE integrations RENAME COLUMN hook_id_hash TO token_hash;
