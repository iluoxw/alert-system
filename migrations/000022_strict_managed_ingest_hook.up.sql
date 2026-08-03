-- Hard cut to the sole Alert webhook contract:
-- POST /ingest/v1/hook/{hook_id}. Rename the credential hash so the schema no
-- longer suggests a second Header token contract. Renaming preserves all
-- existing managed hook credentials and the uniqueness guarantee.
ALTER TABLE integrations RENAME COLUMN token_hash TO hook_id_hash;
ALTER INDEX integrations_token_hash_uidx RENAME TO integrations_hook_id_hash_uidx;
